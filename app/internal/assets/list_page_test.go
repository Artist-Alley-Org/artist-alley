// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #429 P0b — the asset browse query, converted from sqlc to hand-built
// SQL so the visibility predicate can reach it.
//
// The load-bearing test here is the PARITY test: for authenticated
// callers this rewrite must return byte-identical rows, in identical
// order, across every filter combination. The retained sqlc
// ListAssetsPage is used as the oracle — the old implementation and the
// new one run against the same data and their results are compared
// directly, rather than the new one being checked against hand-written
// expectations that could encode the same mistake twice.
//
// Skips without AA_DB_PASSWORD, same convention as the other
// integration suites.

package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

func listPagePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
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

const listPageOwner int64 = 4290001

// seedBrowseAssets plants a spread that exercises every dimension the
// browse query and the predicate care about.
func seedBrowseAssets(t *testing.T, pool *pgxpool.Pool) []uuid.UUID {
	t.Helper()
	ctx := context.Background()
	type seed struct {
		title       string
		status      string
		sensitivity string
		processing  string
		deleted     bool
	}
	seeds := []seed{
		{"p0b alpha public", "active", "public", "ready", false},
		{"p0b bravo draft", "draft", "public", "ready", false},
		{"p0b charlie archived", "archived", "public", "ready", false},
		{"p0b delta team", "active", "team", "ready", false},
		{"p0b echo restricted", "active", "restricted", "ready", false},
		{"p0b foxtrot processing", "active", "public", "processing", false},
		{"p0b golf deleted", "active", "public", "ready", true},
		{"p0b hotel public two", "active", "public", "ready", false},
	}
	ids := make([]uuid.UUID, 0, len(seeds))
	for i, s := range seeds {
		id := uuid.New()
		ids = append(ids, id)
		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		// Distinct created_at so ordering + cursor paging are
		// deterministic rather than tie-broken by chance.
		_, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
			                    processing_status, created_at, deleted_at)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),$4,$5,$6,
			        NOW() - ($7::int * INTERVAL '1 minute'), `+del+`)`,
			id, s.title, listPageOwner, s.status, s.sensitivity, s.processing, i)
		if err != nil {
			t.Fatalf("seed %q: %v", s.title, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
	})
	return ids
}

func ownerRefPtr() *int64 { r := listPageOwner; return &r }

// TestListAssetsPage_AuthenticatedParity is the reason this PR is safe:
// across every filter combination, the hand-built query must return
// exactly what the sqlc query returned for an authenticated caller.
func TestListAssetsPage_AuthenticatedParity(t *testing.T) {
	pool := listPagePool(t)
	seedBrowseAssets(t, pool)
	ctx := context.Background()
	q := New(pool)
	caller := visibility.NewCaller(ownerRefPtr())

	statusActive := "active"
	qText := "p0b"
	cases := []struct {
		name string
		p    ListAssetsPageGatedParams
	}{
		{"owner only", ListAssetsPageGatedParams{OwnerUserRef: ownerRefPtr(), RowLimit: 50}},
		{"owner + status", ListAssetsPageGatedParams{OwnerUserRef: ownerRefPtr(), Status: &statusActive, RowLimit: 50}},
		{"owner + text query", ListAssetsPageGatedParams{OwnerUserRef: ownerRefPtr(), Q: &qText, RowLimit: 50}},
		{"small limit (paging boundary)", ListAssetsPageGatedParams{OwnerUserRef: ownerRefPtr(), RowLimit: 3}},
		{"no filters at all", ListAssetsPageGatedParams{RowLimit: 25}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ListAssetsPageGated(ctx, pool, caller, nil, c.p)
			if err != nil {
				t.Fatalf("gated: %v", err)
			}
			want, err := q.ListAssetsPage(ctx, ListAssetsPageParams{
				IncludeDeleted:  c.p.IncludeDeleted,
				OwnerUserRef:    c.p.OwnerUserRef,
				AssetType:       c.p.AssetType,
				Status:          c.p.Status,
				Q:               c.p.Q,
				CursorCreatedAt: c.p.CursorCreatedAt,
				CursorID:        c.p.CursorID,
				RowLimit:        c.p.RowLimit,
			})
			if err != nil {
				t.Fatalf("sqlc oracle: %v", err)
			}
			assertSameRows(t, want, got)
		})
	}

	// Cursor paging: walk the whole set two rows at a time through both
	// implementations and require the same sequence. Paging is where an
	// ordering or tie-break mistake would surface.
	t.Run("cursor paging walks identically", func(t *testing.T) {
		var gotCursor, wantCursor struct {
			ts pgtype.Timestamptz
			id pgtype.UUID
		}
		for page := 0; page < 5; page++ {
			got, err := ListAssetsPageGated(ctx, pool, caller, nil, ListAssetsPageGatedParams{
				OwnerUserRef: ownerRefPtr(), RowLimit: 2,
				CursorCreatedAt: gotCursor.ts, CursorID: gotCursor.id,
			})
			if err != nil {
				t.Fatalf("gated page %d: %v", page, err)
			}
			want, err := q.ListAssetsPage(ctx, ListAssetsPageParams{
				OwnerUserRef: ownerRefPtr(), RowLimit: 2,
				CursorCreatedAt: wantCursor.ts, CursorID: wantCursor.id,
			})
			if err != nil {
				t.Fatalf("oracle page %d: %v", page, err)
			}
			assertSameRows(t, want, got)
			if len(got) == 0 {
				break
			}
			last := got[len(got)-1]
			gotCursor.ts, gotCursor.id = last.CreatedAt, last.ID
			wantCursor.ts, wantCursor.id = last.CreatedAt, last.ID
		}
	})
}

func assertSameRows(t *testing.T, want []ListAssetsPageRow, got []ListAssetsPageGatedRow) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("row count: sqlc oracle returned %d, gated returned %d", len(want), len(got))
	}
	for i := range want {
		if want[i].ID != got[i].ID {
			t.Fatalf("row %d: id %v != %v (order or membership diverged)", i, want[i].ID, got[i].ID)
		}
		if want[i].Title != got[i].Title || want[i].Status != got[i].Status ||
			want[i].CreatedAt != got[i].CreatedAt || want[i].DeletedAt != got[i].DeletedAt {
			t.Errorf("row %d (%v): field mismatch between oracle and gated query", i, want[i].ID)
		}
	}
}

// TestListAssetsPage_AnonymousGated exercises the anonymous branch even
// though no route reaches it yet (#415 opens that). If this ever
// regresses, the leak lands the moment anonymous routing ships.
func TestListAssetsPage_AnonymousGated(t *testing.T) {
	pool := listPagePool(t)
	seedBrowseAssets(t, pool)
	ctx := context.Background()

	rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(nil), nil, ListAssetsPageGatedParams{
		OwnerUserRef: ownerRefPtr(), RowLimit: 50,
	})
	if err != nil {
		t.Fatalf("anonymous list: %v", err)
	}
	// Only the two active+public+ready rows may appear.
	if len(rows) != 2 {
		t.Fatalf("anonymous saw %d rows, want exactly the 2 published-public-ready ones", len(rows))
	}
	for _, r := range rows {
		if r.Status != "active" {
			t.Errorf("anonymous saw non-active asset %q", r.Title)
		}
		if r.DeletedAt.Valid {
			t.Errorf("anonymous saw soft-deleted asset %q", r.Title)
		}
	}
}

// TestListAssetsPage_IncludeDeleted covers the superadmin escape hatch
// and, more importantly, its NARROWNESS: the flag waives soft-delete
// and nothing else.
func TestListAssetsPage_IncludeDeleted(t *testing.T) {
	pool := listPagePool(t)
	seedBrowseAssets(t, pool)
	ctx := context.Background()
	yes := true

	t.Run("authenticated caller sees soft-deleted rows", func(t *testing.T) {
		rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(ownerRefPtr()), nil,
			ListAssetsPageGatedParams{OwnerUserRef: ownerRefPtr(), IncludeDeleted: &yes, RowLimit: 50})
		if err != nil {
			t.Fatalf("include_deleted: %v", err)
		}
		var deleted int
		for _, r := range rows {
			if r.DeletedAt.Valid {
				deleted++
			}
		}
		if deleted == 0 {
			t.Error("include_deleted returned no soft-deleted rows; the admin escape hatch is broken")
		}
	})

	t.Run("without the flag, soft-deleted rows stay hidden", func(t *testing.T) {
		rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(ownerRefPtr()), nil,
			ListAssetsPageGatedParams{OwnerUserRef: ownerRefPtr(), RowLimit: 50})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, r := range rows {
			if r.DeletedAt.Valid {
				t.Errorf("soft-deleted asset %q visible without include_deleted", r.Title)
			}
		}
	})

	// The security property of choosing a narrow option over "skip the
	// predicate": if the caller's superadmin gate ever regresses, an
	// anonymous caller reaching this flag still cannot see unpublished
	// or non-public content — only soft-deleted PUBLIC content.
	t.Run("the flag never waives publication or sensitivity", func(t *testing.T) {
		rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(nil), nil,
			ListAssetsPageGatedParams{OwnerUserRef: ownerRefPtr(), IncludeDeleted: &yes, RowLimit: 50})
		if err != nil {
			t.Fatalf("anonymous + include_deleted: %v", err)
		}
		for _, r := range rows {
			if r.Status != "active" {
				t.Errorf("include_deleted leaked non-active asset %q to an anonymous caller", r.Title)
			}
			if r.ProcessingStatus != "ready" {
				t.Errorf("include_deleted leaked still-processing asset %q to an anonymous caller", r.Title)
			}
		}
		if len(rows) == 0 {
			t.Log("no rows: acceptable, but the assertions above are the point")
		}
	})
}

// TestGetAsset_AnonymousDenied covers the CanSee call #439 added to
// GetAsset, which shipped without a test.
//
// The assertion is ANONYMOUS denied, not non-owner denied. Per ADR
// 0064's corollary the authenticated EntityAsset predicate is
// `deleted_at IS NULL` and nothing more, so an authenticated non-owner
// legitimately SEES a draft asset's row — a "non-owner denied" test
// would assert the opposite of the decided design and fail. Writing
// that is the reflexive mistake here.
func TestGetAsset_AnonymousDenied(t *testing.T) {
	pool := listPagePool(t)
	seedBrowseAssets(t, pool)
	ctx := context.Background()

	var pub, draft string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM assets WHERE owner_user_ref=$1 AND status='active'
		   AND sensitivity='public' AND processing_status='ready' AND deleted_at IS NULL LIMIT 1`,
		listPageOwner).Scan(&pub); err != nil {
		t.Fatalf("find public asset: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM assets WHERE owner_user_ref=$1 AND status='draft' AND deleted_at IS NULL LIMIT 1`,
		listPageOwner).Scan(&draft); err != nil {
		t.Fatalf("find draft asset: %v", err)
	}

	anon := visibility.NewCaller(nil)
	pubID, draftID := uuid.MustParse(pub), uuid.MustParse(draft)

	okPub, err := visibility.CanSee(ctx, pool, visibility.EntityAsset, anon, pubID)
	if err != nil || !okPub {
		t.Errorf("anonymous denied a public+active+ready asset (ok=%v err=%v)", okPub, err)
	}
	okDraft, err := visibility.CanSee(ctx, pool, visibility.EntityAsset, anon, draftID)
	if err != nil || okDraft {
		t.Errorf("anonymous admitted a DRAFT asset (ok=%v err=%v)", okDraft, err)
	}
}

// hashForAsset derives a unique 64-hex storage hash from an asset id.
func hashForAsset(id uuid.UUID) string {
	sum := sha256.Sum256(id[:])
	return hex.EncodeToString(sum[:])
}

// seedAssetWithCol plants one asset at a sensitivity/owner, with a
// storage object and (optionally) a `col` variant, so
// preview_available's two inputs — variant existence and content
// readability — can be exercised independently (#471).
func seedAssetWithCol(t *testing.T, pool *pgxpool.Pool, sensitivity string, owner int64, withCol bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	hash := hashForAsset(id)
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 1, 'fs')
		 ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	if withCol {
		if _, err := pool.Exec(ctx,
			`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 1)
			 ON CONFLICT (object_hash, variant_key) DO NOTHING`, hash); err != nil {
			t.Fatalf("seed col variant: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status, file_hash)
		 VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), 'active', $4, 'ready', $5)`,
		id, "pa-"+sensitivity, owner, sensitivity, hash); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id) })
	return id
}

// TestListAssetsPage_PreviewAvailable pins the #471 flag: preview_available
// is true iff a servable `col` variant exists AND the caller passes the
// content plane (ADR 0064). A restricted asset the caller cannot read is
// false — identical to "no preview", so the flag never confirms
// 'restricted'.
func TestListAssetsPage_PreviewAvailable(t *testing.T) {
	pool := listPagePool(t)
	ctx := context.Background()

	pub := seedAssetWithCol(t, pool, "public", listPageOwner, true)            // readable + col
	restricted := seedAssetWithCol(t, pool, "restricted", listPageOwner, true) // owner-only + col
	pubNoCol := seedAssetWithCol(t, pool, "public", listPageOwner, false)      // readable, NO col

	find := func(rows []ListAssetsPageGatedRow, id uuid.UUID) (ListAssetsPageGatedRow, bool) {
		for _, r := range rows {
			if uuid.UUID(r.ID.Bytes) == id {
				return r, true
			}
		}
		return ListAssetsPageGatedRow{}, false
	}
	mustFlag := func(t *testing.T, rows []ListAssetsPageGatedRow, id uuid.UUID, want bool) {
		t.Helper()
		r, ok := find(rows, id)
		if !ok {
			t.Fatalf("asset %v not listed (row plane hid it — different contract)", id)
		}
		if r.PreviewAvailable != want {
			t.Errorf("asset %v: preview_available=%v, want %v", id, r.PreviewAvailable, want)
		}
	}

	stranger := int64(4290999)
	readAll := func(code string) bool { return code == visibility.ContentReadAll }
	params := ListAssetsPageGatedParams{RowLimit: 500}

	// Authenticated non-owner: restricted is LISTED (deferred sensitivity
	// rule) but its bytes are gated → preview_available false. This is the
	// core #471 assertion.
	t.Run("authenticated non-owner", func(t *testing.T) {
		rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(&stranger), nil, params)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		mustFlag(t, rows, pub, true)
		mustFlag(t, rows, restricted, false)
		mustFlag(t, rows, pubNoCol, false) // readable but no col
	})

	// Owner reads their own bytes at any tier → restricted true.
	t.Run("owner", func(t *testing.T) {
		rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(ownerRefPtr()), nil, params)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		mustFlag(t, rows, pub, true)
		mustFlag(t, rows, restricted, true)
		mustFlag(t, rows, pubNoCol, false)
	})

	// content.read.all reads every tier's bytes → restricted true, still
	// gated on col existence.
	t.Run("content.read.all", func(t *testing.T) {
		rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(&stranger), readAll, params)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		mustFlag(t, rows, restricted, true)
		mustFlag(t, rows, pubNoCol, false)
	})

	// Anonymous: public with col is true; the restricted row is not even
	// listed to anonymous, so only assert the public ones.
	t.Run("anonymous", func(t *testing.T) {
		rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(nil), nil, params)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		mustFlag(t, rows, pub, true)
		mustFlag(t, rows, pubNoCol, false)
		if _, ok := find(rows, restricted); ok {
			t.Error("anonymous listed a restricted asset (row-plane leak)")
		}
	})
}
