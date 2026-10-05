// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// openTestPool wires a pgxpool against the compose stack. Skips
// when AA_DB_PASSWORD is unset (matches the convention across the
// rest of app/internal integration tests).
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	name := testdb.Name(t)
	user := envOr("AA_DB_USER", "artist_alley")

	dsn := "host=" + host + " port=" + port + " user=" + user + " password=" + pwd + " dbname=" + name + " sslmode=disable"
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgxpool parse config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgxpool new: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestCheckSchemaFreshness_OK exercises the happy path: after
// db.Migrate ran (implicit in the compose stack's boot), the DB max
// equals the embedded max. This is the case operator boots run
// millions of times a day; verifying it explicitly here means a
// broken parse or a stale goose_db_version schema fails the test
// loudly instead of silently drifting.
func TestCheckSchemaFreshness_OK(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)

	got, err := CheckSchemaFreshness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckSchemaFreshness: %v", err)
	}
	if got.Status != SchemaOK {
		t.Fatalf("Status = %v (%d); want SchemaOK. embeddedMax=%d dbMax=%d warning=%q",
			got.Status, int(got.Status), got.EmbeddedMaxVer, got.DBMaxVer, got.Warning)
	}
	if got.Warning != "" {
		t.Errorf("Warning = %q; want empty on SchemaOK", got.Warning)
	}
	if got.EmbeddedMaxVer == 0 {
		t.Errorf("EmbeddedMaxVer = 0; migrations FS embed must be reachable")
	}
	if got.DBMaxVer != got.EmbeddedMaxVer {
		t.Errorf("DBMaxVer=%d != EmbeddedMaxVer=%d despite SchemaOK", got.DBMaxVer, got.EmbeddedMaxVer)
	}
}

// TestCheckSchemaFreshness_UnknownNewerSchema simulates the
// "running old binary against newer DB" case by INSERTing a fake
// version_id past the embedded max. Cleanup rolls the row back so
// the compose stack's DB is unaffected by test order.
func TestCheckSchemaFreshness_UnknownNewerSchema(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)

	// Pick a version well past any plausible embedded max so
	// concurrent-test-run ordering never accidentally matches it.
	const fakeFutureVersion = 99999

	if _, err := pool.Exec(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`,
		fakeFutureVersion); err != nil {
		t.Fatalf("seed fake future version: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM goose_db_version WHERE version_id = $1`, fakeFutureVersion)
	})

	got, err := CheckSchemaFreshness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckSchemaFreshness: %v", err)
	}
	if got.Status != SchemaUnknownNewerSchema {
		t.Fatalf("Status = %v; want SchemaUnknownNewerSchema. embeddedMax=%d dbMax=%d",
			got.Status, got.EmbeddedMaxVer, got.DBMaxVer)
	}
	if got.Warning == "" {
		t.Errorf("Warning must be populated on SchemaUnknownNewerSchema; got empty")
	}
	if got.DBMaxVer != fakeFutureVersion {
		t.Errorf("DBMaxVer = %d; want %d (fake future version)", got.DBMaxVer, fakeFutureVersion)
	}
}

// TestCheckSchemaFreshness_UnappliedMigrations exercises the
// defensive branch: dbMax < embeddedMax. Simulated by DELETEing the
// current-head row from goose_db_version (so the DB "forgets" the
// most recent migration) — the schema itself is unchanged, only the
// tracking table is rolled back. Cleanup re-INSERTs the row so
// subsequent tests aren't confused.
func TestCheckSchemaFreshness_UnappliedMigrations(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)

	var currentMax int64
	if err := pool.QueryRow(ctx,
		`SELECT MAX(version_id) FROM goose_db_version`).Scan(&currentMax); err != nil {
		t.Fatalf("read current max: %v", err)
	}
	if currentMax == 0 {
		t.Skip("no migrations applied; can't simulate unapplied case")
	}

	// Snapshot the row so we can restore it exactly.
	var isApplied bool
	if err := pool.QueryRow(ctx,
		`SELECT is_applied FROM goose_db_version WHERE version_id = $1`, currentMax).Scan(&isApplied); err != nil {
		t.Fatalf("snapshot current: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM goose_db_version WHERE version_id = $1`, currentMax); err != nil {
		t.Fatalf("simulate unapplied: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, $2)
			 ON CONFLICT (version_id) DO NOTHING`, currentMax, isApplied)
	})

	got, err := CheckSchemaFreshness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckSchemaFreshness: %v", err)
	}
	if got.Status != SchemaUnappliedMigrations {
		t.Fatalf("Status = %v; want SchemaUnappliedMigrations. embeddedMax=%d dbMax=%d",
			got.Status, got.EmbeddedMaxVer, got.DBMaxVer)
	}
	if got.Warning == "" {
		t.Errorf("Warning must be populated on SchemaUnappliedMigrations; got empty")
	}
	if got.EmbeddedMaxVer <= got.DBMaxVer {
		t.Errorf("embeddedMax=%d must exceed dbMax=%d for this test to be meaningful",
			got.EmbeddedMaxVer, got.DBMaxVer)
	}
}

// TestSchemaStatus_String smoke-tests the stringer covers every
// enum value so a new SchemaStatus addition breaks this test loudly.
func TestSchemaStatus_String(t *testing.T) {
	cases := map[SchemaStatus]string{
		SchemaOK:                  "ok",
		SchemaUnappliedMigrations: "unapplied_migrations",
		SchemaUnknownNewerSchema:  "unknown_newer_schema",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("(%d).String() = %q; want %q", int(s), got, want)
		}
	}
}
