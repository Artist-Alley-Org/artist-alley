// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #574 — `aa seed` must be able to populate a database nobody has
// migrated yet.
//
// The nightly failed because the seeder's first phase (resolveLookups)
// reads `user` / `workflow_states` / `asset_types`, but only the serve
// path ever called Migrate. `docker compose up -d` returns before the
// app finishes migrating and the seed runs with `--no-deps`, so
// whichever site lost the race died on
// `relation "user" does not exist` and left an empty instance for every
// UI spec to assert against.
//
// These tests pin the two properties that fix depends on, against a
// REAL, freshly-created database rather than the shared dev one:
//
//   - Migrate on a never-migrated database produces exactly the tables
//     resolveLookups needs.
//   - Migrate is safe to call concurrently, because after the fix BOTH
//     the booting server and the seed process migrate the same database
//     within seconds of each other.
//
// Skips (does not fail) when AA_DB_PASSWORD is unset, matching the
// other integration tests.

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/mscrnt/artist-alley/app/internal/config"
)

// envOr is shared with schema_freshness_test.go in this package.

// freshDatabase creates a brand-new, empty database and returns a
// config pointed at it. Dropped on cleanup.
func freshDatabase(t *testing.T) config.Config {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	port, err := strconv.Atoi(envOr("AA_DB_PORT", "5432"))
	if err != nil {
		t.Fatalf("bad AA_DB_PORT: %v", err)
	}
	cfg := config.Config{
		DBHost:     envOr("AA_DB_HOST", "postgres"),
		DBPort:     port,
		DBUser:     envOr("AA_DB_USER", "artist_alley"),
		DBPassword: pwd,
		DBName:     envOr("AA_DB_NAME", "artist_alley"),
		DBSSLMode:  "disable",
	}

	adminDSN := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName)
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	ctx := t.Context()

	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("dev Postgres not reachable (%v); skipping", err)
	}

	// Unique per test so parallel runs never collide. Postgres
	// identifiers cap at 63 bytes; this is comfortably short.
	name := fmt.Sprintf("aa_migrate_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		// Cleanup runs after the test's context is cancelled, so this
		// keeps its own deadline — t.Context() here would be dead on
		// arrival and the cleanup a silent no-op (#622).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()

		// FORCE terminates any lingering backend (the advisory-lock
		// connections in the concurrency test) so the drop can't hang.
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: drop database %s: %v", name, err)
		}
		admin.Close()
	})

	cfg.DBName = name
	return cfg
}

func openCfg(t *testing.T, cfg config.Config) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", cfg.DBName, err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB
}

func tableExists(t *testing.T, sqlDB *sql.DB, table string) bool {
	t.Helper()
	ctx := t.Context()

	var exists bool
	err := sqlDB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema='public' AND table_name=$1)`, table).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return exists
}

// The exact tables Runner.resolveLookups reads before anything else.
// If Migrate leaves any of them missing, `aa seed` dies on its first
// phase — which is the bug.
var resolveLookupsTables = []string{"user", "workflow_states", "asset_types"}

func TestMigrate_FreshDatabaseGivesSeederItsSchema(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)

	// Precondition: genuinely unmigrated. Without this the test could
	// pass against an already-migrated database and prove nothing.
	for _, tbl := range resolveLookupsTables {
		if tableExists(t, sqlDB, tbl) {
			t.Fatalf("precondition failed: %q already exists in a fresh database", tbl)
		}
	}
	ctx := t.Context()

	if err := Migrate(ctx, cfg); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}

	for _, tbl := range resolveLookupsTables {
		if !tableExists(t, sqlDB, tbl) {
			t.Errorf("after Migrate, %q is still missing — the seeder's resolveLookups would fail", tbl)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	cfg := freshDatabase(t)
	ctx := t.Context()

	// Twice in a row: a re-seed of an existing instance runs Migrate
	// against a database that is already fully migrated.
	for i := 1; i <= 2; i++ {
		if err := Migrate(ctx, cfg); err != nil {
			t.Fatalf("Migrate run %d: %v", i, err)
		}
	}
	if !tableExists(t, openCfg(t, cfg), "user") {
		t.Error(`"user" missing after two migrate runs`)
	}
}

// After the fix the booting server AND the seed process both migrate the
// same database, seconds apart, on a database that starts empty. Without
// the advisory lock both would see version 0 pending, both would apply
// 00001, and the loser would fail on `relation "user" already exists` —
// trading one race for another.
func TestMigrate_ConcurrentCallersAllSucceed(t *testing.T) {
	cfg := freshDatabase(t)

	const callers = 4
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})

	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := t.Context()

			<-start // release together, maximising overlap
			errs[i] = Migrate(ctx, cfg)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate caller %d failed: %v", i, err)
		}
	}
	if !tableExists(t, openCfg(t, cfg), "user") {
		t.Error(`"user" missing after concurrent migrate`)
	}
}
