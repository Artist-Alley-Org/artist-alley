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

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
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

	// Activities ledger writer + baseURL resolver (Phase 1.22.A-bis-4
	// per ADR 0044). When wired, the Create/Update/Delete/Add/Remove
	// collection endpoints route their domain writes through
	// h.activities.WithEmission so each emits a properly-shaped
	// AP activity. nil-safe pre-ADR-0044 fallback for tests.
	activities *activities.Writer
	baseURLFn  func(ctx context.Context) string
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

// SetActivitiesWriter installs the federation activity-ledger
// writer + baseURL resolver per ADR 0044. Mirrors the setter on
// posts.Handler / social.Handler / messages.Handler.
func (h *Handler) SetActivitiesWriter(w *activities.Writer, baseURLFn func(ctx context.Context) string) {
	h.activities = w
	h.baseURLFn = baseURLFn
}

// actorContext builds an emit.ActorContext for the authenticated
// caller from the configured baseURL.
func (h *Handler) actorContext(ctx context.Context, caller *auth.Identity) emit.ActorContext {
	if h.baseURLFn == nil {
		return emit.ActorContext{UserRef: caller.UserRef, Username: caller.Username}
	}
	return emit.ActorContext{
		UserRef:  caller.UserRef,
		Username: caller.Username,
		BaseURL:  h.baseURLFn(ctx),
	}
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

	// Gold-standard path: WithEmissionFn so we capture the
	// generated collection UUID and use it to build the activity's
	// URI in the same tx. 1.22.B-cleanup made activities required.
	if h.activities == nil {
		return nil, errCollectionsFederationNotWired
	}
	var saved Collection
	err := h.activities.WithEmissionFn(ctx, func(tx pgx.Tx) (activities.EmissionInput, error) {
		r, err := New(tx).CreateCollection(ctx, CreateCollectionParams{
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
			return activities.EmissionInput{}, fmt.Errorf("collections: create: %w", err)
		}
		saved = r
		em := emit.CreateCollection(h.actorContext(ctx, id), emit.CollectionRef{
			ID:          uuid.UUID(r.ID.Bytes).String(),
			Name:        r.Name,
			Description: r.Description,
			OwnerRef:    r.OwnerUserRef,
		})
		return activities.EmissionInput{Activity: em.Activity}, nil
	})
	if err != nil {
		return nil, err
	}
	h.cacheAdd(saved)
	return openapi.CreateCollection201JSONResponse(rowToAPI(saved)), nil
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
	// Gold-standard path: UpdateCollection + Update activity in
	// the same tx. WithEmissionFn so the post-write row drives
	// the activity payload. 1.22.B-cleanup made activities required.
	if h.activities == nil {
		return nil, errCollectionsFederationNotWired
	}
	var saved Collection
	errRun := h.activities.WithEmissionFn(ctx, func(tx pgx.Tx) (activities.EmissionInput, error) {
		r, err := New(tx).UpdateCollection(ctx, UpdateCollectionParams{
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
			return activities.EmissionInput{}, fmt.Errorf("collections: update: %w", err)
		}
		saved = r
		em := emit.UpdateCollection(h.actorContext(ctx, caller), emit.CollectionRef{
			ID:          uuid.UUID(r.ID.Bytes).String(),
			Name:        r.Name,
			Description: r.Description,
			OwnerRef:    r.OwnerUserRef,
		})
		return activities.EmissionInput{Activity: em.Activity}, nil
	})
	if errRun != nil {
		if errors.Is(errRun, pgx.ErrNoRows) {
			return openapi.UpdateCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, errRun
	}
	h.cacheAdd(saved)
	return openapi.UpdateCollection200JSONResponse(rowToAPI(saved)), nil
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
	// Gold-standard path: DeleteCollection + Delete activity in
	// one tx per AP §6.4 Tombstone semantics. 1.22.B-cleanup made
	// activities required.
	if h.activities == nil {
		return nil, errCollectionsFederationNotWired
	}
	em := emit.DeleteCollection(h.actorContext(ctx, caller), uuid.UUID(pgID.Bytes).String(), cur.Name)
	err = h.activities.WithEmission(ctx, activities.EmissionInput{
		Activity: em.Activity,
	}, func(tx pgx.Tx) error {
		return New(tx).DeleteCollection(ctx, pgID)
	})
	if err != nil {
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

	// Hub `tab` is a pre-canned filter that overrides owner/visibility/
	// featured. The mapping below mirrors the OpenAPI doc; an unknown
	// tab falls through to the no-tab branch (treated as "all").
	ownerPtr := req.Params.OwnerRef
	featuredPtr := req.Params.Featured
	var excludeOwnerPtr *int64
	var sharedWithPtr *int64
	caller := auth.IdentityFromContext(ctx)
	if req.Params.Tab != nil && caller != nil {
		// oapi-codegen drops the type-name prefix on enum constants
		// when there are no collisions — 'Public' / 'Shared' /
		// 'Featured' / 'All' / 'Mine' are now globally unique after
		// the 1.22.C-a visibility-enum cleanup, so the prefixed
		// ListCollectionsParamsTabXxx names are gone.
		switch *req.Params.Tab {
		case openapi.Mine:
			ownerPtr = &caller.UserRef
			visPtr = nil
			featuredPtr = nil
		case openapi.Featured:
			vis := "org-only"
			visPtr = &vis
			f := true
			featuredPtr = &f
			ownerPtr = nil
		case openapi.Public:
			// "Public" tab kept as the user-facing label but now
			// maps to org-only at the storage layer (1.22.C-a).
			vis := "org-only"
			visPtr = &vis
			ownerPtr = nil
			featuredPtr = nil
		case openapi.Shared:
			sharedWithPtr = &caller.UserRef
			excludeOwnerPtr = &caller.UserRef
			ownerPtr = nil
			visPtr = nil
			featuredPtr = nil
		case openapi.All:
			// no overrides — the listing already enforces visibility
			// at the row level via the existing filter.
		}
	}

	var qNamePtr *string
	if req.Params.Q != nil {
		q := strings.TrimSpace(*req.Params.Q)
		if q != "" {
			qNamePtr = &q
		}
	}

	// Fetch limit+1 to know whether there's a next page without a
	// separate COUNT.
	fetch := limit + 1
	rows, err := New(h.Pool).ListCollectionsPage(ctx, ListCollectionsPageParams{
		OwnerUserRef:    ownerPtr,
		ExcludeOwner:    excludeOwnerPtr,
		Visibility:      visPtr,
		Featured:        featuredPtr,
		QName:           qNamePtr,
		SharedWithUser:  sharedWithPtr,
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
	assetIDStr := uuid.UUID(in.AssetId).String()

	// Gold-standard path: Add(object=asset, target=collection)
	// per AP §6.6 / §7.8. 1.22.B-cleanup made activities required.
	if h.activities == nil {
		return nil, errCollectionsFederationNotWired
	}
	var fkAssetMissing bool
	em := emit.AddToCollection(
		h.actorContext(ctx, caller),
		activities.ObjectKindAsset,
		assetIDStr,
		uuid.UUID(pgID.Bytes).String(),
		cur.Name,
	)
	errRun := h.activities.WithEmission(ctx, activities.EmissionInput{
		Activity: em.Activity,
	}, func(tx pgx.Tx) error {
		err := New(tx).AddCollectionResource(ctx, AddCollectionResourceParams{
			CollectionID: pgID,
			AssetID:      pgAsset,
			SortOrder:    int32Or(in.SortOrder, 0),
			Pinned:       boolOr(in.Pinned, true),
			ExpiresAt:    pgTimestamptzFromPtr(in.ExpiresAt),
		})
		if err != nil && strings.Contains(err.Error(), "collection_resources_asset_id_fkey") {
			fkAssetMissing = true
			return errAssetMissing
		}
		return err
	})
	if fkAssetMissing {
		return openapi.AddCollectionResource404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}
	if errRun != nil {
		return nil, fmt.Errorf("collections: add resource: %w", errRun)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.AddCollectionResource204Response{}, nil
}

// errAssetMissing is the sentinel signalling FK-violation on
// asset_id inside the WithEmission closure. Used to roll back +
// return 404 without surfacing as a 500 server error.
var errAssetMissing = errors.New("collections: asset row absent")

// errCollectionsFederationNotWired surfaces in tests that forget
// to call SetActivitiesWriter on the handler. Production never
// sees it: api.go always wires the writer at boot.
var errCollectionsFederationNotWired = errors.New("collections: activities.Writer not configured (call SetActivitiesWriter at boot)")

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
	assetIDStr := uuid.UUID(req.AssetId).String()
	pgAsset := pgtype.UUID{Bytes: uuid.UUID(req.AssetId), Valid: true}

	// Gold-standard path: Remove(object=asset, target=collection)
	// per AP §6.7 / §7.9. 1.22.B-cleanup made activities required.
	if h.activities == nil {
		return nil, errCollectionsFederationNotWired
	}
	em := emit.RemoveFromCollection(
		h.actorContext(ctx, caller),
		activities.ObjectKindAsset,
		assetIDStr,
		uuid.UUID(pgID.Bytes).String(),
		cur.Name,
	)
	errRun := h.activities.WithEmission(ctx, activities.EmissionInput{
		Activity: em.Activity,
	}, func(tx pgx.Tx) error {
		return New(tx).RemoveCollectionResource(ctx, RemoveCollectionResourceParams{
			CollectionID: pgID,
			AssetID:      pgAsset,
		})
	})
	if errRun != nil {
		return nil, fmt.Errorf("collections: remove resource: %w", errRun)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.RemoveCollectionResource204Response{}, nil
}

// ---------------------------------------------------------------------------
// ACLs — additive grants on top of visibility (ADR 0010 L6)
// ---------------------------------------------------------------------------
//
// Same shape as post ACLs: polymorphic principal, three permission
// levels, optional expiry. Read access required to list; write access
// required to mutate.

func (h *Handler) ListCollectionAcls(
	ctx context.Context,
	req openapi.ListCollectionAclsRequestObject,
) (openapi.ListCollectionAclsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListCollectionAcls401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	row, err := h.getByIDCached(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListCollectionAcls404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	// canRead: owner always; public visibility always; otherwise need
	// mutate to view the ACL list (a stricter rule than read).
	if row.OwnerUserRef != caller.UserRef && row.Visibility != "public" && !canMutateCollection(caller, row) {
		return openapi.ListCollectionAcls403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not visible to this user"},
		}, nil
	}
	rows, err := New(h.Pool).ListCollectionAcls(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("collections: list acls: %w", err)
	}
	out := make([]openapi.AclEntry, 0, len(rows))
	for _, r := range rows {
		e := openapi.AclEntry{
			PrincipalType: openapi.AclEntryPrincipalType(r.PrincipalType),
			PrincipalId:   r.PrincipalID,
			Permission:    openapi.AclEntryPermission(r.Permission),
			GrantedAt:     r.GrantedAt.Time,
		}
		if r.GrantedByRsUserID != nil {
			e.GrantedByRsUserId = r.GrantedByRsUserID
		}
		if r.ExpiresAt.Valid {
			t := r.ExpiresAt.Time
			e.ExpiresAt = &t
		}
		out = append(out, e)
	}
	return openapi.ListCollectionAcls200JSONResponse(out), nil
}

func (h *Handler) AddCollectionAcl(
	ctx context.Context,
	req openapi.AddCollectionAclRequestObject,
) (openapi.AddCollectionAclResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddCollectionAcl401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddCollectionAcl400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	row, err := h.getByIDCached(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddCollectionAcl404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutateCollection(caller, row) {
		return openapi.AddCollectionAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the collection owner"},
		}, nil
	}
	var expires pgtype.Timestamptz
	if req.Body.ExpiresAt != nil {
		expires = pgtype.Timestamptz{Time: *req.Body.ExpiresAt, Valid: true}
	}
	if err := New(h.Pool).AddCollectionAcl(ctx, AddCollectionAclParams{
		CollectionID:        pgID,
		PrincipalType:       string(req.Body.PrincipalType),
		PrincipalID:         req.Body.PrincipalId,
		Permission:          string(req.Body.Permission),
		GrantedByRsUserID:   &caller.UserRef,
		ExpiresAt:           expires,
	}); err != nil {
		return nil, fmt.Errorf("collections: add acl: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.AddCollectionAcl204Response{}, nil
}

func (h *Handler) RemoveCollectionAcl(
	ctx context.Context,
	req openapi.RemoveCollectionAclRequestObject,
) (openapi.RemoveCollectionAclResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemoveCollectionAcl401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	row, err := h.getByIDCached(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RemoveCollectionAcl404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutateCollection(caller, row) {
		return openapi.RemoveCollectionAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the collection owner"},
		}, nil
	}
	rows, err := New(h.Pool).RemoveCollectionAcl(ctx, RemoveCollectionAclParams{
		CollectionID:  pgID,
		PrincipalType: string(req.PrincipalType),
		PrincipalID:   req.PrincipalId,
		Permission:    string(req.Permission),
	})
	if err != nil {
		return nil, fmt.Errorf("collections: remove acl: %w", err)
	}
	if rows == 0 {
		return openapi.RemoveCollectionAcl404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "ACL entry not found"},
		}, nil
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.RemoveCollectionAcl204Response{}, nil
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
		AssetType: r.AssetType,
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
