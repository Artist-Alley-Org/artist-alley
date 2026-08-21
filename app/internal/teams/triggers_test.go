// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package teams hosts the team-DAG slice of the API (ADR 0010, Layer 4).
//
// This file covers the closure-maintenance and cycle-rejection triggers
// in migration 00001. The triggers are SQL-side, so the test exercises
// them with direct SQL — no application handlers needed yet.
package teams_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// TestTeamClosureTriggers verifies the trigger machinery for the team
// DAG end-to-end:
//
//   - Self-row insertion on teams CREATE
//   - Transitive closure population on team_parents INSERT
//   - DAG support (multiple parents propagate their own ancestors)
//   - Cycle rejection on team_parents INSERT
//   - Full closure rebuild on team_parents DELETE
//
// All test rows are created in a single transaction wrapping the whole
// test, then rolled back via t.Cleanup, so the test leaves the DB in
// exactly the state it found it.
func TestTeamClosureTriggers(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	// All work happens in a single transaction that we roll back at
	// the end. Triggers fire inside the transaction so the test still
	// exercises the real maintenance functions.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Defer (not t.Cleanup) so LIFO order runs Rollback BEFORE the
	// outer pool.Close() — otherwise pool.Close() blocks forever
	// waiting for the still-active tx to release its connection.
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Fresh UUIDs every run — avoids collisions if a previous run
	// crashed out mid-transaction without rollback.
	diablo := uuid.New()
	rnd := uuid.New()
	character := uuid.New()
	crossStudio := uuid.New()

	mustExec(t, ctx, tx, `
		INSERT INTO teams (id, slug, name) VALUES
		    ($1, $2, 'Diablo'),
		    ($3, $4, 'Diablo R&D'),
		    ($5, $6, 'Character Art'),
		    ($7, $8, 'Cross-Studio Art Review')
	`,
		diablo, "test_diablo_"+diablo.String()[:8],
		rnd, "test_rnd_"+rnd.String()[:8],
		character, "test_character_"+character.String()[:8],
		crossStudio, "test_xstudio_"+crossStudio.String()[:8],
	)

	// --- self-rows ---------------------------------------------------------
	for _, id := range []uuid.UUID{diablo, rnd, character, crossStudio} {
		var depth int
		err := tx.QueryRow(ctx,
			`SELECT depth FROM team_closure WHERE ancestor_id = $1 AND descendant_id = $1`,
			id,
		).Scan(&depth)
		if err != nil {
			t.Fatalf("missing self-row for %s: %v", id, err)
		}
		if depth != 0 {
			t.Errorf("self-row depth for %s = %d, want 0", id, depth)
		}
	}

	// --- chain: diablo -> rnd -> character --------------------------------
	mustExec(t, ctx, tx,
		`INSERT INTO team_parents (parent_id, child_id) VALUES ($1, $2), ($3, $4)`,
		diablo, rnd, rnd, character,
	)

	pairs := closurePairs(t, ctx, tx, diablo, rnd, character, crossStudio)
	wantChain := map[[2]uuid.UUID]bool{
		{diablo, rnd}:       true,
		{rnd, character}:    true,
		{diablo, character}: true,
	}
	for pair := range wantChain {
		if !pairs[pair] {
			t.Errorf("missing closure pair: %s -> %s", pair[0], pair[1])
		}
	}

	// --- DAG: add cross_studio -> character (second parent for character) -
	mustExec(t, ctx, tx,
		`INSERT INTO team_parents (parent_id, child_id) VALUES ($1, $2)`,
		crossStudio, character,
	)

	pairs = closurePairs(t, ctx, tx, diablo, rnd, character, crossStudio)
	if !pairs[[2]uuid.UUID{crossStudio, character}] {
		t.Errorf("DAG: cross_studio -> character closure pair missing")
	}
	// Previous closures (diablo -> character via rnd) must still exist.
	if !pairs[[2]uuid.UUID{diablo, character}] {
		t.Errorf("DAG: pre-existing diablo -> character pair was lost")
	}

	// --- cycle rejection: character -> diablo (character is already
	//     a descendant of diablo, so this would close a cycle).
	//
	// Wrap in a SAVEPOINT so the raised exception doesn't poison the
	// surrounding transaction. The trigger raises ERRCODE check_violation
	// which Postgres treats as a regular error — without the savepoint
	// the whole tx would need to be rolled back.
	mustExec(t, ctx, tx, `SAVEPOINT cycle_test`)
	_, err = tx.Exec(ctx,
		`INSERT INTO team_parents (parent_id, child_id) VALUES ($1, $2)`,
		character, diablo,
	)
	if err == nil {
		t.Fatalf("cycle insert should have failed but didn't")
	}
	mustExec(t, ctx, tx, `ROLLBACK TO SAVEPOINT cycle_test`)

	// --- edge delete + closure rebuild ------------------------------------
	// Remove rnd -> character. Closure should drop rnd->character and
	// diablo->character (which depended on it), but keep
	// cross_studio->character and diablo->rnd untouched.
	mustExec(t, ctx, tx,
		`DELETE FROM team_parents WHERE parent_id = $1 AND child_id = $2`,
		rnd, character,
	)

	pairs = closurePairs(t, ctx, tx, diablo, rnd, character, crossStudio)
	if pairs[[2]uuid.UUID{rnd, character}] {
		t.Errorf("rebuild: rnd -> character should be gone")
	}
	if pairs[[2]uuid.UUID{diablo, character}] {
		t.Errorf("rebuild: diablo -> character should be gone (path via rnd severed)")
	}
	if !pairs[[2]uuid.UUID{crossStudio, character}] {
		t.Errorf("rebuild: cross_studio -> character should still exist")
	}
	if !pairs[[2]uuid.UUID{diablo, rnd}] {
		t.Errorf("rebuild: diablo -> rnd should still exist (untouched)")
	}
}

// closurePairs returns the set of (ancestor, descendant) entries in
// team_closure, restricted to the supplied team IDs and excluding
// self-rows.
func closurePairs(t *testing.T, ctx context.Context, tx pgx.Tx, ids ...uuid.UUID) map[[2]uuid.UUID]bool {
	t.Helper()
	rows, err := tx.Query(ctx, `
		SELECT ancestor_id, descendant_id
		  FROM team_closure
		 WHERE ancestor_id = ANY($1)
		   AND descendant_id = ANY($1)
		   AND ancestor_id <> descendant_id
	`, ids)
	if err != nil {
		t.Fatalf("closure query: %v", err)
	}
	defer rows.Close()
	out := make(map[[2]uuid.UUID]bool)
	for rows.Next() {
		var a, d uuid.UUID
		if err := rows.Scan(&a, &d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[[2]uuid.UUID{a, d}] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func mustExec(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", firstLine(sql), err)
	}
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Connection helper (mirrors the pattern in other handler tests).
// ---------------------------------------------------------------------------

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
