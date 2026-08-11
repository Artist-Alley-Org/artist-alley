// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #557 — a like must be VISIBLE on the next read of the post.
//
// posts.like_count and posts.comment_count live on the posts row and are
// maintained by database triggers in the `social` package's mutations.
// The posts row is served from a by-id LRU (h.byID). Nothing connected
// the two: a like incremented the database and left every reader on the
// stale number until an unrelated write to that post evicted the entry.
//
// Nobody noticed while the counter was decoration — a small "♥ 3" on a
// hover overlay. #557 put a like BUTTON next to it, at which point the
// defect reads as "the heart fills but the number never moves", and a
// reload does not fix it because the reload hits the same stale entry.
//
// This test asserts through the cache the way a reader does: warm the
// entry, move the counter the way a trigger does, invalidate the way
// social.invalidatePostCache does, and require the next read to be
// current. It deliberately does NOT import `social` — `posts` is what
// `social` imports, so the dependency only runs one way; what is shared
// is the domain NAME, which is the thing that can drift and therefore
// the thing worth pinning.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

func TestPostCounterCache_InvalidationMakesTheNewCountVisible(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := context.Background()

	ref, _ := seedAuthor(t, pool, "", "Counter Subject", "", false)
	postID := seedAuthoredPost(t, pool, ref)
	pgID := pgtype.UUID{Bytes: postID, Valid: true}

	// Warm the by-id cache, the way any read of the feed does.
	warm, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch (warm): %v", err)
	}
	before := warm.LikeCount

	// Move the counter the way the like trigger does.
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET like_count = like_count + 1 WHERE id = $1`, postID); err != nil {
		t.Fatalf("bump like_count: %v", err)
	}

	// PRECONDITION — this is the defect, and it must still be present,
	// or the test below proves nothing. A cache that happened to miss
	// here would make the assertion pass without the invalidation.
	stale, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch (stale): %v", err)
	}
	if stale.LikeCount != before {
		t.Skipf("post cache did not retain the entry (got %d, warmed %d); "+
			"nothing to invalidate, so this test cannot measure anything",
			stale.LikeCount, before)
	}

	// The invalidation `social` performs, addressed by the SHARED domain
	// constant. Spelling the string literally here instead would let the
	// two sides drift silently — a publish to an unregistered domain is
	// dropped without error.
	if err := h.registry.InvalidateNow(ctx, cache.DomainPostByID, uuid.UUID(pgID.Bytes).String()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	fresh, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch (fresh): %v", err)
	}
	if fresh.LikeCount != before+1 {
		t.Errorf("like_count after invalidation = %d, want %d — the post cache is still serving the pre-like number",
			fresh.LikeCount, before+1)
	}
}

// TestPostCacheDomain_MatchesTheSharedConstant pins the one thing that
// can silently break the fix above: `posts` registering its cache under
// one name while `social` publishes invalidations to another.
//
// cache.Registry drops a payload for an unregistered domain without an
// error — correctly, since peers register different domains — so a
// mismatch here is not a crash, a log line, or a failing request. It is
// a counter that quietly stops updating.
func TestPostCacheDomain_MatchesTheSharedConstant(t *testing.T) {
	if cacheDomainPostByID != cache.DomainPostByID {
		t.Errorf("posts registers %q but the shared constant is %q; "+
			"social's invalidations would be published to a domain nobody listens on",
			cacheDomainPostByID, cache.DomainPostByID)
	}
}
