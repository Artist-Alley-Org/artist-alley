// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1165 — THE `field:` VALUE GRAMMAR AND ITS OPERATOR.
//
// These tests need no database, which is the point: the properties they
// hold down are properties of the PARSER, and the parser is the thing
// #1165 asks to fail closed. A grammar bug that reached SQL would be
// caught by field_filter_test.go's fixtures — but only on a developer
// machine with AA_DB_PASSWORD set, and only for the operators someone
// remembered to seed data for. These run everywhere, every time.
//
// The assertion that matters most is [TestFieldOperator_UnknownFails].
// An unknown operator must be REFUSED, not quietly read as equality and
// not quietly dropped. A dropped predicate renders a result set that
// looks narrowed and is not, which is the defect the whole `filter=`
// parameter exists to prevent, and it is worse than an error because
// nobody sees it.

package facet

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// TestFieldOperator_ParsesAndRoundTrips is the wire contract: every
// operator survives ParseSelection → Params unchanged, so the token the
// advanced page put in the URL is the token a bookmark, a shared link, a
// saved search or a federated peer replays.
func TestFieldOperator_ParsesAndRoundTrips(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want string // Params() output; empty means "same as in"
	}{
		{"equality", "field:material=steel", ""},
		{"equality with = in the value", "field:formula=a=b", ""},
		{"contains", "field:notes~urgent", ""},
		{"contains with spaces in the value", "field:notes~two words", ""},
		{"contains with an = in the value", "field:notes~a=b", ""},
		// A bound canonicalises to RFC3339 UTC, so the URL shows the
		// instant that will actually run rather than a date whose
		// meaning depends on which server read it.
		{"lower bound, date only", "field:expires>=2026-01-31",
			"field:expires>=2026-01-31T00:00:00Z"},
		// ⚠️ THE UPPER BOUND OF A DATE IS THE END OF THAT DAY. Reading
		// `<=2026-01-31` literally would drop every row stamped later
		// than midnight — 23h59m of matches silently missing.
		{"upper bound, date only", "field:expires<=2026-01-31",
			"field:expires<=2026-01-31T23:59:59.999999Z"},
		{"lower bound, RFC3339", "field:expires>=2026-01-31T09:30:00Z", ""},
		// A zoned instant normalises to UTC: the same moment written two
		// ways must produce ONE cache key and one predicate.
		{"bound in another zone normalises", "field:expires>=2026-01-31T09:30:00+02:00",
			"field:expires>=2026-01-31T07:30:00Z"},
		// The code half keeps the tolerance it has always had.
		{"code is trimmed and lowered", "field: Material =steel", "field:material=steel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sel, err := ParseSelection([]string{c.in})
			if err != nil {
				t.Fatalf("ParseSelection(%q) = %v, want it to parse", c.in, err)
			}
			want := c.want
			if want == "" {
				want = c.in
			}
			got := sel.Params()
			if len(got) != 1 || got[0] != want {
				t.Errorf("round-trip of %q = %v, want [%q]", c.in, got, want)
			}
		})
	}
}

// TestFieldOperator_UnknownFails is #1165's headline acceptance.
//
// Every input here is REFUSED at parse time, which [http.Handler] maps
// to 400 invalid_filter. The failure being tested is not "it errors" but
// the two alternatives it rules out: degrading to equality (answering a
// different question than was asked) and dropping the term (returning an
// unfiltered page that looks filtered).
func TestFieldOperator_UnknownFails(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
	}{
		// Operators we deliberately do NOT define. `>` and `<` are
		// EXCLUSIVE bounds; silently treating them as inclusive would be
		// an off-by-one the caller cannot see, so they are refused
		// rather than approximated.
		{"exclusive greater-than", "field:expires>2026-01-31"},
		{"exclusive less-than", "field:expires<2026-01-31"},
		{"not-equal", "field:material!=steel"},
		{"starts-with", "field:notes^=draft"},
		// No operator at all — the shape that predates #1157's grammar.
		{"no operator", "field:material"},
		{"empty value", "field:material="},
		{"empty value after contains", "field:notes~"},
		{"empty code", "field:=steel"},
		// A code that is not a slug cannot name a field definition, and
		// admitting it would put caller text where a code belongs.
		{"code is not a slug", "field:mat erial=steel"},
		{"code with a dot", "field:mat.erial=steel"},
		// A bound whose value is not a timestamp would reach a
		// ::TIMESTAMPTZ cast and raise 22P02 mid-query — a 500 on a
		// caller mistake, which is what canonicalBound exists to stop.
		{"bound is not a date", "field:expires>=soon"},
		{"bound is a malformed date", "field:expires>=2026-13-45"},
		// ⭐ `field:expires<=42` USED TO BE ON THIS LIST and is now
		// valid, which is #1173's whole point: a bound is temporal or
		// NUMERIC, and which one is a property of the value's spelling.
		// It moved to TestOrderedBound_NumericIsALexicallyValidBound,
		// with the schema-aware half — whether a field DECLARED as a
		// date may be compared to 42 — refused in Selection.Authorize
		// against a real row, because that answer needs one.
		//
		// The VALUE-DOMAIN rejections replace it here, because they are
		// still knowable without a schema. Each is a spelling
		// strconv.ParseFloat accepts and no column can be compared
		// against: `value_num >= 'NaN'` is false for every row including
		// the ones holding NaN, which is a filter that looks applied and
		// is not.
		{"bound is NaN", "field:width>=NaN"},
		{"bound is Inf", "field:width>=Inf"},
		{"bound is -Infinity", "field:width<=-Infinity"},
		{"bound is a number with a unit", "field:width>=1920px"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sel, err := ParseSelection([]string{c.in})
			if err == nil {
				t.Fatalf("ParseSelection(%q) accepted it and produced %v — an unknown "+
					"or malformed operator must fail CLOSED at parse time, never "+
					"degrade to equality and never drop the predicate", c.in, sel.Params())
			}
			if !strings.Contains(err.Error(), "filter") {
				t.Errorf("ParseSelection(%q) = %v, want ErrBadFilter", c.in, err)
			}
		})
	}
}

// TestFieldOperator_UnknownIsUnsatisfiableInSQL is the SECOND gate.
//
// [Selection.With] is exported and takes no error, so a programmatic
// caller — SelectionFromDSL, the saved-search executor, a future stored
// query — can seed a term ParseSelection never saw. Such a term must
// make the entity UNSATISFIABLE (zero rows, zero count), never render a
// fragment that omits its predicate.
//
// `satisfiable == false` is the assertion, not the absence of an error:
// returning ("", nil, true) would be a selection that matches EVERYTHING,
// which is precisely the failure this whole file guards.
func TestFieldOperator_UnknownIsUnsatisfiableInSQL(t *testing.T) {
	for _, bad := range []string{
		"material", // no operator
		"expires>2026-01-31",
		"material!=steel",
		"expires>=not-a-date",
	} {
		t.Run(bad, func(t *testing.T) {
			sel := Selection{}.With(FacetField, bad)
			frag, args, satisfiable := sel.SQL(visibility.EntityAsset, "assets", 0, RenderContext{})
			if satisfiable {
				t.Errorf("SQL for a malformed term %q reported satisfiable, "+
					"fragment=%q args=%v — a term the parser would have refused "+
					"must match nothing, not match everything", bad, frag, args)
			}
		})
	}
}

// TestFieldOperator_TermsCombine is the grouping rule (#1165), and it is
// the half a single-filter test cannot see.
//
// `field:` is a FAMILY of dimensions collapsed into one FacetType, so
// the plain "one dimension, OR its values" rule is wrong for it: it ORed
// terms naming DIFFERENT fields, which made a second filter WIDEN the
// result set. The operator turns that from surprising into unshippable,
// because the two bounds of a range are two terms on ONE field and ORed
// they match every row that has a date at all.
//
// Asserting on the rendered SQL rather than on row counts is deliberate:
// the shape is the contract, it holds for every entity and every
// fixture, and it needs no database.
func TestFieldOperator_TermsCombine(t *testing.T) {
	for _, c := range []struct {
		name       string
		terms      []string
		wantGroups int // AND-ed sub-groups at the top level of the dimension
		why        string
	}{
		{
			name:       "two values of one field, one operator, OR",
			terms:      []string{"material=steel", "material=brass"},
			wantGroups: 1,
			why:        "\"material is steel or brass\" is one question with two answers",
		},
		{
			name:       "two different fields AND",
			terms:      []string{"material=steel", "stage=lookdev"},
			wantGroups: 2,
			why:        "two rows of a form narrow each other; ORing them WIDENS on the second tick",
		},
		{
			name:       "the two bounds of a range AND",
			terms:      []string{"expires>=2026-01-01", "expires<=2026-06-30"},
			wantGroups: 2,
			why:        "ORed, a range matches every row that has a date at all",
		},
		{
			name:       "contains beside a bound ANDs",
			terms:      []string{"notes~draft", "expires>=2026-01-01"},
			wantGroups: 2,
			why:        "different operators on one field are different constraints",
		},
		{
			name:       "two contains on one field OR",
			terms:      []string{"notes~draft", "notes~wip"},
			wantGroups: 1,
			why:        "same field, same operator — two acceptable answers to one question",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			sel := Selection{}
			for _, term := range c.terms {
				sel = sel.With(FacetField, term)
			}
			frag, args, satisfiable := sel.SQL(visibility.EntityAsset, "assets", 0, RenderContext{})
			if !satisfiable {
				t.Fatalf("selection %v was unsatisfiable", c.terms)
			}
			if len(args) != len(c.terms) {
				t.Fatalf("got %d args for %d terms — the one-placeholder-per-term "+
					"contract the whole filter path rests on is broken",
					len(args), len(c.terms))
			}
			if got := topLevelGroups(t, frag); got != c.wantGroups {
				t.Errorf("selection %v rendered %d AND-ed group(s), want %d — %s\nSQL: %s",
					c.terms, got, c.wantGroups, c.why, frag)
			}
		})
	}
}

// topLevelGroups counts the parenthesised sub-groups a dimension's
// fragment ANDs together.
//
// It walks paren depth rather than counting a substring, because every
// term's own SQL contains `AND (` inside its EXISTS body and a textual
// count reads those as group separators — which is exactly the false
// negative this helper was written to remove after the first draft of
// [TestFieldOperator_TermsCombine] reported correct SQL as broken.
func topLevelGroups(t *testing.T, frag string) int {
	t.Helper()
	const prefix = " AND ("
	if !strings.HasPrefix(frag, prefix) || !strings.HasSuffix(frag, ")") {
		t.Fatalf("fragment does not have the documented ' AND (…)' shape: %q", frag)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(frag, prefix), ")")
	depth, groups := 0, 0
	for _, r := range body {
		switch r {
		case '(':
			if depth == 0 {
				groups++
			}
			depth++
		case ')':
			depth--
		}
	}
	return groups
}

// TestFieldOperator_ReadsTheRightColumn pins each operator to the
// storage column its field type actually uses.
//
// A date bound that also asked value_text would match a text field's
// rows through a cast, and — the reason this is a test and not a comment
// — it would give up the partial index `(field_id, value_date)` that
// makes a range on a large corpus affordable at all.
func TestFieldOperator_ReadsTheRightColumn(t *testing.T) {
	for _, c := range []struct {
		name    string
		term    string
		want    []string
		absent  []string
		because string
	}{
		{
			name:    "equality asks both text columns",
			term:    "material=steel",
			want:    []string{"ffv.value_text", "ffv.value_options"},
			absent:  []string{"value_date"},
			because: "a select writes value_text and a multi_select writes value_options",
		},
		{
			name:   "contains asks both text columns, case-insensitively",
			term:   "notes~urgent",
			want:   []string{"strpos(LOWER(ffv.value_text)", "unnest(COALESCE(ffv.value_options"},
			absent: []string{"value_date", "ILIKE"},
			because: "strpos has no metacharacters, so the caller's text needs no " +
				"escaping and no future edit can drop one",
		},
		{
			name:    "a lower bound asks value_date alone",
			term:    "expires>=2026-01-01",
			want:    []string{"ffv.value_date >="},
			absent:  []string{"value_text", "value_options"},
			because: "a range over text is not a narrower question, it is a meaningless one",
		},
		{
			name:    "an upper bound asks value_date alone",
			term:    "expires<=2026-01-01",
			want:    []string{"ffv.value_date <="},
			absent:  []string{"value_text", "value_options"},
			because: "same, and it keeps the (field_id, value_date) index usable",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			frag, _, satisfiable := Selection{}.With(FacetField, c.term).
				SQL(visibility.EntityAsset, "assets", 0, RenderContext{})
			if !satisfiable {
				t.Fatalf("term %q was unsatisfiable", c.term)
			}
			for _, w := range c.want {
				if !strings.Contains(frag, w) {
					t.Errorf("term %q rendered SQL without %q — %s\nSQL: %s",
						c.term, w, c.because, frag)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(frag, a) {
					t.Errorf("term %q rendered SQL containing %q, which it must not — %s\nSQL: %s",
						c.term, a, c.because, frag)
				}
			}
		})
	}
}

// TestFieldOperator_SplitIsTotal checks the code/operator/value split
// against values that contain operator characters.
//
// The split scans for the FIRST character from [fieldOpChars] because no
// field code may contain one. Everything after the matched operator is
// value, including further operator characters — so a caller searching
// for the literal text "a>=b" gets that, not a parse error.
func TestFieldOperator_SplitIsTotal(t *testing.T) {
	for _, c := range []struct {
		in    string
		code  string
		op    FieldOp
		value string
	}{
		{"material=steel", "material", FieldOpEq, "steel"},
		{"formula=a=b", "formula", FieldOpEq, "a=b"},
		{"formula=a>=b", "formula", FieldOpEq, "a>=b"},
		{"notes~a~b", "notes", FieldOpContains, "a~b"},
		{"notes~a=b", "notes", FieldOpContains, "a=b"},
		{"expires>=2026-01-01", "expires", FieldOpAtLeast, "2026-01-01"},
		{"expires<=2026-01-01", "expires", FieldOpAtMost, "2026-01-01"},
		// `>=` is matched before `>`, so this is never read as a bare
		// `>` followed by a value starting `=`.
		{"expires>=2026-01-01T00:00:00Z", "expires", FieldOpAtLeast, "2026-01-01T00:00:00Z"},
	} {
		t.Run(c.in, func(t *testing.T) {
			code, op, value, ok := SplitFieldTerm(c.in)
			if !ok {
				t.Fatalf("SplitFieldTerm(%q) refused it", c.in)
			}
			if code != c.code || op != c.op || value != c.value {
				t.Errorf("SplitFieldTerm(%q) = (%q, %q, %q), want (%q, %q, %q)",
					c.in, code, op, value, c.code, c.op, c.value)
			}
		})
	}
}

// TestFieldOperator_CacheKeyDistinguishesOperators is the correctness
// half of [Selection.CacheKey], in the direction that fails BOTH ways.
//
// `notes~draft` and `notes=draft` are different result sets. If they
// shared a key the first caller's page would be served to the second for
// the rest of the TTL — and the same holds for the two ends of a range,
// which differ only by their operator character.
func TestFieldOperator_CacheKeyDistinguishesOperators(t *testing.T) {
	keys := map[string]string{}
	for _, term := range []string{
		"notes=draft", "notes~draft",
		"expires>=2026-01-01T00:00:00Z", "expires<=2026-01-01T00:00:00Z",
	} {
		k := Selection{}.With(FacetField, term).CacheKey()
		if prev, clash := keys[k]; clash {
			t.Errorf("%q and %q share cache key %q — two different result sets "+
				"would be served from one entry", prev, term, k)
		}
		keys[k] = term
	}
}
