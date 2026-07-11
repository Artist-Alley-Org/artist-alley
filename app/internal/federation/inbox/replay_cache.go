// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox

import (
	"sync"
	"time"
)

// ReplayCache is a bounded short-TTL set of recently-received
// activity URIs. The inbox pipeline checks it BEFORE running
// HTTP-Signature verification — a replayed envelope (legit
// retry from a peer, or hostile replay within the ±5min
// window) short-circuits to "already seen" without burning
// the per-request verify cost.
//
// Authoritative dedup is the federation_inbox.activity_uri
// UNIQUE constraint — the cache is an OPTIMISATION, not a
// correctness primitive. A cold cache after process restart
// just means the DB catches the dup instead. Per the design
// proposal §5.5 addition B: the UNIQUE index is the
// load-bearing primitive; this cache is defense-in-depth +
// CPU-burn protection.
//
// Sizing: the default 16,384-entry LRU at 30s TTL covers
// ~500 envelopes/sec arrival rate — well past the §5.5
// addition A rate limit (100 req/sec per peer × small N peers).
// Old entries age out via TTL even if the LRU never evicts.
//
// Concurrency: protected by a single sync.Mutex. The cache
// operations are O(1); the mutex won't be the bottleneck at
// the rates the rate-limit allows.
type ReplayCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	max     int
	ttl     time.Duration
}

// NewReplayCache builds a cache with the supplied capacity +
// TTL. Both defaults are tuned for the walled-garden v1 scale
// per the design proposal; callers can override for tests.
func NewReplayCache(maxEntries int, ttl time.Duration) *ReplayCache {
	if maxEntries <= 0 {
		maxEntries = 16_384
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &ReplayCache{
		entries: make(map[string]time.Time, maxEntries),
		max:     maxEntries,
		ttl:     ttl,
	}
}

// Seen reports whether the activity URI was seen in the last
// TTL window. Side effect: regardless of return value, the
// entry's timestamp is refreshed (so a peer retrying the same
// envelope stays "seen" for another full TTL). Caller is
// expected to short-circuit on `true`.
//
// Cold-cache safety: this is best-effort. The DB UNIQUE
// constraint is the authoritative dedup; missing the cache
// just means we run the extra verify.
func (c *ReplayCache) Seen(uri string) bool {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpiredLocked(now)
	if ts, ok := c.entries[uri]; ok && now.Sub(ts) < c.ttl {
		// Refresh + report seen.
		c.entries[uri] = now
		return true
	}
	c.entries[uri] = now
	// Bound the map size: if we're over the cap, evict the
	// oldest entry. Cheap because the cap is small + this
	// only fires on saturation.
	if len(c.entries) > c.max {
		c.evictOldestLocked()
	}
	return false
}

// Len returns the current number of cached entries.
// Exposed for observability + tests.
func (c *ReplayCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// evictExpiredLocked drops entries older than TTL. Called
// before any read so a peer's burst of expired entries doesn't
// linger and skew the eviction policy.
func (c *ReplayCache) evictExpiredLocked(now time.Time) {
	// Only iterate when the map is over a small threshold —
	// otherwise the per-call scan dominates the cache cost.
	if len(c.entries) < 64 {
		return
	}
	for uri, ts := range c.entries {
		if now.Sub(ts) >= c.ttl {
			delete(c.entries, uri)
		}
	}
}

// evictOldestLocked drops a single oldest entry. O(n) scan but
// only fires when the map exceeds capacity; bounded by the
// chosen cap (16k by default).
func (c *ReplayCache) evictOldestLocked() {
	var oldestURI string
	var oldestTS time.Time
	first := true
	for uri, ts := range c.entries {
		if first || ts.Before(oldestTS) {
			oldestURI = uri
			oldestTS = ts
			first = false
		}
	}
	if oldestURI != "" {
		delete(c.entries, oldestURI)
	}
}
