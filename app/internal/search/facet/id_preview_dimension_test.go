// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25a: the `preview` and `id` dimensions at the facet
// layer: vocabulary, cardinality, the fail-closed second gates and the
// shape of the rendered predicate.
//
// These need no database. The properties they hold down are properties
// of the PARSER and the RENDERER: which values are legal, how many, and
// what the predicate is built from. The behaviour of the predicate on
// rows is search's preview_dimension_test.go and id_dimension_test.go.
//
// ⛔ Written over the string forms of the dimensions (`FacetType("id")`)
// rather than the new constants, so the file compiles on the commit
// before this sprint and its assertions go red there for the right
// reason: an unknown dimension is refused by ParseSelection and is
// unsatisfiable in SQL.

package facet

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	dimPreview = FacetType("preview")
	dimID      = FacetType("id")
	ipA        = "0f0e6c1a-1111-4a5b-8c7d-000000000001"
)

func ipIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, uuid.NewSHA1(uuid.NameSpaceOID, []byte{byte(i), byte(i >> 8)}).String())
	}
	return out
}

func ipFilters(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "id:"+id)
	}
	return out
}

func ipRC() RenderContext {
	return RenderContext{Caller: visibility.NewCaller(nil), CallerArg: "0"}
}

func TestPreviewVocabulary_OneValue(t *testing.T) {
	if PreviewMissing != dsl.PreviewMissing {
		t.Fatalf("the facet layer spells the value %q and the parser %q", PreviewMissing, dsl.PreviewMissing)
	}
	for _, c := range []struct {
		in   string
		ok   bool
		want string
	}{
		{"missing", true, "missing"},
		{" Missing ", true, "missing"},
		{"present", false, ""},
		{"", false, ""},
		{"none", false, ""},
		{"true", false, ""},
	} {
		got, ok := dimPreview.CanonicalValue(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("preview CanonicalValue(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
	if _, err := ParseSelection([]string{"preview:present"}); !errors.Is(err, ErrBadFilter) {
		t.Errorf("filter=preview:present: err = %v, want ErrBadFilter; there is no `present` value", err)
	}
	if _, err := ParseSelection([]string{"preview:missing"}); err != nil {
		t.Errorf("filter=preview:missing refused: %v", err)
	}
}

func TestIDValue_CanonicalisesLikeCollection(t *testing.T) {
	for _, in := range []string{ipA, strings.ToUpper(ipA), "{" + ipA + "}", strings.ReplaceAll(ipA, "-", ""), " " + ipA + " "} {
		got, ok := dimID.CanonicalValue(in)
		if !ok || got != ipA {
			t.Errorf("id CanonicalValue(%q) = (%q, %v), want (%q, true)", in, got, ok, ipA)
		}
	}
	for _, in := range []string{"", "junk", ipA + "x", "0f0e6c1a-1111-4a5b-8c7d"} {
		if _, ok := dimID.CanonicalValue(in); ok {
			t.Errorf("id CanonicalValue(%q) accepted a non-UUID", in)
		}
	}
	if _, ok := ParseFacetType("id"); !ok {
		t.Error("ParseFacetType(\"id\") unknown")
	}
	if _, ok := ParseFacetType("preview"); !ok {
		t.Error("ParseFacetType(\"preview\") unknown")
	}
}

// TestIDCardinality_Boundaries is the bound at all three numbers the
// brief names, on the `filter=` path, plus the collapse case.
func TestIDCardinality_Boundaries(t *testing.T) {
	for _, n := range []int{1, 49, 50} {
		sel, err := ParseSelection(ipFilters(ipIDs(n)))
		if err != nil {
			t.Errorf("%d distinct ids refused: %v", n, err)
		}
		if got := len(sel.Terms()); got != n {
			t.Errorf("%d ids parsed to %d terms", n, got)
		}
	}
	_, err := ParseSelection(ipFilters(ipIDs(51)))
	if err == nil {
		t.Fatal("51 distinct ids accepted; the canonical stored form could not replay")
	}
	if !errors.Is(err, ErrTooManyIDs) || !errors.Is(err, ErrBadFilter) {
		t.Errorf("51 ids: err = %v; want ErrTooManyIDs wrapping ErrBadFilter so the handler's 400 mapping holds", err)
	}
	// 52 raw entries, two of them repeats and one of those in a second
	// spelling: 50 distinct, accepted.
	ids := ipIDs(50)
	raw := append(append([]string{}, ids...), strings.ToUpper(ids[0]), "{"+ids[1]+"}")
	sel, err := ParseSelection(ipFilters(raw))
	if err != nil {
		t.Fatalf("52 raw entries collapsing to 50 distinct refused: %v", err)
	}
	if got := len(sel.Terms()); got != 50 {
		t.Errorf("collapsed to %d terms, want 50", got)
	}
	// ⛔ The bound is on DISTINCT ids: 51 distinct plus a duplicate is
	// still 51.
	if _, err := ParseSelection(ipFilters(append(ipIDs(51), ids[0]))); err == nil {
		t.Error("51 distinct ids plus a duplicate accepted")
	}
}

// TestIDCardinality_SecondGateInSQL pins the fail-closed direction for a
// selection built programmatically past the parsers.
func TestIDCardinality_SecondGateInSQL(t *testing.T) {
	var sel Selection
	for _, id := range ipIDs(51) {
		sel = sel.With(dimID, id)
	}
	if err := sel.Validate(); !errors.Is(err, ErrTooManyIDs) {
		t.Errorf("Validate over 51 With()'d ids: %v", err)
	}
	if _, _, ok := sel.SQL(visibility.EntityAsset, "a", 0, ipRC()); ok {
		t.Error("SQL rendered a 51-id selection; a selection the stored form could not replay must be unsatisfiable")
	}
	var fifty Selection
	for _, id := range ipIDs(50) {
		fifty = fifty.With(dimID, id)
	}
	frag, args, ok := fifty.SQL(visibility.EntityAsset, "a", 0, ipRC())
	if !ok {
		t.Fatal("a 50-id selection is unsatisfiable")
	}
	if len(args) != 50 || strings.Count(frag, "a.id = $") != 50 {
		t.Errorf("50 ids rendered %d args and %d comparisons", len(args), strings.Count(frag, "a.id = $"))
	}
	if !strings.Contains(frag, ") OR (") && !strings.Contains(frag, " OR ") {
		t.Errorf("id values must OR; fragment: %s", frag)
	}
}

func TestIDDimension_EveryEntityAnswersOnItsOwnID(t *testing.T) {
	sel := Selection{}.With(dimID, ipA)
	for _, c := range []struct {
		e     visibility.EntityType
		alias string
	}{
		{visibility.EntityAsset, "assets"},
		{visibility.EntityPost, "posts"},
		{visibility.EntityCollection, "c"},
	} {
		frag, args, ok := sel.SQL(c.e, c.alias, 0, RenderContext{})
		if !ok {
			t.Errorf("%s: id: unsatisfiable; every entity has an id", c.e)
			continue
		}
		if !strings.Contains(frag, c.alias+".id = $1::UUID") {
			t.Errorf("%s: fragment does not compare its own id column: %s", c.e, frag)
		}
		if len(args) != 1 || args[0] != ipA {
			t.Errorf("%s: args = %v", c.e, args)
		}
	}
}

// TestPreviewDimension_PredicateShape reads the rendered asset arm for
// the three authorities it must compose and the one it must not.
func TestPreviewDimension_PredicateShape(t *testing.T) {
	sel := Selection{}.With(dimPreview, "missing")

	// Posts and collections cannot answer.
	for _, e := range []visibility.EntityType{visibility.EntityPost, visibility.EntityCollection} {
		if _, _, ok := sel.SQL(e, "x", 0, ipRC()); ok {
			t.Errorf("%s satisfies preview:missing; only an asset has a file", e)
		}
	}

	// ⛔ No caller placeholder: fail CLOSED, as the kind: post arm does.
	if _, _, ok := sel.SQL(visibility.EntityAsset, "a", 0, RenderContext{}); ok {
		t.Error("preview:missing rendered with an empty CallerArg; the zero Caller is wider than anonymous")
	}

	stranger := int64(7)
	rc := RenderContext{Caller: visibility.NewCaller(&stranger), CallerArg: "$9"}
	frag, args, ok := sel.SQL(visibility.EntityAsset, "assets", 0, rc)
	if !ok {
		t.Fatal("preview:missing unsatisfiable for an asset")
	}
	if len(args) != 1 || args[0] != "missing" {
		t.Errorf("args = %v, want the one vocabulary literal", args)
	}
	for _, must := range []string{
		"variant_key = 'col'", // the `preview_available` EXISTS, negated
		"NOT EXISTS",          //
		"regexp_replace(lower(assets.file_extension)",   // dispatch.PreviewableSQL, router normalisation
		"assets.owner_user_ref = NULLIF($9::BIGINT, 0)", // PreviewReadableSQL, on the caller's placeholder
	} {
		if !strings.Contains(frag, must) {
			t.Errorf("predicate lacks %q:\n%s", must, frag)
		}
	}
	// ⛔ NEVER processing_status. A poster writes `col` under `pending`
	// and a fan failure marks a row `ready` with none.
	if strings.Contains(frag, "processing_status") {
		t.Errorf("predicate reads processing_status, which is wrong in both directions:\n%s", frag)
	}
	// ⛔ The PICTURE plane, not the field plane: no mutation disjunct
	// can appear here whatever MutationCaps says.
	team := uuid.New()
	rcHolder := RenderContext{Caller: visibility.NewCaller(&stranger), CallerArg: "$9",
		MutationCaps: visibility.AssetMutationCaps{Teams: []uuid.UUID{team}}}
	fragHolder, _, _ := sel.SQL(visibility.EntityAsset, "assets", 0, rcHolder)
	if strings.Contains(fragHolder, team.String()) || strings.Contains(fragHolder, "team_id IN") {
		t.Errorf("the mutation scope reached the picture plane:\n%s", fragHolder)
	}
	if fragHolder != frag {
		t.Errorf("a mutation holder renders a different preview predicate than a stranger; the plane must ignore mutation scope")
	}
	// system.admin: the plane folds to nothing, the other two conjuncts stay.
	rcAdmin := RenderContext{Caller: visibility.NewCaller(&stranger), CallerArg: "$9",
		Caps: visibility.ContentCaps{SystemAdmin: true}}
	fragAdmin, _, ok := sel.SQL(visibility.EntityAsset, "assets", 0, rcAdmin)
	if !ok || strings.Contains(fragAdmin, "NULLIF") || !strings.Contains(fragAdmin, "variant_key = 'col'") {
		t.Errorf("system.admin predicate wrong:\n%s", fragAdmin)
	}
}

func TestNewDimensions_AreFilterOnly(t *testing.T) {
	for _, ft := range AllFacets() {
		if ft == dimPreview || ft == dimID {
			t.Errorf("%s is in AllFacets; no aggregator was added and none may be", ft)
		}
	}
	if dimPreview.conjunctive() || dimID.conjunctive() {
		t.Error("preview/id must OR their values")
	}
	if dimPreview.ordered() || dimID.ordered() {
		t.Error("preview/id are not ordered dimensions")
	}
}
