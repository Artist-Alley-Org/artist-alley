package fixturesweep_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/fixturesweep"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// The sweep deletes from a shared database, so the property that matters
// is not "it removes fixtures" — it is "it does not remove anything
// else". These tests assert the protective half.

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	dsn := "host=" + envOr("AA_DB_HOST", "postgres") +
		" port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(k, d string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return d
}

// seedNames is what the CLI reads out of dataset.collections.json.
var seedNames = []string{"Project Echo", "Engine Core"}

func assetType(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM asset_types ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Skipf("no asset_types in this database: %v", err)
	}
	return id
}

// The headline property: an asset carrying the seeder's provenance
// marker survives, and one without it does not.
func TestSweep_KeepsProvenancedAssets_RemovesUnmarked(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t)
	at := assetType(t, ctx, pool)

	var realID, fixtureID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO assets (title, asset_type, metadata)
		VALUES ('sweep real asset', $1, '{"acquisition_source":"pexels"}'::jsonb)
		RETURNING id::text`, at).Scan(&realID); err != nil {
		t.Fatalf("insert real: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO assets (title, asset_type, metadata)
		VALUES ('sweep fixture asset', $1, '{}'::jsonb)
		RETURNING id::text`, at).Scan(&fixtureID); err != nil {
		t.Fatalf("insert fixture: %v", err)
	}
	testdb.Purge(t, pool, realID, `DELETE FROM assets WHERE id::text = $1`)

	rep, err := fixturesweep.Run(ctx, pool, seedNames, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep == nil || !rep.Applied {
		t.Fatal("Run reported no applied sweep")
	}

	var realLives, fixtureLives bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM assets WHERE id::text=$1)`, realID).Scan(&realLives); err != nil {
		t.Fatalf("check real: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM assets WHERE id::text=$1)`, fixtureID).Scan(&fixtureLives); err != nil {
		t.Fatalf("check fixture: %v", err)
	}
	if !realLives {
		t.Error("the sweep deleted an asset carrying acquisition_source — this is the unrecoverable failure")
	}
	if fixtureLives {
		t.Error("the sweep left an unmarked asset behind")
	}
}

// A dry run must be observably inert. This is the flag the operator
// relies on before ever passing -apply.
func TestSweep_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t)
	at := assetType(t, ctx, pool)

	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO assets (title, asset_type, metadata) VALUES ('sweep dryrun fixture', $1, '{}'::jsonb)
		RETURNING id::text`, at).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, id, `DELETE FROM assets WHERE id::text = $1`)
	})

	rep, err := fixturesweep.Run(ctx, pool, seedNames, false)
	if err != nil {
		t.Fatalf("Run(dry): %v", err)
	}
	if rep.Applied {
		t.Error("a dry run reported Applied")
	}
	if rep.Tables[0].Fixture == 0 {
		t.Error("the dry run counted no fixtures; it is not exercising the census")
	}
	var lives bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM assets WHERE id::text=$1)`, id).Scan(&lives); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !lives {
		t.Error("the DRY RUN deleted a row — the -apply flag is not what decides writes")
	}
	for _, tr := range rep.Tables {
		if tr.Deleted != 0 {
			t.Errorf("dry run reported %d deletions for %s", tr.Deleted, tr.Table)
		}
	}
}

// A rule that matches a row as both fixture and real must abort the
// WHOLE sweep, not merely skip the overlapping rows. That is the
// property standing between a drifted rule and unrecoverable data loss,
// so it is driven directly: Rules is swapped for one that provably
// contradicts itself, and a fixture asset the real sweep WOULD have
// deleted is left in place as a canary. If the abort works, the canary
// survives.
func TestSweep_ContradictionAbortsEverything(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t)
	at := assetType(t, ctx, pool)

	var canary string
	if err := pool.QueryRow(ctx, `
		INSERT INTO assets (title, asset_type, metadata) VALUES ('sweep abort canary', $1, '{}'::jsonb)
		RETURNING id::text`, at).Scan(&canary); err != nil {
		t.Fatalf("insert canary: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, canary, `DELETE FROM assets WHERE id::text = $1`)
	})

	original := fixturesweep.Rules
	t.Cleanup(func() { fixturesweep.Rules = original })
	fixturesweep.Rules = []fixturesweep.Rule{{
		Table: "assets",
		// Both true for every row: the rule claims everything is a
		// fixture and everything is real.
		Fixture:   `true`,
		Protected: `true`,
		Why:       "deliberately self-contradictory, for this test",
	}}

	rep, err := fixturesweep.Run(ctx, pool, seedNames, true)
	if err == nil {
		t.Fatal("a self-contradictory rule was accepted; the sweep must refuse")
	}
	var ce *fixturesweep.ErrContradiction
	if !errors.As(err, &ce) {
		t.Fatalf("want ErrContradiction, got %T: %v", err, err)
	}
	if ce.Table != "assets" || ce.N == 0 {
		t.Errorf("contradiction reported as table=%q n=%d; both should be populated", ce.Table, ce.N)
	}
	if rep != nil {
		for _, tr := range rep.Tables {
			if tr.Deleted != 0 {
				t.Errorf("%d rows were deleted from %s despite the abort", tr.Deleted, tr.Table)
			}
		}
	}

	var lives bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM assets WHERE id::text=$1)`, canary).Scan(&lives); err != nil {
		t.Fatalf("check canary: %v", err)
	}
	if !lives {
		t.Error("the canary was deleted despite the contradiction abort — the abort is not load-bearing")
	}
}
