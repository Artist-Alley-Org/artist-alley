// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visualembed

import "sync/atomic"

// Counter is the observability surface for the async visualembed job.
// Owned by the visualembed package (not the shared search.Counter) so
// embed durations don't pollute the search-query p50/p95/p99 window.
// The surrounding search health JSON reads a Snapshot() at gauge time
// and appends the counters to the notes[] array.
//
// Zero value is usable — nil-safe on the pending-gauge accessor too.
type Counter struct {
	success      atomic.Int64
	transient    atomic.Int64
	permanent    atomic.Int64
	rateLimited  atomic.Int64
	skipped      atomic.Int64
	pending      atomic.Int64
}

// NewCounter constructs a Counter with all counters zeroed.
func NewCounter() *Counter { return &Counter{} }

// RecordSuccess bumps the success counter. Called by Job.Handle when
// the sidecar returned an embedding + the upsert landed.
func (c *Counter) RecordSuccess() {
	if c == nil {
		return
	}
	c.success.Add(1)
}

// RecordTransientFailed bumps the transient-fail counter. Called by
// Job.Handle when the sidecar was unreachable or the rate-limit wait
// timed out — the jobs framework will re-queue per MaxAttempts.
func (c *Counter) RecordTransientFailed() {
	if c == nil {
		return
	}
	c.transient.Add(1)
}

// RecordPermanentFailed bumps the permanent-fail counter. Called by
// Job.Handle on decode errors, dim mismatches, or missing asset bytes
// — situations where a retry won't help.
func (c *Counter) RecordPermanentFailed() {
	if c == nil {
		return
	}
	c.permanent.Add(1)
}

// RecordRateLimitedWait bumps the rate-limit wait counter. Called by
// Job.Handle when the shared limiter blocked but eventually released
// within the wait budget. Distinct from RecordTransientFailed because
// a rate-limited wait is a HEALTHY throttle, not a failure.
func (c *Counter) RecordRateLimitedWait() {
	if c == nil {
		return
	}
	c.rateLimited.Add(1)
}

// RecordSkipped bumps the skip counter. Called by dispatch when the
// asset didn't meet the guard conditions (non-image, provider nil,
// auto-embed disabled). Silent bypass — no error surfaces.
func (c *Counter) RecordSkipped() {
	if c == nil {
		return
	}
	c.skipped.Add(1)
}

// StartPending bumps the in-flight gauge. Paired with EndPending in
// Job.Handle via defer so both success + failure decrement the gauge.
func (c *Counter) StartPending() {
	if c == nil {
		return
	}
	c.pending.Add(1)
}

// EndPending decrements the in-flight gauge.
func (c *Counter) EndPending() {
	if c == nil {
		return
	}
	c.pending.Add(-1)
}

// Snapshot returns the current counter values as a map suitable for
// appending to the /admin/search/health notes[] gauge surface. Reads
// are atomic; the returned map is a point-in-time snapshot (not
// synchronized across counters — an event bumping counter A while we
// read counter B is a normal race, and the JSON is already eventually
// consistent from the client's perspective).
func (c *Counter) Snapshot() map[string]int64 {
	if c == nil {
		return nil
	}
	return map[string]int64{
		"visual_embed_auto_success":            c.success.Load(),
		"visual_embed_auto_transient_failed":   c.transient.Load(),
		"visual_embed_auto_permanent_failed":   c.permanent.Load(),
		"visual_embed_auto_rate_limited_wait":  c.rateLimited.Load(),
		"visual_embed_auto_skipped":            c.skipped.Load(),
		"visual_embed_auto_pending":            c.pending.Load(),
	}
}
