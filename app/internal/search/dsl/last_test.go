// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25b: the `!lastN` verb and the `last` dimension at the
// grammar: the fold, the range, the malformations, placement, and the
// all-DSL half of the similarity incompatibility.
//
// # ⛔ WRITTEN AGAINST THE SURFACE, SO THEY COMPILE ON THE COMMIT BEFORE
//
// Phrased over strings, the public AST and [dsl.Filters] compared whole,
// never over a symbol this sprint introduced (`Field("last")`, not
// `dsl.FieldLast`; the Filters struct is compared through reflect so a
// field the old struct lacks is a value mismatch, not a build error).
// On `dev` at d54714bb `!last5` is an unknown verb and `last:5` an
// unknown field; every positive assertion below goes red there for one
// of those two reasons.

package dsl_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
)

const fieldLast = dsl.Field("last")

func TestLast_AliasAndCanonicalAreOneTree(t *testing.T) {
	for _, c := range []struct{ alias, typed, want string }{
		{"!last5", "last:5", "5"},
		{"!last1", "last:1", "1"},
		{"!last10000", "last:10000", "10000"},
		{"!LAST7", "LAST:7", "7"}, // the name folds case, the digits do not
	} {
		a := mustParse(t, c.alias)
		b := mustParse(t, c.typed)
		if !reflect.DeepEqual(a.Root, b.Root) {
			t.Errorf("%q parses to %#v and %q to %#v; the alias must fold onto the typed node", c.alias, a.Root, c.typed, b.Root)
		}
		fm, ok := a.Root.(dsl.FieldMatchNode)
		if !ok || fm.Field != fieldLast || fm.Value != c.want {
			t.Errorf("%q parsed to %#v, want FieldMatchNode{last, %q}", c.alias, a.Root, c.want)
		}
	}
	// Leading zeros are digits; the fold writes the canonical digits so
	// the alias still equals the typed spelling AFTER the bridge
	// canonicalises the typed one (facet.CanonicalValue drops the zeros).
	z := mustParse(t, "!last05")
	if fm, ok := z.Root.(dsl.FieldMatchNode); !ok || fm.Value != "5" {
		t.Errorf("!last05 parsed to %#v, want the canonical value 5", z.Root)
	}
}

func TestLast_CompilesToOneFilterTerm(t *testing.T) {
	for _, in := range []string{"!last20 cat", "cat AND last:20", "last:20 AND cat", "(cat OR dog) AND !last20"} {
		q := mustParse(t, in)
		compiled, err := dsl.Compile(q)
		if err != nil {
			t.Fatalf("Compile(%q): %v", in, err)
		}
		want := dsl.Filters{Lasts: []string{"20"}}
		if !reflect.DeepEqual(compiled.Filters, want) {
			t.Errorf("Compile(%q).Filters = %#v, want %#v", in, compiled.Filters, want)
		}
		if compiled.FreeText == "" || strings.Contains(compiled.FreeText, "last") {
			t.Errorf("Compile(%q).FreeText = %q; the window is a filter, not a word", in, compiled.FreeText)
		}
	}
}

// TestLast_MalformedIsRefused covers every refusal the brief lists, on
// the alias. `!last 5` lexes as `!last` (empty payload) and `5`, so it is
// refused as the empty payload; `!last:5` is the shared colon look-ahead.
func TestLast_MalformedIsRefused(t *testing.T) {
	for _, in := range []string{
		"!last", "!last 5", "!last0", "!last10001", "!last-1", "!lastx", "!last5x", "!last:5", "!last+5", "!last 0",
	} {
		_, err := dsl.Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) accepted it", in)
			continue
		}
		var de dsl.DSLError
		if !errors.As(err, &de) {
			t.Errorf("Parse(%q): %v is not a DSLError", in, err)
			continue
		}
		if !strings.Contains(err.Error(), "!last") {
			t.Errorf("Parse(%q) = %q, want a message naming the verb", in, err)
		}
	}
	// The unrelated unknown verbs stay unknown, with their own kind.
	for _, in := range []string{"!bogus", "!", "!lastly5x"} {
		_, err := dsl.Parse(in)
		var de dsl.DSLError
		if err == nil || !errors.As(err, &de) {
			t.Errorf("Parse(%q) = %v, want a DSLError", in, err)
		}
	}
}

func TestLast_ParseWindowIsTheOneGrammar(t *testing.T) {
	for _, c := range []struct {
		in   string
		ok   bool
		want int
	}{
		{"1", true, 1}, {"5", true, 5}, {"10000", true, 10000}, {"05", true, 5}, {"0010000", true, 10000},
		{"", false, 0}, {"0", false, 0}, {"10001", false, 0}, {"-1", false, 0}, {"+5", false, 0},
		{"x", false, 0}, {"5x", false, 0}, {" 5", false, 0}, {"5 ", false, 0}, {"1e3", false, 0},
		{"99999999999999999999", false, 0},
	} {
		got, ok := dsl.ParseLastWindow(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseLastWindow(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
	if dsl.MaxLastWindow != 10000 {
		t.Errorf("MaxLastWindow = %d, want 10000", dsl.MaxLastWindow)
	}
}

func TestLast_PlacementIsTopLevelOnlyOnBothSpellings(t *testing.T) {
	for _, in := range []string{
		"NOT !last5", "NOT last:5", "cat OR !last5", "cat OR last:5", "!last5 OR cat",
		"cat AND (dog OR last:5)", "(cat OR !last5) AND dog", "NOT (cat AND last:5)",
	} {
		q := mustParse(t, in)
		_, err := dsl.Compile(q)
		var de dsl.DSLError
		if err == nil || !errors.As(err, &de) || !strings.Contains(err.Error(), "top-level") {
			t.Errorf("Compile(%q) = %v, want a placement refusal", in, err)
		}
	}
	for _, in := range []string{
		"!last5", "last:5", "cat !last5", "!last5 cat", "(cat OR dog) AND !last5", "(cat OR dog) AND last:5",
		"!last5 AND extension:png", "!last5 AND !nopreviews", "!last5 AND !list" + vA,
	} {
		q := mustParse(t, in)
		if _, err := dsl.Compile(q); err != nil {
			t.Errorf("Compile(%q) refused a legal top-level conjunction: %v", in, err)
		}
	}
}

// TestLast_RepeatedValuesSurviveToTheSelection: the compiler does not
// decide single-valuedness; it carries both terms so the ONE authority
// (facet.Selection.Validate) can refuse them on every entry path.
func TestLast_RepeatedValuesSurviveToTheSelection(t *testing.T) {
	q := mustParse(t, "last:3 AND last:5")
	compiled, err := dsl.Compile(q)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !reflect.DeepEqual(compiled.Filters.Lasts, []string{"3", "5"}) {
		t.Errorf("Lasts = %v, want both terms carried whole", compiled.Filters.Lasts)
	}
}

// TestLast_WithSimilarityIsOneDeterministicRefusal: the all-DSL forms,
// on both spellings, refused by the compiler with the one value the
// engine also returns for the split form.
func TestLast_WithSimilarityIsOneDeterministicRefusal(t *testing.T) {
	for _, in := range []string{
		"last:5 AND similar_to:" + vA, "!last5 AND similar_to:" + vA,
		"similar_to:" + vA + " AND !last5", "similar_to:" + vA + " last:5", "cat !last5 similar_to:" + vA,
	} {
		q := mustParse(t, in)
		_, err := dsl.Compile(q)
		var de dsl.DSLError
		if err == nil || !errors.As(err, &de) {
			t.Errorf("Compile(%q) = %v, want a DSLError", in, err)
			continue
		}
		if !reflect.DeepEqual(de, dsl.ErrLastWithSimilarity) {
			t.Errorf("Compile(%q) = %#v, want exactly ErrLastWithSimilarity", in, de)
		}
	}
	// Either alone is fine.
	for _, in := range []string{"similar_to:" + vA, "!last5", "cat similar_to:" + vA} {
		if _, err := dsl.Compile(mustParse(t, in)); err != nil {
			t.Errorf("Compile(%q): %v", in, err)
		}
	}
}

func TestLast_CanonicalizeWritesTheTypedSpelling(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"!last20 cat", "last:20 cat"},
		{"cat AND !last3", "cat AND last:3"},
		{"!last05", "last:5"},
		{`"!last5"`, `"!last5"`}, // a quoted verb is a phrase and stays one
		{"last:20 cat", "last:20 cat"},
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
	if _, err := dsl.Canonicalize("!last0"); err == nil {
		t.Error("Canonicalize(!last0) accepted what the parser refuses")
	}
	if got := dsl.SerializeTerm(fieldLast, "7"); got != "last:7" {
		t.Errorf("SerializeTerm(last, 7) = %q", got)
	}
}

func TestLast_IsInTheWhitelist(t *testing.T) {
	if f, ok := dsl.ParseField("last"); !ok || f != fieldLast {
		t.Errorf("ParseField(last) = (%q, %v)", f, ok)
	}
	found := false
	for _, f := range dsl.AllFields {
		if f == fieldLast {
			found = true
		}
	}
	if !found {
		t.Error("AllFields lacks `last`, so the unknown-field error would not list it")
	}
}
