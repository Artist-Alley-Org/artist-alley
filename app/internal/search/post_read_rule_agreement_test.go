// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #873 — a post you can read is a post you can FIND.
//
// The defect: browse composed the rich post read rule (author, the five
// tiers, the follow graph, live ACL grants) and search composed
// visibility.Filter's coarser EntityPost branch, `public OR author`. So
// an org-only post — the walled-garden DEFAULT tier — was on your feed
// and absent from your search results. No error, no empty state that
// explains itself, just absence; and the tag facet and the autocomplete
// were wrong the same way, which is worse, because an undercount looks
// exactly like a correct count.
//
// The assertion is therefore an AGREEMENT, not a list of expectations:
// for every (caller × post) pair, search, the tag facet and the
// completions must return the post iff BROWSE returns it. Browse is the
// oracle because it runs the rule the product means (and, since #660, the
// same rule GET /posts/{id} enforces). A `want` column sits beside it so
// "all four surfaces are wrong in the same way" cannot pass — the same
// two-part shape as posts.TestListPosts_NeverExceedsSingleItemGate plus
// TestListPosts_TierMatrix.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/posts"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Synthetic refs. posts.author_user_ref, user_follows and
// post_acls.principal_id carry no FK to the user table (federation-
// friendly by design), so these need no rows in "user".
const (
	prAuthor    int64 = 8730001 // owns one post per tier
	prGrantee   int64 = 8730002 // signed in, no relationship; holds the ACL grants
	prFollower  int64 = 8730003 // follows prAuthor
	prModerator int64 = 8730004 // posts.admin
)

// prPhrase appears in every fixture title, and nowhere else in any
// developer's database, so a hit is attributable to these rows. It is
// also the suggest prefix and the search query text.
const prPhrase = "zarvexil"

// prPost is one fixture row: a tier, an author, and (for the two
// explicit-share rows) a grant to prGrantee.
type prPost struct {
	name       string
	author     int64
	visibility string
	grant      string // "", "live" or "expired"
}

var prFixture = []prPost{
	{name: "public", author: prAuthor, visibility: "public"},
	{name: "org-only", author: prAuthor, visibility: "org-only"},
	{name: "private", author: prAuthor, visibility: "private"},
	{name: "followers", author: prAuthor, visibility: "followers"},
	{name: "shared-live", author: prAuthor, visibility: "explicit-share", grant: "live"},
	{name: "shared-expired", author: prAuthor, visibility: "explicit-share", grant: "expired"},
	// The grantee's OWN post, at the tier nobody else may read, so
	// "returned my own post" is a distinct column from every other case.
	{name: "own", author: prGrantee, visibility: "private"},
}

// prTitle / prTag are per-post and unique: the facet answers in tags and
// suggest answers in titles, so each surface needs a value that maps
// back to exactly one row.
func prTitle(name string) string { return prPhrase + " " + name }
func prTag(name string) string   { return prPhrase + "-" + name }

// prSeed plants the fixture and returns name → id.
func prSeed(t *testing.T, pool *pgxpool.Pool) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ids := map[string]uuid.UUID{}
	for _, p := range prFixture {
		id := uuid.New()
		ids[p.name] = id
		if _, err := pool.Exec(ctx, `
			INSERT INTO posts (id, author_user_ref, title, description, visibility)
			VALUES ($1, $2, $3, $4, $5)`,
			id, p.author, prTitle(p.name), "fixture body "+p.name, p.visibility); err != nil {
			t.Fatalf("seed %s post: %v", p.name, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_tags (post_id, tag) VALUES ($1, $2)`, id, prTag(p.name)); err != nil {
			t.Fatalf("seed %s tag: %v", p.name, err)
		}
		switch p.grant {
		case "live":
			prGrant(t, pool, id, "NOW() + INTERVAL '1 day'")
		case "expired":
			prGrant(t, pool, id, "NOW() - INTERVAL '1 day'")
		}
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM post_acls WHERE post_id = $1`, id)
			_, _ = pool.Exec(c, `DELETE FROM post_tags WHERE post_id = $1`, id)
			_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
		})
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, prFollower, prAuthor); err != nil {
		t.Fatalf("seed follow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_follows WHERE follower_user_ref = $1`, prFollower)
	})
	return ids
}

// prGrant writes one post_acls row for prGrantee with the given expiry
// expression. principal_id is TEXT and the ref is a bigint — the same
// mismatch the rule's own cast exists for (#874).
func prGrant(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID, expiry string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO post_acls (post_id, principal_type, principal_id, permission, expires_at)
		VALUES ($1, 'user', $2::BIGINT::TEXT, 'read', `+expiry+`)`,
		postID, prGrantee); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
}

// prCaller is one caller class under test.
type prCaller struct {
	name string
	id   *auth.Identity // nil = anonymous
	ref  *int64         // nil = anonymous
	caps visibility.PostCaps
	// want[fixture name] — may this caller read this post? Stated
	// independently of every surface, so all four being wrong together
	// still fails.
	want map[string]bool
}

func prCallers() []prCaller {
	grantee, follower, moderator := prGrantee, prFollower, prModerator
	return []prCaller{
		{
			name: "anonymous", id: nil, ref: nil,
			want: map[string]bool{
				"public": true, "org-only": false, "private": false, "followers": false,
				"shared-live": false, "shared-expired": false, "own": false,
			},
		},
		{
			// The grant holder. org-only is the widened case that
			// mattered most in practice: it is the DEFAULT tier, so
			// before #873 most of the corpus was unfindable.
			name: "grantee", id: prIdentity(prGrantee), ref: &grantee,
			want: map[string]bool{
				"public": true, "org-only": true, "private": false, "followers": false,
				"shared-live": true, "shared-expired": false, "own": true,
			},
		},
		{
			name: "follower", id: prIdentity(prFollower), ref: &follower,
			want: map[string]bool{
				"public": true, "org-only": true, "private": false, "followers": true,
				"shared-live": false, "shared-expired": false, "own": false,
			},
		},
		{
			// posts.admin opens `private` and nothing else — matching
			// what the single-item gate has always granted a moderator.
			// It does NOT open followers or explicit-share.
			name: "moderator", id: prIdentity(prModerator, posts.CapPostsAdmin), ref: &moderator,
			caps: visibility.PostCaps{SeesAllPrivate: true},
			want: map[string]bool{
				"public": true, "org-only": true, "private": true, "followers": false,
				"shared-live": false, "shared-expired": false, "own": true,
			},
		},
	}
}

func prIdentity(ref int64, caps ...string) *auth.Identity {
	return &auth.Identity{UserRef: ref, AuthMethod: "session", Capabilities: caps}
}

// prBrowsed is the ORACLE: the ids the post feed hands this caller.
// posts.ListPostsPageGated is the browse query itself, not a re-statement
// of it.
func prBrowsed(t *testing.T, pool *pgxpool.Pool, c prCaller) map[uuid.UUID]bool {
	t.Helper()
	h := &posts.Handler{Pool: pool}
	rows, err := h.ListPostsPageGated(context.Background(), c.id, posts.ListPostsPageParams{
		RowLimit:       200,
		CursorPostedAt: pgtype.Timestamptz{},
		CursorID:       pgtype.UUID{},
	})
	if err != nil {
		t.Fatalf("browse as %s: %v", c.name, err)
	}
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[uuid.UUID(r.ID.Bytes)] = true
	}
	return out
}

// prSearched is /search, restricted to posts.
func prSearched(t *testing.T, pool *pgxpool.Pool, c prCaller) map[uuid.UUID]bool {
	t.Helper()
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          prPhrase,
		Types:         []HitType{HitTypePost},
		Limit:         MaxLimit,
		CallerUserRef: c.ref,
		PostCaps:      c.caps,
	})
	if err != nil {
		t.Fatalf("search as %s: %v", c.name, err)
	}
	out := map[uuid.UUID]bool{}
	for _, h := range res.Hits {
		out[h.ID] = true
	}
	return out
}

// prFaceted is /search/facets: the tag bucket set. Each fixture post
// carries one unique tag, so a bucket present means that post counted.
func prFaceted(t *testing.T, pool *pgxpool.Pool, c prCaller) map[string]int64 {
	t.Helper()
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	resp := d.Run(context.Background(), facet.Request{
		QueryText: prPhrase,
		Facets:    []facet.FacetType{facet.FacetTag},
		Caller:    visibility.NewCaller(c.ref),
		PostCaps:  c.caps,
	})
	out := map[string]int64{}
	for _, b := range resp.Facets[facet.FacetTag].Buckets {
		out[b.Value] = b.Count
	}
	return out
}

// prSuggested is /search/suggest: the post-title completion set.
func prSuggested(t *testing.T, pool *pgxpool.Pool, c prCaller) map[string]bool {
	t.Helper()
	resp, err := suggest.NewService(pool).Suggest(context.Background(), suggest.Request{
		Prefix:   prPhrase,
		Caller:   visibility.NewCaller(c.ref),
		PostCaps: c.caps,
		Limit:    suggest.MaxResults,
	})
	if err != nil {
		t.Fatalf("suggest as %s: %v", c.name, err)
	}
	out := map[string]bool{}
	for _, s := range resp.Suggestions {
		if s.Kind == suggest.KindPostTitle {
			out[s.Value] = true
		}
	}
	return out
}

// TestPostReadRule_SearchAgreesWithBrowse is the #873 regression test.
//
// It fails on today's dev in eleven places: every org-only, followers
// and ACL-granted cell, on all three search surfaces, plus the
// moderator's private one.
func TestPostReadRule_SearchAgreesWithBrowse(t *testing.T) {
	pool := coPool(t)
	ids := prSeed(t, pool)

	for _, c := range prCallers() {
		t.Run(c.name, func(t *testing.T) {
			browsed := prBrowsed(t, pool, c)
			searched := prSearched(t, pool, c)
			faceted := prFaceted(t, pool, c)
			suggested := prSuggested(t, pool, c)

			for _, p := range prFixture {
				want := c.want[p.name]
				id := ids[p.name]

				// The intent oracle first: if browse itself is wrong,
				// agreement with it proves nothing.
				if got := browsed[id]; got != want {
					t.Errorf("BROWSE: %s post returned=%v, want %v — the oracle itself "+
						"disagrees with the stated rule", p.name, got, want)
				}
				if got := searched[id]; got != want {
					t.Errorf("SEARCH: %s post returned=%v, want %v (browse=%v) — a post you "+
						"can read must be a post you can find",
						p.name, got, want, browsed[id])
				}
				if got := faceted[prTag(p.name)] > 0; got != want {
					t.Errorf("FACET: %s post counted=%v, want %v (browse=%v) — an undercount "+
						"is indistinguishable from a correct count",
						p.name, got, want, browsed[id])
				}
				if got := suggested[prTitle(p.name)]; got != want {
					t.Errorf("SUGGEST: %s post completed=%v, want %v (browse=%v)",
						p.name, got, want, browsed[id])
				}
			}
		})
	}
}

// TestPostSearchCache_KeyedOnPostCaps pins the cache half. The cached
// result set is now the post-read-rule one, so a key that ignores
// posts.admin keeps serving a caller the wider page after the capability
// was REVOKED — for the whole TTL. Same failure #899 fixed for the
// content plane; the post plane arrived with #873 and needs its own.
func TestPostSearchCache_KeyedOnPostCaps(t *testing.T) {
	ref := prGrantee
	base := Query{Text: prPhrase, Limit: 25, CallerUserRef: &ref}
	withCap := base
	withCap.PostCaps = visibility.PostCaps{SeesAllPrivate: true}
	if keyForQuery(base) == keyForQuery(withCap) {
		t.Fatal("the search cache key ignores post capabilities: a caller who LOSES " +
			"posts.admin would keep being served the cached private-inclusive " +
			"result set until the entry expired")
	}
}
