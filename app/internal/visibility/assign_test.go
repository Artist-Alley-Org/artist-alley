// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #954 / #953 — the ONE rule for who may put a row in a team, tested at
// the point of definition.
//
// The two call sites (posts.CreatePost, assets.CreateAsset) each have
// their own matrix asserting the rule through their own handler. This
// file is the reason those two matrices agree: it tests the shared
// predicate directly, and it reaches the three inputs neither handler
// can — an anonymous caller (both answer 401 first), the nil UUID, and a
// nil capability checker.
//
// Skips without AA_DB_PASSWORD.

package visibility

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	atMember   int64 = 9540001
	atStranger int64 = 9540002
)

// atTeam seeds a team, optionally soft-deleted.
func atTeam(t *testing.T, pool *pgxpool.Pool, deleted bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO teams (id, slug, name, deleted_at)
		 VALUES ($1, $2, 'assign-test', CASE WHEN $3 THEN now() ELSE NULL END)`,
		id, "at_"+id.String()[:8], deleted,
	); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

func atJoin(t *testing.T, pool *pgxpool.Pool, team uuid.UUID, ref int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, team, ref,
	); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

func TestCanAssignToTeam(t *testing.T) {
	pool := contentPool(t)
	ctx := context.Background()

	live := atTeam(t, pool, false)
	other := atTeam(t, pool, false)
	dead := atTeam(t, pool, true)
	atJoin(t, pool, live, atMember)
	atJoin(t, pool, dead, atMember)

	strangerRef, memberRef := atStranger, atMember
	authed := NewCaller(&strangerRef)
	memberCaller := NewCaller(&memberRef)
	anon := NewCaller(nil)

	cases := []struct {
		name        string
		caller      Caller
		caps        CapabilityChecker
		scopedTeams []uuid.UUID
		team        uuid.UUID
		want        bool
		why         string
	}{
		{
			name: "direct member", caller: memberCaller, caps: notAdmin, team: live, want: true,
			why: "the ordinary path",
		},
		{
			name: "not a member, no grant", caller: authed, caps: notAdmin, team: live, want: false,
			why: "#954's bug in one line: existence was the whole check",
		},
		{
			name: "scoped grant over the team", caller: authed, caps: notAdmin,
			scopedTeams: []uuid.UUID{live}, team: live, want: true,
			why: "the closure-expanded set from Identity.ScopedTeams",
		},
		{
			name: "scoped grant over a DIFFERENT team", caller: authed, caps: notAdmin,
			scopedTeams: []uuid.UUID{other}, team: live, want: false,
			why: "a scope is a scope",
		},
		{
			name: "system.admin", caller: authed, caps: isAdmin, team: live, want: true,
			why: "the escape hatch",
		},
		{
			name: "system.admin on a SOFT-DELETED team", caller: authed, caps: isAdmin, team: dead, want: false,
			why: "liveness is probed BEFORE the authorisation disjunction, so a " +
				"deleted team is refused uniformly rather than left reachable by " +
				"whoever holds the wildcard",
		},
		{
			name: "member of a soft-deleted team", caller: memberCaller, caps: notAdmin, team: dead, want: false,
			why: "the FK does not read teams.deleted_at",
		},
		{
			name: "nonexistent team", caller: memberCaller, caps: notAdmin, team: uuid.New(), want: false,
			why: "indistinguishable from unauthorised — the caller cannot tell them apart",
		},
		{
			name: "anonymous, team exists", caller: anon, caps: notAdmin, team: live, want: false,
			why: "the sentinel ref 0 must never match a membership row; refused " +
				"before the query rather than by it",
		},
		{
			name: "anonymous holding the wildcard", caller: anon, caps: isAdmin, team: live, want: false,
			why: "an anonymous identity holds nothing; a checker that says otherwise " +
				"is a bug in the caller, and this fails closed on it",
		},
		{
			name: "nil UUID", caller: memberCaller, caps: notAdmin, team: uuid.Nil, want: false,
			why: "the zero value of a decoded-but-absent uuid must not be a team",
		},
		{
			name: "nil capability checker", caller: memberCaller, caps: nil, team: live, want: true,
			why: "a nil checker is 'holds nothing', not a panic — membership still decides",
		},
		{
			name: "nil checker, not a member", caller: authed, caps: nil, team: live, want: false,
			why: "and it confers nothing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanAssignToTeam(ctx, pool, tc.caller, tc.caps, tc.scopedTeams, tc.team)
			if err != nil {
				t.Fatalf("CanAssignToTeam: %v", err)
			}
			if got != tc.want {
				t.Errorf("CanAssignToTeam = %v, want %v (%s)", got, tc.want, tc.why)
			}
		})
	}
}
