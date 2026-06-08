// Integration tests for the demo-seed loader endpoints. Real
// Postgres (skips without AA_DB_PASSWORD); validates the
// load-bearing properties the apply-side script needs:
//
//   1. Timestamps backfill is bulk + idempotent + reports
//      skipped-unknown count without erroring.
//   2. Comment forge respects supplied stable id (re-run
//      returns existing row with AlreadyExisted=true) +
//      forged author + reply-parent thread placement.

package seed_test

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/seed"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0xf]
	}
	return string(out)
}

// seedFixture inserts a user + post + asset for the test to
// backfill against; t.Cleanup handles teardown including
// any comments authored by the test user.
func seedFixture(t *testing.T, pool *pgxpool.Pool) (userRef int64, postID, assetID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	username := "seed-test-" + randHex(4)
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Seed Test', 1) RETURNING ref`,
		username,
	).Scan(&userRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	postID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Seed test post', 'org-only')`,
		postID, userRef,
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	assetID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, owner_user_ref, title, asset_type) VALUES ($1, $2, 'Seed test asset', 1)`,
		assetID, userRef,
	); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM comments WHERE author_user_ref = $1`, userRef)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, assetID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, userRef)
	})
	return userRef, postID, assetID
}

// --- timestamps backfill ---------------------------------------------

func TestBackfillTimestamps_HappyPath_PerKindCountsCorrect(t *testing.T) {
	pool := openTestPool(t)
	userRef, postID, assetID := seedFixture(t, pool)
	h := seed.NewAdminHandler(pool, nil, nil)

	pastT := time.Date(2025, 4, 15, 10, 30, 0, 0, time.UTC)
	res, err := h.BackfillTimestamps(context.Background(), nil, userRef, []seed.TimestampItem{
		{Kind: seed.TimestampKindAsset, ID: assetID, CreatedAt: pastT},
		{Kind: seed.TimestampKindPost, ID: postID, CreatedAt: pastT.Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.AssetUpdated != 1 || res.PostUpdated != 1 || res.SkippedUnknownID != 0 {
		t.Errorf("counts: %+v want asset=1 post=1 skipped=0", res)
	}

	// Verify the timestamps actually landed (asset.created_at +
	// posts.posted_at — the latter drives feed ordering).
	var assetCreated time.Time
	_ = pool.QueryRow(context.Background(),
		`SELECT created_at FROM assets WHERE id = $1`, assetID,
	).Scan(&assetCreated)
	if !assetCreated.Equal(pastT) {
		t.Errorf("asset.created_at: got %v want %v", assetCreated, pastT)
	}
	var postPostedAt time.Time
	_ = pool.QueryRow(context.Background(),
		`SELECT posted_at FROM posts WHERE id = $1`, postID,
	).Scan(&postPostedAt)
	if !postPostedAt.Equal(pastT.Add(time.Hour)) {
		t.Errorf("post.posted_at: got %v want %v", postPostedAt, pastT.Add(time.Hour))
	}
}

func TestBackfillTimestamps_Idempotent_SameEndStateAfterRerun(t *testing.T) {
	pool := openTestPool(t)
	userRef, postID, _ := seedFixture(t, pool)
	h := seed.NewAdminHandler(pool, nil, nil)
	pastT := time.Date(2025, 4, 15, 10, 30, 0, 0, time.UTC)

	first, _ := h.BackfillTimestamps(context.Background(), nil, userRef, []seed.TimestampItem{
		{Kind: seed.TimestampKindPost, ID: postID, CreatedAt: pastT},
	})
	second, _ := h.BackfillTimestamps(context.Background(), nil, userRef, []seed.TimestampItem{
		{Kind: seed.TimestampKindPost, ID: postID, CreatedAt: pastT},
	})
	if first.PostUpdated != 1 || second.PostUpdated != 1 {
		t.Errorf("both runs should update 1 row: first=%+v second=%+v", first, second)
	}
}

func TestBackfillTimestamps_UnknownID_CountedAsSkipped_NoError(t *testing.T) {
	pool := openTestPool(t)
	userRef, _, _ := seedFixture(t, pool)
	h := seed.NewAdminHandler(pool, nil, nil)

	bogus := uuid.New()
	res, err := h.BackfillTimestamps(context.Background(), nil, userRef, []seed.TimestampItem{
		{Kind: seed.TimestampKindPost, ID: bogus, CreatedAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.PostUpdated != 0 || res.SkippedUnknownID != 1 {
		t.Errorf("unknown id: %+v want post_updated=0 skipped=1", res)
	}
}

func TestBackfillTimestamps_BatchOverCap_ReturnsErr(t *testing.T) {
	pool := openTestPool(t)
	h := seed.NewAdminHandler(pool, nil, nil)
	items := make([]seed.TimestampItem, 1001)
	for i := range items {
		items[i] = seed.TimestampItem{Kind: seed.TimestampKindPost, ID: uuid.New(), CreatedAt: time.Now()}
	}
	_, err := h.BackfillTimestamps(context.Background(), nil, 0, items)
	if err != seed.ErrTimestampsBatchTooLarge {
		t.Errorf("over-cap: got %v want ErrTimestampsBatchTooLarge", err)
	}
}

// --- comment forge ---------------------------------------------------

func TestCreateComment_HappyPath_RespectsForcedAuthorAndCreatedAt(t *testing.T) {
	pool := openTestPool(t)
	userRef, postID, _ := seedFixture(t, pool)
	h := seed.NewAdminHandler(pool, nil, nil)
	pastT := time.Date(2025, 4, 15, 10, 30, 0, 0, time.UTC)

	res, err := h.CreateComment(context.Background(), nil, userRef, seed.CommentInput{
		TargetKind:    seed.CommentTargetPost,
		TargetID:      postID,
		AuthorUserRef: userRef,
		Body:          "nice work alice",
		CreatedAt:     &pastT,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.AlreadyExisted {
		t.Error("fresh insert; AlreadyExisted should be false")
	}
	if res.AuthorUserRef == nil || *res.AuthorUserRef != userRef {
		t.Errorf("forged author: got %v want %d", res.AuthorUserRef, userRef)
	}
	if !res.CreatedAt.Equal(pastT) {
		t.Errorf("created_at: got %v want %v", res.CreatedAt, pastT)
	}
}

func TestCreateComment_IdempotentOnSuppliedID(t *testing.T) {
	pool := openTestPool(t)
	userRef, postID, _ := seedFixture(t, pool)
	h := seed.NewAdminHandler(pool, nil, nil)
	commentID := uuid.New()

	first, err := h.CreateComment(context.Background(), nil, userRef, seed.CommentInput{
		ID:            &commentID,
		TargetKind:    seed.CommentTargetPost,
		TargetID:      postID,
		AuthorUserRef: userRef,
		Body:          "v1 body",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.AlreadyExisted {
		t.Error("first insert; AlreadyExisted should be false")
	}
	if first.ID != commentID {
		t.Errorf("id: got %v want %v", first.ID, commentID)
	}

	second, err := h.CreateComment(context.Background(), nil, userRef, seed.CommentInput{
		ID:            &commentID,
		TargetKind:    seed.CommentTargetPost,
		TargetID:      postID,
		AuthorUserRef: userRef,
		Body:          "v2 body — should be IGNORED on idempotent re-run",
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.AlreadyExisted {
		t.Error("re-run; AlreadyExisted should be true")
	}
	if second.Body != "v1 body" {
		t.Errorf("idempotent body: got %q want %q", second.Body, "v1 body")
	}
}

func TestCreateComment_UnknownAuthor_ReturnsErrAuthorNotFound(t *testing.T) {
	pool := openTestPool(t)
	_, postID, _ := seedFixture(t, pool)
	h := seed.NewAdminHandler(pool, nil, nil)
	_, err := h.CreateComment(context.Background(), nil, 0, seed.CommentInput{
		TargetKind:    seed.CommentTargetPost,
		TargetID:      postID,
		AuthorUserRef: 999_999_999, // never seeded
		Body:          "won't land",
	})
	if err != seed.ErrAuthorNotFound {
		t.Errorf("got %v want ErrAuthorNotFound", err)
	}
}

func TestCreateComment_UnknownTarget_ReturnsErrTargetNotFound(t *testing.T) {
	pool := openTestPool(t)
	userRef, _, _ := seedFixture(t, pool)
	h := seed.NewAdminHandler(pool, nil, nil)
	_, err := h.CreateComment(context.Background(), nil, userRef, seed.CommentInput{
		TargetKind:    seed.CommentTargetPost,
		TargetID:      uuid.New(), // never seeded
		AuthorUserRef: userRef,
		Body:          "won't land",
	})
	if err != seed.ErrTargetNotFound {
		t.Errorf("got %v want ErrTargetNotFound", err)
	}
}

func TestCreateComment_ReplyToParent_RootIDAndDepthDerived(t *testing.T) {
	pool := openTestPool(t)
	userRef, postID, _ := seedFixture(t, pool)
	h := seed.NewAdminHandler(pool, nil, nil)

	root, err := h.CreateComment(context.Background(), nil, userRef, seed.CommentInput{
		TargetKind:    seed.CommentTargetPost,
		TargetID:      postID,
		AuthorUserRef: userRef,
		Body:          "root",
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	if root.Depth != 0 || root.RootID != root.ID {
		t.Errorf("root: depth=%d root_id=%v id=%v want depth=0 root_id=id", root.Depth, root.RootID, root.ID)
	}

	reply, err := h.CreateComment(context.Background(), nil, userRef, seed.CommentInput{
		TargetKind:    seed.CommentTargetPost,
		TargetID:      postID,
		AuthorUserRef: userRef,
		ParentID:      &root.ID,
		Body:          "reply",
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if reply.Depth != 1 || reply.RootID != root.ID {
		t.Errorf("reply: depth=%d root_id=%v want depth=1 root_id=%v", reply.Depth, reply.RootID, root.ID)
	}
}
