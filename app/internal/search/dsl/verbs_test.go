// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25a: the `!nopreviews` and `!list` verbs, the `preview`
// and `id` dimensions, and the top-level-only placement rule.
//
// # ⛔ WRITTEN AGAINST THE SURFACE, SO THEY COMPILE ON THE COMMIT BEFORE
//
// Every assertion here is phrased over strings and the public AST, a
// node's dynamic type, a Field compared as a string, [dsl.Filters]
// compared whole with reflect, and never over a symbol this sprint
// introduced. That is deliberate: on `dev` at b031044f these tests
// COMPILE and FAIL, which is the only kind of fail-before-fix evidence
// worth having. A test that fails to compile on the old code proves
// nothing about behaviour.
//
// What was true before: `!nopreviews` and `!list…` lexed as one word
// each and parsed as free text; `preview:` and `id:` were unknown
// fields; nothing had a placement rule.

package dsl_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
)

const (
	vA = "0f0e6c1a-1111-4a5b-8c7d-000000000001"
	vB = "0f0e6c1a-2222-4a5b-8c7d-000000000002"
	vC = "0f0e6c1a-3333-4a5b-8c7d-000000000003"
)

// fieldTerms collects every `field:value` leaf in walk order.
func fieldTerms(n dsl.Node) []dsl.FieldMatchNode {
	switch x := n.(type) {
	case dsl.AndNode:
		return append(fieldTerms(x.Left), fieldTerms(x.Right)...)
	case dsl.OrNode:
		return append(fieldTerms(x.Left), fieldTerms(x.Right)...)
	case dsl.NotNode:
		return fieldTerms(x.Inner)
	case dsl.FieldMatchNode:
		return []dsl.FieldMatchNode{x}
	}
	return nil
}

func mustParse(t *testing.T, in string) dsl.Query {
	t.Helper()
	q, err := dsl.Parse(in)
	if err != nil {
		t.Fatalf("Parse(%q): %v", in, err)
	}
	return q
}

func compileFilters(t *testing.T, in string) dsl.Filters {
	t.Helper()
	c, err := dsl.Compile(mustParse(t, in))
	if err != nil {
		t.Fatalf("Compile(%q): %v", in, err)
	}
	return c.Filters
}

// ── A. Grammar ───────────────────────────────────────────────────────

// TestVerb_NoPreviewsIsAConstraintNotFreeText is the sprint's first
// witness: the word the owner types is a dimension, not a tsquery term.
func TestVerb_NoPreviewsIsAConstraintNotFreeText(t *testing.T) {
	q := mustParse(t, "!nopreviews")
	if _, free := q.Root.(dsl.FreeTextNode); free {
		t.Fatal("`!nopreviews` parsed as FreeTextNode: the verb reached plainto_tsquery as " +
			"the word \"nopreviews\" and the query returned an empty 200")
	}
	fm, ok := q.Root.(dsl.FieldMatchNode)
	if !ok {
		t.Fatalf("Root = %T, want FieldMatchNode", q.Root)
	}
	if string(fm.Field) != "preview" || fm.Value != "missing" {
		t.Errorf("folded to %s:%s, want preview:missing", fm.Field, fm.Value)
	}
	// And the compiled query carries NO text: a verb is a filter.
	c, err := dsl.Compile(q)
	if err != nil {
		t.Fatal(err)
	}
	if c.FreeText != "" || c.TSQuery != "" {
		t.Errorf("a verb contributed text: FreeText=%q TSQuery=%q", c.FreeText, c.TSQuery)
	}
}

func TestVerb_ListParsesToIDTerms(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"!list" + vA, []string{vA}},
		{"!list" + vA + "," + vB + "," + vC, []string{vA, vB, vC}},
	} {
		q := mustParse(t, c.in)
		if _, free := q.Root.(dsl.FreeTextNode); free {
			t.Errorf("%q parsed as free text", c.in)
			continue
		}
		terms := fieldTerms(q.Root)
		got := make([]string, 0, len(terms))
		for _, term := range terms {
			if string(term.Field) != "id" {
				t.Errorf("%q: term on field %q, want id", c.in, term.Field)
			}
			got = append(got, term.Value)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: ids = %v, want %v (exactly N, in order)", c.in, got, c.want)
		}
	}
}

// TestVerb_AliasAndCanonicalAreOneTree is "one grammar, no second
// executor" stated as an equality: the alias and the typed spelling
// produce the SAME AST and the SAME compiled Filters, so nothing
// downstream can tell which one was typed.
func TestVerb_AliasAndCanonicalAreOneTree(t *testing.T) {
	for _, c := range []struct{ alias, canonical string }{
		{"!nopreviews", "preview:missing"},
		{"!list" + vA, "id:" + vA},
		{"!list" + vA + "," + vB, "(id:" + vA + " AND id:" + vB + ")"},
		{"cat AND !nopreviews", "cat AND preview:missing"},
		{"cat !list" + vA + "," + vB, "cat (id:" + vA + " AND id:" + vB + ")"},
	} {
		a, b := mustParse(t, c.alias), mustParse(t, c.canonical)
		if !reflect.DeepEqual(a.Root, b.Root) {
			t.Errorf("AST differs:\n  %q → %#v\n  %q → %#v", c.alias, a.Root, c.canonical, b.Root)
		}
		fa, fb := compileFilters(t, c.alias), compileFilters(t, c.canonical)
		if !reflect.DeepEqual(fa, fb) {
			t.Errorf("Filters differ:\n  %q → %+v\n  %q → %+v", c.alias, fa, c.canonical, fb)
		}
	}
}

func TestVerb_UUIDsCanonicaliseToLowercaseHyphenated(t *testing.T) {
	upper := strings.ToUpper(vA)
	braced := "{" + vA + "}"
	terms := fieldTerms(mustParse(t, "!list"+upper+","+braced).Root)
	if len(terms) != 1 {
		t.Fatalf("%d id terms, want 1: the same UUID in two spellings must collapse to one", len(terms))
	}
	if terms[0].Value != vA {
		t.Errorf("value = %q, want the canonical lowercase hyphenated %q", terms[0].Value, vA)
	}
}

func TestVerb_DuplicatesCollapseInTheFold(t *testing.T) {
	terms := fieldTerms(mustParse(t, "!list"+vA+","+vA+","+vB+","+vA).Root)
	if len(terms) != 2 {
		t.Fatalf("%d id terms, want 2 (a, b): %v", len(terms), terms)
	}
	if terms[0].Value != vA || terms[1].Value != vB {
		t.Errorf("order = %s, %s; want first-seen order a, b", terms[0].Value, terms[1].Value)
	}
}

// TestVerb_MalformedIsAnErrorNamingTheForm covers every malformation the
// brief lists. On `dev` each of these parses as free text and the
// assertion that an error came back is what goes red.
func TestVerb_MalformedIsAnErrorNamingTheForm(t *testing.T) {
	for _, c := range []struct{ in, wantInMsg string }{
		{"!nopreview", "!nopreviews"},     // unknown verb; the message lists the real one
		{"!nopreviewsx", "!nopreviews"},   // known verb with a payload it does not take
		{"!list", "!list<uuid>"},          // no ids
		{"!list:" + vA + ":" + vB, "':'"}, // colon is not the delimiter
		{"!list" + vA + ",," + vB, "empty entry"},
		{"!list" + vA + ",", "empty entry"},
		{"!list,", "empty entry"},
		{"!listnot-a-uuid", "not a UUID"},
		{"!list" + vA + ",not-a-uuid", "not a UUID"},
		{"!bogus", "unknown verb"},
		{"!", "unknown verb"},
		// ⛔ PINNED UNKNOWN IN 25a. Sprint 25b registers `last` and flips
		// this case to a parse; until then it must not silently be free
		// text, which is what it was.
		{"!last5", "unknown verb"},
	} {
		_, err := dsl.Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) accepted it", c.in)
			continue
		}
		var de dsl.DSLError
		if !errors.As(err, &de) {
			t.Errorf("Parse(%q): %v is not a DSLError, so the HTTP edge cannot render it", c.in, err)
			continue
		}
		if !strings.Contains(err.Error(), c.wantInMsg) {
			t.Errorf("Parse(%q) = %q, want a message naming %q", c.in, err, c.wantInMsg)
		}
	}
}

func TestVerb_QuotedVerbIsAPhrase(t *testing.T) {
	q := mustParse(t, `"!nopreviews"`)
	if _, ok := q.Root.(dsl.PhraseNode); !ok {
		t.Errorf("a quoted verb is a phrase, got %T", q.Root)
	}
}

// ── B. Placement ─────────────────────────────────────────────────────

func TestPlacement_TopLevelOnlyDimensionsRefuseNotAndOr(t *testing.T) {
	rejected := []string{
		"NOT !nopreviews",
		"NOT preview:missing",
		"NOT !list" + vA,
		"NOT id:" + vA,
		"cat OR !list" + vA,
		"cat OR id:" + vA,
		"cat OR !nopreviews",
		"cat OR preview:missing",
		"!nopreviews OR cat",
		"cat AND (dog OR id:" + vA + ")",
		"cat AND (dog OR !list" + vA + "," + vB + ")",
		"(cat OR !nopreviews) AND dog",
		"NOT (cat AND !nopreviews)",
		"NOT (!list" + vA + ")",
	}
	for _, in := range rejected {
		q, err := dsl.Parse(in)
		if err == nil {
			_, err = dsl.Compile(q)
		}
		if err == nil {
			t.Errorf("%q was accepted; under the compiler's flattening it would mean the "+
				"OPPOSITE of what it says", in)
			continue
		}
		if !strings.Contains(err.Error(), "top-level") {
			t.Errorf("%q refused for the wrong reason: %v", in, err)
		}
	}
	accepted := []string{
		"(cat OR dog) AND id:" + vA,
		"(cat OR dog) AND !list" + vA + "," + vB,
		"cat !nopreviews",
		"!nopreviews AND !list" + vA,
		"(!nopreviews)",
		"((preview:missing))",
		"NOT cat AND id:" + vA,
		"cat AND NOT dog AND !nopreviews",
		"(cat OR NOT dog) AND preview:missing AND id:" + vB,
	}
	for _, in := range accepted {
		if _, err := dsl.Compile(mustParse(t, in)); err != nil {
			t.Errorf("%q refused: %v; a top-level conjunct beside any structure is legal", in, err)
		}
	}
}

// TestPlacement_AliasAndCanonicalFailIdentically pins that the check
// runs on the RESOLVED dimension: the two spellings cannot acquire
// different placement semantics because by the time placement is
// judged there is only one of them.
func TestPlacement_AliasAndCanonicalFailIdentically(t *testing.T) {
	for _, pair := range [][2]string{
		{"NOT !nopreviews", "NOT preview:missing"},
		{"cat OR !list" + vA, "cat OR id:" + vA},
	} {
		msgs := [2]string{}
		for i, in := range pair {
			q, err := dsl.Parse(in)
			if err == nil {
				_, err = dsl.Compile(q)
			}
			if err == nil {
				t.Fatalf("%q accepted", in)
			}
			msgs[i] = err.Error()
		}
		if msgs[0] != msgs[1] {
			t.Errorf("%q and %q fail differently:\n  %s\n  %s", pair[0], pair[1], msgs[0], msgs[1])
		}
	}
}

// TestPlacement_ExistingDimensionsKeepFlattening is the preservation
// half: ADR 0093's flattening contract for every OTHER dimension is not
// redesigned by the rule above.
func TestPlacement_ExistingDimensionsKeepFlattening(t *testing.T) {
	for _, in := range []string{
		"NOT extension:png",
		"cat OR extension:png",
		"NOT (tag:a OR extension:png)",
		"extension:png OR extension:jpg",
	} {
		f := compileFilters(t, in)
		if len(f.Extensions) == 0 {
			t.Errorf("%q: extension term vanished; flattening for existing dimensions must be unchanged", in)
		}
	}
}

func TestSerializeTerm_NewDimensionsRenderTyped(t *testing.T) {
	if got := dsl.SerializeTerm(dsl.Field("preview"), "missing"); got != "preview:missing" {
		t.Errorf("preview term = %q", got)
	}
	if got := dsl.SerializeTerm(dsl.Field("id"), vA); got != "id:"+vA {
		t.Errorf("id term = %q", got)
	}
}

// TestIDCardinality_ByteArithmetic is the reason the bound is 50,
// asserted rather than recorded: the dimension's own canonical form at
// the bound fits the parser's cap with room, and the alias fits in less.
func TestIDCardinality_ByteArithmetic(t *testing.T) {
	ids := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		ids = append(ids, "0f0e6c1a-4444-4a5b-8c7d-"+strings.Repeat("0", 10)+string(rune('0'+i/10))+string(rune('0'+i%10)))
	}
	canonical := "id:" + strings.Join(ids, " AND id:")
	if len(canonical) != 2195 {
		t.Errorf("50-id canonical form is %d bytes, the ADR says 2,195", len(canonical))
	}
	alias := "!list" + strings.Join(ids, ",")
	if len(alias) != 1854 {
		t.Errorf("50-id alias is %d bytes, the ADR says 1,854", len(alias))
	}
	if len(canonical) >= dsl.MaxInputBytes {
		t.Fatalf("the canonical form at the bound (%d) does not fit MaxInputBytes (%d)", len(canonical), dsl.MaxInputBytes)
	}
	if _, err := dsl.Parse(canonical); err != nil {
		t.Errorf("the 50-id canonical form does not parse: %v", err)
	}
	// 100 would not: the bound is where the arithmetic says it is.
	if hundred := 100*39 + 99*5; hundred <= dsl.MaxInputBytes {
		t.Errorf("100 ids would fit (%d bytes); the bound's reasoning no longer holds", hundred)
	}
}
