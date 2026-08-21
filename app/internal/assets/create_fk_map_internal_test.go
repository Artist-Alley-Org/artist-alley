// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #966, the anti-drift half — an internal test, because the thing under
// test is the completeness of `createAssetFKConstraints` itself and an
// external test can only restate it.
//
// asset_fk_leak_test.go proves the three known keys behave. This proves
// nobody can add a FOURTH without noticing: it reads the foreign keys
// the DATABASE declares on `assets` and asserts every one is either in
// the 400 map or on the short excluded list with a reason. A migration
// that FKs a new client-writable column fails here rather than shipping
// a fresh 500 with a fresh constraint name in it.
//
// That is the whole lesson of #941/#946 mechanised: "when a rule covers
// a column, enumerate its siblings" only holds if something enumerates
// them for you.
//
// Skips without AA_DB_PASSWORD.

package assets

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// fkMapPool is a bare pool — this test needs no handler, no storage and
// no fixtures, only the catalogue.
func fkMapPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCreateAssetFKMap_CoversEveryForeignKeyOnAssets(t *testing.T) {
	pool := fkMapPool(t)

	rows, err := pool.Query(context.Background(),
		`SELECT conname FROM pg_constraint
		  WHERE conrelid = 'assets'::regclass AND contype = 'f'
		  ORDER BY conname`)
	if err != nil {
		t.Fatalf("read assets foreign keys: %v", err)
	}
	defer rows.Close()

	var declared []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan constraint name: %v", err)
		}
		declared = append(declared, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate constraints: %v", err)
	}
	// A guard on the guard: if the catalogue query ever stops matching,
	// an empty result would make this test vacuously green — the shape
	// of failure #941 shipped behind.
	if len(declared) == 0 {
		t.Fatal("no foreign keys found on assets — the catalogue query is wrong, not the schema")
	}

	// Handled somewhere else on purpose. A name lands here by decision,
	// with the decision written next to it.
	excluded := map[string]string{
		"assets_team_id_fkey": "answers 404 'team not found' so POST /assets is not a team-existence probe (#953)",
	}

	for _, name := range declared {
		if _, mapped := createAssetFKConstraints[name]; mapped {
			continue
		}
		if why, ok := excluded[name]; ok {
			t.Logf("%s excluded: %s", name, why)
			continue
		}
		t.Errorf("foreign key %q on assets is neither in createAssetFKConstraints nor excluded "+
			"with a reason — a client value violating it surfaces as a 500 carrying its name", name)
	}

	// And the reverse direction: a mapped constraint that no longer
	// exists is dead weight that quietly stops covering anything.
	present := map[string]bool{}
	for _, name := range declared {
		present[name] = true
	}
	for name := range createAssetFKConstraints {
		if !present[name] {
			t.Errorf("createAssetFKConstraints maps %q, which is not a foreign key on assets — "+
				"stale entry, or the constraint was renamed and its violations now leak", name)
		}
	}
}
