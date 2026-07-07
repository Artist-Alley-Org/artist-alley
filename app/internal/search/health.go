package search

import (
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/observability/healthhandler"
)

func strconvFormatInt(n int64) string { return strconv.FormatInt(n, 10) }

// Counter is the observability surface for the search subsystem.
// Wired through observability/healthhandler.HandlerFor to expose
// GET /admin/search/health for admin dashboards.
//
// Counters:
//   - Requests, per result class (hit / empty / error / cache_hit /
//     cache_miss / rate_limited)
//   - Latency samples over a rolling window (p50/p95/p99 percentiles)
//
// The Cache exposes its own hit/miss/invalidation counters via
// Cache.Stats(); we surface those alongside the request counters in
// the Snapshot payload so the admin UI has a single JSON to render.
type Counter struct {
	mu       sync.Mutex
	requests map[string]int64

	// rolling latency window; append-then-slice-to-window
	latencies []time.Duration
	windowCap int

	// cacheStats is the callback the health handler uses to pull
	// live cache counters at snapshot time. Wired by boot; nil in
	// tests that don't care.
	cacheStats func() CacheStatsSnapshot

	// gaugeStats is the callback for pg_stat-backed gauges
	// (Phase 1.16.B-5). Nil-safe. Returns a map of
	// "gauge_name" → int64 that gets appended to Notes[].
	gaugeStats func() map[string]int64

	// startedAt is set by NewCounter; surfaced as an uptime hint
	// via the shim's Uptime field.
	startedAt time.Time
}

// NewCounter constructs a Counter with the requested rolling-
// window capacity. Passing 0 defaults to 5000 samples — ~10 min
// of traffic at 8 req/s.
func NewCounter(windowCap int) *Counter {
	if windowCap <= 0 {
		windowCap = 5000
	}
	return &Counter{
		requests:  map[string]int64{},
		latencies: make([]time.Duration, 0, windowCap),
		windowCap: windowCap,
		startedAt: time.Now(),
	}
}

// SetCacheStatsProvider wires the callback the Snapshot uses to
// pull live cache counters. Boot calls this after constructing
// both the Counter and the Cache.
func (c *Counter) SetCacheStatsProvider(fn func() CacheStatsSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cacheStats = fn
}

// SetGaugeStatsProvider wires the pg_stat-backed gauge callback.
// Phase 1.16.B-5 addition. Values are appended to Notes[] as
// "gauge_name=<value>" lines so the flat health JSON surface
// keeps working without a schema evolution.
func (c *Counter) SetGaugeStatsProvider(fn func() map[string]int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gaugeStats = fn
}

// Result classifies the outcome of one /search request for the
// requests_total counter. String rather than iota so the JSON
// snapshot is human-readable without a symbol table lookup.
type Result string

const (
	ResultHit         Result = "hit"
	ResultEmpty       Result = "empty"
	ResultError       Result = "error"
	ResultCacheHit    Result = "cache_hit"
	ResultCacheMiss   Result = "cache_miss"
	ResultRateLimited Result = "rate_limited"
	ResultBadRequest  Result = "bad_request"
	// Phase 1.16.B-3 result classes — surfaced alongside the
	// existing BM25 counters so operators see hybrid + vector
	// query volume separately.
	ResultVectorHit             Result = "vector_hit"
	ResultVectorMiss            Result = "vector_miss"
	ResultSimilarToNotEmbedded  Result = "similar_to_not_embedded"
	ResultByImageNotImplemented Result = "by_image_not_implemented"
	ResultDSLParseError         Result = "dsl_parse_error"
	// Phase 1.16.B-4 result classes for the saved-search
	// coordinator + run jobs. Surfaced in the by_result map on
	// /admin/search/health so operators see coordinator health +
	// per-run outcome mix.
	ResultSavedSearchCoordinatorTick Result = "saved_search_coordinator_tick"
	ResultSavedSearchRunHit          Result = "saved_search_run_hit"
	ResultSavedSearchRunEmpty        Result = "saved_search_run_empty"
	ResultSavedSearchRunDisabled     Result = "saved_search_run_disabled"
	ResultSavedSearchRunError        Result = "saved_search_run_error"
	ResultSavedSearchDeltaHit        Result = "saved_search_delta_hit"
	ResultSavedSearchNotificationSent Result = "saved_search_notification_sent"
	// Phase 1.16.B-5-followup — feedback loop result classes.
	// Increment per successful Submit/Delete; the shared search
	// Counter's requests[] map surfaces them alongside the query
	// result classes so operators can spot feedback traffic mixed
	// with search traffic on one health JSON.
	ResultSearchFeedbackUp       Result = "search_feedback_up"
	ResultSearchFeedbackDown     Result = "search_feedback_down"
	ResultSearchFeedbackUndo     Result = "search_feedback_undo"
	ResultSearchFeedbackRateLimit Result = "search_feedback_rate_limit"
	ResultSearchFeedbackDisabled Result = "search_feedback_disabled"
)

// RecordLatency bumps the per-result-class request counter AND
// observes the request duration into the rolling latency window.
// Called per /search request from the HTTP handler — real request
// timings feed the p50/p95/p99 percentiles on /admin/search/health.
//
// For event-only notifications (saved-search coordinator ticks,
// feedback submissions, similar) that don't measure a real duration,
// use RecordEvent instead — otherwise the zero-value observations
// pollute the latency histogram.
func (c *Counter) RecordLatency(r Result, latency time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests[string(r)]++
	if len(c.latencies) >= c.windowCap {
		// slide the window: drop the oldest 25% so we don't
		// re-alloc on every append. Cheap because the window is
		// small (5k default).
		drop := c.windowCap / 4
		c.latencies = append(c.latencies[:0], c.latencies[drop:]...)
	}
	c.latencies = append(c.latencies, latency)
}

// RecordEvent bumps the per-result-class request counter WITHOUT
// touching the latency window. Callers that emit non-latency events
// (saved-search coordinator ticks, feedback events, etc.) use this
// so their zero-duration signal doesn't drag the p50/p95/p99
// histogram downward.
//
// Both methods increment the same requests[class] map — dashboard
// tiles that group by result class see both event + latency callers
// contribute identically.
func (c *Counter) RecordEvent(r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests[string(r)]++
}

// counterSnapshot implements healthhandler.Counter — the interface
// the shim expects. Kept on a private type so the exported Counter
// isn't tied to the shim's shape.
type counterSnapshot struct {
	c *Counter
}

// Snapshot returns the SubsystemHealth payload for the health
// handler. Thread-safe: takes the counter's mutex once and returns
// a snapshot value.
func (s counterSnapshot) Snapshot() healthhandler.SubsystemHealth {
	s.c.mu.Lock()
	requests := make(map[string]int64, len(s.c.requests))
	total := int64(0)
	for k, v := range s.c.requests {
		requests[k] = v
		total += v
	}
	lats := make([]time.Duration, len(s.c.latencies))
	copy(lats, s.c.latencies)
	cacheProvider := s.c.cacheStats
	gaugeProvider := s.c.gaugeStats
	started := s.c.startedAt
	s.c.mu.Unlock()

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	p50 := pct(lats, 0.50)
	p95 := pct(lats, 0.95)
	p99 := pct(lats, 0.99)

	notes := []string{
		"latency_p50_ms=" + itoaMillis(p50),
		"latency_p95_ms=" + itoaMillis(p95),
		"latency_p99_ms=" + itoaMillis(p99),
		"sample_count=" + itoa(int64(len(lats))),
		"uptime_seconds=" + itoa(int64(time.Since(started).Seconds())),
	}
	if cacheProvider != nil {
		cs := cacheProvider()
		notes = append(notes,
			"cache_entries="+itoa(int64(cs.Entries)),
			"cache_hits="+itoa(cs.Hits),
			"cache_misses="+itoa(cs.Misses),
			"cache_invalidations="+itoa(cs.Invalidations),
		)
	}
	// Phase 1.16.B-5 — pg_stat-backed gauges. Deterministic key
	// order (sorted) so the JSON diff stays stable across snapshot
	// polls; the admin dashboard renders each key as its own tile.
	if gaugeProvider != nil {
		gauges := gaugeProvider()
		keys := make([]string, 0, len(gauges))
		for k := range gauges {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			notes = append(notes, k+"="+itoa(gauges[k]))
		}
	}

	return healthhandler.SubsystemHealth{
		Subsystem:    "search",
		CounterTotal: total,
		ByResult:     requests,
		Notes:        notes,
	}
}

// AsSnapshot returns the counter wrapped in the healthhandler
// Counter interface. Boot passes the result to
// healthhandler.HandlerFor("search", counter, "system.admin").
func (c *Counter) AsSnapshot() healthhandler.Counter {
	return counterSnapshot{c: c}
}

// pct returns the P-percentile latency from a sorted slice. Returns
// 0 for an empty slice; interpolates between adjacent samples
// for smoother output on small windows.
func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lo)
	return sorted[lo] + time.Duration(float64(sorted[hi]-sorted[lo])*frac)
}

// keep atomic imported for future counter additions — some fields
// will migrate off the mutex once contention shows up in load
// testing. Explicit no-op so the linter doesn't cull the import.
var _ atomic.Int64

func itoa(n int64) string { return strconvFormatInt(n) }
func itoaMillis(d time.Duration) string { return strconvFormatInt(d.Milliseconds()) }
