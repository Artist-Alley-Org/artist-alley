// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #920 — soft-deleting an asset did not evict the posts holding it.
//
// ListPostAssets has always joined `assets` with `a.deleted_at IS NULL`,
// so the QUERY was never wrong. What was wrong is that soft-delete
// writes only the asset row while the post cache is keyed on the post,
// and nothing on the delete path told the post cache its answer had
// changed. `GET /posts/{id}` went on serving the deleted asset in full —
// title, description, file hash, byte size — until the process
// restarted. The restart being the only thing that fixed it is what
// identifies this as invalidation rather than a read-rule gap.
//
// The test that matters is the one that never restarts anything: it
// populates the cache, soft-deletes, and reads again through the same
// handler. It also asserts the stale read FIRST, so the fixture is
// proven to actually reach the cached path — a test that populated
// nothing would pass trivially.
//
// Scope: this is cache invalidation only. Whether ContentReadable
// should consult deleted_at is #898 and stays separate.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const aiOwner int64 = 9200001

// seedAsset inserts a real asset row so the ListPostAssets join has
// something to find. post_assets has no FK to assets in every
// deployment shape, but the join does — a synthetic uuid would make the
// member invisible from the start and the test would pass on a bug.
func seedAsset(t *testing.T, pool *pgxpool.Pool, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO assets (id, owner_user_ref, title, asset_type, status, processing_status)
		 VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), 'active', 'ready')`,
		id, aiOwner, title); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(t.Context(), `DELETE FROM assets WHERE id=$1`, id) })
	return id
}

// aiSeedPost creates a post with the given assets as members.
func aiSeedPost(t *testing.T, pool *pgxpool.Pool, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	postID := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1,$2,$3,'public')`,
		postID, aiOwner, "asset-invalidation post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	for i, m := range members {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`,
			postID, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `DELETE FROM post_assets WHERE post_id=$1`, postID)
		_, _ = pool.Exec(t.Context(), `DELETE FROM posts WHERE id=$1`, postID)
	})
	return postID
}

func softDelete(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, assetID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
}

func restore(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE assets SET deleted_at = NULL WHERE id = $1`, assetID); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func memberIDs(t *testing.T, h *Handler, postID uuid.UUID) []uuid.UUID {
	t.Helper()
	p, err := h.fetchFullPost(t.Context(), pgtype.UUID{Bytes: postID, Valid: true})
	if err != nil {
		t.Fatalf("fetchFullPost: %v", err)
	}
	out := make([]uuid.UUID, 0, len(p.Members))
	for _, m := range p.Members {
		out = append(out, uuid.UUID(m.AssetId))
	}
	return out
}

func contains(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestInvalidateForAsset_SoftDeleteEvictsHoldingPosts(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	keep := seedAsset(t, pool, "kept member")
	doomed := seedAsset(t, pool, "soft-deleted member")
	postID := aiSeedPost(t, pool, keep, doomed)

	// Populate the cache — this is the read the bug served stale.
	if got := memberIDs(t, h, postID); len(got) != 2 {
		t.Fatalf("post has %d members before the delete, want 2 — fixture is wrong", len(got))
	}

	softDelete(t, pool, doomed)

	// RED-FIRST, in-test: with the cache still holding the pre-delete
	// answer, the deleted asset is still served. This asserts the
	// fixture genuinely reaches the cached path; without it a passing
	// test below would prove nothing.
	if got := memberIDs(t, h, postID); !contains(got, doomed) {
		t.Fatal("the post did not serve the soft-deleted asset from cache before " +
			"invalidation — this fixture never populated the cache, so the " +
			"assertion below would pass on the unfixed code too")
	}

	if err := InvalidateForAsset(t.Context(), h.registry, pool, doomed); err != nil {
		t.Fatalf("InvalidateForAsset: %v", err)
	}

	got := memberIDs(t, h, postID)
	if contains(got, doomed) {
		t.Error("the post still lists the soft-deleted asset after invalidation")
	}
	if !contains(got, keep) {
		t.Error("invalidation dropped the surviving member too")
	}
	if len(got) != 1 {
		t.Errorf("post has %d members after the delete, want 1", len(got))
	}
}

// Restore is the same bug wearing the other hat. Delete evicts the
// cached copies; they are then repopulated WITHOUT the asset. Restoring
// it without a second eviction leaves it missing from its posts until a
// restart.
func TestInvalidateForAsset_RestoreEvictsHoldingPosts(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	asset := seedAsset(t, pool, "restored member")
	postID := aiSeedPost(t, pool, asset)

	softDelete(t, pool, asset)
	if err := InvalidateForAsset(t.Context(), h.registry, pool, asset); err != nil {
		t.Fatalf("InvalidateForAsset (delete): %v", err)
	}
	// Repopulate the cache in the deleted state — this is what makes
	// restore need its own eviction.
	if got := memberIDs(t, h, postID); len(got) != 0 {
		t.Fatalf("post has %d members while the asset is deleted, want 0", len(got))
	}

	restore(t, pool, asset)

	if got := memberIDs(t, h, postID); contains(got, asset) {
		t.Fatal("the post already shows the restored asset without invalidation — " +
			"the cache was not populated, so this test proves nothing")
	}

	if err := InvalidateForAsset(t.Context(), h.registry, pool, asset); err != nil {
		t.Fatalf("InvalidateForAsset (restore): %v", err)
	}
	if got := memberIDs(t, h, postID); !contains(got, asset) {
		t.Error("the restored asset is still missing from its post — restore needs " +
			"the same invalidation delete does")
	}
}

// An asset in several posts evicts all of them, not just the first.
func TestInvalidateForAsset_EvictsEveryHoldingPost(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	shared := seedAsset(t, pool, "member of two posts")
	postA := aiSeedPost(t, pool, shared)
	postB := aiSeedPost(t, pool, shared)

	if len(memberIDs(t, h, postA)) != 1 || len(memberIDs(t, h, postB)) != 1 {
		t.Fatal("fixture: both posts should start with the shared member")
	}

	softDelete(t, pool, shared)
	if err := InvalidateForAsset(t.Context(), h.registry, pool, shared); err != nil {
		t.Fatalf("InvalidateForAsset: %v", err)
	}

	if contains(memberIDs(t, h, postA), shared) {
		t.Error("post A still lists the deleted asset")
	}
	if contains(memberIDs(t, h, postB), shared) {
		t.Error("post B still lists the deleted asset — only the first holder was evicted")
	}
}

// Nil-safe: a handler built without a registry (test fixtures, and any
// build that does not wire caching) must not panic.
func TestInvalidateForAsset_NilRegistryIsNoOp(t *testing.T) {
	pool := previewPool(t)
	if err := InvalidateForAsset(t.Context(), nil, pool, uuid.New()); err != nil {
		t.Errorf("nil registry returned %v, want nil", err)
	}
}

// An asset in no posts is not an error.
func TestInvalidateForAsset_UnheldAsset(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	if err := InvalidateForAsset(t.Context(), h.registry, pool, uuid.New()); err != nil {
		t.Errorf("unheld asset returned %v, want nil", err)
	}
}
