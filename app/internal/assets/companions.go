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
	"github.com/mscrnt/artist-alley/app/internal/storage"
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
