// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.A — per-user state cache.
//
// Read on every login attempt (the auth-layer guard in Commit 2
// resolves the target user's state before deciding whether to mint
// a session). Write-invalidated by every transition. A single int64
// per entry; capacity 50k comfortably fits the active-user set on
// any plausible install — and the LRU eviction handles overflow.
//
// # Why a separate cache from h.byRef (UserPublic)
//
// h.byRef holds the full openapi.UserPublic shape (profile fields,
// counts, derived URIs). Today UserPublic does NOT carry the
// approval state — adding it would force every read site to widen
// + every write site to re-warm. A dedicated state-only cache:
//
//   * Stays cheap (int64-per-entry, no struct copy on hit).
//   * Invalidates independently (a profile edit shouldn't bust the
//     state cache; a state transition shouldn't bust profile reads
//     that don't carry state today).
//   * Is read by the auth boundary BEFORE the user is "logged in" —
//     i.e. before any UserPublic profile lookup happens — so the
//     state read is on the cold-cache path more often than not.
//     A small dedicated cache warms faster than the full profile
//     cache (which also takes the user's tag/post counts via a
//     join the auth gate doesn't need).
//
// # Cross-process invalidation
//
// Per ADR 0013, cache.Cache writes through to the registry's
// NOTIFY/LISTEN bus. A state transition on instance A invalidates
// the same key on instance B at the next message poll. The auth
// guard's read-through pattern means worst-case staleness is
// bounded by the LISTEN poll interval (sub-second on default
// settings).

package users

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// CacheDomainUserState is the cache.Registry domain for per-user
// state. Per-domain const so the registry can NOTIFY-route across
// instances without ambiguity.
const CacheDomainUserState = "user.state"

// UserStateCache wraps cache.Cache[UserState] with read-through
// from Postgres. Constructed in NewHandler when a registry is
// provided; the Handler exposes Get/Invalidate via methods that
// nil-check (tests can construct a bare Handler without a
// registry and the state methods degrade to direct PG reads).
type UserStateCache struct {
	c *cache.Cache[UserState]
}

// newUserStateCache registers a 50k-entry LRU for UserState
// values on the provided registry. Returns nil when registry is
// nil so tests that don't wire caching get a Handler whose
// state path falls through to PG every time.
func newUserStateCache(registry *cache.Registry) *UserStateCache {
	if registry == nil {
		return nil
	}
	return &UserStateCache{
		c: cache.Register[UserState](registry, CacheDomainUserState, 50_000),
	}
}

// GetUserState resolves a user's state. Cache-first; on miss,
// queries user.approved + caches the result. Returns the typed
// UserState (not the raw int64) so call sites can use the typed
// helpers (CanAuthenticate, ValidateTransition).
//
// Unknown / out-of-range column values defensively map to
// UserStateDisabled — the schema CHECK from migration 00001 should
// make this impossible, but defense-in-depth at the boundary
// means a corrupted row can't be reported as "active".
func (h *Handler) GetUserState(ctx context.Context, userRef int64) (UserState, error) {
	key := strconv.FormatInt(userRef, 10)
	if h.state != nil {
		if hit, ok := h.state.c.Get(key); ok {
			return hit, nil
		}
	}
	var approved int64
	if err := h.Pool.QueryRow(ctx,
		`SELECT approved FROM "user" WHERE ref = $1`, userRef,
	).Scan(&approved); err != nil {
		return 0, fmt.Errorf("users: read state for ref=%d: %w", userRef, err)
	}
	s := UserState(approved)
	if !s.IsKnown() {
		// Schema CHECK should prevent this; defense-in-depth.
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn,
				"users.state.unknown_value",
				slog.Int64("user_ref", userRef),
				slog.Int64("approved", approved),
			)
		}
		s = UserStateDisabled
	}
	if h.state != nil {
		h.state.c.Add(key, s)
	}
	return s, nil
}

// InvalidateUserState clears the cached state for one user, broadcasting
// the eviction to peer instances via the registry's NOTIFY bus. Called
// from every transition site (Approve / Disable / Archive / Restore).
// nil-safe.
func (h *Handler) InvalidateUserState(ctx context.Context, userRef int64) {
	if h.state == nil {
		return
	}
	key := strconv.FormatInt(userRef, 10)
	if err := h.state.c.Invalidate(ctx, key); err != nil && h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn,
			"users.state.cache.invalidate.failed",
			slog.Int64("user_ref", userRef),
			slog.String("err", err.Error()),
		)
	}
}

// InvalidateUserState is the cross-package entry point — mirrors
// InvalidateProfile. Callers in auth/ (Commit 2's session
// revocation cascade) use this to bust the state cache after
// mutating user.approved out of band (e.g. the bootstrap path
// promoting a user from pending to active).
//
// nil-safe handler: when subtitles-style optional wiring leaves
// the registry unset, the call is a no-op.
func InvalidateUserState(ctx context.Context, registry *cache.Registry, userRef int64) {
	if registry == nil {
		return
	}
	key := strconv.FormatInt(userRef, 10)
	_ = registry.Emit(ctx, CacheDomainUserState, key)
}
