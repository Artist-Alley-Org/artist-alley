// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
// wired up alongside the legacy collection table.
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
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
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

	// metadataGate plumbs the Phase 1.9.B required-collection-field
	// gate into Create. nil-safe: a Handler built without one acts
	// as if no required collection fields are configured (preserves
	// pre-1.9.B behaviour for tests that don't wire metadata).
	metadataGate MetadataGate

	// previewLadder reports the operator's CONFIGURED preview variant
	// keys, cached (#591). nil-safe: nil means no ladder, so
	// ladder_available is false and clients stay on the `col` rung.
	previewLadder sysconfig.PreviewLadderReader

	// Audit records admin lifecycle events (soft_deleted; restore
	// fires from softdelete.Service directly). Nil-safe.
	Audit *audit.Recorder

	// SoftDelete handles restore. Wired at boot in api.go.
	SoftDelete *softdelete.Service
}

// MetadataGate is the minimal interface collections.Create needs to
// enforce required-on-create + seed initial values. metadata.Handler
// satisfies it directly; tests can implement a fake.
type MetadataGate interface {
	// RequiredCollectionFields lists every active collection-scoped
	// field_definition whose required=TRUE. Used by Create as the
	// pre-insert validation gate.
	RequiredCollectionFields(ctx context.Context) ([]RequiredField, error)
	// UpsertCollectionFieldValueInTx writes one value inside the
	// caller's tx. Run in the same tx as the collection INSERT so
	// a failed value write rolls the whole creation back.
	UpsertCollectionFieldValueInTx(
		ctx context.Context,
		tx pgx.Tx,
		collectionID, fieldID uuid.UUID,
		raw CollectionFieldValueInput,
		callerRef int64,
	) error
}

// RequiredField is the abridged field-definition shape Create needs
// to render the 422 reason. Mirrors the openapi enum on `type`.
type RequiredField struct {
	ID    uuid.UUID
	Code  string
	Label string
	Type  string
}

// CollectionFieldValueInput is the value shape collections.Create
// passes to MetadataGate. One of value_* per field type; the gate
// implementation maps to the typed columns.
type CollectionFieldValueInput struct {
	ValueText    *string
	ValueNum     *float64
	ValueDate    *time.Time
	ValueOptions *[]string
	ValueRef     *uuid.UUID
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

// SetMetadataGate plugs in the Phase 1.9.B metadata helper so
// CreateCollection can validate required collection-scoped fields
// and seed initial values inside the create tx.
func (h *Handler) SetMetadataGate(g MetadataGate) {
	h.metadataGate = g
}

// SetPreviewLadder installs the cached configured-ladder reader (#591).
func (h *Handler) SetPreviewLadder(r sysconfig.PreviewLadderReader) { h.previewLadder = r }

// ladder returns the configured preview variant keys, or nil when the
// reader is not wired — the conservative answer (ladder_available false).
func (h *Handler) ladder(ctx context.Context) []string {
	if h.previewLadder == nil {
		return nil
	}
	return h.previewLadder(ctx)
}

// RequiredCollectionFieldMissingError signals that a required
// collection field was absent from the create body. The HTTP
// handler converts this to a 422 with field_code/field_label so
// the UI can highlight the offending input.
type RequiredCollectionFieldMissingError struct {
	FieldCode  string
	FieldLabel string
}

func (e *RequiredCollectionFieldMissingError) Error() string {
	return fmt.Sprintf("required collection field missing: %s", e.FieldCode)
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

	// Phase 1.9.B — required-collection-field gate. Runs BEFORE any
	// write so a missing required field returns 422 without leaving
	// a half-created collection behind. Only CREATE enforces; UPDATE
	// doesn't re-validate (mirrors typical CMS patterns — fields can
	// be added after a collection exists without retroactively
	// breaking it). Skipped when metadataGate is nil (test/wiring
	// fallback).
	suppliedValues := map[uuid.UUID]CollectionFieldValueInput{}
	if in.FieldValues != nil {
		for _, fv := range *in.FieldValues {
			suppliedValues[uuid.UUID(fv.FieldId)] = CollectionFieldValueInput{
				ValueText:    fv.ValueText,
				ValueNum:     float64Ptr(fv.ValueNum),
				ValueDate:    fv.ValueDate,
				ValueOptions: fv.ValueOptions,
				ValueRef:     uuidPtrFromOpenAPI(fv.ValueRef),
			}
		}
	}
	if h.metadataGate != nil {
		required, rfErr := h.metadataGate.RequiredCollectionFields(ctx)
		if rfErr != nil {
			return nil, fmt.Errorf("collections: list required fields: %w", rfErr)
		}
		for _, rf := range required {
			if _, ok := suppliedValues[rf.ID]; !ok {
				return openapi.CreateCollection422JSONResponse{
					Error:      "required collection field missing: " + rf.Code,
					Reason:     openapi.RequiredCollectionFieldMissing,
					FieldCode:  &rf.Code,
					FieldLabel: &rf.Label,
				}, nil
			}
		}
	}

	// Gold-standard path: WithEmissionFn so we capture the
	// generated collection UUID and use it to build the activity's
	// URI in the same tx. 1.22.B-cleanup made activities required.
	var saved Collection
	err := h.activities.WithEmissionFn(ctx, func(tx pgx.Tx) (activities.EmissionInput, error) {
		r, err := New(tx).CreateCollection(ctx, CreateCollectionParams{
			OwnerUserRef: id.UserRef,
			Name:         name,
			Description:  strOr(in.Description, ""),
			Visibility:   visibility,
			Membership:   membership,
			ExpiresAt:    pgTimestamptzFromPtr(in.ExpiresAt),
			Purpose:      in.Purpose,
		})
		if err != nil {
			return activities.EmissionInput{}, fmt.Errorf("collections: create: %w", err)
		}
		saved = r
		// Phase 1.9.B — seed the supplied field values inside the
		// same tx so a write failure rolls the collection back.
		// Required-field validation already passed; per-field type
		// validation happens inside the metadata helper.
		if h.metadataGate != nil {
			for fieldID, val := range suppliedValues {
				if upErr := h.metadataGate.UpsertCollectionFieldValueInTx(
					ctx, tx, uuid.UUID(r.ID.Bytes), fieldID, val, id.UserRef,
				); upErr != nil {
					return activities.EmissionInput{}, fmt.Errorf("collections: seed value: %w", upErr)
				}
			}
		}
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

// float64Ptr widens an openapi float32 pointer to a float64 pointer
// for the metadata-gate value shape. Mirrors the narrowing pattern
// in metadata/handler.go.
func float64Ptr(p *float32) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}

// uuidPtrFromOpenAPI converts an oapi UUID pointer to a uuid.UUID
// pointer.
func uuidPtrFromOpenAPI(p *openapi_types.UUID) *uuid.UUID {
	if p == nil {
		return nil
	}
	v := uuid.UUID(*p)
	return &v
}

// ---------------------------------------------------------------------------
// GetCollection
// ---------------------------------------------------------------------------

// collectionCaller builds the visibility caller for the request,
// anonymous when there is no identity (#415).
func collectionCaller(ctx context.Context) visibility.Caller {
	if id := auth.IdentityFromContext(ctx); id != nil {
		return visibility.NewCaller(&id.UserRef)
	}
	return visibility.NewCaller(nil)
}

func (h *Handler) GetCollection(
	ctx context.Context,
	req openapi.GetCollectionRequestObject,
) (openapi.GetCollectionResponseObject, error) {
	// #415 — anonymous callers are admitted, and every caller now passes
	// a real check. Before this, ANY authenticated caller could fetch ANY
	// collection by id; the only gate was "is there an identity".
	id := auth.IdentityFromContext(ctx)
	visible, visErr := visibility.CanSee(ctx, h.Pool, visibility.EntityCollection,
		collectionCaller(ctx), uuid.UUID(req.Id))
	if visErr != nil || !visible {
		// Fail closed. The superadmin soft-deleted branch below is
		// reached via the ErrNoRows path, so it stays intact for admins,
		// who CanSee admits.
		if id == nil || !id.Can(auth.SuperAdminCapability) {
			return openapi.GetCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	row, err := h.getByIDCached(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Phase 1.55.C-1b: admin callers still see soft-deleted
			// rows (so the Restore button on /collections/{id} has
			// something to render). Non-admin callers stay on the
			// 404 path — soft-deleted collections are invisible
			// to them.
			if id != nil && id.Can(auth.SuperAdminCapability) {
				if adminRow, adminErr := New(h.Pool).GetCollectionIncludingDeleted(ctx, pgID); adminErr == nil {
					return openapi.GetCollection200JSONResponse(rowToAPI(adminRow)), nil
				}
			}
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

	// Phase 1.16 optimistic-concurrency check. Compares the
	// loaded row's updated_at against the caller's last-known
	// value. Truncate both sides to µs (Postgres stores at µs;
	// Go marshals at ns). Caller opts in by sending the field;
	// absent = legacy last-write-wins.
	if in.IfUnchangedSince != nil && cur.UpdatedAt.Valid {
		stored := cur.UpdatedAt.Time.Truncate(time.Microsecond)
		sent := in.IfUnchangedSince.Truncate(time.Microsecond)
		if !stored.Equal(sent) {
			return openapi.UpdateCollection409JSONResponse{
				Error:     "collection was edited by someone else after your last load; reload and try again",
				UpdatedAt: cur.UpdatedAt.Time,
			}, nil
		}
	}
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
	var saved Collection
	errRun := h.activities.WithEmissionFn(ctx, func(tx pgx.Tx) (activities.EmissionInput, error) {
		r, err := New(tx).UpdateCollection(ctx, UpdateCollectionParams{
			ID:          pgID,
			Name:        namePtr,
			Description: in.Description,
			Visibility:  visPtr,
			Membership:  memPtr,
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
	reason := extractSoftDeleteReason(req.Body)
	if len(reason) > softDeleteReasonMaxLen {
		return openapi.DeleteCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "reason exceeds 500 chars"},
		}, nil
	}
	// Phase 1.55.C-1b: DeleteCollection is now soft-delete. Row
	// remains for the operator recovery window (default 30 days
	// via sysconfig.CollectionRetentionDays); the gc coordinator
	// hard-deletes past retention, at which point
	// collection_resources / collection_posts / collection_acls
	// cascade via existing FK ON DELETE CASCADE. Activity emit
	// still fires immediately per AP §6.4 Tombstone semantics —
	// peers see the delete right away; the local row stays
	// recoverable for the operator.
	em := emit.DeleteCollection(h.actorContext(ctx, caller), uuid.UUID(pgID.Bytes).String(), cur.Name)
	err = h.activities.WithEmission(ctx, activities.EmissionInput{
		Activity: em.Activity,
	}, func(tx pgx.Tx) error {
		return New(tx).DeleteCollection(ctx, DeleteCollectionParams{
			ID:            pgID,
			DeletedReason: softDeleteReasonPtr(reason),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("collections: delete: %w", err)
	}
	if h.Audit != nil {
		h.Audit.AdminCollectionSoftDeleted(ctx, nil, uuid.UUID(pgID.Bytes).String(), caller.UserRef, reason)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.DeleteCollection204Response{}, nil
}

// ---------------------------------------------------------------------------
// RestoreCollection — Phase 1.55.C-1b
// ---------------------------------------------------------------------------

// RestoreCollection clears deleted_at + deleted_reason on a soft-
// deleted collection. Admin-only.
func (h *Handler) RestoreCollection(
	ctx context.Context,
	req openapi.RestoreCollectionRequestObject,
) (openapi.RestoreCollectionResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.IsAnonymous() {
		return openapi.RestoreCollection401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(auth.SuperAdminCapability) {
		return openapi.RestoreCollection403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "admin capability required"},
		}, nil
	}
	if h.SoftDelete == nil {
		return nil, fmt.Errorf("collections: RestoreCollection: SoftDelete service unwired")
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if err := h.SoftDelete.RestoreCollection(ctx, nil, uuid.UUID(req.Id), id.UserRef); err != nil {
		if errors.Is(err, softdelete.ErrNotDeleted) || errors.Is(err, softdelete.ErrNotFound) {
			return openapi.RestoreCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not soft-deleted"},
			}, nil
		}
		return nil, fmt.Errorf("collections: restore: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.RestoreCollection204Response{}, nil
}

// extractSoftDeleteReason pulls the reason from an optional
// SoftDeleteRequest body. Empty body / empty reason both map to "".
func extractSoftDeleteReason(body *openapi.SoftDeleteRequest) string {
	if body == nil || body.Reason == nil {
		return ""
	}
	return strings.TrimSpace(*body.Reason)
}

// softDeleteReasonPtr returns nil for empty strings, else a pointer
// to the value.
func softDeleteReasonPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// softDeleteReasonMaxLen bounds the operator-supplied reason string.
const softDeleteReasonMaxLen = 500

// ---------------------------------------------------------------------------
// ListCollections
// ---------------------------------------------------------------------------

func (h *Handler) ListCollections(
	ctx context.Context,
	req openapi.ListCollectionsRequestObject,
) (openapi.ListCollectionsResponseObject, error) {
	// #415 — anonymous callers are admitted; the predicate decides which
	// rows they see (anonymous => public, non-deleted only). #449 made
	// that true: until then this comment described an intent the query
	// never implemented.
	id := auth.IdentityFromContext(ctx)

	// Phase 1.55.C-1b: ?include_deleted=true is admin-only.
	// Non-admins silently see the default filtered list.
	includeDeleted := false
	if req.Params.IncludeDeleted != nil && *req.Params.IncludeDeleted && id.Can(auth.SuperAdminCapability) {
		includeDeleted = true
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
	var includeDeletedArg *bool
	if includeDeleted {
		t := true
		includeDeletedArg = &t
	}
	// #449 — gated. The sqlc query this replaces applied no visibility
	// rule, so a caller who set none of the optional filters received
	// the whole table; anonymous callers enumerated private
	// collections. See ListCollectionsPageGated.
	rows, err := ListCollectionsPageGated(ctx, h.Pool, collectionCaller(ctx), ListCollectionsPageGatedParams{
		IncludeDeleted:  includeDeletedArg,
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
		c := rowToAPI(r)
		// Surface soft-delete state for the admin trash view.
		if r.DeletedAt.Valid {
			dt := r.DeletedAt.Time
			c.DeletedAt = &dt
			c.DeletedReason = r.DeletedReason
		}
		items = append(items, c)
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
	// #438 — anonymous callers are admitted, and every caller now passes
	// a real check on the PARENT collection. Before this, the handler
	// checked only that an identity existed, so any authenticated caller
	// could enumerate any collection's contents including ones they hold
	// no ACL on. The row-level gate lives in the query below; both are
	// required, because a public collection may contain non-public assets.
	caller := collectionCaller(ctx)
	visible, visErr := visibility.CanSee(ctx, h.Pool, visibility.EntityCollection,
		caller, uuid.UUID(req.Id))
	if visErr != nil || !visible {
		// Fail closed, 404 not 403 (ADR 0064) — do not confirm the
		// collection exists.
		return openapi.ListCollectionResources404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
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

	// caps only short-circuits preview_available for SystemAdmin /
	// content.read.all (#471); it does not affect row visibility.
	var caps visibility.CapabilityChecker
	if id := auth.IdentityFromContext(ctx); id != nil {
		caps = func(code string) bool { return id.Can(code) }
	}
	fetch := limit + 1
	rows, err := ListCollectionResourcesPageGated(ctx, h.Pool, caller, caps,
		ListCollectionResourcesPageGatedParams{
			CollectionID:    pgID,
			CursorSortOrder: cursorSort,
			CursorAddedAt:   cursorAdded,
			RowLimit:        fetch,
			Ladder:          h.ladder(ctx),
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

	// #882 — the ASSET gate. Everything above authorises the
	// COLLECTION; until this landed nothing looked at the asset at all,
	// so any collection owner could pin any asset in the instance given
	// its UUID, and a 404-vs-204 probe confirmed whether an arbitrary
	// UUID existed.
	//
	// A refusal here is deliberately the SAME 404 the FK miss below
	// returns — same status, same body. Anything else (403
	// "forbidden", a distinct message) re-creates the enumeration
	// oracle this check exists to remove.
	collectible, err := h.mayCollectAsset(ctx, caller, uuid.UUID(in.AssetId))
	if err != nil {
		return nil, fmt.Errorf("collections: add resource: asset gate: %w", err)
	}
	if !collectible {
		return openapi.AddCollectionResource404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}

	// Gold-standard path: Add(object=asset, target=collection)
	// per AP §6.6 / §7.8. 1.22.B-cleanup made activities required.
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
// return 404 without surfacing as a 500 server error. It is now a
// race backstop rather than the primary path — mayCollectAsset
// rejects an absent asset before any activity is emitted.
var errAssetMissing = errors.New("collections: asset row absent")

// mayCollectAsset answers "may this caller put THIS asset into a
// collection" (#882). You may only collect what you can actually see.
//
// # Why this is not visibility.CanSee alone
//
// The obvious call — CanSee(EntityAsset) — gates NOTHING here. Per ADR
// 0064 sensitivity lives on the CONTENT plane, not the row plane, so
// EntityAsset's authenticated predicate is `deleted_at IS NULL` and
// nothing more (visibility/predicate.go, EntityAsset branch; CanSee's
// own doc says as much). Every authenticated caller is row-visible to
// every undeleted asset, so a gate built on CanSee alone would return
// true for a restricted asset it has never been allowed to view, and
// would review as if it worked.
//
// # The rule
//
// The conjunction visibility.FieldsReadable already documents
// (visibility/member.go, "the CONJUNCTION of the two planes"): a caller
// may collect an asset iff they could have reached that ROW standalone
// AND could have reached its BYTES. FieldsReadable itself is not
// callable here — it takes an already-fetched MemberRow supplied by the
// container queries — so this composes the same two planes from their
// existing entry points rather than writing a third expression of the
// rule (#892 / epic #665 consolidated exactly that duplication):
//
//   - ROW plane   — visibility.CanSee(EntityAsset): exists and is not
//     soft-deleted. Load-bearing on its own account: ContentReadable
//     never looks at deleted_at, so without this conjunct a caller
//     could pin a deleted public asset — a member row the contents
//     query then drops in SQL, i.e. an invisible phantom member.
//   - CONTENT plane — visibility.CanReadContent (ADR 0064): the tier
//     rule. Public admits everyone, team admits the asset's team,
//     restricted / embargo / anything unrecognised admit only the owner
//     and the two capability holders.
//
// # The short-circuits are inherited deliberately
//
// CanReadContent admits SystemAdmin and ContentReadAll at every tier,
// and this path keeps both. ContentReadAll's whole purpose is a role
// (the public demo's demo-viewer) that RENDERS a mostly-restricted
// catalogue; a caller who is allowed to view every asset is by this
// endpoint's own rule — "you may collect what you can see" — allowed to
// collect them. Narrowing it here would put the add path out of step
// with FieldsReadable, which would then render the very members this
// refused to create.
//
// # Fails closed
//
// A nonexistent asset stops at the ROW plane. CanReadContent wraps
// pgx.ErrNoRows into an error (it is the "we could not load the row"
// case), so the race in which the asset is deleted between the two
// queries is folded into "not collectible" rather than surfacing as a
// 500 — which would also be an oracle, since a 500 is distinguishable
// from a 404.
func (h *Handler) mayCollectAsset(ctx context.Context, id *auth.Identity, assetID uuid.UUID) (bool, error) {
	if id == nil {
		return false, nil
	}
	caller := visibility.NewCaller(&id.UserRef)
	caps := visibility.CapabilityChecker(func(code string) bool { return id.Can(code) })

	visible, err := visibility.CanSee(ctx, h.Pool, visibility.EntityAsset, caller, assetID)
	if err != nil {
		return false, fmt.Errorf("row plane: %w", err)
	}
	if !visible {
		return false, nil
	}

	readable, err := visibility.CanReadContent(ctx, h.Pool, caller, caps, assetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("content plane: %w", err)
	}
	return readable, nil
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
	assetIDStr := uuid.UUID(req.AssetId).String()
	pgAsset := pgtype.UUID{Bytes: uuid.UUID(req.AssetId), Valid: true}

	// Gold-standard path: Remove(object=asset, target=collection)
	// per AP §6.7 / §7.9. 1.22.B-cleanup made activities required.
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
	//
	// #661 flagged this as a hand-maintained restatement of
	// visibility.Filter that should be consolidated onto it. It is
	// NOT a restatement — it is a DIFFERENT rule, deliberately, and
	// folding it into the predicate would WIDEN access rather than
	// consolidate it. The authenticated EntityCollection predicate is
	// `public OR owner OR a live collection_acls grant`; this gate
	// drops the grant disjunct (a read-grantee may use the collection
	// without seeing who else was granted what) and adds a
	// collections.admin / system.admin bypass the predicate
	// deliberately refuses to carry. "Who may read the grant list" is
	// a management question, not the row-visibility question ADR 0063
	// answers.
	//
	// The soft-delete dimension is covered upstream: getByIDCached
	// reads GetCollection, which filters `deleted_at IS NULL`.
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
		if r.GrantedByUserRef != nil {
			e.GrantedByUserRef = r.GrantedByUserRef
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
		CollectionID:     pgID,
		PrincipalType:    string(req.Body.PrincipalType),
		PrincipalID:      req.Body.PrincipalId,
		Permission:       string(req.Body.Permission),
		GrantedByUserRef: &caller.UserRef,
		ExpiresAt:        expires,
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

// resourceRowToAPI serialises ONE membership row.
//
// The two branches are the #883 allow-list. The placeholder branch is
// written as a complete literal rather than as "build the full row, then
// clear the sensitive fields": a field added to CollectionResource later
// is absent from a literal by construction, whereas a clear-list has to
// be remembered. That is the deny-list failure mode this issue exists to
// avoid, and it is why the shared assignments below are duplicated
// instead of hoisted.
func resourceRowToAPI(r ListCollectionResourcesPageGatedRow) openapi.CollectionResource {
	if r.Restricted {
		out := openapi.CollectionResource{
			// collection_resources columns only — nothing from `assets`.
			CollectionId: openapi_types.UUID(r.CollectionID.Bytes),
			AssetId:      openapi_types.UUID(r.AssetID.Bytes),
			SortOrder:    int(r.SortOrder),
			Pinned:       r.Pinned,
			AddedAt:      r.AddedAt.Time,
			Restricted:   true,
		}
		if r.ExpiresAt.Valid {
			t := r.ExpiresAt.Time
			out.ExpiresAt = &t
		}
		// Absent, not "", when the owner has no resolvable name — a
		// client must not be able to read anything off the difference
		// between "withheld" and "empty".
		if r.OwnerDisplayName != "" {
			v := r.OwnerDisplayName
			out.OwnerDisplayName = &v
		}
		return out
	}

	title := r.Title
	assetType := r.AssetType
	status := openapi.CollectionResourceStatus(r.Status)
	preview, ladder, scrub := r.PreviewAvailable, r.LadderAvailable, r.ScrubAvailable
	out := openapi.CollectionResource{
		CollectionId: openapi_types.UUID(r.CollectionID.Bytes),
		AssetId:      openapi_types.UUID(r.AssetID.Bytes),
		SortOrder:    int(r.SortOrder),
		Pinned:       r.Pinned,
		AddedAt:      r.AddedAt.Time,
		Restricted:   false,
		Title:        &title,
		AssetType:    &assetType,
		Status:       &status,
		FileHash:     r.FileHash,
		// #595 — the media-type + blur-up fields. A member tile renders
		// through the same CardThumb as a browse tile, and CardThumb
		// reads the media type off the extension alone (video / 3D badge
		// + sprite-scrub hover preview). Without these the tile is an
		// untyped still. Encoded exactly as assets.assetRowToAPI does.
		// They are `omitempty` pointers now only because the placeholder
		// branch above needs them absent; on THIS branch every one is
		// still populated unconditionally, and member_allowlist_test.go
		// pins that.
		FileExtension:    r.FileExtension,
		PreviewAvailable: &preview,
		LadderAvailable:  &ladder,
		ScrubAvailable:   &scrub,
		// #640 — the member tile's aspect ratio. Same pair-or-neither
		// contract as everywhere else; the gated row already dropped a
		// half-populated pair.
		PixelWidth:  r.PixelWidth,
		PixelHeight: r.PixelHeight,
	}
	if len(r.Thumbhash) > 0 {
		v := base64.StdEncoding.EncodeToString(r.Thumbhash)
		out.Thumbhash = &v
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
