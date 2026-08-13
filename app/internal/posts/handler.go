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
	"strconv"
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
	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/notifications"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/social/mention"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/users"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Cache domain name. Stable string used as NOTIFY target — peer
// instances key off this when dispatching invalidations.
// cacheDomainPostByID is cache.DomainPostByID. Aliased rather than
// re-spelled: the constant moved to the cache package because `social`
// has to invalidate this domain too and cannot import this one (see
// cache.DomainPostByID for the whole argument).
const cacheDomainPostByID = cache.DomainPostByID

// Capability gates. `posts.admin` lets a moderator edit/delete any
// post; `system.admin` is the global override.
//
// Aliases of the shared package's constants rather than fresh literals:
// the post READ rule consults the same two codes from inside
// visibility (#873), and two spellings of a capability code is the same
// class of defect as two spellings of the rule that reads it.
const (
	CapPostsAdmin  = visibility.PostsAdmin
	CapSystemAdmin = visibility.SystemAdmin
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

	// Activities ledger writer + baseURL resolver (Phase 1.22.A-bis-2
	// per ADR 0044). Wired post-construction in api.go. Handlers
	// use h.activities.WithEmission to record every social action
	// in the same tx as its domain write. nil-safe: tests that
	// don't wire the writer fall back to the pre-ADR-0044 behaviour
	// (direct domain writes; no activity record).
	activities *activities.Writer
	baseURLFn  func(ctx context.Context) string

	// previewLadder reports the operator's CONFIGURED preview variant
	// keys, cached (#591). Feeds member.asset.ladder_available so the
	// browse grid can build a responsive srcset instead of serving one
	// 320px square at every viewport.
	//
	// nil-safe, and nil means "no ladder" — ladder_available comes back
	// false and the client keeps using the single `col` rung it already
	// knows exists. Tests that don't wire it get the conservative
	// answer rather than a panic or a wrong true.
	previewLadder sysconfig.PreviewLadderReader

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

	// notifier fires the "someone shared a post with you" notification
	// from AddPostAcl (#875). Same local-interface shape social and
	// messages use, and at boot it is the SAME socialNotifyAdapter over
	// the one notifications.Writer, so shares inherit the block and
	// channel-preference gating every other verb goes through.
	//
	// nil-safe: unwired (tests that don't care) means the grant lands
	// silently, which is precisely the pre-#875 behaviour rather than a
	// panic.
	notifier notifier

	// feedFilters resolves the caller's browse-feed content filters
	// (#891) — today just "hide restricted members". See
	// feed_filters.go for the seam and why nil means "filter nothing".
	feedFilters feedFilterReader
}

// notifier is the notifications.Writer slice this package needs.
// Declared locally so posts doesn't import notifications for the writer
// itself (it already imports the package for the verb + payload-key
// constants, which are just strings).
type notifier interface {
	Notify(ctx context.Context, recipient int64, actor *int64, verb, targetKind, targetID string, payload map[string]any) error
}

// The social-graph seam this package used to carry (followChecker /
// SetFollowChecker, wired at boot from social.Handler) is gone. It
// existed so canReadPost could ask "does the caller follow the author?"
// in Go, and it came with a nil-safe fallback that treated the
// `followers` tier as public whenever the seam was unwired — a fixture
// convenience that would have opened every followers-tier post on any
// boot-order slip. The follow check is now a conjunct of the one read
// rule (visibility.postReadableExpr), evaluated against user_follows in
// the same query that returns the rows, so there is nothing to inject
// and nothing to degrade to.

// SetPreviewLadder installs the cached configured-ladder reader (#591).
func (h *Handler) SetPreviewLadder(r sysconfig.PreviewLadderReader) { h.previewLadder = r }

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

// SetNotifier installs the cross-package notifications writer (#875).
// Post-construction setter, same shape as social.Handler's.
func (h *Handler) SetNotifier(n notifier) { h.notifier = n }

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

	// #922 — the member gate, widened to the covers by #941. Every
	// asset the BODY names has to be one this caller can actually read,
	// and it runs BEFORE the transaction opens so a refusal never
	// writes a post row it then has to roll back.
	//
	// The refusal is the same shape as the FK-violation 404 below,
	// deliberately: an unreadable asset and a nonexistent one must be
	// indistinguishable, or POST /posts becomes a UUID-existence probe.
	//
	// The covers are here rather than in a check of their own because
	// the rule has exactly one home (visibility.CanAttachAsset, ADR
	// 0064) and consolidating it there was the whole point of #922.
	// Only the EXPLICIT covers are added: the implicit cover is
	// members[0], already in this list, and re-gating it would just
	// double the query count on the common path.
	for _, aid := range attachablesOf(in) {
		ok, gErr := h.mayAttachAsset(ctx, id, aid)
		if gErr != nil {
			return nil, fmt.Errorf("posts: member gate: %w", gErr)
		}
		if !ok {
			return openapi.CreatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found: " + aid.String()},
			}, nil
		}
	}

	// #954 — the team gate. `team_id` used to be taken verbatim from the
	// body with the FOREIGN KEY as its only validation, so any EXISTING
	// team id was accepted: a post could be attributed to any studio on
	// the instance, which also handed that team's `posts.admin` holders
	// edit and delete rights over it.
	//
	// The rule has one home — visibility.CanAssignToTeam, shared with
	// assets.CreateAsset (#953) — and the refusal is deliberately the
	// SAME 404 the FK violation below answers with, so an unauthorised
	// team and a nonexistent one are indistinguishable and POST /posts
	// does not become a team-existence probe.
	//
	// The grant half asks about `posts.admin`: that is already the "I
	// manage this team's posts" claim, and it is exactly the right
	// assignment confers on the receiving team. ScopedTeams deliberately
	// excludes GLOBAL holdings — see CanAssignToTeam.
	//
	// Runs before the transaction opens, like the member gate above, so
	// a refusal never writes a row it has to roll back.
	if in.TeamId != nil {
		ok, gErr := h.mayAssignToTeam(ctx, id, uuid.UUID(*in.TeamId))
		if gErr != nil {
			return nil, fmt.Errorf("posts: team gate: %w", gErr)
		}
		if !ok {
			return openapi.CreatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
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
		// Cover RACE BACKSTOP, the counterpart of the member one below
		// (#941). The gate above already refused every cover this
		// caller cannot read, absent ones included, so reaching here
		// means the asset was hard-deleted in the gap. Before #941 an
		// unreadable-or-absent cover fell straight through to the
		// wrapped error and answered 500 — an unhandled SQLSTATE 23503
		// dressed up as a server fault.
		if id, is := fkCoverAsset(err, coverID, coverThumbnailID); is {
			return openapi.CreatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found: " + id},
			}, nil
		}
		return nil, fmt.Errorf("posts: create: %w", err)
	}

	// Members. Idempotent on (post_id, asset_id) so de-dupes on input.
	// The FK branch below is a RACE BACKSTOP since #922 — the gate
	// above already refused every absent asset — kept because the asset
	// can still be hard-deleted between the two.
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
	// A written post is the same post a read returns (#655). Everything
	// enrichPreview derives is invisible to fetchFullPost: it is either
	// per-caller (preview_available, ladder_available) or written outside
	// the cached ListPostAssets row (pixel dimensions, the async
	// thumbhash). Four fields accumulated in that hole because the write
	// paths never made the call — see enrichPreview's own doc comment.
	if err := h.enrichForCaller(ctx, full); err != nil {
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
	readable, err := h.canReadPost(ctx, id, full)
	if err != nil {
		return nil, err
	}
	if !readable {
		return openapi.GetPost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not visible to this user"},
		}, nil
	}
	if err := h.enrichForCaller(ctx, full); err != nil {
		return nil, err
	}
	// #891 stops at the feed, and this is the line. The preference hides
	// a post from a LIST when nothing in it is visible, precisely so the
	// reader is never handed an empty card — "arguably worse than a
	// placeholder" is the reason that rule exists. Applying the member
	// half here would rebuild that empty card on the one surface it was
	// avoided on: an all-restricted post opened by id would render a
	// viewer with nothing in it and no statement of why.
	//
	// So a post asked for BY NAME answers with its placeholders, and
	// #913's "Request access" button — which lives on the placeholder —
	// survives the filter. Drive the setting from browse; open a post and
	// you see what is actually in it.
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
	if !canMutatePost(caller, current.AuthorUserRef, current.TeamID) {
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
		// The disclosure boundary. canMutatePost now admits a
		// team-scoped posts.admin (#930); changing `visibility` is a
		// decision about who can REACH the post, not about what it
		// says, and is held to the narrower gate. Compared against the
		// current value so a PATCH that merely echoes the visibility
		// back is not refused.
		if s != current.Visibility && !canWidenPostAccess(caller, current.AuthorUserRef) {
			return openapi.UpdatePost403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
					Error: "changing a post's visibility is reserved to its author",
				},
			}, nil
		}
		visPtr = &s
	}
	// #941 — the cover gate on the UPDATE path. A gate that only
	// guards CreatePost is not a gate: PATCH /posts/{id} sets
	// `cover_asset_id` on an existing post, so the same unreadable
	// asset walks in one call later. #922 learned this once already,
	// with POST /posts/{id}/assets.
	//
	// Placed AFTER canMutatePost on purpose. The post-level refusal has
	// to be settled first, or a caller who may not touch this post at
	// all would learn from the 404-vs-403 which asset UUIDs exist.
	//
	// The tx is open, but nothing has been written into it yet and the
	// deferred Rollback covers the return — so, as on create, a refusal
	// leaves no row behind.
	var coverPtr pgtype.UUID
	if in.CoverAssetId != nil {
		coverID := uuid.UUID(*in.CoverAssetId)
		ok, gErr := h.mayAttachAsset(ctx, caller, coverID)
		if gErr != nil {
			return nil, fmt.Errorf("posts: cover gate: %w", gErr)
		}
		if !ok {
			return openapi.UpdatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found: " + coverID.String()},
			}, nil
		}
		coverPtr = pgtype.UUID{Bytes: coverID, Valid: true}
	}

	// #946 — the OTHER cover column, which this handler never passed at
	// all. `cover_thumbnail_asset_id` was declared on `PostUpdate` and
	// accepted by `UpdatePostParams`, and nothing joined the two: it
	// arrived as a NULL narg, the query's COALESCE kept the current
	// value, and the caller got 200 for a write that never happened.
	// CreatePost has always set it; only PATCH dropped it.
	//
	// It is gated rather than merely wired. This column is not a post
	// member and carries its own FK, so it is a second door into the
	// same room #941 just locked — connecting it ungated would re-open
	// that hole on the one column nobody was watching. Same adapter,
	// same rule, one home (visibility.CanAttachAsset, ADR 0064).
	//
	// The refusal is byte-identical to the cover's, and to the FK
	// backstop below, so an unreadable thumbnail and a nonexistent one
	// stay indistinguishable — otherwise PATCH becomes a UUID-existence
	// probe on a second field.
	//
	// `state_id` has the identical plumbing defect on this handler and
	// is deliberately left alone: wiring it would add a fifth site that
	// writes a client-supplied workflow state with no transition
	// validation, on a subsystem whose Transition() still has zero
	// callers. That is #949, blocked on #895/#896/#897.
	var thumbPtr pgtype.UUID
	if in.CoverThumbnailAssetId != nil {
		thumbID := uuid.UUID(*in.CoverThumbnailAssetId)
		ok, gErr := h.mayAttachAsset(ctx, caller, thumbID)
		if gErr != nil {
			return nil, fmt.Errorf("posts: cover thumbnail gate: %w", gErr)
		}
		if !ok {
			return openapi.UpdatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found: " + thumbID.String()},
			}, nil
		}
		thumbPtr = pgtype.UUID{Bytes: thumbID, Valid: true}
	}

	if _, err := q.UpdatePost(ctx, UpdatePostParams{
		ID:                    pgID,
		Title:                 in.Title,
		Description:           in.Description,
		Visibility:            visPtr,
		CoverAssetID:          coverPtr,
		CoverThumbnailAssetID: thumbPtr,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		// Cover race backstop — see CreatePost. Same 404 body as the
		// gate above, so "you may not read it" and "it is gone" stay
		// indistinguishable on this path too (#941). Both columns are
		// passed now: the thumbnail's FK is a distinct constraint, and
		// handing fkCoverAsset a zero UUID for it would name
		// 00000000-… in the body of a refusal about a real asset (#946).
		if id, is := fkCoverAsset(err, coverPtr, thumbPtr); is {
			return openapi.UpdatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found: " + id},
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
	// Same shape a GET returns (#655). See CreatePost.
	if err := h.enrichForCaller(ctx, full); err != nil {
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
	if !canMutatePost(caller, cur.AuthorUserRef, cur.TeamID) {
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
			// deleted_by_user_ref is what makes the delete undoable
			// by the person who did it (#931) — see auth.CanRestoreDeleted.
			deleter := caller.UserRef
			return New(tx).SoftDeletePost(ctx, SoftDeletePostParams{
				ID:               pgID,
				DeletedReason:    softDeleteReasonPtr(reason),
				DeletedByUserRef: &deleter,
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
// post. See assets.Handler.RestoreAsset for the shape and
// auth.CanRestoreDeleted for the rule: you undo your own delete, system.admin
// undoes any. Previously system.admin only, while DeletePost was open
// to the author — so an author could delete their post and then not
// get it back (#931).
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
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	deletedBy, err := New(h.Pool).GetPostDeletedBy(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RestorePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not soft-deleted"},
			}, nil
		}
		return nil, fmt.Errorf("posts: load deleted_by: %w", err)
	}
	if !auth.CanRestoreDeleted(id, deletedBy) {
		return openapi.RestorePost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "this post was deleted by someone else; ask an administrator to restore it",
			},
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
	//
	// `?visibility=` NARROWS within what the caller may read; it never
	// widens it. Authorization is the read rule's job (readRuleSQL,
	// spliced into the query below), so this parameter is a plain
	// display filter: `?visibility=private` means "the private posts I
	// may read" — my own, plus everyone's for a moderator — and
	// `followers` / `explicit-share` behave the same way. Before #660 it
	// went into the query as a bare `visibility = $n` with no author or
	// relationship conjunct anywhere, which handed every signed-in
	// caller every other author's private posts.
	//
	// The DEFAULT stays org-only so the browse feed keeps showing the
	// walled-garden tier and nothing else — without it, signing in would
	// start dropping your own private posts into the public grid. An
	// explicit author filter for yourself still drops the default, since
	// "show me my posts" means all of your tiers.
	var visPtr *string
	switch {
	case req.Params.Visibility != nil:
		v := string(*req.Params.Visibility)
		visPtr = &v
	case req.Params.AuthorRef != nil && *req.Params.AuthorRef == caller.UserRef:
		visPtr = nil
	default:
		v := "org-only"
		visPtr = &v
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

	// ?team_id= scopes the feed to one team's posts — the team page's
	// content (#684).
	//
	// It NARROWS and cannot widen. There is no authorization decision
	// here on purpose: no membership check, no liveness probe, no 404
	// for a team the caller isn't in. The read rule (readRuleSQL, spliced
	// below) still decides every row, and it never consults team_id, so
	// this conjunct can only ever remove posts from the page the caller
	// would have got anyway. A non-member asking for a team they have
	// nothing to do with gets that team's posts THEY could already read
	// — typically just the org-only tier — which is the same answer
	// browse gives them, filtered.
	//
	// Which also means the endpoint is not a team-existence probe: an
	// unknown, a soft-deleted and a real-but-empty team all answer with
	// an empty page. Adding a "team not found" 404 here would create the
	// probe that visibility.CanAssignToTeam goes to some trouble to
	// avoid on the write side.
	var teamID pgtype.UUID
	if req.Params.TeamId != nil {
		teamID = pgtype.UUID{Bytes: *req.Params.TeamId, Valid: true}
	}

	// feed=following (Phase 1.17.G2) restricts the page to what the
	// caller follows — authors AND teams, as one union (#1048).
	// Anonymous callers can never satisfy this (the 401 path above
	// returns first); for authenticated callers who follow nothing, the
	// EXISTS subqueries yield an empty page rather than a 4xx — matches
	// every social platform's "your following tab is empty" UX.
	//
	// Both graphs, because there is one control. The rail above the grid
	// lists the teams the caller follows and the filter beside it says
	// "Following", so an account that follows four studios and no people
	// clicking it got an empty feed while the studios' posts sat one tab
	// away. Splitting the control in two (People / Studios) was
	// considered and rejected: it doubles a control that already competes
	// for room, to expose a distinction nobody asked for.
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

	// ?dir=asc walks the feed oldest-first (#868). The browse page's
	// Newest/Oldest control has sent this since #706 seeded it from the
	// `browse_sort` preference; until now nothing declared it, so the
	// server never saw it and "Oldest" rendered newest under a label
	// that promised otherwise — the same defect #691 removed from the
	// feed FILTER, left in place on the SORT.
	//
	// The flag reaches the keyset predicate as well as the ORDER BY
	// (feedOrder), because a cursor only means anything relative to the
	// order that produced it.
	//
	// Only `asc` is read; absent, empty and anything else are `desc`.
	// That is a deliberate default rather than a missing validation:
	// nothing in this stack enforces a query-parameter enum at bind
	// time (the generated wrapper binds `dir` as a plain string and
	// `ListPostsParamsDir.Valid()` has no caller), so the comparison
	// has to be positive. `?feed=` is read exactly the same way one
	// block above. A junk value therefore lands on the documented
	// default instead of a 500 or an arbitrary order.
	ascending := req.Params.Dir != nil && *req.Params.Dir == openapi.Asc

	fetch := limit + 1
	rows, err := h.ListPostsPageGated(ctx, caller, ListPostsPageParams{
		IncludeDeleted:  includeDeletedArg,
		AuthorUserRef:   authorPtr,
		Visibility:      visPtr,
		Q:               qText,
		Tag:             tagPtr,
		FeedFollowerRef: followerPtr,
		TeamID:          teamID,
		CursorPostedAt:  cursorTs,
		CursorID:        cursorID,
		RowLimit:        fetch,
		Ascending:       ascending,
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

	// The per-caller derivations — preview_available (#471), member
	// readability (#883), and the author identity (#557) — all happen
	// here, from the per-request identity, and are never baked into the
	// cross-caller Post cache. Pointers into `items` so enrichPreview can
	// replace each post's Members slice in place.
	ptrs := make([]*openapi.Post, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	if err := h.enrichForCaller(ctx, ptrs...); err != nil {
		return nil, err
	}

	// #921 — restricted placeholders are subtracted from the feed BY
	// DEFAULT, applied STRICTLY ON TOP of everything above. enrichPreview
	// has just marked every member this caller may not read;
	// applyHideRestricted reads that mark and nothing else, so this can
	// only subtract from a page the read rule already decided. See
	// feed_filters.go.
	//
	// Read the condition as "unless the reader asked for them back".
	// #891 shipped this as an opt-in and the default was measured wrong:
	// a third of one seeded account's 82-post feed was entirely
	// placeholders. The line the default draws — a placeholder belongs
	// where you asked a question or opened a container, not where you
	// were handed a feed — is why GetPost (an explicit request for one
	// post) and collection contents (an opened container) both still
	// render them, and why extending this filter to either would be a
	// bug and not a consistency fix.
	//
	// NOTHING ABOUT THE READ RULE MOVED. ListPosts still receives every
	// row the rule returns; this subtracts afterwards off one
	// already-computed field. ADR 0020 and ADR 0064 are amended to name
	// that split — the rule is unchanged, the default PRESENTATION is
	// not.
	//
	// After the cursor bookkeeping on purpose. `lastPostedAt`/`lastID`
	// track the last row the QUERY returned, and `next_cursor` keys off
	// `len(rows)`, so a page that hides three posts still hands back a
	// cursor that resumes exactly where the SQL left off — it returns
	// fewer items, never a different window. (`PostList` carries no
	// total, so there is no count to disagree with what renders; the
	// client's infinite scroll follows the cursor.)
	if show := h.showRestricted(ctx, caller.UserRef); !show {
		items = applyHideRestricted(items, caller.UserRef)
	}

	resp := openapi.PostList{Items: items}
	if len(rows) > int(limit) {
		next := encodeCursor(lastPostedAt, lastID)
		resp.NextCursor = &next
	}
	return openapi.ListPosts200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// ListPostsSharedWithMe — the "Shared with me" surface (#875)
// ---------------------------------------------------------------------------
//
// Where shares accumulate. A grant used to be findable only if the
// sharer also sent a link out of band: the notification did not exist,
// and ListPosts above pins visibility to `org-only` when the caller
// sends no `?visibility=`, which no frontend surface does — so a shared
// post never entered the recipient's grid.
//
// The fix is NOT to widen that default. A share is low-volume and
// high-salience: burying it in the busiest grid in the app is the wrong
// place for it, and putting an EXISTS over post_acls into the feed would
// change the shape (and the cache key) of the hottest query in the app
// for content better served by being announced. Every prior-art surface
// worth copying does the same two things instead — tell the recipient,
// and give shares somewhere of their own to land. This is the second.
//
// Everything after the query is the feed's own tail: fetchFullPost per
// row through the shared post cache, then enrichPreview for the
// per-caller preview flags. Deliberately identical, so a post looks the
// same here as it does anywhere else it is listed.
func (h *Handler) ListPostsSharedWithMe(
	ctx context.Context,
	req openapi.ListPostsSharedWithMeRequestObject,
) (openapi.ListPostsSharedWithMeResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListPostsSharedWithMe401JSONResponse{
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
			return openapi.ListPostsSharedWithMe500JSONResponse{
				InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: id, Valid: true}
	}

	rows, err := h.ListSharedWithMeGated(ctx, caller.UserRef, cursorTs, cursorID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("posts: shared with me: %w", err)
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
			// The query already filters deleted_at IS NULL, so a miss
			// here means the post was deleted between the two reads.
			// Drop it rather than 500 — this surface has no trash view.
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, err
		}
		items = append(items, *full)
		lastPostedAt = r.PostedAt.Time
		lastID = uuid.UUID(r.ID.Bytes)
	}

	ptrs := make([]*openapi.Post, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	if err := h.enrichForCaller(ctx, ptrs...); err != nil {
		return nil, err
	}

	resp := openapi.PostList{Items: items}
	if len(rows) > int(limit) {
		next := encodeCursor(lastPostedAt, lastID)
		resp.NextCursor = &next
	}
	return openapi.ListPostsSharedWithMe200JSONResponse(resp), nil
}

// GetPostsByAsset returns the visibility-filtered posts whose members
// include the given asset (#478 slice-2, ADR 0070). An asset is a
// many-to-many member of ≥0 posts, so this is a slice of the same feed
// keyed on the asset — no new enforcement plane.
//
// Anonymous admission is decided upstream by the public-mode gate
// (auth.PublicSurfaceRoutes): with public mode off an anonymous request
// never reaches here. Visibility is the same read rule the feed and
// GetPost use (readRuleSQL) rather than a tier list restated here —
// anonymous sees the public tier, an authenticated caller additionally
// sees org-only, their own posts at every tier, followed authors'
// followers-tier posts, and (as a moderator) private ones. Bounded
// result (no cursor); the client redirects when exactly one post is
// visible and lists when several.
func (h *Handler) GetPostsByAsset(
	ctx context.Context,
	req openapi.GetPostsByAssetRequestObject,
) (openapi.GetPostsByAssetResponseObject, error) {
	ids, err := h.ListPostsByAssetGated(ctx, auth.IdentityFromContext(ctx), req.Id)
	if err != nil {
		return nil, err
	}

	items := make([]openapi.Post, 0, len(ids))
	for _, id := range ids {
		full, err := h.fetchFullPost(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // raced deletion between the list and the fetch
			}
			return nil, err
		}
		items = append(items, *full)
	}

	// preview_available (#471) and the author (#557) are per-caller —
	// derive them from the request identity, same as ListPosts.
	ptrs := make([]*openapi.Post, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	if err := h.enrichForCaller(ctx, ptrs...); err != nil {
		return nil, err
	}

	return openapi.GetPostsByAsset200JSONResponse(openapi.PostList{Items: items}), nil
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
	if !canMutatePost(caller, cur.AuthorUserRef, cur.TeamID) {
		return openapi.AddPostAsset403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	// #922 — the same member gate CreatePost applies. A gate on create
	// alone would not be a gate: this endpoint attaches an asset to an
	// EXISTING post and is reachable by exactly the same callers.
	//
	// canMutatePost above answered "may you change this post"; this
	// answers the separate question "may you reach this asset". Both
	// are required — the first is about the container, the second about
	// the thing being put in it.
	attachable, err := h.mayAttachAsset(ctx, caller, uuid.UUID(req.Body.AssetId))
	if err != nil {
		return nil, fmt.Errorf("posts: member gate: %w", err)
	}
	if !attachable {
		return openapi.AddPostAsset404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}

	pgAsset := pgtype.UUID{Bytes: uuid.UUID(req.Body.AssetId), Valid: true}
	if err := q.AddPostAsset(ctx, AddPostAssetParams{
		PostID:    pgID,
		AssetID:   pgAsset,
		SortOrder: int32Or(req.Body.SortOrder, 0),
	}); err != nil {
		// Race backstop since #922; the gate above refuses an absent
		// asset before this runs.
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
	if !canMutatePost(caller, cur.AuthorUserRef, cur.TeamID) {
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
// Authorization: adding, removing AND listing all require write access
// to the post (canMutatePost) — author, posts.admin or system.admin.
// Add/remove has always been gated that way so a viewer can't expand
// their own access by editing the ACL list; listing joined it in #876.
//
// #667 wired post_acls into the read rule, which incidentally handed the
// grant list to every GRANTEE: share a post with someone and they could
// enumerate everyone else it was shared with, who granted it and when
// each grant expires. That followed from gating on "can read the post",
// and the note left here at the time named the fix — "a separate
// authorization here, not a second read rule" — which is what this is.
//
// The gate is a DIFFERENT rule from the read rule, not a restatement of
// it, and the difference is the point: it drops the grant disjunct (a
// grantee may use the share without seeing the guest list) and drops the
// org-only tier too (being signed in is not a management relationship
// with somebody else's post). collections.ListCollectionAcls has always
// drawn exactly this line, for exactly this reason — "who may read the
// grant list is a management question, not the row-visibility question".
//
// Note what did NOT change: the read rule. A grantee still reads the
// post, still finds it at GET /posts/{id}, still sees it on their
// "Shared with me" surface. Only the guest list closed.
//
// The 404-before-403 order is kept: a caller who cannot mutate a post
// that does not exist gets "not found", same as every other post route,
// so this endpoint stays no more enumerable than GetPost.

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
	cur, err := New(h.Pool).GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListPostAcls404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef, cur.TeamID) {
		return openapi.ListPostAcls403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
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
	// NOT canMutatePost. Writing an ACL row hands a named principal
	// access to the post — the same lever as `visibility`, reached
	// through a different endpoint, and therefore held to the same
	// narrower gate (#930). Widening canMutatePost to team-scoped
	// grants without this would have let a team lead share a
	// colleague's private post with whoever they liked.
	//
	// RemovePostAcl deliberately keeps the wider gate: revoking a grant
	// narrows access, and a management capability that can tidy up but
	// not hand out is the right asymmetry.
	if !canWidenPostAccess(caller, cur.AuthorUserRef) {
		return openapi.AddPostAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "granting access to a post is reserved to its author",
			},
		}, nil
	}
	// The principal has to be a reference the read rule can actually
	// match, and the type has to be one this surface honours. Before
	// #916 neither was checked: `principal_id` is TEXT, so a username
	// went straight into the column, the read rule compared it against
	// `$n::BIGINT::TEXT` and never matched, and the caller got a 204
	// for a grant that did nothing. notifyShare below was the only code
	// that noticed — it parses the same value, and on failure it told
	// the log rather than the caller.
	if err := acls.ValidateContentPrincipal(
		string(req.Body.PrincipalType), req.Body.PrincipalId,
	); err != nil {
		return openapi.AddPostAcl400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
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
	h.notifyShare(ctx, caller.UserRef, cur, req.Body)
	return openapi.AddPostAcl204Response{}, nil
}

// notifyShare tells the grantee a post was shared with them (#875).
//
// Before this, a share was completely silent AND invisible: nothing was
// sent, and the browse feed's default `org-only` filter meant the post
// never appeared in the recipient's grid either. Sharing only worked if
// the sharer separately sent a link out of band. The notification is how
// you LEARN; /account/shared-posts is where shares accumulate.
//
// Runs AFTER the row is written and deliberately returns nothing: the
// grant is the user's action and it has already succeeded, so a notify
// failure is logged and dropped rather than turned into a 500 that would
// tell the author their share failed when it did not. Same best-effort
// discipline as the @-mention emit in CreatePost.
//
// Only `user` principals notify. A `role` or `team` grant names no
// single recipient — resolving one into a recipient set is a fan-out
// this surface does not have (and role/team principals do not even grant
// read yet; see visibility.PostLiveGrantSQL). Skipping is the honest
// answer; inventing a fan-out here would be a second, undertested
// membership rule.
//
// principal_id is TEXT in the schema and a user ref is a BIGINT, so a
// row whose principal_id is not parseable as one is not a recipient. It
// is logged rather than ignored: a user-typed grant that can never
// notify is a data problem worth seeing, not a routine skip.
func (h *Handler) notifyShare(
	ctx context.Context,
	actorRef int64,
	post GetPostRow,
	body *openapi.AclCreate,
) {
	if h.notifier == nil || body == nil || string(body.PrincipalType) != "user" {
		return
	}
	recipient, err := strconv.ParseInt(body.PrincipalId, 10, 64)
	if err != nil {
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "posts.acl.notify.bad_principal",
				slog.String("principal_id", body.PrincipalId),
				slog.String("post_id", uuidString(post.ID)),
			)
		}
		return
	}
	actor := actorRef
	if err := h.notifier.Notify(ctx, recipient, &actor,
		notifications.VerbPostSharedWithMe,
		notifications.TargetKindPost,
		uuidString(post.ID),
		map[string]any{
			notifications.PayloadKeyPostTitle: post.Title,
		},
	); err != nil && h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "posts.acl.notify.error",
			slog.Int64("recipient", recipient),
			slog.String("post_id", uuidString(post.ID)),
			slog.String("err", err.Error()),
		)
	}
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
	if !canMutatePost(caller, cur.AuthorUserRef, cur.TeamID) {
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

// InvalidateForAsset drops the cached copy of every post that lists
// assetID as a member. It is the cross-package entry point assets/
// calls after a write that changes whether the asset is a member the
// API will render — soft-delete and restore.
//
// Why this is needed at all: ListPostAssets joins `assets` with
// `a.deleted_at IS NULL`, so the QUERY has always been right. What was
// wrong is that soft-deleting an asset writes only the asset row, and
// the post cache is keyed on the post. Nothing on the delete path told
// the post cache that its answer had changed, so `GET /posts/{id}`
// went on serving the deleted asset in full — title, description,
// file hash, byte size — until the process restarted (#920).
//
// Uses Registry.InvalidateNow rather than Emit: the caller's next read
// can be the very next request, so the local LRU has to be dropped
// synchronously and not via a NOTIFY round-trip.
//
// Best-effort and nil-safe. A cache invalidation failing must not turn
// a completed delete into a 500 — the same discipline cacheInvalidate
// applies. Returns the first error purely so callers can log it.
func InvalidateForAsset(
	ctx context.Context,
	registry *cache.Registry,
	pool *pgxpool.Pool,
	assetID uuid.UUID,
) error {
	if registry == nil || pool == nil {
		return nil
	}
	ids, err := New(pool).PostIDsForAsset(ctx, pgtype.UUID{Bytes: assetID, Valid: true})
	if err != nil {
		return fmt.Errorf("posts: post ids for asset %s: %w", assetID, err)
	}
	var firstErr error
	for _, id := range ids {
		if err := registry.InvalidateNow(ctx, cacheDomainPostByID, uuidString(id)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func uuidString(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------

// mayAttachAsset answers "may this caller make THIS asset a member of a
// post" (#922).
//
// # What was wrong before
//
// The members loop handled exactly one failure — a foreign-key
// violation became a 404 — and there was no readability check at all.
// Any authenticated caller could name any asset UUID as a member of
// their own post, including assets they had never been allowed to view.
//
// That does not leak the CONTENT: ADR 0064's member conjunction still
// runs per-caller at render time, so a viewer who is not independently
// entitled sees a placeholder carrying the real owner's name. What it
// permitted is unwanted ASSOCIATION — attaching someone's restricted
// work to your post without their consent, so that everyone who IS
// entitled to see it meets it framed by you.
//
// Whether referencing another artist's work should be a first-class
// feature with consent rules is #923, a policy question above this
// floor. This is only the floor.
//
// # Why it is not a second rule
//
// The two-plane conjunction lives in visibility.CanAttachAsset, which
// the collection surface calls through collections.mayCollectAsset
// (#882). This is the posts-side adapter over the same function, not a
// second readability notion — epic #665, and the sprints #892 and #904
// each spent deleting one.
func (h *Handler) mayAttachAsset(ctx context.Context, id *auth.Identity, assetID uuid.UUID) (bool, error) {
	if id == nil {
		return false, nil
	}
	return visibility.CanAttachAsset(
		ctx,
		h.Pool,
		visibility.NewCaller(&id.UserRef),
		visibility.CapabilityChecker(func(code string) bool { return id.Can(code) }),
		assetID,
	)
}

// mayAssignToTeam adapts an *auth.Identity to visibility.CanAssignToTeam
// — the SHARED rule behind `PostCreate.team_id` (#954) and
// `AssetCreate.team_id` (#953). assets.Handler has the mirror-image
// adapter; the rule itself exists once.
//
// It is a method for the same reason mayAttachAsset is: CreatePost
// declares a local `visibility` string for the post's visibility tier,
// which shadows the package name at that call site.
//
// The capability the SCOPED half asks about is `posts.admin`, the same
// code canMutatePost consults. That is deliberate rather than a shared
// cross-entity code: assignment CONFERS the team-scoped mutation right
// on the receiving team, so the code that names that right is the one
// entitled to hand it out. `assets.admin` plays the identical role on
// the asset side, and a single code for both would let a holder of one
// plant rows in the other's space.
func (h *Handler) mayAssignToTeam(ctx context.Context, id *auth.Identity, teamID uuid.UUID) (bool, error) {
	if id == nil {
		return false, nil
	}
	return visibility.CanAssignToTeam(
		ctx,
		h.Pool,
		visibility.NewCaller(&id.UserRef),
		visibility.CapabilityChecker(func(code string) bool { return id.Can(code) }),
		id.ScopedTeams(CapPostsAdmin),
		teamID,
	)
}

// canMutatePost returns true if the caller can edit/delete this post.
// Author, system.admin, a global posts.admin, or a posts.admin scoped
// to the post's team.
//
// The team-scoped disjunct is #930's other half: an art director whose
// grant is scoped to one team could not manage that team's posts,
// because this only ever consulted GLOBAL grants. teamID comes from
// `posts.team_id`, which is NULLABLE — a post with no team has no
// scope for InTeam to check, so the disjunct is skipped rather than
// treated as "no scope required, therefore anyone passes".
//
// The closure walk is already done: the resolver pre-expands scoped
// grants through `team_closure`, so a grant on a parent team covers
// every descendant without this function knowing the hierarchy exists.
func canMutatePost(id *auth.Identity, authorRef int64, teamID pgtype.UUID) bool {
	if id == nil || id.IsAnonymous() {
		return false
	}
	// Authorship. Ref 0 is the anonymous sentinel and is never a
	// principal on either side of the comparison.
	if id.UserRef != 0 && authorRef != 0 && id.UserRef == authorRef {
		return true
	}
	if id.Can(CapSystemAdmin) {
		return true
	}
	if teamID.Valid && id.Can(CapPostsAdmin, auth.InTeam(uuid.UUID(teamID.Bytes))) {
		return true
	}
	return id.Can(CapPostsAdmin)
}

// canWidenPostAccess is the narrower question canMutatePost is not:
// may this caller change WHO CAN REACH the post, as opposed to what it
// says? Author, global posts.admin, or system.admin — deliberately not
// a holder who arrives only through the new team-scoped disjunct.
//
// Two endpoints reach that lever and both use this gate:
//
//   - `PATCH /posts/{id}` carries `visibility`, so extending
//     canMutatePost to team-scoped grants would otherwise have handed a
//     team lead the power to flip a colleague's private post to
//     org-only.
//   - `AddPostAcl` writes a grant row naming a principal, which is the
//     same widening reached through a different endpoint. Gating it on
//     canMutatePost — as it was, before canMutatePost grew a
//     team-scoped disjunct — would have let a team lead share a
//     colleague's private post with anyone they chose.
//
// Removing an ACL is NOT here: revoking narrows access, and a
// management capability that can tidy up but not hand out is the right
// asymmetry.
//
// That is a disclosure decision, and "manage my team's posts" is not a
// grant of it — the same boundary migration 00037 draws for
// `assets.admin` and `status`.
//
// Global posts.admin keeps it: that is the instance moderator role and
// this change is not the place to renegotiate what it means.
func canWidenPostAccess(id *auth.Identity, authorRef int64) bool {
	if id == nil || id.IsAnonymous() {
		return false
	}
	if id.UserRef != 0 && authorRef != 0 && id.UserRef == authorRef {
		return true
	}
	return id.Can(CapPostsAdmin) || id.Can(CapSystemAdmin)
}

// canReadPost gates the single-item read path (GetPost). ListPostAcls
// used to share it and no longer does — listing a post's grants is a
// management question gated on canMutatePost since #876; see the note
// above that handler.
//
// It does not decide anything itself: it runs the ONE read rule
// (readRuleSQL) as an EXISTS probe against the post's id, which is the
// same fragment ListPostsPageGated filters the feed with. That is the
// #660 fix — before it, this function was the real rule and the list
// query was a second, weaker restatement of it, so the list returned
// posts this function would have refused.
//
// The DB error case is propagated rather than swallowed: the caller
// turns it into a 500. A read gate that answers "no" on a transport
// blip looks exactly like a permissions bug to whoever hits it.
func (h *Handler) canReadPost(ctx context.Context, id *auth.Identity, p *openapi.Post) (bool, error) {
	if id == nil {
		return false, nil
	}
	return h.postReadable(ctx, id, uuid.UUID(p.Id))
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
		a := memberToAsset(m)
		// Restricted is FALSE here and the asset is complete, always.
		// This row is CACHED cross-caller (h.byID), so it must not carry
		// any per-caller decision; enrichPreview re-derives #883's
		// redaction on a fresh copy for each request. Baking it here
		// would serve one caller's answer to the next — the same trap
		// preview_available fell into (#471).
		out.Members = append(out.Members, openapi.PostMember{
			AssetId:   openapi_types.UUID(m.AssetID.Bytes),
			SortOrder: int(m.SortOrder),
			Asset:     &a,
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

// enrichPreview sets member.asset.preview_available on the given posts
// for the request's caller (#471). It runs ONE batched query over every
// member asset id across all posts — no per-asset, no per-post round
// trips — joining `col` variant existence + the caller's team membership,
// then decides readability in-Go via visibility.ContentReadable.
//
// It is ALSO the #883 redaction point: a member the caller fails
// visibility.FieldsReadable on is rewritten in place as a placeholder —
// asset_id + sort_order + restricted + the owner's display name, and no
// `asset` object at all. Same pass, same batched query, because it is the
// same per-caller readability decision; splitting them would be two
// expressions of one rule that can disagree.
//
// CACHE SAFETY (the reason the posts path was deferred): the full Post is
// cached by id in h.byID, and its Members slice header aliases the cached
// backing array. Readability is PER-CALLER, so writing it into that
// shared array would leak one caller's answer to the next. This therefore
// replaces each post's Members with a FRESH slice and mutates only the
// copy. PostMember.Asset is a POINTER (#883 made it optional so a
// placeholder can omit it), so copy() alone no longer detaches — the
// Asset VALUE is cloned per member below, or the enrich would write
// straight through into the cached object.
func (h *Handler) enrichPreview(ctx context.Context, posts ...*openapi.Post) error {
	caller := visibility.NewCaller(nil)
	var caps visibility.CapabilityChecker
	// #939 — the caller's `assets.admin` scope. Widens the FIELD plane
	// of a restricted member (ADR 0064) and nothing else.
	var mut visibility.AssetMutationCaps
	if id := auth.IdentityFromContext(ctx); id != nil {
		caller = visibility.NewCaller(&id.UserRef)
		caps = func(code string) bool { return id.Can(code) }
		mut = visibility.ResolveAssetMutationCaps(
			func(code string) bool { return id.Can(code) },
			id.ScopedTeams(visibility.AssetsAdmin),
		)
	}

	idSet := make(map[uuid.UUID]struct{})
	for _, p := range posts {
		if p == nil {
			continue
		}
		for _, m := range p.Members {
			idSet[uuid.UUID(m.AssetId)] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id.String())
	}

	// The configured ladder travels in as a PARAMETER rather than being
	// baked into the SQL (#591). Read once for the whole batch — a cached
	// in-process lookup, not a config round-trip per row.
	var ladder []string
	if h.previewLadder != nil {
		ladder = h.previewLadder(ctx)
	}

	// a.status + a.processing_status feed visibility.FieldsReadable's
	// row-plane conjuncts, and the owner display name is the ONE
	// asset-derived value a #883 placeholder carries. Both ride this
	// query rather than a second round-trip.
	//
	// #1023 — the name comes from visibility.OwnerDisplayNameSQL, not
	// from a pair of LEFT JOINs written here. The joins that used to sit
	// in this FROM clause resolved
	// `COALESCE(NULLIF(up.display_name,''), u.username, '')`, a copy of
	// the display-name ladder that never consulted
	// `hide_from_anonymous` — so THIS query is where an owner who took
	// ADR 0024's opt-out had their username handed to an anonymous
	// caller, on any public post carrying one of their restricted
	// assets. It is also the only reason those joins existed.
	rows, err := h.Pool.Query(ctx, `
		SELECT a.id, a.sensitivity, a.status, a.processing_status, a.owner_user_ref,
		       `+visibility.OwnerDisplayNameSQL("a.owner_user_ref", caller.IsAnonymous)+` AS owner_display_name,
		       (a.file_hash IS NOT NULL AND EXISTS (
		            SELECT 1 FROM storage_variants sv
		             WHERE sv.object_hash = a.file_hash AND sv.variant_key = 'col')) AS has_col,
		       `+sysconfig.LadderSatisfiedSQL("a.file_hash", "$3")+` AS has_ladder,
		       (a.file_hash IS NOT NULL AND EXISTS (
		            SELECT 1 FROM storage_variants sv
		             WHERE sv.object_hash = a.file_hash AND sv.variant_key = 'sprites.vtt')) AS has_scrub,
		       (a.team_id IS NOT NULL AND EXISTS (
		            SELECT 1 FROM team_memberships tm
		             WHERE tm.team_id = a.team_id AND tm.user_ref = $2::BIGINT)) AS is_member,
		       a.team_id,
		       a.thumbhash,
		       `+pixeldims.SelectColumnsSQL("a.id")+`
		FROM assets a
		WHERE a.id = ANY($1::uuid[])`,
		ids, caller.UserRef, ladder)
	if err != nil {
		return fmt.Errorf("posts: preview enrich: %w", err)
	}
	defer rows.Close()

	avail := make(map[uuid.UUID]bool, len(idSet))
	ladderOK := make(map[uuid.UUID]bool, len(idSet))
	// The hover-scrub gate (#835), computed from the SAME readability
	// decision as the two above for the same reason.
	scrubOK := make(map[uuid.UUID]bool, len(idSet))
	// Source pixel dimensions per member asset (#640). They ride this
	// pass rather than ListPostAssets because that query's rows are
	// CACHED per post and these have to reach a caller whose members came
	// off the cache — the same reason the two availability flags are
	// computed here. Unlike those, dimensions are not per-caller; they
	// are just metadata about a row the caller can already see.
	dims := make(map[uuid.UUID][2]int32, len(idSet))
	// Blur-up placeholder per member asset (#648). Rides this pass for
	// the SAME reason as dims, and one more: thumbhash is written
	// ASYNCHRONOUSLY. It is computed synchronously at upload only when
	// the image decodes there; every other case — a decode that failed,
	// an asset that predates the column — is backfilled later by the
	// raster worker (preview.backfillThumbhash → SetAssetThumbhashIfMissing),
	// which has no reason to invalidate a post cache. On the cached
	// ListPostAssets row a post read before its raster job finished would
	// pin thumbhash=NULL until an unrelated post write evicted it, which
	// is the same "server has it, client can't see it" failure this issue
	// exists to close. Read per request instead; it costs one more column
	// on a query that already runs.
	hashes := make(map[uuid.UUID]string, len(idSet))
	// #883 — per-caller member readability, and the owner display name
	// that is the only thing a redacted member carries. A member whose id
	// is MISSING from `readable` (the asset row vanished between the
	// cached member list and this query) stays false and is therefore
	// redacted: fail closed.
	readable := make(map[uuid.UUID]bool, len(idSet))
	ownerNames := make(map[uuid.UUID]string, len(idSet))
	for rows.Next() {
		var (
			id        pgtype.UUID
			sens      string
			status    string
			procState string
			owner     *int64
			ownerName string
			hasCol    bool
			hasLadder bool
			hasScrub  bool
			isMember  bool
			thumb     []byte
			pxW       *int32
			pxH       *int32
			teamID    *uuid.UUID
		)
		if err := rows.Scan(&id, &sens, &status, &procState, &owner, &ownerName,
			&hasCol, &hasLadder, &hasScrub, &isMember, &teamID, &thumb, &pxW, &pxH); err != nil {
			return fmt.Errorf("posts: preview enrich scan: %w", err)
		}
		if pixeldims.Sane(pxW, pxH) {
			dims[uuid.UUID(id.Bytes)] = [2]int32{*pxW, *pxH}
		}
		// TWO decisions from one row (#939). `ok` is the FIELD plane and
		// admits a scoped `assets.admin` holder; `picture` is the same
		// conjunction WITHOUT that disjunct and gates the blur plus the
		// three availability flags, because ADR 0064 puts the thumbhash
		// on the binary side — "a thumbhash IS a blur" — and the flags
		// are a promise the binary handlers still refuse to keep.
		//
		// They agree for every caller except a mutation holder, so the
		// original invariant (flags and redaction never disagree on a
		// restricted asset) is preserved: a true ladder flag on gated
		// bytes is still impossible.
		fr := visibility.FieldsRow{
			Sensitivity:      sens,
			Status:           status,
			ProcessingStatus: procState,
			OwnerUserRef:     owner,
			IsTeamMember:     isMember,
			TeamID:           teamID,
		}
		fr.ApplyMutationCaps(mut)
		ok := visibility.FieldsReadable(fr, caller, caps)
		picture := visibility.PreviewReadable(fr, caller, caps)
		if len(thumb) > 0 && picture {
			// Base64 on the wire, matching assets.rowToAPI — the
			// frontend decoder takes either form and this is the one
			// every other surface already ships.
			hashes[uuid.UUID(id.Bytes)] = base64.StdEncoding.EncodeToString(thumb)
		}
		readable[uuid.UUID(id.Bytes)] = ok
		ownerNames[uuid.UUID(id.Bytes)] = ownerName
		avail[uuid.UUID(id.Bytes)] = hasCol && picture
		ladderOK[uuid.UUID(id.Bytes)] = hasLadder && picture
		scrubOK[uuid.UUID(id.Bytes)] = hasScrub && picture
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("posts: preview enrich rows: %w", err)
	}

	for _, p := range posts {
		if p == nil {
			continue
		}
		fresh := make([]openapi.PostMember, len(p.Members))
		for i, m := range p.Members {
			id := uuid.UUID(m.AssetId)
			if !readable[id] {
				// The #883 placeholder, written as a complete literal so
				// that a field added to PostMember later is absent by
				// construction rather than by remembering to clear it.
				// Asset is nil: the whole object is withheld, not
				// blanked, so there is no empty-vs-withheld difference
				// for a client to read anything off.
				fresh[i] = openapi.PostMember{
					AssetId:    m.AssetId,
					SortOrder:  m.SortOrder,
					Restricted: true,
				}
				if n := ownerNames[id]; n != "" {
					v := n
					fresh[i].OwnerDisplayName = &v
				}
				continue
			}
			// CLONE the asset value. p.Members[i].Asset points into the
			// cross-caller cache; mutating through it would write this
			// caller's flags into every subsequent caller's response.
			a := *m.Asset
			prev, lad, scr := avail[id], ladderOK[id], scrubOK[id]
			a.PreviewAvailable = &prev
			a.LadderAvailable = &lad
			a.ScrubAvailable = &scr
			a.PixelWidth, a.PixelHeight = nil, nil
			if wh, ok := dims[id]; ok {
				w, h := wh[0], wh[1]
				a.PixelWidth, a.PixelHeight = &w, &h
			}
			a.Thumbhash = nil
			if th, ok := hashes[id]; ok {
				v := th
				a.Thumbhash = &v
			}
			fresh[i] = openapi.PostMember{
				AssetId:    m.AssetId,
				SortOrder:  m.SortOrder,
				Restricted: false,
				Asset:      &a,
			}
		}
		p.Members = fresh
	}
	return nil
}

// ptrPM returns a pointer to a copy of v. openapi.Asset's fields became
// pointers when #899 shrank the schema's `required` list so a withheld
// payload could omit them.
func ptrPM[T any](v T) *T { return &v }

func memberToAsset(m ListPostAssetsRow) openapi.Asset {
	a := openapi.Asset{
		Id:            openapi_types.UUID(m.AssetID.Bytes),
		Title:         &m.Title,
		Description:   &m.Description,
		AssetType:     &m.AssetType,
		Status:        ptrPM(openapi.AssetStatus(m.Status)),
		FileHash:      m.FileHash,
		FileExtension: m.FileExtension,
		FileSizeBytes: m.FileSizeBytes,
		Tags:          &[]string{},
		CreatedAt:     &m.AssetCreatedAt.Time,
		UpdatedAt:     &m.AssetUpdatedAt.Time,
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

// attachablesOf lists every asset a PostCreate body names, in the
// order the refusal message should report them: members first (so the
// #922 behaviour is byte-identical for a body with no explicit cover),
// then the explicit cover, then the explicit cover thumbnail.
//
// The implicit cover is deliberately absent. When `cover_asset_id` is
// omitted the handler copies members[0], which is already in this list;
// adding it again would double the gate query on the path almost every
// upload takes.
func attachablesOf(in *openapi.PostCreate) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(in.Members)+2)
	for _, m := range in.Members {
		out = append(out, uuid.UUID(m.AssetId))
	}
	if in.CoverAssetId != nil {
		out = append(out, uuid.UUID(*in.CoverAssetId))
	}
	if in.CoverThumbnailAssetId != nil {
		out = append(out, uuid.UUID(*in.CoverThumbnailAssetId))
	}
	return out
}

// fkCoverAsset maps a foreign-key violation on either cover column back
// to the UUID that caused it, so the 404 can name the same asset the
// gate would have named. Returns false for any other error.
//
// The thumbnail constraint name CONTAINS neither of the other two as a
// substring, which matters because isFKError matches on substring:
// posts_cover_asset_id_fkey vs posts_cover_thumbnail_asset_id_fkey.
func fkCoverAsset(err error, cover, thumb pgtype.UUID) (string, bool) {
	switch {
	case isFKError(err, "posts_cover_thumbnail_asset_id_fkey"):
		return uuidString(thumb), true
	case isFKError(err, "posts_cover_asset_id_fkey"):
		return uuidString(cover), true
	}
	return "", false
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
