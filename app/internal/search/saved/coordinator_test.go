// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #292 — the coordinator must reschedule onto an interval-aligned grid
// so concurrent / restart-spawned coordinators converge on one pending
// row (via a shared idempotency key) instead of stacking into a hot
// loop. These are pure-time tests of that alignment.
package saved_test

import (
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/search/saved"
)

func TestNextCoordinatorTick_AlignsToGrid(t *testing.T) {
	// 60s grid. Any instant inside the 12:00:00–12:00:59 window must
	// schedule the next tick at 12:01:00 — never now()+60.
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{0, 200 * time.Millisecond, 15 * time.Second, 59*time.Second + 900*time.Millisecond} {
		got := saved.NextCoordinatorTick(base.Add(offset), 60)
		want := base.Add(time.Minute)
		if !got.Equal(want) {
			t.Errorf("NextCoordinatorTick(base+%s) = %s, want %s", offset, got.Format(time.RFC3339Nano), want.Format(time.RFC3339))
		}
	}
}

// Two coordinators that reschedule at slightly different sub-second
// moments in the same window must produce the SAME idempotency key —
// that shared key is what dedups them to one pending row. With the old
// now()+60 scheme these drifted into different seconds and accumulated.
func TestCoordinatorTickKey_ConvergesWithinWindow(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	a := saved.CoordinatorTickKey(saved.NextCoordinatorTick(base.Add(300*time.Millisecond), 60))
	b := saved.CoordinatorTickKey(saved.NextCoordinatorTick(base.Add(1200*time.Millisecond), 60))
	if a != b {
		t.Fatalf("keys diverged within the same window: %q vs %q", a, b)
	}
	// The next window must produce a different key (still ticks forward).
	c := saved.CoordinatorTickKey(saved.NextCoordinatorTick(base.Add(61*time.Second), 60))
	if c == a {
		t.Fatalf("next-window key must differ, both were %q", a)
	}
}

func TestNextCoordinatorTick_ZeroWakeUsesDefault(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 30, 0, time.UTC)
	got := saved.NextCoordinatorTick(base, 0)
	// Default cadence is 60s → next :00 boundary is 12:01:00.
	want := time.Date(2026, 7, 15, 12, 1, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("zero wake: got %s, want %s (default %ds cadence)", got.Format(time.RFC3339), want.Format(time.RFC3339), saved.DefaultCoordinatorWakeSeconds)
	}
}
