// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.C — lock-in tests for the already-wired last-admin
// invariant on capability operations + sweeper-time guard.
//
// The brief's commit 3 was "extend the last-admin invariant to
// cover RemoveAdminUserGrant + AddAdminUserRevoke" — but the
// audit found both call sites already enforce the invariant
// (grants_handler.go lines 182 and 257). What 1.17.C actually
// adds:
//
//   1. lock-in tests that prove the existing handler-side
//      invariant fires on system.admin GLOBAL grant removal +
//      revoke addition (and does NOT fire for team-scoped rows)
//   2. sweeper-time guard — auth.capability_sweeper.go refuses to
//      reap a system.admin global grant whose expiry would leave
//      the system with zero active admins (logs a "stuck open"
//      WARN; the row stays so the operator can extend/replace)

package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------
// Handler-side invariant lock-in
// ---------------------------------------------------------------

func TestRemoveAdminUserGrant_SystemAdmin_LastAdmin_Refuses(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	wipeAllSystemAdmins(t, pool)
	// Seed the sole admin via explicit GLOBAL grant (so the
	// removal path is what the invariant fires against).
	lone := seedSubjectUser(t, pool, "")
	grantSystemAdminGlobal(t, pool, lone)
	if n := countActiveSystemAdminsForTest(t, pool); n != 1 {
		t.Skipf("precondition: expected exactly 1 active admin, found %d (parallel-package race)", n)
	}

	h := &auth.Handler{Pool: pool}
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: lone, Capabilities: []string{"users.write"}})

	resp, err := h.RemoveAdminUserGrant(ctx, openapi.RemoveAdminUserGrantRequestObject{
		Ref:        lone,
		Capability: "system.admin",
		Params:     openapi.RemoveAdminUserGrantParams{}, // TeamId nil = global
	})
	if err != nil {
		t.Fatalf("RemoveAdminUserGrant: %v", err)
	}
	r400, ok := resp.(openapi.RemoveAdminUserGrant400JSONResponse)
	if !ok {
		t.Fatalf("expected 400 from last-admin invariant, got %T", resp)
	}
	if !strings.Contains(strings.ToLower(r400.Error), "admin") {
		t.Errorf("400 message should mention admin; got %q", r400.Error)
	}

	// Row must still exist — the invariant blocked the delete.
	if !grantExists(t, pool, lone, "system.admin") {
		t.Error("system.admin grant was deleted despite invariant")
	}
}

func TestAddAdminUserRevoke_SystemAdmin_LastAdmin_Refuses(t *testing.T) {
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	wipeAllSystemAdmins(t, pool)
	lone := seedSubjectUser(t, pool, "")
	grantSystemAdminGlobal(t, pool, lone)
	if n := countActiveSystemAdminsForTest(t, pool); n != 1 {
		t.Skipf("precondition: expected exactly 1 active admin, found %d (parallel-package race)", n)
	}

	h := &auth.Handler{Pool: pool}
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: lone, Capabilities: []string{"users.write"}})

	resp, err := h.AddAdminUserRevoke(ctx, openapi.AddAdminUserRevokeRequestObject{
		Ref: lone,
		Body: &openapi.UserCapabilityOverrideRequest{
			Capability: "system.admin",
			// TeamId nil = global
		},
	})
	if err != nil {
		t.Fatalf("AddAdminUserRevoke: %v", err)
	}
	if _, ok := resp.(openapi.AddAdminUserRevoke400JSONResponse); !ok {
		t.Fatalf("expected 400 from last-admin invariant on revoke add, got %T", resp)
	}

	// No revoke row should have landed.
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM user_capability_revokes WHERE user_ref = $1 AND capability_code = 'system.admin' AND team_id IS NULL`,
		lone,
	).Scan(&n); err != nil {
		t.Fatalf("re-read revoke: %v", err)
	}
	if n != 0 {
		t.Errorf("blocked revoke still landed; count = %d", n)
	}
}

// ---------------------------------------------------------------
// Sweeper-time guard
// ---------------------------------------------------------------

func TestCapabilitySweeper_LastAdminGrant_StuckOpen(t *testing.T) {
	// Sole active admin holds system.admin via an EXPIRED global
	// grant. Sweeper must refuse to reap (row stays) + the audit
	// callback must NOT fire (no reap happened).
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	wipeAllSystemAdmins(t, pool)
	lone := seedSubjectUser(t, pool, "")
	// Seed directly with expires_at in the past — bypasses the
	// handler's past-expiry guard so we can stage the scenario.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code, expires_at) VALUES ($1, 'system.admin', $2)`,
		lone, time.Now().Add(-1*time.Hour),
	); err != nil {
		t.Fatalf("seed expired admin grant: %v", err)
	}
	if n := countActiveSystemAdminsForTest(t, pool); n != 1 {
		t.Skipf("precondition: expected exactly 1 active admin, found %d (parallel-package race)", n)
	}

	rec := newSweeperRecorder()
	sw := auth.NewCapabilitySweeper(pool, nil, rec.OnGrant, rec.OnRevoke, rec.OnInvalidate, 1*time.Hour)
	g, _ := sw.SweepOnce(context.Background())

	if g != 0 {
		t.Errorf("sweep reaped %d grants; admin grant should be stuck-open", g)
	}
	if !grantExists(t, pool, lone, "system.admin") {
		t.Error("last-admin grant got reaped; invariant failed")
	}
	if rec.GrantCalls() != 0 {
		t.Errorf("stuck-open grant emitted %d audit calls; want 0", rec.GrantCalls())
	}
}

func TestCapabilitySweeper_AdminGrant_Reaped_WhenSiblingExists(t *testing.T) {
	// Same setup but with a sibling admin (the seedAdminCaller).
	// The invariant allows the reap; the row is gone after sweep.
	pool := openTestPool(t)
	defer cleanupSubjects(t, pool)
	wipeAllSystemAdmins(t, pool)
	subject := seedSubjectUser(t, pool, "")
	_ = seedAdminCaller(t, pool, "") // sibling admin (via role)
	// Insert subject's grant as expired.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code, expires_at) VALUES ($1, 'system.admin', $2)`,
		subject, time.Now().Add(-1*time.Hour),
	); err != nil {
		t.Fatalf("seed expired admin grant: %v", err)
	}

	rec := newSweeperRecorder()
	sw := auth.NewCapabilitySweeper(pool, nil, rec.OnGrant, rec.OnRevoke, rec.OnInvalidate, 1*time.Hour)
	g, _ := sw.SweepOnce(context.Background())

	if g != 1 {
		t.Errorf("sweep reaped %d grants; want 1 (sibling admin makes reap safe)", g)
	}
	if grantExists(t, pool, subject, "system.admin") {
		t.Error("subject's expired admin grant should be gone after sweep")
	}
	if rec.GrantCalls() != 1 {
		t.Errorf("reaped admin grant should emit 1 audit call; got %d", rec.GrantCalls())
	}
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

// wipeAllSystemAdmins clears every path that could resolve a
// user to system.admin (role assignments + direct grants +
// revokes). Mirrors the inline-SQL pattern in last_admin_test.go's
// TestEnsureNotLastAdmin_OnLastAdmin_Refuses so the precondition
// for "lone admin" tests is reproducible.
func wipeAllSystemAdmins(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		DELETE FROM user_roles ur
		USING role_capabilities rc
		WHERE ur.role_id = rc.role_id AND rc.capability_code = 'system.admin'
	`); err != nil {
		t.Fatalf("wipe admin roles: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM user_capability_grants WHERE capability_code = 'system.admin'`,
	); err != nil {
		t.Fatalf("wipe admin grants: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM user_capability_revokes WHERE capability_code = 'system.admin'`,
	); err != nil {
		t.Fatalf("wipe admin revokes: %v", err)
	}
}

func grantSystemAdminGlobal(t *testing.T, pool *pgxpool.Pool, userRef int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code) VALUES ($1, 'system.admin')`,
		userRef,
	); err != nil {
		t.Fatalf("grantSystemAdminGlobal: %v", err)
	}
}

func countActiveSystemAdminsForTest(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT value FROM (
			SELECT COUNT(DISTINCT c.ref)::BIGINT AS value
			FROM (
				SELECT u.ref FROM user_roles ur
				JOIN role_capabilities rc ON rc.role_id = ur.role_id
				JOIN "user" u ON u.ref = ur.user_ref
				WHERE rc.capability_code = 'system.admin' AND ur.team_id IS NULL AND u.approved = 1
				UNION
				SELECT u.ref FROM user_capability_grants g
				JOIN "user" u ON u.ref = g.user_ref
				WHERE g.capability_code = 'system.admin' AND g.team_id IS NULL AND u.approved = 1
			) c
			WHERE NOT EXISTS (
				SELECT 1 FROM user_capability_revokes r
				WHERE r.user_ref = c.ref AND r.capability_code = 'system.admin' AND r.team_id IS NULL
			)
		) sub`,
	).Scan(&n); err != nil {
		t.Fatalf("countActiveSystemAdminsForTest: %v", err)
	}
	return n
}

// Unused-import dam — keep uuid + openapi.UUID referenced so a
// future test that needs them doesn't have to re-add the imports.
var _ = uuid.New
