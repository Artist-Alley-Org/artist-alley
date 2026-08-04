// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #883 — the membership-never-widens contract.
//
// A CONTRACT test on visibility.MemberReadable, deliberately not a
// per-endpoint one, for the same reason predicate_matrix_test.go is a
// contract test on ToSQL: three surfaces (post members, collection
// contents, IIIF collection manifests) route their answer through this
// one function, so pinning it here governs all of them at once.
//
// It states the requirement, not the implementation. Every case is
// derived from the two planes an asset already lives under, computed
// INDEPENDENTLY of MemberReadable:
//
//	rowVisible   — the REAL EntityAsset predicate, executed as SQL
//	               against the seeded row (not re-derived in Go)
//	contentOK    — ContentReadable, the ADR 0064 tier rule
//
// and then asserts MemberReadable == rowVisible AND contentOK. Written
// that way, the test fails if either conjunct is dropped, and it fails
// for a reason that names which guarantee was lost. A per-case expected
// boolean typed out by hand would only re-encode whatever the code did
// on the day it was written.
//
// SOFT-DELETE is the one conjunct held out, and the predicate has an
// option that says exactly that: rowVisible is taken with
// IncludeSoftDeleted(), which drops the `deleted_at IS NULL` clause and
// NOTHING else. MemberReadable deliberately does not decide soft-delete
// — a deleted member is not a placeholder, it is gone, and the container
// queries drop it in SQL. That division of labour is asserted where it
// lives, in the collection and post contents tests, not re-implemented
// here; using the option rather than skipping the case keeps every other
// conjunct sharp on the deleted row too.
//
// Skips without AA_DB_PASSWORD, same convention as the rest.

package visibility

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	mmOwner   int64 = 8830001
	mmTeamMem int64 = 8830002
	mmOther   int64 = 8830003
)

type mmCase struct {
	name        string
	status      string
	sensitivity string
	processing  string
	deleted     bool
	teamOwned   bool
}

// mmSeeds spans the axes the rule reads: sensitivity tier (all four,
// including one unrecognised value so the fail-closed default is
// covered), publication status, processing state, soft-delete, and team
// ownership.
var mmSeeds = []mmCase{
	{"public/active/ready", "active", "public", "ready", false, false},
	{"public/draft", "draft", "public", "ready", false, false},
	{"public/archived", "archived", "public", "ready", false, false},
	{"public/processing", "active", "public", "pending", false, false},
	{"team", "active", "team", "ready", false, true},
	{"team/no-team-row", "active", "team", "ready", false, false},
	{"restricted", "active", "restricted", "ready", false, false},
	{"embargo", "active", "embargo", "ready", false, false},
	{"public/soft-deleted", "active", "public", "ready", true, false},
}

// seedMemberMatrix plants the asset rows plus a team the mmTeamMem user
// belongs to, and returns ids parallel to mmSeeds.
func seedMemberMatrix(t *testing.T, pool *pgxpool.Pool) []uuid.UUID {
	t.Helper()
	ctx := context.Background()

	teamID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, name, slug) VALUES ($1, $2, $3)`,
		teamID, "mm-team-883", "mm-team-883"); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)`,
		teamID, mmTeamMem); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	ids := make([]uuid.UUID, len(mmSeeds))
	for i, s := range mmSeeds {
		id := uuid.New()
		ids[i] = id
		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		var team any
		if s.teamOwned {
			team = teamID
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, team_id, deleted_at)
			VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), $4, $5, $6, $7, %s)`, del),
			id, "mm-"+s.name, mmOwner, s.status, s.sensitivity, s.processing, team); err != nil {
			t.Fatalf("seed %q: %v", s.name, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM team_memberships WHERE team_id = $1`, teamID)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id = $1`, teamID)
	})
	return ids
}

// mmRow reads back the row exactly as a container query would project
// it, plus the caller's team membership — so the MemberRow handed to the
// function under test is built from the DATABASE, not from the fixture
// literal that wrote it.
func mmRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, caller Caller) MemberRow {
	t.Helper()
	var row MemberRow
	err := pool.QueryRow(context.Background(), `
		SELECT a.sensitivity, a.status, a.processing_status, a.owner_user_ref,
		       (a.team_id IS NOT NULL AND EXISTS (
		            SELECT 1 FROM team_memberships tm
		             WHERE tm.team_id = a.team_id AND tm.user_ref = $2::BIGINT))
		  FROM assets a WHERE a.id = $1`,
		id, caller.UserRef,
	).Scan(&row.Sensitivity, &row.Status, &row.ProcessingStatus, &row.OwnerUserRef, &row.IsTeamMember)
	if err != nil {
		t.Fatalf("read member row %v: %v", id, err)
	}
	return row
}

// TestMemberMatrix_NeverWiderThanTheItemAlone is the invariant #883
// exists for. For every caller × every asset shape: a member reached
// THROUGH a container must never be readable when the same asset,
// addressed directly, is not.
//
// The right-hand side runs the REAL predicate SQL, so this compares the
// member rule against the row rule as the database actually applies it.
// Re-deriving "what anonymous can see" in Go would let both sides drift
// together and pass regardless.
func TestMemberMatrix_NeverWiderThanTheItemAlone(t *testing.T) {
	pool := matrixPool(t)
	ids := seedMemberMatrix(t, pool)

	callers := []struct {
		name   string
		caller Caller
	}{
		{"anonymous", anonCaller()},
		{"owner", userCaller(mmOwner)},
		{"team member", userCaller(mmTeamMem)},
		{"authenticated stranger", userCaller(mmOther)},
	}

	for _, c := range callers {
		// One SQL round-trip per caller for the whole id set: which of
		// these assets does the ROW predicate admit standalone. See the
		// file header for why soft-delete is waived.
		standalone := visibleIDsOpts(t, pool, EntityAsset, c.caller, "assets", ids, IncludeSoftDeleted())
		for i, s := range mmSeeds {
			row := mmRow(t, pool, ids[i], c.caller)
			member := MemberReadable(row, c.caller, nil)
			if member && !standalone[ids[i]] {
				t.Errorf("%s / %s: readable AS A MEMBER but NOT standalone — "+
					"membership widened this item, which is exactly what #883 forbids",
					c.name, s.name)
			}
		}
	}
}

// TestMemberMatrix_IsRowPlaneAndContentPlane pins the rule to its two
// components rather than to a hand-typed truth table.
//
// Dropping either conjunct from MemberReadable turns this red, and the
// message says which: remove the ContentReadable call and every
// restricted/embargo case fails for an authenticated stranger (whose row
// predicate is soft-delete only); remove the anonymous status /
// processing guards and the draft, archived and processing cases fail
// for anonymous.
func TestMemberMatrix_IsRowPlaneAndContentPlane(t *testing.T) {
	pool := matrixPool(t)
	ids := seedMemberMatrix(t, pool)

	callers := []struct {
		name   string
		caller Caller
	}{
		{"anonymous", anonCaller()},
		{"owner", userCaller(mmOwner)},
		{"team member", userCaller(mmTeamMem)},
		{"authenticated stranger", userCaller(mmOther)},
	}

	for _, c := range callers {
		standalone := visibleIDsOpts(t, pool, EntityAsset, c.caller, "assets", ids, IncludeSoftDeleted())
		for i, s := range mmSeeds {
			row := mmRow(t, pool, ids[i], c.caller)
			rowOK := standalone[ids[i]]
			contentOK := ContentReadable(row.Sensitivity, row.OwnerUserRef, c.caller, nil, row.IsTeamMember)
			want := rowOK && contentOK
			got := MemberReadable(row, c.caller, nil)
			if got != want {
				t.Errorf("%s / %s: MemberReadable=%v, want %v (row plane=%v, content plane=%v)",
					c.name, s.name, got, want, rowOK, contentOK)
			}
		}
	}
}

// TestMemberMatrix_ContainerVisibilityIsIrrelevant is the third axis the
// brief names, and it is stated as an INDEPENDENCE rather than a table:
// MemberReadable takes no container argument at all, so a member's
// readability cannot vary with the post or collection it sits in, and
// owning the CONTAINER confers nothing over its members.
//
// The expectations are written per caller class in full, which is the
// one place in this file a hand-typed table is the right instrument:
// these are the answers the OWNER'S RULE demands, independent of how the
// two planes happen to compose.
func TestMemberMatrix_ContainerVisibilityIsIrrelevant(t *testing.T) {
	pool := matrixPool(t)
	ids := seedMemberMatrix(t, pool)

	for _, c := range []struct {
		name   string
		caller Caller
		// want is keyed by mmSeeds[i].name.
		want map[string]bool
	}{
		{
			// An anonymous visitor gets the public/active/ready floor and
			// nothing else, in a container exactly as out of one.
			name: "anonymous", caller: anonCaller(),
			want: map[string]bool{
				"public/active/ready": true,
				"public/draft":        false,
				"public/archived":     false,
				"public/processing":   false,
				"team":                false,
				"team/no-team-row":    false,
				"restricted":          false,
				"embargo":             false,
			},
		},
		{
			// A signed-in stranger clears the CONTENT plane on public
			// tiers only. Draft / archived / processing are NOT withheld
			// from them, because the authenticated row predicate admits
			// those standalone (ADR 0063) — withholding here would be
			// narrower than the item view, which is allowed but is not
			// what the code does, and stating it wrongly is how this test
			// would start lying.
			name: "authenticated stranger", caller: userCaller(mmOther),
			want: map[string]bool{
				"public/active/ready": true,
				"public/draft":        true,
				"public/archived":     true,
				"public/processing":   true,
				"team":                false,
				"team/no-team-row":    false,
				"restricted":          false,
				"embargo":             false,
			},
		},
		{
			// Team membership unlocks the team tier and nothing above it.
			// "team/no-team-row" is the guard: sensitivity='team' with
			// team_id NULL must not resolve to "everyone is a member".
			name: "team member", caller: userCaller(mmTeamMem),
			want: map[string]bool{
				"public/active/ready": true,
				"public/draft":        true,
				"public/archived":     true,
				"public/processing":   true,
				"team":                true,
				"team/no-team-row":    false,
				"restricted":          false,
				"embargo":             false,
			},
		},
		{
			name: "owner", caller: userCaller(mmOwner),
			want: map[string]bool{
				"public/active/ready": true,
				"public/draft":        true,
				"public/archived":     true,
				"public/processing":   true,
				"team":                true,
				"team/no-team-row":    true,
				"restricted":          true,
				"embargo":             true,
			},
		},
	} {
		for i, s := range mmSeeds {
			// Soft-delete is the SQL's job — see the file header.
			if s.deleted {
				continue
			}
			want, ok := c.want[s.name]
			if !ok {
				t.Fatalf("%s: no expectation for seed %q — a new axis was added "+
					"without deciding what it means", c.name, s.name)
			}
			row := mmRow(t, pool, ids[i], c.caller)
			if got := MemberReadable(row, c.caller, nil); got != want {
				t.Errorf("%s / %s: MemberReadable=%v, want %v", c.name, s.name, got, want)
			}
		}
	}
}

// TestMemberMatrix_CapabilityShortCircuits pins the two capabilities
// that cross every tier, because both are easy to lose in a refactor and
// losing either turns a demo install into a wall of placeholders.
func TestMemberMatrix_CapabilityShortCircuits(t *testing.T) {
	pool := matrixPool(t)
	ids := seedMemberMatrix(t, pool)

	for _, cap := range []string{SystemAdmin, ContentReadAll} {
		caps := func(code string) bool { return code == cap }
		stranger := userCaller(mmOther)
		for i, s := range mmSeeds {
			if s.deleted {
				continue
			}
			row := mmRow(t, pool, ids[i], stranger)
			if !MemberReadable(row, stranger, caps) {
				t.Errorf("%s holder was refused member %q", cap, s.name)
			}
		}
	}
}

// TestMemberReadable_FailsClosed covers the query-free guards, which
// need no fixture: an unrecognised tier, a NULL owner, and the anonymous
// sentinel colliding with owner ref 0.
func TestMemberReadable_FailsClosed(t *testing.T) {
	anon := anonCaller()
	zeroOwner := int64(0)

	if MemberReadable(MemberRow{
		Sensitivity: "some-new-tier-2027", Status: "active", ProcessingStatus: "ready",
	}, anon, nil) {
		t.Error("an unrecognised sensitivity tier must DENY, never inherit public")
	}
	if MemberReadable(MemberRow{
		Sensitivity: "restricted", Status: "active", ProcessingStatus: "ready",
		OwnerUserRef: &zeroOwner,
	}, anon, nil) {
		t.Error("an asset owned by ref 0 matched the anonymous sentinel as its owner")
	}
	if MemberReadable(MemberRow{
		Sensitivity: "restricted", Status: "active", ProcessingStatus: "ready",
		OwnerUserRef: nil,
	}, userCaller(mmOther), nil) {
		t.Error("a NULL owner must never match a caller")
	}
	// And the positive control, so the three above are not passing
	// because the function returns false unconditionally.
	if !MemberReadable(MemberRow{
		Sensitivity: "public", Status: "active", ProcessingStatus: "ready",
	}, anon, nil) {
		t.Fatal("a public/active/ready member must be readable by anyone — the guards above are vacuous")
	}
}
