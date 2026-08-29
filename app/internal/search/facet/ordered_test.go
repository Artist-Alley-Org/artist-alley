// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 18b — TYPED ORDERED COMPARISON, ORDERED GROUPING, AND
// `file_size`.
//
// What is here is the half that needs no database: the value grammar,
// the canonical identity a cache key is built from, which sub-group a
// term lands in, which column a bound renders against, and which entity
// arms can satisfy a size filter. The half that can only be checked
// against ROWS — the four-population intersection fixture and the
// schema-aware type refusal — lives in search/ordered_filter_test.go,
// because both need `field_definition.type` and `assets.file_size_bytes`
// to be real.
//
// ⛔ THE TWO VALIDITY CLASSES ARE DIFFERENT AND THE TESTS MUST NOT SWAP
// THEM. A malformed bound is knowable without a schema, so it is refused
// by ParseSelection and is a 400. Whether a FIELD may be compared this
// way needs a row, so it is refused by Selection.Authorize and is an
// EMPTY RESULT SET. A test asserting the wrong one looks correct and
// proves nothing, so every assertion below names which class it is in.

package facet

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// TestOrderedBound_NumericIsALexicallyValidBound is the case that MOVED
// off TestFieldOperator_UnknownFails's list.
//
// `field:expires<=42` was refused at parse time because canonicalBound
// tried RFC3339, then `2006-01-02`, then gave up — so `>=` was date-only
// and a numeric field could not be compared at all. It parses now, and
// the question the old test was really asking (may a field DECLARED as a
// date be compared to 42?) moved to the seam that can answer it.
func TestOrderedBound_NumericIsALexicallyValidBound(t *testing.T) {
	for _, token := range []string{
		"field:pixel_width>=1920",
		"field:pixel_width<=42",
		"field:weight>=-3.5",
		"field:ratio<=0.5",
	} {
		if _, err := ParseSelection([]string{token}); err != nil {
			t.Errorf("ParseSelection(%q) = %v, want accepted — a bound is temporal OR\n"+
				"  numeric, decided by the value's own spelling. Refusing here would\n"+
				"  put a schema question (is this field a number?) on a pure path\n"+
				"  that has no schema to ask.", token, err)
		}
	}
}

// TestOrderedBound_TemporalAndNumericAreDisjoint is the premise the
// whole no-schema split rests on, asserted rather than assumed.
//
// If any string parsed as BOTH a date and a number the domain would not
// be a function of the value, and canonical identity — hence the cache
// key — would depend on which parse ran first.
func TestOrderedBound_TemporalAndNumericAreDisjoint(t *testing.T) {
	for _, c := range []struct {
		in   string
		want orderedDomain
		why  string
	}{
		{"2026", domainNumeric, "a bare year has no punctuation, so no date layout matches it"},
		{"2026-01-31", domainTemporal, "the full `2006-01-02` layout"},
		{"2026-01-31T12:00:00Z", domainTemporal, "RFC3339"},
		{"1920", domainNumeric, "an integer"},
		{"1.92e3", domainNumeric, "an exponent form of the same integer"},
		{"-3.5", domainNumeric, "a negative decimal"},
		{"0", domainNumeric, "zero"},
	} {
		t.Run(c.in, func(t *testing.T) {
			_, dom, ok := canonicalBound(c.in, FieldOpAtLeast)
			if !ok {
				t.Fatalf("canonicalBound(%q) refused it; expected %s (%s)", c.in, c.want, c.why)
			}
			if dom != c.want {
				t.Errorf("canonicalBound(%q) landed in domain %v, want %v — %s",
					c.in, dom, c.want, c.why)
			}
		})
	}
}

// String makes the failures above readable.
func (d orderedDomain) String() string {
	switch d {
	case domainTemporal:
		return "temporal"
	case domainNumeric:
		return "numeric"
	case domainBytes:
		return "bytes"
	}
	return "none"
}

// TestOrderedBound_NumericCanonicalIdentity is the CACHE KEY property,
// stated as the property rather than as a notation.
//
//	Two spellings that denote the same float64 produce the SAME canonical
//	term and therefore the SAME CacheKey, and that canonical form reads
//	back to the same float64.
//
// A CacheKey is text. Without this, `1920` and `1.92e3` are two keys for
// one result set, so the same page is computed and stored twice — and
// worse, a re-save of a loaded query would rewrite the stored DSL for no
// reason, which is the churn SelectionToDSL's determinism exists to stop.
func TestOrderedBound_NumericCanonicalIdentity(t *testing.T) {
	spellings := []string{"1920", "1920.0", "1.92e3", "+1920", "1.920000e+03", "0001920"}
	var wantKey, wantTerm string
	for i, sp := range spellings {
		s, err := ParseSelection([]string{"field:pixel_width>=" + sp})
		if err != nil {
			t.Fatalf("ParseSelection(field:pixel_width>=%s): %v", sp, err)
		}
		term := s.Params()[0]
		key := s.CacheKey()
		if i == 0 {
			wantTerm, wantKey = term, key
			continue
		}
		if term != wantTerm {
			t.Errorf("%q canonicalised to %q, but %q canonicalised to %q — two spellings\n"+
				"  of one bound must produce ONE canonical term or the stored DSL churns\n"+
				"  on every re-save.", sp, term, spellings[0], wantTerm)
		}
		if key != wantKey {
			t.Errorf("%q and %q produced different CacheKeys (%q vs %q) — the same\n"+
				"  result set would be computed and cached twice.", sp, spellings[0], key, wantKey)
		}
	}
	// And the canonical form reads back to the value it denotes, so the
	// round trip is exact rather than merely stable.
	if got := wantTerm; got != "field:pixel_width>=1920" {
		t.Errorf("canonical term = %q, want %q — the shortest decimal that reads back\n"+
			"  to the same float64.", got, "field:pixel_width>=1920")
	}
	n, err := strconv.ParseFloat(strings.TrimPrefix(wantTerm, "field:pixel_width>="), 64)
	if err != nil || n != 1920 {
		t.Errorf("the canonical form %q does not read back to 1920 (got %v, %v)", wantTerm, n, err)
	}
}

// TestFileSize_ValueGrammar is the LEXICAL / VALUE-DOMAIN class: pure,
// no schema, rejected in CanonicalValue and surfacing as 400.
func TestFileSize_ValueGrammar(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string // canonical form; empty means "must be refused"
		why  string
	}{
		{">=12345", ">=12345", "the plain lower bound"},
		{"<=12345", "<=12345", "the plain upper bound"},
		{">=0", ">=0", "zero bytes is a real bound"},
		{">=+12345", ">=12345", "a leading plus denotes the same bound and must share its key"},
		{">= 12345", ">=12345", "surrounding whitespace is not part of the number"},
		{">=-1", ">=-1", "a negative bound is well formed; it simply admits every sized row"},

		{"12345", "", "no operator at all — the shape that predates this dimension"},
		{">12345", "", "`>` is EXCLUSIVE and we define inclusive bounds only; " +
			"treating it as `>=` would be an off-by-one the caller cannot see"},
		{"<12345", "", "same, the other way"},
		{"=12345", "", "equality is not an ordering and this dimension defines none"},
		{"~12345", "", "contains is not an ordering either"},
		{">=", "", "an empty value"},
		{">=12.5", "", "a fractional byte does not exist"},
		{">=1MB", "", "units are not part of this grammar; accepting the digits " +
			"would run a filter three orders of magnitude away from the one asked for"},
		{">=1e3", "", "base 10 means base 10 — no exponent form"},
		{">=0x2000", "", "no base prefix"},
		{">=1_000", "", "no digit separators"},
		{">=9223372036854775808", "", "one past int64"},
		{">=-9223372036854775809", "", "one past int64 the other way"},
		{">=abc", "", "not a number at all"},
		{">=NaN", "", "not an integer, and not a bound"},
	} {
		t.Run(c.in, func(t *testing.T) {
			got, ok := FacetFileSize.CanonicalValue(c.in)
			if c.want == "" {
				if ok {
					t.Errorf("file_size:%s was ACCEPTED as %q — %s.\n"+
						"  This class is pure and must be a 400, never a predicate that\n"+
						"  matches nothing under a label promising a narrowing.", c.in, got, c.why)
				}
				return
			}
			if !ok {
				t.Fatalf("file_size:%s was refused; want %q — %s", c.in, c.want, c.why)
			}
			if got != c.want {
				t.Errorf("file_size:%s canonicalised to %q, want %q — %s", c.in, got, c.want, c.why)
			}
		})
	}
}

// TestFileSize_TheWireFormNeedsTheColon pins the spelling, because the
// obvious-looking alternative is unreachable BY CONSTRUCTION and always
// will be.
//
// ParseSelection does strings.Cut(r, ":") to find the dimension, so a
// token with no colon has no dimension. `file_size>=12345` is therefore
// malformed forever — that is a property of the WIRE FORM, not of this
// dimension, and a test that used it as a fail-before would be measuring
// something that can never be fixed.
func TestFileSize_TheWireFormNeedsTheColon(t *testing.T) {
	if _, err := ParseSelection([]string{"file_size:>=12345"}); err != nil {
		t.Errorf("filter=file_size:>=12345 was rejected (%v) — this is the wire spelling", err)
	}
	if _, err := ParseSelection([]string{"file_size>=12345"}); err == nil {
		t.Errorf("filter=file_size>=12345 was ACCEPTED — it carries no colon, so it names\n" +
			"  no dimension. If this ever starts parsing, the dimension separator has\n" +
			"  moved and every other filter token's meaning has changed with it.")
	}
}

// TestFileSize_Int64BeyondFloat53 is why the parse is ParseInt and not
// ParseFloat.
//
// 2^53+1 is the first integer a float64 cannot represent: it rounds to
// 2^53. A byte count parsed through a float would therefore come back as
// a DIFFERENT number than the caller wrote — silently, and only for
// large files, which is exactly where a size filter gets used.
func TestFileSize_Int64BeyondFloat53(t *testing.T) {
	for _, raw := range []string{
		"9007199254740993",    // 2^53 + 1
		"9007199254740995",    // 2^53 + 3
		"9223372036854775807", // math.MaxInt64
	} {
		got, ok := FacetFileSize.CanonicalValue(">=" + raw)
		if !ok {
			t.Fatalf("file_size:>=%s was refused; it is inside int64", raw)
		}
		if got != ">="+raw {
			t.Errorf("file_size:>=%s canonicalised to %q — the digits changed, which means\n"+
				"  the value went through a float64. Beyond 2^53 a float64 cannot tell\n"+
				"  consecutive integers apart, so the bound that runs is not the one\n"+
				"  the caller wrote.", raw, got)
		}
	}
}

// TestFileSize_GroupsByOperator is the COMBINATION rule, on rendered SQL.
//
// ⛔ It is the reason this dimension needed a classification at all. Two
// bounds ORed read "bigger than A or smaller than B", which is every
// asset that has a size — the range that looks like it worked. The
// population proof is search/ordered_filter_test.go; this is the
// structural half, which localises a failure to the grouping pass.
func TestFileSize_GroupsByOperator(t *testing.T) {
	for _, c := range []struct {
		name       string
		terms      []string
		wantGroups int
		why        string
	}{
		{
			name: "one bound is one group", terms: []string{">=100"},
			wantGroups: 1, why: "nothing to combine",
		},
		{
			name:       "two bounds with the SAME operator are one group, ORed",
			terms:      []string{">=100", ">=200"},
			wantGroups: 1,
			why: "a value list: `at least 100 or at least 200` is the looser of the two, " +
				"which is what ticking two lower bounds asks for",
		},
		{
			name:       "two bounds with DIFFERENT operators are two groups, ANDed",
			terms:      []string{">=100", "<=200"},
			wantGroups: 2,
			why: "the intersection. One group here means the bounds ORed, which matches " +
				"every asset with a size at all",
		},
		{
			name:       "three terms, two operators",
			terms:      []string{">=100", ">=200", "<=900"},
			wantGroups: 2,
			why:        "the two lower bounds share a group and the upper bound gets its own",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := Selection{}
			for _, term := range c.terms {
				s = s.With(FacetFileSize, term)
			}
			frag, args, satisfiable := s.SQL(visibility.EntityAsset, "a", 0, RenderContext{})
			if !satisfiable {
				t.Fatalf("terms %v were unsatisfiable on an ASSET", c.terms)
			}
			if len(args) != len(c.terms) {
				t.Fatalf("terms %v bound %d args, want %d — one placeholder per term",
					c.terms, len(args), len(c.terms))
			}
			if got := topLevelGroups(t, frag); got != c.wantGroups {
				t.Errorf("terms %v rendered %d sub-groups, want %d — %s\nSQL: %s",
					c.terms, got, c.wantGroups, c.why, frag)
			}
			// A single group must OR and multiple groups must AND, which
			// is what makes the group COUNT meaningful above.
			if c.wantGroups == 1 && len(c.terms) > 1 && !strings.Contains(frag, " OR ") {
				t.Errorf("terms %v share a sub-group but are not ORed — %s\nSQL: %s",
					c.terms, c.why, frag)
			}
		})
	}
}

// TestFileSize_ReadsTheBigintColumn pins the predicate: the operator is
// spliced from a closed Go constant, the caller's bytes stay in the
// placeholder, and the cast is BIGINT.
func TestFileSize_ReadsTheBigintColumn(t *testing.T) {
	for _, c := range []struct {
		term string
		want string
	}{
		{">=12345", "a.file_size_bytes >= substr($1::TEXT, 3)::BIGINT"},
		{"<=12345", "a.file_size_bytes <= substr($1::TEXT, 3)::BIGINT"},
	} {
		t.Run(c.term, func(t *testing.T) {
			frag, args, satisfiable := Selection{}.With(FacetFileSize, c.term).
				SQL(visibility.EntityAsset, "a", 0, RenderContext{})
			if !satisfiable {
				t.Fatalf("term %q was unsatisfiable on an asset", c.term)
			}
			if !strings.Contains(frag, c.want) {
				t.Errorf("term %q rendered %q, want it to contain %q", c.term, frag, c.want)
			}
			// ⛔ The caller's digits are an ARG, never inline SQL.
			if strings.Contains(frag, "12345") {
				t.Errorf("term %q inlined the caller's value into the SQL: %s", c.term, frag)
			}
			if len(args) != 1 || args[0] != c.term {
				t.Errorf("term %q bound args %v, want exactly [%q]", c.term, args, c.term)
			}
		})
	}
}

// TestFileSize_OnlyAnAssetHasAFile is the CROSS-ARM behaviour, and the
// failure it rules out is the one that makes a filter WIDEN.
//
// A post is a set of members and a collection is a container; neither
// carries a byte count. Both must therefore be UNSATISFIABLE, which
// Selection.SQL returns as satisfiable=false and every call site honours
// by returning nothing for that entity. An arm that treated an active
// narrowing filter as "no constraint" would return every post and every
// collection beside the qualifying assets.
func TestFileSize_OnlyAnAssetHasAFile(t *testing.T) {
	for _, c := range []struct {
		entity visibility.EntityType
		want   bool
	}{
		{visibility.EntityAsset, true},
		{visibility.EntityPost, false},
		{visibility.EntityCollection, false},
	} {
		_, _, satisfiable := Selection{}.With(FacetFileSize, ">=12345").
			SQL(c.entity, "a", 0, RenderContext{})
		if satisfiable != c.want {
			t.Errorf("file_size:>=12345 on %v was satisfiable=%v, want %v.\n"+
				"  For a post or a collection, TRUE would mean the fragment renders\n"+
				"  with no constraint — a filter that makes the result set LARGER.",
				c.entity, satisfiable, c.want)
		}
	}
}

// TestOrderedBound_ColumnFollowsTheDomain is the typed-comparison fix,
// on rendered SQL.
//
// ⛔ Before 18b the column came from the OPERATOR: `>=` meant value_date,
// full stop. The operator says which SIDE of a bound a row must fall on
// and nothing about what kind of quantity is bounded.
func TestOrderedBound_ColumnFollowsTheDomain(t *testing.T) {
	for _, c := range []struct {
		name   string
		term   string
		want   string
		absent []string
	}{
		{
			name: "a temporal bound reads value_date", term: "expires>=2026-01-01",
			want: "ffv.value_date >= ", absent: []string{"value_num", "value_text"},
		},
		{
			name: "a numeric bound reads value_num", term: "pixel_width>=1920",
			want: "ffv.value_num >= ", absent: []string{"value_date", "value_text"},
		},
		{
			name: "a numeric upper bound reads value_num", term: "pixel_width<=1920",
			want: "ffv.value_num <= ", absent: []string{"value_date"},
		},
		{
			name: "a temporal upper bound still reads value_date", term: "expires<=2026-01-01",
			want: "ffv.value_date <= ", absent: []string{"value_num"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := ParseSelection([]string{"field:" + c.term})
			if err != nil {
				t.Fatalf("ParseSelection(field:%s): %v", c.term, err)
			}
			frag, _, satisfiable := s.SQL(visibility.EntityAsset, "a", 0, RenderContext{})
			if !satisfiable {
				t.Fatalf("term %q was unsatisfiable", c.term)
			}
			if !strings.Contains(frag, c.want) {
				t.Errorf("term %q rendered SQL without %q — the column must come from the\n"+
					"  bound's DOMAIN, not from its operator.\nSQL: %s", c.term, c.want, frag)
			}
			for _, a := range c.absent {
				if strings.Contains(frag, a) {
					t.Errorf("term %q rendered SQL containing %q, which it must not.\nSQL: %s",
						c.term, a, frag)
				}
			}
			// The cast has to match the column, or Postgres raises
			// mid-query on a value the parser already accepted.
			wantCast := "::TIMESTAMPTZ"
			if strings.Contains(c.want, "value_num") {
				wantCast = "::DOUBLE PRECISION"
			}
			if !strings.Contains(frag, wantCast) {
				t.Errorf("term %q rendered SQL without the %s cast.\nSQL: %s", c.term, wantCast, frag)
			}
		})
	}
}

// TestOrderedBound_FieldGroupingIsUnchanged is the PRESERVATION half of
// the grouping change.
//
// 18b widened the operator extraction in Selection.SQL from `field:` to
// `field:` plus the ordered dimensions. The failure mode to avoid is
// widening it to ALL dimensions, which would make `extension:png`
// operator-aware and give `tag:` a value grammar it never declared.
func TestOrderedBound_FieldGroupingIsUnchanged(t *testing.T) {
	// Two values of one non-ordered dimension stay ONE group (ORed),
	// exactly as before, even though one of them contains a `>` — which
	// is now an operator character somewhere else in the grammar.
	s := Selection{}.With(FacetExtension, "png").With(FacetExtension, "jpg")
	frag, _, ok := s.SQL(visibility.EntityAsset, "a", 0, RenderContext{})
	if !ok {
		t.Fatalf("two extensions were unsatisfiable")
	}
	if got := topLevelGroups(t, frag); got != 1 {
		t.Errorf("two extension values rendered %d sub-groups, want 1 — a non-ordered\n"+
			"  dimension has exactly one sub-group and always has.\nSQL: %s", got, frag)
	}
	if !strings.Contains(frag, " OR ") {
		t.Errorf("two extension values were not ORed.\nSQL: %s", frag)
	}
	// A value that merely LOOKS like a bound is still an opaque value in
	// a dimension that is not ordered.
	if _, err := ParseSelection([]string{"tag:>=5"}); err != nil {
		t.Errorf("tag:>=5 was rejected (%v) — a tag is opaque text and `>=5` is a legal\n"+
			"  tag. Rejecting it would mean the ordered grammar leaked into a\n"+
			"  dimension that never declared one.", err)
	}
	tagSel, _ := ParseSelection([]string{"tag:>=5", "tag:>=6"})
	tagFrag, _, tagOK := tagSel.SQL(visibility.EntityAsset, "a", 0, RenderContext{})
	if !tagOK {
		t.Fatalf("two bound-shaped tags were unsatisfiable")
	}
	if got := topLevelGroups(t, tagFrag); got != 1 {
		t.Errorf("two bound-shaped TAGS rendered %d sub-groups, want 1 — the operator\n"+
			"  extraction must be gated on the dimension's classification, not on\n"+
			"  whether a value happens to start with `>=`.\nSQL: %s", got, tagFrag)
	}
}

// TestOrderedDimension_ClassificationIsShort holds the list itself down.
//
// A dimension becomes operator-aware by appearing in orderedDomain and
// nowhere else. If a future change makes this list longer without a
// deliberate decision, every dimension added to it silently gains a
// value grammar its CanonicalValue does not implement.
func TestOrderedDimension_ClassificationIsShort(t *testing.T) {
	ordered := map[FacetType]bool{}
	for _, ft := range []FacetType{
		FacetAssetType, FacetTag, FacetSensitivity, FacetOwner, FacetExtension,
		FacetCollection, FacetField, FacetAI, FacetKind, FacetVisibility, FacetFileSize,
	} {
		if ft.ordered() {
			ordered[ft] = true
		}
	}
	if len(ordered) != 1 || !ordered[FacetFileSize] {
		t.Errorf("the ordered dimensions are %v, want exactly {file_size}.\n"+
			"  ⛔ `field:` must NOT be here: its orderedness is a property of each\n"+
			"  field definition's declared TYPE, answered per term against the\n"+
			"  database in Selection.Authorize.", keysOf(ordered))
	}
	if FacetFileSize.orderedDomain() != domainBytes {
		t.Errorf("file_size's domain is %v, want bytes — a float64 cannot represent\n"+
			"  every value of a BIGINT column.", FacetFileSize.orderedDomain())
	}
}

func keysOf(m map[FacetType]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	return out
}
