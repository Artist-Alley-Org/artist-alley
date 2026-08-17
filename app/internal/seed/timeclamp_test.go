// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Pure (no-DB) unit tests for the generation-time clamp (#1174).
//
// The site datasets carry created_at/updated_at scattered around their
// generation date in BOTH directions, so a fresh seed used to write rows
// dated months into the future — 36 posts and 155 assets out to
// 2026-12-14 — which pin the head of every Newest sort until the date
// arrives. These tests pin the three properties the fix rests on: no row
// timestamp exceeds the generation instant, past dates are untouched (so
// the corpus keeps its dataset dates and stays reproducible), and the
// derived social-history span inherits the clamp.

package seed

import (
	"testing"
	"time"
)

var clampNow = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func TestClampToPast_LeavesPastAndPresentUntouched(t *testing.T) {
	cases := []time.Time{
		clampNow.AddDate(-2, 0, 0),
		clampNow.AddDate(0, -4, 0),
		clampNow.Add(-time.Second),
		clampNow, // the boundary is inclusive: "≤ now" is not future
	}
	for _, in := range cases {
		if got := clampToPast(in, clampNow); !got.Equal(in) {
			t.Errorf("clampToPast(%s) = %s, want it unchanged", in, got)
		}
	}
}

func TestClampToPast_ReflectsFutureIntoThePast(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
	}{
		{"one second ahead", clampNow.Add(time.Second)},
		{"a day ahead", clampNow.AddDate(0, 0, 1)},
		{"the dataset's worst case, ~4 months ahead", time.Date(2026, 12, 14, 11, 12, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := clampToPast(c.in, clampNow)
		if got.After(clampNow) {
			t.Errorf("%s: clampToPast(%s) = %s, still in the future", c.name, c.in, got)
		}
		// Reflection, not pinning: the result must be strictly before
		// now, so a run's future rows spread across the corpus instead
		// of collapsing onto one instant at the head of the feed.
		if !got.Before(clampNow) {
			t.Errorf("%s: clampToPast(%s) = %s, want strictly before %s", c.name, c.in, got, clampNow)
		}
		if want := clampNow.Sub(got); want != c.in.Sub(clampNow) {
			t.Errorf("%s: overshoot %s reflected to %s, want them equal", c.name, c.in.Sub(clampNow), want)
		}
	}
}

func TestRowTimes_ClampsAndKeepsUpdatedAtOrAfterCreated(t *testing.T) {
	r := &Runner{genTime: clampNow}

	// Both future. Reflection reverses their order, so the raw pair
	// would come out with updated_at BEFORE created_at — a row updated
	// before it existed. rowTimes has to restore the invariant.
	created, updated := r.rowTimes("2026-12-11T16:05:00Z", "2026-12-14T11:12:00Z")
	if !created.Valid || !updated.Valid {
		t.Fatalf("both timestamps should parse: created=%v updated=%v", created, updated)
	}
	if created.Time.After(clampNow) || updated.Time.After(clampNow) {
		t.Errorf("clamped pair still future: created=%s updated=%s", created.Time, updated.Time)
	}
	if updated.Time.Before(created.Time) {
		t.Errorf("updated %s is before created %s", updated.Time, created.Time)
	}

	// A past pair rides through byte-identical — the reproducibility
	// property the seed's deterministic history depends on.
	created, updated = r.rowTimes("2025-01-02T21:37:00Z", "2025-05-22T03:06:53Z")
	if got := created.Time.Format(time.RFC3339); got != "2025-01-02T21:37:00Z" {
		t.Errorf("past created_at rewritten to %s", got)
	}
	if got := updated.Time.Format(time.RFC3339); got != "2025-05-22T03:06:53Z" {
		t.Errorf("past updated_at rewritten to %s", got)
	}

	// A missing updated_at still falls back to created_at.
	created, updated = r.rowTimes("2025-01-02T21:37:00Z", "")
	if !updated.Valid || !updated.Time.Equal(created.Time) {
		t.Errorf("absent updated_at = %v, want the created_at %v", updated, created)
	}
}

func TestRowTime_InvalidStaysInvalid(t *testing.T) {
	r := &Runner{genTime: clampNow}
	for _, s := range []string{"", "not-a-time", "2026-08-16"} {
		if ts := r.rowTime(s); ts.Valid {
			t.Errorf("rowTime(%q) = %v, want invalid", s, ts)
		}
	}
}

func TestContentSpan_NeverEndsInTheFuture(t *testing.T) {
	r := &Runner{genTime: clampNow}
	cat := &catalogues{Posts: []manifestPost{
		{ID: "a", CreatedAt: "2025-01-02T21:37:00Z"},
		{ID: "b", CreatedAt: "2026-03-04T05:06:07Z"},
		{ID: "c", CreatedAt: "2026-12-11T16:05:00Z"}, // the future one
	}}
	lo, hi := r.contentSpan(cat)
	if hi.After(clampNow) {
		t.Fatalf("span hi = %s, after the generation instant %s", hi, clampNow)
	}
	if lo.After(hi) {
		t.Fatalf("span lo %s after hi %s", lo, hi)
	}
	// Every derived follow/like/comment instant is drawn from this span,
	// so a past-bounded span is what keeps the whole social graph out of
	// the future.
	for _, parts := range [][]string{{"follow", "u1", "u2"}, {"likeat", "c", "u3"}, {"cmtat", "b", "u4"}} {
		if ts := stableTimeBetween(lo, hi, parts...); ts.After(clampNow) {
			t.Errorf("derived instant %v = %s, after %s", parts, ts, clampNow)
		}
	}
}

func TestNewRunner_GenTimeIsSet(t *testing.T) {
	// The clamp is only load-bearing if the constructor stamps it; a
	// zero genTime would push EVERY dataset date into the far past.
	before := time.Now().UTC()
	r := NewRunner(nil, nil, Options{})
	after := time.Now().UTC()
	if r.genTime.Before(before) || r.genTime.After(after) {
		t.Fatalf("genTime = %s, want it inside [%s, %s]", r.genTime, before, after)
	}
}
