// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1075 — THE TAG SOURCE RAN WITH NO CALLER AT ALL.
//
// suggest.tags() had no `caller` parameter, while all three of its
// siblings did, and its doc claimed the gate was structural:
//
//	"Not visibility-gated on the tag itself — tags are publicly
//	 meaningful even when the posts they appear on aren't; the JOIN
//	 restricts to posts the caller can see."
//
// The second half of that sentence was false. `JOIN posts p ON p.id =
// pt.post_id` requires the post to EXIST, not to be readable; there was
// no predicate on `p` anywhere in the statement. So the tag corpus was
// every tag on every post on the instance — private, draft, and every
// tier the caller cannot read — and /search/suggest takes a PREFIX, so
// it was walkable. Anonymous callers reach it on a public install.
//
// The assertions are COMPARATIVE, in the shape #902's suite established,
// because a change that hid tags from everybody would satisfy a
// leak-only test and ship a broken product:
//
//   - a tag that lives only on posts this caller cannot read is ABSENT;
//   - a tag on a post they CAN read is PRESENT, for the same caller in
//     the same call;
//   - and the SAME TAG STRING flips to present the moment it is also
//     applied to a readable post — which is the proof the gate keys on
//     the post's readability and not on anything about the string.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Synthetic refs. posts.author_user_ref carries no FK to the user table
// (federation-friendly by design), so these need no rows in "user".
const (
	stlAuthor    int64 = 10751101
	stlStranger  int64 = 10751102
	stlModerator int64 = 10751103
)

// stlPrefix is the completion prefix under attack. Nonsense on purpose,
// so a completion is attributable to this fixture and to nothing else in
// any developer's database — a leak test that can be satisfied by an
// unrelated row is not a leak test.
const stlPrefix = "vothquenlix"

// The two tags. Both share the prefix, so ONE query returns both if the
// gate is open and exactly one if it is closed.
const (
	stlEmbargoedTag = stlPrefix + "-emb"
	stlPublishedTag = stlPrefix + "-pub"
)

// stlSeedPost plants one post at `vis` carrying `tag`, and returns its id.
func stlSeedPost(t *testing.T, pool *pgxpool.Pool, author int64, vis, tag string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1, $2, $3, $4, $5)`,
		id, author, stlPrefix+" fixture "+vis, "fixture body", vis); err != nil {
		t.Fatalf("seed %s post: %v", vis, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO post_tags (post_id, tag) VALUES ($1, $2)`, id, tag); err != nil {
		t.Fatalf("seed %s tag: %v", vis, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_tags WHERE post_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

// stlTags runs the completion endpoint's service and returns the TAG
// suggestions only, as a set.
func stlTags(t *testing.T, pool *pgxpool.Pool, ref *int64, caps visibility.PostCaps) map[string]bool {
	t.Helper()
	resp, err := suggest.NewService(pool).Suggest(context.Background(), suggest.Request{
		Prefix:   stlPrefix,
		Caller:   visibility.NewCaller(ref),
		PostCaps: caps,
		Limit:    suggest.MaxResults,
	})
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	out := map[string]bool{}
	for _, s := range resp.Suggestions {
		if s.Kind == suggest.KindTag {
			out[s.Value] = true
		}
	}
	return out
}

// TestSuggestTags_PrivatePostTagNeverCompletes is the exploit and its
// counterweight in one table.
//
// The `want` column is stated per caller INDEPENDENTLY of the surface —
// the same two-part shape as the post-read-rule agreement suite — so
// "the gate is wrong for everybody in the same direction" cannot pass.
func TestSuggestTags_PrivatePostTagNeverCompletes(t *testing.T) {
	pool := coPool(t)
	stlSeedPost(t, pool, stlAuthor, "private", stlEmbargoedTag)
	stlSeedPost(t, pool, stlAuthor, "public", stlPublishedTag)

	author, stranger, moderator := stlAuthor, stlStranger, stlModerator
	for _, c := range []struct {
		name string
		ref  *int64
		caps visibility.PostCaps
		// May this caller read the PRIVATE post? The published tag is on
		// a public post, so it is readable by everyone in every row.
		wantEmbargoed bool
	}{
		// The unauthenticated case, which is the one that matters most:
		// /search/suggest is reachable anonymously on a public install,
		// so this leak needed no account at all.
		{"anonymous", nil, visibility.PostCaps{}, false},
		{"a signed-in stranger", &stranger, visibility.PostCaps{}, false},
		// The counterweights. An author who cannot complete their own
		// tags, or a moderator who cannot complete the tags of the posts
		// they moderate, is the "fix" that turns the feature off.
		{"the author", &author, visibility.PostCaps{}, true},
		{"posts.admin", &moderator, visibility.PostCaps{SeesAllPrivate: true}, true},
	} {
		got := stlTags(t, pool, c.ref, c.caps)

		if got[stlEmbargoedTag] != c.wantEmbargoed {
			if c.wantEmbargoed {
				t.Errorf("%s: lost the completion for a tag on a post they may READ — "+
					"the gate is too wide and the feature is off for the people it serves",
					c.name)
			} else {
				t.Errorf("%s: completed %q, which lives ONLY on a post they cannot read. "+
					"That is the tag vocabulary of private content, recovered by prefix "+
					"walk without ever opening a post (#1075)", c.name, stlEmbargoedTag)
			}
		}
		// The positive control, in the SAME call: if this fails the
		// assertion above proves nothing, because a query returning
		// nothing at all would satisfy it.
		if !got[stlPublishedTag] {
			t.Errorf("%s: the PUBLIC post's tag stopped completing (got %v) — a gate that "+
				"drops readable tags is not a fix, it is an outage", c.name, got)
		}
	}
}

// TestSuggestTags_SameTagStringOppositeVerdicts is the sharpest form of
// the assertion, and the one a mutation test cannot fake.
//
// Everything about the STRING is held constant — same tag, same caller,
// same prefix, same call — and only the readability of the post carrying
// it changes. If the completion appears, it appeared because a readable
// post carries the tag; if it does not, no readable post does. That is
// precisely the property the old code claimed and did not have.
//
// It also pins the direction the fix must NOT overshoot into: a tag is
// not poisoned by appearing on a private post. Once it is also on a
// public one it is public vocabulary, and withholding it there would
// make the tag rail depend on content the caller cannot see — an
// undercount, which is the #873 failure that looks like nothing at all.
func TestSuggestTags_SameTagStringOppositeVerdicts(t *testing.T) {
	pool := coPool(t)
	const shared = stlPrefix + "-shared"

	// Act one: the tag exists only on a post this caller cannot read.
	stlSeedPost(t, pool, stlAuthor, "private", shared)
	stranger := stlStranger
	if stlTags(t, pool, &stranger, visibility.PostCaps{})[shared] {
		t.Fatalf("%q completed while it existed ONLY on a private post — the leak is live, "+
			"and the second half of this test cannot mean anything until it is closed", shared)
	}

	// Act two: the SAME string, now also applied to a public post.
	stlSeedPost(t, pool, stlAuthor, "public", shared)
	if !stlTags(t, pool, &stranger, visibility.PostCaps{})[shared] {
		t.Errorf("%q did not complete even though a PUBLIC post carries it — the gate is "+
			"keying on something other than the readability of the posts the tag is on, "+
			"and the tag rail now undercounts public vocabulary", shared)
	}
}
