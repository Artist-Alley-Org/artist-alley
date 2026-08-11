// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #557 — the author identity that rides on every post payload, and the
// two properties that make it safe to put there.
//
// 1. IT RESPECTS THE ANONYMOUS OPT-OUT. `user_profiles.hide_from_anonymous`
//    (ADR 0024's opt-out, ADR 0070 §3) makes a profile 404 to anonymous
//    callers. Denormalising the author onto the post would be a side
//    channel around that — the same name and face, arriving on someone
//    else's card. TestEnrichAuthors_AnonymousWithholding is the
//    assertion that would otherwise ship broken for the minority of
//    users who set the flag, and stay broken indefinitely because the
//    default is false and nothing else would notice.
//
//    It asserts on the RESPONSE BODY, not on a status code, and not
//    only on the fields it expects: it marshals the post and greps the
//    whole JSON for every identifying string. A field added to
//    PostAuthor later that happens to carry the name fails this test
//    rather than shipping quietly.
//
//    ⚠️ The anonymous path is REACHABLE. `GET /posts` 401s anonymous
//    callers (posts are members-only — auth.PublicSurfaceRoutes
//    deliberately omits the feed), but `/posts/by-asset` and
//    `/collections/{id}/posts` are both public-surface routes that
//    return PostList through the same enrichment. So this is a live
//    surface, not future-proofing.
//
// 2. IT IS ONE QUERY PER PAGE. The whole justification for putting the
//    author on the payload is that the alternative — ~20 `GET /users/{ref}`
//    per feed page — is a permanent tax. A resolution that is itself an
//    N+1 would have moved the cost to the server and called it a fix.
//    TestEnrichAuthors_OneQueryForNPosts counts the queries pgx actually
//    issues, via a QueryTracer on its own pool, so the assertion is on
//    observed database traffic and not on a code shape someone could
//    refactor past.
//
// Skips without AA_DB_PASSWORD, like the other posts integration tests.

package posts

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// seedAuthor plants a user + profile row and returns its ref and
// username. `fullname` goes on the "user" row (the rung an anonymous
// caller must never reach); `displayName` and `avatarURL` go on the
// profile; `hidden` sets the opt-out.
func seedAuthor(
	t *testing.T,
	pool *pgxpool.Pool,
	fullname, displayName, avatarURL string,
	hidden bool,
) (int64, string) {
	t.Helper()
	ctx := context.Background()
	username := "au-" + uuid.NewString()[:8]
	pw := "irrelevant"
	u, err := auth.New(pool).CreateUser(ctx, auth.CreateUserParams{Username: &username, Password: &pw})
	if err != nil {
		t.Fatalf("seed author: %v", err)
	}
	if fullname != "" {
		if _, err := pool.Exec(ctx, `UPDATE "user" SET fullname = $1 WHERE ref = $2`, fullname, u.Ref); err != nil {
			t.Fatalf("set fullname: %v", err)
		}
	}
	var avatar *string
	if avatarURL != "" {
		avatar = &avatarURL
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_profiles (user_ref, display_name, avatar_url, hide_from_anonymous)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_ref) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       avatar_url = EXCLUDED.avatar_url,
		       hide_from_anonymous = EXCLUDED.hide_from_anonymous`,
		u.Ref, displayName, avatar, hidden); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_profiles WHERE user_ref = $1`, u.Ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, u.Ref)
	})
	return u.Ref, username
}

// seedAuthoredPost creates a memberless public post by the given author.
// Memberless on purpose: this pass is about the author and nothing else,
// and a post with no members is a real case (ADR 0073 articles, or a
// post whose only asset was soft-deleted).
func seedAuthoredPost(t *testing.T, pool *pgxpool.Pool, author int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1,$2,'author enrich probe','public')`,
		id, author); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

// authorBodyJSON marshals the post the way the API would and returns
// the JSON as a string, for substring assertions against the WHOLE
// payload. (Named apart from write_enrich_test.go's `bodyOf`, which
// renders through an httptest recorder for a different purpose.)
func authorBodyJSON(t *testing.T, p *openapi.Post) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal post: %v", err)
	}
	return string(b)
}

// TestEnrichAuthors_AnonymousWithholding is the load-bearing assertion
// of #557: an anonymous caller must not receive the identity of an
// author who opted out of anonymous exposure, and an authenticated
// caller must.
//
// Two subjects, because the withholding has two halves and only one of
// them is obvious:
//
//   - `hidden` opted out. Anonymous gets NO author object at all — not
//     a redacted one, not a "hidden user" placeholder. A placeholder
//     saying somebody opted out still discloses that they posted.
//   - `open` did not opt out, and has NO profile display_name, so their
//     display string falls back a rung. Anonymous must land on the
//     USERNAME, not on `user.fullname` — real name is
//     authenticated-only (ADR 0070 §3) and the display-name ladder is
//     the quiet way to leak it. This half is the one a test that only
//     checked the opt-out would miss entirely.
func TestEnrichAuthors_AnonymousWithholding(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	const (
		hiddenReal    = "Hidden Real Name"
		hiddenDisplay = "Hidden Display Name"
		hiddenAvatar  = "https://example.invalid/hidden-avatar.png"
		openReal      = "Open Real Name"
	)
	hiddenRef, hiddenUser := seedAuthor(t, pool, hiddenReal, hiddenDisplay, hiddenAvatar, true)
	openRef, openUser := seedAuthor(t, pool, openReal, "", "", false)

	hiddenPost := seedAuthoredPost(t, pool, hiddenRef)
	openPost := seedAuthoredPost(t, pool, openRef)

	load := func(id uuid.UUID) *openapi.Post {
		t.Helper()
		p, err := h.fetchFullPost(context.Background(), pgtype.UUID{Bytes: id, Valid: true})
		if err != nil {
			t.Fatalf("fetch %v: %v", id, err)
		}
		return p
	}

	// ── Anonymous ────────────────────────────────────────────────────
	anonCtx := context.Background() // no identity == anonymous
	anonHidden, anonOpen := load(hiddenPost), load(openPost)
	if err := h.enrichAuthors(anonCtx, anonHidden, anonOpen); err != nil {
		t.Fatalf("enrich anonymous: %v", err)
	}

	if anonHidden.Author != nil {
		t.Errorf("anonymous received an author object for an opted-out user: %+v", *anonHidden.Author)
	}
	// The whole body, not just the field we expected to be nil. A future
	// field carrying the name fails here.
	body := authorBodyJSON(t, anonHidden)
	for _, leak := range []string{hiddenReal, hiddenDisplay, hiddenAvatar, hiddenUser} {
		if strings.Contains(body, leak) {
			t.Errorf("anonymous post body leaked %q from an opted-out author: %s", leak, body)
		}
	}
	// The post itself is unaffected — its own visibility is a separate
	// question, already answered by the read rule. Withholding the
	// identity must not withhold the post.
	if anonHidden.AuthorUserRef != hiddenRef {
		t.Errorf("author_user_ref should be untouched, got %d want %d", anonHidden.AuthorUserRef, hiddenRef)
	}

	if anonOpen.Author == nil {
		t.Fatal("anonymous should receive the author of a user who did NOT opt out")
	}
	if got := anonOpen.Author.DisplayName; got != openUser {
		t.Errorf("anonymous display_name = %q, want the username %q (the fullname rung is authenticated-only)", got, openUser)
	}
	if strings.Contains(authorBodyJSON(t, anonOpen), openReal) {
		t.Errorf("anonymous post body leaked the real name %q via the display-name fallback", openReal)
	}

	// ── Authenticated ────────────────────────────────────────────────
	// A plain signed-in stranger — no capabilities, not the author.
	authCtx := ctxAs(peStranger)
	authHidden, authOpen := load(hiddenPost), load(openPost)
	if err := h.enrichAuthors(authCtx, authHidden, authOpen); err != nil {
		t.Fatalf("enrich authenticated: %v", err)
	}

	if authHidden.Author == nil {
		t.Fatal("an authenticated caller must still receive the opted-out user's identity (the opt-out is about ANONYMOUS exposure)")
	}
	if got := authHidden.Author.DisplayName; got != hiddenDisplay {
		t.Errorf("authenticated display_name = %q, want %q", got, hiddenDisplay)
	}
	if got := authHidden.Author.Username; got != hiddenUser {
		t.Errorf("authenticated username = %q, want %q", got, hiddenUser)
	}
	if authHidden.Author.AvatarUrl == nil || *authHidden.Author.AvatarUrl != hiddenAvatar {
		t.Errorf("authenticated avatar_url = %v, want %q", authHidden.Author.AvatarUrl, hiddenAvatar)
	}
	if authHidden.Author.Ref != hiddenRef {
		t.Errorf("authenticated ref = %d, want %d", authHidden.Author.Ref, hiddenRef)
	}

	if authOpen.Author == nil {
		t.Fatal("authenticated caller lost the open author")
	}
	if got := authOpen.Author.DisplayName; got != openReal {
		t.Errorf("authenticated display_name = %q, want the fullname %q", got, openReal)
	}
}

// TestEnrichAuthors_CacheIsolation pins the reason this is an
// enrichment pass and not a column on the cached Post.
//
// fetchFullPost reads through h.byID, a cross-caller LRU. If the author
// were baked in there, the FIRST reader of a post would decide what
// every later reader sees — an authenticated read would populate the
// entry with an identity, and the next anonymous reader would get it on
// a cache hit. The opt-out would then hold or fail depending on read
// order, which is the worst possible failure mode: intermittent, and
// only for the minority who set the flag.
//
// So: read authenticated first (warming the cache with a post that HAS
// an author), then read anonymously through the same handler and assert
// the identity is gone.
func TestEnrichAuthors_CacheIsolation(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	const display = "Cache Isolation Subject"
	ref, _ := seedAuthor(t, pool, "Cache Isolation Real Name", display, "", true)
	postID := seedAuthoredPost(t, pool, ref)
	pgID := pgtype.UUID{Bytes: postID, Valid: true}

	warm, err := h.fetchFullPost(context.Background(), pgID)
	if err != nil {
		t.Fatalf("fetch (warm): %v", err)
	}
	if err := h.enrichAuthors(ctxAs(peStranger), warm); err != nil {
		t.Fatalf("enrich authenticated: %v", err)
	}
	if warm.Author == nil {
		t.Fatal("authenticated read should carry the author (precondition)")
	}

	// Same handler, same cache entry, anonymous caller.
	cold, err := h.fetchFullPost(context.Background(), pgID)
	if err != nil {
		t.Fatalf("fetch (cached): %v", err)
	}
	if err := h.enrichAuthors(context.Background(), cold); err != nil {
		t.Fatalf("enrich anonymous: %v", err)
	}
	if cold.Author != nil {
		t.Errorf("the cached post served an author to an anonymous caller: %+v", *cold.Author)
	}
	if strings.Contains(authorBodyJSON(t, cold), display) {
		t.Errorf("cached post body leaked %q to an anonymous caller", display)
	}
	// And the authenticated copy is untouched — the anonymous pass must
	// not have reached back through the cache and cleared it.
	if warm.Author == nil {
		t.Error("the anonymous pass mutated the authenticated caller's post")
	}
}

// countingTracer counts pgx query starts. pgx calls TraceQueryStart once
// per Query/Exec/QueryRow, which is exactly the unit "how many round
// trips did this cost" is asking about.
type countingTracer struct {
	mu sync.Mutex
	n  int
	on bool
}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on {
		c.n++
	}
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *countingTracer) start() { c.mu.Lock(); c.on = true; c.n = 0; c.mu.Unlock() }

func (c *countingTracer) stop() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.on = false
	return c.n
}

// tracedPool is previewPool with a query counter wired into the
// connection config. Its own pool, because the tracer is per-connection
// config and the shared helper's pool has none.
func tracedPool(t *testing.T) (*pgxpool.Pool, *countingTracer) {
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
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tracer := &countingTracer{}
	cfg.ConnConfig.Tracer = tracer
	// One connection: pgx's own connection SETUP issues queries, and a
	// pool that opens a second connection mid-measurement would count
	// them. Pinning the pool to one warmed connection makes the counter
	// mean "queries this code issued".
	cfg.MinConns, cfg.MaxConns = 1, 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, tracer
}

// TestEnrichAuthors_OneQueryForNPosts is the anti-N+1 assertion.
//
// It counts the queries pgx issues, not the shape of the code. 24 posts
// by 6 authors resolve in ONE query; the same measurement with a single
// post is also one, so the count does not scale with either N.
//
// If someone later "simplifies" this into a per-post lookup — the exact
// thing the payload denormalisation exists to remove — the count
// becomes 24 and this fails.
func TestEnrichAuthors_OneQueryForNPosts(t *testing.T) {
	pool, tracer := tracedPool(t)
	h := peHandler(pool)

	const (
		authors        = 6
		postsPerAuthor = 4
	)
	posts := make([]*openapi.Post, 0, authors*postsPerAuthor)
	for i := 0; i < authors; i++ {
		ref, _ := seedAuthor(t, pool, "", "Batch Author", "", false)
		for j := 0; j < postsPerAuthor; j++ {
			id := seedAuthoredPost(t, pool, ref)
			posts = append(posts, &openapi.Post{
				Id:            id,
				AuthorUserRef: ref,
			})
		}
	}

	ctx := ctxAs(peStranger)

	tracer.start()
	if err := h.enrichAuthors(ctx, posts...); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	got := tracer.stop()
	if got != 1 {
		t.Errorf("enrichAuthors issued %d queries for %d posts by %d authors; want exactly 1",
			got, len(posts), authors)
	}

	// Every post got its author — a "one query" that resolved nothing
	// would also pass the count.
	for i, p := range posts {
		if p.Author == nil {
			t.Fatalf("post %d has no author after enrichment", i)
		}
		if p.Author.Ref != p.AuthorUserRef {
			t.Errorf("post %d: author.ref = %d, want %d", i, p.Author.Ref, p.AuthorUserRef)
		}
	}

	// And the count does not depend on N: one post is also one query.
	tracer.start()
	if err := h.enrichAuthors(ctx, posts[0]); err != nil {
		t.Fatalf("enrich single: %v", err)
	}
	if got := tracer.stop(); got != 1 {
		t.Errorf("enrichAuthors issued %d queries for 1 post; want exactly 1", got)
	}

	// Zero posts must not touch the database at all.
	tracer.start()
	if err := h.enrichAuthors(ctx); err != nil {
		t.Fatalf("enrich empty: %v", err)
	}
	if got := tracer.stop(); got != 0 {
		t.Errorf("enrichAuthors issued %d queries for an empty page; want 0", got)
	}
}
