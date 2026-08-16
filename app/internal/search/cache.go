// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	appcache "github.com/mscrnt/artist-alley/app/internal/cache"
)

// CacheDomain is the domain name the QueryResultCache registers
// under in the shared cache.Registry. Cross-instance invalidations
// (broadcast via the existing "cache_invalidate" LISTEN/NOTIFY
// channel) travel with this string.
const CacheDomain = "search.query_result"

// DefaultCacheMaxEntries caps the in-process LRU. Bounded so a
// runaway query loop can't blow the heap.
const DefaultCacheMaxEntries = 10000

// DefaultCacheTTL is how long a cached QueryResult stays fresh
// before the next request bypasses the cache. Coarse invalidation
// (any write purges) is the primary consistency mechanism; the
// TTL just backstops the case where a write happened on a peer
// that couldn't broadcast (e.g., NOTIFY dropped).
const DefaultCacheTTL = 60 * time.Second

// Cache is the QueryResultCache. Wraps the generic cache.Cache from
// app/internal/cache so peer instances receive purges over the
// existing LISTEN/NOTIFY infrastructure with zero extra wiring.
type Cache struct {
	inner *appcache.Cache[cacheEntry]
	stats stats

	// invalidateAll is the coarse-invalidation broadcaster; wraps
	// inner.InvalidateAll so tests can substitute a no-op.
	invalidateAll func(ctx context.Context) error

	// nowFunc is time.Now, indirected so tests can freeze the
	// clock.
	nowFunc func() time.Time
	logger  *slog.Logger

	// ttl is the freshness window. Sub-TTL requests hit the cache;
	// past-TTL requests bypass + refresh.
	ttl time.Duration
}

// cacheEntry is one stored QueryResult with the time it was
// stored — TTL is checked at read time so the LRU never returns
// stale entries even before the next write triggers a purge.
type cacheEntry struct {
	Stored time.Time
	Result QueryResult
}

// stats tracks the counters surfaced by /admin/search/health.
type stats struct {
	hits          atomic.Int64
	misses        atomic.Int64
	invalidations atomic.Int64
}

// NewCache constructs a Cache and registers it with the shared
// cache.Registry under CacheDomain. maxEntries is the LRU bound;
// pass 0 for DefaultCacheMaxEntries. ttl is the freshness window;
// pass 0 for DefaultCacheTTL.
func NewCache(reg *appcache.Registry, maxEntries int, ttl time.Duration, logger *slog.Logger) *Cache {
	if maxEntries <= 0 {
		maxEntries = DefaultCacheMaxEntries
	}
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	inner := appcache.Register[cacheEntry](reg, CacheDomain, maxEntries)
	c := &Cache{
		inner:   inner,
		nowFunc: time.Now,
		logger:  logger,
		ttl:     ttl,
	}
	c.invalidateAll = func(ctx context.Context) error {
		return inner.InvalidateAll(ctx)
	}
	return c
}

// keyForQuery returns the LRU key for a Query. Includes caller
// user_ref so User A's cached result never serves to User B —
// the visibility floor at the cache layer.
//
// #899 — it also includes the caller's CONTENT CAPABILITIES, because
// the cached QueryResult is now the post-withholding one. User ref
// alone was sufficient while every caller with the same ref got the
// same bytes; it stopped being sufficient the moment two requests from
// the same ref could differ. The direction that matters is REVOKE: a
// caller who loses `content.read.all` would otherwise keep being served
// the cached UNREDACTED titles and thumbhashes for the rest of the TTL.
func keyForQuery(q Query) string {
	var callerID int64
	if q.CallerUserRef != nil {
		callerID = *q.CallerUserRef
	}
	// Sort types so [asset,post] and [post,asset] produce the same
	// key. Local copy so we don't reorder the caller's slice.
	types := make([]string, 0, len(q.Types))
	for _, t := range q.Types {
		types = append(types, string(t))
	}
	// Deterministic order without importing sort here — the sets
	// are tiny + fixed-size, three at most.
	orderTypesInPlace(types)
	sb := strings.Builder{}
	sb.WriteString(q.Text)
	sb.WriteByte('|')
	sb.WriteString(strings.Join(types, ","))
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatInt(callerID, 10))
	sb.WriteByte('|')
	sb.WriteString(q.Caps.CacheKey())
	sb.WriteByte('|')
	// #873 — and the post capabilities, for the same reason in the same
	// direction: `posts.admin` widens which post ROWS the result set
	// contains, so a caller who loses it would otherwise keep being
	// served the cached wider page for the rest of the TTL.
	sb.WriteString(q.PostCaps.CacheKey())
	sb.WriteByte('|')
	// #939 — and the asset-mutation scope, same reason, same direction:
	// `assets.admin` widens the FIELDS a restricted asset's card
	// carries, so a caller who LOSES it would otherwise keep being
	// served the cached titles, descriptions and tags for the rest of
	// the TTL. Folding a per-caller value in costs no cross-caller hit
	// rate, because the key already includes the caller's user_ref —
	// see AssetMutationCaps.CacheKey for the full argument.
	sb.WriteString(q.MutationCaps.CacheKey())
	sb.WriteByte('|')
	// #907 — and the FACET SELECTION, which is a different kind of
	// argument from the three above it and worth saying so.
	//
	// Those three are revoke-direction arguments: the same query from
	// the same caller, and a capability change makes the cached bytes
	// wrong in one direction. This one is plain correctness and it fails
	// in BOTH directions. Two selections on the same `q` from the same
	// caller are two different result sets; leaving the selection out of
	// the key would have `filter=extension:png` and
	// `filter=extension:jpg` collide on one entry and serve each other's
	// hits — and the unfiltered page serve a filtered one — for the rest
	// of the TTL.
	//
	// Filters were absent from this key only because the engine ignored
	// them. The moment they are real they belong here, and that is the
	// whole rule: every input the Engine READS is a component of the key.
	sb.WriteString(q.Filters.CacheKey())
	sb.WriteByte('|')
	// #1117 — and the MATURE AXIS, which is a third kind of argument
	// again: two of its three conjuncts are not properties of the caller
	// at all.
	//
	// `show_mature` is a PREFERENCE, so it moves without any capability
	// changing and without the caller changing — the same ref, the same
	// caps, the same query, two different result sets. And the instance
	// switch is the OPERATOR's, so it moves for every caller at once: an
	// operator who disallows mature content would otherwise keep serving
	// every warm entry's mature rows until the TTL expired, which is the
	// staleness sysconfig.SetMatureContent refuses to allow at the read
	// and would have reintroduced here.
	//
	// Three characters, one per conjunct, fixed-width — so a missing
	// conjunct cannot alias with a present one. See
	// visibility.MatureViewer.CacheKey, whose doc names this function as
	// its consumer.
	sb.WriteString(q.Mature.CacheKey())
	sb.WriteByte('|')
	sb.WriteString(strconv.Itoa(q.Limit))
	sb.WriteByte('|')
	// Phase 1.16.B-3 — vector-hint identifier folds into the key
	// so similar_to:<uuid> queries cache independently of their
	// text component. Empty for pure-BM25 queries; asset:<uuid>
	// for DSL similar_to; image:<sha256> for the reserved
	// /search/by-image endpoint.
	sb.WriteString(q.SimilarityHintID)
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatFloat(q.HybridWeight, 'g', -1, 64))
	sb.WriteByte('|')
	if q.Cursor != nil {
		sb.WriteString(strconv.FormatFloat(q.Cursor.LastScore, 'g', -1, 64))
		sb.WriteByte(':')
		sb.WriteString(q.Cursor.LastID.String())
		sb.WriteByte(':')
		sb.WriteString(string(q.Cursor.LastType))
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// orderTypesInPlace sorts a tiny (<=3) string slice with a hand-
// rolled selection sort. Avoids the sort.Strings import path and
// the resulting escape-to-heap for a slice this small.
func orderTypesInPlace(s []string) {
	for i := 0; i < len(s); i++ {
		min := i
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[min] {
				min = j
			}
		}
		if min != i {
			s[i], s[min] = s[min], s[i]
		}
	}
}

// Get returns the cached result for q, or (zero, false) on miss /
// stale entry. Missed reads bump the miss counter; hit reads bump
// the hit counter.
func (c *Cache) Get(q Query) (QueryResult, bool) {
	key := keyForQuery(q)
	v, ok := c.inner.Get(key)
	if !ok {
		c.stats.misses.Add(1)
		return QueryResult{}, false
	}
	if c.nowFunc().Sub(v.Stored) > c.ttl {
		// Stale — drop the entry locally + treat as miss. No
		// broadcast; the entry expiring in-place isn't a
		// consistency event other peers need to know about.
		c.stats.misses.Add(1)
		return QueryResult{}, false
	}
	c.stats.hits.Add(1)
	// Return a deep copy so caller mutations of Hits don't touch
	// the cached entry. Hits are values, but ExtraJSON is a []byte
	// shared reference.
	return cloneResult(v.Result), true
}

// Put stores q → r in the cache. No return value — the write is
// best-effort. Overwrites any existing entry for the same key.
func (c *Cache) Put(q Query, r QueryResult) {
	c.inner.Add(keyForQuery(q), cacheEntry{Stored: c.nowFunc(), Result: cloneResult(r)})
}

// cloneResult returns a shallow-plus-JSON copy of r. Hits are
// value-copied by the slice-append; ExtraJSON is byte-copied.
func cloneResult(r QueryResult) QueryResult {
	out := r
	if r.Hits != nil {
		out.Hits = make([]Hit, len(r.Hits))
		copy(out.Hits, r.Hits)
		for i := range out.Hits {
			if len(r.Hits[i].ExtraJSON) > 0 {
				buf := make([]byte, len(r.Hits[i].ExtraJSON))
				copy(buf, r.Hits[i].ExtraJSON)
				out.Hits[i].ExtraJSON = buf
			}
		}
	}
	if r.NextCursor != nil {
		c := *r.NextCursor
		out.NextCursor = &c
	}
	if r.TypesMatched != nil {
		out.TypesMatched = make([]HitType, len(r.TypesMatched))
		copy(out.TypesMatched, r.TypesMatched)
	}
	return out
}

// InvalidateAll drops every cached entry and broadcasts a domain
// purge to peer instances. Called by the cross-package invalidators
// below on any write to a searchable entity.
func (c *Cache) InvalidateAll(ctx context.Context) error {
	c.stats.invalidations.Add(1)
	return c.invalidateAll(ctx)
}

// InvalidateOnAssetWrite drops the cache in response to an asset
// write. Coarse — any asset touch could change the ranking of any
// query. Matches the 60s TTL cadence + keeps the invalidation
// logic dependency-free of per-query provenance tracking.
func (c *Cache) InvalidateOnAssetWrite(ctx context.Context, _ uuid.UUID) {
	if err := c.InvalidateAll(ctx); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "search.cache.invalidate_asset_error",
			slog.String("err", err.Error()))
	}
}

// InvalidateOnCollectionWrite drops the cache in response to a
// collection write.
func (c *Cache) InvalidateOnCollectionWrite(ctx context.Context, _ uuid.UUID) {
	if err := c.InvalidateAll(ctx); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "search.cache.invalidate_collection_error",
			slog.String("err", err.Error()))
	}
}

// InvalidateOnPostWrite drops the cache in response to a post
// write.
func (c *Cache) InvalidateOnPostWrite(ctx context.Context, _ uuid.UUID) {
	if err := c.InvalidateAll(ctx); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "search.cache.invalidate_post_error",
			slog.String("err", err.Error()))
	}
}

// InvalidateOnTagChange drops the cache in response to any tag
// change — tags appear on many entities so per-tag targeting isn't
// meaningfully cheaper than the coarse purge.
func (c *Cache) InvalidateOnTagChange(ctx context.Context, _ string) {
	if err := c.InvalidateAll(ctx); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "search.cache.invalidate_tag_error",
			slog.String("err", err.Error()))
	}
}

// InvalidateOnFieldValueWrite drops the cache when a custom field
// value changes on an asset. Custom fields feed the assets
// search_text tsvector via the rebuild function's aggregation.
func (c *Cache) InvalidateOnFieldValueWrite(ctx context.Context, _ uuid.UUID, _ int64) {
	if err := c.InvalidateAll(ctx); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn, "search.cache.invalidate_fieldvalue_error",
			slog.String("err", err.Error()))
	}
}

// Stats returns a snapshot of the cache counters. Surfaced by
// /admin/search/health.
func (c *Cache) Stats() CacheStatsSnapshot {
	return CacheStatsSnapshot{
		Entries:       c.inner.Len(),
		Hits:          c.stats.hits.Load(),
		Misses:        c.stats.misses.Load(),
		Invalidations: c.stats.invalidations.Load(),
	}
}

// CacheStatsSnapshot is the read-side projection of the cache
// counters. All fields are cheap Copy-of-Atomic values.
type CacheStatsSnapshot struct {
	Entries       int   `json:"entries"`
	Hits          int64 `json:"hits"`
	Misses        int64 `json:"misses"`
	Invalidations int64 `json:"invalidations"`
}
