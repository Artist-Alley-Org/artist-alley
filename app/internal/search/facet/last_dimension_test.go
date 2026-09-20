// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25b: the `last` dimension at the facet layer: vocabulary,
// the single-value bound at every entry path, the fail-closed second
// gates, the ordering fact, and the shape of the rendered window.
//
// No database. Written over the string form of the dimension
// (`FacetType("last")`) so the file compiles on the commit before this
// sprint and goes red there for the right reason: an unknown dimension
// is refused by ParseSelection and unsatisfiable in SQL. The behaviour
// on rows is search's last_window_test.go.

package facet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const dimLast = FacetType("last")

func TestLastValue_Vocabulary(t *testing.T) {
	for _, c := range []struct {
		in   string
		ok   bool
		want string
	}{
		{"1", true, "1"}, {"5", true, "5"}, {"10000", true, "10000"}, {"05", true, "5"}, {" 7 ", true, "7"},
		{"0", false, ""}, {"10001", false, ""}, {"", false, ""}, {"-1", false, ""}, {"abc", false, ""},
		{"5x", false, ""}, {"+5", false, ""}, {"1e2", false, ""},
	} {
		got, ok := dimLast.CanonicalValue(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("last CanonicalValue(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
	if _, ok := ParseFacetType("last"); !ok {
		t.Error("ParseFacetType(\"last\") unknown")
	}
	for _, bad := range []string{"last:0", "last:10001", "last:abc", "last:"} {
		if _, err := ParseSelection([]string{bad}); !errors.Is(err, ErrBadFilter) {
			t.Errorf("filter=%s: err = %v, want ErrBadFilter", bad, err)
		}
	}
	sel, err := ParseSelection([]string{"last:05"})
	if err != nil {
		t.Fatalf("filter=last:05 refused: %v", err)
	}
	if n, ok := sel.RecentWindow(); !ok || n != 5 {
		t.Errorf("RecentWindow() = (%d, %v), want (5, true)", n, ok)
	}
	if _, ok := (Selection{}).RecentWindow(); ok {
		t.Error("an empty selection reports a window")
	}
}

// TestLast_IsSingleValuedOnEveryEntryPath: two DISTINCT windows are one
// refusal through `filter=`, through With (the programmatic path, at
// Validate and again at SQL), while a repeated identical value collapses.
func TestLast_IsSingleValuedOnEveryEntryPath(t *testing.T) {
	_, err := ParseSelection([]string{"last:3", "last:5"})
	if !errors.Is(err, ErrLastNotSingle) || !errors.Is(err, ErrBadFilter) {
		t.Errorf("filter=last:3&filter=last:5: err = %v, want ErrLastNotSingle wrapping ErrBadFilter", err)
	}
	sel, err := ParseSelection([]string{"last:5", "last:05", "last:5"})
	if err != nil || len(sel.Terms()) != 1 {
		t.Errorf("repeated identical windows: err %v, %d terms; want one term", err, len(sel.Terms()))
	}
	two := Selection{}.With(dimLast, "3").With(dimLast, "5")
	if err := two.Validate(); !errors.Is(err, ErrLastNotSingle) {
		t.Errorf("Validate over two With()'d windows: %v", err)
	}
	if _, _, ok := two.SQL(visibility.EntityAsset, "a", 0, ipRC()); ok {
		t.Error("SQL rendered a two-window selection; it must be unsatisfiable")
	}
}

// TestLast_FailsClosedWithoutArms: a site that supplies no window arms
// gets nothing under `last:N`, on every entity, rather than an unbounded
// set. This is the RenderContext contract every other caller-dependent
// dimension follows.
func TestLast_FailsClosedWithoutArms(t *testing.T) {
	sel := Selection{}.With(dimLast, "3")
	for _, e := range []visibility.EntityType{visibility.EntityAsset, visibility.EntityCollection, visibility.EntityPost} {
		if _, _, ok := sel.SQL(e, "x", 0, ipRC()); ok {
			t.Errorf("%s: last:3 rendered with no arms; must be unsatisfiable", e)
		}
	}
	// And an entity the window was not formed over is outside it.
	arms := []RecentArm{{Entity: visibility.EntityAsset, Alias: "a", Where: " AND (a.deleted_at IS NULL)"}}
	rc := ipRC()
	rc.RecentArms = arms
	if _, _, ok := sel.SQL(visibility.EntityPost, "posts", 0, rc); ok {
		t.Error("a post rendered inside a window formed over assets alone")
	}
	if _, _, ok := sel.SQL(visibility.EntityAsset, "assets", 0, rc); !ok {
		t.Error("an asset is unsatisfiable inside a window formed over assets")
	}
}

// TestLast_OrderingFact: `type ASC` is `rank DESC`, the one fact the
// window, the keyset, the merge and the cut are all derived from, and
// the clocks are the ones the brief names.
func TestLast_OrderingFact(t *testing.T) {
	order := []visibility.EntityType{visibility.EntityAsset, visibility.EntityCollection, visibility.EntityPost}
	for i := 1; i < len(order); i++ {
		if order[i-1].String() >= order[i].String() {
			t.Fatalf("hit type strings are not ascending: %s, %s", order[i-1], order[i])
		}
		if RecentRank(order[i-1]) <= RecentRank(order[i]) {
			t.Errorf("RecentRank(%s)=%d must exceed RecentRank(%s)=%d so type ASC is rank DESC",
				order[i-1], RecentRank(order[i-1]), order[i], RecentRank(order[i]))
		}
	}
	if RecentClock(visibility.EntityAsset) != "created_at" || RecentClock(visibility.EntityCollection) != "created_at" ||
		RecentClock(visibility.EntityPost) != "posted_at" {
		t.Error("clocks: assets and collections rank on created_at, posts on posted_at")
	}
}

// TestLast_PredicateShape reads the rendered predicate for the parts it
// must be made of: the entity's own clock, id and rank compared at-or-
// before a cutoff taken from the union of the supplied arms, each arm
// carrying its already-bound baseline verbatim, ordered and limited by
// the bound N.
func TestLast_PredicateShape(t *testing.T) {
	sel := Selection{}.With(dimLast, "3")
	rc := ipRC()
	rc.RecentArms = []RecentArm{
		{Entity: visibility.EntityAsset, Alias: "a", Where: " AND (a.deleted_at IS NULL) AND (ASSETBASE $2)"},
		{Entity: visibility.EntityCollection, Alias: "c", Where: " AND (COLLBASE $3)"},
		{Entity: visibility.EntityPost, Alias: "p", Where: " AND (POSTBASE $4)"},
	}
	frag, args, ok := sel.SQL(visibility.EntityPost, "posts", 4, rc)
	if !ok {
		t.Fatal("unsatisfiable")
	}
	if len(args) != 1 || args[0] != "3" {
		t.Errorf("args = %v, want the one bound window", args)
	}
	for _, want := range []string{
		"ROW(posts.posted_at, posts.id, 0) >= (",
		"a.created_at AS ts, a.id AS id, 2 AS rank FROM assets a WHERE TRUE AND (a.deleted_at IS NULL) AND (ASSETBASE $2)",
		"c.created_at AS ts, c.id AS id, 1 AS rank FROM collections c WHERE TRUE AND (COLLBASE $3)",
		"p.posted_at AS ts, p.id AS id, 0 AS rank FROM posts p WHERE TRUE AND (POSTBASE $4)",
		"UNION ALL",
		"ORDER BY u.ts DESC, u.id DESC, u.rank DESC",
		"LIMIT ($5::TEXT)::BIGINT",
		"ORDER BY w.ts ASC, w.id ASC, w.rank ASC",
		"LIMIT 1)",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment lacks %q:\n%s", want, frag)
		}
	}
	// The asset arm reads its own clock and rank.
	afrag, _, ok := sel.SQL(visibility.EntityAsset, "assets", 4, rc)
	if !ok || !strings.Contains(afrag, "ROW(assets.created_at, assets.id, 2) >= (") {
		t.Errorf("asset arm: %v\n%s", ok, afrag)
	}
}

// TestLast_ArmsAreTheAggregatorsBaselines: RecentArms renders each
// entity from the same functions the counts are made of, for a caller
// class where every conjunct is non-empty, and binds their args in arm
// order from offset+1.
func TestLast_ArmsAreTheAggregatorsBaselines(t *testing.T) {
	ref := int64(25040001)
	req := Request{Caller: visibility.NewCaller(&ref)}
	all := []visibility.EntityType{visibility.EntityAsset, visibility.EntityCollection, visibility.EntityPost}
	arms, args, err := RecentArms(context.Background(), req, all, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(arms) != 3 {
		t.Fatalf("%d arms", len(arms))
	}
	assetFrag, assetArgs, err := buildAssetVisibilityAppendedSQL(context.Background(), req.Caller, req.Caps, req.MutationCaps, req.Mature, 4)
	if err != nil {
		t.Fatal(err)
	}
	if arms[0].Where != assetFrag || arms[0].Alias != "a" {
		t.Errorf("asset arm is not buildAssetVisibilityAppendedSQL's fragment:\n%s\nvs\n%s", arms[0].Where, assetFrag)
	}
	collFrag, collArgs, err := buildCollectionVisibilitySQL(context.Background(), req.Caller, req.Caps.Checker(), "c", 4+len(assetArgs))
	if err != nil {
		t.Fatal(err)
	}
	if arms[1].Where != collFrag || !strings.Contains(collFrag, "c.deleted_at IS NULL") {
		t.Errorf("collection arm:\n%s\nvs\n%s", arms[1].Where, collFrag)
	}
	postFrag, postArgs, err := buildPostVisibilityAppendedSQL(context.Background(), req.Caller, req.PostCaps, false, req.Mature, "p", 4+len(assetArgs)+len(collArgs))
	if err != nil {
		t.Fatal(err)
	}
	if arms[2].Where != postFrag {
		t.Errorf("post arm:\n%s\nvs\n%s", arms[2].Where, postFrag)
	}
	if len(args) != len(assetArgs)+len(collArgs)+len(postArgs) {
		t.Errorf("%d args, want %d", len(args), len(assetArgs)+len(collArgs)+len(postArgs))
	}
	// A subset of types forms a subset of arms.
	only, _, err := RecentArms(context.Background(), req, []visibility.EntityType{visibility.EntityPost}, 0)
	if err != nil || len(only) != 1 || only[0].Entity != visibility.EntityPost {
		t.Errorf("post-only arms: %v %v", only, err)
	}
}

// TestLast_NotInTheRail: filter-only, like preview and id.
func TestLast_NotInTheRail(t *testing.T) {
	for _, ft := range AllFacets() {
		if ft == dimLast {
			t.Error("last is in AllFacets; it is a window, not a bucket list")
		}
	}
	// And it narrows every other facet's population (ForFacet keeps it).
	sel := Selection{}.With(dimLast, "3").With(FacetExtension, "png")
	kept := sel.ForFacet(FacetExtension)
	if _, ok := kept.RecentWindow(); !ok {
		t.Error("ForFacet(extension) dropped the window; the rail must count inside it")
	}
}
