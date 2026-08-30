// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1368 — the SELECTION half of "one grammar, one saved query".
//
// ⛔ THESE ARE NOT FAIL-BEFORE TESTS, and saying so is part of the
// sprint's own acceptance. Every case here names a member that does not
// exist on `dev` (`SelectionToDSL`, `ComposeDSL`, the plural
// `dsl.Filters` slices), so none of them could be run against the old
// behaviour even in principle. They guard the new REPRESENTATION.
//
// The user-visible defect — a saved search replaying wider than the
// search it was saved from — is guarded by a browser user flow that runs
// unchanged against both, in
// scripts/dogfood/ui/tests/standalone/saved-search-filters-1368.spec.ts.
//
// Pure Go, no database: every claim below is about the representation,
// and a claim about the POPULATION belongs where the population is —
// facet_filter_test.go for the interactive path, the browser flow for
// the saved one.
package search

import (
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
)

// sel builds a Selection from `dimension:value` wire tokens, through the
// SAME parser the rail's `filter=` parameter uses. Deliberately not
// facet.Selection.With: a test that hand-built terms would skip
// CanonicalValue and could assert a round trip for a value the product
// cannot actually produce.
func sel(t *testing.T, tokens ...string) facet.Selection {
	t.Helper()
	s, err := facet.ParseSelection(tokens)
	if err != nil {
		t.Fatalf("ParseSelection(%v): %v", tokens, err)
	}
	return s
}

// roundTrip drives Selection → canonical DSL → lexer → parser → compiler
// → Selection. This is the contract, and it is one function so no test
// can accidentally assert a shorter version of it.
func roundTrip(t *testing.T, s facet.Selection) (facet.Selection, string) {
	t.Helper()
	text, err := SelectionToDSL(s)
	if err != nil {
		t.Fatalf("SelectionToDSL(%v): %v", s.Params(), err)
	}
	parsed, err := dsl.Parse(text)
	if err != nil {
		t.Fatalf("the canonical DSL %q does not parse: %v", text, err)
	}
	compiled, err := dsl.Compile(parsed)
	if err != nil {
		t.Fatalf("the canonical DSL %q does not compile: %v", text, err)
	}
	out, err := SelectionFromDSL(compiled.Filters, facet.Selection{})
	if err != nil {
		t.Fatalf("SelectionFromDSL after %q: %v", text, err)
	}
	return out, text
}

// assertSameSelection compares by CacheKey, which is sorted and
// length-prefixed per term — so it is set equality over (dimension,
// value) pairs and cannot be satisfied by a differently-ordered or
// colon-ambiguous near-miss.
func assertSameSelection(t *testing.T, want, got facet.Selection, via string) {
	t.Helper()
	if want.CacheKey() == got.CacheKey() {
		return
	}
	t.Errorf("round trip changed the selection\n  via  %s\n  want %v\n  got  %v",
		via, want.Params(), got.Params())
}

// TestRoundTrip_EverySavableDimension — the six dimensions a savable
// interactive search can carry, one term each.
//
// The list is the savable surface traced from PRODUCERS (the rail's
// facet.AllFacets, plus the advanced page's `field:` and `type:`), not
// the facet registry. See dslFieldForFacet.
func TestRoundTrip_EverySavableDimension(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"tag", "tag:sketch"},
		{"asset_type", "asset_type:Image"},
		{"sensitivity", "sensitivity:public"},
		{"owner", "owner:alice"},
		{"extension", "extension:png"},
		{"field equality", "field:color_space=sRGB"},
		// #1173 — the seventh, and the first whose VALUE is a bound.
		{"file_size", "file_size:>=12345"},
		// #1173 sprint 18c — the eighth, and the first whose value is
		// another row's NATURAL KEY. It carries a `:` inside the value,
		// so its canonical spelling is QUOTED; that is decided by the
		// lexer, not by this list.
		{"workflow_state", "workflow_state:asset:1/published"},
		{"workflow_state none", "workflow_state:none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sel(t, tc.token)
			out, text := roundTrip(t, in)
			assertSameSelection(t, in, out, text)
		})
	}
}

// TestRoundTrip_MultiplicitySurvives is the compiler defect this sprint
// closes, per dimension.
//
// ⛔ THE ASSERTION IS ON THE TERM COUNT AS WELL AS THE SET, because the
// bug being guarded is a LOSS: `Extension = m.Value` kept the last term
// and dropped the first, and a set comparison against a one-term
// expectation would have passed on exactly that.
func TestRoundTrip_MultiplicitySurvives(t *testing.T) {
	cases := []struct {
		name   string
		tokens []string
	}{
		// The four that collapsed. Each was a plain assignment in
		// walkFieldMatch, so the FIRST value vanished silently.
		{"two extensions", []string{"extension:png", "extension:jpg"}},
		{"two sensitivities", []string{"sensitivity:public", "sensitivity:team"}},
		{"two asset types", []string{"asset_type:Image", "asset_type:Video"}},
		{"two owners", []string{"owner:alice", "owner:bob"}},
		// tag was already a slice; it is here so a future "simplify the
		// four into one loop" cannot fold the conjunctive one in with them.
		{"two tags", []string{"tag:sketch", "tag:lowpoly"}},
		// The field family's three N≥2 shapes (ADR 0093's 2026-08-20
		// amendment): same code + same operator OR, same code + different
		// operators AND, different codes AND.
		{"one field code, two values", []string{"field:color_space=sRGB", "field:color_space=AdobeRGB"}},
		{"one field code, two operators", []string{"field:licence_expires>=2026-01-01", "field:licence_expires<=2026-06-30"}},
		{"two field codes", []string{"field:color_space=sRGB", "field:version=v2"}},
		// #1173 — the ordered dimension's two N≥2 shapes. Both bounds
		// have to SURVIVE as two terms, because the grouping rule that
		// makes them an intersection is applied downstream in
		// facet.subGroupKey and has nothing to work with if the
		// compiler collapsed them.
		{"two size bounds, opposite operators", []string{"file_size:>=100", "file_size:<=900"}},
		{"two size bounds, same operator", []string{"file_size:>=100", "file_size:>=200"}},
		// Every dimension at once, which is the shape a real saved search has.
		{"mixed", []string{
			"tag:sketch", "tag:lowpoly", "extension:png", "extension:jpg",
			"asset_type:Image", "owner:alice", "sensitivity:public",
			"field:color_space=sRGB", "field:version~v2",
			"file_size:>=100", "file_size:<=900",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sel(t, tc.tokens...)
			if len(in.Terms()) != len(tc.tokens) {
				t.Fatalf("premise failed: %d tokens produced %d terms", len(tc.tokens), len(in.Terms()))
			}
			out, text := roundTrip(t, in)
			if got := len(out.Terms()); got != len(tc.tokens) {
				t.Errorf("%d terms went in and %d came back — a term was dropped\n  via %s\n  got %v",
					len(tc.tokens), got, text, out.Params())
			}
			assertSameSelection(t, in, out, text)
		})
	}
}

// TestRoundTrip_FieldOperators covers the three operator shapes #1165
// shipped, since a `field:` value travels through the DSL opaquely and
// an operator character is exactly the kind of byte a naive serializer
// would mangle.
//
// ⭐ The date bound also proves the canonicalisation claim: the wire
// value `<=2026-06-30` becomes the LAST MICROSECOND of that day on both
// paths, so the stored DSL names the same instant the rail did rather
// than midnight — a difference of 23h59m of matches.
func TestRoundTrip_FieldOperators(t *testing.T) {
	in := sel(t,
		"field:color_space=sRGB",
		"field:credit~Blossom",
		"field:licence_expires>=2026-01-01",
		"field:licence_expires<=2026-06-30",
	)
	out, text := roundTrip(t, in)
	assertSameSelection(t, in, out, text)

	var upper string
	for _, term := range out.Terms() {
		if term.Type != facet.FacetField {
			continue
		}
		if code, op, value, ok := facet.SplitFieldTerm(term.Value); ok &&
			code == "licence_expires" && op == facet.FieldOpAtMost {
			upper = value
		}
	}
	if !strings.HasPrefix(upper, "2026-06-30T23:59:59") {
		t.Errorf("the upper date bound survived as %q, want the end of 2026-06-30", upper)
	}
}

// TestRoundTrip_QuotedValues — the values a bare token cannot carry,
// including as an opaque `field:` value, which is the case a serializer
// that only quoted the dimension's own values would miss.
func TestRoundTrip_QuotedValues(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"plain", "tag:sketch"},
		{"whitespace", "tag:two words"},
		{"colon", "tag:ns:value"},
		// ⚠️ LOWERCASE. The lexer upper-cases before matching keywords, so
		// this is the spelling that actually exercises the keyword arm.
		{"reads as or", "tag:or"},
		{"reads as and", "tag:and"},
		{"reads as not", "tag:not"},
		{"embedded quote", `tag:say "hi"`},
		{"parens", "tag:a(b)c"},
		// #1368's losslessness hole.
		{"trailing backslash", `tag:abc\`},
		{"backslash before quote", `tag:abc\"def`},
		{"windows path", `tag:C:\art\ref`},
		// The same shapes as an opaque field value.
		{"field value with a space", "field:credit=Kenneth Blossom"},
		{"field value with a colon", "field:note=see: page 4"},
		{"field value that reads as or", "field:status=or"},
		{"field value with a quote", `field:note=he said "no"`},
		{"field value with a backslash", `field:path=C:\art\`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sel(t, tc.token)
			out, text := roundTrip(t, in)
			assertSameSelection(t, in, out, text)
		})
	}
}

// TestRoundTrip_WorkflowStateIdentities — #1173 sprint 18c.
//
// ⭐ IT IS THE FIRST NON-`field:` DIMENSION WHOSE CANONICAL SPELLING IS
// QUOTED. An asset domain is `asset:<ref>`, so the value carries a `:`,
// which terminates the lexer's word run — [dsl.Serialize] finds that out
// by lexing its own candidate token and emits
// `workflow_state:"asset:1/published"` with no rule added anywhere.
//
// ⛔ AND THE UNKNOWN IDENTITY IS PART OF THE CONTRACT. A saved query
// naming a state an operator later deletes must stay REPRESENTABLE: it
// returns zero rows (asserted against real rows in
// workflow_state_filter_test.go), and it must never become unparseable.
func TestRoundTrip_WorkflowStateIdentities(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"an ordinary identity", "workflow_state:asset:1/published"},
		{"the reserved literal", "workflow_state:none"},
		{"a non-asset domain", "workflow_state:post/published"},
		{"an unknown but well-formed identity", "workflow_state:asset:9/never_defined"},
		// The free-text code shapes #897 permits. Each is a value a bare
		// token cannot carry, and none of them needs a rule of its own.
		{"a code containing a further slash", "workflow_state:asset:1/stage/final"},
		{"a code with whitespace", "workflow_state:asset:1/awaiting art director"},
		{"a code that reads as or", "workflow_state:asset:1/or"},
		{"a code with a quote", `workflow_state:asset:1/say "when"`},
		{"a code with a backslash", `workflow_state:asset:1/C:\art\ref`},
		{"a code ending in a backslash", `workflow_state:asset:1/trailing\`},
		{"a code with parens", "workflow_state:asset:1/stage(2)"},
		{"mixed case, preserved", "workflow_state:asset:1/Pending_Review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sel(t, tc.token)
			out, text := roundTrip(t, in)
			assertSameSelection(t, in, out, text)
		})
	}
}

// TestRoundTrip_WorkflowStateMultiplicitySurvives — two identities are
// two terms, and the compiler must not collapse them.
//
// ⛔ The count assertion is the point: an asset holds exactly one state,
// so a compiler that kept only the last value would return a STRICT
// SUBSET and a set comparison against a one-term expectation would pass
// on exactly that. #1368 found four dimensions doing it.
func TestRoundTrip_WorkflowStateMultiplicitySurvives(t *testing.T) {
	for _, tokens := range [][]string{
		{"workflow_state:asset:1/draft", "workflow_state:asset:1/published"},
		{"workflow_state:asset:1/draft", "workflow_state:none"},
		{"workflow_state:asset:1/draft", "workflow_state:post/published", "workflow_state:none"},
		{"workflow_state:asset:1/published", "extension:png", "tag:sketch"},
	} {
		in := sel(t, tokens...)
		if len(in.Terms()) != len(tokens) {
			t.Fatalf("premise failed: %d tokens produced %d terms", len(tokens), len(in.Terms()))
		}
		out, text := roundTrip(t, in)
		if got := len(out.Terms()); got != len(tokens) {
			t.Errorf("%d terms went in and %d came back — a term was dropped\n  via %s\n  got %v",
				len(tokens), got, text, out.Params())
		}
		assertSameSelection(t, in, out, text)
	}
}

// TestSelectionFromDSL_RefusesAMalformedWorkflowIdentity — the
// fail-closed direction on the way IN, for the dimension's own grammar.
//
// A hand-typed `workflow_state:published` has no domain. On the
// `filter=` path that is a 400; on the DSL path it must be a DSLError
// rather than a term that renders a predicate nobody can satisfy.
func TestSelectionFromDSL_RefusesAMalformedWorkflowIdentity(t *testing.T) {
	for _, text := range []string{
		`workflow_state:published`,
		`workflow_state:"/published"`,
		`workflow_state:"asset:1/"`,
	} {
		parsed, err := dsl.Parse(text)
		if err != nil {
			t.Fatalf("premise failed: %q does not parse (%v); the refusal under test is "+
				"the VALUE's, not the grammar's", text, err)
		}
		compiled, err := dsl.Compile(parsed)
		if err != nil {
			t.Fatalf("premise failed: %q does not compile (%v)", text, err)
		}
		if _, err := SelectionFromDSL(compiled.Filters, facet.Selection{}); err == nil {
			t.Errorf("SelectionFromDSL accepted %q — a concrete identity is "+
				"<domain>/<code> with both halves non-empty", text)
		}
	}
}

// TestSelectionToDSL_IsDeterministic — the same SET of terms yields the
// same string no matter what order it was ticked in, so a reload and a
// re-save do not rewrite the stored query.
func TestSelectionToDSL_IsDeterministic(t *testing.T) {
	forward := sel(t, "tag:sketch", "extension:png", "owner:alice", "field:color_space=sRGB")
	backward := sel(t, "field:color_space=sRGB", "owner:alice", "extension:png", "tag:sketch")

	a, err := SelectionToDSL(forward)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SelectionToDSL(backward)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("tick order changed the canonical DSL:\n  %q\n  %q", a, b)
	}
	for i := 0; i < 5; i++ {
		again, err := SelectionToDSL(forward)
		if err != nil {
			t.Fatal(err)
		}
		if again != a {
			t.Fatalf("SelectionToDSL is not stable: %q then %q", a, again)
		}
	}
}

// TestComposeDSL_PreservesTheExpression is the precedence hazard at the
// level the save path actually uses.
//
// ⛔ The OR case is the discriminator. A serializer that appends without
// wrapping produces a string that parses, compiles and carries the right
// filter set — it is only the MEANING of the saved expression that
// changes, and only for a top-level disjunction.
func TestComposeDSL_PreservesTheExpression(t *testing.T) {
	filters := sel(t, "extension:png")

	t.Run("top-level OR stays one operand", func(t *testing.T) {
		out, err := ComposeDSL("cat OR dog", filters)
		if err != nil {
			t.Fatal(err)
		}
		q, err := dsl.Parse(out)
		if err != nil {
			t.Fatalf("%q does not parse: %v", out, err)
		}
		and, ok := q.Root.(dsl.AndNode)
		if !ok {
			t.Fatalf("%q parsed as %T; the OR swallowed the filter", out, q.Root)
		}
		if _, ok := and.Left.(dsl.OrNode); !ok {
			t.Errorf("%q: left operand is %T, want the whole saved expression", out, and.Left)
		}
	})

	t.Run("an already-parenthesised expression still parses", func(t *testing.T) {
		out, err := ComposeDSL("(cat OR dog) AND bird", filters)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dsl.Parse(out); err != nil {
			t.Fatalf("%q does not parse: %v", out, err)
		}
	})

	t.Run("a phrase survives as a phrase", func(t *testing.T) {
		out, err := ComposeDSL(`"a b c"`, filters)
		if err != nil {
			t.Fatal(err)
		}
		q, err := dsl.Parse(out)
		if err != nil {
			t.Fatalf("%q does not parse: %v", out, err)
		}
		and, ok := q.Root.(dsl.AndNode)
		if !ok {
			t.Fatalf("%q parsed as %T", out, q.Root)
		}
		ph, ok := and.Left.(dsl.PhraseNode)
		if !ok || ph.Text != "a b c" {
			t.Errorf("%q: left operand is %#v, want PhraseNode{a b c}", out, and.Left)
		}
	})

	t.Run("similar_to survives", func(t *testing.T) {
		const id = "0192abcd-1234-5678-9abc-def012345678"
		out, err := ComposeDSL("similar_to:"+id, filters)
		if err != nil {
			t.Fatal(err)
		}
		q, err := dsl.Parse(out)
		if err != nil {
			t.Fatalf("%q does not parse: %v", out, err)
		}
		compiled, err := dsl.Compile(q)
		if err != nil {
			t.Fatal(err)
		}
		if compiled.SimilarToAssetID != id {
			t.Errorf("%q: SimilarToAssetID = %q, want the anchor", out, compiled.SimilarToAssetID)
		}
	})

	t.Run("N=0 is byte-for-byte", func(t *testing.T) {
		for _, expr := range []string{"cat", "cat OR dog", `"a b c"`, "(x AND y) OR z"} {
			out, err := ComposeDSL(expr, facet.Selection{})
			if err != nil {
				t.Fatal(err)
			}
			if out != expr {
				t.Errorf("with no filters, %q was rewritten to %q", expr, out)
			}
		}
	})

	t.Run("a filter-only search composes to the filters alone", func(t *testing.T) {
		out, err := ComposeDSL("", filters)
		if err != nil {
			t.Fatal(err)
		}
		if out != "extension:png" {
			t.Errorf("ComposeDSL(\"\", extension:png) = %q", out)
		}
	})
}

// TestComposeDSL_FreeTextIsTheExpressionAlone.
//
// ⛔ THIS IS THE HALF THE ISSUE'S DATA-FLOW TRACE MISSED, and without it
// the count equality the sprint is measured by is unreachable. Nothing
// executes CompiledQuery.TSQuery; what runs is plainto_tsquery over
// Query.Text, and both DSL callers used to set that to the WHOLE DSL
// STRING. A canonical saved query would therefore have searched the text
// for the words "extension" and "png".
func TestComposeDSL_FreeTextIsTheExpressionAlone(t *testing.T) {
	cases := []struct {
		expr    string
		tokens  []string
		want    string
		comment string
	}{
		{"cat", []string{"extension:png"}, "cat", "the filter is not a word to search for"},
		{"cat dog", []string{"tag:sketch"}, "cat dog", "both words survive"},
		{"cat OR dog", []string{"extension:png"}, "cat dog",
			"OR is a stop word to Postgres either way, so the lexemes are unchanged"},
		{`"a b c"`, []string{"tag:sketch"}, "a b c", "a phrase contributes its text"},
		{"", []string{"extension:png"}, "", "a filter-only search has no text at all"},
		{"cat AND tag:sketch", nil, "cat", "a typed field term is a filter, not text"},
	}
	for _, tc := range cases {
		out, err := ComposeDSL(tc.expr, sel(t, tc.tokens...))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := dsl.Parse(out)
		if err != nil {
			t.Fatalf("%q does not parse: %v", out, err)
		}
		compiled, err := dsl.Compile(parsed)
		if err != nil {
			t.Fatal(err)
		}
		if compiled.FreeText != tc.want {
			t.Errorf("FreeText of %q = %q, want %q (%s)", out, compiled.FreeText, tc.want, tc.comment)
		}
	}
}

// TestSelectionToDSL_RefusesWhatItCannotSpell.
//
// ⛔ A DROPPED TERM IS THE BUG. `ai`, `kind`, `collection` and
// `visibility` are registered dimensions with no DSL spelling and no
// savable producer; if one ever reaches the serializer the save must
// FAIL rather than persist a query that is wider than the page it came
// from, which is #1368 with a different dimension.
func TestSelectionToDSL_RefusesWhatItCannotSpell(t *testing.T) {
	for _, token := range []string{
		"ai:pure",
		"kind:image",
		"visibility:public",
		"collection:0192abcd-1234-4567-89ab-cdef01234567",
	} {
		s, err := facet.ParseSelection([]string{token})
		if err != nil {
			t.Fatalf("premise failed: %q is no longer a parseable filter (%v); if the "+
				"dimension was retired, drop it from this list", token, err)
		}
		if _, err := SelectionToDSL(s); !errors.Is(err, ErrDimensionNotRepresentable) {
			t.Errorf("SelectionToDSL(%q) err = %v, want ErrDimensionNotRepresentable", token, err)
		}
	}
}

// TestSelectionFromDSL_CanonicalisesFieldValues — the fail-closed
// direction on the way IN.
//
// A hand-typed `field:` date bound reaches facet.dimensionSQL and is
// spliced into a ::TIMESTAMPTZ cast. Unvalidated, a malformed one is a
// Postgres 22P02 raised mid-query — a 500 on a typo. This is the check
// the `filter=` path has had since #1165 and the DSL path did not.
func TestSelectionFromDSL_CanonicalisesFieldValues(t *testing.T) {
	t.Run("a bad bound is rejected", func(t *testing.T) {
		parsed, err := dsl.Parse(`field:licence_expires>=notadate`)
		if err != nil {
			t.Fatal(err)
		}
		compiled, err := dsl.Compile(parsed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SelectionFromDSL(compiled.Filters, facet.Selection{}); err == nil {
			t.Error("a malformed date bound reached the selection; it would raise 22P02 in SQL")
		}
	})

	t.Run("a date-only bound is normalised the same way the rail normalises it", func(t *testing.T) {
		parsed, err := dsl.Parse(`field:licence_expires<=2026-06-30`)
		if err != nil {
			t.Fatal(err)
		}
		compiled, err := dsl.Compile(parsed)
		if err != nil {
			t.Fatal(err)
		}
		fromDSL, err := SelectionFromDSL(compiled.Filters, facet.Selection{})
		if err != nil {
			t.Fatal(err)
		}
		fromRail := sel(t, "field:licence_expires<=2026-06-30")
		assertSameSelection(t, fromRail, fromDSL, "field:licence_expires<=2026-06-30")
	})
}

// TestSelectionFromDSL_ComposesOntoTheRail keeps the property #907
// established: a `dsl=` and a `filter=` on the same request narrow
// together rather than one replacing the other.
func TestSelectionFromDSL_ComposesOntoTheRail(t *testing.T) {
	rail := sel(t, "extension:png")
	parsed, err := dsl.Parse("tag:sketch")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := dsl.Compile(parsed)
	if err != nil {
		t.Fatal(err)
	}
	both, err := SelectionFromDSL(compiled.Filters, rail)
	if err != nil {
		t.Fatal(err)
	}
	want := sel(t, "extension:png", "tag:sketch")
	assertSameSelection(t, want, both, "dsl=tag:sketch over filter=extension:png")
}
