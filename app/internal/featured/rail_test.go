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
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// defaultLadder is the stock preview ladder, passed explicitly by these
// tests because ListPublicRail takes the CONFIGURED rungs as a
// parameter rather than assuming them (#591).
var defaultLadder = []string{"col", "preview", "screen", "hires"}

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
	rows, err := ListPublicRail(context.Background(), pool, caller, 500, defaultLadder)
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

	rows, err := ListPublicRail(context.Background(), pool, visibility.NewCaller(nil), 500, defaultLadder)
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

	rows, err := ListPublicRail(context.Background(), pool, visibility.NewCaller(nil), 500, defaultLadder)
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
	rows, err := ListPublicRail(context.Background(), pool, visibility.NewCaller(&stranger), 500, defaultLadder)
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

// --- #559: collection covers (ADR 0027 hero-card fallback) -----------
//
// A featured COLLECTION used to contribute only its name, because every
// image hint came from the asset join — which cannot match a collection
// subject. The landing page rendered a row of blank white tiles.
//
// The cover is now derived from the collection's most-recent post, which
// means an ASSET is being surfaced through a COLLECTION placement. That
// is a new path into asset bytes, so these tests carry the same burden
// as TestRail_FeaturingNeverWidensAccess: the cover must clear the
// caller's asset predicate AND the public tier, or the tile stays
// title-only.

// railStoredAsset creates an asset with real bytes behind it: a
// storage_objects row, a servable `col` variant, and the asset itself.
// The col variant is what preview_available keys on.
func railStoredAsset(t *testing.T, pool *pgxpool.Pool, title, sensitivity string, withCol bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	// storage_objects.hash is CHECK-constrained to ^[0-9a-f]{64}$ — a
	// sha256 in hex. Derive one from a fresh UUID so parallel runs never
	// collide on the primary key.
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString())))

	if _, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1, 'image/png', 'fs')`, hash); err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM storage_objects WHERE hash=$1`, hash)
	})

	if withCol {
		if _, err := pool.Exec(ctx, `
			INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type)
			VALUES ($1, 'col', 1, 'image/png')`, hash); err != nil {
			t.Fatalf("seed col variant: %v", err)
		}
	}

	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, file_hash)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready',$5)`,
		id, title, railOwner, sensitivity, hash); err != nil {
		t.Fatalf("seed asset %q: %v", title, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

// railPostInCollection creates a post covered by `cover` and links it to
// `coll`. `age` shifts created_at back, so tests can control which post
// the "most-recent" rule picks.
func railPostInCollection(t *testing.T, pool *pgxpool.Pool, coll, cover uuid.UUID, age time.Duration) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, cover_asset_id, created_at, updated_at)
		VALUES ($1,$2,'rail cover post',$3, now() - $4::interval, now() - $4::interval)`,
		id, railOwner, cover, age.String()); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id=$1`, id)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO collection_posts (collection_id, post_id) VALUES ($1,$2)`, coll, id); err != nil {
		t.Fatalf("link post to collection: %v", err)
	}
	return id
}

// railRowFor finds the placement row for one subject.
func railRowFor(t *testing.T, pool *pgxpool.Pool, caller visibility.Caller, subject uuid.UUID) (RailRow, bool) {
	t.Helper()
	rows, err := ListPublicRail(context.Background(), pool, caller, 500, defaultLadder)
	if err != nil {
		t.Fatalf("ListPublicRail: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.SubjectID.Bytes) == subject {
			return r, true
		}
	}
	return RailRow{}, false
}

func TestRail_CollectionGetsCoverFromMostRecentPost(t *testing.T) {
	pool := railPool(t)
	anon := visibility.NewCaller(nil)

	coll := railCollection(t, pool, "rail-cover-public", "public")
	place(t, pool, "collection", coll, "public", 0)

	older := railStoredAsset(t, pool, "rail-cover-older", "public", true)
	newer := railStoredAsset(t, pool, "rail-cover-newer", "public", true)
	railPostInCollection(t, pool, coll, older, 48*time.Hour)
	railPostInCollection(t, pool, coll, newer, 1*time.Hour)

	row, ok := railRowFor(t, pool, anon, coll)
	if !ok {
		t.Fatal("public collection missing from the rail")
	}
	if row.AssetFileHash == nil {
		t.Fatal("collection has an eligible public cover but no file hash — the tile would be blank")
	}
	if !row.CoverAssetID.Valid {
		t.Fatal("no cover_asset_id; the client cannot build a variant URL from a collection id")
	}
	if got := uuid.UUID(row.CoverAssetID.Bytes); got != newer {
		t.Errorf("cover = %v, want the MOST RECENT post's asset %v (ADR 0027)", got, newer)
	}
	if !row.AssetPreviewAvailable {
		t.Error("preview_available false despite a servable col variant; the tile renders title-only")
	}
}

// THE SECURITY CASE. A collection may be public while its contents are
// not. Deriving a cover must not become a side channel into assets the
// caller cannot see — the cover goes through the caller's asset
// predicate, so an invisible member yields title-only.
func TestRail_CollectionCoverNeverLeaksInvisibleMembers(t *testing.T) {
	pool := railPool(t)
	anon := visibility.NewCaller(nil)

	for _, tc := range []struct {
		name        string
		sensitivity string
		status      string
	}{
		{"team-tier member", "team", "active"},
		{"embargo member", "embargo", "active"},
		{"restricted member", "restricted", "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coll := railCollection(t, pool, "rail-cover-gated-"+tc.sensitivity, "public")
			place(t, pool, "collection", coll, "public", 0)
			gated := railStoredAsset(t, pool, "rail-gated-"+tc.sensitivity, tc.sensitivity, true)
			railPostInCollection(t, pool, coll, gated, time.Hour)

			row, ok := railRowFor(t, pool, anon, coll)
			if !ok {
				t.Fatal("public collection vanished from the rail; it should still show title-only")
			}
			if row.AssetFileHash != nil {
				t.Errorf("leaked a file hash for a %s member: %q", tc.sensitivity, *row.AssetFileHash)
			}
			if row.CoverAssetID.Valid {
				t.Errorf("leaked cover_asset_id %v for a %s member — featuring must never widen access",
					uuid.UUID(row.CoverAssetID.Bytes), tc.sensitivity)
			}
			if row.AssetPreviewAvailable {
				t.Errorf("preview_available true for a %s member; the client would request bytes it "+
					"may not have", tc.sensitivity)
			}
			if row.Title == "" {
				t.Error("title empty; the tile should still identify the collection")
			}
		})
	}
}

// Zero-console-404 property (#471): a collection whose cover has no
// servable col must not advertise one, or the tile fires a request that
// 404s on the front page.
func TestRail_CollectionCoverRequiresServableVariant(t *testing.T) {
	pool := railPool(t)
	anon := visibility.NewCaller(nil)

	coll := railCollection(t, pool, "rail-cover-novariant", "public")
	place(t, pool, "collection", coll, "public", 0)
	// Public and visible, but no `col` variant was ever produced.
	noCol := railStoredAsset(t, pool, "rail-cover-nocol", "public", false)
	railPostInCollection(t, pool, coll, noCol, time.Hour)

	row, ok := railRowFor(t, pool, anon, coll)
	if !ok {
		t.Fatal("collection missing from the rail")
	}
	if row.AssetPreviewAvailable {
		t.Error("preview_available true with no col variant; the tile would fire a 404")
	}
	if row.CoverAssetID.Valid {
		t.Error("cover_asset_id set with no servable variant; hash and id must stay in lockstep")
	}
	if row.AssetFileHash != nil {
		t.Error("file hash advertised with no servable variant")
	}
}

// An empty collection is the ordinary case on a fresh install: it must
// render as a title-only tile, not disappear and not break.
func TestRail_EmptyCollectionIsTitleOnly(t *testing.T) {
	pool := railPool(t)
	anon := visibility.NewCaller(nil)

	coll := railCollection(t, pool, "rail-cover-empty", "public")
	place(t, pool, "collection", coll, "public", 0)

	row, ok := railRowFor(t, pool, anon, coll)
	if !ok {
		t.Fatal("empty public collection should still appear, title-only")
	}
	if row.Title != "rail-cover-empty" {
		t.Errorf("title = %q, want the collection name", row.Title)
	}
	if row.AssetFileHash != nil || row.CoverAssetID.Valid || row.AssetPreviewAvailable {
		t.Error("empty collection advertised a cover")
	}
}
