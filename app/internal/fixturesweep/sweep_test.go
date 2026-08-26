package fixturesweep_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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

// seedCat is what the CLI reads out of the seed profiles: the real
// collection names and the real post ids.
var seedCat = fixturesweep.Catalogue{
	CollectionNames: []string{"Project Echo", "Engine Core"},
	// A syntactically valid uuid that no row holds, so the posts rule
	// protects nothing here and the assets assertions stay about assets.
	PostIDs: []string{"00000000-0000-0000-0000-000000000001"},
}

// assetType returns a usable asset_types key.
//
// ⛔ IT USED TO ASK FOR `id`, AND asset_types HAS NO `id` — its key is
// `ref`. So the query raised 42703 on every database that has ever
// existed, the blanket `t.Skipf` swallowed it as "no asset_types here",
// and the three tests below — INCLUDING the contradiction-abort canary
// that the whole safety model rests on — silently skipped from the day
// they were written (#1245) until #1276 went looking. A destructive tool
// whose protective tests never execute is the exact shape ADR 0095 was
// written to prevent.
//
// The skip is now narrow on purpose: an EMPTY table is a database that
// has not been seeded and is a fair reason to skip; an ERROR is a broken
// fixture and must be loud. A blanket skip cannot tell them apart, and
// that is how this hid.
func assetType(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var ref int64
	err := pool.QueryRow(ctx, `SELECT ref FROM asset_types ORDER BY ref LIMIT 1`).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Skip("asset_types is empty; this database has not been seeded")
	}
	if err != nil {
		t.Fatalf("reading asset_types: %v — a query error is a broken fixture, "+
			"not a reason to skip a destructive tool's safety test", err)
	}
	return ref
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
	// ⛔ IN t.Cleanup, NOT INLINE. `testdb.Purge` executes immediately, so
	// calling it here deleted the "real" asset BEFORE the sweep ran — and
	// the test then reported the sweep as having destroyed it, which is
	// this file's single most alarming failure message. It said so for the
	// first time in #1276, because until the `asset_types.id` fix above
	// this test had never executed at all.
	t.Cleanup(func() {
		testdb.Purge(t, pool, realID, `DELETE FROM assets WHERE id::text = $1`)
	})

	rep, err := fixturesweep.Run(ctx, pool, seedCat, true)
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

	rep, err := fixturesweep.Run(ctx, pool, seedCat, false)
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
		Table:       "assets",
		IDColumn:    "id",
		LabelColumn: "title",
		// Both true for every row: the rule claims everything is a
		// fixture and everything is real.
		Fixture:   `true`,
		Protected: `true`,
		Why:       "deliberately self-contradictory, for this test",
	}}

	rep, err := fixturesweep.Run(ctx, pool, seedCat, true)
	if err == nil {
		t.Fatal("a self-contradictory rule was accepted; the sweep must refuse")
	}
	var ce *fixturesweep.ErrContradiction
	if !errors.As(err, &ce) {
		t.Fatalf("want ErrContradiction, got %T: %v", err, err)
	}
	if len(ce.Tables) != 1 || ce.Tables[0].Table != "assets" || ce.Tables[0].N == 0 {
		t.Fatalf("contradiction reported as %+v; the table and its count should be populated", ce.Tables)
	}
	// ⚠️ #1276: the abort must NAME the rows. "assets: N rows" is true and
	// leaves the operator writing SQL to find out which — which is why the
	// sweep sat unusable after every dogfood run rather than being fixed.
	if len(ce.Tables[0].Sample) == 0 {
		t.Error("the abort named no rows; an operator cannot act on a count alone")
	}
	found := false
	for _, r := range ce.Tables[0].Sample {
		if r.ID == canary {
			found = true
		}
		if r.ID == "" || r.Label == "" {
			t.Errorf("a sampled row carries no id or label: %+v", r)
		}
	}
	if !found && int64(len(ce.Tables[0].Sample)) == ce.Tables[0].N {
		t.Error("the sample claims to be complete but omits the canary")
	}
	if msg := err.Error(); !strings.Contains(msg, "assets") ||
		!strings.Contains(msg, ce.Tables[0].Sample[0].ID) {
		t.Errorf("the error text must carry the table and the row ids; got %q", msg)
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

// TWO tables contradicting at once. The abort used to return the FIRST
// one it found, so an operator fixed one rule, re-ran, and was told about
// the next — and on a persistent stack each round trip costs a dogfood
// run to reproduce the state.
func TestSweep_ReportsEveryContradictingTable(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t)
	at := assetType(t, ctx, pool)

	var canary string
	if err := pool.QueryRow(ctx, `
		INSERT INTO assets (title, asset_type, metadata) VALUES ('sweep multi canary', $1, '{}'::jsonb)
		RETURNING id::text`, at).Scan(&canary); err != nil {
		t.Fatalf("insert canary: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, canary, `DELETE FROM assets WHERE id::text = $1`)
	})

	original := fixturesweep.Rules
	t.Cleanup(func() { fixturesweep.Rules = original })
	// field_definition rather than posts: every migrated database carries
	// the bootstrap field defaults, whereas a package-test database has no
	// posts at all — and a table with no rows cannot contradict, so the
	// test would have asserted its way to a false negative.
	var fields int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM field_definition`).Scan(&fields); err != nil {
		t.Fatalf("counting field_definition: %v", err)
	}
	if fields == 0 {
		t.Skip("field_definition is empty; this database has not been migrated")
	}
	fixturesweep.Rules = []fixturesweep.Rule{
		{Table: "assets", IDColumn: "id", LabelColumn: "title",
			Fixture: `true`, Protected: `true`, Why: "self-contradictory, for this test"},
		{Table: "field_definition", IDColumn: "id", LabelColumn: "code",
			Fixture: `true`, Protected: `true`, Why: "self-contradictory, for this test"},
	}

	_, err := fixturesweep.Run(ctx, pool, seedCat, true)
	var ce *fixturesweep.ErrContradiction
	if !errors.As(err, &ce) {
		t.Fatalf("want ErrContradiction, got %T: %v", err, err)
	}
	if len(ce.Tables) != 2 {
		t.Fatalf("want both contradicting tables reported, got %d: %+v", len(ce.Tables), ce.Tables)
	}
	msg := err.Error()
	for _, want := range []string{"assets", "field_definition"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error text omits %q: %q", want, msg)
		}
	}

	var lives bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM assets WHERE id::text=$1)`, canary).Scan(&lives); err != nil {
		t.Fatalf("check canary: %v", err)
	}
	if !lives {
		t.Error("the canary was deleted despite two contradictions")
	}
}

// ZERO rows. A stack that has never had a dogfood run must sweep cleanly
// rather than treating "nothing matched" as a problem.
func TestSweep_CleanDatabaseSweepsWithoutError(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t)

	original := fixturesweep.Rules
	t.Cleanup(func() { fixturesweep.Rules = original })
	// Nothing is a fixture; everything real. The census must still run and
	// the sweep must still succeed with nothing to do.
	fixturesweep.Rules = []fixturesweep.Rule{{
		Table: "assets", IDColumn: "id", LabelColumn: "title",
		Fixture: `false`, Protected: `true`, Why: "nothing to sweep, for this test",
	}}

	rep, err := fixturesweep.Run(ctx, pool, seedCat, true)
	if err != nil {
		t.Fatalf("a sweep with nothing to do must succeed: %v", err)
	}
	if !rep.Applied {
		t.Error("Run reported no applied sweep")
	}
	for _, tr := range rep.Tables {
		if tr.Deleted != 0 || tr.Contradiction != 0 {
			t.Errorf("%s: deleted=%d contradiction=%d, want 0/0", tr.Table, tr.Deleted, tr.Contradiction)
		}
	}
}

// EVERY parameterised rule binds $1, and $1 means whatever that rule says
// it means. The tidier-looking alternative — collections at $1, posts at
// $2 — fails on the server: a predicate referencing only $2 makes
// Postgres expect two parameters and then refuse to infer a type for the
// $1 nothing mentions (42P18). Driven against a real server, because
// "the bind is accepted" is a claim only the server can settle.
func TestArgsFor_EveryRuleBindsOneSlot(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t)

	for _, tc := range []struct {
		name string
		rule fixturesweep.Rule
		sql  string
	}{
		{"no parameter",
			fixturesweep.Rule{Table: "assets", Param: fixturesweep.ParamNone},
			`SELECT count(*) FROM assets WHERE false`},
		{"collection names",
			fixturesweep.Rule{Table: "collections", Param: fixturesweep.ParamCollectionNames},
			`SELECT count(*) FROM collections WHERE name = ANY($1::text[])`},
		{"post ids",
			fixturesweep.Rule{Table: "posts", Param: fixturesweep.ParamPostIDs},
			`SELECT count(*) FROM posts WHERE id = ANY($1::uuid[])`},
		// A parameterised RULE whose composed STATEMENT has no
		// placeholder. This is the satellite delete's shape, and sending
		// it an argument is "mismatched param and argument count".
		{"parameterised rule, unparameterised statement",
			fixturesweep.Rule{Table: "posts", Param: fixturesweep.ParamPostIDs},
			`SELECT count(*) FROM posts WHERE title ~ '[0-9]{13}'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n int64
			if err := pool.QueryRow(ctx, tc.sql,
				fixturesweep.ArgsForTest(tc.rule, tc.sql, seedCat)...).Scan(&n); err != nil {
				t.Fatalf("bind rejected: %v", err)
			}
		})
	}

	// ⚠️ And EVERY statement shape the sweep composes, over the REAL
	// rules. The four do not agree about parameters — the census and the
	// parent DELETE use Fixture AND Protected, but the SATELLITE delete
	// composes ONLY Fixture, and the posts rule's parameter lives in
	// Protected. Deciding the argument list from the rule alone therefore
	// sent one argument to a statement with no placeholder and broke every
	// real -apply run, which only the census shape would have missed.
	for _, r := range fixturesweep.Rules {
		for _, shape := range []struct {
			name string
			sql  string
		}{
			{"census", `SELECT count(*) FILTER (WHERE ` + r.Fixture + `), ` +
				`count(*) FILTER (WHERE ` + r.Protected + `) FROM ` + r.Table},
			{"fixture-only", `SELECT count(*), 0::bigint FROM ` + r.Table +
				` WHERE ` + r.Fixture},
			{"delete-shape", `SELECT count(*), 0::bigint FROM ` + r.Table +
				` WHERE (` + r.Fixture + `) AND NOT (` + r.Protected + `)`},
		} {
			t.Run(shape.name+"/"+r.Table, func(t *testing.T) {
				var a, b int64
				if err := pool.QueryRow(ctx, shape.sql,
					fixturesweep.ArgsForTest(r, shape.sql, seedCat)...).Scan(&a, &b); err != nil {
					t.Fatalf("bind rejected for %s: %v", r.Table, err)
				}
			})
		}
	}
}
