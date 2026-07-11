// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"testing"
	"time"
)

// latencySample count is the load-bearing invariant these tests
// protect. RecordLatency observes into the rolling window; RecordEvent
// increments only the request counter map. Prior to Phase
// 1.16.B-followup-3, both were the same Record() method — event
// callers were forced to pass duration=0 which polluted p50/p95/p99.
// Splitting the surface + these assertions prevent future regression.

func TestCounter_RecordLatency_IncrementsBothByResultAndLatency(t *testing.T) {
	c := NewCounter(0) // default window cap
	c.RecordLatency(ResultHit, 100*time.Millisecond)
	c.RecordLatency(ResultHit, 200*time.Millisecond)
	c.RecordLatency(ResultEmpty, 50*time.Millisecond)

	snap := c.AsSnapshot().Snapshot()
	if got := snap.ByResult["hit"]; got != 2 {
		t.Fatalf("by_result[hit] = %d; want 2", got)
	}
	if got := snap.ByResult["empty"]; got != 1 {
		t.Fatalf("by_result[empty] = %d; want 1", got)
	}
	if got := snap.CounterTotal; got != 3 {
		t.Fatalf("counter_total = %d; want 3", got)
	}
	// Latency window must have exactly 3 observations. The percentiles
	// end up in Notes as strings; check the raw slice via reflection
	// through the counter's internal state.
	c.mu.Lock()
	got := len(c.latencies)
	c.mu.Unlock()
	if got != 3 {
		t.Fatalf("latencies length = %d; want 3", got)
	}
}

func TestCounter_RecordEvent_IncrementsByResultOnly_NoLatencyObservation(t *testing.T) {
	c := NewCounter(0)
	c.RecordEvent(ResultSavedSearchCoordinatorTick)
	c.RecordEvent(ResultSearchFeedbackUp)
	c.RecordEvent(ResultSearchFeedbackUp)

	snap := c.AsSnapshot().Snapshot()
	if got := snap.ByResult["saved_search_coordinator_tick"]; got != 1 {
		t.Fatalf("by_result[saved_search_coordinator_tick] = %d; want 1", got)
	}
	if got := snap.ByResult["search_feedback_up"]; got != 2 {
		t.Fatalf("by_result[search_feedback_up] = %d; want 2", got)
	}
	if got := snap.CounterTotal; got != 3 {
		t.Fatalf("counter_total = %d; want 3", got)
	}
	// Load-bearing: latency window MUST stay empty when only events
	// were recorded. Pre-split, these three calls would have added
	// three 0-value observations dragging p50 to 0ms.
	c.mu.Lock()
	got := len(c.latencies)
	c.mu.Unlock()
	if got != 0 {
		t.Fatalf("latencies length after events-only = %d; want 0 (events must NOT touch the latency window)", got)
	}
}

// TestCounter_MixedEventAndLatency_LatencyWindowUnaffectedByEvents —
// end-to-end guarantee that events + latency-observations coexist in
// the same requests[] map but the latency window only holds real
// timings.
func TestCounter_MixedEventAndLatency_LatencyWindowUnaffectedByEvents(t *testing.T) {
	c := NewCounter(0)
	c.RecordLatency(ResultHit, 100*time.Millisecond)
	c.RecordEvent(ResultSearchFeedbackUp)
	c.RecordEvent(ResultSearchFeedbackDown)
	c.RecordLatency(ResultHit, 200*time.Millisecond)

	c.mu.Lock()
	latencyCount := len(c.latencies)
	c.mu.Unlock()
	if latencyCount != 2 {
		t.Fatalf("latency window should hold only the 2 RecordLatency samples; got %d", latencyCount)
	}
	snap := c.AsSnapshot().Snapshot()
	if snap.CounterTotal != 4 {
		t.Fatalf("counter_total = %d; want 4 (both event + latency callers contribute)", snap.CounterTotal)
	}
}
