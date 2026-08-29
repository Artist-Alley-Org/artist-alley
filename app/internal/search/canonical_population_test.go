// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1368 — the round trip, asserted against REAL ROWS.
//
// canonical_test.go proves the representation survives: the same set of
// (dimension, value) terms comes back. That is the whole of the claim
// only if a Selection is the only thing the Engine reads, which it is —
// but "the Selection is identical, therefore the population is" is an
// inference, and the acceptance this sprint was written against asks for
// the population itself.
//
// So every case here runs the SAME query twice against the same corpus,
// once with the selection the rail produced and once with the selection
// recovered from its canonical DSL, and asserts the hit ID SETS are
// equal. ⛔ Set equality rather than a count: a count comparison is
// satisfied by two different result sets of the same size, which on an
// OR dimension is exactly the near-miss a collapsing compiler produces.
//
// ⛔ NOT A FAIL-BEFORE TEST — see canonical_test.go's header.
package search

import (
	"context"
	"sort"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// throughDSL is roundTrip's population-side twin: it returns the
// selection recovered from the canonical DSL, and the DSL itself for
// failure messages.
func throughDSL(t *testing.T, s facet.Selection) (facet.Selection, string) {
	t.Helper()
	return roundTrip(t, s)
}

func hitIDs(res QueryResult) []string {
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.ID.String())
	}
	sort.Strings(out)
	return out
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRoundTrip_SamePopulation_RailDimensions drives the five rail
// dimensions over the #907 corpus, whose buckets deliberately hold
// DIFFERENT numbers of rows so a filter that quietly stopped applying
// cannot pass.
func TestRoundTrip_SamePopulation_RailDimensions(t *testing.T) {
	pool := coPool(t)
	ffSeed(t, pool)
	e := NewEngine(pool)
	ref := ffOwner

	// The premise, stated on the wire before anything is compared: the
	// unfiltered corpus is five rows, so every "same population" below is
	// a claim about a set that a dropped filter would visibly widen.
	if n := len(ffEngineRun(t, e, &ref, facet.Selection{}).Hits); n != 5 {
		t.Fatalf("unfiltered search returned %d hits, want 5 — the fixture is wrong "+
			"and every assertion below is meaningless", n)
	}

	cases := []struct {
		name string
		raw  []string
		want int
	}{
		{"extension", []string{"extension:png"}, 3},
		{"tag", []string{"tag:sketch"}, 3},
		{"owner", []string{"owner:ff-owner-907"}, 5},
		{"sensitivity", []string{"sensitivity:public"}, 5},
		// ⭐ N>=2 ON AN OR DIMENSION, and the arithmetic is the point:
		// png alone is 3, jpg alone is 1, and both together is 4 —
		// strictly MORE than max(3, 1), which is what an OR looks like.
		// The pre-#1368 compiler kept only the last term and would have
		// answered 1 here.
		{"two extensions OR", []string{"extension:png", "extension:jpg"}, 4},
		// ⭐ N>=2 ON THE CONJUNCTIVE DIMENSION: sketch is 3, wip is 2,
		// both is 1 — strictly FEWER than min(3, 2), which is an AND.
		{"two tags AND", []string{"tag:sketch", "tag:wip"}, 1},
		{"two dimensions AND", []string{"extension:png", "tag:wip"}, 2},
		{"a value nothing carries", []string{"extension:tiff"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			direct, err := facet.ParseSelection(c.raw)
			if err != nil {
				t.Fatalf("parse %v: %v", c.raw, err)
			}
			fromDSL, text := throughDSL(t, direct)

			a := ffEngineRun(t, e, &ref, direct)
			b := ffEngineRun(t, e, &ref, fromDSL)

			if len(a.Hits) != c.want {
				t.Fatalf("the rail's own selection %v returned %d hits, want %d — the "+
					"fixture moved and the comparison below would be vacuous",
					c.raw, len(a.Hits), c.want)
			}
			if !sameIDs(hitIDs(a), hitIDs(b)) {
				t.Errorf("the canonical DSL returned a DIFFERENT population\n"+
					"  filters %v\n  dsl     %s\n  rail    %v\n  replay  %v",
					c.raw, text, hitIDs(a), hitIDs(b))
			}
			if b.TotalCount != a.TotalCount {
				t.Errorf("total_count %d via the rail, %d via %s", a.TotalCount, b.TotalCount, text)
			}
		})
	}
}

// TestRoundTrip_SamePopulation_FieldDimension is the same claim for the
// dimension the DSL could not name at all before this sprint, over the
// #1157/#1165 corpus.
//
// It covers all three operator shapes plus the two N>=2 groupings the
// 2026-08-20 ADR amendment defines: same code + same operator ORs, and
// the two ends of a date range AND.
func TestRoundTrip_SamePopulation_FieldDimension(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)
	ref := fdOwner
	e := NewEngine(pool)

	// Everything below is an OPEN field, so an all-refusing checker is
	// the honest caller here. The gated arm has its own test.
	caps := visibility.CapabilityChecker(func(string) bool { return false })

	run := func(s facet.Selection) QueryResult {
		t.Helper()
		res, err := e.Run(context.Background(), Query{
			Text:          fdPhrase,
			Types:         []HitType{HitTypeAsset},
			Limit:         50,
			Filters:       s,
			CallerUserRef: &ref,
			CapChecker:    caps,
		})
		if err != nil {
			t.Fatalf("engine: %v", err)
		}
		return res
	}

	cases := []struct {
		name string
		raw  []string
		want int
	}{
		{"equality across both storage columns", []string{"field:" + fdOpenCode + "=Locked"}, 2},
		{"contains, case-insensitively", []string{"field:" + fdNotesCode + "~review"}, 1},
		{"a lower bound alone", []string{"field:" + fdDateCode + ">=2026-04-01"}, 2},
		{"an upper bound alone", []string{"field:" + fdDateCode + "<=2026-08-01"}, 2},
		// ⭐ THE TWO BOUNDS AND: each alone admits 2, together 1. Losing
		// either bound in the round trip would answer 2, and losing both
		// would answer 3 — every failure is a different number from the
		// right one, which is what makes this case a discriminator.
		{"a closed range ANDs its ends", []string{
			"field:" + fdDateCode + ">=2026-04-01",
			"field:" + fdDateCode + "<=2026-08-01",
		}, 1},
		// Different codes AND.
		{"two field codes narrow", []string{
			"field:" + fdOpenCode + "=Locked",
			"field:" + fdNotesCode + "~review",
		}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			direct, err := facet.ParseSelection(c.raw)
			if err != nil {
				t.Fatalf("parse %v: %v", c.raw, err)
			}
			fromDSL, text := throughDSL(t, direct)

			a := run(direct)
			b := run(fromDSL)

			if len(a.Hits) != c.want {
				t.Fatalf("the rail's own selection %v returned %d hits, want %d — the "+
					"fixture moved and the comparison below would be vacuous",
					c.raw, len(a.Hits), c.want)
			}
			if !sameIDs(hitIDs(a), hitIDs(b)) {
				t.Errorf("the canonical DSL returned a DIFFERENT population\n"+
					"  filters %v\n  dsl     %s\n  rail    %v\n  replay  %v",
					c.raw, text, hitIDs(a), hitIDs(b))
			}
		})
	}
}

// TestRoundTrip_SamePopulation_GatedFieldStaysRefused.
//
// ⛔ THE ROUND TRIP MUST NOT BE A WAY IN. `field:` reaching the DSL is
// new surface, and the capability gate it has to pass
// (facet.Selection.Authorize) is keyed on the selection rather than on
// how the selection was spelled. A caller without the capability gets an
// empty page for a gated field, and must get the same empty page when
// the same term arrives through canonical DSL.
func TestRoundTrip_SamePopulation_GatedFieldStaysRefused(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)
	ref := fdOwner
	e := NewEngine(pool)

	term := "field:" + fdGatedCode + "=Secret"
	direct, err := facet.ParseSelection([]string{term})
	if err != nil {
		t.Fatal(err)
	}
	fromDSL, text := throughDSL(t, direct)

	run := func(s facet.Selection, caps visibility.CapabilityChecker) int {
		res, err := e.Run(context.Background(), Query{
			Text:          fdPhrase,
			Types:         []HitType{HitTypeAsset},
			Limit:         50,
			Filters:       s,
			CallerUserRef: &ref,
			CapChecker:    caps,
		})
		if err != nil {
			t.Fatalf("engine: %v", err)
		}
		return len(res.Hits)
	}
	without := visibility.CapabilityChecker(func(string) bool { return false })
	with := visibility.CapabilityChecker(func(code string) bool { return code == fdGatedCap })

	if n := run(direct, without); n != 0 {
		t.Fatalf("premise failed: the gated field returned %d hits to a caller without "+
			"the capability, so this test cannot detect a bypass", n)
	}
	if n := run(fromDSL, without); n != 0 {
		t.Errorf("the canonical DSL %s returned %d hits for a gated field the caller "+
			"may not read", text, n)
	}
	// And the gate is a gate, not a dead predicate: the holder still sees
	// the row through both spellings.
	if a, b := run(direct, with), run(fromDSL, with); a != 1 || b != a {
		t.Errorf("with the capability: rail %d hits, canonical DSL %d, want 1 each", a, b)
	}
}
