// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/seed"
)

// TestMain initialises atrest with a throwaway master key so the
// SeedCreateUser → EnsureCurrentForUser → Generate path (1.22.I-b)
// can mint a keypair. Production wires AA_MASTER_KEY from the env;
// tests don't depend on the operator's real key.
func TestMain(m *testing.M) {
	key := make([]byte, atrest.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		panic("seed_test: seed master key: " + err.Error())
	}
	if err := atrest.InitWithKey(key); err != nil {
		panic("seed_test: atrest init: " + err.Error())
	}
	os.Exit(m.Run())
}

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
	ctx := t.Context()

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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)

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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)
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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)

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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)
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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)
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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)
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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)
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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)
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
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)

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

// --- user forge ------------------------------------------------------

// fakeHasher exposes a known-input/known-output hash so the
// test can verify the password column landed via the hash
// path (not as plaintext).
func fakeHasher(plaintext string) (string, error) {
	return "hashed:" + plaintext, nil
}

func TestCreateUser_HappyPath_PasswordHashedAndPersisted(t *testing.T) {
	pool := openTestPool(t)
	h := seed.NewAdminHandler(pool, nil, nil, nil, fakeHasher, nil)
	username := "seed-user-" + randHex(4)
	pw := "Sup3rs3cret"
	fullname := "Sofia Hernandez"

	res, err := h.CreateUser(context.Background(), nil, 0, seed.UserInput{
		Username: username,
		Fullname: &fullname,
		Password: &pw,
		Approved: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, res.Ref)
	})
	if res.AlreadyExisted {
		t.Error("fresh insert; AlreadyExisted should be false")
	}
	if res.Username != username {
		t.Errorf("username: got %q want %q", res.Username, username)
	}
	if res.Ref == 0 {
		t.Error("ref: should be assigned")
	}

	// Verify the password column carries the hash, NOT the
	// plaintext. The seed pattern is "operator-supplied plaintext
	// → hash via the configured hasher → persist hash."
	var got *string
	_ = pool.QueryRow(context.Background(),
		`SELECT password FROM "user" WHERE ref = $1`, res.Ref,
	).Scan(&got)
	if got == nil || *got != "hashed:"+pw {
		t.Errorf("password column: got %v want %q", got, "hashed:"+pw)
	}

	// Verify the federation keypair landed in the same tx as
	// the user — 1.22.I-b precondition for the encrypted
	// federation paths I-e/I-f. Exactly one current key,
	// algorithm pinned.
	var keyCount int
	var algorithm string
	var isCurrent bool
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*), MAX(algorithm), bool_or(is_current)
		  FROM federation_user_keys
		 WHERE user_ref = $1
	`, res.Ref).Scan(&keyCount, &algorithm, &isCurrent); err != nil {
		t.Fatalf("federation_user_keys query: %v", err)
	}
	if keyCount != 1 || !isCurrent || algorithm != "naclbox-x25519-v1" {
		t.Errorf("federation key shape: count=%d current=%v algorithm=%q",
			keyCount, isCurrent, algorithm)
	}
}

func TestCreateUser_NoPassword_PersistsNULL(t *testing.T) {
	// Fictional seed users that exist only as actors (never log
	// in) should have password=NULL — the seed loader's common
	// case.
	pool := openTestPool(t)
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil) // hasher unused → no password
	username := "seed-actor-" + randHex(4)

	res, err := h.CreateUser(context.Background(), nil, 0, seed.UserInput{
		Username: username,
		Approved: true,
		// No Password
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, res.Ref)
	})

	var got *string
	_ = pool.QueryRow(context.Background(),
		`SELECT password FROM "user" WHERE ref = $1`, res.Ref,
	).Scan(&got)
	if got != nil {
		t.Errorf("password column: got %q want NULL", *got)
	}
}

func TestCreateUser_PasswordWithoutHasher_ReturnsErr(t *testing.T) {
	pool := openTestPool(t)
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil) // hasher nil
	pw := "Sup3rs3cret"

	_, err := h.CreateUser(context.Background(), nil, 0, seed.UserInput{
		Username: "seed-user-" + randHex(4),
		Password: &pw,
		Approved: true,
	})
	if err != seed.ErrPasswordHasherNotWired {
		t.Errorf("err: got %v want ErrPasswordHasherNotWired", err)
	}
}

func TestCreateUser_IdempotentOnUsername(t *testing.T) {
	pool := openTestPool(t)
	h := seed.NewAdminHandler(pool, nil, nil, nil, fakeHasher, nil)
	username := "seed-user-idem-" + randHex(4)

	first, err := h.CreateUser(context.Background(), nil, 0, seed.UserInput{
		Username: username,
		Approved: true,
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, first.Ref)
	})
	if first.AlreadyExisted {
		t.Error("first insert; AlreadyExisted should be false")
	}

	second, err := h.CreateUser(context.Background(), nil, 0, seed.UserInput{
		Username: username,
		Approved: true,
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.AlreadyExisted {
		t.Error("re-run; AlreadyExisted should be true")
	}
	if second.Ref != first.Ref {
		t.Errorf("idempotent ref: got %d want %d", second.Ref, first.Ref)
	}

	// Re-run must not duplicate the federation keypair —
	// EnsureCurrentForUser's idempotency contract (1.22.I-b).
	var keyCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM federation_user_keys WHERE user_ref = $1`,
		first.Ref).Scan(&keyCount); err != nil {
		t.Fatalf("federation_user_keys count: %v", err)
	}
	if keyCount != 1 {
		t.Errorf("idempotent re-run created duplicate keypair: count = %d, want 1", keyCount)
	}
}

// TestCreateUser_BackfillsKeyForPreExistingKeylessUser models the
// upgrade scenario: a user already exists in the database (created
// before 1.22.I-b shipped) with no federation keypair. A re-run of
// the seed loader on the same username must backfill the missing
// key — gold-standard idempotency means "ensure the state is
// correct," not "skip if the user exists."
func TestCreateUser_BackfillsKeyForPreExistingKeylessUser(t *testing.T) {
	pool := openTestPool(t)
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)
	username := "seed-prekey-" + randHex(4)

	// Insert user directly via SQL, bypassing the seed handler so
	// no key gets generated. Models the pre-upgrade state.
	var ref int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		username).Scan(&ref); err != nil {
		t.Fatalf("pre-insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	// Confirm no key exists.
	var preCount int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM federation_user_keys WHERE user_ref = $1`, ref,
	).Scan(&preCount)
	if preCount != 0 {
		t.Fatalf("test setup: pre-existing user already has key (count=%d)", preCount)
	}

	// Call CreateUser with the same username — should hit the
	// ON CONFLICT DO NOTHING + already-existed path AND backfill
	// the federation key.
	res, err := h.CreateUser(context.Background(), nil, 0, seed.UserInput{
		Username: username,
		Approved: true,
	})
	if err != nil {
		t.Fatalf("seed re-run: %v", err)
	}
	if !res.AlreadyExisted {
		t.Error("user existed pre-call; AlreadyExisted should be true")
	}
	if res.Ref != ref {
		t.Errorf("ref: got %d want %d", res.Ref, ref)
	}

	// Federation key should now exist.
	var postCount int
	var isCurrent bool
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), bool_or(is_current) FROM federation_user_keys WHERE user_ref = $1`,
		ref).Scan(&postCount, &isCurrent); err != nil {
		t.Fatalf("post-count: %v", err)
	}
	if postCount != 1 || !isCurrent {
		t.Errorf("backfill failed: count=%d current=%v want 1/true", postCount, isCurrent)
	}
}

func TestCreateUser_RespectsCreatedAt(t *testing.T) {
	pool := openTestPool(t)
	h := seed.NewAdminHandler(pool, nil, nil, nil, nil, nil)
	pastT := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)

	res, err := h.CreateUser(context.Background(), nil, 0, seed.UserInput{
		Username:  "seed-user-pastt-" + randHex(4),
		Approved:  true,
		CreatedAt: &pastT,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, res.Ref)
	})
	if !res.CreatedAt.Equal(pastT) {
		t.Errorf("created_at: got %v want %v", res.CreatedAt, pastT)
	}
}
