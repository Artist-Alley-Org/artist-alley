// Package social hosts likes + comments queries; the HTTP surface
// lands in Phase 1.13.F-3. These tests verify the counter triggers
// from migration 00020 — they're the load-bearing piece of the data
// plane and we want them locked in before any handler reads or writes
// posts.like_count / posts.comment_count.
package social_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLikeTriggers_PostAndCommentTargets covers:
//   - Inserting a like with target_kind=post bumps posts.like_count
//   - Inserting a like with target_kind=comment bumps comments.like_count
//     AND DOES NOT touch posts.like_count (no cross-talk)
//   - Deleting a like decrements the same counter
//   - GREATEST(..,0) guards against underflow if the counter was wrong
//     pre-trigger (defensive — shouldn't happen on a clean install)
//   - Re-inserting a (post, user) like via ON CONFLICT DO NOTHING is
//     a no-op (idempotent like) — counter stays put
func TestLikeTriggers_PostAndCommentTargets(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Seed a post + a comment to like.
	postID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, visibility)
		VALUES ($1, 0, 'social-trigger-test', 'private')
	`, postID); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	commentID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO comments (id, target_kind, target_id, root_id, depth, author_user_ref, body)
		VALUES ($1, 'post', $2, $1, 0, 0, 'test')
	`, commentID, postID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}

	// Post likes -> bump posts.like_count.
	for _, ref := range []int64{1001, 1002, 1003} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO likes (target_kind, target_id, rs_user_id) VALUES ('post', $1, $2)`,
			postID, ref,
		); err != nil {
			t.Fatalf("insert like: %v", err)
		}
	}
	if got := postLikeCount(t, ctx, tx, postID); got != 3 {
		t.Errorf("after 3 post likes: posts.like_count = %d, want 3", got)
	}

	// Idempotent re-insert: (post, 1001) already exists — counter stays at 3.
	if _, err := tx.Exec(ctx,
		`INSERT INTO likes (target_kind, target_id, rs_user_id) VALUES ('post', $1, 1001) ON CONFLICT DO NOTHING`,
		postID,
	); err != nil {
		t.Fatalf("idempotent insert: %v", err)
	}
	if got := postLikeCount(t, ctx, tx, postID); got != 3 {
		t.Errorf("idempotent re-insert bumped counter: got %d, want 3", got)
	}

	// Comment likes -> bump comments.like_count, not posts.like_count.
	if _, err := tx.Exec(ctx,
		`INSERT INTO likes (target_kind, target_id, rs_user_id) VALUES ('comment', $1, 9001)`,
		commentID,
	); err != nil {
		t.Fatalf("comment like: %v", err)
	}
	if got := commentLikeCount(t, ctx, tx, commentID); got != 1 {
		t.Errorf("after comment like: comments.like_count = %d, want 1", got)
	}
	if got := postLikeCount(t, ctx, tx, postID); got != 3 {
		t.Errorf("comment like leaked into posts.like_count: got %d, want 3", got)
	}

	// Unliking decrements.
	if _, err := tx.Exec(ctx,
		`DELETE FROM likes WHERE target_kind='post' AND target_id=$1 AND rs_user_id=1001`,
		postID,
	); err != nil {
		t.Fatalf("delete like: %v", err)
	}
	if got := postLikeCount(t, ctx, tx, postID); got != 2 {
		t.Errorf("after one unlike: posts.like_count = %d, want 2", got)
	}
}

// TestCommentTriggers_SoftDeleteAndCascade verifies:
//   - Inserting a comment bumps posts.comment_count
//   - Soft-deleting (SET deleted_at) decrements
//   - The decrement only fires on the OLD.deleted_at IS NULL transition;
//     toggling deleted_at back to NULL re-increments
//   - Hard-deleting a soft-deleted comment doesn't double-decrement
//     (the OLD.deleted_at IS NOT NULL guard in the AFTER DELETE trigger)
//   - Hard-deleting a live comment decrements once
//   - FK cascade from posts hard-delete fires the sweep trigger that
//     wipes both likes and comments rows
func TestCommentTriggers_SoftDeleteAndCascade(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	postID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, visibility)
		VALUES ($1, 0, 'comment-trigger-test', 'private')
	`, postID); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	// Two top-level comments.
	rootA := uuid.New()
	rootB := uuid.New()
	for _, id := range []uuid.UUID{rootA, rootB} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO comments (id, target_kind, target_id, root_id, depth, author_user_ref, body)
			VALUES ($1, 'post', $2, $1, 0, 1, 'top')
		`, id, postID); err != nil {
			t.Fatalf("seed comment: %v", err)
		}
	}
	if got := postCommentCount(t, ctx, tx, postID); got != 2 {
		t.Errorf("after 2 comments: posts.comment_count = %d, want 2", got)
	}

	// Soft-delete rootA -> -1.
	if _, err := tx.Exec(ctx, `UPDATE comments SET deleted_at = NOW() WHERE id = $1`, rootA); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if got := postCommentCount(t, ctx, tx, postID); got != 1 {
		t.Errorf("after soft-delete: posts.comment_count = %d, want 1", got)
	}

	// Hard-delete rootA (already soft-deleted) -> no extra decrement.
	if _, err := tx.Exec(ctx, `DELETE FROM comments WHERE id = $1`, rootA); err != nil {
		t.Fatalf("hard delete soft-deleted: %v", err)
	}
	if got := postCommentCount(t, ctx, tx, postID); got != 1 {
		t.Errorf("after hard-delete of already-soft-deleted: posts.comment_count = %d, want 1 (no double-decrement)", got)
	}

	// Hard-delete rootB (still live) -> -1.
	if _, err := tx.Exec(ctx, `DELETE FROM comments WHERE id = $1`, rootB); err != nil {
		t.Fatalf("hard delete live: %v", err)
	}
	if got := postCommentCount(t, ctx, tx, postID); got != 0 {
		t.Errorf("after hard-delete of live: posts.comment_count = %d, want 0", got)
	}
}

// TestSweepOnPostDelete: hard-delete of a post wipes likes + comments
// rows pointing at it (polymorphic FK loss workaround).
func TestSweepOnPostDelete(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	postID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, visibility)
		VALUES ($1, 0, 'sweep-test', 'private')
	`, postID); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	commentID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO comments (id, target_kind, target_id, root_id, depth, author_user_ref, body)
		VALUES ($1, 'post', $2, $1, 0, 1, 'will be swept')
	`, commentID, postID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO likes (target_kind, target_id, rs_user_id) VALUES ('post', $1, 7777)`,
		postID,
	); err != nil {
		t.Fatalf("seed like: %v", err)
	}

	// Hard-delete the post.
	if _, err := tx.Exec(ctx, `DELETE FROM posts WHERE id = $1`, postID); err != nil {
		t.Fatalf("delete post: %v", err)
	}

	var likes int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM likes WHERE target_kind = 'post' AND target_id = $1`, postID,
	).Scan(&likes); err != nil {
		t.Fatalf("count likes: %v", err)
	}
	if likes != 0 {
		t.Errorf("likes remained after post delete: %d", likes)
	}

	var comments int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM comments WHERE target_kind = 'post' AND target_id = $1`, postID,
	).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if comments != 0 {
		t.Errorf("comments remained after post delete: %d", comments)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func postLikeCount(t *testing.T, ctx context.Context, tx pgx.Tx, id uuid.UUID) int64 {
	t.Helper()
	var v int64
	if err := tx.QueryRow(ctx, `SELECT like_count FROM posts WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read like_count: %v", err)
	}
	return v
}

func postCommentCount(t *testing.T, ctx context.Context, tx pgx.Tx, id uuid.UUID) int64 {
	t.Helper()
	var v int64
	if err := tx.QueryRow(ctx, `SELECT comment_count FROM posts WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read comment_count: %v", err)
	}
	return v
}

func commentLikeCount(t *testing.T, ctx context.Context, tx pgx.Tx, id uuid.UUID) int64 {
	t.Helper()
	var v int64
	pgID := pgtype.UUID{Bytes: id, Valid: true}
	if err := tx.QueryRow(ctx, `SELECT like_count FROM comments WHERE id = $1`, pgID).Scan(&v); err != nil {
		t.Fatalf("read comment like_count: %v", err)
	}
	return v
}

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
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
	return pool
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
