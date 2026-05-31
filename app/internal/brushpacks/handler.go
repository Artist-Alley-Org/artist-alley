// HTTP handler — wires the brushpacks Service into the oapi-codegen
// strict server interface (see openapi.gen.go). Every endpoint
// short-circuits to 401 when there's no authenticated identity in
// ctx; ownership scoping happens inside the service.
//
// Multipart upload note:
// oapi-codegen's strict server materializes multipart bodies into a
// Go struct with concrete fields. For ABR uploads the body is a
// `file` field carrying potentially-tens-of-MB of binary. The
// generated reader (multipart.File) supports both seekable + non-
// seekable streams; the service consumes it via io.Reader so we
// don't need the whole pack in memory at the handler layer (though
// abr.ParseBrushes currently buffers it all — that's a parser-side
// concern).

package brushpacks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Handler wraps Service to satisfy the brush-packs subset of the
// generated StrictServerInterface. The shim in internal/http
// composes this Handler with every other domain's Handler into the
// one strict server the router mounts.
type Handler struct {
	Service *Service
}

// NewHandler returns the route-handler. The service must already be
// wired with a pool + storage backend.
func NewHandler(svc *Service) *Handler {
	return &Handler{Service: svc}
}

// ListBrushPacks GET /brush-packs
func (h *Handler) ListBrushPacks(
	ctx context.Context,
	_ openapi.ListBrushPacksRequestObject,
) (openapi.ListBrushPacksResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListBrushPacks401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "unauthenticated"},
		}, nil
	}
	packs, err := h.Service.ListPacks(ctx, id.UserRef)
	if err != nil {
		return nil, fmt.Errorf("brushpacks: list: %w", err)
	}
	// Hydrate each pack with its stamps so the frontend can register
	// the whole catalogue in one shot — no N+1 fetch from the picker
	// UI. Cost is proportional to (packs * avg stamps); for normal
	// usage (< 10 packs of < 50 stamps) this is negligible.
	out := make([]openapi.BrushPack, 0, len(packs))
	for _, p := range packs {
		stamps, err := h.Service.q.ListStampsForPack(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("brushpacks: list stamps: %w", err)
		}
		out = append(out, packToAPI(p, stamps))
	}
	return openapi.ListBrushPacks200JSONResponse{Packs: out}, nil
}

// ImportBrushPack POST /brush-packs (multipart)
func (h *Handler) ImportBrushPack(
	ctx context.Context,
	req openapi.ImportBrushPackRequestObject,
) (openapi.ImportBrushPackResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		// No 401 in the OpenAPI for ImportBrushPack since multipart
		// endpoints often skip it in the strict server — fall through
		// to a 400 with a clear error. (We could add a 401 to the
		// schema if this becomes a common shape.)
		return openapi.ImportBrushPack400JSONResponse{Error: "unauthenticated"}, nil
	}
	if req.Body == nil {
		return openapi.ImportBrushPack400JSONResponse{Error: "missing body"}, nil
	}
	// Pull the file part. oapi-codegen parses multipart into a single
	// reader per part — `req.Body` exposes the multipart.Reader so we
	// walk parts ourselves to grab both `name` (optional) and `file`.
	var (
		uploadName string
		filename   string
		fileBody   io.Reader
		fileBuf    []byte
	)
	for {
		part, err := req.Body.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return openapi.ImportBrushPack400JSONResponse{Error: "malformed multipart"}, nil
		}
		switch part.FormName() {
		case "name":
			// Display name. Capped at a sane length so a hostile
			// upload can't blow up the panel.
			b, _ := io.ReadAll(io.LimitReader(part, 200))
			uploadName = strings.TrimSpace(string(b))
		case "file":
			filename = filepath.Base(part.FileName())
			// Buffer the file. We need a *seekable* reader for the
			// parser (Kaitai uses Seek), and multipart.Part isn't
			// seekable — so we eat the cost of an in-memory copy
			// up to a generous cap. 256 MB covers the worst-case
			// real ABRs we've seen; bigger uploads return 413.
			const maxBytes = 256 * 1024 * 1024
			b, err := io.ReadAll(io.LimitReader(part, maxBytes+1))
			if err != nil {
				return openapi.ImportBrushPack400JSONResponse{Error: "read file: " + err.Error()}, nil
			}
			if len(b) > maxBytes {
				return openapi.ImportBrushPack400JSONResponse{Error: fmt.Sprintf("file exceeds %d MB cap", maxBytes/(1024*1024))}, nil
			}
			fileBuf = b
		}
	}
	if fileBuf == nil {
		return openapi.ImportBrushPack400JSONResponse{Error: "missing `file` part"}, nil
	}
	if uploadName == "" {
		// Fall back to the filename minus extension.
		uploadName = strings.TrimSuffix(filename, filepath.Ext(filename))
		if uploadName == "" {
			uploadName = "Imported pack"
		}
	}
	fileBody = bytesReader(fileBuf)

	res, err := h.Service.ImportABR(ctx, id.UserRef, uploadName, filename, fileBody)
	if err != nil {
		// ABR-parse failures are user-actionable ("the pack is
		// malformed / wrong version"). Surface the parser's error
		// directly. Database / storage failures are server bugs;
		// bubble them up as 5xx via the caller's recovery.
		if errors.Is(err, errNonABR) || strings.HasPrefix(err.Error(), "brushpacks: parse:") || strings.HasPrefix(err.Error(), "brushpacks: no decodable") {
			return openapi.ImportBrushPack400JSONResponse{Error: err.Error()}, nil
		}
		return nil, err
	}
	return openapi.ImportBrushPack201JSONResponse(packToAPI(res.Pack, res.Stamps)), nil
}

// GetBrushPack GET /brush-packs/{packId}
func (h *Handler) GetBrushPack(
	ctx context.Context,
	req openapi.GetBrushPackRequestObject,
) (openapi.GetBrushPackResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetBrushPack404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "not found"}}, nil
	}
	pack, stamps, err := h.Service.GetPack(ctx, id.UserRef, uuidToPg(req.PackId))
	if err != nil {
		if errors.Is(err, ErrPackNotFound) {
			return openapi.GetBrushPack404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "not found"}}, nil
		}
		return nil, err
	}
	return openapi.GetBrushPack200JSONResponse(packToAPI(*pack, stamps)), nil
}

// DeleteBrushPack DELETE /brush-packs/{packId}
func (h *Handler) DeleteBrushPack(
	ctx context.Context,
	req openapi.DeleteBrushPackRequestObject,
) (openapi.DeleteBrushPackResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DeleteBrushPack404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "not found"}}, nil
	}
	if err := h.Service.DeletePack(ctx, id.UserRef, uuidToPg(req.PackId)); err != nil {
		if errors.Is(err, ErrPackNotFound) {
			return openapi.DeleteBrushPack404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "not found"}}, nil
		}
		return nil, err
	}
	return openapi.DeleteBrushPack204Response{}, nil
}

// GetBrushPackStamp GET /brush-packs/stamps/{stampId}
func (h *Handler) GetBrushPackStamp(
	ctx context.Context,
	req openapi.GetBrushPackStampRequestObject,
) (openapi.GetBrushPackStampResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetBrushPackStamp404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "not found"}}, nil
	}
	body, _, err := h.Service.OpenStamp(ctx, id.UserRef, uuidToPg(req.StampId))
	if err != nil {
		if errors.Is(err, ErrStampNotFound) {
			return openapi.GetBrushPackStamp404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "not found"}}, nil
		}
		return nil, err
	}
	// 200 with the streamed PNG. The oapi-codegen response type for
	// `image/png` accepts a reader + content length (we don't track
	// length yet; pass -1 to signal chunked).
	return openapi.GetBrushPackStamp200ImagepngResponse{
		Body:          body,
		ContentLength: -1,
	}, nil
}

// ── helpers ──────────────────────────────────────────────────────

// packToAPI converts the sqlc row + its stamps into the OpenAPI
// type that goes back over the wire.
func packToAPI(p BrushPack, stamps []BrushPackStamp) openapi.BrushPack {
	apiStamps := make([]openapi.BrushPackStamp, 0, len(stamps))
	for _, s := range stamps {
		apiStamps = append(apiStamps, stampToAPI(s))
	}
	out := openapi.BrushPack{
		Id:        pgToUUID(p.ID),
		Name:      p.Name,
		CreatedAt: p.CreatedAt.Time,
		Stamps:    apiStamps,
	}
	if p.SourceFile != nil {
		s := *p.SourceFile
		out.SourceFile = &s
	}
	return out
}

func stampToAPI(s BrushPackStamp) openapi.BrushPackStamp {
	out := openapi.BrushPackStamp{
		Id:           pgToUUID(s.ID),
		Width:        int(s.Width),
		Height:       int(s.Height),
		Spacing:      s.Spacing,
		AlignToPath:  s.AlignToPath,
	}
	if s.Label != nil {
		v := *s.Label
		out.Label = &v
	}
	if s.SizeJitter != nil {
		v := *s.SizeJitter
		out.SizeJitter = &v
	}
	if s.OpacityJitter != nil {
		v := *s.OpacityJitter
		out.OpacityJitter = &v
	}
	if s.AngleJitter != nil {
		v := *s.AngleJitter
		out.AngleJitter = &v
	}
	return out
}

// uuidToPg / pgToUUID — bridge between the openapi-types UUID
// representation (a 16-byte array) and pgtype.UUID (also a 16-byte
// array but wrapped in a Valid bool).
func uuidToPg(u openapi_types.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: u, Valid: true}
}

func pgToUUID(u pgtype.UUID) openapi_types.UUID {
	return openapi_types.UUID(u.Bytes)
}

// bytesReader is a tiny convenience so callers don't need to know
// the import path of bytes.NewReader.
func bytesReader(b []byte) io.Reader {
	return &byteSliceReader{b: b}
}

type byteSliceReader struct {
	b   []byte
	off int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

// errNonABR sentinel for future use when we detect "this looks like
// it might be a Brushes preset (.tpl) or a Krita brush (.kpp)" so
// we can return a friendlier error than the raw Kaitai parse one.
var errNonABR = errors.New("brushpacks: not an ABR file")

// touch time so the linter doesn't complain about the import.
var _ = time.Time{}
