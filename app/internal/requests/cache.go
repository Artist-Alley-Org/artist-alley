// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.E — per-admin pending-request count cache.
//
// The approver-facing badge / count surfaces ("you have N pending
// requests waiting") read this on every page load. The underlying
// query is COUNT(*) on a partial index — cheap per call — but at
// admin-population scale on a busy install the LISTEN/NOTIFY
// invalidation pattern keeps the dispatcher cold-cached on every
// admin's badge.
//
// # Key shape
//
// Key = approver user_ref (int64 stringified).
// Value = count (int64).
//
// 5k entries comfortably fits the entire admin/approver pool on
// any plausible install. The brief calls out the per-admin shape
// — different approvers see different counts because the
// capability gate filters what each can actually decide.
//
// Phase-D note: 1.17.E ships an UNFILTERED count (all pending
// rows) because per-approver capability filtering is a follow-up.
// The cache key is still per-approver so when the filter ships
// the cache contract doesn't change.
//
// # There is no exported invalidator, deliberately (#947)
//
// This package used to export InvalidatePendingCountAll and
// InvalidatePendingCountFor as a "broadcast-only path for callers
// outside this package". Neither was ever called, and the state
// machine says neither can be: `pending` is entered only by Submit
// and left only by Grant / Deny (state.go — pending→expired is not in
// the matrix, the sweeper walks granted→expired), and all three run
// inside this package and already call h.invalidateCount, which evicts
// locally AND broadcasts in one call.
//
// So an external caller would necessarily be evicting on a transition
// that does not change the count. Do not re-add one: add the eviction
// to the transition instead, next to the write that causes it.

package requests

import (
	"log/slog"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// CacheDomainPendingRequestCount is the registry domain for the
// per-approver count cache. NOTIFY/LISTEN dispatches eviction
// across instances when any request is submitted / decided.
const CacheDomainPendingRequestCount = "request.pending_count"

// countCacheKey is the single key under which the unfiltered
// pending count lives. MVP serves every approver the same
// number; per-approver capability filtering is a polish-phase
// follow-up that swaps this for per-approver keys.
const countCacheKey = "all"

// pendingCountCache wraps the typed cache.Cache so the Handler's
// read path stays clean. nil-safe — tests without a registry get
// a Handler whose count path queries PG every time.
type pendingCountCache struct {
	c      *cache.Cache[int64]
	logger *slog.Logger
}

func newPendingCountCache(registry *cache.Registry, logger *slog.Logger) *pendingCountCache {
	if registry == nil {
		return nil
	}
	return &pendingCountCache{
		c:      cache.Register[int64](registry, CacheDomainPendingRequestCount, 5_000),
		logger: logger,
	}
}
