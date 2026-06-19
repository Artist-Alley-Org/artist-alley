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
// global pending-count cache key. Cross-package entry point —
// used by the auth.CapabilitySweeper when a request-cascade
// reaps a granted request (so peer instances' badge caches
// drop their stale entries).
//
// Implementation: emit a NOTIFY on the same channel the cache
// listens on. The LOCAL cache eviction is the Handler's
// responsibility (h.counts.c.Invalidate handles both local +
// broadcast); this helper is the broadcast-only path for
// external callers that don't hold the local cache reference.
func InvalidatePendingCountAll(ctx context.Context, registry *cache.Registry) {
	if registry == nil {
		return
	}
	_ = registry.Emit(ctx, CacheDomainPendingRequestCount, countCacheKey)
}

// InvalidatePendingCountFor — reserved for per-approver
// filtering follow-up. At MVP all approvers share the global
// key, so this delegates to InvalidatePendingCountAll.
//
// approverRef is kept on the signature so call sites can
// pre-emptively pass the deciding admin's ref; when per-
// approver filtering ships, this routes to that specific
// key without churning the call sites.
func InvalidatePendingCountFor(ctx context.Context, registry *cache.Registry, approverRef int64) {
	_ = approverRef
	InvalidatePendingCountAll(ctx, registry)
}
