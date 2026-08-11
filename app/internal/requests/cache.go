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

package requests

import (
	"context"
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
// follow-up that swaps this for per-approver keys without
// changing the InvalidatePendingCountAll contract.
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

// InvalidatePendingCountAll broadcasts an eviction for the
// global pending-count cache key. It is the broadcast-only path
// for callers outside this package, which do not hold the local
// cache reference; the Handler's own writes use h.invalidateCount,
// which evicts locally AND broadcasts in one call.
//
// Implementation: emit a NOTIFY on the same channel the cache
// listens on.
//
// # ZERO CALLERS, and that is correct (#935)
//
// This doc used to assert that "the auth.CapabilitySweeper" called
// this "when a request-cascade reaps a granted request". It does
// not, and it should not:
//
//   - the sweeper's cascade is SetRequestCascade →
//     requests.Handler.MarkExpired, which moves a request from
//     granted to expired;
//   - the cached number counts PENDING rows. granted→expired is not
//     a pending-count transition, so there is nothing stale to
//     evict.
//
// Every transition that DOES change the count (submit, decide) runs
// inside this package and already calls h.invalidateCount. So the
// absence of an external caller is the design working, not a wiring
// gap — do not "fix" it by inventing one.
func InvalidatePendingCountAll(ctx context.Context, registry *cache.Registry) {
	if registry == nil {
		return
	}
	_ = registry.Emit(ctx, CacheDomainPendingRequestCount, countCacheKey)
}

// InvalidatePendingCountFor — RESERVED. Zero callers, deliberately,
// and #935 swept it once already. Read this before flagging it again.
//
// What it is for: per-approver capability filtering (the Phase-D note
// at the top of this file). Today every approver shares countCacheKey,
// so this can only delegate to InvalidatePendingCountAll; when the
// filter ships, the key becomes per-approver and this routes to it.
//
// # The honest counter-argument, so the next sweep does not
// # have to reconstruct it
//
// The stated rationale — "keep the ref on the signature so call sites
// don't churn later" — protects nothing, because there are no call
// sites to protect. It is a forward-compatible signature for a
// function nobody invokes, delegating to another function nobody
// invokes. On that reading it is dead code, not reserved code, and the
// argument for deleting both is a good one.
//
// It is kept because deleting a documented seam is a scope decision,
// not a sweep decision, and because the per-approver follow-up is
// still on the board. If that follow-up is dropped, delete this and
// InvalidatePendingCountAll together — they stand or fall as a pair.
//
// What it must NOT be is wired to a caller for the sake of having one.
// A call from a path that does not change a pending count is not
// wiring; it is a cache eviction that fires for no reason and a false
// precedent for the next reader.
func InvalidatePendingCountFor(ctx context.Context, registry *cache.Registry, approverRef int64) {
	_ = approverRef
	InvalidatePendingCountAll(ctx, registry)
}
