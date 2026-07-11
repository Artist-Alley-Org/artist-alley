// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Last-admin invariant tests. Real Postgres; skips without
// AA_DB_PASSWORD. Covers the four mutation paths the
// invariant guards (deactivate / demote / revoke-grant /
// add-revoke) at the helper layer — the handler-layer test
// shims would require wiring the full request stack, which
// these guards don't need; the helper's contract IS the
// invariant.

package auth_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0xf]
	}
	return string(out)
}

// seedAdmin creates an approved user + assigns the seeded
// "Admin" role (which grants system.admin per migration 00002).
func seedAdmin(t *testing.T, pool *pgxpool.Pool, label string) int64 {
	t.Helper()
	ctx := context.Background()
	q := auth.New(pool)

	username := "lastadmin-" + label + "-" + randHex(4)
	pw := "irrelevant" // not used for login in these tests
	user, err := q.CreateUser(ctx, auth.CreateUserParams{
		Username: &username,
		Password: &pw,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	role, err := q.FindRoleByName(ctx, "Admin")
	if err != nil {
		t.Fatalf("find Admin role: %v", err)
	}
	if err := q.SetUserGlobalRole(ctx, auth.SetUserGlobalRoleParams{
		UserRef: user.Ref,
		RoleID:  role.ID,
	}); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_roles WHERE user_ref = $1`, user.Ref)
		_, _ = pool.Exec(c, `DELETE FROM user_capability_grants WHERE user_ref = $1`, user.Ref)
		_, _ = pool.Exec(c, `DELETE FROM user_capability_revokes WHERE user_ref = $1`, user.Ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, user.Ref)
	})
	return user.Ref
}

func TestEnsureNotLastAdmin_NotAnAdmin_Passes(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	q := auth.New(pool)

	// User without admin role/grant — guard MUST pass regardless
	// of total admin count.
	username := "noadmin-" + randHex(4)
	pw := "irrelevant"
	user, err := q.CreateUser(ctx, auth.CreateUserParams{
		Username: &username, Password: &pw,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, user.Ref)
	})

	if err := auth.EnsureNotLastAdmin(ctx, q, user.Ref); err != nil {
		t.Errorf("non-admin guard: got %v want nil", err)
	}
}

func TestEnsureNotLastAdmin_WhenOtherAdminsExist_Passes(t *testing.T) {
	pool := openTestPool(t)
	q := auth.New(pool)
	a := seedAdmin(t, pool, "a")
	_ = seedAdmin(t, pool, "b") // exists so the guard on a passes

	if err := auth.EnsureNotLastAdmin(context.Background(), q, a); err != nil {
		t.Errorf("with sibling admin: got %v want nil", err)
	}
}

func TestEnsureNotLastAdmin_OnLastAdmin_Refuses(t *testing.T) {
	pool := openTestPool(t)
	q := auth.New(pool)

	// Wipe any existing admins (including the bootstrap admin)
	// so the seeded user is the ONLY one in this test's view.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		DELETE FROM user_roles ur
		USING role_capabilities rc
		WHERE ur.role_id = rc.role_id AND rc.capability_code = 'system.admin'
	`); err != nil {
		t.Fatalf("wipe admins: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM user_capability_grants WHERE capability_code = 'system.admin'`); err != nil {
		t.Fatalf("wipe grants: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM user_capability_revokes WHERE capability_code = 'system.admin'`); err != nil {
		t.Fatalf("wipe revokes: %v", err)
	}

	a := seedAdmin(t, pool, "lonely")

	err := auth.EnsureNotLastAdmin(ctx, q, a)
	if !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("last-admin guard: got %v want ErrLastAdmin", err)
	}
}

func TestUserHoldsSystemAdmin_ViaExplicitGrant_Counts(t *testing.T) {
	// A user with NO admin role but an explicit system.admin
	// grant still holds the capability. The guard MUST count
	// them as an admin (revoking the grant would strip powers).
	pool := openTestPool(t)
	q := auth.New(pool)
	ctx := context.Background()

	username := "grantadmin-" + randHex(4)
	pw := "irrelevant"
	user, err := q.CreateUser(ctx, auth.CreateUserParams{Username: &username, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_capability_grants (user_ref, capability_code) VALUES ($1, 'system.admin')`,
		user.Ref,
	); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_capability_grants WHERE user_ref = $1`, user.Ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, user.Ref)
	})

	holds, err := q.UserHoldsSystemAdmin(ctx, user.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if holds == 0 {
		t.Error("grant-only admin: UserHoldsSystemAdmin returned 0; expected 1")
	}
}

func TestUserHoldsSystemAdmin_WhenRevokeOverrides_DoesNotCount(t *testing.T) {
	// A user with admin role + an EXPLICIT REVOKE of system.admin
	// does NOT hold the capability per the resolution contract.
	pool := openTestPool(t)
	q := auth.New(pool)
	ctx := context.Background()

	a := seedAdmin(t, pool, "revoked")
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_capability_revokes (user_ref, capability_code) VALUES ($1, 'system.admin')`,
		a,
	); err != nil {
		t.Fatalf("seed revoke: %v", err)
	}

	holds, err := q.UserHoldsSystemAdmin(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if holds != 0 {
		t.Errorf("revoke-overridden admin: holds=%d want 0", holds)
	}
}

func TestRoleGrantsSystemAdmin_AdminRoleReturnsTrue(t *testing.T) {
	pool := openTestPool(t)
	q := auth.New(pool)
	role, err := q.FindRoleByName(context.Background(), "Admin")
	if err != nil {
		t.Fatal(err)
	}
	grants, err := q.RoleGrantsSystemAdmin(context.Background(), role.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grants == 0 {
		t.Error("Admin role: RoleGrantsSystemAdmin returned 0; expected 1")
	}
}

// pgtype.UUID is used elsewhere in the auth package; keep the
// import live without forcing a dependency in this test.
var _ pgtype.UUID
