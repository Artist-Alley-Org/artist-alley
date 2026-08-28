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

	"github.com/mscrnt/artist-alley/app/internal/acls"
	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/coverfocal"
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

// collectionCaps adapts an auth.Identity to the capability half of the
// visibility helpers' caller pair. Nil identity yields a nil checker,
// which every helper reads as "holds nothing" — an anonymous caller
// must not be handed a checker that could accidentally answer true.
func collectionCaps(id *auth.Identity) visibility.CapabilityChecker {
	if id == nil {
		return nil
	}
	return func(code string) bool { return id.Can(code) }
}

func (h *Handler) GetCollection(
	ctx context.Context,
	req openapi.GetCollectionRequestObject,
) (openapi.GetCollectionResponseObject, error) {
	// #415 — anonymous callers are admitted, and every caller now passes
	// a real check. Before this, ANY authenticated caller could fetch ANY
	// collection by id; the only gate was "is there an identity".
	id := auth.IdentityFromContext(ctx)
	// #1059 — the read rule is visibility.CanReadCollection, which is
	// the row plane OR system.admin. That admin disjunct used to be
	// spelled out right here, and again in facet.Selection.Authorize for
	// the "Search in this collection" button on the very page this
	// endpoint renders; two copies of one rule is how an admin ends up
	// able to open a collection and unable to search inside it.
	readable, readErr := visibility.CanReadCollection(ctx, h.Pool,
		collectionCaller(ctx), collectionCaps(id), uuid.UUID(req.Id))
	if readErr != nil || !readable {
		// Fail closed, including on error — a 500 here would be
		// distinguishable from a 404 and so would answer "does this id
		// exist" for a collection the caller may not read.
		return openapi.GetCollection404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
		}, nil
	}
	// An admin passes the line above without the row plane agreeing, so
	// the soft-deleted branch below is still reached via ErrNoRows and
	// the Restore button still has something to render.
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
					out := rowToAPI(adminRow)
					if err := h.attachCovers(ctx, &out); err != nil {
						return nil, err
					}
					return openapi.GetCollection200JSONResponse(out), nil
				}
			}
			return openapi.GetCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
			}, nil
		}
		return nil, err
	}
	// The mosaic cover (#1026) is an ENRICHMENT PASS after the cache
	// read, never a field inside the cached Collection: which members a
	// caller may picture depends on who is asking, and ADR 0013's
	// 2026-08-11 amendment is exact about where such a value may live.
	// See attachCovers.
	out := rowToAPI(row)
	if err := h.attachCovers(ctx, &out); err != nil {
		return nil, err
	}
	return openapi.GetCollection200JSONResponse(out), nil
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

	// Load current row first to enforce ownership (canMutateCollection
	// reads owner_user_ref) and to answer the if_unchanged_since check
	// below against a real updated_at.
	//
	// #1073: this used to claim it also fed "ClearCollectionExpiresAt
	// the right id". No such call was ever made from here — the comment
	// asserted a guarantee the code did not provide, and it is why a
	// TTL that could not be cleared survived three releases of review.
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

	// #1027 — the chosen cover. Mutually exclusive with its clear flag,
	// refused rather than resolved: if a client sends both it has two
	// intentions and the server has no basis for preferring either, and
	// silently discarding one is how a "clear" that never happened gets
	// shipped. Same shape as metadata's default_value / clear_default.
	clearCover := in.ClearCover != nil && *in.ClearCover
	if clearCover && in.CoverAssetId != nil {
		return openapi.UpdateCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "send either cover_asset_id or clear_cover, not both",
			},
		}, nil
	}
	var coverPtr *uuid.UUID
	if in.CoverAssetId != nil {
		want := uuid.UUID(*in.CoverAssetId)
		// The PICTURE plane, not the field plane: a cover IS a picture,
		// and ADR 0064 hands a scoped `assets.admin` holder an asset's
		// fields while explicitly withholding its image. Gating on
		// readability instead would let such a holder pin a picture they
		// are not allowed to look at onto a collection other people read.
		//
		// The caller triple comes from the same helper the read path
		// uses, so "may point at" and "may see painted" are one rule
		// evaluated twice rather than two rules.
		cCaller, cCaps, _, cMature := CoverCallerFromContext(ctx)
		mayPicture, err := CallerMayPictureAsset(ctx, h.Pool, cCaller, cCaps, cMature, want)
		if err != nil {
			return nil, err
		}
		if !mayPicture {
			// ONE response for "no such asset" and "not yours to look
			// at". Distinguishing them turns this endpoint into an
			// existence oracle: a curator could enumerate asset ids and
			// read the difference between 400-missing and 403-forbidden
			// as "this id exists and is hidden from me", which is the
			// fact the picture plane withholds in the first place.
			return openapi.UpdateCollection400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "cover_asset_id is not an asset you can use as a cover",
				},
			}, nil
		}
		coverPtr = &want
	}

	// #1207 — the featured rail's own cover. Same three moves as
	// cover_asset_id directly above, for the same three reasons, over a
	// second column: the exclusivity 400, the PICTURE-plane check, and
	// the one 400 that does not distinguish "no such asset" from "not
	// yours to look at".
	//
	// It is a separate block rather than a loop over the two because the
	// only shared part is the shape; a loop would have to carry two
	// different error strings and two different openapi fields through
	// it, and the reader would have to unroll it to check either.
	clearFeaturedCover := in.ClearFeaturedCover != nil && *in.ClearFeaturedCover
	if clearFeaturedCover && in.FeaturedCoverAssetId != nil {
		return openapi.UpdateCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "send either featured_cover_asset_id or clear_featured_cover, not both",
			},
		}, nil
	}
	var featuredCoverPtr *uuid.UUID
	if in.FeaturedCoverAssetId != nil {
		want := uuid.UUID(*in.FeaturedCoverAssetId)
		cCaller, cCaps, _, cMature := CoverCallerFromContext(ctx)
		mayPicture, err := CallerMayPictureAsset(ctx, h.Pool, cCaller, cCaps, cMature, want)
		if err != nil {
			return nil, err
		}
		if !mayPicture {
			return openapi.UpdateCollection400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "featured_cover_asset_id is not an asset you can use as a cover",
				},
			}, nil
		}
		featuredCoverPtr = &want
	}

	// #1207 — the focal point, which is a PAIR and is validated as one.
	// See [validateFocalPair] for the three states it refuses and why
	// each of them would otherwise reach the column CHECK as a
	// constraint error instead of a 400.
	clearFocal := in.ClearFeaturedCoverFocal != nil && *in.ClearFeaturedCoverFocal
	if resp := validateFocalPair(
		"featured_cover_focal_x", "featured_cover_focal_y", "clear_featured_cover_focal",
		in.FeaturedCoverFocalX, in.FeaturedCoverFocalY, clearFocal,
	); resp != nil {
		return *resp, nil
	}

	// #1207 — the COLLECTION cover's own focal pair, on the square
	// destination. Validated by the same three refusals as the featured
	// pair; a shared helper rather than a third copy of them, because
	// three copies of a range check is how one of them ends up admitting
	// 1.5.
	clearCoverFocal := in.ClearCoverFocal != nil && *in.ClearCoverFocal
	if resp := validateFocalPair(
		"cover_focal_x", "cover_focal_y", "clear_cover_focal",
		in.CoverFocalX, in.CoverFocalY, clearCoverFocal,
	); resp != nil {
		return *resp, nil
	}

	// #1212 — the zoom for each slot. Its own value and its own clear
	// flag per slot, NOT folded into the focal pair's, because zoom and
	// position are independent settings: "back to the fit, still
	// positioned left" is an ordinary thing to want and a shared flag
	// could not say it.
	clearFeaturedCoverZoom := in.ClearFeaturedCoverZoom != nil && *in.ClearFeaturedCoverZoom
	if resp := validateZoom(
		"featured_cover_zoom", "clear_featured_cover_zoom",
		in.FeaturedCoverZoom, clearFeaturedCoverZoom,
	); resp != nil {
		return *resp, nil
	}
	clearCoverZoom := in.ClearCoverZoom != nil && *in.ClearCoverZoom
	if resp := validateZoom(
		"cover_zoom", "clear_cover_zoom",
		in.CoverZoom, clearCoverZoom,
	); resp != nil {
		return *resp, nil
	}

	// #1073 — expires_at is a tri-state, and the third state needs a
	// flag. `CollectionUpdate.ExpiresAt` is a *time.Time with
	// `omitempty`, so by the time a body reaches here "absent" and
	// "explicit null" are the SAME value: a nil pointer. The old
	// COALESCE read that nil as "keep", which meant a caller who sent
	// `{"expires_at": null}` to remove a TTL got a 200 and an unchanged
	// column — the clear silently did not happen. Same wall, same fix,
	// and the same 400, as clear_cover directly above.
	clearExpiresAt := in.ClearExpiresAt != nil && *in.ClearExpiresAt
	if clearExpiresAt && in.ExpiresAt != nil {
		return openapi.UpdateCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "send either expires_at or clear_expires_at, not both",
			},
		}, nil
	}

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
			// Each CASE in the query reads its clear flag first, so a
			// value and its clear flag cannot both take effect; the two
			// 400s above are what make those combinations unreachable
			// rather than merely ordered.
			ExpiresAt:      pgTimestamptzFromPtr(in.ExpiresAt),
			ClearExpiresAt: clearExpiresAt,
			ClearCover:     clearCover,
			CoverAssetID:   pgUUIDFromPtr(coverPtr),
			// #1207. Two clear flags, and the focal pair rides the
			// second one for both axes — see the query's CASE arms.
			ClearFeaturedCover:      clearFeaturedCover,
			FeaturedCoverAssetID:    pgUUIDFromPtr(featuredCoverPtr),
			ClearFeaturedCoverFocal: clearFocal,
			FeaturedCoverFocalX:     in.FeaturedCoverFocalX,
			FeaturedCoverFocalY:     in.FeaturedCoverFocalY,
			ClearCoverFocal:         clearCoverFocal,
			CoverFocalX:             in.CoverFocalX,
			CoverFocalY:             in.CoverFocalY,
			// #1212. Each zoom rides its own clear flag; see the
			// query's CASE arms and [validateZoom].
			ClearFeaturedCoverZoom: clearFeaturedCoverZoom,
			FeaturedCoverZoom:      in.FeaturedCoverZoom,
			ClearCoverZoom:         clearCoverZoom,
			CoverZoom:              in.CoverZoom,
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
		// deleted_by_user_ref is what makes the delete undoable by the
		// person who did it (#931) — see auth.CanRestoreDeleted.
		deleter := caller.UserRef
		return New(tx).DeleteCollection(ctx, DeleteCollectionParams{
			ID:               pgID,
			DeletedReason:    softDeleteReasonPtr(reason),
			DeletedByUserRef: &deleter,
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
// deleted collection. See auth.CanRestoreDeleted for the rule: you undo
// your own delete, system.admin undoes any. Previously system.admin
// only, while DeleteCollection was open to the owner — so an owner
// could delete their collection and then not get it back (#931).
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
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	deletedBy, err := New(h.Pool).GetCollectionDeletedBy(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RestoreCollection404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not soft-deleted"},
			}, nil
		}
		return nil, fmt.Errorf("collections: load deleted_by: %w", err)
	}
	if !auth.CanRestoreDeleted(id, deletedBy) {
		return openapi.RestoreCollection403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "this collection was deleted by someone else; ask an administrator to restore it",
			},
		}, nil
	}
	if h.SoftDelete == nil {
		return nil, fmt.Errorf("collections: RestoreCollection: SoftDelete service unwired")
	}
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
		// ⚠️ `hubPublicTier` is `public`, not `org-only`, and that is a
		// FIX (#1104), not a restatement.
		//
		// Both tabs below pinned `visibility = 'org-only'` from v0.1.0
		// until now, under a comment claiming the Public tab "maps to
		// org-only at the storage layer". That was true when it was
		// written: the baseline CHECK admitted private | org-only |
		// followers | explicit-share and there was no public tier at
		// all. Migration 00008 then ADDED `public` as a new, higher
		// tier ABOVE org-only, and nothing came back to this switch.
		//
		// So the Public tab — "every install-public collection" per the
		// OpenAPI description — has been returning exactly the
		// collections that are NOT install-public, and the Featured tab
		// has been ANDing a featured filter onto that same wrong set. On
		// this dev database that is 2 rows of 6, and 0 of the 6 featured
		// ones. #1104's report ("no featured collections in
		// collections") had TWO causes stacked on it; the scope split
		// was the one that was looked for, and this was underneath.
		//
		// The comment about oapi-codegen dropping the type-name prefix
		// went stale in the same commit that found this: adding an
		// org/public enum to FeaturedItemInput reintroduced the
		// collision, so the constants are prefixed again.
		const hubPublicTier = "public"

		switch *req.Params.Tab {
		case openapi.ListCollectionsParamsTabMine:
			ownerPtr = &caller.UserRef
			visPtr = nil
			featuredPtr = nil
		case openapi.ListCollectionsParamsTabFeatured:
			// NO TIER PIN (#1121). The question the comment that stood
			// here left open — "whether the tier pin belongs here AT
			// ALL" — is answered: it does not.
			//
			// The Featured tab means "featured collections THIS VIEWER
			// MAY SEE", and every read on this list is already gated by
			// the row predicate spliced in below (`visibility.Filter`
			// over EntityCollection: owner OR live ACL OR
			// `visibility='public'`, AND not soft-deleted). A
			// `visibility = 'public'` equality on top of that is a
			// SECOND, NARROWER rule for the same question — and a
			// second rule is free to disagree with the first, which is
			// exactly what it did.
			//
			// It disagreed with the RAIL. `featured.ListPublicRail`
			// gates its collection arm on the row predicate and nothing
			// else, so since #1104 made org-scoped featuring reachable,
			// an admin who featured an `org-only` collection saw it on
			// the rail and never on this tab. Two surfaces answering
			// one question differently is the divergence #1104 had just
			// finished eliminating for SCOPE; this is the same medicine
			// on the same table.
			//
			// ⚠️ THIS DOES NOT WIDEN ACCESS, and the reason is
			// structural rather than a promise. Featuring is a
			// PLACEMENT, not a grant (the "FEATURING NEVER WIDENS
			// ACCESS" line on `GET /featured`) — dropping the pin
			// removes a conjunct from a predicate whose remaining
			// conjuncts still have to admit the caller. A viewer who
			// cannot read the collection could not read it with the pin
			// either; the pin was only ever hiding rows from viewers
			// who WERE entitled to them.
			//
			// The Public tab below keeps its pin, and that is not an
			// inconsistency: `public` is that tab's whole CONTRACT
			// ("every install-public collection"), so there the tier is
			// the question rather than an extra answer.
			visPtr = nil
			f := true
			featuredPtr = &f
			ownerPtr = nil
		case openapi.ListCollectionsParamsTabPublic:
			vis := hubPublicTier
			visPtr = &vis
			ownerPtr = nil
			featuredPtr = nil
		case openapi.ListCollectionsParamsTabShared:
			sharedWithPtr = &caller.UserRef
			excludeOwnerPtr = &caller.UserRef
			ownerPtr = nil
			visPtr = nil
			featuredPtr = nil
		case openapi.ListCollectionsParamsTabAll:
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
	// The mosaic covers for the whole page in ONE query (#1026). The
	// hub renders up to 200 of these cards; a per-card composition
	// would be the N+1 the deleted client-side store existed to soften.
	// Pointers INTO `items`, taken after the slice is fully built so no
	// append can move the backing array under them.
	ptrs := make([]*openapi.Collection, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	if err := h.attachCovers(ctx, ptrs...); err != nil {
		return nil, err
	}
	resp := openapi.CollectionList{Items: items}
	if len(rows) > int(limit) {
		next := encodeCursor(lastCreatedAt, lastID)
		resp.NextCursor = &next
	}
	return openapi.ListCollections200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// The asset-membership endpoints are gone (#1161, #1236, ADR 0091)
// ---------------------------------------------------------------------------
//
// `POST /collections/{id}/resources` and
// `DELETE /collections/{id}/resources/{asset_id}` used to live here.
// They wrote `collection_resources` rows — a bare asset pinned into a
// collection — and that is the second publication path ADR 0091 exists
// to remove: "collections and browse contain posts only", so dropping
// a file into a collection published it with no title, no framing and
// no moment where the artist decided the work was ready.
//
// v0.10.1 (#1185) removed the visible half: the collection page's asset
// section and every affordance that reached these endpoints. #1161
// removed the writes.
//
// `GET /collections/{id}/resources` went with #1236, and it is worth
// recording WHY it outlived them by one release. It was kept on the
// grounds that "the cover picker reads it" — which #1232 had already
// falsified by moving the picker to posts. Nothing else called it: the
// grep across `web/src` came back empty, and the surface it had been
// built for (the member grid, with #883's restricted placeholders,
// #1133's card fields and its own cursor) had no page left to render
// on. A read endpoint justified by a caller that no longer exists is
// a claim about the model, and the claim was wrong.
//
// ⚠️ The rows are NOT converted into posts. ADR 0091 decision 4 is
// explicit: an auto-generated post is a publication nobody authored,
// with a title nobody wrote. The memberships simply stop being
// writable; the assets remain in their owners' storage, losing nothing.
//
// `mayCollectAsset` and the `errAssetMissing` sentinel went with them.
// The RULE they adapted did not: it lives in
// visibility.CanSeeAssetContent, which posts.mayAttachAsset composes
// for exactly the same question on the surface that now owns it — "you
// may only put in a post what you can actually see". Keeping a
// caller-less collections-side copy would have left a second adapter
// for a future contributor to reach for on a path this ADR closed.
//
// ---------------------------------------------------------------------------
// ⛔ `collection_resources` IS INTERNAL, NOT DEAD — read this before
// proposing the DROP (#1236, resolving ADR 0091's "becomes internal or
// disappears")
// ---------------------------------------------------------------------------
//
// No endpoint reads or writes the table any more, and after #1236 no
// rendered surface draws from it. That is NOT the same as unreferenced,
// and a counter-example search found live consumers on both sides:
//
//   - WRITERS. The seeder still inserts rows deliberately
//     (`seed.SeedInsertCollectionResource`; see the note at its call
//     site). `POST /search/save-as-collection` also still materialises
//     one row per search hit (`search.createCollectionWithResults`) —
//     unpinned, so those rows never painted a mosaic or moved a count,
//     but they are ordinary production writes.
//   - READERS. The federation shares gate resolves a share's container
//     membership through a `collection_resources` JOIN
//     (`federation/shares/queries.sql`), which is LIVE access-control
//     semantics. Scoped search reads it too: the `collection:` facet on
//     the asset entity (`search/facet.dimensionSQL`) and the reindex
//     job's `ScopeCollection` both resolve "assets inside this
//     collection" through it.
//
// So the table stays, with no reader or writer on the API surface. A
// future DROP has to answer for each of the consumers above — a
// migration that only greps for endpoints will find nothing and be
// wrong.

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
	// Requires WRITE access — owner, collections.admin or system.admin.
	// Read access is deliberately NOT enough (#933), which is the same
	// rule #876 settled for ListPostAcls.
	//
	// #661 flagged this as a hand-maintained restatement of
	// visibility.Filter that should be consolidated onto it. It is
	// NOT a restatement — it is a DIFFERENT rule, deliberately, and
	// folding it into the predicate would WIDEN access rather than
	// consolidate it. The authenticated EntityCollection predicate is
	// `public OR owner OR a live collection_acls grant`; this gate
	// drops BOTH the public disjunct and the grant disjunct, and adds a
	// collections.admin / system.admin bypass the predicate deliberately
	// refuses to carry. "Who may read the grant list" is a management
	// question, not the row-visibility question ADR 0063 answers.
	//
	// The public disjunct was the #933 leak. `visibility = 'public'` is
	// a statement about the collection's CONTENTS. It says nothing about
	// who the owner individually shared it with — that is a statement
	// about the owner's working relationships, and admitting every
	// authenticated caller to it handed out each grantee's principal_id
	// and permission level to anyone with an account and no connection
	// to the collection. The comment above this gate already argued for
	// the tighter rule while the condition below it did the opposite.
	//
	// The soft-delete dimension is covered upstream: getByIDCached
	// reads GetCollection, which filters `deleted_at IS NULL`.
	if !canMutateCollection(caller, row) {
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
	// Same boundary check as AddPostAcl — collection_acls has the
	// identical shape and the identical read rule, so it had the
	// identical defect (#916).
	if err := acls.ValidateContentPrincipal(
		string(req.Body.PrincipalType), req.Body.PrincipalId,
	); err != nil {
		return openapi.AddCollectionAcl400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
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

// InvalidateAfterRestore evicts the by-id cache for a collection that
// has just come back, for callers outside this package.
//
// The one caller is the composition root's restorer adapter, which a
// granted restoration appeal (#931) goes through instead of
// RestoreCollection. Mirrors assets.Handler.InvalidateAfterRestore; the
// posts handler needs no equivalent, because its restore path evicts
// nothing.
func (h *Handler) InvalidateAfterRestore(ctx context.Context, id uuid.UUID) {
	h.cacheInvalidate(ctx, pgtype.UUID{Bytes: id, Valid: true})
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
//
// The two guards mirror canMutatePost (#936): an anonymous identity is
// never a principal, and ref 0 is the anonymous SENTINEL rather than a
// user, so it must not match on either side of the ownership
// comparison. `collections.owner_user_ref` is `bigint NOT NULL`, so
// there is no NULL-owner case to trap the way assets have — but a row
// written with owner_user_ref = 0 would otherwise be "owned" by every
// anonymous caller. No user holds ref 0 today; that is data, not a
// structural guarantee.
//
// Deliberately NOT mirrored from canMutatePost: the team-scoped
// disjunct. `collections` has no team_id column at all, so auth.InTeam
// would have nothing to scope against. Giving collections a team is a
// schema decision, not a hardening.
func canMutateCollection(id *auth.Identity, row Collection) bool {
	if id == nil || id.IsAnonymous() {
		return false
	}
	if id.UserRef != 0 && row.OwnerUserRef != 0 && row.OwnerUserRef == id.UserRef {
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
	// #1027 — the curator's SETTING, shipped unconditionally. It is not
	// the render answer and carries no picture: `covers` is what a client
	// paints, and ComposeCovers re-runs the viewer's picture plane over
	// this id before it appears there. What this exposes is that a cover
	// was chosen and which asset id it is, to a caller who has already
	// passed the collection's own read gate — the same class of fact as
	// the member ids /collections/{id}/resources hands that caller, where
	// a member they may not picture is returned as a VISIBLE placeholder
	// precisely so #881's "request access" has something to attach to.
	// Withholding it here would instead break the edit form, which needs
	// it to show the curator what is currently set.
	if r.CoverAssetID.Valid {
		v := openapi_types.UUID(r.CoverAssetID.Bytes)
		c.CoverAssetId = &v
	}
	// #1207 — the featured rail's own cover and its focal point, on the
	// same terms as cover_asset_id above: the curator's SETTING, so the
	// edit form can show what is currently chosen. What the rail PAINTS
	// is decided by featured.ListPlacements, which re-runs the reader's
	// picture plane over each rung of the preference order; these three
	// carry no picture and make no claim about renderability.
	//
	// The focal pair is copied as-is rather than defaulted to 0.5: null
	// is "never positioned", and the editor needs that distinct from a
	// deliberate centring so its reset control knows whether there is
	// anything to reset.
	if r.FeaturedCoverAssetID.Valid {
		v := openapi_types.UUID(r.FeaturedCoverAssetID.Bytes)
		c.FeaturedCoverAssetId = &v
	}
	c.FeaturedCoverFocalX = r.FeaturedCoverFocalX
	c.FeaturedCoverFocalY = r.FeaturedCoverFocalY
	c.CoverFocalX = r.CoverFocalX
	c.CoverFocalY = r.CoverFocalY
	// #1212 — the zoom for each slot, copied as-is for the same reason
	// the focal pair is: nil is "never zoomed" and an explicit 1 is "at
	// the fit, deliberately". They render identically and the editor's
	// reset needs to tell them apart, so nothing here defaults one to
	// the other.
	c.FeaturedCoverZoom = r.FeaturedCoverZoom
	c.CoverZoom = r.CoverZoom
	return c
}

// validateFocalPair wraps [coverfocal.Validate] in this endpoint's own
// 400 envelope.
//
// #1207 wrote the refusals here because collections had the only two
// focal pairs. #1210 gave posts a third, so the rule moved to
// internal/coverfocal and this became the envelope alone. The comment
// that used to sit here said why one function served two pairs: "three
// copies of a range check is how one of them ends up admitting 1.5".
// That argument did not stop applying at two.
func validateFocalPair(
	xName, yName, clearName string,
	x, y *float64,
	clear bool,
) *openapi.UpdateCollection400JSONResponse {
	msg := coverfocal.Validate(xName, yName, clearName, x, y, clear)
	if msg == "" {
		return nil
	}
	r := openapi.UpdateCollection400JSONResponse{
		BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg},
	}
	return &r
}

// MinCoverZoom / MaxCoverZoom bound a cover crop's zoom (#1212), and
// both ends come from something other than taste.
//
// The floor is geometry: the crop window is the FIT window divided by
// the zoom, so anything below 1 asks for a window larger than the
// picture. There are no pixels out there.
//
// The ceiling comes from the preview ladder's real rungs. A cover
// carrying a crop must be painted from a CONTAIN rung — `col` is
// `fit: cover` at 320px, a square already cropped at the centre, so
// positioning applied to it crops a crop (migration 00055's warning).
// The contain rungs are `preview` 1024, `screen` 1920 and `hires` 4096
// (sysconfig/previews.go), and `preview` is the one a cover is
// GUARANTEED to have — it is what CollectionCover.preview_available
// reports. Zooming to z feeds the card 1/z of the picture's fitted
// width, so it demands z times the source pixels per CSS pixel; the
// browser answers by climbing the srcset, and 4096 is exactly four
// times 1024. At 4 the ladder still has a rung to climb to. Past 4 it
// has none, and every further step is upscaling bytes the server never
// made.
//
// The same two numbers are the column CHECK in migration 00056 and the
// slider's clamp in the editor. Three copies of one rule, which is
// deliberate: the constraint is where a broken client stops, this is
// where the caller gets a 400 it can act on instead of a constraint
// error, and the clamp is where a curator is stopped before either.
const (
	MinCoverZoom = 1.0
	MaxCoverZoom = 4.0
)

// validateZoom refuses the two shapes a cover zoom must never reach the
// database in, and it is ONE function because #1212 has two of them.
//
// Unlike the focal pair there is no "both or neither" rule to enforce —
// a zoom is one number, and it is independent of the positioning it
// travels with. What is left is the exclusivity every clear flag on
// this endpoint carries, and the range, which would otherwise arrive at
// the column CHECK as a constraint error and surface as a 500.
//
// Absent entirely is valid: "leave alone" always is.
func validateZoom(
	name, clearName string,
	z *float64,
	clear bool,
) *openapi.UpdateCollection400JSONResponse {
	bad := func(msg string) *openapi.UpdateCollection400JSONResponse {
		r := openapi.UpdateCollection400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg},
		}
		return &r
	}
	if clear && z != nil {
		return bad("send either " + name + " or " + clearName + ", not both")
	}
	if z != nil && (*z < MinCoverZoom || *z > MaxCoverZoom) {
		return bad(name + " must be between 1 and 4")
	}
	return nil
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

// pgUUIDFromPtr is pgTimestamptzFromPtr for a UUID: nil becomes the
// invalid (SQL NULL) value, which every COALESCE partial update in
// queries.sql reads as "leave this column alone".
func pgUUIDFromPtr(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
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
} = (*Handler)(nil)
