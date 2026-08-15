// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #577 — migration 00041 creates `team_follows`, the teams-rail
// bookmark table.
//
// The interesting properties are not "a table appeared". They are:
//
//   - the table does not exist before 00041 (so the migration is not a
//     no-op that some earlier file already did);
//   - the PK is on (user_ref, team_id), which is what makes
//     ON CONFLICT DO NOTHING an idempotent follow rather than a
//     silently-dropped write;
//   - BOTH foreign keys exist and BOTH cascade — a follow is
//     meaningless without either end, and a dangling half is a row
//     nothing can clean up;
//   - the cascades actually fire, proven by deleting each parent and
//     watching the row go, rather than by reading pg_constraint and
//     believing it;
//   - Down really drops it, and a re-Up is clean.
//
// The soft-delete case is deliberately NOT tested here, because the
// schema cannot express it: `teams.deleted_at` is invisible to a
// foreign key, so a tombstoned team keeps satisfying this constraint.
// That gap is closed in the handler by an explicit liveness probe and
// is pinned by TestFollowTeam_SoftDeletedTeamRefused in the teams
// package. Splitting it that way is the point — the constraint and the
// probe guard different things and neither substitutes for the other.

package db

import (
	"database/sql"
	"testing"
)

const (
	teamFollowsBeforeVersion = 40 // 00040_auditor_admin_read_caps
	teamFollowsAtVersion     = 41 // 00041_team_follows
)

// tableExists lives in migrate_fresh_test.go — same package, same
// question, and a second copy would be one more thing to keep in step.

// fkDeleteRule returns a named FK's ON DELETE action, or "" when the
// constraint does not exist.
func fkDeleteRule(t *testing.T, sqlDB *sql.DB, constraint string) string {
	t.Helper()
	var rule string
	err := sqlDB.QueryRowContext(t.Context(),
		`SELECT rc.delete_rule
		   FROM information_schema.referential_constraints rc
		  WHERE rc.constraint_schema = 'public' AND rc.constraint_name = $1`,
		constraint).Scan(&rule)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read delete rule for %s: %v", constraint, err)
	}
	return rule
}

func TestMigration00041_TeamFollows_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	// ── Before 00041 the table must not exist ────────────────────────
	if _, err := p.UpTo(ctx, teamFollowsBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", teamFollowsBeforeVersion, err)
	}
	if tableExists(t, sqlDB, "team_follows") {
		t.Fatalf("team_follows exists before 00041 — the migration is redundant")
	}

	// ── Up ───────────────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, teamFollowsAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", teamFollowsAtVersion, err)
	}
	if !tableExists(t, sqlDB, "team_follows") {
		t.Fatalf("team_follows missing after 00041 Up")
	}

	// The PK is the idempotence mechanism, so pin its exact columns and
	// their order. `ON CONFLICT (user_ref, team_id)` in queries.sql
	// names this constraint by shape; if the columns ever change, the
	// insert stops conflicting and a double-follow becomes an error
	// instead of a no-op.
	var pkCols string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		   FROM pg_constraint c
		   JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
		   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		  WHERE c.conname = 'team_follows_pkey'`).Scan(&pkCols); err != nil {
		t.Fatalf("read team_follows PK columns: %v", err)
	}
	if pkCols != "user_ref,team_id" {
		t.Errorf("team_follows PK = (%s), want (user_ref,team_id)", pkCols)
	}

	// Both FKs, both cascading.
	for _, fk := range []string{"team_follows_user_ref_fkey", "team_follows_team_id_fkey"} {
		if got := fkDeleteRule(t, sqlDB, fk); got != "CASCADE" {
			t.Errorf("%s delete rule = %q, want CASCADE", fk, got)
		}
	}

	// ── The cascades FIRE ────────────────────────────────────────────
	//
	// Reading pg_constraint proves the declaration; deleting a parent
	// proves the behaviour. Two independent (user, team) pairs so each
	// half can be knocked out without the other's result being an
	// accident of ordering.
	mkUser := func(name string) int64 {
		t.Helper()
		var ref int64
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
			name).Scan(&ref); err != nil {
			t.Fatalf("seed user %s: %v", name, err)
		}
		return ref
	}
	mkTeam := func(slug string) string {
		t.Helper()
		var id string
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO teams (slug, name) VALUES ($1, $1) RETURNING id::text`,
			slug).Scan(&id); err != nil {
			t.Fatalf("seed team %s: %v", slug, err)
		}
		return id
	}
	countFollows := func() int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM team_follows`).Scan(&n); err != nil {
			t.Fatalf("count team_follows: %v", err)
		}
		return n
	}

	userA, userB := mkUser("tf-a"), mkUser("tf-b")
	teamA, teamB := mkTeam("tf-team-a"), mkTeam("tf-team-b")
	for _, pair := range [][2]any{{userA, teamA}, {userB, teamB}} {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO team_follows (user_ref, team_id) VALUES ($1, $2)`,
			pair[0], pair[1]); err != nil {
			t.Fatalf("seed follow %v: %v", pair, err)
		}
	}
	if n := countFollows(); n != 2 {
		t.Fatalf("seeded %d follows, want 2", n)
	}

	// The PK really does reject the duplicate the query's ON CONFLICT
	// absorbs. Without this the idempotence claim rests on a clause
	// guarding a constraint nobody checked was there.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO team_follows (user_ref, team_id) VALUES ($1, $2)`,
		userA, teamA); err == nil {
		t.Error("duplicate (user_ref, team_id) accepted — the PK is not enforcing uniqueness")
	}

	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM "user" WHERE ref = $1`, userA); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := countFollows(); n != 1 {
		t.Errorf("after deleting the user, %d follows remain, want 1 — user cascade did not fire", n)
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM teams WHERE id = $1::uuid`, teamB); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	if n := countFollows(); n != 0 {
		t.Errorf("after deleting the team, %d follows remain, want 0 — team cascade did not fire", n)
	}

	// ── Down drops it ────────────────────────────────────────────────
	if _, err := p.DownTo(ctx, teamFollowsBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", teamFollowsBeforeVersion, err)
	}
	if tableExists(t, sqlDB, "team_follows") {
		t.Errorf("team_follows survives 00041 Down")
	}

	// ── Re-Up is clean ───────────────────────────────────────────────
	if _, err := p.UpTo(ctx, teamFollowsAtVersion); err != nil {
		t.Fatalf("re-apply 00041 after down: %v", err)
	}
	if !tableExists(t, sqlDB, "team_follows") {
		t.Errorf("team_follows not restored after re-Up")
	}
}
