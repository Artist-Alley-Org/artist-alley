package iiif

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/iiif/presentation"
	"github.com/mscrnt/artist-alley/app/internal/observability/healthhandler"
)

// HealthCounter aggregates observability signals across every IIIF
// subsystem (Presentation manifests, Content Search 2.0, Image API
// 3.0, 2.0→3.0 redirects, federated canvas resolution) into a
// single /admin/iiif/health snapshot. Phase 1.54.B.
//
// One counter, four recorder surfaces:
//
//   - presentation.Counter — RecordManifestRequest / cache hit /
//     cache miss / federated canvas
//   - content_search.Counter — RecordContentSearch
//   - redirect.Counter — RecordLegacyRewrite
//   - healthhandler.Counter — Snapshot() for the shared shim
//
// Latency histograms roll on a fixed-capacity window (5000 samples
// default; ~10 min at 8 req/s). Percentiles are computed on
// snapshot rather than continuously so the hot path stays lock-
// contention-free.
type HealthCounter struct {
	mu sync.Mutex

	// Flat counter map keyed by "family/subkey" strings so ByResult
	// can be serialised directly. Keys used:
	//   manifest_requests/asset/200          — manifest served, 200
	//   manifest_requests/collection/404     — collection not found
	//   manifest_cache/hit
	//   manifest_cache/miss
	//   federated_canvas/served
	//   content_search/asset                 — asset-scope query
	//   content_search/collection            — collection-scope query
	//   redirect_2to3/manifest               — /iiif/2/{id}/manifest → 301
	//   redirect_2to3/info                   — /iiif/2/{id}/info.json → 301
	//   redirect_2to3/image                  — tile request → 301
	counters map[string]int64

	// Manifest generation latencies (all statuses; rolling window).
	manifestLatencies []time.Duration

	// Content Search 2.0 latencies (rolling window).
	contentSearchLatencies []time.Duration

	windowCap int
	startedAt time.Time
}

// NewHealthCounter constructs a HealthCounter with the requested
// per-histogram rolling capacity. Zero defaults to 5000 samples.
func NewHealthCounter(windowCap int) *HealthCounter {
	if windowCap <= 0 {
		windowCap = 5000
	}
	return &HealthCounter{
		counters:               map[string]int64{},
		manifestLatencies:      make([]time.Duration, 0, windowCap),
		contentSearchLatencies: make([]time.Duration, 0, windowCap),
		windowCap:              windowCap,
		startedAt:              time.Now(),
	}
}

// ---------------- presentation.Counter surface ------------------

// RecordManifestRequest is called once per manifest generation.
func (c *HealthCounter) RecordManifestRequest(kind presentation.EntityType, status int, latency time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counters["manifest_requests/"+string(kind)+"/"+strconv.Itoa(status)]++
	if status < 500 {
		// Only successful + client-error latencies feed the
		// histogram. 500s are usually pool exhaustion / peer
		// timeouts and skew the p99 way past the interesting range.
		c.appendManifestLatency(latency)
	}
}

// RecordManifestCacheHit / Miss track the ManifestCache effectiveness.
func (c *HealthCounter) RecordManifestCacheHit() {
	c.mu.Lock()
	c.counters["manifest_cache/hit"]++
	c.mu.Unlock()
}

func (c *HealthCounter) RecordManifestCacheMiss() {
	c.mu.Lock()
	c.counters["manifest_cache/miss"]++
	c.mu.Unlock()
}

// RecordFederatedCanvas fires each time a canvas / image URL was
// resolved against a peer directory. A count near zero means either
// no federated assets in circulation OR the resolver is failing
// silently — the Notes[] "resolver falls back to empty" line points
// operators at the second case.
func (c *HealthCounter) RecordFederatedCanvas() {
	c.mu.Lock()
	c.counters["federated_canvas/served"]++
	c.mu.Unlock()
}

// ---------------- content_search.Counter surface ----------------

// RecordContentSearch is called once per Content Search 2.0 request.
// hitCount is surfaced as a note (avg hits per query) so operators
// can spot "always zero" — usually a stale index.
func (c *HealthCounter) RecordContentSearch(scope string, hitCount int, latency time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counters["content_search/"+scope]++
	c.counters["content_search_hits/"+scope] += int64(hitCount)
	c.appendContentSearchLatency(latency)
}

// ---------------- redirect.Counter surface ----------------------

// RecordLegacyRewrite is called once per /iiif/2/... 301. Split by
// kind (manifest / info / image) so operators see which legacy
// grammar is still in use — helps decide when it's safe to drop.
func (c *HealthCounter) RecordLegacyRewrite(kind string) {
	c.mu.Lock()
	c.counters["redirect_2to3/"+kind]++
	c.mu.Unlock()
}

// ---------------- healthhandler.Counter surface -----------------

// Snapshot renders the SubsystemHealth payload the shared shim
// serialises. Follows the B-5 dashboard subsystem-card shape:
// ByResult carries the counters map; Notes[] carries derived stats
// + operator hints. Deterministic key order via sort.
func (c *HealthCounter) Snapshot() healthhandler.SubsystemHealth {
	c.mu.Lock()
	counters := make(map[string]int64, len(c.counters))
	total := int64(0)
	for k, v := range c.counters {
		counters[k] = v
		// Only manifest+content-search+redirect requests count
		// toward the top-line total. Cache hit/miss and federated-
		// canvas events are per-request telemetry, not their own
		// request class.
		if strings.HasPrefix(k, "manifest_requests/") ||
			(strings.HasPrefix(k, "content_search/") && !strings.HasPrefix(k, "content_search_hits/")) ||
			strings.HasPrefix(k, "redirect_2to3/") {
			total += v
		}
	}
	mLats := make([]time.Duration, len(c.manifestLatencies))
	copy(mLats, c.manifestLatencies)
	csLats := make([]time.Duration, len(c.contentSearchLatencies))
	copy(csLats, c.contentSearchLatencies)
	started := c.startedAt
	c.mu.Unlock()

	sort.Slice(mLats, func(i, j int) bool { return mLats[i] < mLats[j] })
	sort.Slice(csLats, func(i, j int) bool { return csLats[i] < csLats[j] })

	notes := []string{
		"manifest_latency_p50_ms=" + itoaMillis(pct(mLats, 0.50)),
		"manifest_latency_p95_ms=" + itoaMillis(pct(mLats, 0.95)),
		"manifest_latency_p99_ms=" + itoaMillis(pct(mLats, 0.99)),
		"content_search_latency_p50_ms=" + itoaMillis(pct(csLats, 0.50)),
		"content_search_latency_p95_ms=" + itoaMillis(pct(csLats, 0.95)),
		"content_search_latency_p99_ms=" + itoaMillis(pct(csLats, 0.99)),
		"manifest_sample_count=" + strconv.Itoa(len(mLats)),
		"content_search_sample_count=" + strconv.Itoa(len(csLats)),
		"uptime_seconds=" + strconv.FormatInt(int64(time.Since(started).Seconds()), 10),
		// Cache-hit ratio: dashboard renders this as-is; NaN safe
		// (zero denom → 0).
		"cache_hit_ratio=" + ratio(counters["manifest_cache/hit"], counters["manifest_cache/hit"]+counters["manifest_cache/miss"]),
		// Ephemeral operator-facing hints per the B-5 Notes[]
		// convention. Dashboard renders each as a bullet.
		"note=Multi-page PDF surfaced as metadata-only until per-page Image API grammar lands (see follow-up)",
		"note=Anonymous callers gated at IIIF layer; consolidation with visibility.Filter tracked in #185",
		"note=Federation peer resolver falls back to empty-string on directory failure — degraded remote canvas, not 500",
	}

	return healthhandler.SubsystemHealth{
		Subsystem:    "iiif",
		CounterTotal: total,
		ByResult:     counters,
		Notes:        notes,
	}
}

// appendManifestLatency slides the window when full.
func (c *HealthCounter) appendManifestLatency(d time.Duration) {
	if len(c.manifestLatencies) >= c.windowCap {
		drop := c.windowCap / 4
		c.manifestLatencies = append(c.manifestLatencies[:0], c.manifestLatencies[drop:]...)
	}
	c.manifestLatencies = append(c.manifestLatencies, d)
}

// appendContentSearchLatency slides the window when full.
func (c *HealthCounter) appendContentSearchLatency(d time.Duration) {
	if len(c.contentSearchLatencies) >= c.windowCap {
		drop := c.windowCap / 4
		c.contentSearchLatencies = append(c.contentSearchLatencies[:0], c.contentSearchLatencies[drop:]...)
	}
	c.contentSearchLatencies = append(c.contentSearchLatencies, d)
}

// pct returns the P-percentile from a sorted latency slice.
// Interpolates between adjacent samples for smoother output on
// small windows. Empty slice → 0.
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

func itoaMillis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// ratio formats hit/total as a "0.xxx" string. Zero denom → "0.000".
func ratio(num, denom int64) string {
	if denom == 0 {
		return "0.000"
	}
	return strconv.FormatFloat(float64(num)/float64(denom), 'f', 3, 64)
}

// Silence sync/atomic import until the mutex hot path is
// refactored to atomics on load-test signal. Explicit no-op keeps
// the import visible so future edits don't lose the reservation.
var _ atomic.Int64
