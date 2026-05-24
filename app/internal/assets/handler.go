// Package assets implements the storage-facing endpoints of the
// artist-alley HTTP API: upload (single-shot for now; TUS-resumable
// in a follow-up), download original, and download a named variant.
//
// The handler is a thin layer over storage.Service. Authorization is
// "must be authenticated" for Phase 1.4.C; per-pin access control
// lands when the resource layer (separate phase) introduces user-
// owned objects.
package assets

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// PinSubjectTypeUser is the canonical pin subject type for "this
// upload belongs to a user account, not to any richer resource". When
// the resource layer lands, uploads tied to a resource will use
// "resource" as the subject type instead.
const PinSubjectTypeUser = "user"

// Handler implements the assets-related slice of
// openapi.StrictServerInterface.
type Handler struct {
	Service *storage.Service
	Logger  *slog.Logger
}

// NewHandler binds the handler to a storage Service.
func NewHandler(svc *storage.Service, logger *slog.Logger) *Handler {
	return &Handler{Service: svc, Logger: logger}
}

// UploadAsset implements POST /api/v1/assets.
//
// Streams the request body through storage.Service.UploadOriginal,
// which hashes on the fly, dedups against any existing object with
// the same sha256, and adds a `user:<ref>` pin so the caller owns
// the upload.
func (h *Handler) UploadAsset(
	ctx context.Context,
	req openapi.UploadAssetRequestObject,
) (openapi.UploadAssetResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UploadAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UploadAsset500JSONResponse{
			InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "missing request body"},
		}, nil
	}

	contentType := "application/octet-stream"
	if req.Params.XContentType != nil && *req.Params.XContentType != "" {
		contentType = *req.Params.XContentType
	}

	pin := storage.PinRef{
		SubjectType: PinSubjectTypeUser,
		SubjectID:   strconv.FormatInt(id.UserRef, 10),
	}

	result, err := h.Service.UploadOriginal(ctx, req.Body, contentType, pin)
	if err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelError, "assets.upload.error",
			slog.Int64("user_ref", id.UserRef),
			slog.String("err", err.Error()),
		)
		return openapi.UploadAsset500JSONResponse{
			InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "could not store upload"},
		}, nil
	}

	return openapi.UploadAsset201JSONResponse(openapi.UploadResult{
		Hash:           result.Hash,
		Size:           result.Size,
		ContentType:    result.ContentType,
		Deduped:        result.Deduped,
		PinSubjectType: result.Pin.SubjectType,
		PinSubjectId:   result.Pin.SubjectID,
	}), nil
}

// DownloadAssetOriginal implements GET /api/v1/assets/{hash}.
func (h *Handler) DownloadAssetOriginal(
	ctx context.Context,
	req openapi.DownloadAssetOriginalRequestObject,
) (openapi.DownloadAssetOriginalResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DownloadAssetOriginal401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	return h.download(ctx, req.Hash, storage.VariantOriginal, headerVal(req.Params.Range), originalResponder{})
}

// DownloadAssetVariant implements GET /api/v1/assets/{hash}/{variant}.
func (h *Handler) DownloadAssetVariant(
	ctx context.Context,
	req openapi.DownloadAssetVariantRequestObject,
) (openapi.DownloadAssetVariantResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DownloadAssetVariant401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	resp, err := h.download(ctx, req.Hash, req.Variant, headerVal(req.Params.Range), variantResponder{})
	if err != nil || resp == nil {
		return nil, err
	}
	return resp.(openapi.DownloadAssetVariantResponseObject), nil
}

// --- shared download path -------------------------------------------------

// downloadResponder turns a generic download outcome into the
// per-operation response type the codegen wants. Allows us to share
// the actual handler logic between the two GET endpoints.
type downloadResponder interface {
	full(body io.Reader, length int64) any
	partial(body io.Reader, length int64) any
	notFound() any
}

type originalResponder struct{}

func (originalResponder) full(body io.Reader, length int64) any {
	return openapi.DownloadAssetOriginal200ApplicationoctetStreamResponse{Body: body, ContentLength: length}
}
func (originalResponder) partial(body io.Reader, length int64) any {
	return openapi.DownloadAssetOriginal206ApplicationoctetStreamResponse{Body: body, ContentLength: length}
}
func (originalResponder) notFound() any {
	return openapi.DownloadAssetOriginal404JSONResponse{
		NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "object not found"},
	}
}

type variantResponder struct{}

func (variantResponder) full(body io.Reader, length int64) any {
	return openapi.DownloadAssetVariant200ApplicationoctetStreamResponse{Body: body, ContentLength: length}
}
func (variantResponder) partial(body io.Reader, length int64) any {
	return openapi.DownloadAssetVariant206ApplicationoctetStreamResponse{Body: body, ContentLength: length}
}
func (variantResponder) notFound() any {
	return openapi.DownloadAssetVariant404JSONResponse{
		NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "object not found"},
	}
}

func (h *Handler) download(
	ctx context.Context,
	hash, variant, rangeHeader string,
	resp downloadResponder,
) (openapi.DownloadAssetOriginalResponseObject, error) {
	if rangeHeader == "" {
		body, info, err := h.Service.Download(ctx, hash, variant)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return resp.notFound().(openapi.DownloadAssetOriginalResponseObject), nil
			}
			return nil, err
		}
		return resp.full(body, info.Size).(openapi.DownloadAssetOriginalResponseObject), nil
	}

	// Honour a single-range request. Multi-range (the rare comma form)
	// returns a 200 with the full body — same as nginx's default for
	// edge cases.
	offset, length, ok := parseSingleRange(rangeHeader)
	if !ok {
		body, info, err := h.Service.Download(ctx, hash, variant)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return resp.notFound().(openapi.DownloadAssetOriginalResponseObject), nil
			}
			return nil, err
		}
		return resp.full(body, info.Size).(openapi.DownloadAssetOriginalResponseObject), nil
	}

	body, err := h.Service.DownloadRange(ctx, hash, variant, offset, length)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return resp.notFound().(openapi.DownloadAssetOriginalResponseObject), nil
		}
		return nil, err
	}
	return resp.partial(body, length).(openapi.DownloadAssetOriginalResponseObject), nil
}

// --- helpers ---------------------------------------------------------------

func headerVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// parseSingleRange parses "bytes=<off>-<end>" or "bytes=<off>-" into
// (offset, length, ok). Returns ok=false for any form we don't
// understand (multi-range, suffix-range), letting the caller fall
// back to a 200 full-body response.
func parseSingleRange(h string) (offset, length int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if strings.Contains(spec, ",") {
		return 0, 0, false // multi-range
	}
	dash := strings.IndexByte(spec, '-')
	if dash <= 0 {
		// "-N" suffix range; not bothering for Phase 1.4.C.
		return 0, 0, false
	}
	startStr, endStr := spec[:dash], spec[dash+1:]
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if endStr == "" {
		// "bytes=N-" — to EOF
		return start, 0, true
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, end - start + 1, true
}

// Compile-time assertion: the Handler satisfies the part of
// StrictServerInterface it claims to. Catches drift if codegen
// signatures change.
var _ interface {
	UploadAsset(context.Context, openapi.UploadAssetRequestObject) (openapi.UploadAssetResponseObject, error)
	DownloadAssetOriginal(context.Context, openapi.DownloadAssetOriginalRequestObject) (openapi.DownloadAssetOriginalResponseObject, error)
	DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error)
} = (*Handler)(nil)
