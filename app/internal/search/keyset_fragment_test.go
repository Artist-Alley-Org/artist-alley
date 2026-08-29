// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1356 — the rendering rules of the per-arm keyset, with no corpus.
//
// ⛔ THIS LIVES IN ITS OWN FILE ON PURPOSE. cursor_exhaustion_test.go
// names nothing that #1356 introduced, so it COMPILES AND RUNS against
// the unfixed engine — which is what lets the fix be demonstrated by
// restoring one source file and watching the walk report 30 of 48. A
// unit test naming [keysetFragment] in that file would turn that
// demonstration into a build error, which proves nothing.

package search

import (
	"testing"

	"github.com/google/uuid"
)

// TestKeysetFragment covers the rendering rules that do not need a
// corpus — including the one branch this suite cannot reach at runtime.
func TestKeysetFragment(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	cur := &Cursor{LastScore: 0.5, LastID: id, LastType: HitTypeCollection}

	t.Run("the first page is not positioned", func(t *testing.T) {
		frag, args := keysetFragment(HitTypeAsset, nil, 1.0, "s", "id", 4)
		if frag != "" || args != nil {
			t.Fatalf("a nil cursor must render nothing, got %q with %v", frag, args)
		}
	})

	t.Run("the denominator is rendered, never inverted", func(t *testing.T) {
		frag, args := keysetFragment(HitTypeAsset, cur, 2.5, "SCOREEXPR", "id", 4)
		// ⛔ The normalisation belongs in SQL. Inverting it in Go —
		// `score < last_score * max` — is a DIFFERENT rounding of the
		// same two doubles and would put the boundary row on either side
		// of the cut depending on the values.
		if !containsAll(frag, "(SCOREEXPR)::FLOAT8 / $7::FLOAT8", "ROW(", "$5::FLOAT8", "$6::UUID") {
			t.Fatalf("fragment does not divide by the bound maximum: %s", frag)
		}
		if len(args) != 3 || args[0] != 0.5 || args[2] != 2.5 {
			t.Fatalf("args are %v, want (last_score, last_id, max)", args)
		}
	})

	t.Run("a zero maximum renders a constant, not a division", func(t *testing.T) {
		// A filter-only search ranks every row at 0, and Run leaves that
		// type unnormalised rather than dividing by zero. The fragment
		// has to agree with that branch instead of inventing a
		// denominator for it.
		frag, args := keysetFragment(HitTypeAsset, cur, 0, "SCOREEXPR", "id", 4)
		if containsAll(frag, "SCOREEXPR") {
			t.Fatalf("a zero maximum must not divide: %s", frag)
		}
		if !containsAll(frag, "ROW(0::FLOAT8, id)") {
			t.Fatalf("a zero maximum must render the constant: %s", frag)
		}
		if len(args) != 2 {
			t.Fatalf("args are %v, want (last_score, last_id) with no maximum", args)
		}
	})

	t.Run("the type tie-break decides the comparison operator", func(t *testing.T) {
		// Ordering's third component is `Type DESC`, and it is constant
		// within an arm. An arm whose type sorts AFTER the cursor's owns
		// the rows at the cursor's exact (score, id); one that sorts
		// before does not. `<=` versus `<` on the row comparison is how
		// that is spelled, and it must mirror cursorLess term for term.
		after, _ := keysetFragment(HitTypeAsset, cur, 1, "s", "id", 4) // "asset" < "collection"
		if !containsAll(after, ") <= ROW(") {
			t.Fatalf("an arm sorting after the cursor's type must use <=: %s", after)
		}
		before, _ := keysetFragment(HitTypePost, cur, 1, "s", "id", 4) // "post" > "collection"
		if !containsAll(before, ") < ROW(") || containsAll(before, "<=") {
			t.Fatalf("an arm sorting before the cursor's type must use <: %s", before)
		}
	})
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
