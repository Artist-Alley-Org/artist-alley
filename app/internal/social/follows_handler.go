// Follow / block HTTP handlers (Phase 1.17.G2, feat/user-surfaces).
//
// Endpoints implemented here:
//
//	POST   /users/{ref}/follow          idempotent follow
//	DELETE /users/{ref}/follow          idempotent unfollow
//	GET    /users/{ref}/followers       paginated reverse-edge list
//	GET    /users/{ref}/following       paginated forward-edge list
//	GET    /users/{ref}/relationship    caller's relationship snapshot
//	POST   /users/{ref}/block           directional block (auto-unfollow both ways)
//	DELETE /users/{ref}/block           idempotent unblock
//	GET    /account/blocked             my block list with reasons
//
// Permission-aware writers (per memory `project_phase_1_17_inflight`):
// follow attempts are refused with 403 when either party has blocked
// the other. The same gate fires in the future I2 notifications
// writer + I DM writer; HasBlockBetween is the single source of
// truth for "are these two users mutually visible?"

package social

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// FollowUser — POST /users/{ref}/follow.
//
// Self-follow → 400. Cross-block → 403. Otherwise idempotent insert
// and 204. Target-user-not-found surfaces as 404 from the FK
// failure semantics — we do an explicit lookup first so the error
// message is intelligible rather than relying on pg's error code.
func (h *Handler) FollowUser(
	ctx context.Context,
	req openapi.FollowUserRequestObject,
) (openapi.FollowUserResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.FollowUser401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	target := req.Ref
	if target == id.UserRef {
		return openapi.FollowUser400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "cannot follow yourself",
			},
		}, nil
	}
	q := New(h.Pool)
	if err := h.requireUserExists(ctx, q, target); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.FollowUser404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, err
	}
	blocked, err := h.hasBlockBetween(ctx, q, id.UserRef, target)
	if err != nil {
		return nil, err
	}
	if blocked {
		return openapi.FollowUser403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "follow blocked by an active block edge between the two users",
			},
		}, nil
	}
	if err := q.FollowUser(ctx, FollowUserParams{
		FollowerUserRef: id.UserRef,
		FolloweeUserRef: target,
	}); err != nil {
		return nil, err
	}
	// Invalidate the edge + both counts. NOTIFY broadcasts each
	// invalidation to federated peers so cross-instance views stay
	// in sync the moment the write commits here.
	h.invalidateFollowEdge(ctx, id.UserRef, target)
	if h.Logger != nil {
		h.Logger.Info("follow",
			slog.Int64("follower", id.UserRef),
			slog.Int64("followee", target),
		)
	}
	return openapi.FollowUser204Response{}, nil
}

// UnfollowUser — DELETE /users/{ref}/follow.
//
// Idempotent: 204 whether the edge existed or not. The rows-affected
// from the DELETE feeds an audit hint later but isn't surfaced to
// the caller (modern platforms hide it for privacy).
func (h *Handler) UnfollowUser(
	ctx context.Context,
	req openapi.UnfollowUserRequestObject,
) (openapi.UnfollowUserResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UnfollowUser401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	q := New(h.Pool)
	_, err := q.UnfollowUser(ctx, UnfollowUserParams{
		FollowerUserRef: id.UserRef,
		FolloweeUserRef: req.Ref,
	})
	if err != nil {
		return nil, err
	}
	h.invalidateFollowEdge(ctx, id.UserRef, req.Ref)
	return openapi.UnfollowUser204Response{}, nil
}

// ListUserFollowers — GET /users/{ref}/followers.
//
// Limit is clamped; the openapi spec validates the lower bound. The
// list is keyed off followee_user_ref so the reverse-lookup index
// (idx_user_follows_followee) handles it without a sequential scan
// even on large graphs.
func (h *Handler) ListUserFollowers(
	ctx context.Context,
	req openapi.ListUserFollowersRequestObject,
) (openapi.ListUserFollowersResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListUserFollowers401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	q := New(h.Pool)
	if err := h.requireUserExists(ctx, q, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListUserFollowers404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, err
	}
	limit := resolveLimit(req.Params.Limit)
	rows, err := q.ListFollowers(ctx, ListFollowersParams{
		FolloweeUserRef: req.Ref,
		Limit:           int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := openapi.SocialUserList{Users: make([]openapi.SocialUserSummary, 0, len(rows))}
	for _, r := range rows {
		out.Users = append(out.Users, socialUserFromFollowerRow(r))
	}
	return openapi.ListUserFollowers200JSONResponse(out), nil
}

// ListUserFollowing — GET /users/{ref}/following.
func (h *Handler) ListUserFollowing(
	ctx context.Context,
	req openapi.ListUserFollowingRequestObject,
) (openapi.ListUserFollowingResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListUserFollowing401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	q := New(h.Pool)
	if err := h.requireUserExists(ctx, q, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListUserFollowing404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, err
	}
	limit := resolveLimit(req.Params.Limit)
	rows, err := q.ListFollowing(ctx, ListFollowingParams{
		FollowerUserRef: req.Ref,
		Limit:           int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := openapi.SocialUserList{Users: make([]openapi.SocialUserSummary, 0, len(rows))}
	for _, r := range rows {
		out.Users = append(out.Users, socialUserFromFollowingRow(r))
	}
	return openapi.ListUserFollowing200JSONResponse(out), nil
}

// GetUserRelationship — GET /users/{ref}/relationship.
//
// One round-trip the profile page calls so it can render the right
// Follow/Unfollow/Blocked/Block button without three separate API
// calls. The relationship is anchored on the caller's perspective —
// the anonymous case returns an all-false response (still 200) so
// client code can short-circuit on `is_self`.
func (h *Handler) GetUserRelationship(
	ctx context.Context,
	req openapi.GetUserRelationshipRequestObject,
) (openapi.GetUserRelationshipResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetUserRelationship401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	q := New(h.Pool)
	if err := h.requireUserExists(ctx, q, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetUserRelationship404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, err
	}
	if req.Ref == id.UserRef {
		// Self — short-circuit.
		return openapi.GetUserRelationship200JSONResponse(openapi.UserRelationship{
			IsSelf: true,
		}), nil
	}
	following, err := h.isFollowing(ctx, q, id.UserRef, req.Ref)
	if err != nil {
		return nil, err
	}
	followedBy, err := h.isFollowing(ctx, q, req.Ref, id.UserRef)
	if err != nil {
		return nil, err
	}
	// Two distinct EXISTS for the block axis so we can populate the
	// two booleans separately. HasBlockBetween's bidirectional shape
	// is the right primitive for the visibility gate, but the
	// relationship snapshot needs the per-direction split so the UI
	// can say "you blocked them" vs "they blocked you".
	blockedByMe, err := q.IsBlocking(ctx, IsBlockingParams{
		BlockerUserRef: id.UserRef,
		BlockedUserRef: req.Ref,
	})
	if err != nil {
		return nil, err
	}
	blockedByThem, err := q.IsBlocking(ctx, IsBlockingParams{
		BlockerUserRef: req.Ref,
		BlockedUserRef: id.UserRef,
	})
	if err != nil {
		return nil, err
	}
	return openapi.GetUserRelationship200JSONResponse(openapi.UserRelationship{
		IsSelf:          false,
		IsFollowing:     following,
		IsFollowedBy:    followedBy,
		IsBlockedByMe:   blockedByMe,
		IsBlockedByThem: blockedByThem,
	}), nil
}

// BlockUser — POST /users/{ref}/block.
//
// Block is the contradictory-state stop: any pre-existing follow
// edge in EITHER direction is removed before the block lands. The
// reason field is optional and private to the blocker.
func (h *Handler) BlockUser(
	ctx context.Context,
	req openapi.BlockUserRequestObject,
) (openapi.BlockUserResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.BlockUser401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	if req.Ref == id.UserRef {
		return openapi.BlockUser400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "cannot block yourself",
			},
		}, nil
	}
	q := New(h.Pool)
	if err := h.requireUserExists(ctx, q, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.BlockUser404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, err
	}
	var reason *string
	if req.Body != nil && req.Body.Reason != nil {
		trimmed := strings.TrimSpace(*req.Body.Reason)
		if trimmed != "" {
			reason = &trimmed
		}
	}
	// Auto-unfollow both directions BEFORE recording the block —
	// keeps the state coherent if the block insert somehow fails:
	// the follows are gone, the block isn't, the user can retry.
	// Reverse would leave a block + a stale follow on transient
	// failure.
	if _, err := q.UnfollowUser(ctx, UnfollowUserParams{
		FollowerUserRef: id.UserRef,
		FolloweeUserRef: req.Ref,
	}); err != nil {
		return nil, err
	}
	if _, err := q.UnfollowUser(ctx, UnfollowUserParams{
		FollowerUserRef: req.Ref,
		FolloweeUserRef: id.UserRef,
	}); err != nil {
		return nil, err
	}
	if err := q.BlockUser(ctx, BlockUserParams{
		BlockerUserRef: id.UserRef,
		BlockedUserRef: req.Ref,
		Reason:         reason,
	}); err != nil {
		return nil, err
	}
	// Invalidate both follow directions (block auto-unfollowed) AND
	// the bidirectional block cache. Each NOTIFY broadcasts so peers
	// drop their stale copies too.
	h.invalidateFollowEdge(ctx, id.UserRef, req.Ref)
	h.invalidateFollowEdge(ctx, req.Ref, id.UserRef)
	h.invalidateBlockEdge(ctx, id.UserRef, req.Ref)
	if h.Logger != nil {
		h.Logger.Info("block",
			slog.Int64("blocker", id.UserRef),
			slog.Int64("blocked", req.Ref),
		)
	}
	return openapi.BlockUser204Response{}, nil
}

// UnblockUser — DELETE /users/{ref}/block. Does NOT re-establish
// any prior follow edges (those were removed at block time).
func (h *Handler) UnblockUser(
	ctx context.Context,
	req openapi.UnblockUserRequestObject,
) (openapi.UnblockUserResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UnblockUser401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	q := New(h.Pool)
	if _, err := q.UnblockUser(ctx, UnblockUserParams{
		BlockerUserRef: id.UserRef,
		BlockedUserRef: req.Ref,
	}); err != nil {
		return nil, err
	}
	h.invalidateBlockEdge(ctx, id.UserRef, req.Ref)
	return openapi.UnblockUser204Response{}, nil
}

// ListMyBlocked — GET /account/blocked. Private to the caller; the
// reverse direction ("who blocked me") is deliberately not exposed.
func (h *Handler) ListMyBlocked(
	ctx context.Context,
	req openapi.ListMyBlockedRequestObject,
) (openapi.ListMyBlockedResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListMyBlocked401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	q := New(h.Pool)
	limit := resolveLimit(req.Params.Limit)
	rows, err := q.ListBlocked(ctx, ListBlockedParams{
		BlockerUserRef: id.UserRef,
		Limit:          int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := openapi.BlockedUserList{Users: make([]openapi.BlockedUserSummary, 0, len(rows))}
	for _, r := range rows {
		out.Users = append(out.Users, blockedUserFromRow(r))
	}
	return openapi.ListMyBlocked200JSONResponse(out), nil
}

// requireUserExists returns pgx.ErrNoRows when the target user
// doesn't exist. Used by the handlers to map to 404 rather than
// letting the downstream call surface a less-intelligible error.
func (h *Handler) requireUserExists(ctx context.Context, q *Queries, ref int64) error {
	_, err := q.UserExists(ctx, ref)
	return err
}

// resolveLimit applies the [1, maxListLimit] clamp and the 50
// default when the caller omitted the param.
func resolveLimit(limit *int) int {
	if limit == nil {
		return 50
	}
	v := *limit
	if v < 1 {
		v = 1
	}
	if v > maxListLimit {
		v = maxListLimit
	}
	return v
}

// --- row → openapi projection helpers --------------------------------------

func socialUserFromFollowerRow(r ListFollowersRow) openapi.SocialUserSummary {
	since := r.CreatedAt.Time
	return openapi.SocialUserSummary{
		Ref:         r.Ref,
		Username:    derefString(r.Username),
		Since:       &since,
		DisplayName: r.DisplayName,
		AvatarUrl:   r.AvatarUrl,
	}
}

func socialUserFromFollowingRow(r ListFollowingRow) openapi.SocialUserSummary {
	since := r.CreatedAt.Time
	return openapi.SocialUserSummary{
		Ref:         r.Ref,
		Username:    derefString(r.Username),
		Since:       &since,
		DisplayName: r.DisplayName,
		AvatarUrl:   r.AvatarUrl,
	}
}

func blockedUserFromRow(r ListBlockedRow) openapi.BlockedUserSummary {
	return openapi.BlockedUserSummary{
		Ref:         r.Ref,
		Username:    derefString(r.Username),
		Since:       r.CreatedAt.Time,
		DisplayName: r.DisplayName,
		AvatarUrl:   r.AvatarUrl,
		Reason:      r.Reason,
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// --- cache-aware read helpers ---------------------------------------------

// isFollowing wraps the IsFollowing query with a cache.Cache[bool]
// lookup. Cold miss falls back to DB and populates. Exposed at
// package level for cross-package consumers (posts.handler.go uses
// this for the visibility='followers' gate).
func (h *Handler) isFollowing(ctx context.Context, q *Queries, follower, followee int64) (bool, error) {
	key := followKey(follower, followee)
	if h.followEdge != nil {
		if hit, ok := h.followEdge.Get(key); ok {
			return hit, nil
		}
	}
	v, err := q.IsFollowing(ctx, IsFollowingParams{
		FollowerUserRef: follower,
		FolloweeUserRef: followee,
	})
	if err != nil {
		return false, err
	}
	if h.followEdge != nil {
		h.followEdge.Add(key, v)
	}
	return v, nil
}

// IsFollowing is the cross-package entry point — posts.handler.go
// calls this for visibility='followers' gating. Pool defaults to the
// handler's pool when q is nil to avoid forcing callers to thread
// their own *Queries through.
func (h *Handler) IsFollowing(ctx context.Context, follower, followee int64) (bool, error) {
	return h.isFollowing(ctx, New(h.Pool), follower, followee)
}

// hasBlockBetween wraps HasBlockBetween with the canonical-pair-keyed
// cache. Consumed by both the follow-time validation and (in I2+)
// the notification dispatcher's visibility gate.
func (h *Handler) hasBlockBetween(ctx context.Context, q *Queries, a, b int64) (bool, error) {
	key := canonicalBlockKey(a, b)
	if h.blockEdge != nil {
		if hit, ok := h.blockEdge.Get(key); ok {
			return hit, nil
		}
	}
	v, err := q.HasBlockBetween(ctx, HasBlockBetweenParams{
		BlockerUserRef: a,
		BlockedUserRef: b,
	})
	if err != nil {
		return false, err
	}
	if h.blockEdge != nil {
		h.blockEdge.Add(key, v)
	}
	return v, nil
}

// HasBlockBetween is the cross-package entry point — future I/I2/L
// writers consult this before dispatching notifications, DMs, or
// request approvals against a pair.
func (h *Handler) HasBlockBetween(ctx context.Context, a, b int64) (bool, error) {
	return h.hasBlockBetween(ctx, New(h.Pool), a, b)
}

// --- cache invalidation ---------------------------------------------------

// invalidateFollowEdge drops the follower→followee edge from the LRU
// + counts for both sides. NOTIFY broadcasts each invalidation.
func (h *Handler) invalidateFollowEdge(ctx context.Context, follower, followee int64) {
	if h.followEdge != nil {
		if err := h.followEdge.Invalidate(ctx, followKey(follower, followee)); err != nil && h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "social.cache.invalidate.error",
				slog.String("domain", cacheDomainFollowEdge),
				slog.String("err", err.Error()),
			)
		}
	}
	if h.followerCount != nil {
		_ = h.followerCount.Invalidate(ctx, countKey(followee))
	}
	if h.followingCount != nil {
		_ = h.followingCount.Invalidate(ctx, countKey(follower))
	}
}

// invalidateBlockEdge drops the canonical-pair block cache. Called
// on block + unblock + (indirectly) when an admin force-clears blocks.
func (h *Handler) invalidateBlockEdge(ctx context.Context, a, b int64) {
	if h.blockEdge != nil {
		if err := h.blockEdge.Invalidate(ctx, canonicalBlockKey(a, b)); err != nil && h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "social.cache.invalidate.error",
				slog.String("domain", cacheDomainBlockEdge),
				slog.String("err", err.Error()),
			)
		}
	}
}
