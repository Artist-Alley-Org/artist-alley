// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The invariant behind `aa seed --reset` (#1274): when the command
// returns, the bootstrap admin is still an admin.
//
// # Why this is stated as an invariant and not as a rule about the schema
//
// The defect it guards was a legal foreign key. `seed.Reset` truncates
// `assets ... CASCADE`; migration 00047 gave `teams` a `hero_asset_id`
// reference to `assets`; `user_roles`, `user_capability_grants` and
// `user_capability_revokes` all reference `teams`. CASCADE follows
// foreign keys transitively and does not care what their ON DELETE
// action says, so truncating assets emptied all three and took the
// bootstrap admin's GLOBAL role with them.
//
// #361 had already fixed that exact end state once, by taking `teams`
// out of the TRUNCATE list and deleting its rows one at a time instead.
// Its guard is a rule about the statement — "do not NAME teams" — and
// 00047 re-opened the same hole a month later without naming anything.
// A stricter rule about the cascade graph would have fared no better:
// there is nothing wrong with the foreign key.
//
// So the assertion here is about the OUTCOME an operator cares about,
// and it keeps working for whatever the next migration adds:
//
//	after `--reset`, `admin` exists, holds the global Admin role, and
//	CountSystemAdmins() — the query the login gate, the last-admin
//	invariant and the setup wizard all read — sees exactly that.
//
// # Why it lives in package main
//
// Because `resetContent` is the unit under test, and the ordering IS the
// fix. `seed.Reset` on its own is not broken and this file must not
// pretend otherwise: it destroys content, the admin's role included,
// which is what its caller asked for. What was missing was the second
// `bootstrap.Run` afterwards, and that composition only exists here.
// A test that called `seed.Reset` and `bootstrap.Run` itself would pass
// on the pre-fix tree — it would be asserting that bootstrap repairs an
// admin, which was never in doubt, rather than that `aa seed --reset`
// calls it. Verified the other way round too: with the second
// `bootstrap.Run` removed from resetContent, every assertion below
// fails.

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/bootstrap"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func resetAdminEnvOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func openResetAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	dsn := fmt.Sprintf("host=%s port=%s user=%s dbname=%s sslmode=disable password=%s",
		resetAdminEnvOr("AA_DB_HOST", "postgres"), resetAdminEnvOr("AA_DB_PORT", "5432"),
		resetAdminEnvOr("AA_DB_USER", "artist_alley"), testdb.Name(t), pwd)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// openResetAdminPoolNamed is openResetAdminPool with a distinctive
// application_name, so a wait observation in pg_stat_activity can name
// exactly which backend it is watching.
func openResetAdminPoolNamed(t *testing.T, appName string) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	dsn := fmt.Sprintf("host=%s port=%s user=%s dbname=%s sslmode=disable password=%s application_name=%s",
		resetAdminEnvOr("AA_DB_HOST", "postgres"), resetAdminEnvOr("AA_DB_PORT", "5432"),
		resetAdminEnvOr("AA_DB_USER", "artist_alley"), testdb.Name(t), pwd, appName)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// adminAuthority is the post-reset state this file is about, read three
// independent ways so a partial repair cannot look like a whole one.
type adminAuthority struct {
	userExists  bool
	globalRoles int   // user_roles rows for `admin` with team_id IS NULL
	systemAdmin int64 // CountSystemAdmins() — the capability, not the row
}

func readAdminAuthority(t *testing.T, pool *pgxpool.Pool) adminAuthority {
	t.Helper()
	ctx := t.Context()
	var got adminAuthority

	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM "user" WHERE username = $1)`,
		bootstrap.DefaultUsername).Scan(&got.userExists); err != nil {
		t.Fatalf("probe admin user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM user_roles ur
		  JOIN "user" u ON u.ref = ur.user_ref
		  JOIN roles r  ON r.id  = ur.role_id
		 WHERE u.username = $1 AND ur.team_id IS NULL AND r.name = 'Admin'`,
		bootstrap.DefaultUsername).Scan(&got.globalRoles); err != nil {
		t.Fatalf("probe admin global role: %v", err)
	}
	n, err := auth.New(pool).CountSystemAdmins(ctx)
	if err != nil {
		t.Fatalf("CountSystemAdmins: %v", err)
	}
	got.systemAdmin = n
	return got
}

func assertAdminUsable(t *testing.T, pool *pgxpool.Pool, when string) {
	t.Helper()
	got := readAdminAuthority(t, pool)
	if !got.userExists {
		t.Fatalf("%s: the `%s` user is gone. seed.Reset is specified to keep it "+
			"(DELETE FROM \"user\" WHERE username <> $1) — if that changed, `aa seed --reset` "+
			"now needs an operator to re-run the setup wizard.", when, bootstrap.DefaultUsername)
	}
	if got.globalRoles != 1 {
		t.Errorf("%s: `%s` holds %d global Admin role row(s), want exactly 1. "+
			"Something in the reset emptied user_roles and nothing put it back — this is "+
			"#1274. The instance answers 200 to /auth/login and 403 to every admin endpoint "+
			"until the SERVER is restarted, because run() bootstraps again on boot and the "+
			"seeder is a separate process.", when, bootstrap.DefaultUsername, got.globalRoles)
	}
	if got.systemAdmin < 1 {
		t.Errorf("%s: CountSystemAdmins() == %d. Whatever holds the role row, the "+
			"system.admin capability does not resolve — /auth/me answers "+
			"\"capabilities\": [] and the setup wizard reports needs_setup.",
			when, got.systemAdmin)
	}
}

// TestResetContent_KeepsTheBootstrapAdminUsable drives the exact
// sequence `aa seed --reset` runs, twice, and asserts the invariant
// after each pass.
func TestResetContent_KeepsTheBootstrapAdminUsable(t *testing.T) {
	pool := openResetAdminPool(t)
	ctx := t.Context()

	// The federation keypair bootstrap.Run mints is wrapped with the
	// at-rest cipher; production wires it from AA_MASTER_KEY.
	if !atrest.Initialised() {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i + 1)
		}
		if err := atrest.InitWithKey(key); err != nil {
			t.Fatalf("atrest: %v", err)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// DefaultAdminEnabled so the run cannot depend on ambient config,
	// and AdminPath inside the test's own directory so nothing here can
	// touch a real /var/lib/artist-alley/bootstrap-admin.txt.
	cfg := bootstrap.Config{
		ScrambleKey:         "reset-admin-test-scramble-key",
		AdminPath:           t.TempDir(),
		DefaultAdminEnabled: true,
	}

	// Precondition. Run creates `admin` on a database that has no system
	// admin at all and repairs it on one that has the user but not the
	// role — both are the states this suite's other packages leave
	// behind, and both are fine. What it will NOT do is create `admin`
	// while some OTHER user holds system.admin, because it no-ops on
	// CountSystemAdmins() > 0; so assert the precondition rather than
	// assume it, and say which of the two it was.
	if err := bootstrap.Run(ctx, pool, cfg, logger, nil); err != nil {
		t.Fatalf("bootstrap.Run (precondition): %v", err)
	}
	if before := readAdminAuthority(t, pool); !before.userExists || before.globalRoles != 1 {
		t.Skipf("precondition not met: `%s` exists=%v with %d global Admin role(s) — another "+
			"package left a different system admin in this database, so bootstrap.Run "+
			"no-opped and there is no admin here to lose",
			bootstrap.DefaultUsername, before.userExists, before.globalRoles)
	}

	// THE TRANSITIVE PATH, planted explicitly. `teams` reaches the role
	// tables and `assets` reaches `teams`, so this is the shape that
	// turns a content truncate into a loss of authority. Planted rather
	// than assumed because an empty `teams` would still be TRUNCATEd by
	// the cascade (TRUNCATE does not care whether the child has rows) —
	// but a fixture that lands proves the chain exists in the schema
	// this test is running against, which is the half a future migration
	// could remove.
	plantTeamWithHeroAsset(t, pool)

	if err := resetContent(ctx, pool, cfg, logger); err != nil {
		t.Fatalf("resetContent (first): %v", err)
	}
	assertAdminUsable(t, pool, "after one --reset")

	// Twice in a row. bootstrap.Run is documented idempotent; the second
	// pass is where "idempotent" stops being a claim. It also covers the
	// case an operator actually hits — a reset against an instance that
	// was already reset, where the first pass left `admin` with a role
	// and the second must not take it away again.
	if err := resetContent(ctx, pool, cfg, logger); err != nil {
		t.Fatalf("resetContent (second): %v", err)
	}
	assertAdminUsable(t, pool, "after a second consecutive --reset")
}

// TestResetContent_RecreatesTheAdminWhenItIsGone is the empty case: a
// reset on an instance with NO admin at all. bootstrap.Run CREATES here
// rather than repairs, which is the branch that picks a password — so it
// is also the branch AA_BOOTSTRAP_DEFAULT_ADMIN gates, and the one place
// where the answer differs by configuration.
//
// With the flag ON (the demo/dev shape, and what the dogfood stack runs)
// the account comes back with the published default password.
//
// With it OFF, the account still comes back and still holds the role —
// asserted below with DefaultAdminEnabled:false — but with a fresh
// RANDOM password, announced in the log and written to
// <AdminPath>/bootstrap-admin.txt. The previous password does not
// survive, because the user row did not. That is the correct answer for
// a create, and it is the reason the recovery branch must NOT announce:
// there the password is untouched, and saying otherwise would overwrite
// a live credential file with a string that opens nothing.
func TestResetContent_RecreatesTheAdminWhenItIsGone(t *testing.T) {
	pool := openResetAdminPool(t)
	ctx := t.Context()

	if !atrest.Initialised() {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i + 1)
		}
		if err := atrest.InitWithKey(key); err != nil {
			t.Fatalf("atrest: %v", err)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	adminDir := t.TempDir()
	cfg := bootstrap.Config{
		ScrambleKey:         "reset-admin-test-scramble-key",
		AdminPath:           adminDir,
		DefaultAdminEnabled: false, // the secure default: random password
	}

	// Empty the instance of its admin the way a botched restore would.
	// Scoped to `admin`'s OWN rows, not `DELETE FROM user_roles`: this
	// runs against the suite's shared test database, and a blanket
	// delete would be indistinguishable from a fixture wipe to whatever
	// package runs next.
	for _, stmt := range []string{
		`DELETE FROM user_roles WHERE user_ref IN (SELECT ref FROM "user" WHERE username = $1)`,
		`DELETE FROM user_capability_grants WHERE user_ref IN (SELECT ref FROM "user" WHERE username = $1)`,
		`DELETE FROM "user" WHERE username = $1`,
	} {
		if _, err := pool.Exec(ctx, stmt, bootstrap.DefaultUsername); err != nil {
			t.Fatalf("empty the instance (%s): %v", stmt, err)
		}
	}
	if n, err := auth.New(pool).CountSystemAdmins(ctx); err != nil {
		t.Fatalf("CountSystemAdmins: %v", err)
	} else if n != 0 {
		t.Skipf("another package left %d system admin(s) in this database, so this is not "+
			"the empty case", n)
	}
	// Hand the database back with the admin the rest of the suite would
	// have found: this test deliberately leaves one holding a RANDOM
	// password, which is correct for the case it covers and wrong to
	// leave lying around.
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, stmt := range []string{
			`DELETE FROM user_roles WHERE user_ref IN (SELECT ref FROM "user" WHERE username = $1)`,
			`DELETE FROM "user" WHERE username = $1`,
		} {
			if _, err := pool.Exec(c, stmt, bootstrap.DefaultUsername); err != nil {
				t.Errorf("restore admin (%s): %v", stmt, err)
			}
		}
		demo := cfg
		demo.DefaultAdminEnabled = true
		if err := bootstrap.Run(c, pool, demo, logger, nil); err != nil {
			t.Errorf("restore admin: %v", err)
		}
	})

	if err := resetContent(ctx, pool, cfg, logger); err != nil {
		t.Fatalf("resetContent: %v", err)
	}
	assertAdminUsable(t, pool, "after --reset on an instance with no admin")

	// The create branch announced, which means it wrote the password
	// file. Its presence is the observable difference between "created"
	// and "repaired" and is what the recovery branch must not produce.
	if _, err := os.Stat(adminDir + "/bootstrap-admin.txt"); err != nil {
		t.Errorf("the create branch did not write bootstrap-admin.txt (%v). With "+
			"AA_BOOTSTRAP_DEFAULT_ADMIN off the random password exists nowhere else, so an "+
			"operator who reset an admin-less instance would have no way back in.", err)
	}
}

// plantTeamWithHeroAsset builds assets → teams → user_roles, the chain
// that carries a content truncate into the role tables.
func plantTeamWithHeroAsset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()

	var ownerRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Reset Hero Owner', 1)
		 RETURNING ref`,
		"reset-hero-"+strconv.FormatInt(time.Now().UnixNano()%1e9, 36)).Scan(&ownerRef); err != nil {
		t.Fatalf("hero owner: %v", err)
	}
	var assetTypeRef int64
	if err := pool.QueryRow(ctx,
		`SELECT ref FROM asset_types ORDER BY ref LIMIT 1`).Scan(&assetTypeRef); err != nil {
		t.Fatalf("asset type: %v", err)
	}
	assetID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, owner_user_ref, title, asset_type) VALUES ($1, $2, 'Hero', $3)`,
		assetID, ownerRef, assetTypeRef); err != nil {
		t.Fatalf("hero asset: %v", err)
	}
	teamID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, name, slug, hero_asset_id) VALUES ($1, 'Reset Team', $2, $3)`,
		teamID, "reset-team-"+teamID.String()[:8], assetID); err != nil {
		t.Fatalf("team with hero asset — if this failed because the column is gone, the "+
			"transitive path this test plants no longer exists and the fixture needs "+
			"re-pointing at whatever replaced it: %v", err)
	}
	// A TEAM-scoped role for the same owner. It is expected to die with
	// the team, and saying so here is what keeps the assertions above
	// honestly scoped to the GLOBAL role.
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_roles (user_ref, role_id, team_id)
		SELECT $1, r.id, $2 FROM roles r WHERE r.name = 'Admin'`,
		ownerRef, teamID); err != nil {
		t.Fatalf("team-scoped role: %v", err)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ownerRef)
	})
}
