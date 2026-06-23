package metadata

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/observability/healthhandler"
)

// ExtractionResult names the per-event outcome the counter
// records. Stable enum — the admin UI graphs against these
// strings.
type ExtractionResult string

const (
	ResultSuccess           ExtractionResult = "success"
	ResultNoMetadata        ExtractionResult = "no_metadata"
	ResultUnsupportedFormat ExtractionResult = "unsupported_format"
	ResultMalformedFile     ExtractionResult = "malformed_file"
	ResultLibraryError      ExtractionResult = "library_error"
	ResultValidationFailed  ExtractionResult = "validation_failed"
)

// Counter is the per-process extraction-event counter. Atomic
// counters per (format, result) pair + per-result aggregates;
// last_success / last_failure timestamps under one mutex (rare
// path; cheap).
//
// Implements [healthhandler.Counter]. One per process; wired
// alongside the ExtractJobHandler in the boot stage.
type Counter struct {
	total atomic.Int64

	mu          sync.RWMutex
	byFormat    map[string]int64
	byResult    map[string]int64
	lastSuccess time.Time
	lastFailure time.Time
}

// NewCounter constructs an empty counter.
func NewCounter() *Counter {
	return &Counter{
		byFormat: map[string]int64{},
		byResult: map[string]int64{},
	}
}

// Record bumps the counters for a single extraction outcome.
// Called from the job handler after every Extract + Apply pass.
// format may be empty for whole-file failures (e.g., missing
// MIME); result is required.
func (c *Counter) Record(format string, result ExtractionResult, at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	c.total.Add(1)
	c.mu.Lock()
	if format != "" {
		c.byFormat[format]++
	}
	c.byResult[string(result)]++
	switch result {
	case ResultSuccess, ResultNoMetadata:
		// no_metadata is a normal outcome — operators don't want
		// it screaming as a failure but also don't want it
		// hiding successes. Counted as success-for-recency.
		c.lastSuccess = at
	default:
		c.lastFailure = at
	}
	c.mu.Unlock()
}

// Snapshot implements [healthhandler.Counter]. Returns a deep
// copy so concurrent updates after Snapshot don't mutate the
// returned struct's maps.
func (c *Counter) Snapshot() healthhandler.SubsystemHealth {
	c.mu.RLock()
	defer c.mu.RUnlock()

	byFormat := make(map[string]int64, len(c.byFormat))
	for k, v := range c.byFormat {
		byFormat[k] = v
	}
	byResult := make(map[string]int64, len(c.byResult))
	for k, v := range c.byResult {
		byResult[k] = v
	}

	out := healthhandler.SubsystemHealth{
		CounterTotal: c.total.Load(),
		ByFormat:     byFormat,
		ByResult:     byResult,
	}
	if !c.lastSuccess.IsZero() {
		t := c.lastSuccess
		out.LastSuccess = &t
	}
	if !c.lastFailure.IsZero() {
		t := c.lastFailure
		out.LastFailure = &t
	}
	return out
}

// Compile-time assertion.
var _ healthhandler.Counter = (*Counter)(nil)
