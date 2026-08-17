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
	"strconv"
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

// TestListPostsPage_TrashViewStillAuthorizes is the soft-delete half of
// #873, and it exists because that failure is SILENT.
//
// The read rule deliberately carries no `deleted_at` conjunct — soft-
// delete is an orthogonal axis each caller owns, so the feed can offer
// an admin-only include_deleted flag. Moving the rule into
// visibility.Predicate, which DOES own soft-delete, put those two axes
// in one expression for the first time; the danger is a shape where
// waiving soft-delete waives an authorization disjunct with it. The
// extra rows that would return look exactly like the deleted ones the
// caller asked for, so nothing about the response says anything is
// wrong.
//
// Both halves are asserted, because either one alone is passable by a
// broken implementation: the trash view still SEES deleted posts, and it
// still REFUSES the ones the caller may not read.
func TestListPostsPage_TrashViewStillAuthorizes(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	deleted := map[string]uuid.UUID{}
	for _, tier := range []string{"private", "org-only", "followers"} {
		id := seedTierPost(t, pool, lvAuthor, tier)
		if _, err := pool.Exec(t.Context(),
			`UPDATE posts SET deleted_at = NOW() WHERE id = $1`, id); err != nil {
			t.Fatalf("soft-delete %s: %v", tier, err)
		}
		deleted[tier] = id
	}

	includeDeleted := true
	listed := func(id *auth.Identity) map[uuid.UUID]bool {
		t.Helper()
		rows, err := h.ListPostsPageGated(auth.WithIdentity(t.Context(), id), id,
			ListPostsPageParams{IncludeDeleted: &includeDeleted, RowLimit: maxListLimit})
		if err != nil {
			t.Fatalf("ListPostsPageGated: %v", err)
		}
		out := map[uuid.UUID]bool{}
		for _, r := range rows {
			out[uuid.UUID(r.ID.Bytes)] = true
		}
		return out
	}

	// posts.admin: the trash view works — the private post is there —
	// and it is still bounded by the rule, so the followers post they do
	// not follow is not.
	mod := listed(lvIdentity(lvModerator, CapPostsAdmin))
	if !mod[deleted["private"]] {
		t.Error("the admin trash view lost a soft-deleted PRIVATE post — include_deleted " +
			"stopped showing deleted rows, which is the flag's entire purpose")
	}
	if mod[deleted["followers"]] {
		t.Error("the admin trash view returned a followers-only post to a caller who does " +
			"not follow the author — waiving soft-delete waived an authorization disjunct too")
	}

	// A non-admin caller passing the same flag (the handler gates it, but
	// the query must not depend on that gate) still gets only what the
	// rule admits.
	str := listed(lvIdentity(lvStranger))
	if str[deleted["private"]] {
		t.Error("include_deleted handed a stranger another author's private post")
	}
	if str[deleted["followers"]] {
		t.Error("include_deleted handed a stranger a followers-only post they cannot read")
	}
	if !str[deleted["org-only"]] {
		t.Error("include_deleted dropped a soft-deleted org-only post the stranger may read — " +
			"the soft-delete waiver did not take effect at all")
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

// ---------------------------------------------------------------------------
// #1193 — the DEFAULT display filter
// ---------------------------------------------------------------------------

// TestDefaultFeedTiers_IsEveryTierButPrivate reads the tiers out of the
// database's own CHECK constraint and requires the browse default to
// decide about every one of them.
//
// It is the counterpart of postVisibilityTiers' use above, and it exists
// because #1193 IS the failure it guards: `public` was added to the
// column by migration 00008 and the browse default — a hand-written list
// of one tier — never learned about it, so for three releases a member's
// wall silently excluded every post published to the world. A tier added
// tomorrow will be a SHARED tier far more often than a private one, so
// the list that must not go stale is this one.
//
// Stated as "the constraint's tiers, minus private" rather than as a
// literal set, so it fails on a NEW tier rather than passing on a
// hardcoded copy of the answer.
func TestDefaultFeedTiers_IsEveryTierButPrivate(t *testing.T) {
	pool := previewPool(t)

	want := map[string]bool{}
	for _, tier := range postVisibilityTiers(t, pool) {
		if tier != "private" {
			want[tier] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("no tiers parsed out of posts_visibility_check — the comparison below would be vacuous")
	}

	got := map[string]bool{}
	for _, tier := range defaultFeedTiers {
		got[tier] = true
	}
	if got["private"] {
		t.Error("defaultFeedTiers contains `private` — the browse wall would show a " +
			"caller their own unpublished drafts, and a moderator EVERY user's")
	}
	for tier := range want {
		if !got[tier] {
			t.Errorf("the tier %q exists in posts_visibility_check and is missing from "+
				"defaultFeedTiers — a post at that tier is invisible on the browse wall "+
				"to everyone entitled to read it, which is exactly #1193", tier)
		}
	}
	for tier := range got {
		if tier != "private" && !want[tier] {
			t.Errorf("defaultFeedTiers names %q, which the database does not allow", tier)
		}
	}
}

// TestListPosts_DefaultWallIsTheReadableUnion is #1193's regression test,
// and the assertion that fails loudest on the old code is the first one:
// a PUBLIC post was absent from a signed-in member's default wall.
//
// One post per tier by one author, plus the two relationships that open
// tiers (a follow, an ACL grant), then the DEFAULT page — no
// `?visibility=` at all, which is what every frontend surface sends —
// for four caller classes.
//
// Both directions are asserted per caller. "Everything readable is
// present" is the fix; "private is absent" is the half of the old
// default that survives it, and without it the fix would be indist-
// inguishable from dropping the display filter entirely — which would
// put a moderator's wall full of other people's private drafts.
func TestListPosts_DefaultWallIsTheReadableUnion(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	ids := map[string]uuid.UUID{}
	for _, tier := range postVisibilityTiers(t, pool) {
		ids[tier] = seedTierPost(t, pool, lvAuthor, tier)
	}
	// lvFollower follows the author; lvStranger holds an explicit grant
	// on the explicit-share post. Two different doors into the union, so
	// a fix that opened only one of them fails here.
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, lvFollower, lvAuthor); err != nil {
		t.Fatalf("seed follow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_follows WHERE follower_user_ref=$1`, lvFollower)
	})
	aclGrant(t, pool, ids["explicit-share"], "user", strconv.FormatInt(lvStranger, 10), nil)

	cases := []struct {
		caller string
		id     *auth.Identity
		want   map[string]bool
	}{
		{
			// The whole point. A signed-in member with no relationship to
			// the author sees the world-visible post and the walled-garden
			// one; the old default showed them only the second.
			caller: "stranger", id: lvIdentity(lvStranger),
			want: map[string]bool{
				"public": true, "org-only": true,
				"followers": false, "explicit-share": true, "private": false,
			},
		},
		{
			// A follow pays off on the WALL, not only on the Following tab.
			caller: "follower", id: lvIdentity(lvFollower),
			want: map[string]bool{
				"public": true, "org-only": true,
				"followers": true, "explicit-share": false, "private": false,
			},
		},
		{
			// The author's own private post stays off their own wall. It is
			// reachable — `?visibility=private` and the own-author filter
			// both return it — just not mixed into the browse grid.
			caller: "author", id: lvIdentity(lvAuthor),
			want: map[string]bool{
				"public": true, "org-only": true,
				"followers": true, "explicit-share": true, "private": false,
			},
		},
		{
			// The load-bearing negative. posts.admin can READ every private
			// post on the instance, so a default of "no filter at all"
			// would hand a moderator every user's drafts as their browse
			// feed.
			caller: "moderator", id: lvIdentity(lvModerator, CapPostsAdmin),
			want: map[string]bool{
				"public": true, "org-only": true,
				"followers": false, "explicit-share": false, "private": false,
			},
		},
	}

	for _, tc := range cases {
		got := map[uuid.UUID]bool{}
		for _, p := range lvListed(t, h, tc.id, nil) {
			got[uuid.UUID(p.Id)] = true
		}
		for tier, wantSeen := range tc.want {
			if got[ids[tier]] == wantSeen {
				continue
			}
			if wantSeen {
				t.Errorf("%s: the %s post is MISSING from the default wall — the display "+
					"filter is narrower than what this caller may read (#1193)",
					tc.caller, tier)
				continue
			}
			t.Errorf("%s: the %s post is PRESENT on the default wall and must not be",
				tc.caller, tier)
		}
	}
}

// TestListPosts_ExplicitTierStillNarrows is the direction guard beside
// the union above.
//
// Widening a DEFAULT is the kind of change that gets implemented by
// deleting the filter, and a deleted filter takes `?visibility=` with it
// silently: every explicit narrowing would answer the whole wall, and
// nothing about the response would say so. So one tier is asked for by
// name and the other four must be absent.
func TestListPosts_ExplicitTierStillNarrows(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	ids := map[string]uuid.UUID{}
	for _, tier := range postVisibilityTiers(t, pool) {
		ids[tier] = seedTierPost(t, pool, lvAuthor, tier)
	}

	v := "public"
	got := map[uuid.UUID]bool{}
	for _, p := range lvListed(t, h, lvIdentity(lvStranger), &v) {
		got[uuid.UUID(p.Id)] = true
	}
	if !got[ids["public"]] {
		t.Fatal("?visibility=public did not return the public post — the assertions " +
			"below would pass on a filter that returns nothing at all")
	}
	if got[ids["org-only"]] {
		t.Error("?visibility=public returned an ORG-ONLY post — the explicit filter no " +
			"longer narrows, so the union default has swallowed it")
	}
}
