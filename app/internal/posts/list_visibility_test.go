// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #660 — the list path may never return a post the single-item path
// would refuse.
//
// The bug these tests exist for: `GET /posts?visibility=private` put the
// caller-supplied tier into the feed query as a bare `visibility = $n`
// with no author or relationship conjunct, so any signed-in caller got
// every author's private posts — title, description and all — while
// `GET /posts/{id}` on the very same id returned 403. Two expressions of
// one rule; the weaker one was the one that shipped.
//
// So the load-bearing test here is NOT "private posts are hidden". It is
// the AGREEMENT: for every tier a post can hold and every caller class,
// anything the list hands out must also be obtainable from GET
// /posts/{id}. Written that way it keeps its teeth as the rule changes —
// widen the rule and the test follows it, add a tier and the test picks
// it up from the database's own CHECK constraint rather than from a list
// somebody has to remember to extend.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs. posts.author_user_ref and user_follows carry no FK to
// the user table (federation-friendly by design), so these need no rows
// in "user" — which also keeps the fixture out of every other suite's
// way.
const (
	lvAuthor    int64 = 6600001 // owns one post per tier
	lvStranger  int64 = 6600002 // signed in, no relationship, no caps
	lvFollower  int64 = 6600003 // follows lvAuthor
	lvModerator int64 = 6600004 // posts.admin
)

// postVisibilityTiers reads the tiers a post row may hold from the
// database's own CHECK constraint, so a tier added by a later migration
// is covered here without anyone editing this file. Hardcoding the list
// is what let `explicit-share` sit unexamined while `private` got all
// the attention.
func postVisibilityTiers(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var def string
	if err := pool.QueryRow(t.Context(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conname = 'posts_visibility_check'`).Scan(&def); err != nil {
		t.Fatalf("read posts_visibility_check: %v", err)
	}
	found := regexp.MustCompile(`'([a-z-]+)'::text`).FindAllStringSubmatch(def, -1)
	if len(found) == 0 {
		t.Fatalf("no tiers parsed out of constraint: %s", def)
	}
	tiers := make([]string, 0, len(found))
	for _, m := range found {
		tiers = append(tiers, m[1])
	}
	sort.Strings(tiers)
	return tiers
}

// seedTierPost plants one post at one tier and cleans it up after.
func seedTierPost(t *testing.T, pool *pgxpool.Pool, author int64, visibility string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, author, "lv "+visibility, "lv body "+visibility, visibility); err != nil {
		t.Fatalf("seed %s post: %v", visibility, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

func lvIdentity(ref int64, caps ...string) *auth.Identity {
	return &auth.Identity{UserRef: ref, AuthMethod: "session", Capabilities: caps}
}

// lvListed runs one list request and returns the posts it handed out.
func lvListed(t *testing.T, h *Handler, id *auth.Identity, visibility *string) []openapi.Post {
	t.Helper()
	limit := maxListLimit
	params := openapi.ListPostsParams{Limit: &limit}
	if visibility != nil {
		v := openapi.ListPostsParamsVisibility(*visibility)
		params.Visibility = &v
	}
	resp, err := h.ListPosts(auth.WithIdentity(t.Context(), id), openapi.ListPostsRequestObject{Params: params})
	if err != nil {
		t.Fatalf("ListPosts: %v", err)
	}
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts returned %T, want 200", resp)
	}
	return ok.Items
}

// lvGetOK reports whether the single-item path hands this post to this
// caller. This is the oracle: whatever it refuses, the list must not
// return.
func lvGetOK(t *testing.T, h *Handler, id *auth.Identity, postID openapi.Post) bool {
	t.Helper()
	resp, err := h.GetPost(auth.WithIdentity(t.Context(), id), openapi.GetPostRequestObject{Id: postID.Id})
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	_, is := resp.(openapi.GetPost200JSONResponse)
	return is
}

// TestListPosts_NeverExceedsSingleItemGate is the #660 regression test.
//
// It fails on the pre-fix code: as lvStranger with visibility=private it
// receives lvAuthor's private post from the list, and GetPost on that
// same id answers 403.
func TestListPosts_NeverExceedsSingleItemGate(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	tiers := postVisibilityTiers(t, pool)
	for _, v := range tiers {
		seedTierPost(t, pool, lvAuthor, v)
	}
	// One post owned by the stranger too, so "the list returned
	// something" is never trivially satisfied by an empty page.
	seedTierPost(t, pool, lvStranger, "org-only")

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, lvFollower, lvAuthor); err != nil {
		t.Fatalf("seed follow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_follows WHERE follower_user_ref=$1`, lvFollower)
	})

	callers := map[string]*auth.Identity{
		"author":    lvIdentity(lvAuthor),
		"stranger":  lvIdentity(lvStranger),
		"follower":  lvIdentity(lvFollower),
		"moderator": lvIdentity(lvModerator, CapPostsAdmin),
		"superuser": lvIdentity(lvModerator, auth.SuperAdminCapability),
	}

	// Every tier the API will accept as a filter, plus the unfiltered
	// feed. A tier the DB allows but the API's enum does not is skipped
	// rather than forced through: the handler is reached only via a
	// validated request, and testing a value production cannot send
	// would be a fixture pretending to be a code path.
	filters := []*string{nil}
	for _, tier := range tiers {
		if !openapi.ListPostsParamsVisibility(tier).Valid() {
			t.Logf("tier %q is not a ListPostsParamsVisibility value; skipped as a filter", tier)
			continue
		}
		v := tier
		filters = append(filters, &v)
	}

	for name, id := range callers {
		for _, f := range filters {
			label := "(none)"
			if f != nil {
				label = *f
			}
			for _, p := range lvListed(t, h, id, f) {
				if lvGetOK(t, h, id, p) {
					continue
				}
				t.Errorf(
					"caller %s, ?visibility=%s: list returned post %s "+
						"(author %d, visibility %s, title %q) that GET /posts/{id} refuses — "+
						"the list path is wider than the read gate",
					name, label, p.Id, p.AuthorUserRef, p.Visibility, p.Title,
				)
			}
		}
	}
}

// TestListPosts_TierMatrix is the intent oracle beside the invariant
// above. The agreement test cannot tell "both paths are right" from
// "both paths are wrong in the same way", so this one states what each
// caller class is supposed to see, tier by tier, and fails if the shared
// rule drifts in either direction.
func TestListPosts_TierMatrix(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	ids := map[string]uuid.UUID{}
	for _, v := range []string{"private", "org-only", "followers", "explicit-share"} {
		ids[v] = seedTierPost(t, pool, lvAuthor, v)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, lvFollower, lvAuthor); err != nil {
		t.Fatalf("seed follow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_follows WHERE follower_user_ref=$1`, lvFollower)
	})

	// want[caller][tier] — may this caller find lvAuthor's post at this
	// tier by filtering the list on that tier?
	cases := []struct {
		caller string
		id     *auth.Identity
		want   map[string]bool
	}{
		{
			// The author sees every one of their own tiers. #660 must not
			// be "fixed" by making the feed narrower for legitimate use.
			caller: "author", id: lvIdentity(lvAuthor),
			want: map[string]bool{"private": true, "org-only": true, "followers": true, "explicit-share": true},
		},
		{
			caller: "stranger", id: lvIdentity(lvStranger),
			want: map[string]bool{"private": false, "org-only": true, "followers": false, "explicit-share": false},
		},
		{
			// Following the author opens the followers tier and nothing else.
			caller: "follower", id: lvIdentity(lvFollower),
			want: map[string]bool{"private": false, "org-only": true, "followers": true, "explicit-share": false},
		},
		{
			// posts.admin opens `private` — matching what the single-item
			// gate has always granted a moderator. It does NOT open
			// explicit-share, because the read gate does not either.
			caller: "moderator", id: lvIdentity(lvModerator, CapPostsAdmin),
			want: map[string]bool{"private": true, "org-only": true, "followers": false, "explicit-share": false},
		},
	}

	for _, tc := range cases {
		for tier, want := range tc.want {
			v := tier
			got := false
			for _, p := range lvListed(t, h, tc.id, &v) {
				if uuid.UUID(p.Id) == ids[tier] {
					got = true
					break
				}
			}
			if got != want {
				t.Errorf("%s, ?visibility=%s: author's post visible = %v, want %v",
					tc.caller, tier, got, want)
			}
		}
	}
}

// TestListPosts_VisibilityFilterOnlyNarrows pins the semantics chosen for
// #660: `?visibility=X` selects among the tiers the caller's identity
// already admits. For a non-author `?visibility=private` therefore means
// "my own private posts" — not "everybody's", and not "nothing".
func TestListPosts_VisibilityFilterOnlyNarrows(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	theirs := seedTierPost(t, pool, lvAuthor, "private")
	mine := seedTierPost(t, pool, lvStranger, "private")

	v := "private"
	var sawMine, sawTheirs bool
	for _, p := range lvListed(t, h, lvIdentity(lvStranger), &v) {
		switch uuid.UUID(p.Id) {
		case mine:
			sawMine = true
		case theirs:
			sawTheirs = true
		}
	}
	if !sawMine {
		t.Error("?visibility=private dropped the caller's OWN private post — the filter narrowed past what they may read")
	}
	if sawTheirs {
		t.Error("?visibility=private returned another author's private post — #660 has regressed")
	}
}
