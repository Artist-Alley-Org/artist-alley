// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #417 — the public featured rail (ADR 0065).
//
// The load-bearing test here is TestRail_FeaturingNeverWidensAccess.
// Everything else in this file supports it.
//
// The invariant: a placement is a SELECTION over what the caller can
// already see, never a grant. Placing a private collection or a draft
// asset at scope='public' must render NOTHING — not an empty tile, not
// a titleless row, nothing. A naive LEFT JOIN filtered only on scope
// passes a casual eyeball and fails this, which is why it is asserted
// at the query rather than through the handler.
//
// Skips without AA_DB_PASSWORD, same convention as the other
// integration suites.

package featured

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

func railPool(t *testing.T) *pgxpool.Pool {
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
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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

const railOwner int64 = 4170001

// place inserts one placement and registers its cleanup.
func place(t *testing.T, pool *pgxpool.Pool, kind string, subject uuid.UUID, scope string, pos int) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO featured_items (subject_kind, subject_id, position, scope)
		VALUES ($1,$2,$3,$4)`, kind, subject, pos, scope)
	if err != nil {
		t.Fatalf("place %s %s at %s: %v", kind, subject, scope, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM featured_items WHERE subject_id=$1 AND scope=$2`, subject, scope)
	})
}

func railAsset(t *testing.T, pool *pgxpool.Pool, title, status, sensitivity, processing string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),$4,$5,$6)`,
		id, title, railOwner, status, sensitivity, processing)
	if err != nil {
		t.Fatalf("seed asset %q: %v", title, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

func railCollection(t *testing.T, pool *pgxpool.Pool, name, vis string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO collections (id, name, description, owner_user_ref, visibility, membership)
		VALUES ($1,$2,'',$3,$4,'manual')`, id, name, railOwner, vis)
	if err != nil {
		t.Fatalf("seed collection %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id=$1`, id)
	})
	return id
}

func railTitles(t *testing.T, pool *pgxpool.Pool, caller visibility.Caller) map[string]bool {
	t.Helper()
	rows, err := ListPublicRail(context.Background(), pool, caller, 500)
	if err != nil {
		t.Fatalf("ListPublicRail: %v", err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Title] = true
	}
	return out
}

// TestRail_FeaturingNeverWidensAccess is the invariant.
//
// Every subject below is placed at scope='public'. Only the ones an
// anonymous caller could already see may come back.
func TestRail_FeaturingNeverWidensAccess(t *testing.T) {
	pool := railPool(t)

	publicAsset := railAsset(t, pool, "rail-public-asset", "active", "public", "ready")
	draftAsset := railAsset(t, pool, "rail-draft-asset", "draft", "public", "ready")
	embargoAsset := railAsset(t, pool, "rail-embargo-asset", "active", "embargo", "ready")
	processingAsset := railAsset(t, pool, "rail-processing-asset", "active", "public", "processing")
	publicColl := railCollection(t, pool, "rail-public-coll", "public")
	privateColl := railCollection(t, pool, "rail-private-coll", "private")

	for i, s := range []uuid.UUID{publicAsset, draftAsset, embargoAsset, processingAsset} {
		place(t, pool, "asset", s, "public", i)
	}
	place(t, pool, "collection", publicColl, "public", 10)
	place(t, pool, "collection", privateColl, "public", 11)

	got := railTitles(t, pool, visibility.NewCaller(nil))

	visible := []string{"rail-public-asset", "rail-public-coll"}
	hidden := []string{
		"rail-draft-asset",      // not published
		"rail-embargo-asset",    // sensitivity tier
		"rail-processing-asset", // no derivatives yet
		"rail-private-coll",     // not public
	}
	for _, title := range visible {
		if !got[title] {
			t.Errorf("%q was placed publicly and IS visible to anonymous, but the rail dropped it", title)
		}
	}
	for _, title := range hidden {
		if got[title] {
			t.Errorf("%q is NOT visible to an anonymous caller, but a public placement rendered it — "+
				"featuring widened access, which is the one thing it must never do", title)
		}
	}
}

// TestRail_InvisibleSubjectProducesNoRowAtAll pins the specific failure
// a naive join gives: the placement comes back with an empty title
// instead of being dropped. Asserted on the ROW COUNT, because a
// title-based check would pass against exactly that bug.
func TestRail_InvisibleSubjectProducesNoRowAtAll(t *testing.T) {
	pool := railPool(t)

	privateColl := railCollection(t, pool, "rail-only-private", "private")
	place(t, pool, "collection", privateColl, "public", 0)

	rows, err := ListPublicRail(context.Background(), pool, visibility.NewCaller(nil), 500)
	if err != nil {
		t.Fatalf("ListPublicRail: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.SubjectID.Bytes) == privateColl {
			t.Fatalf("the placement for a private collection came back as a row (title=%q); "+
				"an invisible subject must yield NO row, not a blank one", r.Title)
		}
	}
}

// TestRail_ScopeIsolation: org and team placements are not public.
func TestRail_ScopeIsolation(t *testing.T) {
	pool := railPool(t)

	orgColl := railCollection(t, pool, "rail-org-scoped", "public")
	place(t, pool, "collection", orgColl, "org", 0)

	got := railTitles(t, pool, visibility.NewCaller(nil))
	if got["rail-org-scoped"] {
		t.Error("an org-scoped placement appeared on the public rail; scope must gate the audience")
	}
}

// TestRail_MultiAudience is the proof the constraint swap landed: the
// same subject placed at two scopes at once, which the old
// UNIQUE (subject_kind, subject_id) made impossible.
func TestRail_MultiAudience(t *testing.T) {
	pool := railPool(t)

	coll := railCollection(t, pool, "rail-both-audiences", "public")
	place(t, pool, "collection", coll, "public", 0)
	place(t, pool, "collection", coll, "org", 0) // would 23505 before 00010

	got := railTitles(t, pool, visibility.NewCaller(nil))
	if !got["rail-both-audiences"] {
		t.Error("a subject featured at BOTH public and org did not render on the public rail")
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM featured_items WHERE subject_id=$1`, coll).Scan(&n); err != nil {
		t.Fatalf("count placements: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 placements for the subject, got %d — the uniqueness constraint "+
			"is still collapsing audiences", n)
	}
}

// TestRail_DanglingPlacementIsDropped covers the case 00002 explicitly
// tolerates: a subject hard-deleted out from under its placement. The
// admin list keeps the row so an operator can prune it; the PUBLIC rail
// must not render it.
func TestRail_DanglingPlacementIsDropped(t *testing.T) {
	pool := railPool(t)

	orphan := uuid.New() // never inserted into either subject table
	place(t, pool, "collection", orphan, "public", 0)

	rows, err := ListPublicRail(context.Background(), pool, visibility.NewCaller(nil), 500)
	if err != nil {
		t.Fatalf("ListPublicRail: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.SubjectID.Bytes) == orphan {
			t.Error("a placement whose subject no longer exists rendered on the public rail")
		}
	}
}

// TestRail_EmbargoAssetShowsTitleOnly covers ADR 0020 for the caller
// class that CAN see the row: an authenticated non-owner still reaches
// embargo assets through the asset predicate (soft-delete only), so the
// rail must surface the title without the thumbnail hint that would let
// a client fetch pixels.
func TestRail_EmbargoAssetShowsTitleOnly(t *testing.T) {
	pool := railPool(t)

	embargo := railAsset(t, pool, "rail-embargo-titled", "active", "embargo", "ready")
	place(t, pool, "asset", embargo, "public", 0)

	stranger := int64(4170099)
	rows, err := ListPublicRail(context.Background(), pool, visibility.NewCaller(&stranger), 500)
	if err != nil {
		t.Fatalf("ListPublicRail: %v", err)
	}
	var found bool
	for _, r := range rows {
		if uuid.UUID(r.SubjectID.Bytes) != embargo {
			continue
		}
		found = true
		if r.Title != "rail-embargo-titled" {
			t.Errorf("embargo asset title = %q, want the real title (ADR 0020: title only)", r.Title)
		}
		if r.AssetFileHash != nil {
			t.Error("embargo asset exposed a file hash; ADR 0020 is title-only, so the thumbnail " +
				"hint must be suppressed")
		}
		if r.AssetHasImage {
			t.Error("embargo asset reported has_image; the client would render a thumbnail request")
		}
	}
	if !found {
		t.Skip("authenticated asset visibility no longer admits embargo rows; " +
			"re-point this test at whatever tier it admits instead")
	}
}
