// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1104 — the owner's reproduction, as a test.
//
// "In collections, it doesn't show any featured collections even though
// we have featured collections on the browse feed."
//
// The two surfaces had picked their audience by hand and picked
// differently: the browse rail read scope='public', the collections
// hub's Featured tab read scope='org', and the only writer produced
// 'org'. So a placement made through the admin UI appeared in the hub
// and never on the rail, and a placement from the seed appeared on the
// rail and never in the hub. This asserts the two surfaces now answer
// the same question, which is the acceptance criterion — and it does it
// across the package boundary on purpose, because that boundary is
// where the disagreement lived and neither package's own tests could
// see it.

package collections

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/featured"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// featuredLadder is the stock preview ladder. ListPublicRail takes the
// CONFIGURED rungs as a parameter (#591) rather than assuming them.
var featuredLadder = []string{"col", "preview", "screen", "hires"}

// placeFeaturedCollection seeds a public collection and features it at
// `scope`, returning its id and name.
func placeFeaturedCollection(t *testing.T, pool *pgxpool.Pool, name, scope string) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, name, owner_user_ref, visibility, membership)
		VALUES ($1,$2,$3,'public','manual')`, id, name, listCollOwner); err != nil {
		t.Fatalf("seed collection %q: %v", name, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO featured_items (subject_kind, subject_id, position, scope)
		VALUES ('collection',$1,800,$2)`, id, scope); err != nil {
		t.Fatalf("place %q at %s: %v", name, scope, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM featured_items WHERE subject_id = $1`, id)
		_, _ = pool.Exec(bg, `DELETE FROM collections WHERE id = $1`, id)
	})
	return id, name
}

// inHubFeaturedTab reports whether the collection shows under the hub's
// Featured tab for this caller.
func inHubFeaturedTab(t *testing.T, pool *pgxpool.Pool, caller visibility.Caller, id uuid.UUID) bool {
	t.Helper()
	yes := true
	rows, err := ListCollectionsPageGated(context.Background(), pool, caller,
		ListCollectionsPageGatedParams{Featured: &yes, RowLimit: 500})
	if err != nil {
		t.Fatalf("hub featured tab: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.ID.Bytes) == id {
			return true
		}
	}
	return false
}

// onBrowseRail reports whether the collection shows on the browse rail
// for this caller.
func onBrowseRail(t *testing.T, pool *pgxpool.Pool, caller visibility.Caller, id uuid.UUID) bool {
	t.Helper()
	rows, err := featured.ListPlacements(context.Background(), pool, featured.PlacementQuery{Caller: caller, Limit: 500, Ladder: featuredLadder})
	if err != nil {
		t.Fatalf("browse rail: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.SubjectID.Bytes) == id {
			return true
		}
	}
	return false
}

// TestFeaturedSurfacesAgree_SignedIn is the owner's reproduction with an
// `org` placement — what POST /admin/featured writes by default, and
// what the rail could never show before #1104.
func TestFeaturedSurfacesAgree_SignedIn(t *testing.T) {
	pool := listCollPool(t)
	id, name := placeFeaturedCollection(t, pool, "1104 org placement", featured.ScopeOrg)
	caller := visibility.NewCaller(collOwnerPtr())

	if !inHubFeaturedTab(t, pool, caller, id) {
		t.Errorf("%q (scope=org) is missing from the hub's Featured tab", name)
	}
	if !onBrowseRail(t, pool, caller, id) {
		t.Errorf("%q (scope=org) is missing from the browse rail for a signed-in viewer. "+
			"This is the mirror half of #1104: everything the admin UI features is org-scoped, "+
			"and the rail asked only for public", name)
	}
}

// TestFeaturedSurfacesAgree_SeedShape is the half the owner actually
// saw: the seed writes `public`, so the hub tab — which asked for `org`
// — was empty on a freshly seeded install while the rail was full.
func TestFeaturedSurfacesAgree_SeedShape(t *testing.T) {
	pool := listCollPool(t)
	id, name := placeFeaturedCollection(t, pool, "1104 public placement", featured.ScopePublic)
	caller := visibility.NewCaller(collOwnerPtr())

	if !inHubFeaturedTab(t, pool, caller, id) {
		t.Errorf("%q (scope=public) is missing from the hub's Featured tab — this is the owner's "+
			"report: the seed writes public placements and the tab asked for org", name)
	}
	if !onBrowseRail(t, pool, caller, id) {
		t.Errorf("%q (scope=public) is missing from the browse rail", name)
	}
}

// TestFeaturedSurfacesAgree_Anonymous pins the arm that must NOT have
// widened. An anonymous viewer sees the `public` placement on both
// surfaces and the `org` one on neither — the same answer the rail gave
// before #1104, now also given by the hub.
func TestFeaturedSurfacesAgree_Anonymous(t *testing.T) {
	pool := listCollPool(t)
	pubID, _ := placeFeaturedCollection(t, pool, "1104 anon public", featured.ScopePublic)
	orgID, _ := placeFeaturedCollection(t, pool, "1104 anon org", featured.ScopeOrg)
	anon := visibility.NewCaller(nil)

	if !inHubFeaturedTab(t, pool, anon, pubID) {
		t.Error("a public placement on a public collection is missing from the hub for anonymous")
	}
	if !onBrowseRail(t, pool, anon, pubID) {
		t.Error("a public placement on a public collection is missing from the rail for anonymous")
	}
	if inHubFeaturedTab(t, pool, anon, orgID) {
		t.Error("an ORG placement reached an anonymous viewer's hub Featured tab; `org` is the " +
			"internal signed-in audience (ADR 0065) and #1104 must not have widened it")
	}
	if onBrowseRail(t, pool, anon, orgID) {
		t.Error("an ORG placement reached an anonymous viewer's browse rail")
	}
}
