// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// GitHub #341 — featured_items curation integration tests.
//
// Real Postgres (skipped without AA_DB_PASSWORD). Covers:
//
//   * Add appends with monotonically increasing positions
//   * Add of an already-featured subject → ErrAlreadyFeatured
//   * List resolves the subject title (asset title + collection name)
//     in position order
//   * Remove deletes the row; Remove of a missing id → ErrNotFound
//   * Reorder rewrites positions to the supplied id order

package featured_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/featured"
)

func TestAdd_AppendsWithIncreasingPositions(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)
	h := featured.NewHandler(pool, discardLoggerF())

	a := seedAssetF(t, pool, "first")
	b := seedAssetF(t, pool, "second")

	r1, err := h.Add(context.Background(), featured.AddInput{SubjectKind: "asset", SubjectID: a})
	if err != nil {
		t.Fatalf("Add a: %v", err)
	}
	r2, err := h.Add(context.Background(), featured.AddInput{SubjectKind: "asset", SubjectID: b})
	if err != nil {
		t.Fatalf("Add b: %v", err)
	}
	// Asserted as a DELTA, not as absolute 0,1. Add() appends at
	// (global MAX(position) + 1), so any row left in featured_items by
	// another test — or by the ADR 0065 migration backfilling existing
	// curation — makes an absolute assertion fail for reasons that have
	// nothing to do with appending. This failed exactly that way once.
	if r2.Position != r1.Position+1 {
		t.Errorf("positions = %d,%d; second must be first+1", r1.Position, r2.Position)
	}
}

func TestAdd_Duplicate_ReturnsErrAlreadyFeatured(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)
	h := featured.NewHandler(pool, discardLoggerF())

	a := seedAssetF(t, pool, "dupe")
	if _, err := h.Add(context.Background(), featured.AddInput{SubjectKind: "asset", SubjectID: a}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	_, err := h.Add(context.Background(), featured.AddInput{SubjectKind: "asset", SubjectID: a})
	if !errors.Is(err, featured.ErrAlreadyFeatured) {
		t.Errorf("second Add err = %v, want ErrAlreadyFeatured", err)
	}
}

func TestList_ResolvesTitles_InOrder(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)
	h := featured.NewHandler(pool, discardLoggerF())

	assetID := seedAssetF(t, pool, "my asset")
	collID := seedCollectionF(t, pool, "my collection")

	if _, err := h.Add(context.Background(), featured.AddInput{SubjectKind: "collection", SubjectID: collID}); err != nil {
		t.Fatalf("Add collection: %v", err)
	}
	if _, err := h.Add(context.Background(), featured.AddInput{SubjectKind: "asset", SubjectID: assetID}); err != nil {
		t.Fatalf("Add asset: %v", err)
	}

	rows, err := h.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	if rows[0].SubjectKind != "collection" || rows[0].Title != "my collection" {
		t.Errorf("row0 = %q/%q, want collection/my collection", rows[0].SubjectKind, rows[0].Title)
	}
	if rows[1].SubjectKind != "asset" || rows[1].Title != "my asset" {
		t.Errorf("row1 = %q/%q, want asset/my asset", rows[1].SubjectKind, rows[1].Title)
	}
}

func TestRemove_DeletesRow_MissingIsNotFound(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)
	h := featured.NewHandler(pool, discardLoggerF())

	a := seedAssetF(t, pool, "removable")
	row, err := h.Add(context.Background(), featured.AddInput{SubjectKind: "asset", SubjectID: a})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := h.Remove(context.Background(), uuid.UUID(row.ID.Bytes)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := h.Remove(context.Background(), uuid.New()); !errors.Is(err, featured.ErrNotFound) {
		t.Errorf("Remove(missing) = %v, want ErrNotFound", err)
	}
}

func TestReorder_RewritesPositions(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)
	h := featured.NewHandler(pool, discardLoggerF())

	a := seedAssetF(t, pool, "a")
	b := seedAssetF(t, pool, "b")
	r1, _ := h.Add(context.Background(), featured.AddInput{SubjectKind: "asset", SubjectID: a})
	r2, _ := h.Add(context.Background(), featured.AddInput{SubjectKind: "asset", SubjectID: b})

	// Reverse the order.
	if err := h.Reorder(context.Background(), []uuid.UUID{
		uuid.UUID(r2.ID.Bytes), uuid.UUID(r1.ID.Bytes),
	}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	rows, err := h.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 || uuid.UUID(rows[0].ID.Bytes) != uuid.UUID(r2.ID.Bytes) {
		t.Errorf("after reorder, first row = %v, want %v", rows[0].ID, r2.ID)
	}
}

// ---------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------

func openPoolF(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOrF("AA_DB_HOST", "postgres")
	port := envOrF("AA_DB_PORT", "5432")
	user := envOrF("AA_DB_USER", "artist_alley")
	name := envOrF("AA_DB_NAME", "artist_alley")
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
	t.Cleanup(pool.Close)
	return pool
}

func envOrF(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func discardLoggerF() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedUserF(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, password) VALUES ($1, '') RETURNING ref`,
		"ft-"+uuid.New().String()[:8],
	).Scan(&ref); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref) })
	return ref
}

func seedAssetF(t *testing.T, pool *pgxpool.Pool, title string) uuid.UUID {
	t.Helper()
	owner := seedUserF(t, pool)
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO assets (title, asset_type, owner_user_ref) VALUES ($1, 1, $2) RETURNING id`,
		title, owner,
	).Scan(&id); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	return id
}

func seedCollectionF(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	owner := seedUserF(t, pool)
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO collections (owner_user_ref, name) VALUES ($1, $2) RETURNING id`,
		owner, name,
	).Scan(&id); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id) })
	return id
}

func cleanupFeatured(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, _ = pool.Exec(context.Background(), `DELETE FROM featured_items`)
}

// ---------------------------------------------------------------------------
// #1104 / #1088 — the write path can name an audience
// ---------------------------------------------------------------------------

// scopeOf reads the PERSISTED audience of one placement.
//
// Deliberately not the scope on the row Add returns. That row is the
// INSERT's RETURNING, which is close enough to a handler echo to be
// worth avoiding on principle: this asserts what is in the table.
func scopeOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT scope FROM featured_items WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("read persisted scope: %v", err)
	}
	return got
}

// TestAdd_ScopeDefaultsToOrgAndAcceptsPublic is #1088's answer: the
// audience is now expressible, `org` is still what an omitted scope
// writes, and `public` — previously reachable only by a direct database
// write — goes through the API.
func TestAdd_ScopeDefaultsToOrgAndAcceptsPublic(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)
	h := featured.NewHandler(pool, discardLoggerF())
	ctx := context.Background()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"omitted stays org", "", featured.ScopeOrg},
		{"explicit org", featured.ScopeOrg, featured.ScopeOrg},
		{"explicit public", featured.ScopePublic, featured.ScopePublic},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := seedAssetF(t, pool, "scope-"+c.name)
			row, err := h.Add(ctx, featured.AddInput{
				SubjectKind: "asset", SubjectID: a, Scope: c.in,
			})
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if got := scopeOf(t, pool, uuid.UUID(row.ID.Bytes)); got != c.want {
				t.Errorf("persisted scope = %q, want %q", got, c.want)
			}
		})
	}
}

// TestAdd_RefusesUnwritableScopes: `team` is a real value of the column
// and is not offered on the write path (it would need a team_id this
// payload cannot name), and an unknown string must not reach Postgres
// as a 23514 the caller sees as a 500.
func TestAdd_RefusesUnwritableScopes(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)
	h := featured.NewHandler(pool, discardLoggerF())

	for _, scope := range []string{featured.ScopeTeam, "everyone", "ORG"} {
		a := seedAssetF(t, pool, "bad-scope-"+scope)
		_, err := h.Add(context.Background(), featured.AddInput{
			SubjectKind: "asset", SubjectID: a, Scope: scope,
		})
		if !errors.Is(err, featured.ErrScopeNotWritable) {
			t.Errorf("Add(scope=%q) err = %v, want ErrScopeNotWritable", scope, err)
		}
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM featured_items WHERE subject_id = $1`, a).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("Add(scope=%q) was refused but wrote %d row(s)", scope, n)
		}
	}
}

// TestFeaturedItemsTeamScopeCheck exercises featured_items_team_scope_
// check, which #1088 called "currently decorative" because nothing in
// the tree could produce a row that tests it. It is not decorative — it
// is the reason `team` stays off the write path — so it gets an
// assertion rather than a comment.
//
// The constraint is an equivalence, not an implication, so both
// directions are asserted: scope='team' REQUIRES a team_id, and every
// other scope FORBIDS one. The second half is the one that matters for
// the audience predicate: without it a `public` row could carry a
// team_id and read as scoped to a team it is not scoped to.
func TestFeaturedItemsTeamScopeCheck(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)
	ctx := context.Background()

	a := seedAssetF(t, pool, "team-scope-check")

	var team uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO teams (slug, name) VALUES ($1, 'team scope check') RETURNING id`,
		"tsc-"+uuid.New().String()[:8],
	).Scan(&team); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, team) })

	insert := func(scope string, teamID *uuid.UUID) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO featured_items (subject_kind, subject_id, position, scope, team_id)
			VALUES ('asset', $1, 900, $2, $3)`, a, scope, teamID)
		return err
	}
	violates := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) &&
			pgErr.Code == "23514" &&
			pgErr.ConstraintName == "featured_items_team_scope_check"
	}

	if err := insert(featured.ScopeTeam, nil); !violates(err) {
		t.Errorf("scope='team' with a NULL team_id was accepted (err=%v); that placement is an "+
			"audience of nobody that still occupies a uniqueness slot", err)
	}
	if err := insert(featured.ScopePublic, &team); !violates(err) {
		t.Errorf("scope='public' with a team_id was accepted (err=%v); a non-team audience must "+
			"not carry a team", err)
	}
	if err := insert(featured.ScopeTeam, &team); err != nil {
		t.Errorf("scope='team' with a team_id was REJECTED (%v); the constraint is meant to permit "+
			"exactly this pairing", err)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM featured_items WHERE subject_id = $1`, a)
}
