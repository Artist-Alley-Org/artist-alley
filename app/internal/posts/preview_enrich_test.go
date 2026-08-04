// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #471 — per-caller preview_available on post members, and the cache
// isolation that makes it safe.
//
// enrichPreview derives preview_available per REQUEST (readability is
// per-caller, ADR 0064) but the full Post is cached cross-caller by id
// (h.byID) with a Members slice that aliases the cached backing array.
// The load-bearing test here is the ISOLATION one: enriching for a
// caller who CAN read a restricted member must not leak that `true` into
// the cache or to the next caller. If enrichPreview ever mutates the
// cached array in place, that test fails.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	pePostOwner int64 = 4710001
	peStranger  int64 = 4710999
)

func previewPool(t *testing.T) *pgxpool.Pool {
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

func peHash(id uuid.UUID) string {
	sum := sha256.Sum256(id[:])
	return hex.EncodeToString(sum[:])
}

// seedPreviewAsset plants an asset at a sensitivity, owned by
// pePostOwner, with a storage object and optionally a `col` variant.
func seedPreviewAsset(t *testing.T, pool *pgxpool.Pool, sensitivity string, withCol bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	hash := peHash(id)
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1,1,'fs') ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	if withCol {
		if _, err := pool.Exec(ctx,
			`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1,'col',1)
			 ON CONFLICT (object_hash, variant_key) DO NOTHING`, hash); err != nil {
			t.Fatalf("seed col: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status, file_hash)
		 VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready',$5)`,
		id, "pe-"+sensitivity, pePostOwner, sensitivity, hash); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id) })
	return id
}

// seedPreviewPost creates a public post owned by pePostOwner with the
// given member assets, in order.
func seedPreviewPost(t *testing.T, pool *pgxpool.Pool, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1,$2,$3,'public')`,
		postID, pePostOwner, "pe post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	for i, m := range members {
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`, postID, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id=$1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, postID)
	})
	return postID
}

func peHandler(pool *pgxpool.Pool) *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(pool, logger, cache.NewRegistry(pool, logger))
}

func ctxAs(ref int64) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{UserRef: ref, AuthMethod: "session"})
}

// memberFlag reads preview_available for one member.
//
// Since #883 a member the caller may not see carries NO asset object at
// all — the flag is subsumed by the placeholder, which is a strictly
// stronger statement than preview_available=false. So the lookup keys on
// PostMember.AssetId (always present) rather than on Asset.Id, and a
// redacted member reports false.
func memberFlag(t *testing.T, p *openapi.Post, assetID uuid.UUID) bool {
	t.Helper()
	for _, m := range p.Members {
		if uuid.UUID(m.AssetId) != assetID {
			continue
		}
		if m.Restricted || m.Asset == nil {
			return false
		}
		return m.Asset.PreviewAvailable
	}
	t.Fatalf("asset %v not a member of the post", assetID)
	return false
}

// TestEnrichPreview_PerCaller pins the readability matrix on post members.
func TestEnrichPreview_PerCaller(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := context.Background()

	pubCol := seedPreviewAsset(t, pool, "public", true)            // readable + col
	restrictedCol := seedPreviewAsset(t, pool, "restricted", true) // owner-only + col
	pubNoCol := seedPreviewAsset(t, pool, "public", false)         // readable, no col
	postID := seedPreviewPost(t, pool, pubCol, restrictedCol, pubNoCol)
	pgID := pgtype.UUID{Bytes: postID, Valid: true}

	// Owner reads every tier.
	ownerPost, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := h.enrichPreview(ctxAs(pePostOwner), ownerPost); err != nil {
		t.Fatalf("enrich owner: %v", err)
	}
	if !memberFlag(t, ownerPost, pubCol) {
		t.Error("owner: public+col preview_available should be true")
	}
	if !memberFlag(t, ownerPost, restrictedCol) {
		t.Error("owner: restricted+col preview_available should be true for the owner")
	}
	if memberFlag(t, ownerPost, pubNoCol) {
		t.Error("owner: public-no-col preview_available should be false")
	}

	// A stranger reads public but not restricted.
	strangerPost, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch2: %v", err)
	}
	if err := h.enrichPreview(ctxAs(peStranger), strangerPost); err != nil {
		t.Fatalf("enrich stranger: %v", err)
	}
	if !memberFlag(t, strangerPost, pubCol) {
		t.Error("stranger: public+col should be true")
	}
	if memberFlag(t, strangerPost, restrictedCol) {
		t.Error("stranger: restricted+col MUST be false — the #471 gate")
	}
	// #883 subsumed the flag with a redaction: the stranger no longer
	// gets an asset object to carry a flag on at all. Asserted here as
	// well as in member_allowlist_test.go so this file's fixture cannot
	// drift into a state where memberFlag's nil branch is what makes the
	// assertion above pass.
	for _, m := range strangerPost.Members {
		if uuid.UUID(m.AssetId) == restrictedCol && (!m.Restricted || m.Asset != nil) {
			t.Error("stranger: the restricted member should be a placeholder with no asset")
		}
	}
}

// TestEnrichPreview_CacheIsolation is the leak test the posts path was
// deferred for: enriching for a caller who can read a restricted member
// must not write that `true` into the cross-caller cache.
func TestEnrichPreview_CacheIsolation(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := context.Background()

	restrictedCol := seedPreviewAsset(t, pool, "restricted", true)
	postID := seedPreviewPost(t, pool, restrictedCol)
	pgID := pgtype.UUID{Bytes: postID, Valid: true}
	key := postID.String()

	// Prime the cache (baked preview_available is false), then enrich for
	// the OWNER, who reads the restricted member → true.
	ownerView, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := h.enrichPreview(ctxAs(pePostOwner), ownerView); err != nil {
		t.Fatalf("enrich owner: %v", err)
	}
	if !memberFlag(t, ownerView, restrictedCol) {
		t.Fatal("precondition: owner should read the restricted member")
	}

	// The CACHED post must be untouched — still false. This is the leak.
	// Note the cached row is the UNREDACTED one (#883 redaction is
	// per-request, never baked); member_allowlist_test.go's cache test
	// asserts that direction.
	cached, ok := h.byID.Get(key)
	if !ok {
		t.Fatal("post was not cached; test cannot verify isolation")
	}
	if memberFlag(t, &cached, restrictedCol) {
		t.Fatal("LEAK: the owner's enrich mutated the cross-caller cache to true")
	}

	// And a stranger, served from that cache, must see false — not the
	// owner's true.
	strangerView, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch2: %v", err)
	}
	if err := h.enrichPreview(ctxAs(peStranger), strangerView); err != nil {
		t.Fatalf("enrich stranger: %v", err)
	}
	if memberFlag(t, strangerView, restrictedCol) {
		t.Fatal("LEAK: a stranger saw the owner's preview_available=true on a restricted member")
	}
}

// setThumbhash writes assets.thumbhash for an already-seeded asset —
// the shape of the raster worker's backfill, deliberately AFTER the post
// row exists.
func setThumbhash(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID, raw []byte) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET thumbhash = $2 WHERE id = $1`, assetID, raw); err != nil {
		t.Fatalf("set thumbhash: %v", err)
	}
}

func memberThumbhash(t *testing.T, p *openapi.Post, assetID uuid.UUID) *string {
	t.Helper()
	for _, m := range p.Members {
		if uuid.UUID(m.AssetId) != assetID {
			continue
		}
		if m.Restricted || m.Asset == nil {
			// A redacted member ships no thumbhash — the blur-up is
			// derived from the real pixels, so it is content (#883).
			return nil
		}
		return m.Asset.Thumbhash
	}
	t.Fatalf("asset %v not a member of the post", assetID)
	return nil
}

// TestEnrichPreview_Thumbhash is #648: every post member shipped
// thumbhash=null, so the blur-up placeholder was dead on the browse feed
// while the value sat in the database.
//
// The second half is why the field rides enrichPreview instead of the
// cached ListPostAssets row: thumbhash is backfilled ASYNCHRONOUSLY by
// the raster worker, which never invalidates a post cache. Baked into
// the cached row, a post read before its raster job finished would pin
// null indefinitely — the same class of bug, one step later.
func TestEnrichPreview_Thumbhash(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := context.Background()

	withHash := seedPreviewAsset(t, pool, "public", true)
	noHash := seedPreviewAsset(t, pool, "public", true) // e.g. a non-image
	raw := []byte{0x01, 0x02, 0x03, 0xfe, 0xff}
	wantB64 := base64.StdEncoding.EncodeToString(raw)
	setThumbhash(t, pool, withHash, raw)

	postID := seedPreviewPost(t, pool, withHash, noHash)
	pgID := pgtype.UUID{Bytes: postID, Valid: true}

	p, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := h.enrichPreview(ctxAs(pePostOwner), p); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	got := memberThumbhash(t, p, withHash)
	if got == nil {
		t.Fatal("member with a thumbhash shipped null — #648")
	}
	if *got != wantB64 {
		t.Errorf("thumbhash = %q, want base64 %q", *got, wantB64)
	}
	// An asset genuinely without one degrades to null, not "".
	if th := memberThumbhash(t, p, noHash); th != nil {
		t.Errorf("member without a thumbhash should be null, got %q", *th)
	}

	// Backfill AFTER the post is cached. The cached row is stale by
	// construction; the per-request enrich must still carry the value.
	raw2 := []byte{0xaa, 0xbb, 0xcc}
	setThumbhash(t, pool, noHash, raw2)
	p2, err := h.fetchFullPost(ctx, pgID) // served from h.byID
	if err != nil {
		t.Fatalf("fetch2: %v", err)
	}
	if err := h.enrichPreview(ctxAs(pePostOwner), p2); err != nil {
		t.Fatalf("enrich2: %v", err)
	}
	got2 := memberThumbhash(t, p2, noHash)
	if got2 == nil || *got2 != base64.StdEncoding.EncodeToString(raw2) {
		t.Fatal("a thumbhash backfilled after the post was cached never reached the caller")
	}
}
