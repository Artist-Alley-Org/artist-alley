// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package acls hosts the per-resource ACL slice of the API (ADR 0010
// Layer 6). For now this package contains only the trigger tests that
// lock in the sweep semantics from migration 00001 — the OpenAPI surface
// + handlers land in Phase 1.7.B-7.
package acls_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// TestACLSweep_OnRoleAndTeamDelete verifies the two principal-sweep
// triggers fire correctly when a role or team is deleted:
//
//   - Deleting a role removes ACL rows whose principal_type='role' and
//     principal_id is the role's id; rows for other principal types
//     remain.
//   - Deleting a team does the same for principal_type='team'.
//   - User-principal rows are NOT swept (we have no trigger on the legacy
//     user table and dangling rows are tolerated by the handler-side
//     check).
//
// All work happens in a rolled-back transaction so the test leaves no
// rows behind, including for the borrowed existing post.
func TestACLSweep_OnRoleAndTeamDelete(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// We need a real post to satisfy the FK. Seed one in this tx.
	// The post needs an author (user.ref); 0 is fine for the test
	// since we never use this post via the user-facing API.
	postID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, visibility)
		VALUES ($1, 0, 'acl-trigger-test', 'private')
	`, postID); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	// Principals: a role and a team.
	var roleID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO roles (name) VALUES ('acl_trigger_test_role') RETURNING id`,
	).Scan(&roleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	var teamID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO teams (slug, name) VALUES ('acl_trigger_test', 'ACL Test Team') RETURNING id`,
	).Scan(&teamID); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	// Four ACL rows: two role (read+write), one team (admin), one
	// dangling user (never gets cleaned up by these triggers).
	if _, err := tx.Exec(ctx, `
		INSERT INTO post_acls (post_id, principal_type, principal_id, permission) VALUES
		    ($1, 'role', $2::text, 'read'),
		    ($1, 'role', $2::text, 'write'),
		    ($1, 'team', $3::text, 'admin'),
		    ($1, 'user', '42',     'read')
	`, postID, roleID, teamID); err != nil {
		t.Fatalf("seed ACL rows: %v", err)
	}

	// Baseline.
	counts := aclCountsByPrincipal(t, ctx, tx, postID)
	if counts["role"] != 2 || counts["team"] != 1 || counts["user"] != 1 {
		t.Fatalf("baseline counts wrong: %v", counts)
	}

	// Delete the role → role rows go; others stay.
	if _, err := tx.Exec(ctx, `DELETE FROM roles WHERE id = $1`, roleID); err != nil {
		t.Fatalf("delete role: %v", err)
	}
	counts = aclCountsByPrincipal(t, ctx, tx, postID)
	if counts["role"] != 0 {
		t.Errorf("after role delete, role rows = %d; want 0", counts["role"])
	}
	if counts["team"] != 1 {
		t.Errorf("after role delete, team rows = %d; want 1 (unchanged)", counts["team"])
	}
	if counts["user"] != 1 {
		t.Errorf("after role delete, user rows = %d; want 1 (unchanged)", counts["user"])
	}

	// Delete the team → team rows go; user stays (no user-deletion sweep).
	if _, err := tx.Exec(ctx, `DELETE FROM teams WHERE id = $1`, teamID); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	counts = aclCountsByPrincipal(t, ctx, tx, postID)
	if counts["team"] != 0 {
		t.Errorf("after team delete, team rows = %d; want 0", counts["team"])
	}
	if counts["user"] != 1 {
		t.Errorf("after team delete, user rows = %d; want 1 (unchanged, no user-sweep trigger)", counts["user"])
	}
}

// TestACLCascade_OnPostDelete verifies the real FK to posts(id) ON
// DELETE CASCADE clears the ACL rows when the post itself is deleted.
// (This is plain SQL, not a trigger, but it's part of the integrity
// contract.)
func TestACLCascade_OnPostDelete(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	postID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, visibility)
		VALUES ($1, 0, 'acl-cascade-test', 'private')
	`, postID); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO post_acls (post_id, principal_type, principal_id, permission)
		VALUES ($1, 'user', '99', 'read'), ($1, 'user', '99', 'write')
	`, postID); err != nil {
		t.Fatalf("seed ACL rows: %v", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM posts WHERE id = $1`, postID); err != nil {
		t.Fatalf("delete post: %v", err)
	}
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM post_acls WHERE post_id = $1`, postID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected ACL rows gone after post delete, %d remain", remaining)
	}
}

// aclCountsByPrincipal returns a map of principal_type -> row count for
// the given post. Principal types missing from the result are absent
// from the map; the caller's nil-safe lookup returns 0 in that case.
func aclCountsByPrincipal(t *testing.T, ctx context.Context, tx pgx.Tx, postID uuid.UUID) map[string]int {
	t.Helper()
	rows, err := tx.Query(ctx, `
		SELECT principal_type, count(*)::int
		  FROM post_acls
		 WHERE post_id = $1
		 GROUP BY 1
	`, postID)
	if err != nil {
		t.Fatalf("acl count query: %v", err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[k] = v
	}
	return out
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
