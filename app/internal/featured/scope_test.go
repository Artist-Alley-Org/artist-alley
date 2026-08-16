// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1104 — the featured audience is chosen by the VIEWER, from one
// expression, and the anonymous arm did not move.
//
// Before #1104 the rail asked for `scope = 'public'` and the
// collections hub's Featured tab asked for `scope = 'org'`, while the
// only writer produced `org` and the seed produced `public`. No row
// could satisfy both surfaces, which is why the hub tab was empty on
// every seeded install. The tests here pin the fix in both directions:
// the signed-in arm gained `org` (the bug), and the anonymous arm gained
// NOTHING (the hazard).
//
// In-package so the rail fixture helpers in rail_test.go are reachable;
// the write-path half lives in handler_test.go, which is the external
// test package.

package featured

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// TestScopeVisibleSQL_AnonymousArmIsUnchanged is the byte-identical pin
// the widening required.
//
// The literal below is what featured/rail.go's WHERE clause said before
// #1104, copied here verbatim. It is asserted as a STRING rather than
// only through behaviour because "the anonymous audience did not
// change" is a claim about the predicate, and a behavioural test over a
// fixture can only ever say "did not change FOR THESE ROWS". Both
// assertions run — this one, and TestRail_AnonymousSeesExactlyThePublic
// Placements below — because each catches what the other cannot.
func TestScopeVisibleSQL_AnonymousArmIsUnchanged(t *testing.T) {
	const pre1104 = "f.scope = 'public'"
	if got := ScopeVisibleSQL("f", visibility.NewCaller(nil)); got != pre1104 {
		t.Errorf("anonymous arm = %q, want the pre-#1104 literal %q.\n"+
			"Widening the anonymous audience puts internal placements in front of "+
			"logged-out readers. If that is genuinely intended it is a visibility "+
			"decision needing its own issue, not a side effect of a reader refactor.",
			got, pre1104)
	}
}

// TestScopeVisibleSQL_SignedInArmAddsOrgAndNothingElse pins the other
// side: `org` joins `public`, and `team` joins neither. A team
// placement's audience is one team, which needs a membership test;
// admitting the scope here would hand every signed-in caller every
// team's placements.
func TestScopeVisibleSQL_SignedInArmAddsOrgAndNothingElse(t *testing.T) {
	ref := railOwner
	got := ScopeVisibleSQL("f", visibility.NewCaller(&ref))
	const want = "f.scope IN ('org', 'public')"
	if got != want {
		t.Errorf("signed-in arm = %q, want %q", got, want)
	}
}

// TestScopeVisibleSQL_AliasIsApplied guards the one way this helper can
// be silently wrong at a call site: an unqualified column reference
// joins fine against a single-table query and becomes ambiguous — or,
// worse, resolves to the wrong table — the moment a reader joins
// something else that has a `scope` column.
func TestScopeVisibleSQL_AliasIsApplied(t *testing.T) {
	ref := railOwner
	for _, alias := range []string{"f", "fi"} {
		if got := ScopeVisibleSQL(alias, visibility.NewCaller(&ref)); got[:len(alias)+1] != alias+"." {
			t.Errorf("alias %q not applied: %q", alias, got)
		}
	}
}

// railTeam seeds a live team and returns its id.
func railTeam(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`,
		id, "rail-"+id.String()[:8], name)
	if err != nil {
		t.Fatalf("seed team %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id=$1`, id)
	})
	return id
}

// placeTeamScoped inserts a scope='team' placement, which the ordinary
// `place` helper cannot do: featured_items_team_scope_check requires a
// team_id for that scope and forbids one for every other.
func placeTeamScoped(t *testing.T, pool *pgxpool.Pool, kind string, subject, team uuid.UUID, pos int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO featured_items (subject_kind, subject_id, position, scope, team_id)
		VALUES ($1,$2,$3,'team',$4)`, kind, subject, pos, team)
	if err != nil {
		t.Fatalf("place team-scoped %s %s: %v", kind, subject, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM featured_items WHERE subject_id=$1 AND scope='team'`, subject)
	})
}

// scopeFixture places one visible collection at each of the three
// audiences and returns their titles. Every subject is public and
// anonymously readable, so the ONLY thing that can keep one off a rail
// is its scope — which is what these tests are about. (The "featuring
// never widens access" half is TestRail_FeaturingNeverWidensAccess's
// job and is not restated here.)
func scopeFixture(t *testing.T, pool *pgxpool.Pool) (publicTitle, orgTitle, teamTitle string) {
	t.Helper()
	publicTitle, orgTitle, teamTitle = "scope-fx-public", "scope-fx-org", "scope-fx-team"

	place(t, pool, "collection", railCollection(t, pool, publicTitle, "public"), "public", 0)
	place(t, pool, "collection", railCollection(t, pool, orgTitle, "public"), "org", 1)
	placeTeamScoped(t, pool, "collection",
		railCollection(t, pool, teamTitle, "public"), railTeam(t, pool, "scope-fx-team-owner"), 2)
	return
}

// TestRail_AnonymousSeesExactlyThePublicPlacements is the behavioural
// half of the anonymous pin: over a fixture holding one placement at
// each audience, an anonymous caller gets the `public` one and only the
// `public` one — the same answer it got before #1104.
func TestRail_AnonymousSeesExactlyThePublicPlacements(t *testing.T) {
	pool := railPool(t)
	pub, org, team := scopeFixture(t, pool)

	got := railTitles(t, pool, visibility.NewCaller(nil))

	if !got[pub] {
		t.Errorf("%q is a public placement on a public collection and did not render for anonymous", pub)
	}
	if got[org] {
		t.Errorf("%q is an ORG placement and rendered for an anonymous caller — #1104 widened the "+
			"anonymous audience, which it must not do", org)
	}
	if got[team] {
		t.Errorf("%q is a TEAM placement and rendered for an anonymous caller", team)
	}
}

// TestRail_SignedInSeesOrgAndPublic is the bug #1104 reports, at the
// rail: before the fix a placement written through POST /admin/featured
// (always `org`) could never appear here for anyone.
func TestRail_SignedInSeesOrgAndPublic(t *testing.T) {
	pool := railPool(t)
	pub, org, team := scopeFixture(t, pool)

	ref := railOwner
	got := railTitles(t, pool, visibility.NewCaller(&ref))

	if !got[org] {
		t.Errorf("%q is an org placement and did not render for a signed-in caller — this is the "+
			"#1104 bug: everything the admin UI features is org-scoped", org)
	}
	if !got[pub] {
		t.Errorf("%q is a public placement and did not render for a signed-in caller; public is a "+
			"SUPERSET audience, not a different surface", pub)
	}
	if got[team] {
		t.Errorf("%q is a TEAM placement and rendered on the browse rail for a caller who is not "+
			"asserted to be in that team; team audiences need a membership test, not a scope test", team)
	}
}

// TestScopeVisibleSQL_PinnedInStaticQueries closes the gap the helper
// cannot close on its own.
//
// Two readers of featured_items are sqlc queries — static strings that
// cannot splice a Go fragment — so they carry the signed-in arm written
// out by hand. A hand-copy that nothing checks is exactly the defect
// #1063 is about, and #1104 exists because three hand-copies disagreed.
// This asserts the copies are the helper's own output, character for
// character, so editing one arm and not the other fails the build.
//
// ⚠️ What it cannot catch: a query that contains the fragment and ALSO
// contains a second, narrower scope condition further down its WHERE
// clause. Containment proves the right rule is present, not that it is
// the only one. The teams query is eight lines long and the collections
// oracle is checked by TestListCollectionsPage_FilterParity against the
// hand-built implementation that splices the helper for real, so both
// have a second line of defence; a THIRD static reader would need one
// of its own.
func TestScopeVisibleSQL_PinnedInStaticQueries(t *testing.T) {
	ref := railOwner
	// The alias each static reader uses. The helper takes the alias, so
	// the pin has to render the fragment the way that file spells it.
	pinned := []struct {
		file  string
		alias string
		why   string
	}{
		{"../teams/queries.sql", "f",
			"ListFeaturedTeams — the signed-in teams rail; 401s anonymous callers before the query runs"},
		{"../teams/queries.sql.go", "f",
			"the generated copy of the same query, which is what actually executes"},
		{"../collections/queries.sql", "fi",
			"ListCollectionsPage — the parity oracle for TestListCollectionsPage_FilterParity"},
		{"../collections/queries.sql.go", "fi",
			"the generated copy of the oracle"},
	}
	for _, p := range pinned {
		src, err := os.ReadFile(p.file)
		if err != nil {
			t.Fatalf("read %s: %v", p.file, err)
		}
		want := ScopeVisibleSQL(p.alias, visibility.NewCaller(&ref))
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not contain the signed-in audience fragment %q.\n"+
				"That file is %s.\n"+
				"It is a STATIC query and cannot splice featured.ScopeVisibleSQL, so it carries "+
				"the fragment by hand — and a hand-copy nothing checks is how #1104 happened "+
				"(three readers, three different scopes). Either paste the helper's exact output "+
				"or convert the reader to a hand-built query that splices it.",
				p.file, want, p.why)
		}
	}
}
