// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/preview/format3d"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// pinSubjectTypeCompanion claims the bytes for one companion row.
// Each companion gets its own pin (subject_id = companion.id) so
// deduplication works at the blob layer — the same texture uploaded
// twice keeps two pins on one storage object.
const pinSubjectTypeCompanion = "asset_companion"

// ---------------------------------------------------------------------------
// ListAssetCompanions  GET /assets/{id}/companions
// ---------------------------------------------------------------------------

func (h *Handler) ListAssetCompanions(
	ctx context.Context,
	req openapi.ListAssetCompanionsRequestObject,
) (openapi.ListAssetCompanionsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListAssetCompanions401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	cacheKey := req.Id.String()
	if h.companions != nil {
		if v, ok := h.companions.Get(cacheKey); ok {
			return openapi.ListAssetCompanions200JSONResponse(v), nil
		}
	}

	q := New(h.Pool)
	if _, err := q.GetAsset(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListAssetCompanions404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: companions: get asset: %w", err)
	}

	rows, err := q.ListAssetCompanions(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true})
	if err != nil {
		return nil, fmt.Errorf("assets: companions: list: %w", err)
	}

	out := make([]openapi.AssetCompanion, 0, len(rows))
	for _, r := range rows {
		out = append(out, companionRowToOpenAPI(r))
	}
	if h.companions != nil {
		h.companions.Add(cacheKey, out)
	}
	return openapi.ListAssetCompanions200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// AddAssetCompanion  POST /assets/{id}/companions
// ---------------------------------------------------------------------------

func (h *Handler) AddAssetCompanion(
	ctx context.Context,
	req openapi.AddAssetCompanionRequestObject,
) (openapi.AddAssetCompanionResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.AddAssetCompanion401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddAssetCompanion400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	companionPath := strings.TrimSpace(req.Params.XCompanionPath)
	if companionPath == "" {
		return openapi.AddAssetCompanion400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "X-Companion-Path header required"},
		}, nil
	}
	if len(companionPath) > 512 {
		return openapi.AddAssetCompanion400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "companion path too long (max 512)"},
		}, nil
	}
	// No path traversal — the LoadingManager will treat these as
	// relative to the model file and we want them rooted there.
	if strings.HasPrefix(companionPath, "/") || strings.Contains(companionPath, "..") {
		return openapi.AddAssetCompanion400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "companion path must be a clean relative path"},
		}, nil
	}

	q := New(h.Pool)
	pgAssetID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if _, err := q.GetAsset(ctx, pgAssetID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddAssetCompanion404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: companions: get asset: %w", err)
	}

	// If a companion already exists at this path, drop it first so we
	// don't end up with a unique-constraint violation. The old pin
	// gets removed too so dedup accounting stays correct.
	if existing, err := q.GetAssetCompanionByPath(ctx, GetAssetCompanionByPathParams{
		AssetID:       pgAssetID,
		CompanionPath: companionPath,
	}); err == nil {
		if err := h.removeCompanion(ctx, q, existing); err != nil {
			return nil, fmt.Errorf("assets: companions: replace existing: %w", err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("assets: companions: probe existing: %w", err)
	}

	contentType := "application/octet-stream"
	if req.Params.XContentType != nil && *req.Params.XContentType != "" {
		contentType = *req.Params.XContentType
	}

	// Stream the bytes content-addressed into storage. We pin under
	// a placeholder subject_id and re-key it once the companion row
	// exists (subject_id needs the row's UUID). Two-step keeps the
	// upload truly atomic across crashes: bytes are pinned the whole
	// time, so they can't get GC'd between the put and the row insert.
	tempSubjectID := uuid.NewString()
	uploadResult, err := h.Storage.UploadOriginal(ctx, req.Body, contentType, storage.PinRef{
		SubjectType: pinSubjectTypeCompanion,
		SubjectID:   tempSubjectID,
	})
	if err != nil {
		return nil, fmt.Errorf("assets: companions: upload: %w", err)
	}

	row, err := q.AddAssetCompanion(ctx, AddAssetCompanionParams{
		AssetID:       pgAssetID,
		CompanionPath: companionPath,
		ObjectHash:    uploadResult.Hash,
		ContentType:   contentType,
		SizeBytes:     uploadResult.Size,
	})
	if err != nil {
		// Roll the temporary pin back so the bytes can be GC'd.
		_ = h.Storage.RemovePin(ctx, storage.PinRef{
			SubjectType: pinSubjectTypeCompanion,
			SubjectID:   tempSubjectID,
		}, uploadResult.Hash)
		return nil, fmt.Errorf("assets: companions: insert: %w", err)
	}

	// Re-key the pin to the companion row's UUID + drop the temp pin.
	if err := h.Storage.AddPin(ctx, storage.PinRef{
		SubjectType: pinSubjectTypeCompanion,
		SubjectID:   row.ID.String(),
	}, uploadResult.Hash); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.companion.repin_failed",
			slog.String("companion_id", row.ID.String()),
			slog.String("err", err.Error()))
	}
	if err := h.Storage.RemovePin(ctx, storage.PinRef{
		SubjectType: pinSubjectTypeCompanion,
		SubjectID:   tempSubjectID,
	}, uploadResult.Hash); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.companion.temp_unpin_failed",
			slog.String("companion_id", row.ID.String()),
			slog.String("err", err.Error()))
	}

	h.invalidateCompanions(ctx, uuid.UUID(req.Id))

	return openapi.AddAssetCompanion201JSONResponse(companionRowToOpenAPI(row)), nil
}

// ---------------------------------------------------------------------------
// DownloadAssetCompanion  GET /assets/{id}/companions/{companion_id}
// ---------------------------------------------------------------------------

func (h *Handler) DownloadAssetCompanion(
	ctx context.Context,
	req openapi.DownloadAssetCompanionRequestObject,
) (openapi.DownloadAssetCompanionResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.DownloadAssetCompanion401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	row, err := h.lookupCompanion(ctx, req.Id, req.CompanionId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DownloadAssetCompanion404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "companion not found"},
			}, nil
		}
		return nil, err
	}

	body, info, err := h.Storage.Download(ctx, row.ObjectHash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return openapi.DownloadAssetCompanion404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "stored object missing for this companion"},
			}, nil
		}
		return nil, err
	}
	return openapi.DownloadAssetCompanion200ApplicationoctetStreamResponse{
		Body:          body,
		ContentLength: info.Size,
	}, nil
}

// ---------------------------------------------------------------------------
// RemoveAssetCompanion  DELETE /assets/{id}/companions/{companion_id}
// ---------------------------------------------------------------------------

func (h *Handler) RemoveAssetCompanion(
	ctx context.Context,
	req openapi.RemoveAssetCompanionRequestObject,
) (openapi.RemoveAssetCompanionResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.RemoveAssetCompanion401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	row, err := h.lookupCompanion(ctx, req.Id, req.CompanionId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RemoveAssetCompanion404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "companion not found"},
			}, nil
		}
		return nil, err
	}
	q := New(h.Pool)
	if err := h.removeCompanion(ctx, q, row); err != nil {
		return nil, fmt.Errorf("assets: companions: remove: %w", err)
	}
	h.invalidateCompanions(ctx, uuid.UUID(req.Id))
	return openapi.RemoveAssetCompanion204Response{}, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// lookupCompanion fetches one companion row by id and verifies it
// belongs to the asset in the URL. Returns pgx.ErrNoRows when the
// companion doesn't exist OR when it exists but on a different
// asset (so the caller treats both as the same 404 surface).
func (h *Handler) lookupCompanion(
	ctx context.Context, assetID, companionID openapi_types.UUID,
) (AssetCompanion, error) {
	row, err := New(h.Pool).GetAssetCompanion(
		ctx,
		pgtype.UUID{Bytes: uuid.UUID(companionID), Valid: true},
	)
	if err != nil {
		return AssetCompanion{}, err
	}
	if row.AssetID.Bytes != uuid.UUID(assetID) {
		return AssetCompanion{}, pgx.ErrNoRows
	}
	return row, nil
}

// removeCompanion deletes the row + unpins the underlying storage
// object. Reusable by both the explicit delete handler and the
// replace-on-upload path.
func (h *Handler) removeCompanion(ctx context.Context, q *Queries, row AssetCompanion) error {
	if err := q.DeleteAssetCompanion(ctx, row.ID); err != nil {
		return fmt.Errorf("delete row: %w", err)
	}
	if err := h.Storage.RemovePin(ctx, storage.PinRef{
		SubjectType: pinSubjectTypeCompanion,
		SubjectID:   row.ID.String(),
	}, row.ObjectHash); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.companion.unpin_failed",
			slog.String("companion_id", row.ID.String()),
			slog.String("hash", row.ObjectHash),
			slog.String("err", err.Error()))
	}
	return nil
}

func (h *Handler) invalidateCompanions(ctx context.Context, assetID uuid.UUID) {
	if h.companions == nil {
		return
	}
	if err := h.companions.Invalidate(ctx, assetID.String()); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.companion.cache_invalidate_failed",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()))
	}
}

func companionRowToOpenAPI(r AssetCompanion) openapi.AssetCompanion {
	return openapi.AssetCompanion{
		Id:          openapi_types.UUID(r.ID.Bytes),
		AssetId:     openapi_types.UUID(r.AssetID.Bytes),
		Path:        r.CompanionPath,
		ContentType: r.ContentType,
		SizeBytes:   r.SizeBytes,
		CreatedAt:   r.CreatedAt.Time,
	}
}

// ---------------------------------------------------------------------------
// GetAssetCompanionRequirements  GET /assets/{id}/companion-requirements
// ---------------------------------------------------------------------------

// declaredCompanions is one parse of one stored model: what it says it
// needs, and how much of that we were able to read.
//
// Cached under the OBJECT HASH (CacheDomainAssetDeclaredCompanions), so
// an entry is immutable for the life of the bytes and there is no
// invalidation path to forget. `Detail` carries a parse failure, which
// is cached too — re-reading a container that did not parse on every
// request buys nothing, and the answer will not change until the bytes
// do, which they cannot.
type declaredCompanions struct {
	Paths   []string
	Support format3d.CompanionSupport
	Detail  string
}

// GetAssetCompanionRequirements reads the stored model, parses the
// external files it declares, and subtracts the companions already
// attached (#754).
//
// # Why this endpoint exists at all
//
// A multi-file model only renders if its siblings are registered as
// companions, and nothing on the upload path derived that list. An
// artist who uploaded a GLB without knowing it names
// `Textures/planks.png` got a job that SUCCEEDED and a card and viewer
// that came out grey — a failure that reads as a renderer bug, which is
// exactly what #689 chased into the renderer before #750 found the real
// cause. This names the gap instead.
//
// # Why it does not call ResolveCompanions
//
// ResolveCompanions fuses two jobs: parse the declarations, and check
// the filesystem for them relative to the model's directory. Only the
// first is meaningful here. An uploaded asset lives in content-addressed
// storage under a hash — there is no sibling directory, so the second
// half would be statting nothing. format3d.DeclaredCompanions is the
// separated half, and it owns the extension table both callers share.
//
// # Why it attaches nothing
//
// Option 1 of #754 and deliberately only option 1: tell the artist what
// is missing. Matching declared paths against other files in the same
// drop (option 2) is a larger change with its own failure modes, and
// the silent-grey-render bug is removed by naming the gap alone.
func (h *Handler) GetAssetCompanionRequirements(
	ctx context.Context,
	req openapi.GetAssetCompanionRequirementsRequestObject,
) (openapi.GetAssetCompanionRequirementsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetAssetCompanionRequirements401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	// This answer is DERIVED FROM THE FILE'S BYTES, so it sits on the
	// content plane and not on the metadata plane — the same gate
	// DownloadAssetFile applies, for the same reason and with the same
	// 404 rather than 403, so a restricted asset's existence is not
	// confirmed. ListAssetCompanions next door is authentication-only
	// because it reports rows a caller attached; this reports what the
	// model itself says, which is a reading of the file.
	caller, caps := contentCaller(ctx)
	allowed, err := visibility.CanReadContent(ctx, h.Pool, caller, caps, uuid.UUID(req.Id),
		visibility.MatureFromContext(ctx))
	if err != nil || !allowed {
		return openapi.GetAssetCompanionRequirements404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}

	q := New(h.Pool)
	pgAssetID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	row, err := q.GetAsset(ctx, pgAssetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetAssetCompanionRequirements404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: companion requirements: get asset: %w", err)
	}

	out := openapi.AssetCompanionRequirements{
		AssetId:  req.Id,
		Status:   openapi.AssetCompanionRequirementsStatusUnsupported,
		Declared: []string{},
		Missing:  []string{},
		Attached: []string{},
	}

	ext := ""
	if row.FileExtension != nil {
		ext = *row.FileExtension
	}
	// No bytes and no readable extension are both "we cannot tell",
	// which is `unsupported` — never an empty `ok`, which would claim
	// the model needs nothing.
	if row.FileHash == nil || *row.FileHash == "" ||
		format3d.CompanionSupportFor(ext) == format3d.CompanionUnsupported {
		return openapi.GetAssetCompanionRequirements200JSONResponse(out), nil
	}

	parsed, err := h.declaredCompanionsFor(ctx, *row.FileHash, ext)
	if err != nil {
		return nil, err
	}
	out.Partial = parsed.Support == format3d.CompanionFirstLevel
	if parsed.Detail != "" {
		out.Status = openapi.AssetCompanionRequirementsStatusUnreadable
		detail := parsed.Detail
		out.Detail = &detail
		return openapi.GetAssetCompanionRequirements200JSONResponse(out), nil
	}
	out.Status = openapi.AssetCompanionRequirementsStatusOk
	if len(parsed.Paths) > 0 {
		out.Declared = append(out.Declared, parsed.Paths...)
	}

	// The subtraction. Recomputed per request rather than cached with
	// the parse, because THIS half changes: attaching a companion is
	// exactly the write that moves a path from `missing` to `attached`.
	rows, err := q.ListAssetCompanions(ctx, pgAssetID)
	if err != nil {
		return nil, fmt.Errorf("assets: companion requirements: list: %w", err)
	}
	have := make(map[string]struct{}, len(rows))
	for _, c := range rows {
		have[c.CompanionPath] = struct{}{}
	}
	for _, p := range out.Declared {
		if _, ok := have[p]; ok {
			out.Attached = append(out.Attached, p)
			continue
		}
		out.Missing = append(out.Missing, p)
	}

	return openapi.GetAssetCompanionRequirements200JSONResponse(out), nil
}

// declaredCompanionsFor reads the stored object and parses it, through
// the hash-keyed cache.
//
// A parse failure is returned as a populated Detail rather than as an
// error: it is an answer about the file ("we read these bytes and could
// not finish"), not a fault in serving the request. A STORAGE failure
// is a real error and propagates — the difference matters, because
// reporting a missing blob as "this model declares nothing" is the
// silent-success failure mode this whole endpoint exists to remove.
func (h *Handler) declaredCompanionsFor(ctx context.Context, hash, ext string) (declaredCompanions, error) {
	key := hash + ":" + strings.ToLower(strings.TrimPrefix(ext, "."))
	if h.declaredCompanions != nil {
		if v, ok := h.declaredCompanions.Get(key); ok {
			return v, nil
		}
	}

	body, _, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// The row points at bytes that are not there. Not cached:
			// unlike a parse failure this can be repaired without the
			// asset changing (a restore, a re-upload of the same
			// content), so a cached "unreadable" would outlive the fix.
			return declaredCompanions{
				Support: format3d.CompanionSupportFor(ext),
				Detail:  "stored object missing for this asset",
			}, nil
		}
		return declaredCompanions{}, fmt.Errorf("assets: companion requirements: download: %w", err)
	}
	defer func() { _ = body.Close() }()

	paths, support, perr := format3d.DeclaredCompanions(ext, body)
	entry := declaredCompanions{Paths: paths, Support: support}
	if perr != nil {
		// Never fold a parse failure into an empty list — see the
		// soft-fail contract in format3d/companions.go. The caller is
		// told we could not read it, which is a different sentence from
		// "it needs nothing".
		entry = declaredCompanions{Support: format3d.CompanionSupportFor(ext), Detail: perr.Error()}
		h.Logger.LogAttrs(ctx, slog.LevelInfo, "assets.companion_requirements.unreadable",
			slog.String("hash", hash),
			slog.String("ext", ext),
			slog.String("err", perr.Error()))
	}
	if h.declaredCompanions != nil {
		h.declaredCompanions.Add(key, entry)
	}
	return entry, nil
}
