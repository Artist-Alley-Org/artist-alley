// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package feedback

import "testing"

func TestCanonicalizeDSL_TrimAndLowercase(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"cat", "cat"},
		{"  Cat  ", "cat"},
		{"Cat AND Dog", "cat and dog"},
		{"cat   AND\tdog", "cat and dog"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := CanonicalizeDSL(tc.in); got != tc.want {
			t.Fatalf("Canonicalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHashDSL_SameCanonical_SameHash(t *testing.T) {
	a := HashDSL("Cat AND Dog")
	b := HashDSL("  cat AND dog  ")
	if a != b {
		t.Fatalf("same canonical form produced different hashes: %s vs %s", a, b)
	}
}

func TestHashDSL_DifferentInputs_DifferentHash(t *testing.T) {
	a := HashDSL("cat")
	b := HashDSL("dog")
	if a == b {
		t.Fatal("different queries hashed to the same value")
	}
}

func TestHashDSL_DeterministicLength(t *testing.T) {
	got := HashDSL("anything")
	if len(got) != 64 {
		t.Fatalf("hex sha256 must be 64 chars, got %d", len(got))
	}
}
