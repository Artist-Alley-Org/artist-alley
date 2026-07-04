package lockout

import (
	"context"
	"strconv"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Domain is the cache.Registry domain name for lockout-status entries.
// Cross-instance invalidation via LISTEN/NOTIFY carries this string
// as the Payload.Domain field; peer instances subscribed to the same
// domain receive drops on the matching key.
const Domain = "auth.lockout"

// DefaultCacheSize is the LRU max-entries. Sized for a solo-deploy
// working set: ~2k active users * (failed_count + timestamp) fits
// comfortably in <200 KB. Peer instances have their own cache; the
// LISTEN/NOTIFY broadcast keeps them consistent.
const DefaultCacheSize = 4096

// DefaultTTL is the freshness window before a cached entry expires
// even without an explicit invalidation. 60s balances staleness cost
// (a user who just fixed their password waits at most 60s to be
// unlocked in cache) against DB load (auth path hits the cache 95%+
// of the time). Explicit invalidations on write shorten this to zero.
const DefaultTTL = 60 * time.Second

// CachedState is the entry shape stored in the LRU. Two fields:
// FailedCount (visibility for admin surfaces + future rate math) and
// LockoutUntil (the load-bearing gate). fetchedAt drives TTL.
type CachedState struct {
	FailedCount  int32
	LockoutUntil time.Time
	fetchedAt    time.Time
}

// Cache is the LockoutStatusCache. Wraps cache.Cache[CachedState]
// (int64 user_ref encoded as decimal string in the shared key space)
// so we inherit LISTEN/NOTIFY cross-instance invalidation for free.
// Nil-safe on every method: a nil receiver returns as if the entry
// weren't present.
type Cache struct {
	inner *cache.Cache[CachedState]
	ttl   time.Duration
}

// NewCache constructs a Cache and registers it with the process-wide
// registry so LISTEN/NOTIFY invalidations reach it from peer
// instances. Uses DefaultCacheSize entries + DefaultTTL freshness.
// Nil registry returns nil (single-process test path skips caching).
func NewCache(registry *cache.Registry) *Cache {
	if registry == nil {
		return nil
	}
	return &Cache{
		inner: cache.Register[CachedState](registry, Domain, DefaultCacheSize),
		ttl:   DefaultTTL,
	}
}

// keyFor stringifies a user_ref for the shared key space.
func keyFor(userRef int64) string {
	return strconv.FormatInt(userRef, 10)
}

// Get returns the cached entry if fresh (within TTL). Nil-safe.
func (c *Cache) Get(userRef int64) (CachedState, bool) {
	if c == nil || c.inner == nil {
		return CachedState{}, false
	}
	entry, ok := c.inner.Get(keyFor(userRef))
	if !ok {
		return CachedState{}, false
	}
	if time.Since(entry.fetchedAt) > c.ttl {
		// Cache.Add re-inserts with fresh fetchedAt on next Put;
		// the stale entry stays but reads miss until refreshed.
		return CachedState{}, false
	}
	return entry, true
}

// Put stores an entry with the fetchedAt stamp set to NOW. Uses Add
// (local-only) rather than Invalidate — populating on read shouldn't
// broadcast to peers.
func (c *Cache) Put(userRef int64, state CachedState) {
	if c == nil || c.inner == nil {
		return
	}
	state.fetchedAt = time.Now()
	c.inner.Add(keyFor(userRef), state)
}

// Invalidate drops the entry locally AND broadcasts to peers via
// LISTEN/NOTIFY through the shared registry. Called from every
// state-change path (IncrementFailedLogin, ResetFailedLogin,
// AdminUnlock).
//
// Uses context.Background so a caller-cancelled request doesn't
// abort the broadcast — invalidation is best-effort but MUST
// always attempt to propagate.
func (c *Cache) Invalidate(userRef int64) {
	if c == nil || c.inner == nil {
		return
	}
	_ = c.inner.Invalidate(context.Background(), keyFor(userRef))
}

// Len returns the current entry count. Used by tests + observability.
func (c *Cache) Len() int {
	if c == nil || c.inner == nil {
		return 0
	}
	return c.inner.Len()
}
