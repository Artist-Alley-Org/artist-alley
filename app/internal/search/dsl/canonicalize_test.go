// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25a: [dsl.Canonicalize], the token-level rewrite that
// makes a stored query carry the typed spelling of a verb.
//
// Kept apart from verbs_test.go on purpose: that file is the
// fail-before-fix witness and compiles on the commit before this
// sprint, while Canonicalize is a symbol this sprint introduces.

package dsl_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
)

// ── C. Canonical text ────────────────────────────────────────────────

func TestCanonicalize_WritesTheTypedSpellingAndNothingElse(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"!nopreviews", "preview:missing"},
		{"cat !nopreviews", "cat preview:missing"},
		{"!list" + vA, "id:" + vA},
		{"!list" + vA + "," + vB, "(id:" + vA + " AND id:" + vB + ")"},
		{"!list" + strings.ToUpper(vA) + "," + vA, "id:" + vA},
		{"(cat   OR   dog)  AND !nopreviews AND !list" + vB, "(cat   OR   dog)  AND preview:missing AND id:" + vB},
		// Untouched: no verbs, a quoted verb, an existing dimension.
		{"cat OR dog", "cat OR dog"},
		{`"!nopreviews"`, `"!nopreviews"`},
		{"extension:png AND NOT tag:x", "extension:png AND NOT tag:x"},
		{"  padded  ", "  padded  "},
		{"", ""},
	} {
		got, err := dsl.Canonicalize(c.in)
		if err != nil {
			t.Errorf("Canonicalize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Canonicalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCanonicalize_PreservesTheTree is the property that makes the
// rewrite safe to store: the canonical text parses to the SAME tree the
// aliased input did, and re-canonicalising is a fixed point.
func TestCanonicalize_PreservesTheTree(t *testing.T) {
	for _, in := range []string{
		"!nopreviews",
		"cat AND !nopreviews",
		"(cat OR dog) AND !list" + vA + "," + vB + " AND extension:png",
		"NOT cat !list" + vC,
		"!list" + vA + " !nopreviews",
	} {
		canon, err := dsl.Canonicalize(in)
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", in, err)
		}
		if !reflect.DeepEqual(mustParse(t, in).Root, mustParse(t, canon).Root) {
			t.Errorf("%q and its canonical %q parse to different trees", in, canon)
		}
		again, err := dsl.Canonicalize(canon)
		if err != nil || again != canon {
			t.Errorf("Canonicalize is not a fixed point: %q → %q (%v)", canon, again, err)
		}
	}
}

func TestCanonicalize_RefusesWhatTheParserRefuses(t *testing.T) {
	for _, in := range []string{"!bogus", "!list", "!list:" + vA, "!listjunk", "!list" + vA + ",,"} {
		if _, err := dsl.Canonicalize(in); err == nil {
			t.Errorf("Canonicalize(%q) accepted a verb the parser refuses", in)
		}
	}
}
