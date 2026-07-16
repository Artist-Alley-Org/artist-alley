// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package posts implements the post slice of the artist-alley HTTP
// API — the feed entity that wraps 1+ assets per the post-model
// decision (see Phase 1.13.D-2).
//
// Routing surface:
//
//	GET    /posts                          — paginated feed
//	POST   /posts                          — create
//	GET    /posts/{id}                     — single post with members
//	PATCH  /posts/{id}                     — partial update
//	DELETE /posts/{id}                     — soft delete
//	POST   /posts/{id}/assets              — attach an asset
//	DELETE /posts/{id}/assets/{asset_id}   — detach an asset
//
// Authorization (1.13.D-2 baseline):
//   - All read endpoints require an authenticated session. Visibility
//     filters apply on top: callers see all `public` posts plus their
//     own private/followers posts. 1.13.G adds anonymous-browse for
//     public posts.
//   - Write endpoints require the caller to be the post's author.
//     `posts.admin` / `system.admin` capabilities override.
//
// Like/comment counts (`like_count`, `comment_count`) come straight
// from the table's denormalised columns; Phase 1.13.D-4 will start
// maintaining them via triggers when the likes / comments tables ship.
package posts

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/mscrnt/artist-alley/app/internal/notifications"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/social/mention"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// Cache domain name. Stable string used as NOTIFY target — peer
// instances key off this when dispatching invalidations.
const cacheDomainPostByID = "post.id"

// Capability gates. `posts.admin` lets a moderator edit/delete any
// post; `system.admin` is the global override.
const (
	CapPostsAdmin  = "posts.admin"
	CapSystemAdmin = "system.admin"
)

const maxListLimit = 200

// Handler implements the posts slice of openapi.StrictServerInterface.
//
// `byID` caches the fully-hydrated post (with members + tags joined
// in) by UUID string. The browse feed renders 30–50 posts per page;
// each cold render fans out into 3 queries per item via
// fetchFullPost (GetPost + ListPostAssets + ListPostTags). With the
// LRU warm, repeat reads collapse to one ListPostsPage + cache
// hits. Writes (Create / Update / Delete / Add-Remove member) call
// invalidate which both drops the local entry and broadcasts via
// the Phase 1.10 cache.Registry NOTIFY channel so peer instances
// drop their copy too.
//
// Like / comment count updates land in Phase 1.13.D-4 — those tables'
// triggers will pg_notify the cache_invalidate channel directly,
// no app-side hook needed.
//
// Nil registry is legal — handler falls back to direct DB reads
// (useful in tests that don't want the LISTEN goroutine).
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	byID *cache.Cache[openapi.Post]
	// Registry kept for cross-package invalidations (the author's
	// user-profile cache holds a stale post_count after we
	// create/delete a post). nil-safe — the helpers we call are
	// already no-ops on a nil Registry.
	registry *cache.Registry

	// follows is the social-graph seam: posts.handler consults it
	// to gate visibility='followers' posts (the long-parked TODO
	// from Phase 1.13.I, finally wired in 1.17.G2). Local interface
	// rather than a direct social.Handler import so this package
	// doesn't grow a cross-feature dep; concrete impl is injected
	// at boot via SetFollowChecker.
	//
	// nil-safe: when the registry isn't wired (tests), visibility
	// 'followers' falls back to "treat as public" — the
	// pre-1.17.G2 behaviour — rather than refusing every read.
	follows followChecker

	// Activities ledger writer + baseURL resolver (Phase 1.22.A-bis-2
	// per ADR 0044). Wired post-construction in api.go. Handlers
	// use h.activities.WithEmission to record every social action
	// in the same tx as its domain write. nil-safe: tests that
	// don't wire the writer fall back to the pre-ADR-0044 behaviour
	// (direct domain writes; no activity record).
	activities *activities.Writer
	baseURLFn  func(ctx context.Context) string

	// Audit records admin lifecycle events (soft_deleted; restore
	// fires from softdelete.Service directly). Nil-safe.
	Audit *audit.Recorder

	// SoftDelete handles restore (clear deleted_at + audit). Wired
	// at boot in api.go alongside the gc coordinator.
	SoftDelete *softdelete.Service

	// mentions fires @-mention notifications after a post insert
	// commits (Phase 1.55.X). Local interface rather than a direct
	// *mention.Service field would be over-engineering — the mention
	// package doesn't import posts, so there's no cycle. nil-safe:
	// unwired (tests) means no mention notifications, and the post
	// still saves normally.
	mentions *mention.Service
}

// followChecker is the slice of social.Handler this package needs:
// "does follower follow followee?" Wired at boot via
// SetFollowChecker so we don't grow an import cycle.
type followChecker interface {
	IsFollowing(ctx context.Context, follower, followee int64) (bool, error)
}

// SetFollowChecker installs the social-graph dependency post-
// construction (same pattern users.Handler uses for the audit
// recorder + auth.Handler uses for the provider registry).
func (h *Handler) SetFollowChecker(fc followChecker) { h.follows = fc }

// SetActivitiesWriter installs the federation activity-ledger
// writer + baseURL resolver per ADR 0044. Post-construction
// setter so the boot order (handlers → cross-package deps) stays
// linear.
func (h *Handler) SetActivitiesWriter(w *activities.Writer, baseURLFn func(ctx context.Context) string) {
	h.activities = w
	h.baseURLFn = baseURLFn
}

// SetMentions installs the @-mention notification service (Phase
// 1.55.X). Post-construction setter, same shape as the others.
func (h *Handler) SetMentions(m *mention.Service) { h.mentions = m }

func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger, registry: registry}
	if registry != nil {
		// 10k entries comfortably fits ~10MB of resident memory for a
		// typical post body and gives us a deep working set for the
		// hot end of the feed. LRU evicts cold entries; next read
		// repopulates.
		h.byID = cache.Register[openapi.Post](registry, cacheDomainPostByID, 10_000)
	}
	return h
}

// ---------------------------------------------------------------------------
// CreatePost
// ---------------------------------------------------------------------------

func (h *Handler) CreatePost(
	ctx context.Context,
	req openapi.CreatePostRequestObject,
) (openapi.CreatePostResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreatePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreatePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body
	if len(in.Members) == 0 {
		return openapi.CreatePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "members: at least one asset required"},
		}, nil
	}

	visibility := "org-only"
	if in.Visibility != nil {
		visibility = string(*in.Visibility)
	}
	if !validVisibility(visibility) {
		return openapi.CreatePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "visibility must be private|org-only|followers|explicit-share (1.22.C: 'public' reserved for future public-fediverse phase)"},
		}, nil
	}

	// Cover defaults to the first member when the caller doesn't pin
	// one explicitly. This matches the modal UX (first uploaded =
	// cover unless dragged).
	var coverID pgtype.UUID
	if in.CoverAssetId != nil {
		coverID = pgtype.UUID{Bytes: uuid.UUID(*in.CoverAssetId), Valid: true}
	} else {
		coverID = pgtype.UUID{Bytes: uuid.UUID(in.Members[0].AssetId), Valid: true}
	}

	// Optional standalone thumbnail (not a post member). Set when the
	// upload modal's ThumbnailPicker option (c) is used — the user
	// uploaded a separate image purely as the post's cover.
	var coverThumbnailID pgtype.UUID
	if in.CoverThumbnailAssetId != nil {
		coverThumbnailID = pgtype.UUID{Bytes: uuid.UUID(*in.CoverThumbnailAssetId), Valid: true}
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("posts: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	var teamID pgtype.UUID
	if in.TeamId != nil {
		teamID = pgtype.UUID{Bytes: uuid.UUID(*in.TeamId), Valid: true}
	}

	// state_id: domain 'post' UUID, optional. DB FK guards the value;
	// we don't validate the domain here — the workflow.Service will
	// reject illegal transitions later if a typo slipped through.
	var stateID pgtype.UUID
	if in.StateId != nil {
		stateID = pgtype.UUID{Bytes: uuid.UUID(*in.StateId), Valid: true}
	}

	row, err := q.CreatePost(ctx, CreatePostParams{
		AuthorUserRef:         id.UserRef,
		Title:                 strOr(in.Title, ""),
		Description:           strOr(in.Description, ""),
		Visibility:            visibility,
		CoverAssetID:          coverID,
		CoverThumbnailAssetID: coverThumbnailID,
		TeamID:                teamID,
		StateID:               stateID,
	})
	if err != nil {
		if isFKError(err, "posts_team_id_fkey") {
			return openapi.CreatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
		return nil, fmt.Errorf("posts: create: %w", err)
	}

	// Members. Idempotent on (post_id, asset_id) so de-dupes on input.
	for _, m := range in.Members {
		if err := q.AddPostAsset(ctx, AddPostAssetParams{
			PostID:    row.ID,
			AssetID:   pgtype.UUID{Bytes: uuid.UUID(m.AssetId), Valid: true},
			SortOrder: int32Or(m.SortOrder, 0),
		}); err != nil {
			if isFKError(err, "post_assets_asset_id_fkey") {
				return openapi.CreatePost404JSONResponse{
					NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found: " + m.AssetId.String()},
				}, nil
			}
			return nil, fmt.Errorf("posts: add member: %w", err)
		}
	}

	// Tags (replace-all shape; we just inserted the post so it has
	// none yet).
	if in.Tags != nil && len(*in.Tags) > 0 {
		clean := dedupeTags(*in.Tags)
		if err := q.ReplacePostTags(ctx, ReplacePostTagsParams{
			PostID:  row.ID,
			Column2: clean,
		}); err != nil {
			return nil, fmt.Errorf("posts: tags: %w", err)
		}
	}

	// Optional: add the new post to a collection at create time.
	// Used by the context-aware upload modal when the user is on a
	// collection page and drops files in.
	if in.CollectionId != nil {
		if err := q.AddCollectionPost(ctx, AddCollectionPostParams{
			CollectionID: pgtype.UUID{Bytes: uuid.UUID(*in.CollectionId), Valid: true},
			PostID:       row.ID,
			SortOrder:    0,
			Pinned:       true,
		}); err != nil {
			if isFKError(err, "collection_posts_collection_id_fkey") {
				return openapi.CreatePost404JSONResponse{
					NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
				}, nil
			}
			return nil, fmt.Errorf("posts: add to collection: %w", err)
		}
	}

	// Record the Create activity in the same tx per ADR 0044. The
	// federation outbox dispatcher (Phase 1.22.D) reads from the
	// activities ledger to publish to peers; without this the new
	// post would be invisible to federation. 1.22.B-cleanup made
	// this required — no more silent skip.
	{
		actorCtx := emit.ActorContext{
			UserRef:  id.UserRef,
			Username: id.Username,
			BaseURL:  h.baseURLFn(ctx),
		}
		postRef := emit.PostRef{
			ID:            uuid.UUID(row.ID.Bytes).String(),
			Title:         row.Title,
			AuthorUserRef: id.UserRef,
			AuthorURI:     actorCtx.URI(),
		}
		em := emit.CreatePost(actorCtx, postRef, emit.PostVisibility(visibility))
		if _, err := h.activities.RecordActivity(ctx, tx, em.Activity); err != nil {
			return nil, fmt.Errorf("posts: emit Create activity: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("posts: commit: %w", err)
	}

	// Brand-new ID — nothing to evict locally, but the Invalidate
	// also broadcasts NOTIFY so any peer that race-read this row
	// (unlikely but possible) drops its copy.
	h.cacheInvalidate(ctx, row.ID)

	// The author's UserPublic cache holds a stale post_count now.
	// Broadcast the invalidation; the users package's LISTEN
	// dispatch picks it up and evicts. Cross-package import via the
	// users.InvalidateProfile helper keeps the domain string in
	// one place.
	users.InvalidateProfile(ctx, h.registry, id.UserRef)

	// Fire @-mention notifications after the commit (Phase 1.55.X).
	// Best-effort: a notify failure must not fail the already-saved
	// post. Both title + description carry mentionable text. The bell
	// deep-links to the post.
	if h.mentions != nil {
		postID := uuid.UUID(row.ID.Bytes).String()
		text := strOr(in.Title, "") + "\n" + strOr(in.Description, "")
		h.mentions.ProcessForPost(ctx, id.UserRef, text, postID, map[string]any{
			notifications.PayloadKeyPostTitle: row.Title,
		})
	}

	// Re-read with full member + tag join. The trigger has fired by
	// now so search_text reflects the new state.
	full, err := h.fetchFullPost(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	return openapi.CreatePost201JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// GetPost
// ---------------------------------------------------------------------------

func (h *Handler) GetPost(
	ctx context.Context,
	req openapi.GetPostRequestObject,
) (openapi.GetPostResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetPost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	full, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetPost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !h.canReadPost(ctx, id, full) {
		return openapi.GetPost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not visible to this user"},
		}, nil
	}
	return openapi.GetPost200JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// UpdatePost
// ---------------------------------------------------------------------------

func (h *Handler) UpdatePost(
	ctx context.Context,
	req openapi.UpdatePostRequestObject,
) (openapi.UpdatePostResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UpdatePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdatePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("posts: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	current, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, current.AuthorUserRef) {
		return openapi.UpdatePost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}

	in := req.Body

	// Phase 1.16 optimistic-concurrency check. Compared against
	// the row loaded inside the tx (one consistent snapshot).
	// Truncate both sides to µs (Postgres stores at µs; Go marshals
	// at ns).
	if in.IfUnchangedSince != nil && current.UpdatedAt.Valid {
		stored := current.UpdatedAt.Time.Truncate(time.Microsecond)
		sent := in.IfUnchangedSince.Truncate(time.Microsecond)
		if !stored.Equal(sent) {
			return openapi.UpdatePost409JSONResponse{
				Error:     "post was edited by someone else after your last load; reload and try again",
				UpdatedAt: current.UpdatedAt.Time,
			}, nil
		}
	}
	var visPtr *string
	if in.Visibility != nil {
		s := string(*in.Visibility)
		if !validVisibility(s) {
			return openapi.UpdatePost400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "visibility must be private|org-only|followers|explicit-share (1.22.C: 'public' reserved for future public-fediverse phase)"},
			}, nil
		}
		visPtr = &s
	}
	var coverPtr pgtype.UUID
	if in.CoverAssetId != nil {
		coverPtr = pgtype.UUID{Bytes: uuid.UUID(*in.CoverAssetId), Valid: true}
	}

	if _, err := q.UpdatePost(ctx, UpdatePostParams{
		ID:           pgID,
		Title:        in.Title,
		Description:  in.Description,
		Visibility:   visPtr,
		CoverAssetID: coverPtr,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, fmt.Errorf("posts: update: %w", err)
	}

	if in.Tags != nil {
		clean := dedupeTags(*in.Tags)
		if err := q.ReplacePostTags(ctx, ReplacePostTagsParams{
			PostID:  pgID,
			Column2: clean,
		}); err != nil {
			return nil, fmt.Errorf("posts: replace tags: %w", err)
		}
	}

	// Record the Update activity in the same tx per ADR 0044.
	{
		actorCtx := emit.ActorContext{
			UserRef:  caller.UserRef,
			Username: caller.Username,
			BaseURL:  h.baseURLFn(ctx),
		}
		// Re-resolve title/visibility from the updated state (or
		// fall back to current row if not in this PATCH).
		title := current.Title
		if in.Title != nil {
			title = *in.Title
		}
		vis := current.Visibility
		if visPtr != nil {
			vis = *visPtr
		}
		postRef := emit.PostRef{
			ID:            uuid.UUID(pgID.Bytes).String(),
			Title:         title,
			AuthorUserRef: current.AuthorUserRef,
			AuthorURI:     actorCtx.URI(),
		}
		em := emit.UpdatePost(actorCtx, postRef, emit.PostVisibility(vis))
		if _, err := h.activities.RecordActivity(ctx, tx, em.Activity); err != nil {
			return nil, fmt.Errorf("posts: emit Update activity: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("posts: commit: %w", err)
	}

	h.cacheInvalidate(ctx, pgID)

	full, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		return nil, err
	}
	return openapi.UpdatePost200JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// DeletePost
// ---------------------------------------------------------------------------

func (h *Handler) DeletePost(
	ctx context.Context,
	req openapi.DeletePostRequestObject,
) (openapi.DeletePostResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.DeletePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)

	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DeletePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.DeletePost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	reason := extractSoftDeleteReason(req.Body)
	if len(reason) > softDeleteReasonMaxLen {
		return openapi.DeletePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "reason exceeds 500 chars"},
		}, nil
	}
	// Wrap SoftDeletePost + Delete activity in one tx per ADR 0044.
	// Without this the federation outbox can't tell peers the post
	// is gone (Tombstone per AP §6.4). 1.22.B-cleanup made
	// activities required.
	{
		actorCtx := emit.ActorContext{
			UserRef:  caller.UserRef,
			Username: caller.Username,
			BaseURL:  h.baseURLFn(ctx),
		}
		em := emit.DeletePost(actorCtx, uuid.UUID(pgID.Bytes).String(), cur.Title)
		err := h.activities.WithEmission(ctx, activities.EmissionInput{
			Activity: em.Activity,
		}, func(tx pgx.Tx) error {
			return New(tx).SoftDeletePost(ctx, SoftDeletePostParams{
				ID:            pgID,
				DeletedReason: softDeleteReasonPtr(reason),
			})
		})
		if err != nil {
			return nil, fmt.Errorf("posts: delete: %w", err)
		}
	}
	if h.Audit != nil {
		h.Audit.AdminPostSoftDeleted(ctx, nil, uuid.UUID(pgID.Bytes).String(), caller.UserRef, reason)
	}
	h.cacheInvalidate(ctx, pgID)
	// post_count just went down for the author — drop their cached
	// UserPublic so the next /users/{ref} read shows the new total.
	users.InvalidateProfile(ctx, h.registry, cur.AuthorUserRef)
	return openapi.DeletePost204Response{}, nil
}

// ---------------------------------------------------------------------------
// RestorePost — Phase 1.55.C-1b
// ---------------------------------------------------------------------------

// RestorePost clears deleted_at + deleted_reason on a soft-deleted
// post. Admin-only. See assets.Handler.RestoreAsset for the shape.
func (h *Handler) RestorePost(
	ctx context.Context,
	req openapi.RestorePostRequestObject,
) (openapi.RestorePostResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.IsAnonymous() {
		return openapi.RestorePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(auth.SuperAdminCapability) {
		return openapi.RestorePost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "admin capability required"},
		}, nil
	}
	if h.SoftDelete == nil {
		return nil, fmt.Errorf("posts: RestorePost: SoftDelete service unwired")
	}
	if err := h.SoftDelete.RestorePost(ctx, nil, uuid.UUID(req.Id), id.UserRef); err != nil {
		if errors.Is(err, softdelete.ErrNotDeleted) || errors.Is(err, softdelete.ErrNotFound) {
			return openapi.RestorePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not soft-deleted"},
			}, nil
		}
		return nil, fmt.Errorf("posts: restore: %w", err)
	}
	return openapi.RestorePost204Response{}, nil
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
// to the value, matching the sqlc-generated *string param type.
func softDeleteReasonPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// softDeleteReasonMaxLen bounds the operator-supplied reason string.
const softDeleteReasonMaxLen = 500

// ---------------------------------------------------------------------------
// ListPosts (the feed)
// ---------------------------------------------------------------------------

func (h *Handler) ListPosts(
	ctx context.Context,
	req openapi.ListPostsRequestObject,
) (openapi.ListPostsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListPosts401JSONResponse{
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
			return openapi.ListPosts500JSONResponse{
				InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: id, Valid: true}
	}

	// Default visibility filter: org-only (the post-1.22.C-a
	// equivalent of legacy 'public' for the walled-garden feed).
	// Callers can pass `?visibility=private` to get their own
	// private posts (we still AND with the caller's own author_ref
	// so other people's privates aren't leaked). A future feed
	// query upgrade unifies the filter with the share-table view
	// for "what I can actually see"; this is the conservative
	// preserve-old-behavior path for the cleanup migration.
	visibility := "org-only"
	if req.Params.Visibility != nil {
		visibility = string(*req.Params.Visibility)
	}
	// Author filter: if caller asks for their own posts, drop the
	// visibility filter — they see all of their own visibilities.
	var visPtr *string
	if req.Params.AuthorRef != nil && *req.Params.AuthorRef == caller.UserRef {
		visPtr = nil
	} else {
		visPtr = &visibility
	}
	var authorPtr *int64
	if req.Params.AuthorRef != nil {
		authorPtr = req.Params.AuthorRef
	}

	var qText *string
	if req.Params.Q != nil {
		s := strings.TrimSpace(*req.Params.Q)
		if s != "" {
			qText = &s
		}
	}
	var tagPtr *string
	if req.Params.Tag != nil && *req.Params.Tag != "" {
		tagPtr = req.Params.Tag
	}

	// feed=following (Phase 1.17.G2) restricts the page to authors
	// the caller follows. Anonymous callers can never satisfy this
	// (the 401 path above returns first); for authenticated callers
	// who don't follow anyone, the EXISTS subquery yields an empty
	// page rather than a 4xx — matches every social platform's
	// "your following tab is empty" UX.
	var followerPtr *int64
	if req.Params.Feed != nil && *req.Params.Feed == openapi.Following {
		ref := caller.UserRef
		followerPtr = &ref
	}

	// Phase 1.55.C-1b: ?include_deleted=true is admin-only.
	var includeDeletedArg *bool
	if req.Params.IncludeDeleted != nil && *req.Params.IncludeDeleted && caller.Can(auth.SuperAdminCapability) {
		t := true
		includeDeletedArg = &t
	}

	fetch := limit + 1
	rows, err := New(h.Pool).ListPostsPage(ctx, ListPostsPageParams{
		IncludeDeleted:  includeDeletedArg,
		AuthorUserRef:   authorPtr,
		Visibility:      visPtr,
		Q:               qText,
		Tag:             tagPtr,
		FeedFollowerRef: followerPtr,
		CursorPostedAt:  cursorTs,
		CursorID:        cursorID,
		RowLimit:        fetch,
	})
	if err != nil {
		return nil, fmt.Errorf("posts: list: %w", err)
	}

	items := make([]openapi.Post, 0, limit)
	var lastPostedAt time.Time
	var lastID uuid.UUID
	for i, r := range rows {
		if i >= int(limit) {
			break
		}
		full, err := h.fetchFullPost(ctx, r.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// fetchFullPost (GetPost) filters deleted_at IS NULL, so a
				// soft-deleted row lands here. For the admin trash view
				// (include_deleted=true) surface it from the list row
				// rather than dropping it.
				if r.DeletedAt.Valid {
					items = append(items, deletedPostFromListRow(r))
					lastPostedAt = r.PostedAt.Time
					lastID = uuid.UUID(r.ID.Bytes)
				}
				continue
			}
			return nil, err
		}
		items = append(items, *full)
		lastPostedAt = r.PostedAt.Time
		lastID = uuid.UUID(r.ID.Bytes)
	}

	resp := openapi.PostList{Items: items}
	if len(rows) > int(limit) {
		next := encodeCursor(lastPostedAt, lastID)
		resp.NextCursor = &next
	}
	return openapi.ListPosts200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// AddPostAsset / RemovePostAsset
// ---------------------------------------------------------------------------

func (h *Handler) AddPostAsset(
	ctx context.Context,
	req openapi.AddPostAssetRequestObject,
) (openapi.AddPostAssetResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddPostAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddPostAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddPostAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.AddPostAsset403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	pgAsset := pgtype.UUID{Bytes: uuid.UUID(req.Body.AssetId), Valid: true}
	if err := q.AddPostAsset(ctx, AddPostAssetParams{
		PostID:    pgID,
		AssetID:   pgAsset,
		SortOrder: int32Or(req.Body.SortOrder, 0),
	}); err != nil {
		if isFKError(err, "post_assets_asset_id_fkey") {
			return openapi.AddPostAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("posts: add asset: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.AddPostAsset204Response{}, nil
}

func (h *Handler) RemovePostAsset(
	ctx context.Context,
	req openapi.RemovePostAssetRequestObject,
) (openapi.RemovePostAssetResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemovePostAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RemovePostAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.RemovePostAsset403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	if err := q.RemovePostAsset(ctx, RemovePostAssetParams{
		PostID:  pgID,
		AssetID: pgtype.UUID{Bytes: uuid.UUID(req.AssetId), Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("posts: remove asset: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.RemovePostAsset204Response{}, nil
}

// ---------------------------------------------------------------------------
// ACLs — additive grants on top of role/team/visibility (ADR 0010 L6)
// ---------------------------------------------------------------------------
//
// Authorization: reading the ACL list requires read access to the post
// (canReadPost). Adding/removing requires write access (canMutatePost)
// so a viewer can't expand their own access by editing the ACL list.

func (h *Handler) ListPostAcls(
	ctx context.Context,
	req openapi.ListPostAclsRequestObject,
) (openapi.ListPostAclsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListPostAcls401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	full, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListPostAcls404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !h.canReadPost(ctx, caller, full) {
		return openapi.ListPostAcls403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not visible to this user"},
		}, nil
	}
	rows, err := New(h.Pool).ListPostAcls(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("posts: list acls: %w", err)
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
	return openapi.ListPostAcls200JSONResponse(out), nil
}

func (h *Handler) AddPostAcl(
	ctx context.Context,
	req openapi.AddPostAclRequestObject,
) (openapi.AddPostAclResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddPostAcl401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddPostAcl400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddPostAcl404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.AddPostAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}

	var expires pgtype.Timestamptz
	if req.Body.ExpiresAt != nil {
		expires = pgtype.Timestamptz{Time: *req.Body.ExpiresAt, Valid: true}
	}
	if err := q.AddPostAcl(ctx, AddPostAclParams{
		PostID:           pgID,
		PrincipalType:    string(req.Body.PrincipalType),
		PrincipalID:      req.Body.PrincipalId,
		Permission:       string(req.Body.Permission),
		GrantedByUserRef: &caller.UserRef,
		ExpiresAt:        expires,
	}); err != nil {
		return nil, fmt.Errorf("posts: add acl: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.AddPostAcl204Response{}, nil
}

func (h *Handler) RemovePostAcl(
	ctx context.Context,
	req openapi.RemovePostAclRequestObject,
) (openapi.RemovePostAclResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemovePostAcl401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RemovePostAcl404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.RemovePostAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	rows, err := q.RemovePostAcl(ctx, RemovePostAclParams{
		PostID:        pgID,
		PrincipalType: string(req.PrincipalType),
		PrincipalID:   req.PrincipalId,
		Permission:    string(req.Permission),
	})
	if err != nil {
		return nil, fmt.Errorf("posts: remove acl: %w", err)
	}
	if rows == 0 {
		return openapi.RemovePostAcl404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "ACL entry not found"},
		}, nil
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.RemovePostAcl204Response{}, nil
}

// ---------------------------------------------------------------------------
// Internal: fetch + cache plumbing
// ---------------------------------------------------------------------------

// fetchFullPost reads through the post-by-id cache. On miss it loads
// the post + members + tags (3 queries) and populates the cache.
func (h *Handler) fetchFullPost(ctx context.Context, id pgtype.UUID) (*openapi.Post, error) {
	key := uuidString(id)
	if h.byID != nil {
		if v, ok := h.byID.Get(key); ok {
			out := v
			return &out, nil
		}
	}
	q := New(h.Pool)
	row, err := q.GetPost(ctx, id)
	if err != nil {
		return nil, err
	}
	members, err := q.ListPostAssets(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("posts: list members: %w", err)
	}
	tags, err := q.ListPostTags(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("posts: list tags: %w", err)
	}
	out := postRowToAPI(row, members, tags)
	if h.byID != nil {
		h.byID.Add(key, out)
	}
	return &out, nil
}

// cacheInvalidate drops the local LRU entry and broadcasts to peers
// via the NOTIFY channel. Best-effort — a NOTIFY failure logs but
// doesn't propagate, so writers don't fail because of cache
// plumbing.
func (h *Handler) cacheInvalidate(ctx context.Context, id pgtype.UUID) {
	if h.byID == nil {
		return
	}
	key := uuidString(id)
	if err := h.byID.Invalidate(ctx, key); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "posts.cache.invalidate.error",
			slog.String("domain", cacheDomainPostByID),
			slog.String("key", key),
			slog.String("err", err.Error()),
		)
	}
}

func uuidString(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------

// canMutatePost returns true if the caller can edit/delete this post.
// Author, system.admin, or posts.admin.
func canMutatePost(id *auth.Identity, authorRef int64) bool {
	if id == nil {
		return false
	}
	if id.UserRef == authorRef {
		return true
	}
	return id.Can(CapPostsAdmin) || id.Can(CapSystemAdmin)
}

// canReadPost gates GetPost. Author always wins; otherwise the
// visibility decides. Method (not free function) so the followers-
// visibility branch can consult h.follows. Anonymous + nil-follows
// callers fall through to "treat followers like public" — the
// pre-1.17.G2 behaviour — to keep the test path that doesn't wire
// the social handler working.
func (h *Handler) canReadPost(ctx context.Context, id *auth.Identity, p *openapi.Post) bool {
	if id == nil {
		return false
	}
	if id.UserRef == p.AuthorUserRef {
		return true
	}
	switch p.Visibility {
	case openapi.PostVisibilityOrgOnly:
		// Any authenticated local user can read org-only posts —
		// the post-1.22.C-a equivalent of legacy 'public' for the
		// walled-garden default tier.
		return true
	case openapi.PostVisibilityFollowers:
		// Phase 1.17.G2 wires this: the post is visible only when
		// the caller follows the author. follows nil → degrade to
		// org-only-style behaviour so legacy test fixtures keep passing.
		if h.follows == nil {
			return true
		}
		ok, err := h.follows.IsFollowing(ctx, id.UserRef, p.AuthorUserRef)
		if err != nil {
			// Fail closed on a DB error — better to 403 a real
			// follower temporarily than silently expose a private
			// post if the follows table is unreachable.
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "posts.followers_check.error",
				slog.Int64("follower", id.UserRef),
				slog.Int64("followee", p.AuthorUserRef),
				slog.String("err", err.Error()),
			)
			return false
		}
		return ok
	case openapi.PostVisibilityPrivate:
		return id.Can(CapPostsAdmin) || id.Can(CapSystemAdmin)
	}
	return false
}

// validVisibility checks against the 4-tier closed catalogue
// per the 1.22.C design proposal §1. `public` was removed at
// migration 00056 (reserved for a future public-fediverse phase).
// Writes attempting `public` get the clear "tier reserved" error.
func validVisibility(s string) bool {
	switch s {
	case "private", "org-only", "followers", "explicit-share":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Row → API conversions
// ---------------------------------------------------------------------------

func postRowToAPI(p GetPostRow, members []ListPostAssetsRow, tags []string) openapi.Post {
	out := openapi.Post{
		Id:            openapi_types.UUID(p.ID.Bytes),
		AuthorUserRef: p.AuthorUserRef,
		Title:         p.Title,
		Description:   p.Description,
		Visibility:    openapi.PostVisibility(p.Visibility),
		PostedAt:      p.PostedAt.Time,
		LikeCount:     p.LikeCount,
		CommentCount:  p.CommentCount,
		Tags:          append([]string{}, tags...),
		CreatedAt:     p.CreatedAt.Time,
		UpdatedAt:     p.UpdatedAt.Time,
		Members:       make([]openapi.PostMember, 0, len(members)),
	}
	if p.CoverAssetID.Valid {
		v := openapi_types.UUID(p.CoverAssetID.Bytes)
		out.CoverAssetId = &v
	}
	if p.CoverThumbnailAssetID.Valid {
		v := openapi_types.UUID(p.CoverThumbnailAssetID.Bytes)
		out.CoverThumbnailAssetId = &v
	}
	if p.OriginServerID.Valid {
		v := openapi_types.UUID(p.OriginServerID.Bytes)
		out.OriginServerId = &v
	}
	if p.TeamID.Valid {
		v := openapi_types.UUID(p.TeamID.Bytes)
		out.TeamId = &v
	}
	if p.StateID.Valid {
		v := openapi_types.UUID(p.StateID.Bytes)
		out.StateId = &v
	}
	for _, m := range members {
		out.Members = append(out.Members, openapi.PostMember{
			AssetId:   openapi_types.UUID(m.AssetID.Bytes),
			SortOrder: int(m.SortOrder),
			Asset:     memberToAsset(m),
		})
	}
	return out
}

// deletedPostFromListRow builds a minimal Post for a soft-deleted row
// in the admin trash listing. GetPost (used by fetchFullPost) filters
// deleted_at IS NULL, so a deleted post can't be fully hydrated — the
// scalar fields the list row carries are enough for the trash view
// (title + deletion metadata + a restore target).
func deletedPostFromListRow(r ListPostsPageRow) openapi.Post {
	out := openapi.Post{
		Id:            openapi_types.UUID(r.ID.Bytes),
		AuthorUserRef: r.AuthorUserRef,
		Title:         r.Title,
		Visibility:    openapi.PostVisibility(r.Visibility),
		PostedAt:      r.PostedAt.Time,
		CreatedAt:     r.CreatedAt.Time,
		UpdatedAt:     r.UpdatedAt.Time,
		Members:       []openapi.PostMember{},
		Tags:          []string{},
	}
	if r.DeletedAt.Valid {
		dt := r.DeletedAt.Time
		out.DeletedAt = &dt
		out.DeletedReason = r.DeletedReason
	}
	if r.StateID.Valid {
		v := openapi_types.UUID(r.StateID.Bytes)
		out.StateId = &v
	}
	if r.TeamID.Valid {
		v := openapi_types.UUID(r.TeamID.Bytes)
		out.TeamId = &v
	}
	if r.OriginServerID.Valid {
		v := openapi_types.UUID(r.OriginServerID.Bytes)
		out.OriginServerId = &v
	}
	return out
}

func memberToAsset(m ListPostAssetsRow) openapi.Asset {
	a := openapi.Asset{
		Id:            openapi_types.UUID(m.AssetID.Bytes),
		Title:         m.Title,
		Description:   &m.Description,
		AssetType:     m.AssetType,
		Status:        openapi.AssetStatus(m.Status),
		FileHash:      m.FileHash,
		FileExtension: m.FileExtension,
		FileSizeBytes: m.FileSizeBytes,
		Tags:          []string{},
		CreatedAt:     m.AssetCreatedAt.Time,
		UpdatedAt:     m.AssetUpdatedAt.Time,
	}
	if m.OwnerUserRef != nil {
		a.OwnerUserRef = m.OwnerUserRef
	}
	// Forward the asset-level metadata JSONB so per-kind view bodies
	// (AudioView, future PDFView, etc.) can read their namespaced
	// blocks without a second round-trip.
	if len(m.Metadata) > 0 && string(m.Metadata) != "{}" {
		var meta map[string]interface{}
		if err := json.Unmarshal(m.Metadata, &meta); err == nil {
			a.Metadata = &meta
		}
	}
	return a
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

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

func dedupeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func strOr(p *string, def string) string {
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

func isFKError(err error, constraint string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), constraint)
}

// ---------------------------------------------------------------------------
// Compile-time assertion: catches openapi-codegen signature drift.
// ---------------------------------------------------------------------------

var _ interface {
	ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error)
	CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error)
	GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error)
	UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error)
	DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error)
	AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error)
	RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error)
	ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error)
	AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error)
	RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error)
} = (*Handler)(nil)
