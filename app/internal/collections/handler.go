// Package collections implements the collection slice of the
// artist-alley HTTP API, per ADR 0009.
//
// A collection is a UUID-keyed, federation-prepared grouping of
// assets. The 1.11.A surface ships manual membership (an explicit
// list of assets) and the CRUD endpoints around it; query-membership
// and per-user grants/access-links arrive in 1.11.B/C.
//
// Reads through GetCollection are cached via an in-process LRU
// fronted by Postgres LISTEN/NOTIFY (cache.Registry) so peers see
// invalidations from any writer — including PHP, once a trigger is
// wired up alongside RS's collection table.
package collections

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// maxListLimit caps the per-page row count regardless of what the
// caller requests. Matches the assets list ceiling.
const maxListLimit = 200

// cacheDomainCollectionByID is the stable NOTIFY domain string for
// per-collection cache entries. Peer instances key off this when
// dispatching invalidations.
const cacheDomainCollectionByID = "collection.id"

// Handler implements the collections slice of openapi.StrictServerInterface.
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// byID caches collection rows by UUID string. The reads through
	// GetCollection hit this first; writes (Create/Update/Delete/
	// AddResource/RemoveResource) call invalidate which drops the
	// local copy AND broadcasts to peers. Nil-safe: a handler built
	// without a registry skips the cache.
	byID *cache.Cache[Collection]
}

// NewHandler binds the collections handler to the DB pool and the
// shared cache registry. Passing a nil registry is legal — the
// handler falls back to direct DB reads (useful in tests that
// don't want to spin up the LISTEN goroutine).
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger}
	if registry != nil {
		// 5000 collection entries is generous: typical installs have
		// dozens, federation-heavy deployments hundreds. Eviction
		// loses cold rows; next read repopulates.
		h.byID = cache.Register[Collection](registry, cacheDomainCollectionByID, 5000)
	}
	return h
}

// ---------------------------------------------------------------------------
// CreateCollection
// ---------------------------------------------------------------------------

func (h *Handler) CreateCollection(
	ctx context.Context,
	req openapi.CreateCollectionRequestObject,
) (openapi.CreateCollectionResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreateCollection401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreateCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return openapi.CreateCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "name is required"},
		}, nil
	}

	visibility := "private"
	if in.Visibility != nil {
		visibility = string(*in.Visibility)
	}
	membership := "manual"
	if in.Membership != nil {
		membership = string(*in.Membership)
	}
	// Phase 1.11.A only supports manual; query/hybrid arrive once the
	// search DSL (ADR 0010) is in place.
	if membership != "manual" {
		return openapi.CreateCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "only membership=manual is supported in this release"},
		}, nil
	}

	row, err := New(h.Pool).CreateCollection(ctx, CreateCollectionParams{
		OwnerUserRef: id.UserRef,
		Name:         name,
		Description:  strOr(in.Description, ""),
		Visibility:   visibility,
		Membership:   membership,
		ExpiresAt:    pgTimestamptzFromPtr(in.ExpiresAt),
		Featured:     boolOr(in.Featured, false),
		Purpose:      in.Purpose,
	})
	if err != nil {
		return nil, fmt.Errorf("collections: create: %w", err)
	}
	h.cacheAdd(row)
	return openapi.CreateCollection201JSONResponse(rowToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// GetCollection
// ---------------------------------------------------------------------------

func (h *Handler) GetCollection(
	ctx context.Context,
	req openapi.GetCollectionRequestObject,
) (openapi.GetCollectionResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetCollection401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	row, err := h.getByIDCached(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.GetCollection200JSONResponse(rowToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// UpdateCollection
// ---------------------------------------------------------------------------

func (h *Handler) UpdateCollection(
	ctx context.Context,
	req openapi.UpdateCollectionRequestObject,
) (openapi.UpdateCollectionResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UpdateCollection401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	// Load current row first to enforce ownership and to give
	// ClearCollectionExpiresAt the right id.
	q := New(h.Pool)
	cur, err := q.GetCollection(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutateCollection(caller, cur) {
		return openapi.UpdateCollection403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the owner of this collection"},
		}, nil
	}

	in := req.Body
	var namePtr *string
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			return openapi.UpdateCollection400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "name cannot be empty"},
			}, nil
		}
		namePtr = &n
	}
	var visPtr *string
	if in.Visibility != nil {
		s := string(*in.Visibility)
		visPtr = &s
	}
	var memPtr *string
	if in.Membership != nil {
		s := string(*in.Membership)
		if s != "manual" {
			return openapi.UpdateCollection400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "only membership=manual is supported in this release"},
			}, nil
		}
		memPtr = &s
	}

	// expires_at is a tri-state: omitted = keep; non-nil = set;
	// explicit null = clear. The generated CollectionUpdate struct
	// uses `omitempty` so we can't distinguish "absent" from "null"
	// via the Go struct alone — the convention here is that a
	// non-nil pointer means "set to this", and clearing the TTL
	// goes through the dedicated query.
	row, err := q.UpdateCollection(ctx, UpdateCollectionParams{
		ID:          pgID,
		Name:        namePtr,
		Description: in.Description,
		Visibility:  visPtr,
		Membership:  memPtr,
		Featured:    in.Featured,
		Purpose:     in.Purpose,
		ExpiresAt:   pgTimestamptzFromPtr(in.ExpiresAt),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, fmt.Errorf("collections: update: %w", err)
	}
	h.cacheAdd(row)
	return openapi.UpdateCollection200JSONResponse(rowToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// DeleteCollection
// ---------------------------------------------------------------------------

func (h *Handler) DeleteCollection(
	ctx context.Context,
	req openapi.DeleteCollectionRequestObject,
) (openapi.DeleteCollectionResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.DeleteCollection401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetCollection(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DeleteCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutateCollection(caller, cur) {
		return openapi.DeleteCollection403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the owner of this collection"},
		}, nil
	}
	if err := q.DeleteCollection(ctx, pgID); err != nil {
		return nil, fmt.Errorf("collections: delete: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.DeleteCollection204Response{}, nil
}

// ---------------------------------------------------------------------------
// ListCollections
// ---------------------------------------------------------------------------

func (h *Handler) ListCollections(
	ctx context.Context,
	req openapi.ListCollectionsRequestObject,
) (openapi.ListCollectionsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListCollections401JSONResponse{
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
			return openapi.ListCollections500JSONResponse{
				InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: id, Valid: true}
	}

	var visPtr *string
	if req.Params.Visibility != nil {
		s := string(*req.Params.Visibility)
		visPtr = &s
	}

	// Fetch limit+1 to know whether there's a next page without a
	// separate COUNT.
	fetch := limit + 1
	rows, err := New(h.Pool).ListCollectionsPage(ctx, ListCollectionsPageParams{
		OwnerUserRef:    req.Params.OwnerRef,
		Visibility:      visPtr,
		Featured:        req.Params.Featured,
		CursorCreatedAt: cursorTs,
		CursorID:        cursorID,
		RowLimit:        fetch,
	})
	if err != nil {
		return nil, fmt.Errorf("collections: list: %w", err)
	}

	items := make([]openapi.Collection, 0, limit)
	var lastCreatedAt time.Time
	var lastID uuid.UUID
	for i, r := range rows {
		if i >= int(limit) {
			break
		}
		items = append(items, rowToAPI(r))
		lastCreatedAt = r.CreatedAt.Time
		lastID = uuid.UUID(r.ID.Bytes)
	}
	resp := openapi.CollectionList{Items: items}
	if len(rows) > int(limit) {
		next := encodeCursor(lastCreatedAt, lastID)
		resp.NextCursor = &next
	}
	return openapi.ListCollections200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// ListCollectionResources
// ---------------------------------------------------------------------------

func (h *Handler) ListCollectionResources(
	ctx context.Context,
	req openapi.ListCollectionResourcesRequestObject,
) (openapi.ListCollectionResourcesResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListCollectionResources401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if _, err := h.getByIDCached(ctx, pgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListCollectionResources404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
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

	var cursorSort *int32
	var cursorAdded pgtype.Timestamptz
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		so, ts, err := decodeResourceCursor(*req.Params.Cursor)
		if err != nil {
			return nil, fmt.Errorf("collections: invalid resource cursor")
		}
		cursorSort = &so
		cursorAdded = pgtype.Timestamptz{Time: ts, Valid: true}
	}

	fetch := limit + 1
	rows, err := New(h.Pool).ListCollectionResourcesPage(ctx, ListCollectionResourcesPageParams{
		CollectionID:    pgID,
		CursorSortOrder: cursorSort,
		CursorAddedAt:   cursorAdded,
		RowLimit:        fetch,
	})
	if err != nil {
		return nil, fmt.Errorf("collections: list resources: %w", err)
	}

	items := make([]openapi.CollectionResource, 0, limit)
	var lastSort int32
	var lastAdded time.Time
	for i, r := range rows {
		if i >= int(limit) {
			break
		}
		items = append(items, resourceRowToAPI(r))
		lastSort = r.SortOrder
		lastAdded = r.AddedAt.Time
	}
	resp := openapi.CollectionResourceList{Items: items}
	if len(rows) > int(limit) {
		next := encodeResourceCursor(lastSort, lastAdded)
		resp.NextCursor = &next
	}
	return openapi.ListCollectionResources200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// AddCollectionResource
// ---------------------------------------------------------------------------

func (h *Handler) AddCollectionResource(
	ctx context.Context,
	req openapi.AddCollectionResourceRequestObject,
) (openapi.AddCollectionResourceResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddCollectionResource401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddCollectionResource400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetCollection(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddCollectionResource404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutateCollection(caller, cur) {
		return openapi.AddCollectionResource403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the owner of this collection"},
		}, nil
	}

	in := req.Body
	pgAsset := pgtype.UUID{Bytes: uuid.UUID(in.AssetId), Valid: true}
	err = q.AddCollectionResource(ctx, AddCollectionResourceParams{
		CollectionID: pgID,
		AssetID:      pgAsset,
		SortOrder:    int32Or(in.SortOrder, 0),
		Pinned:       boolOr(in.Pinned, true),
		ExpiresAt:    pgTimestamptzFromPtr(in.ExpiresAt),
	})
	if err != nil {
		// FK violation on asset_id surfaces as a friendly 404 rather
		// than a 500 — the caller is more likely to recognise their
		// own bad input than our table layout.
		if strings.Contains(err.Error(), "collection_resources_asset_id_fkey") {
			return openapi.AddCollectionResource404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("collections: add resource: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.AddCollectionResource204Response{}, nil
}

// ---------------------------------------------------------------------------
// RemoveCollectionResource
// ---------------------------------------------------------------------------

func (h *Handler) RemoveCollectionResource(
	ctx context.Context,
	req openapi.RemoveCollectionResourceRequestObject,
) (openapi.RemoveCollectionResourceResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemoveCollectionResource401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetCollection(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RemoveCollectionResource404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutateCollection(caller, cur) {
		return openapi.RemoveCollectionResource403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the owner of this collection"},
		}, nil
	}
	if err := q.RemoveCollectionResource(ctx, RemoveCollectionResourceParams{
		CollectionID: pgID,
		AssetID:      pgtype.UUID{Bytes: uuid.UUID(req.AssetId), Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("collections: remove resource: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.RemoveCollectionResource204Response{}, nil
}

// ---------------------------------------------------------------------------
// Cache helpers
// ---------------------------------------------------------------------------

func (h *Handler) getByIDCached(ctx context.Context, id pgtype.UUID) (Collection, error) {
	key := uuidString(id)
	if h.byID != nil {
		if v, ok := h.byID.Get(key); ok {
			return v, nil
		}
	}
	row, err := New(h.Pool).GetCollection(ctx, id)
	if err != nil {
		return row, err
	}
	if h.byID != nil {
		h.byID.Add(key, row)
	}
	return row, nil
}

func (h *Handler) cacheAdd(row Collection) {
	if h.byID == nil {
		return
	}
	h.byID.Add(uuidString(row.ID), row)
}

func (h *Handler) cacheInvalidate(ctx context.Context, id pgtype.UUID) {
	if h.byID == nil {
		return
	}
	if err := h.byID.Invalidate(ctx, uuidString(id)); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "collections.cache.invalidate.error",
			slog.String("domain", cacheDomainCollectionByID),
			slog.String("key", uuidString(id)),
			slog.String("err", err.Error()),
		)
	}
}

// ---------------------------------------------------------------------------
// Permission model
// ---------------------------------------------------------------------------

// CapCollectionsAdmin lets the holder mutate any collection, not just
// their own. system.admin also grants this implicitly.
const (
	CapCollectionsAdmin = "collections.admin"
	CapSystemAdmin      = "system.admin"
)

// canMutateCollection returns true when the caller owns the row or
// carries an override capability. The PHP layer still owns shared
// collection permissions through `collection_grants`; that table
// arrives in Phase 1.11.C.
func canMutateCollection(id *auth.Identity, row Collection) bool {
	if id == nil {
		return false
	}
	if row.OwnerUserRef == id.UserRef {
		return true
	}
	return id.Can(CapCollectionsAdmin) || id.Can(CapSystemAdmin)
}

// ---------------------------------------------------------------------------
// Conversions
// ---------------------------------------------------------------------------

func rowToAPI(r Collection) openapi.Collection {
	c := openapi.Collection{
		Id:           openapi_types.UUID(r.ID.Bytes),
		OwnerUserRef: r.OwnerUserRef,
		Name:         r.Name,
		Description:  r.Description,
		Visibility:   openapi.CollectionVisibility(r.Visibility),
		Membership:   openapi.CollectionMembership(r.Membership),
		Featured:     r.Featured,
		Purpose:      r.Purpose,
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
	if r.ExpiresAt.Valid {
		t := r.ExpiresAt.Time
		c.ExpiresAt = &t
	}
	if r.OriginServerID.Valid {
		v := openapi_types.UUID(r.OriginServerID.Bytes)
		c.OriginServerId = &v
	}
	return c
}

func resourceRowToAPI(r ListCollectionResourcesPageRow) openapi.CollectionResource {
	out := openapi.CollectionResource{
		CollectionId: openapi_types.UUID(r.CollectionID.Bytes),
		AssetId:      openapi_types.UUID(r.AssetID.Bytes),
		SortOrder:    int(r.SortOrder),
		Pinned:       r.Pinned,
		AddedAt:      r.AddedAt.Time,
		Title:        r.Title,
		ResourceType: r.ResourceType,
		Status:       openapi.CollectionResourceStatus(r.Status),
		FileHash:     r.FileHash,
	}
	if r.ExpiresAt.Valid {
		t := r.ExpiresAt.Time
		out.ExpiresAt = &t
	}
	if r.AssetCreatedAt.Valid {
		t := r.AssetCreatedAt.Time
		out.AssetCreatedAt = &t
	}
	return out
}

// ---------------------------------------------------------------------------
// Cursors
// ---------------------------------------------------------------------------

// encodeCursor builds the opaque pagination token for the collections
// list (created_at DESC, id DESC). RFC3339-nano + "|" + uuid, base64-
// url encoded — matches the assets list shape so clients can treat
// both the same way.
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

// encodeResourceCursor for collection_resources pagination
// (sort_order ASC, added_at ASC).
func encodeResourceCursor(sortOrder int32, addedAt time.Time) string {
	raw := fmt.Sprintf("%d|%s", sortOrder, addedAt.UTC().Format(time.RFC3339Nano))
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeResourceCursor(s string) (int32, time.Time, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, time.Time{}, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return 0, time.Time{}, errors.New("bad cursor shape")
	}
	var so int32
	if _, err := fmt.Sscanf(parts[0], "%d", &so); err != nil {
		return 0, time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return 0, time.Time{}, err
	}
	return so, t, nil
}

// ---------------------------------------------------------------------------
// Small option-extraction helpers — keep the handler bodies tidy.
// ---------------------------------------------------------------------------

func uuidString(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func int32Or(p *int, def int32) int32 {
	if p == nil {
		return def
	}
	return int32(*p)
}

func pgTimestamptzFromPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// ---------------------------------------------------------------------------
// Compile-time assertion: catches openapi-codegen signature drift.
// ---------------------------------------------------------------------------

var _ interface {
	ListCollections(context.Context, openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error)
	CreateCollection(context.Context, openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error)
	GetCollection(context.Context, openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error)
	UpdateCollection(context.Context, openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error)
	DeleteCollection(context.Context, openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error)
	ListCollectionResources(context.Context, openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error)
	AddCollectionResource(context.Context, openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error)
	RemoveCollectionResource(context.Context, openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error)
} = (*Handler)(nil)
