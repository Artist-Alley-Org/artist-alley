// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Pure (no-DB) unit tests for the social-history helpers (#563): the
// deterministic follow graph and the stable-hash primitives that drive
// the reproducible like / comment distribution. These prove the two
// acceptance invariants that don't need a live stack: every fictional
// user ends with ≥1 follower AND ≥1 followee, and the graph is
// byte-identical across runs (so both sites seed the same shape).

package seed

import (
	"sort"
	"testing"
	"time"
)

func sortedNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		// Deliberately non-alphabetical raw order to prove followEdges
		// relies on the caller's sort, not insertion order.
		names[i] = "user-" + string(rune('a'+(i*7)%26)) + "-" + itoa(i)
	}
	sort.Strings(names)
	return names
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestFollowEdges_EveryUserHasFollowerAndFollowee(t *testing.T) {
	for _, n := range []int{2, 3, 5, 8, 20, 57} {
		names := sortedNames(n)
		edges := followEdges(names)

		outDeg := map[string]int{}
		inDeg := map[string]int{}
		for _, e := range edges {
			if e[0] == e[1] {
				t.Fatalf("n=%d: self-edge %q", n, e[0])
			}
			outDeg[e[0]]++
			inDeg[e[1]]++
		}
		for _, u := range names {
			if outDeg[u] == 0 {
				t.Errorf("n=%d: user %q follows nobody (out-degree 0)", n, u)
			}
			if inDeg[u] == 0 {
				t.Errorf("n=%d: user %q has no followers (in-degree 0)", n, u)
			}
		}
	}
}

func TestFollowEdges_Deterministic(t *testing.T) {
	names := sortedNames(30)
	a := followEdges(names)
	b := followEdges(names)
	if len(a) != len(b) {
		t.Fatalf("edge count differs across runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("edge %d differs across runs: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestFollowEdges_NoDuplicates(t *testing.T) {
	edges := followEdges(sortedNames(25))
	seen := map[[2]string]struct{}{}
	for _, e := range edges {
		if _, dup := seen[e]; dup {
			t.Fatalf("duplicate edge %v", e)
		}
		seen[e] = struct{}{}
	}
}

func TestFollowEdges_HasPopularitySkew(t *testing.T) {
	// The head of the sorted list should accrue more followers than the
	// tail on average — otherwise the "some users are popular" intent is
	// lost. Compare in-degree of the first vs last fifth.
	names := sortedNames(40)
	edges := followEdges(names)
	inDeg := map[string]int{}
	for _, e := range edges {
		inDeg[e[1]]++
	}
	fifth := len(names) / 5
	var head, tail int
	for _, u := range names[:fifth] {
		head += inDeg[u]
	}
	for _, u := range names[len(names)-fifth:] {
		tail += inDeg[u]
	}
	if head <= tail {
		t.Errorf("expected head more popular than tail, got head=%d tail=%d", head, tail)
	}
}

func TestStableHash_StableAndSalted(t *testing.T) {
	if stableHash("a", "b") != stableHash("a", "b") {
		t.Fatal("stableHash not stable for identical parts")
	}
	if stableHash("a", "b") == stableHash("b", "a") {
		t.Fatal("stableHash should be order-sensitive")
	}
	if stableHash("ab") == stableHash("a", "b") {
		t.Fatal("stableHash should separate parts (no join collision)")
	}
}

func TestStableIntn_InRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		v := stableIntn(7, "x", itoa(i))
		if v < 0 || v >= 7 {
			t.Fatalf("stableIntn out of range: %d", v)
		}
	}
	if stableIntn(0, "x") != 0 {
		t.Fatal("stableIntn(0) should be 0")
	}
}

func TestStableFrac_InUnitInterval(t *testing.T) {
	for i := 0; i < 1000; i++ {
		f := stableFrac("x", itoa(i))
		if f < 0 || f >= 1 {
			t.Fatalf("stableFrac out of [0,1): %v", f)
		}
	}
}

func TestStableTimeBetween_WithinSpanAndDeterministic(t *testing.T) {
	lo := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 500; i++ {
		got := stableTimeBetween(lo, hi, "t", itoa(i))
		if got.Before(lo) || got.After(hi) {
			t.Fatalf("time %v out of span [%v,%v]", got, lo, hi)
		}
		if !got.Equal(stableTimeBetween(lo, hi, "t", itoa(i))) {
			t.Fatal("stableTimeBetween not deterministic")
		}
	}
	// Degenerate span collapses to lo.
	if got := stableTimeBetween(hi, lo, "t"); !got.Equal(hi) {
		t.Fatalf("inverted span should return lo, got %v", got)
	}
}

func TestPickDistinct_ExcludesAndDedups(t *testing.T) {
	pool := sortedNames(20)
	exclude := pool[3]
	got := pickDistinct(pool, 5, exclude, "salt")
	if len(got) != 5 {
		t.Fatalf("want 5, got %d", len(got))
	}
	seen := map[string]struct{}{}
	for _, name := range got {
		if name == exclude {
			t.Fatalf("excluded name %q was picked", exclude)
		}
		if _, dup := seen[name]; dup {
			t.Fatalf("duplicate pick %q", name)
		}
		seen[name] = struct{}{}
	}
	// Deterministic + salt-sensitive.
	if !equalStrings(got, pickDistinct(pool, 5, exclude, "salt")) {
		t.Fatal("pickDistinct not deterministic")
	}
	if equalStrings(got, pickDistinct(pool, 5, exclude, "other")) {
		t.Fatal("pickDistinct should vary by salt")
	}
	// Requesting more than available caps at pool-minus-exclude.
	if n := len(pickDistinct(pool, 999, exclude, "s")); n != len(pool)-1 {
		t.Fatalf("over-request should cap at %d, got %d", len(pool)-1, n)
	}
}

func equalStrings(a, b []string) bool {
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
