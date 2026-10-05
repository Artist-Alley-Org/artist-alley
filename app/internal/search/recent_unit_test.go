// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25b: the recent order's pure parts, with no corpus: the
// cursor's two wire shapes and their compatibility rules, the count
// boundary, the keyset fragment, the comparator, and the cache key.
//
// ⛔ Kept in its own file, beside keyset_fragment_test.go, for the same
// reason that one gives: last_window_test.go names nothing this sprint
// introduced and so runs against the unfixed engine; the symbols below
// would turn that demonstration into a build error.

package search

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
)

func TestRecentCursor_WireShapes(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	t.Run("a relevance cursor keeps the legacy bytes, score always present", func(t *testing.T) {
		for _, score := range []float64{0.5, 0} {
			enc := EncodeCursor(&Cursor{LastScore: score, LastID: id, LastType: HitTypePost})
			raw, _ := base64.RawURLEncoding.DecodeString(enc)
			legacy, _ := json.Marshal(struct {
				S float64   `json:"s"`
				I uuid.UUID `json:"i"`
				T HitType   `json:"t"`
			}{score, id, HitTypePost})
			if string(raw) != string(legacy) {
				t.Errorf("relevance cursor bytes %s, want the legacy %s", raw, legacy)
			}
			back, err := DecodeCursor(enc)
			if err != nil || back.Recent() || back.LastScore != score || back.LastID != id || back.LastType != HitTypePost {
				t.Errorf("round trip: %+v %v", back, err)
			}
		}
	})

	t.Run("a recent cursor carries the discriminator and the microsecond key", func(t *testing.T) {
		ts := time.Date(2026, 9, 19, 12, 34, 56, 789012000, time.UTC)
		enc := EncodeCursor(&Cursor{Order: OrderRecent, LastRecency: ts, LastID: id, LastType: HitTypeAsset})
		raw, _ := base64.RawURLEncoding.DecodeString(enc)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m["o"] != "recent" || m["ts"] != float64(ts.UnixMicro()) || m["i"] != id.String() || m["t"] != "asset" {
			t.Errorf("recent cursor is %s", raw)
		}
		if _, hasScore := m["s"]; hasScore {
			t.Errorf("a recent cursor carries a score: %s", raw)
		}
		back, err := DecodeCursor(enc)
		if err != nil || !back.Recent() || !back.LastRecency.Equal(ts) || back.LastID != id {
			t.Errorf("round trip: %+v %v", back, err)
		}
	})

	t.Run("structural refusals", func(t *testing.T) {
		mk := func(v map[string]any) string {
			b, _ := json.Marshal(v)
			return base64.RawURLEncoding.EncodeToString(b)
		}
		for name, in := range map[string]string{
			"recent without its timestamp": mk(map[string]any{"o": "recent", "i": id, "t": "asset"}),
			"unknown order":                mk(map[string]any{"o": "newest", "ts": 1, "i": id, "t": "asset"}),
			"timestamp without an order":   mk(map[string]any{"ts": 1, "i": id, "t": "asset"}),
			"unknown type":                 mk(map[string]any{"o": "recent", "ts": 1, "i": id, "t": "thing"}),
			"malformed":                    "not-base64!!",
			"not json":                     base64.RawURLEncoding.EncodeToString([]byte("{")),
		} {
			if _, err := DecodeCursor(in); !errors.Is(err, ErrBadCursor) {
				t.Errorf("%s: err = %v, want ErrBadCursor", name, err)
			}
		}
	})

	t.Run("order against the query is checked after composition", func(t *testing.T) {
		recentSel, _ := facet.ParseSelection([]string{"last:5"})
		relevance := &Cursor{LastScore: 1, LastID: id, LastType: HitTypeAsset}
		recent := &Cursor{Order: OrderRecent, LastRecency: time.Now(), LastID: id, LastType: HitTypeAsset}
		for name, q := range map[string]Query{
			"recent cursor, relevance query": {Text: "cat", Cursor: recent},
			"relevance cursor, recent query": {Text: "cat", Filters: recentSel, Cursor: relevance},
		} {
			if err := checkCursorOrder(q); !errors.Is(err, ErrCursorOrder) || !errors.Is(err, ErrBadCursor) {
				t.Errorf("%s: %v", name, err)
			}
		}
		for name, q := range map[string]Query{
			"recent on recent":       {Filters: recentSel, Cursor: recent},
			"relevance on relevance": {Text: "cat", Cursor: relevance},
			"no cursor":              {Filters: recentSel},
		} {
			if err := checkCursorOrder(q); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

// TestRecentCount_Boundary is the unit seam the brief asks for: recent
// mode at exactly the budget reports the number and no cap; relevance at
// the same number keeps its existing capped contract.
func TestRecentCount_Boundary(t *testing.T) {
	if facet.FacetType("last") == "" || dsl.MaxLastWindow != TotalCountCap {
		t.Fatalf("MaxLastWindow (%d) and TotalCountCap (%d) are one budget", dsl.MaxLastWindow, TotalCountCap)
	}
	for _, c := range []struct {
		name       string
		counts     map[HitType]int
		recent     bool
		wantTotal  int
		wantCapped bool
	}{
		{"recent exactly 10000 across arms", map[HitType]int{HitTypeAsset: 6000, HitTypePost: 4000}, true, 10000, false},
		{"recent one arm at 10000", map[HitType]int{HitTypeAsset: 10000}, true, 10000, false},
		{"recent small", map[HitType]int{HitTypeAsset: 2, HitTypePost: 1}, true, 3, false},
		{"relevance exactly 10000", map[HitType]int{HitTypeAsset: 6000, HitTypePost: 4000}, false, 10000, true},
		{"relevance one arm at cap", map[HitType]int{HitTypeAsset: 10001, HitTypePost: 1}, false, 10000, true},
		{"relevance under the cap", map[HitType]int{HitTypeAsset: 9998, HitTypePost: 1}, false, 9999, false},
	} {
		total, capped := assembleTotal(c.counts, c.recent)
		if total != c.wantTotal || capped != c.wantCapped {
			t.Errorf("%s: (%d, %v), want (%d, %v)", c.name, total, capped, c.wantTotal, c.wantCapped)
		}
	}
}

// TestRecentKeyset_OneRowComparison: the recent keyset binds the
// cursor's timestamp, id and rank and compares the arm's constant rank
// inside the tuple; there is no `<`/`<=` switch.
func TestRecentKeyset_OneRowComparison(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	ts := time.Now()
	cur := &Cursor{Order: OrderRecent, LastRecency: ts, LastID: id, LastType: HitTypeCollection}
	if frag, args := recentKeysetFragment(HitTypeAsset, nil, "created_at", "id", 4); frag != "" || args != nil {
		t.Fatalf("a nil cursor must render nothing, got %q %v", frag, args)
	}
	frag, args := recentKeysetFragment(HitTypePost, cur, "posted_at", "id", 6)
	if !strings.Contains(frag, "ROW(posted_at, id, 0) < ROW($7::TIMESTAMPTZ, $8::UUID, $9::INT)") {
		t.Errorf("fragment: %s", frag)
	}
	if len(args) != 3 || args[0] != ts || args[1] != id || args[2] != recentRank(HitTypeCollection) {
		t.Errorf("args: %v", args)
	}
	if strings.Contains(frag, "<=") {
		t.Errorf("the recent keyset must not switch operators: %s", frag)
	}
}

// TestRecentComparator_IsTheTotalOrder: newer first, then greater id,
// then type ASC; the same key both the merge and the cut use, and the
// rank it reads is facet's.
func TestRecentComparator_IsTheTotalOrder(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lo := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	hi := uuid.MustParse("ffffffff-0000-0000-0000-000000000001")
	hits := []Hit{
		{Type: HitTypePost, ID: lo, recency: t0},
		{Type: HitTypeAsset, ID: lo, recency: t0},
		{Type: HitTypeCollection, ID: lo, recency: t0},
		{Type: HitTypePost, ID: hi, recency: t0},
		{Type: HitTypePost, ID: lo, recency: t0.Add(time.Microsecond)},
	}
	sort.SliceStable(hits, func(i, j int) bool { return recentKeyOf(hits[i]).before(recentKeyOf(hits[j])) })
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, string(h.Type)+":"+h.ID.String()[:1]+":"+h.recency.Format("05.000000"))
	}
	want := []string{"post:0:00.000001", "post:f:00.000000", "asset:0:00.000000", "collection:0:00.000000", "post:0:00.000000"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order %v, want %v", got, want)
	}
	// The cut is the same comparator: a cursor at the asset keeps
	// exactly what sorts after it.
	cut := recentKeyOfCursor(Cursor{Order: OrderRecent, LastRecency: t0, LastID: lo, LastType: HitTypeAsset})
	kept := []string{}
	for _, h := range hits {
		if cut.before(recentKeyOf(h)) {
			kept = append(kept, string(h.Type)+":"+h.ID.String()[:1])
		}
	}
	if strings.Join(kept, ",") != "collection:0,post:0" {
		t.Errorf("cut kept %v", kept)
	}
	// A key from the wire (UTC, microseconds) equals one from a scan in
	// another zone.
	local := t0.In(time.FixedZone("x", 3600))
	if recentKeyOf(Hit{Type: HitTypeAsset, ID: lo, recency: local}).before(recentKey{ts: t0, id: lo, t: HitTypeAsset}) {
		t.Error("equal instants in different zones compared as unequal")
	}
	for _, tp := range []HitType{HitTypeAsset, HitTypeCollection, HitTypePost} {
		if recentRank(tp) != facet.RecentRank(entityOf(tp)) {
			t.Errorf("recentRank(%s) is not facet.RecentRank", tp)
		}
	}
}

// TestRecentCacheKey_DistinguishesWindowsAndPages: witness L.
func TestRecentCacheKey_DistinguishesWindowsAndPages(t *testing.T) {
	sel20, _ := facet.ParseSelection([]string{"last:20"})
	sel50, _ := facet.ParseSelection([]string{"last:50"})
	id := uuid.New()
	base := Query{Text: "cat", Filters: sel20}
	other := Query{Text: "cat", Filters: sel50}
	if keyForQuery(base) == keyForQuery(other) {
		t.Error("last:20 cat and last:50 cat share a cache key")
	}
	t1 := time.Now()
	page2 := base
	page2.Cursor = &Cursor{Order: OrderRecent, LastRecency: t1, LastID: id, LastType: HitTypeAsset}
	if keyForQuery(base) == keyForQuery(page2) {
		t.Error("the first page and a recent cursor page share a cache key")
	}
	page2b := base
	page2b.Cursor = &Cursor{Order: OrderRecent, LastRecency: t1.Add(time.Microsecond), LastID: id, LastType: HitTypeAsset}
	if keyForQuery(page2) == keyForQuery(page2b) {
		t.Error("two recent cursors a microsecond apart share a cache key")
	}
	rel := Query{Text: "cat", Cursor: &Cursor{LastID: id, LastType: HitTypeAsset}}
	rec := Query{Text: "cat", Cursor: &Cursor{Order: OrderRecent, LastID: id, LastType: HitTypeAsset}}
	if keyForQuery(rel) == keyForQuery(rec) {
		t.Error("a relevance cursor and a recent cursor at one id share a cache key")
	}
}
