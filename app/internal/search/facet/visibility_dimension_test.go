// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1251 SLICE 2 — THE `visibility` DIMENSION'S PARSER AND ITS RENDER
// CONTRACT.
//
// No database, for the reason selection_test.go's header gives: these
// are properties of the grammar. The DB-backed half — that this
// vocabulary is exactly the column's own CHECK constraint, and that a
// tier NARROWS rather than widens on a live feed — lives in
// posts.TestVisibilityTiers_MatchTheColumnConstraint and
// posts.TestVisibilityFilter_NarrowsNeverWidens, because both need rows.
//
// The properties here are the three a tier dimension can get wrong:
//
//   - a value outside the vocabulary is REFUSED rather than rendered as
//     a predicate that matches nothing;
//   - two tiers are the UNION, because a row is in exactly one tier and
//     AND would be a filter that looks applied and is not;
//   - an ASSET is UNSATISFIABLE, because it has no tier — and
//     "unsupported" has to mean zero rows, never "no constraint".

package facet

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// TestVisibilityDimension_UnknownTierRejected pins the fail-closed
// reading of the value grammar.
//
// There is no `::UUID` cast here to raise a 22P02, so a tolerated junk
// tier would not error — it would render `visibility = 'orgonly'`, match
// no row, and hand an EMPTY page to a caller who asked to narrow. A 400
// at the parser is the difference between a mistake the client can see
// and one that looks like missing data.
func TestVisibilityDimension_UnknownTierRejected(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"visibility:junk",
		"visibility:",
		"visibility:orgonly",  // the hyphen is part of the tier
		"visibility:org_only", // and so is its spelling
		"visibility:publics",
		"visibility:restricted", // a SENSITIVITY value — the other vocabulary
		"visibility:team",       // ditto
	} {
		if _, err := ParseSelection([]string{bad}); err == nil {
			t.Errorf("ParseSelection(%q) was accepted; a tier outside the "+
				"vocabulary must be a 400, not a predicate matching nothing", bad)
		}
	}

	for _, tier := range VisibilityTiers() {
		sel, err := ParseSelection([]string{"visibility:" + tier})
		if err != nil {
			t.Errorf("ParseSelection(visibility:%s): %v", tier, err)
			continue
		}
		if got := sel.Params(); len(got) != 1 || got[0] != "visibility:"+tier {
			t.Errorf("visibility:%s round-tripped as %v", tier, got)
		}
	}

	// Case and whitespace fold, so a hand-typed token and a control's
	// token share one cache key. ⚠️ The OPPOSITE of `tag`, one dimension
	// over: a tier is an enum this repository authored, a tag is user
	// text whose bytes are its identity (migration 00050).
	for _, spelled := range []string{"visibility:PUBLIC", "visibility: Org-Only ", "visibility:pUbLiC"} {
		sel, err := ParseSelection([]string{spelled})
		if err != nil {
			t.Errorf("ParseSelection(%q): %v", spelled, err)
			continue
		}
		if got := sel.Params()[0]; got != strings.ToLower(strings.TrimSpace(
			strings.TrimPrefix(spelled, "visibility:"))) && got != "visibility:"+
			strings.ToLower(strings.TrimSpace(strings.TrimPrefix(spelled, "visibility:"))) {
			t.Errorf("%q canonicalised to %q", spelled, got)
		}
	}

	// The wire name has no alias, unlike `tag`/`tags` and
	// `extension`/`ext`. Asserted so a future alias is a decision rather
	// than an accident.
	if _, ok := ParseFacetType("visibility"); !ok {
		t.Error("ParseFacetType does not know `visibility`")
	}
	if _, ok := ParseFacetType("visibilities"); ok {
		t.Error("ParseFacetType accepted `visibilities` — an unannounced alias")
	}
}

// TestVisibilityDimension_TwoTiersAreTheUnion pins the plural meaning,
// which is OR.
//
// A post is in exactly ONE tier, so AND would be unsatisfiable — the
// same reason `extension` and `sensitivity` are non-conjunctive. It is
// also what the feed's default requires: since #1193 the signed-in
// default is the UNION of four shared tiers, and it reaches SQL as four
// terms of this dimension.
//
// The assertion is on the rendered JOINER rather than on a count,
// because that is the byte a conjunctive() slip would change.
func TestVisibilityDimension_TwoTiersAreTheUnion(t *testing.T) {
	t.Parallel()

	if FacetVisibility.conjunctive() {
		t.Fatal("FacetVisibility is conjunctive — two tiers would AND, and a row " +
			"is in exactly one tier, so the filter would return nothing forever")
	}

	sel := Selection{}.
		With(FacetVisibility, VisibilityPublic).
		With(FacetVisibility, VisibilityOrgOnly)
	frag, args, ok := sel.SQL(visibility.EntityPost, "posts", 0, RenderContext{})
	if !ok {
		t.Fatal("a post is unsatisfiable for `visibility:` — it has the column")
	}
	if len(args) != 2 {
		t.Fatalf("two tiers bound %d args, want 2", len(args))
	}
	if !strings.Contains(frag, " OR ") {
		t.Errorf("two tiers rendered without an OR:\n%s", frag)
	}
	if strings.Contains(frag, "posts.visibility = $1::TEXT AND") {
		t.Errorf("two tiers rendered as an AND:\n%s", frag)
	}

	// A double-tick is the same constraint, not a squared one.
	dup := Selection{}.
		With(FacetVisibility, VisibilityPublic).
		With(FacetVisibility, VisibilityPublic)
	if _, dupArgs, _ := dup.SQL(visibility.EntityPost, "posts", 0, RenderContext{}); len(dupArgs) != 1 {
		t.Errorf("a repeated tier bound %d args, want 1", len(dupArgs))
	}
}

// TestVisibilityDimension_EntityArms pins which entities can answer, and
// — more importantly — that the one that cannot is UNSATISFIABLE rather
// than unconstrained.
//
// Posts and collections carry the SAME five-tier column with the same
// CHECK constraint, so both answer. An ASSET's axis is `sensitivity`, a
// different four-value vocabulary, so it falls through — and the
// fall-through has to mean "this entity matches nothing", because
// reading it as "no constraint" would return every asset on the instance
// to a caller who asked to narrow.
//
// ⚠️ This makes `visibility` the FIRST dimension an asset cannot satisfy,
// which is what makes facet.buildAssetPopulationSQL's unsatisfiable
// branch reachable at last — see the note there.
func TestVisibilityDimension_EntityArms(t *testing.T) {
	t.Parallel()

	sel := Selection{}.With(FacetVisibility, VisibilityPublic)

	for _, e := range []struct {
		entity visibility.EntityType
		alias  string
		want   bool
	}{
		{visibility.EntityPost, "posts", true},
		{visibility.EntityCollection, "c", true},
		{visibility.EntityAsset, "assets", false},
	} {
		frag, args, ok := sel.SQL(e.entity, e.alias, 0, RenderContext{})
		if ok != e.want {
			t.Errorf("%v satisfiable=%v, want %v", e.entity, ok, e.want)
			continue
		}
		if !e.want {
			if frag != "" || len(args) != 0 {
				t.Errorf("%v is unsatisfiable but rendered %q with %d args — an "+
					"unsatisfiable entity must contribute NOTHING, and its caller "+
					"must skip it entirely", e.entity, frag, len(args))
			}
			continue
		}
		if !strings.Contains(frag, e.alias+".visibility = $1::TEXT") {
			t.Errorf("%v rendered %q, want a plain equality on %s.visibility",
				e.entity, frag, e.alias)
		}
		// No LOWER() on the column: the value is already one of five
		// literals this package wrote, and lowering the column would give
		// up collections_visibility_idx.
		if strings.Contains(frag, "LOWER("+e.alias+".visibility)") {
			t.Errorf("%v lowered the COLUMN: %q", e.entity, frag)
		}
	}
}

// TestVisibilityDimension_NeedsNoRenderContext records the property that
// kept this slice from growing plumbing it did not need.
//
// [FacetKind] takes a caller because a post's kind is decided PER MEMBER
// and a member the caller may not read must contribute none. A tier is a
// column on the row itself, so its predicate is caller-INDEPENDENT — and
// the zero-value hazard RenderContext exists to prevent (a forgotten
// context reading as "user zero", which is WIDER than anonymous) cannot
// arise for it.
//
// Asserted rather than assumed, because "this dimension ignores rc" is
// exactly the kind of claim that stops being true quietly.
func TestVisibilityDimension_NeedsNoRenderContext(t *testing.T) {
	t.Parallel()

	sel := Selection{}.With(FacetVisibility, VisibilityPrivate)
	bare, _, okBare := sel.SQL(visibility.EntityPost, "posts", 0, RenderContext{})
	ref := int64(7)
	full, _, okFull := sel.SQL(visibility.EntityPost, "posts", 0, RenderContext{
		Caller:    visibility.NewCaller(&ref),
		CallerArg: "$9",
	})
	if !okBare || !okFull {
		t.Fatalf("satisfiability moved with the render context: bare=%v full=%v",
			okBare, okFull)
	}
	if bare != full {
		t.Errorf("the tier predicate changed with the caller:\n bare: %s\n full: %s\n"+
			"It reads one column on the row and must not consult the caller — the "+
			"caller's rights are a SEPARATE conjunct, ANDed on by the execution site",
			bare, full)
	}
}

// TestVisibilityDimension_ComposesAcrossDimensions pins that a tier
// narrows BESIDE another dimension rather than replacing or widening it.
//
// #1165's defect was terms of one dimension ORing where they should have
// ANDed, and the browse feed sends `visibility` on every request — so it
// is present in combination with every other filter the feed has. A
// cross-dimension slip here would be invisible in any single-filter test.
func TestVisibilityDimension_ComposesAcrossDimensions(t *testing.T) {
	t.Parallel()

	sel := Selection{}.
		With(FacetVisibility, VisibilityPublic).
		With(FacetVisibility, VisibilityOrgOnly).
		With(FacetTag, "alpha").
		With(FacetTag, "beta")
	frag, args, ok := sel.SQL(visibility.EntityPost, "posts", 0, RenderContext{})
	if !ok {
		t.Fatal("post unsatisfiable for visibility+tag")
	}
	if len(args) != 4 {
		t.Fatalf("bound %d args, want 4", len(args))
	}
	// Two dimensions → two ANDed groups. Within them: tiers OR, tags AND.
	if strings.Count(frag, " AND (") < 2 {
		t.Errorf("the two dimensions were not ANDed as separate groups:\n%s", frag)
	}
	if !strings.Contains(frag, "$1::TEXT OR posts.visibility = $2::TEXT") {
		t.Errorf("the tiers did not OR:\n%s", frag)
	}
	if !strings.Contains(frag, "::TEXT) AND EXISTS") {
		t.Errorf("the tags did not AND:\n%s", frag)
	}
}
