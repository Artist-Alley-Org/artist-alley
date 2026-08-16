// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1155 — A SUGGESTION MUST BE BACKED BY THE MATCH RULE THE EXECUTED
// SEARCH APPLIES FOR THAT VIEWER.
//
// The owner's report was that search suggested a term which yielded ZERO
// matches when executed. Reproduced on the dev seed before anything was
// changed: across six typed prefixes, 9 of 25 completions returned zero
// rows on browse and every one of the nine was an ASSET TITLE, while all
// 25 returned rows on /search.
//
// # The mechanism
//
// The four suggest sources draw from `post_tags.tag`, `collections.name`,
// `posts.title` and `assets.title`. The nav box's commit lands on one of
// two surfaces, and they do not search the same corpus: `/search` runs
// the Engine over assets, posts and collections, while browse runs
// `GET /posts`, which matches `posts.search_text` and nothing else.
//
// Those corpora are disjoint in a place that is deliberate. Migration
// 00034 (#883) keeps a member asset's words OUT of its containing post's
// document unless the member is `public/active/ready`. So a `team` asset
// inside a perfectly readable post is completable — its own row passes
// the field plane — and its words are in no post document anywhere.
//
// This is a CORPUS mismatch, exactly the class #1155 names as #1077's
// sibling. It is not closable by moving a gate in either direction: the
// asset IS readable, and the post IS readable, and browse still cannot
// match the word.
//
// # What the fix asserts, and what it must NOT assert
//
// The assertions are COMPARATIVE, in the shape #902 and #1075
// established, because "stop suggesting asset titles on browse" would
// satisfy a zero-result test and ship a worse product. So:
//
//   - the unfindable title is ABSENT under ScopeBrowse;
//   - the SAME title is PRESENT under ScopeSearch, whose surface can
//     answer it — the proof this is a corpus gate and not a removal;
//   - a findable sibling asset in the SAME post is PRESENT under BOTH,
//     so the browse conjunct removes only what browse cannot answer;
//   - and the property #1155's acceptance names: every suggestion
//     returned for a viewer executes to at least one row for that same
//     viewer on that same surface.
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

// Synthetic refs. Neither posts.author_user_ref nor assets.owner_user_ref
// carries an FK to the user table, so these need no rows in "user".
const (
	sxAuthor   int64 = 11551101
	sxStranger int64 = 11551102
)

// sxPrefix is the typed prefix. Nonsense on purpose so every completion
// in these tests is attributable to this fixture and to nothing else in
// any developer's database — a corpus test satisfiable by an unrelated
// row is not a corpus test.
const sxPrefix = "quorbelnax"

const (
	// sxHiddenTitle belongs to a `team` asset. Readable here (the asset
	// row passes the field plane for its owner), and by #883 its words
	// are excluded from its post's document. This is the owner's case.
	sxHiddenTitle = sxPrefix + " teamside"
	// sxFoundTitle belongs to a `public/active/ready` asset in the SAME
	// post, so its words ARE folded into that post's document. The
	// positive control.
	sxFoundTitle = sxPrefix + " publicside"
	// sxPostTitle exercises the post-title source, and sxTag the tag
	// source. Both are indexed into the post's own document (weights A
	// and C), so both are answerable on browse and must SURVIVE the
	// conjunct — three of the four sources are positive controls here.
	sxPostTitle = sxPrefix + " postside"
	sxTag       = sxPrefix + "-tagside"
	// sxCollName exercises the collection source. Its words appear in no
	// post document, so browse cannot answer it and it must be withheld
	// there — the cross-entity half of the same rule.
	sxCollName = sxPrefix + " collside"
)

// sxSeed plants one public post carrying two member assets — one `team`,
// one `public` — and returns nothing but its cleanup. Both assets share
// the prefix, so ONE suggest call returns both if the corpus is
// unchecked and exactly one if it is checked.
func sxSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	postID := uuid.New()
	teamAsset, publicAsset := uuid.New(), uuid.New()

	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1, $2, $3, 'fixture body', 'public')`,
		postID, sxAuthor, sxPostTitle); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO post_tags (post_id, tag) VALUES ($1, $2)`, postID, sxTag); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	collID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, name, description, owner_user_ref, visibility)
		VALUES ($1, $2, 'fixture collection', $3, 'public')`,
		collID, sxCollName, sxAuthor); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	for _, a := range []struct {
		id          uuid.UUID
		title       string
		sensitivity string
	}{
		{teamAsset, sxHiddenTitle, "team"},
		{publicAsset, sxFoundTitle, "public"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready')`,
			a.id, a.title, sxAuthor, a.sensitivity); err != nil {
			t.Fatalf("seed asset %s: %v", a.title, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_assets (post_id, asset_id) VALUES ($1,$2)`,
			postID, a.id); err != nil {
			t.Fatalf("seed membership %s: %v", a.title, err)
		}
	}
	// The membership insert fires rebuild_post_search_text, so the post's
	// document is already correct here — assert it rather than assume it,
	// because the whole fixture rests on #883 having excluded one member
	// and folded in the other. If this ever stops holding, the test below
	// would pass for the wrong reason.
	var hasPublic, hasTeam bool
	if err := pool.QueryRow(ctx, `
		SELECT search_text @@ plainto_tsquery('english', $2),
		       search_text @@ plainto_tsquery('english', $3)
		  FROM posts WHERE id = $1`,
		postID, sxFoundTitle, sxHiddenTitle).Scan(&hasPublic, &hasTeam); err != nil {
		t.Fatalf("read post document: %v", err)
	}
	if !hasPublic {
		t.Fatalf("fixture invalid: the public member's words are not in the post document")
	}
	if hasTeam {
		t.Fatalf("fixture invalid: #883 should have excluded the team member's words")
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM post_tags WHERE post_id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, collID)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = ANY($1::uuid[])`,
			[]uuid.UUID{teamAsset, publicAsset})
	})
}

// sxSuggest runs the endpoint's service for one prefix, caller and scope.
func sxSuggest(t *testing.T, pool *pgxpool.Pool, prefix string, ref *int64, scope suggest.Scope) []suggest.Suggestion {
	t.Helper()
	resp, err := suggest.NewService(pool).Suggest(context.Background(), suggest.Request{
		Prefix: prefix,
		Caller: visibility.NewCaller(ref),
		Scope:  scope,
		Limit:  suggest.MaxResults,
	})
	if err != nil {
		t.Fatalf("suggest(%q, %s): %v", prefix, scope, err)
	}
	return resp.Suggestions
}

func sxValues(sugs []suggest.Suggestion) map[string]bool {
	out := map[string]bool{}
	for _, s := range sugs {
		out[s.Value] = true
	}
	return out
}

// TestSuggest_BrowseScopeOffersOnlyWhatBrowseCanMatch is the owner's
// reproduction and its counterweight in one table.
func TestSuggest_BrowseScopeOffersOnlyWhatBrowseCanMatch(t *testing.T) {
	pool := coPool(t)
	sxSeed(t, pool)

	author := sxAuthor
	for _, c := range []struct {
		name  string
		scope suggest.Scope
		// The team member's title: browse cannot match it, /search can.
		wantHidden bool
	}{
		{"browse — the surface that cannot answer it", suggest.ScopeBrowse, false},
		{"search — the surface that can", suggest.ScopeSearch, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := sxValues(sxSuggest(t, pool, sxPrefix, &author, c.scope))
			if got[sxHiddenTitle] != c.wantHidden {
				t.Errorf("completion of the team member's title = %v, want %v\n"+
					"  scope %q: a suggestion must be backed by the match rule the\n"+
					"  executed search applies on THAT surface (#1155).",
					got[sxHiddenTitle], c.wantHidden, c.scope)
			}
			// The counterweight, asserted on BOTH scopes. A fix that
			// stopped completing asset titles on browse would pass the
			// row above and fail here.
			if !got[sxFoundTitle] {
				t.Errorf("the findable sibling's title was NOT completed under scope %q.\n"+
					"  The conjunct must remove only what the surface cannot answer;\n"+
					"  dropping the source wholesale is #1077's option 2.", c.scope)
			}
		})
	}
}

// TestSuggest_CrossEntitySourcesRespectTheBrowseCorpus covers the other
// half of the corpus rule: a COLLECTION name reaches browse only through
// a post document, and this fixture's collection appears in none, so it
// must be withheld on browse and offered on /search.
//
// The tag and post-title sources are asserted in the same table as
// positive controls. Both are indexed into the post's own document, so
// both stay on browse — which is what makes this a corpus check rather
// than "the browse scope suggests less".
func TestSuggest_CrossEntitySourcesRespectTheBrowseCorpus(t *testing.T) {
	pool := coPool(t)
	sxSeed(t, pool)

	author := sxAuthor
	for _, c := range []struct {
		name  string
		scope suggest.Scope
		// The collection name: no post carries these words.
		wantCollection bool
	}{
		{"browse — no collection query runs here", suggest.ScopeBrowse, false},
		{"search — the Engine queries collections", suggest.ScopeSearch, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := sxValues(sxSuggest(t, pool, sxPrefix, &author, c.scope))
			if got[sxCollName] != c.wantCollection {
				t.Errorf("completion of the collection name = %v, want %v under scope %q",
					got[sxCollName], c.wantCollection, c.scope)
			}
			for _, want := range []struct {
				value, source string
			}{
				{sxTag, "tag"},
				{sxPostTitle, "post title"},
			} {
				if !got[want.value] {
					t.Errorf("the %s source lost %q under scope %q.\n"+
						"  Its words ARE in the post document, so both surfaces can\n"+
						"  answer it and the conjunct must keep it.", want.source, want.value, c.scope)
				}
			}
		})
	}
}

// TestSuggest_EveryBrowseSuggestionExecutesToAResult is #1155's stated
// acceptance, as a property: for the suggested terms across all four
// sources, executing each returns at least one viewer-visible result.
//
// It runs over this file's OWN fixture rather than the dev seed, so it
// asserts the same thing on a fresh CI database as it does locally. A
// property test that skips wherever the corpus is empty is the unreal
// fixture #1049's lesson names — green on unreachable state.
//
// It executes each completion the way browse does — `posts.search_text`
// under the post read rule for this viewer, the three clauses
// [Engine.runPosts] composes — and fails naming the term, so a
// regression points at the source that produced it rather than a count.
func TestSuggest_EveryBrowseSuggestionExecutesToAResult(t *testing.T) {
	pool := coPool(t)
	sxSeed(t, pool)
	ctx := context.Background()

	// Anonymous and authenticated both: the executed rule differs per
	// viewer, and a property that holds for only one is not the property.
	author, stranger := sxAuthor, sxStranger
	for _, c := range []struct {
		name string
		ref  *int64
	}{
		{"anonymous", nil},
		{"a signed-in stranger", &stranger},
		{"the author", &author},
	} {
		t.Run(c.name, func(t *testing.T) {
			caller := visibility.NewCaller(c.ref)
			pred, err := visibility.Filter(ctx, visibility.EntityPost, caller)
			if err != nil {
				t.Fatalf("post predicate: %v", err)
			}
			frag, args := pred.ToSQL("p", 1)

			sugs := sxSuggest(t, pool, sxPrefix, c.ref, suggest.ScopeBrowse)
			if len(sugs) == 0 {
				t.Fatalf("the fixture produced NO completions for %q — the property\n"+
					"  would pass vacuously. Seeding is broken, not the gate.", sxPrefix)
			}
			for _, s := range sugs {
				var n int
				qArgs := append([]any{s.Value}, args...)
				if err := pool.QueryRow(ctx, `
					SELECT COUNT(*) FROM posts p
					 WHERE p.search_text @@ plainto_tsquery('english', $1)`+frag,
					qArgs...).Scan(&n); err != nil {
					t.Fatalf("execute %q: %v", s.Value, err)
				}
				if n == 0 {
					t.Errorf("suggestion %q (kind %s) executes to ZERO posts for this viewer.\n"+
						"  A suggestion is a promise; completing a term the feed cannot\n"+
						"  match is the #1155 defect.", s.Value, s.Kind)
				}
			}
		})
	}
}
