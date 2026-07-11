// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"context"
	"encoding/json"
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

// pinSubjectTypeAlternate claims the bytes for one alternate row.
// Mirrors the companion pin pattern — each alternate gets its own
// pin so deduplication works at the blob layer (the same palette-
// swap output run twice keeps two pins on one storage object).
const pinSubjectTypeAlternate = "asset_alternate"

// defaultAlternateKind is the bucket new alternates land in when the
// caller didn't pass X-Alternate-Kind. Matches the migration default
// so the column never carries an empty string.
const defaultAlternateKind = "authored"

// maxAlternateMetadataBytes caps the JSON blob sent through
// X-Alternate-Metadata. Headers go through HTTP/2 HPACK; very large
// header values choke proxies. 8KB matches the OpenAPI schema limit.
const maxAlternateMetadataBytes = 8 * 1024

// ---------------------------------------------------------------------------
// ListAssetAlternates  GET /assets/{id}/alternates
// ---------------------------------------------------------------------------

func (h *Handler) ListAssetAlternates(
	ctx context.Context,
	req openapi.ListAssetAlternatesRequestObject,
) (openapi.ListAssetAlternatesResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListAssetAlternates401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	cacheKey := req.Id.String()
	if h.alternates != nil {
		if v, ok := h.alternates.Get(cacheKey); ok {
			return openapi.ListAssetAlternates200JSONResponse(v), nil
		}
	}

	q := New(h.Pool)
	if _, err := q.GetAsset(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListAssetAlternates404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: alternates: get asset: %w", err)
	}

	rows, err := q.ListAssetAlternates(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true})
	if err != nil {
		return nil, fmt.Errorf("assets: alternates: list: %w", err)
	}

	out := make([]openapi.AssetAlternate, 0, len(rows))
	for _, r := range rows {
		out = append(out, alternateRowToOpenAPI(r))
	}
	if h.alternates != nil {
		h.alternates.Add(cacheKey, out)
	}
	return openapi.ListAssetAlternates200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// AddAssetAlternate  POST /assets/{id}/alternates
// ---------------------------------------------------------------------------

func (h *Handler) AddAssetAlternate(
	ctx context.Context,
	req openapi.AddAssetAlternateRequestObject,
) (openapi.AddAssetAlternateResponseObject, error) {
	identity := auth.IdentityFromContext(ctx)
	if identity == nil {
		return openapi.AddAssetAlternate401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddAssetAlternate400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	label := strings.TrimSpace(req.Params.XAlternateLabel)
	if label == "" {
		return openapi.AddAssetAlternate400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "X-Alternate-Label header required"},
		}, nil
	}
	if len(label) > 256 {
		return openapi.AddAssetAlternate400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "alternate label too long (max 256)"},
		}, nil
	}

	kind := defaultAlternateKind
	if req.Params.XAlternateKind != nil {
		k := strings.TrimSpace(*req.Params.XAlternateKind)
		if k != "" {
			if len(k) > 64 {
				return openapi.AddAssetAlternate400JSONResponse{
					BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "alternate kind too long (max 64)"},
				}, nil
			}
			kind = k
		}
	}

	// Parse optional per-kind metadata. Defaults to '{}' so the column
	// is never NULL — keeps downstream JSONB readers simple.
	metadataBytes := []byte("{}")
	if req.Params.XAlternateMetadata != nil && *req.Params.XAlternateMetadata != "" {
		raw := *req.Params.XAlternateMetadata
		if len(raw) > maxAlternateMetadataBytes {
			return openapi.AddAssetAlternate400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "alternate metadata too large (max 8KB)"},
			}, nil
		}
		// Validate as JSON object — opaque to us but must be a well-
		// formed JSON value so the JSONB cast at insert doesn't panic.
		var probe any
		if err := json.Unmarshal([]byte(raw), &probe); err != nil {
			return openapi.AddAssetAlternate400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "X-Alternate-Metadata is not valid JSON"},
			}, nil
		}
		metadataBytes = []byte(raw)
	}

	q := New(h.Pool)
	pgAssetID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if _, err := q.GetAsset(ctx, pgAssetID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddAssetAlternate404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: alternates: get asset: %w", err)
	}

	// Replace an existing alternate with the same label — pin /
	// row drop happens first so the unique (asset_id, label)
	// constraint doesn't kill the insert.
	if existing, err := q.GetAssetAlternateByLabel(ctx, GetAssetAlternateByLabelParams{
		AssetID: pgAssetID,
		Label:   label,
	}); err == nil {
		if err := h.removeAlternate(ctx, q, existing); err != nil {
			return nil, fmt.Errorf("assets: alternates: replace existing: %w", err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("assets: alternates: probe existing: %w", err)
	}

	contentType := "application/octet-stream"
	if req.Params.XContentType != nil && *req.Params.XContentType != "" {
		contentType = *req.Params.XContentType
	}

	// Two-phase upload mirrors the companion path: pin under a temp
	// subject id first so the bytes can't be GC'd between the put
	// and the row insert.
	tempSubjectID := uuid.NewString()
	uploadResult, err := h.Storage.UploadOriginal(ctx, req.Body, contentType, storage.PinRef{
		SubjectType: pinSubjectTypeAlternate,
		SubjectID:   tempSubjectID,
	})
	if err != nil {
		return nil, fmt.Errorf("assets: alternates: upload: %w", err)
	}

	var createdBy *int64
	if identity.UserRef != 0 {
		ref := identity.UserRef
		createdBy = &ref
	}

	row, err := q.AddAssetAlternate(ctx, AddAssetAlternateParams{
		AssetID:          pgAssetID,
		Label:            label,
		Kind:             kind,
		ObjectHash:       uploadResult.Hash,
		ContentType:      contentType,
		SizeBytes:        uploadResult.Size,
		OriginServerID:   pgtype.UUID{},
		CreatedByUserRef: createdBy,
		Metadata:         metadataBytes,
	})
	if err != nil {
		_ = h.Storage.RemovePin(ctx, storage.PinRef{
			SubjectType: pinSubjectTypeAlternate,
			SubjectID:   tempSubjectID,
		}, uploadResult.Hash)
		return nil, fmt.Errorf("assets: alternates: insert: %w", err)
	}

	if err := h.Storage.AddPin(ctx, storage.PinRef{
		SubjectType: pinSubjectTypeAlternate,
		SubjectID:   row.ID.String(),
	}, uploadResult.Hash); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.alternate.repin_failed",
			slog.String("alternate_id", row.ID.String()),
			slog.String("err", err.Error()))
	}
	if err := h.Storage.RemovePin(ctx, storage.PinRef{
		SubjectType: pinSubjectTypeAlternate,
		SubjectID:   tempSubjectID,
	}, uploadResult.Hash); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.alternate.temp_unpin_failed",
			slog.String("alternate_id", row.ID.String()),
			slog.String("err", err.Error()))
	}

	h.invalidateAlternates(ctx, uuid.UUID(req.Id))

	return openapi.AddAssetAlternate201JSONResponse(alternateRowToOpenAPI(row)), nil
}

// ---------------------------------------------------------------------------
// DownloadAssetAlternate  GET /assets/{id}/alternates/{alternate_id}
// ---------------------------------------------------------------------------

func (h *Handler) DownloadAssetAlternate(
	ctx context.Context,
	req openapi.DownloadAssetAlternateRequestObject,
) (openapi.DownloadAssetAlternateResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.DownloadAssetAlternate401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	row, err := h.lookupAlternate(ctx, req.Id, req.AlternateId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DownloadAssetAlternate404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "alternate not found"},
			}, nil
		}
		return nil, err
	}

	body, info, err := h.Storage.Download(ctx, row.ObjectHash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return openapi.DownloadAssetAlternate404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "stored object missing for this alternate"},
			}, nil
		}
		return nil, err
	}
	return openapi.DownloadAssetAlternate200ApplicationoctetStreamResponse{
		Body:          body,
		ContentLength: info.Size,
	}, nil
}

// ---------------------------------------------------------------------------
// RemoveAssetAlternate  DELETE /assets/{id}/alternates/{alternate_id}
// ---------------------------------------------------------------------------

func (h *Handler) RemoveAssetAlternate(
	ctx context.Context,
	req openapi.RemoveAssetAlternateRequestObject,
) (openapi.RemoveAssetAlternateResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.RemoveAssetAlternate401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	row, err := h.lookupAlternate(ctx, req.Id, req.AlternateId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RemoveAssetAlternate404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "alternate not found"},
			}, nil
		}
		return nil, err
	}
	q := New(h.Pool)
	if err := h.removeAlternate(ctx, q, row); err != nil {
		return nil, fmt.Errorf("assets: alternates: remove: %w", err)
	}
	h.invalidateAlternates(ctx, uuid.UUID(req.Id))
	return openapi.RemoveAssetAlternate204Response{}, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func (h *Handler) lookupAlternate(
	ctx context.Context, assetID, alternateID openapi_types.UUID,
) (AssetAlternate, error) {
	row, err := New(h.Pool).GetAssetAlternate(
		ctx,
		pgtype.UUID{Bytes: uuid.UUID(alternateID), Valid: true},
	)
	if err != nil {
		return AssetAlternate{}, err
	}
	if row.AssetID.Bytes != uuid.UUID(assetID) {
		return AssetAlternate{}, pgx.ErrNoRows
	}
	return row, nil
}

func (h *Handler) removeAlternate(ctx context.Context, q *Queries, row AssetAlternate) error {
	if err := q.DeleteAssetAlternate(ctx, row.ID); err != nil {
		return fmt.Errorf("delete row: %w", err)
	}
	if err := h.Storage.RemovePin(ctx, storage.PinRef{
		SubjectType: pinSubjectTypeAlternate,
		SubjectID:   row.ID.String(),
	}, row.ObjectHash); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.alternate.unpin_failed",
			slog.String("alternate_id", row.ID.String()),
			slog.String("hash", row.ObjectHash),
			slog.String("err", err.Error()))
	}
	return nil
}

func (h *Handler) invalidateAlternates(ctx context.Context, assetID uuid.UUID) {
	if h.alternates == nil {
		return
	}
	if err := h.alternates.Invalidate(ctx, assetID.String()); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.alternate.cache_invalidate_failed",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()))
	}
}

func alternateRowToOpenAPI(r AssetAlternate) openapi.AssetAlternate {
	out := openapi.AssetAlternate{
		Id:          openapi_types.UUID(r.ID.Bytes),
		AssetId:     openapi_types.UUID(r.AssetID.Bytes),
		Label:       r.Label,
		Kind:        r.Kind,
		ContentType: r.ContentType,
		SizeBytes:   r.SizeBytes,
		CreatedAt:   r.CreatedAt.Time,
		Metadata:    map[string]any{},
	}
	if r.OriginServerID.Valid {
		v := openapi_types.UUID(r.OriginServerID.Bytes)
		out.OriginServerId = &v
	}
	if r.CreatedByUserRef != nil {
		v := *r.CreatedByUserRef
		out.CreatedByUserRef = &v
	}
	// Metadata round-trip — the column is JSONB so the row brings
	// back raw bytes. On parse failure we surface an empty object
	// rather than tank the list endpoint.
	if len(r.Metadata) > 0 {
		var asMap map[string]any
		if err := json.Unmarshal(r.Metadata, &asMap); err == nil {
			out.Metadata = asMap
		}
	}
	return out
}
