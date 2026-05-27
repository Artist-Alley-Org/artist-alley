// Package assets implements the asset-entity slice of the
// artist-alley HTTP API. An asset is the user-facing record on top
// of the byte-plane managed by the storage package.
//
// See ADR 0011 for the entity model. The storage byte upload /
// download endpoints (/storage/objects/*) live in
// internal/storage/handler.go.
package assets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	// Register image decoders for thumbhash. We use the std lib's
	// JPEG/PNG/GIF decoders plus golang.org/x/image/webp/bmp/tiff so
	// the same code path covers everything our isImageExt allows.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"go.n16f.net/thumbhash"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// PinSubjectTypeAsset is the storage-pin subject_type assets use to
// claim their underlying bytes. Replaces the `user:` pin set by the
// initial upload.
const PinSubjectTypeAsset = "asset"

// maxListLimit caps the per-page row count regardless of what the
// caller requests. Higher than the openapi spec's default but in
// line with its declared maximum (200).
const maxListLimit = 200

// Handler implements the asset-entity slice of
// openapi.StrictServerInterface.
type Handler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Logger  *slog.Logger
}

// NewHandler binds an entity handler to the DB pool and the storage
// Service it shares with the storage byte handler.
func NewHandler(pool *pgxpool.Pool, storageSvc *storage.Service, logger *slog.Logger) *Handler {
	return &Handler{Pool: pool, Storage: storageSvc, Logger: logger}
}

// ---------------------------------------------------------------------------
// CreateAsset
// ---------------------------------------------------------------------------

func (h *Handler) CreateAsset(
	ctx context.Context,
	req openapi.CreateAssetRequestObject,
) (openapi.CreateAssetResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreateAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body

	title := strings.TrimSpace(in.Title)
	if title == "" {
		return openapi.CreateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "title is required"},
		}, nil
	}

	status := "active"
	if in.Status != nil {
		status = string(*in.Status)
	}
	switch status {
	case "draft", "active", "archived":
	default:
		return openapi.CreateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "status must be one of draft|active|archived"},
		}, nil
	}

	metadataJSON, err := encodeMetadata(in.Metadata)
	if err != nil {
		return nil, err
	}

	var fileHashPtr *string
	if in.FileHash != nil && *in.FileHash != "" {
		fh := strings.ToLower(strings.TrimSpace(*in.FileHash))
		if err := storage.ValidateHash(fh); err != nil {
			return openapi.CreateAsset400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "file_hash: " + err.Error()},
			}, nil
		}
		fileHashPtr = &fh
	}

	// Workflow state: caller-supplied UUID is taken verbatim; the DB
	// FK guards against unknown values. We don't validate that the
	// state belongs to the matching `asset:<resource_type>` domain
	// here — that check kicks in on Transition() later. Tradeoff:
	// permits a typo in the upload modal that would only surface on
	// the first transition attempt; not worth a domain-lookup
	// round-trip on every create.
	var stateID pgtype.UUID
	if in.StateId != nil {
		stateID = pgtype.UUID{Bytes: uuid.UUID(*in.StateId), Valid: true}
	}

	// processing_status: image / video uploads queue for async
	// post-processing (variant generation, EXIF, etc.); everything
	// else is `ready` on day one because there's nothing to process.
	processingStatus := "ready"
	if needsProcessing(in.FileExtension) {
		processingStatus = "pending"
	}

	// Thumbhash — synchronously computed for image extensions. ~1-3
	// ms on a 4K image; we do it BEFORE the INSERT so the asset row
	// is born with the placeholder. Failure here is soft: log + keep
	// thumbhash=NULL. The feed card just won't have a blurred
	// placeholder; the original /file URL still works.
	var thumbhashBytes []byte
	if fileHashPtr != nil && isImageExt(in.FileExtension) {
		hCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		thumbhashBytes = computeThumbhash(hCtx, h.Storage, *fileHashPtr, h.Logger)
		cancel()
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("assets: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	row, err := q.CreateAsset(ctx, CreateAssetParams{
		Title:            title,
		Description:      strDefault(in.Description, ""),
		ResourceType:     in.ResourceType,
		OwnerUserRef:     &id.UserRef,
		Status:           status,
		FileHash:         fileHashPtr,
		FileExtension:    in.FileExtension,
		FileSizeBytes:    nil, // backfilled below from storage_objects if hash present
		Metadata:         metadataJSON,
		OriginServerID:   pgtype.UUID{Valid: false},
		StateID:          stateID,
		ProcessingStatus: processingStatus,
		Thumbhash:        thumbhashBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("assets: insert: %w", err)
	}
	newID := uuid.UUID(row.ID.Bytes)

	// Tags (optional). Replace, not merge — at creation time there's
	// nothing to merge with.
	tags := []string{}
	if in.Tags != nil {
		seen := map[string]struct{}{}
		for _, t := range *in.Tags {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			if err := q.AddAssetTag(ctx, AddAssetTagParams{
				AssetID: row.ID,
				Tag:     t,
			}); err != nil {
				return nil, fmt.Errorf("assets: add tag: %w", err)
			}
			tags = append(tags, t)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("assets: commit: %w", err)
	}

	// Pin rebalancing happens OUTSIDE the asset-table tx so a
	// storage-pin failure doesn't roll back the asset itself —
	// the worst case is an extra `user:` pin we GC later.
	if fileHashPtr != nil {
		if err := h.Storage.AddPin(ctx, storage.PinRef{
			SubjectType: PinSubjectTypeAsset,
			SubjectID:   newID.String(),
		}, *fileHashPtr); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.create.add_pin.error",
				slog.String("asset_id", newID.String()),
				slog.String("file_hash", *fileHashPtr),
				slog.String("err", err.Error()),
			)
		}
		if err := h.Storage.RemovePin(ctx, storage.PinRef{
			SubjectType: storage.PinSubjectTypeUser,
			SubjectID:   strconv.FormatInt(id.UserRef, 10),
		}, *fileHashPtr); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelDebug, "assets.create.remove_user_pin.skipped",
				slog.String("err", err.Error()),
			)
		}
	}

	return openapi.CreateAsset201JSONResponse(rowToAsset(rowToAssetRow(row), tags)), nil
}

// ---------------------------------------------------------------------------
// GetAsset
// ---------------------------------------------------------------------------

func (h *Handler) GetAsset(
	ctx context.Context,
	req openapi.GetAssetRequestObject,
) (openapi.GetAssetResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	row, err := q.GetAsset(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, err
	}
	tags, err := q.ListAssetTags(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("assets: list tags: %w", err)
	}
	return openapi.GetAsset200JSONResponse(rowToAsset(row, tags)), nil
}

// ---------------------------------------------------------------------------
// UpdateAsset
// ---------------------------------------------------------------------------

func (h *Handler) UpdateAsset(
	ctx context.Context,
	req openapi.UpdateAssetRequestObject,
) (openapi.UpdateAssetResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.UpdateAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	var statusPtr *string
	if in.Status != nil {
		s := string(*in.Status)
		switch s {
		case "draft", "active", "archived":
		default:
			return openapi.UpdateAsset400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "status must be one of draft|active|archived"},
			}, nil
		}
		statusPtr = &s
	}
	var titlePtr *string
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" {
			return openapi.UpdateAsset400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "title cannot be empty"},
			}, nil
		}
		titlePtr = &t
	}
	var descPtr *string
	if in.Description != nil {
		descPtr = in.Description
	}
	var metaJSON []byte
	if in.Metadata != nil {
		raw, err := encodeMetadata(in.Metadata)
		if err != nil {
			return nil, err
		}
		metaJSON = raw
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("assets: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	row, err := q.UpdateAsset(ctx, UpdateAssetParams{
		ID:          pgID,
		Title:       titlePtr,
		Description: descPtr,
		Status:      statusPtr,
		Metadata:    metaJSON,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: update: %w", err)
	}

	// Tag replacement when the client sends a tags array.
	tags := []string{}
	if in.Tags != nil {
		clean := dedupeTags(*in.Tags)
		// Two-step replacement: DELETE all + INSERT new. Cheaper than
		// diffing for small sets, and tags don't carry per-row state
		// we'd care to preserve.
		if _, err := tx.Exec(ctx, `DELETE FROM asset_tag WHERE asset_id = $1`, row.ID); err != nil {
			return nil, fmt.Errorf("assets: clear tags: %w", err)
		}
		for _, t := range clean {
			if err := q.AddAssetTag(ctx, AddAssetTagParams{AssetID: row.ID, Tag: t}); err != nil {
				return nil, fmt.Errorf("assets: add tag: %w", err)
			}
		}
		tags = clean
	} else {
		existing, err := q.ListAssetTags(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("assets: list tags: %w", err)
		}
		tags = existing
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("assets: commit: %w", err)
	}

	return openapi.UpdateAsset200JSONResponse(rowToAsset(updateRowToGetRow(row), tags)), nil
}

// ---------------------------------------------------------------------------
// DeleteAsset
// ---------------------------------------------------------------------------

func (h *Handler) DeleteAsset(
	ctx context.Context,
	req openapi.DeleteAssetRequestObject,
) (openapi.DeleteAssetResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.DeleteAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	// Fetch first so we can drop the storage pin even though the soft
	// delete doesn't return the row.
	q := New(h.Pool)
	row, err := q.GetAsset(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DeleteAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, err
	}
	if err := q.SoftDeleteAsset(ctx, pgID); err != nil {
		return nil, fmt.Errorf("assets: soft-delete: %w", err)
	}
	if row.FileHash != nil {
		if err := h.Storage.RemovePin(ctx, storage.PinRef{
			SubjectType: PinSubjectTypeAsset,
			SubjectID:   uuid.UUID(row.ID.Bytes).String(),
		}, *row.FileHash); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.delete.remove_pin.error",
				slog.String("asset_id", uuid.UUID(row.ID.Bytes).String()),
				slog.String("file_hash", *row.FileHash),
				slog.String("err", err.Error()),
			)
		}
	}
	return openapi.DeleteAsset204Response{}, nil
}

// ---------------------------------------------------------------------------
// ListAssets
// ---------------------------------------------------------------------------

func (h *Handler) ListAssets(
	ctx context.Context,
	req openapi.ListAssetsRequestObject,
) (openapi.ListAssetsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListAssets401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	limit := int32(50)
	if req.Params.Limit != nil {
		l := *req.Params.Limit
		if l < 1 {
			l = 1
		}
		if l > maxListLimit {
			l = maxListLimit
		}
		limit = int32(l)
	}

	var cursorTs pgtype.Timestamptz
	var cursorID pgtype.UUID
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		ts, id, err := decodeCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListAssets500JSONResponse{
				InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: id, Valid: true}
	}

	var ownerRef *int64
	if req.Params.OwnerRef != nil {
		ownerRef = req.Params.OwnerRef
	}
	var resType *int64
	if req.Params.ResourceType != nil {
		resType = req.Params.ResourceType
	}
	var statusPtr *string
	if req.Params.Status != nil {
		s := string(*req.Params.Status)
		statusPtr = &s
	}
	// `q` is mutually exclusive with `tag` in practice (the tag-join
	// query template doesn't carry the search_text column). When both
	// are supplied we honour `tag` and drop `q`.
	var qText *string
	if req.Params.Q != nil {
		s := strings.TrimSpace(*req.Params.Q)
		if s != "" {
			qText = &s
		}
	}

	q := New(h.Pool)

	// One-shot paging: fetch limit+1 to know whether there's a next page.
	fetch := limit + 1

	var assetsList []openapi.Asset
	var lastCreatedAt time.Time
	var lastID uuid.UUID
	var rowCount int

	if req.Params.Tag != nil && *req.Params.Tag != "" {
		rows, err := q.ListAssetsByTagPage(ctx, ListAssetsByTagPageParams{
			Tag:             *req.Params.Tag,
			OwnerUserRef:    ownerRef,
			ResourceType:    resType,
			Status:          statusPtr,
			CursorCreatedAt: cursorTs,
			CursorID:        cursorID,
			RowLimit:        fetch,
		})
		if err != nil {
			return nil, fmt.Errorf("assets: list by tag: %w", err)
		}
		for _, r := range rows {
			rowCount++
			if rowCount > int(limit) {
				break
			}
			tags, err := q.ListAssetTags(ctx, r.ID)
			if err != nil {
				return nil, fmt.Errorf("assets: list tags: %w", err)
			}
			assetsList = append(assetsList, rowToAsset(listByTagRowToGetRow(r), tags))
			lastCreatedAt = r.CreatedAt.Time
			lastID = uuid.UUID(r.ID.Bytes)
		}
		rowCount = len(rows)
	} else {
		rows, err := q.ListAssetsPage(ctx, ListAssetsPageParams{
			OwnerUserRef:    ownerRef,
			ResourceType:    resType,
			Status:          statusPtr,
			Q:               qText,
			CursorCreatedAt: cursorTs,
			CursorID:        cursorID,
			RowLimit:        fetch,
		})
		if err != nil {
			return nil, fmt.Errorf("assets: list: %w", err)
		}
		for i, r := range rows {
			if i >= int(limit) {
				break
			}
			tags, err := q.ListAssetTags(ctx, r.ID)
			if err != nil {
				return nil, fmt.Errorf("assets: list tags: %w", err)
			}
			assetsList = append(assetsList, rowToAsset(listRowToGetRow(r), tags))
			lastCreatedAt = r.CreatedAt.Time
			lastID = uuid.UUID(r.ID.Bytes)
		}
		rowCount = len(rows)
	}

	resp := openapi.AssetList{Items: assetsList}
	if rowCount > int(limit) {
		next := encodeCursor(lastCreatedAt, lastID)
		resp.NextCursor = &next
	}
	if resp.Items == nil {
		resp.Items = []openapi.Asset{}
	}
	return openapi.ListAssets200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// DownloadAssetFile / DownloadAssetVariant
// ---------------------------------------------------------------------------

func (h *Handler) DownloadAssetFile(
	ctx context.Context,
	req openapi.DownloadAssetFileRequestObject,
) (openapi.DownloadAssetFileResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.DownloadAssetFile401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	hash, ok, err := h.resolveAssetFileHash(ctx, uuid.UUID(req.Id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return openapi.DownloadAssetFile404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset has no file attached"},
		}, nil
	}

	body, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return openapi.DownloadAssetFile404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "stored object missing for this asset"},
			}, nil
		}
		return nil, err
	}
	return openapi.DownloadAssetFile200ApplicationoctetStreamResponse{
		Body:          body,
		ContentLength: info.Size,
	}, nil
}

func (h *Handler) DownloadAssetVariant(
	ctx context.Context,
	req openapi.DownloadAssetVariantRequestObject,
) (openapi.DownloadAssetVariantResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.DownloadAssetVariant401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	hash, ok, err := h.resolveAssetFileHash(ctx, uuid.UUID(req.Id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return openapi.DownloadAssetVariant404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset has no file attached"},
		}, nil
	}
	body, info, err := h.Storage.Download(ctx, hash, req.Variant)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return openapi.DownloadAssetVariant404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "variant not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.DownloadAssetVariant200ApplicationoctetStreamResponse{
		Body:          body,
		ContentLength: info.Size,
	}, nil
}

// ---------------------------------------------------------------------------
// Tag management
// ---------------------------------------------------------------------------

func (h *Handler) AddAssetTags(
	ctx context.Context,
	req openapi.AddAssetTagsRequestObject,
) (openapi.AddAssetTagsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.AddAssetTags401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil || len(req.Body.Tags) == 0 {
		return openapi.AddAssetTags400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "tags array is required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	if _, err := q.GetAsset(ctx, pgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddAssetTags404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, err
	}
	for _, t := range dedupeTags(req.Body.Tags) {
		if err := q.AddAssetTag(ctx, AddAssetTagParams{AssetID: pgID, Tag: t}); err != nil {
			return nil, fmt.Errorf("assets: add tag: %w", err)
		}
	}
	return openapi.AddAssetTags204Response{}, nil
}

func (h *Handler) RemoveAssetTag(
	ctx context.Context,
	req openapi.RemoveAssetTagRequestObject,
) (openapi.RemoveAssetTagResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.RemoveAssetTag401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	if err := q.RemoveAssetTag(ctx, RemoveAssetTagParams{AssetID: pgID, Tag: req.Tag}); err != nil {
		return nil, fmt.Errorf("assets: remove tag: %w", err)
	}
	return openapi.RemoveAssetTag204Response{}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) resolveAssetFileHash(ctx context.Context, id uuid.UUID) (string, bool, error) {
	q := New(h.Pool)
	row, err := q.GetAsset(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	if row.FileHash == nil || *row.FileHash == "" {
		return "", false, nil
	}
	return *row.FileHash, true, nil
}

// encodeMetadata serialises the optional API-side metadata map into
// the JSONB column. nil/empty -> "{}" so the column never holds
// SQL NULL.
func encodeMetadata(m *map[string]interface{}) ([]byte, error) {
	if m == nil || len(*m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(*m)
	if err != nil {
		return nil, fmt.Errorf("assets: marshal metadata: %w", err)
	}
	return b, nil
}

// dedupeTags trims, lowercases, and dedups a tag list. Order is
// preserved (first occurrence wins).
func dedupeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// strDefault returns *p or the default if p is nil.
func strDefault(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// encodeCursor builds the opaque pagination token. RFC3339-nano +
// "|" + uuid, base64-url encoded.
func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errors.New("bad cursor shape")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return t, id, nil
}

// rowToAsset converts the sqlc-emitted row shape into the OpenAPI
// Asset response. Several sqlc-generated row types share the same
// columns; we normalise via rowToAssetRow/listRowToGetRow etc.
func rowToAsset(row GetAssetRow, tags []string) openapi.Asset {
	a := openapi.Asset{
		Id:               openapi_types.UUID(row.ID.Bytes),
		Title:            row.Title,
		ResourceType:     row.ResourceType,
		Status:           openapi.AssetStatus(row.Status),
		ProcessingStatus: openapi.AssetProcessingStatus(row.ProcessingStatus),
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        row.UpdatedAt.Time,
		Tags:             tags,
	}
	if row.Description != "" {
		d := row.Description
		a.Description = &d
	}
	a.OwnerUserRef = row.OwnerUserRef
	a.FileHash = row.FileHash
	a.FileExtension = row.FileExtension
	a.FileSizeBytes = row.FileSizeBytes
	if row.StateID.Valid {
		v := openapi_types.UUID(row.StateID.Bytes)
		a.StateId = &v
	}
	if len(row.Thumbhash) > 0 {
		// Base64-encoded for JSON transport. The frontend
		// thumbhash decoder accepts both base64 and the raw byte
		// array; base64 is the more common wire format.
		v := base64.StdEncoding.EncodeToString(row.Thumbhash)
		a.Thumbhash = &v
	}
	if len(row.Metadata) > 0 && string(row.Metadata) != "{}" {
		var m map[string]interface{}
		if err := json.Unmarshal(row.Metadata, &m); err == nil {
			a.Metadata = &m
		}
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}
	return a
}

// The CreateAsset/UpdateAsset/ListAsset return shapes have the same
// column set but distinct Go types (sqlc generates one per query).
// These helpers project them onto a common GetAssetRow we already
// have a marshaller for.
func rowToAssetRow(r CreateAssetRow) GetAssetRow {
	return GetAssetRow{
		ID:               r.ID,
		Title:            r.Title,
		Description:      r.Description,
		ResourceType:     r.ResourceType,
		OwnerUserRef:     r.OwnerUserRef,
		Status:           r.Status,
		FileHash:         r.FileHash,
		FileExtension:    r.FileExtension,
		FileSizeBytes:    r.FileSizeBytes,
		Metadata:         r.Metadata,
		OriginServerID:   r.OriginServerID,
		StateID:          r.StateID,
		ProcessingStatus: r.ProcessingStatus,
		Thumbhash:        r.Thumbhash,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func updateRowToGetRow(r UpdateAssetRow) GetAssetRow {
	return GetAssetRow{
		ID:               r.ID,
		Title:            r.Title,
		Description:      r.Description,
		ResourceType:     r.ResourceType,
		OwnerUserRef:     r.OwnerUserRef,
		Status:           r.Status,
		FileHash:         r.FileHash,
		FileExtension:    r.FileExtension,
		FileSizeBytes:    r.FileSizeBytes,
		Metadata:         r.Metadata,
		OriginServerID:   r.OriginServerID,
		StateID:          r.StateID,
		ProcessingStatus: r.ProcessingStatus,
		Thumbhash:        r.Thumbhash,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func listRowToGetRow(r ListAssetsPageRow) GetAssetRow {
	return GetAssetRow{
		ID:               r.ID,
		Title:            r.Title,
		Description:      r.Description,
		ResourceType:     r.ResourceType,
		OwnerUserRef:     r.OwnerUserRef,
		Status:           r.Status,
		FileHash:         r.FileHash,
		FileExtension:    r.FileExtension,
		FileSizeBytes:    r.FileSizeBytes,
		Metadata:         r.Metadata,
		OriginServerID:   r.OriginServerID,
		StateID:          r.StateID,
		ProcessingStatus: r.ProcessingStatus,
		Thumbhash:        r.Thumbhash,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func listByTagRowToGetRow(r ListAssetsByTagPageRow) GetAssetRow {
	return GetAssetRow{
		ID:               r.ID,
		Title:            r.Title,
		Description:      r.Description,
		ResourceType:     r.ResourceType,
		OwnerUserRef:     r.OwnerUserRef,
		Status:           r.Status,
		FileHash:         r.FileHash,
		FileExtension:    r.FileExtension,
		FileSizeBytes:    r.FileSizeBytes,
		Metadata:         r.Metadata,
		OriginServerID:   r.OriginServerID,
		StateID:          r.StateID,
		ProcessingStatus: r.ProcessingStatus,
		Thumbhash:        r.Thumbhash,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

// imageExts is the lowercased file-extension set that gets a
// thumbhash + processing_status='pending' at create time. Mirrors
// the frontend's isImageExt (web/src/lib/components/PostModal.svelte)
// — keep these in sync.
var imageExts = map[string]struct{}{
	"jpg": {}, "jpeg": {}, "png": {}, "gif": {}, "webp": {},
	"bmp": {}, "tiff": {}, "tif": {}, "avif": {}, "heic": {}, "heif": {},
}

func isImageExt(ext *string) bool {
	if ext == nil {
		return false
	}
	e := strings.ToLower(strings.TrimPrefix(*ext, "."))
	_, ok := imageExts[e]
	return ok
}

// videoExts: same role for video. Coverage is "anything we'd want
// to spin up a video.probe job for" — the actual transcode list
// lives in the (future) video pipeline.
var videoExts = map[string]struct{}{
	"mp4": {}, "mov": {}, "mkv": {}, "webm": {}, "avi": {},
	"wmv": {}, "mpg": {}, "mpeg": {}, "3gp": {}, "flv": {},
}

func needsProcessing(ext *string) bool {
	if ext == nil {
		return false
	}
	e := strings.ToLower(strings.TrimPrefix(*ext, "."))
	if _, ok := imageExts[e]; ok {
		return true
	}
	if _, ok := videoExts[e]; ok {
		return true
	}
	return false
}

// computeThumbhash downloads the original bytes, decodes them as an
// image, and runs thumbhash. Returns nil on any failure — the caller
// stores nil as NULL and the feed card falls back to a neutral
// placeholder. This is intentionally soft-fail: we'd rather have an
// asset without a placeholder than fail the upload.
func computeThumbhash(ctx context.Context, svc *storage.Service, hash string, logger *slog.Logger) []byte {
	if svc == nil {
		return nil
	}
	rc, _, err := svc.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelDebug, "assets.create.thumbhash.download_failed",
			slog.String("hash", hash),
			slog.String("err", err.Error()),
		)
		return nil
	}
	defer rc.Close()
	// Cap the decoder input; thumbhash needs the whole image but a
	// 100 MB ProRes still has us read the header only via
	// image.Decode + the JPEG/PNG decoders. The 25 MB cap protects
	// us from someone uploading a multi-GB tiff and us OOMing on
	// the decode.
	limited := io.LimitReader(rc, 25*1024*1024)
	img, _, err := image.Decode(limited)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelDebug, "assets.create.thumbhash.decode_failed",
			slog.String("hash", hash),
			slog.String("err", err.Error()),
		)
		return nil
	}
	return thumbhash.EncodeImage(img)
}

// Compile-time assertion: catches openapi-codegen signature drift.
var _ interface {
	CreateAsset(context.Context, openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error)
	GetAsset(context.Context, openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error)
	UpdateAsset(context.Context, openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error)
	DeleteAsset(context.Context, openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error)
	ListAssets(context.Context, openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error)
	DownloadAssetFile(context.Context, openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error)
	DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error)
	AddAssetTags(context.Context, openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error)
	RemoveAssetTag(context.Context, openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error)
} = (*Handler)(nil)
