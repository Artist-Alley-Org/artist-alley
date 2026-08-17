// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1106 — the profile's Likes tab, and the one property that makes a
// likes listing safe to publish.
//
// # The rule
//
// A likes listing is a DERIVED listing: its rows are chosen by ONE
// user's actions and read by ANOTHER. Every item in it must therefore
// compose the VIEWER's read rule, never the liker's. User A liking a
// post they can read must not make that post's existence, title or
// cover visible to viewer B — this is the #902 / #1066 class arriving
// on a new surface, and the surface is new precisely because the row
// SET is now under a third party's control.
//
// # Why the assertion is a same-item-opposite-verdicts pair
//
// "The Likes tab returns the posts A liked" passes on a listing with no
// read rule at all. "It returns fewer than A liked" passes on a listing
// that is uniformly broken. What separates a composed listing from an
// uncomposed one is ONE post, liked once, read by two callers with
// opposite verdicts — so every case below drives the same `liked_by=A`
// query as both, and asserts the two answers against each other.
//
// The positive arm is load-bearing in both directions. A gate that
// refuses everyone would pass every withholding case here while making
// the tab permanently empty, so the readable post must SUCCEED for the
// stranger, and the follower must see the followers-tier post the
// stranger cannot — which is also what proves the rule consulted is the
// full post rule (follow graph and all) rather than "public only".
//
// # And that the filter NARROWS
//
// The `likes` table is written by whoever clicks the heart. If the
// conjunct were ORed into the read rule rather than ANDed beside it,
// anybody could put any post into anybody's feed by liking it.
// TestLikedBy_LikingDoesNotWiden drives that directly: A likes their
// OWN private post, and B — who may not read it — must not see it
// through A's likes, through browse, or anywhere else.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	lbLiker     int64 = 11060001 // does the liking; authors the posts
	lbStranger  int64 = 11060002 // follows nobody, holds nothing
	lbFollower  int64 = 11060003 // follows lbLiker
	lbOtherRef  int64 = 11060004 // authors a post lbLiker likes
	lbPageLimit       = 100
)

func lbSeedPost(t *testing.T, pool *pgxpool.Pool, author int64, tier, title string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1,$2,$3,$4)`,
		id, author, title, tier); err != nil {
		t.Fatalf("seed %s post: %v", tier, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM likes WHERE target_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

func lbLike(t *testing.T, pool *pgxpool.Pool, ref int64, postID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO likes (target_kind, target_id, user_ref) VALUES ('post', $1, $2)
		 ON CONFLICT DO NOTHING`, postID, ref); err != nil {
		t.Fatalf("seed like: %v", err)
	}
}

// lbLikesOf drives the REAL endpoint the Likes tab calls, as `viewer`.
// Titles rather than ids, because a leak is a TITLE reaching a caller
// and an id-only assertion would pass on a response that carried one.
func lbLikesOf(t *testing.T, h *Handler, viewer, liker int64) map[uuid.UUID]string {
	t.Helper()
	limit := lbPageLimit
	l := liker
	resp, err := h.ListPosts(ctxAs(viewer), openapi.ListPostsRequestObject{
		Params: openapi.ListPostsParams{LikedBy: &l, Limit: &limit},
	})
	if err != nil {
		t.Fatalf("ListPosts(liked_by): %v", err)
	}
	ok, is200 := resp.(openapi.ListPosts200JSONResponse)
	if !is200 {
		t.Fatalf("ListPosts(liked_by): got %T, want a 200", resp)
	}
	out := map[uuid.UUID]string{}
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = p.Title
	}
	return out
}

// TestLikedBy_ViewerRulePerTier is the same-item-opposite-verdicts pair,
// once per tier the posts CHECK constraint admits.
//
// One liker, one like per post, three viewers. `wantStranger` and
// `wantFollower` differ per tier, so a listing that is uniformly wrong
// in either direction fails somewhere.
func TestLikedBy_ViewerRulePerTier(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, lbFollower, lbOtherRef); err != nil {
		t.Fatalf("seed follow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_follows WHERE follower_user_ref = $1`, lbFollower)
	})

	cases := []struct {
		tier         string
		wantStranger bool
		wantFollower bool
	}{
		{"public", true, true},
		{"org-only", true, true},
		{"private", false, false},
		// The arm that proves the FULL post rule ran: only the follower
		// of the AUTHOR sees it, and the author here is not the liker.
		{"followers", false, true},
		{"explicit-share", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			title := "lb " + tc.tier + " title"
			// Authored by someone OTHER than the liker, deliberately: a
			// listing that accidentally composed the LIKER's rule would
			// pass every case if the liker were also the author.
			postID := lbSeedPost(t, pool, lbOtherRef, tc.tier, title)
			lbLike(t, pool, lbLiker, postID)

			// The control: the AUTHOR reads their own post through
			// someone else's likes. A refusal here means the listing is
			// broken, not gated.
			if got := lbLikesOf(t, h, lbOtherRef, lbLiker); got[postID] != title {
				t.Fatalf("author viewing %s: title = %q, want %q", tc.tier, got[postID], title)
			}

			for _, arm := range []struct {
				who  string
				ref  int64
				want bool
			}{
				{"stranger", lbStranger, tc.wantStranger},
				{"follower", lbFollower, tc.wantFollower},
			} {
				got := lbLikesOf(t, h, arm.ref, lbLiker)
				title2, present := got[postID]
				if arm.want {
					if !present || title2 != title {
						t.Errorf("%s on %s: got %q (present=%v), want %q — the positive arm; "+
							"a listing that refuses everyone passes every other case here",
							arm.who, tc.tier, title2, present, title)
					}
					continue
				}
				if present {
					t.Errorf("%s on %s: a liked post they CANNOT READ appeared as %q — a likes "+
						"listing composes the VIEWER's rule, not the liker's (#1106)",
						arm.who, tc.tier, title2)
				}
			}
		})
	}
}

// TestLikedBy_LikingDoesNotWiden is the narrowing property, stated as a
// property rather than inferred from the tier table.
//
// The `likes` table is written by whoever clicks the heart, so it is the
// one input to this query that the party the read rule protects against
// controls — the same shape as #1123's tag arm, where a post's own
// author writes the matching side. If the conjunct were ORed into the
// rule instead of ANDed beside it, liking a post would publish it.
//
// The assertion is a PAIR on one post: the liker (its author) sees it,
// the stranger does not — through the likes tab AND through the
// unfiltered feed, so "absent" cannot be an artifact of the filter
// itself.
func TestLikedBy_LikingDoesNotWiden(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	const title = "lb private, self-liked"
	postID := lbSeedPost(t, pool, lbLiker, "private", title)
	lbLike(t, pool, lbLiker, postID)

	if got := lbLikesOf(t, h, lbLiker, lbLiker); got[postID] != title {
		t.Fatalf("the liker cannot see their OWN liked private post (%q) — fixture or "+
			"listing is broken, not gated", got[postID])
	}
	if got, present := lbLikesOf(t, h, lbStranger, lbLiker)[postID]; present {
		t.Errorf("liking a private post published it: the stranger got %q. The likes "+
			"conjunct must be ANDed beside the read rule, never ORed into it", got)
	}

	// And it is genuinely unreadable, not merely filtered out by the
	// liked_by branch: the same caller does not get it from the feed.
	limit := lbPageLimit
	resp, err := h.ListPosts(ctxAs(lbStranger), openapi.ListPostsRequestObject{
		Params: openapi.ListPostsParams{Limit: &limit},
	})
	if err != nil {
		t.Fatalf("ListPosts: %v", err)
	}
	if ok, is200 := resp.(openapi.ListPosts200JSONResponse); is200 {
		for _, p := range ok.Items {
			if uuid.UUID(p.Id) == postID {
				t.Errorf("the private post is on the stranger's plain feed too — the fixture " +
					"does not test what this file claims")
			}
		}
	}
}

// TestLikedBy_TierDefaultDoesNotTruncate pins the display-filter call.
//
// `GET /posts` applies a default `visibility` so the browse feed shows
// the shared tiers and not a caller's own drafts (#1193; it was the
// org-only tier alone when this test was written). Left in place on the
// Likes tab that default would silently answer a different question —
// "what they liked, minus the private ones" — while looking like a
// complete list, so `liked_by` drops it exactly as an own-author filter
// does.
//
// The assertion is that a PRIVATE post the viewer CAN read appears: it
// is outside the browse default under either version of that default,
// so it is present only if the default was dropped, and it is visible
// only if the read rule still ran. One row proves both halves.
//
// It was a followers-tier post before #1193 put `followers` INTO the
// browse default, at which point the row stopped discriminating: it
// would have been on the page whether or not the tab dropped the
// filter. The tier moved to the one the default still excludes.
func TestLikedBy_TierDefaultDoesNotTruncate(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	// The VIEWER's own private post, liked by somebody else. Readable by
	// this caller (they wrote it), outside the browse default (it is
	// private), and reached through ?liked_by= rather than through an
	// own-author filter — so the only thing that can put it on the page
	// is the Likes tab dropping the tier default.
	const title = "lb private, outside the browse default"
	postID := lbSeedPost(t, pool, lbFollower, "private", title)
	lbLike(t, pool, lbLiker, postID)

	if got := lbLikesOf(t, h, lbFollower, lbLiker)[postID]; got != title {
		t.Errorf("a readable private post is missing from the Likes tab (got %q). "+
			"The browse display default is still applied, so the tab is answering "+
			"\"what they liked, minus some tiers\" while looking complete", got)
	}
}
