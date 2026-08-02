// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Unit cover for the two pieces of #830 that decide what a term
// BECOMES, independently of any transaction: the slug convention, and
// the matcher that decides whether a term is new at all.
//
// The write path itself is covered end-to-end against a real database
// in open_vocabulary_e2e_test.go — the interesting parts of it are the
// row lock and the rollback, neither of which a unit test can see.
package metadata

import (
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Sunset", "sunset"},
		{"  sunset  ", "sunset"},
		{"Sunset Over Water", "sunset-over-water"},
		{"Black & White", "black-white"},
		{"rock/pop", "rock-pop"},
		{"Rec. 709", "rec-709"},
		{"---leading and trailing---", "leading-and-trailing"},
		{"multiple     spaces", "multiple-spaces"},
		// Nothing addressable: not a term.
		{"!!!", ""},
		{"", ""},
		// Non-ASCII collapses rather than transliterating. Deliberate:
		// guessing that "é" means "e" is a language decision, and the
		// label keeps the original text either way.
		{"café", "caf"},
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugifyCapsLength(t *testing.T) {
	long := ""
	for range 200 {
		long += "a"
	}
	if got := Slugify(long); len(got) != slugMaxLen {
		t.Errorf("Slugify(200 chars) length = %d, want %d", len(got), slugMaxLen)
	}
	// The cap must not leave a trailing hyphen, which would be a slug
	// no UI could reproduce by typing the same words.
	spaced := "aaaaaaaaaa bbbbbbbbbb cccccccccc dddddddddd eeeeeeeeee ffffffffff gggggggggg hhhhhhhhhh"
	if got := Slugify(spaced); got[len(got)-1] == '-' {
		t.Errorf("Slugify capped to a trailing hyphen: %q", got)
	}
}

func vocabularyFixture() []FieldOption {
	return []FieldOption{
		{Value: "abstract", Label: "Abstract"},
		{Value: "black-and-white", Label: "Black and White"},
		{Value: "retired", Label: "Retired", Status: OptionDeprecated},
		{Value: "gone", Label: "Gone For Good", Status: OptionArchived},
		// Bare-slug entry, the form the seeder writes.
		{Value: "portrait"},
		// Nested, because the index walks at full depth for the same
		// reason resolveOptionSlugs does — slugs are unique tree-wide.
		{Value: "europe", Label: "Europe", Children: []FieldOption{
			{Value: "london", Label: "London"},
		}},
	}
}

func TestVocabularyIndex_MatchesSlugAndLabel(t *testing.T) {
	idx := indexVocabulary(vocabularyFixture())
	cases := []struct{ in, want string }{
		{"abstract", "abstract"}, // slug, exact
		{"Abstract", "abstract"}, // label
		{"ABSTRACT", "abstract"}, // either, wrong case
		{"Black and White", "black-and-white"},
		{"black-and-white", "black-and-white"},
		{"portrait", "portrait"}, // bare-slug entry
		{"London", "london"},     // nested, by label
		{"Retired", "retired"},   // deprecated is still matchable
	}
	for _, c := range cases {
		slug, matched, rej := idx.resolveOrMint(c.in, true)
		if rej != nil {
			t.Errorf("resolveOrMint(%q) refused: %v", c.in, rej)
			continue
		}
		if !matched {
			t.Errorf("resolveOrMint(%q) minted %q instead of matching %q", c.in, slug, c.want)
			continue
		}
		if slug != c.want {
			t.Errorf("resolveOrMint(%q) = %q, want %q", c.in, slug, c.want)
		}
	}
}

func TestVocabularyIndex_ArchivedIsNotMatchable(t *testing.T) {
	idx := indexVocabulary(vocabularyFixture())
	// The archived term's LABEL slugifies to something free, so it
	// mints rather than resurrecting.
	slug, matched, rej := idx.resolveOrMint("Gone For Good", true)
	if rej != nil {
		t.Fatalf("refused: %v", rej)
	}
	if matched {
		t.Fatalf("an archived term matched and was resurrected as %q", slug)
	}
	if slug != "gone-for-good" {
		t.Errorf("minted %q, want gone-for-good", slug)
	}
}

func TestVocabularyIndex_ArchivedSlugCollisionRefuses(t *testing.T) {
	idx := indexVocabulary(vocabularyFixture())
	// "Gone" slugifies onto the archived `gone` — refused rather than
	// disambiguated into `gone-2`.
	_, _, rej := idx.resolveOrMint("Gone", true)
	if rej == nil {
		t.Fatal("minting over an archived slug was allowed")
	}
	if rej.unknown() {
		t.Errorf("rejection reported as unknown; want the archived status so the "+
			"operator is told the term exists and is retired (got %+v)", rej)
	}
	if rej.Status != OptionArchived {
		t.Errorf("status = %q, want archived", rej.Status)
	}
}

func TestVocabularyIndex_MintIsIdempotentWithinOnePass(t *testing.T) {
	idx := indexVocabulary(vocabularyFixture())
	first, matched, rej := idx.resolveOrMint("Sunset Over Water", true)
	if rej != nil || matched {
		t.Fatalf("first pass: slug=%q matched=%v rej=%v", first, matched, rej)
	}
	// A second spelling of the same term in the SAME request must match
	// what was just minted, not mint again.
	second, matched, rej := idx.resolveOrMint("  sunset over water  ", true)
	if rej != nil {
		t.Fatalf("second pass refused: %v", rej)
	}
	if !matched {
		t.Error("the same term minted twice in one pass")
	}
	if second != first {
		t.Errorf("second = %q, first = %q", second, first)
	}
}

func TestVocabularyIndex_MintOffRefusesEveryMiss(t *testing.T) {
	idx := indexVocabulary(vocabularyFixture())
	// mint=false is what a field whose flag was turned off between the
	// caller's cached read and the row lock gets.
	_, _, rej := idx.resolveOrMint("Sunset", false)
	if rej == nil {
		t.Fatal("a miss was accepted with minting off")
	}
	if !rej.unknown() {
		t.Errorf("want the unknown-slug rejection a closed field gives, got %+v", rej)
	}
	// Matching still works with minting off.
	if slug, matched, rej := idx.resolveOrMint("Abstract", false); rej != nil || !matched || slug != "abstract" {
		t.Errorf("resolveOrMint(Abstract, false) = (%q, %v, %v)", slug, matched, rej)
	}
}

func TestOpenVocabularyApplies(t *testing.T) {
	cases := []struct {
		ftype string
		open  bool
		want  bool
	}{
		{"multi_select", true, true},
		{"multi_select", false, false},
		// Legal on every type, honoured on one. Setting it elsewhere is
		// inert rather than an error — see the migration's comment.
		{"select", true, false},
		{"tree", true, false},
		{"text", true, false},
	}
	for _, c := range cases {
		if got := openVocabularyApplies(c.ftype, c.open); got != c.want {
			t.Errorf("openVocabularyApplies(%q, %v) = %v, want %v", c.ftype, c.open, got, c.want)
		}
	}
}
