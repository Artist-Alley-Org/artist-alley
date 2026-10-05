// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1251 slice 3 — `GET /posts?ai=` is the browse footer's
// "Hide AI-made work" toggle (ADR 0094 fourth amendment).
//
// # The assertion that actually proves the feature is about MIXED posts
//
// "Does `ai=not_pure` remove the AI post" is one line and it is not
// where this goes wrong. The owner's ruling is a distinction, not a
// filter:
//
//	> If someone filters out AI content it should still show a post that
//	> has mixed AI/non-AI content — only exclude posts with pure AI. AI
//	> could be used as part of an ideation phase and the final project
//	> might be pure human made.
//
// So the fixture below plants FIVE posts that a wrong implementation
// cannot tell apart, and the case that separates them is the mixed one:
//
//	pure       every contributor `generated`     → HIDDEN by not_pure
//	mixed      one `generated`, one undeclared   → SHOWN
//	mixedNone  one `generated`, one `none`       → SHOWN
//	assisted   every contributor `assisted`      → SHOWN
//	undeclared nobody was asked                  → SHOWN
//
// A filter keyed on `posts.ai_provenance` — the LABELLING column, the
// tempting one, and the one that already existed when this shipped —
// passes "the pure post is hidden" and fails three of the four others,
// because that column propagates a positive claim on ANY member and
// reads `generated` for `mixed`, `mixedNone` and (as `assisted`) for
// `assisted` too. Only `posts.ai_pure` (migration 00061) separates them.
//
// `assisted` is in here because it is the case the rule's own migration
// note says people get wrong: purity is NOT "the strongest declaration
// is generated" and NOT "no contributor says none". An all-`assisted`
// post is human work made with AI help and must survive.
//
// # The fails-toward-SHOWING direction, asserted rather than assumed
//
// `undeclared` is the other half. An UNDECLARED contributor makes a post
// not-pure, so a work nobody was asked about SURVIVES the hide toggle.
// The obvious SQL spelling of the asset arm (`ai_provenance <>
// 'generated'`) evaluates to NULL for such a contributor and a NULL
// conjunct drops the row, so every undeclared work would vanish from a
// feed that asked to see non-AI work — the exact error ADR 0094 §3
// forbids, arriving through SQL's NULL semantics. The corpus is
// overwhelmingly undeclared, so this is not an edge case; it is almost
// the whole feed.
//
// # ⛔ A FILTER, NEVER A GATE (ADR 0094 §4)
//
// TestAIFilter_IsAFilterNeverAGate is the one that keeps this column
// cheap. Nothing is withheld on this axis: a caller who does not send
// `?ai=` gets the pure post exactly as before, and the moment something
// starts subtracting on it, every derived copy inherits the
// derived-copies obligation (#1066's list).
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
)

// Synthetic refs, distinct from every other file's so parallel runs
// cannot see each other's fixtures. posts.author_user_ref has no FK.
const (
	aiAuthor int64 = 12510001
	aiOther  int64 = 12510002
	aiAnon   int64 = 0
)

// aiAsset plants one asset carrying a declaration. `decl` is the empty
// string for UNDECLARED, which is written as SQL NULL — not as `none`.
// The distinction is the whole axis: NULL says nobody was asked, `none`
// says the maker declared no AI, and writing the second where the first
// is true fabricates that maker's disclaimer.
//
// The extension is real so these fixtures also carry a resolvable kind,
// which the cross-dimension case below needs.
func aiAsset(t *testing.T, pool *pgxpool.Pool, ext string, assetType int64, decl string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var declArg any
	if decl != "" {
		declArg = decl
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                     processing_status, file_extension, ai_provenance)
		 VALUES ($1,$2,$3,$4,'active','public','ready',$5,$6)`,
		id, "ai-"+ext+"-"+decl, aiAuthor, assetType, ext, declArg); err != nil {
		t.Fatalf("seed asset (%s/%q): %v", ext, decl, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

// aiPost plants a post with the given members and returns its id.
//
// ⚠️ THE MEMBERSHIP IS WRITTEN AFTER THE ASSETS ARE DECLARED, and the
// order is load-bearing rather than incidental. Both derived facts are
// maintained by triggers (00060/00061) that recompute from the post's
// live CONTRIBUTORS, so the insert into `post_assets` is what fires the
// recompute with a population that already carries its declarations.
// The reverse order also converges — there is a trigger on `assets` too
// — but the seed never takes that path and neither should the fixture
// that is supposed to look like it.
//
// The cover is set to the first member, which is what the real create
// path and the seeder both do, and it matters here: contributors are the
// members UNION the two cover pictures, so a post whose cover is not a
// member would have a contributor this helper never named.
func aiPost(t *testing.T, pool *pgxpool.Pool, author int64, visibility string, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	if len(members) == 0 {
		t.Fatal("aiPost: a post with no contributors is never pure; name at least one")
	}
	id := uuid.New()
	at := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility, posted_at, cover_asset_id)
		 VALUES ($1,$2,'ai post','',$3,$4,$5)`,
		id, author, visibility, at, members[0]); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	for i, m := range members {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`, id, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

// aiDerived reads back what the triggers actually wrote. Called once by
// the fixture builder, because every assertion below is downstream of
// these two columns and a fixture that failed to derive would otherwise
// present as a filter bug.
func aiDerived(t *testing.T, pool *pgxpool.Pool, post uuid.UUID) (provenance *string, pure bool) {
	t.Helper()
	if err := pool.QueryRow(t.Context(),
		`SELECT ai_provenance, ai_pure FROM posts WHERE id=$1`, post).Scan(&provenance, &pure); err != nil {
		t.Fatalf("read derived AI facts: %v", err)
	}
	return provenance, pure
}

// aiFeed runs one feed page for a caller (ref 0 = anonymous) with an
// optional `?ai=` and `?kind=`, and returns the ids as a set.
//
// The tier is named explicitly for the same reason kfFeed names it: it
// keeps this a test of the AI conjunct rather than of whatever the
// display default happens to be this month.
func aiFeed(t *testing.T, h *Handler, callerRef int64, ai, kind string) map[uuid.UUID]bool {
	t.Helper()
	resp := aiFeedRaw(t, h, callerRef, ai, kind, nil)
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts(caller=%d ai=%q kind=%q) returned %T, want 200", callerRef, ai, kind, resp)
	}
	out := make(map[uuid.UUID]bool, len(ok.Items))
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = true
	}
	return out
}

// aiFeedAuthored is aiFeed scoped to ONE author, which is what makes an
// arithmetic assertion over "the whole feed" meaningful in a shared test
// database.
//
// ⚠️ WITHOUT THE SCOPE THE SUBTRACTION IS A FLAKE. The feed is capped at
// `limit`, and the tier asked for is `public`, so the unfiltered page is
// every public post the RUN has planted — this file's five plus whatever
// the other packages left behind. Once that exceeds the cap, the
// unfiltered page truncates while the narrower `ai=` pages do not, and
// "not_pure returned a post the unfiltered feed did not" fires on a
// filter that is working perfectly. Scoping removes the cap from the
// argument instead of raising it and hoping.
//
// `author_ref` is a SCOPE and not a filter (ADR 0093 decision 2), which
// is exactly why it is safe here: it selects the corpus this file owns
// and says nothing about the dimension under test.
func aiFeedAuthored(t *testing.T, h *Handler, callerRef, author int64, ai string) map[uuid.UUID]bool {
	t.Helper()
	resp := aiFeedRaw(t, h, callerRef, ai, "", &author)
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts(author=%d ai=%q) returned %T, want 200", author, ai, resp)
	}
	out := make(map[uuid.UUID]bool, len(ok.Items))
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = true
	}
	return out
}

// aiFeedRaw is aiFeed without the 200 assertion — the fail-closed case
// needs to inspect a non-200.
func aiFeedRaw(
	t *testing.T, h *Handler, callerRef int64, ai, kind string, author *int64,
) openapi.ListPostsResponseObject {
	t.Helper()
	ctx := context.Background()
	if callerRef != aiAnon {
		ctx = auth.WithIdentity(ctx, &auth.Identity{UserRef: callerRef, AuthMethod: "session"})
	}
	limit := 200
	params := openapi.ListPostsParams{Limit: &limit}
	vis := openapi.ListPostsParamsVisibility("public")
	params.Visibility = &vis
	params.AuthorRef = author
	if ai != "" {
		v := openapi.ListPostsParamsAi(ai)
		params.Ai = &v
	}
	if kind != "" {
		params.Kind = &kind
	}
	resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: params})
	if err != nil {
		t.Fatalf("ListPosts(caller=%d ai=%q kind=%q): %v", callerRef, ai, kind, err)
	}
	return resp
}

func aiAssertPresent(t *testing.T, what string, got map[uuid.UUID]bool, id uuid.UUID) {
	t.Helper()
	if !got[id] {
		t.Errorf("%s: post %v is MISSING", what, id)
	}
}

func aiAssertAbsent(t *testing.T, what string, got map[uuid.UUID]bool, id uuid.UUID) {
	t.Helper()
	if got[id] {
		t.Errorf("%s: post %v is PRESENT and must not be", what, id)
	}
}

// aiCorpus is the five-post fixture, plus the assertion that the
// TRIGGERS derived what the cases below assume.
//
// ⚠️ THE DERIVATION CHECK IS NOT CEREMONY. Every assertion in this file
// is a statement about `posts.ai_pure`, and a fixture whose triggers did
// not fire — a migration not applied, a membership written in the wrong
// order — would leave all five posts at the column's `false` default,
// where "the pure post is hidden" is FALSE and every other case still
// PASSES. That failure reads as a filter bug and is not one, so the
// fixture proves its own premise before the filter is asked anything.
type aiCorpus struct {
	pure       uuid.UUID
	mixed      uuid.UUID
	mixedNone  uuid.UUID
	assisted   uuid.UUID
	undeclared uuid.UUID
}

func (c aiCorpus) notPure() []uuid.UUID {
	return []uuid.UUID{c.mixed, c.mixedNone, c.assisted, c.undeclared}
}

func (c aiCorpus) all() []uuid.UUID {
	return append([]uuid.UUID{c.pure}, c.notPure()...)
}

func newAICorpus(t *testing.T, pool *pgxpool.Pool) aiCorpus {
	t.Helper()

	gen1 := aiAsset(t, pool, "png", 1, "generated")
	gen2 := aiAsset(t, pool, "png", 1, "generated")
	gen3 := aiAsset(t, pool, "png", 1, "generated")
	gen4 := aiAsset(t, pool, "mp4", 3, "generated")
	none1 := aiAsset(t, pool, "png", 1, "none")
	asst1 := aiAsset(t, pool, "png", 1, "assisted")
	asst2 := aiAsset(t, pool, "png", 1, "assisted")
	undec1 := aiAsset(t, pool, "png", 1, "")
	undec2 := aiAsset(t, pool, "png", 1, "")

	c := aiCorpus{
		// Two contributors, BOTH generated. The only pure one.
		pure: aiPost(t, pool, aiAuthor, "public", gen1, gen4),
		// The owner's case: one declared, one nobody asked about.
		mixed: aiPost(t, pool, aiAuthor, "public", gen2, undec1),
		// The sharper one: one declared `generated`, one declared
		// `none`. `ai_provenance` reads `generated` here too, so a
		// filter on the labelling column hides it.
		mixedNone: aiPost(t, pool, aiAuthor, "public", gen3, none1),
		// Human work made with AI help. `assisted` never contributes to
		// purity — this is NOT "the strongest declaration wins".
		assisted: aiPost(t, pool, aiAuthor, "public", asst1, asst2),
		// The shape of almost the entire real corpus.
		undeclared: aiPost(t, pool, aiAuthor, "public", undec2),
	}

	for name, want := range map[string]struct {
		id   uuid.UUID
		pure bool
		prov string
	}{
		"pure":       {c.pure, true, "generated"},
		"mixed":      {c.mixed, false, "generated"},
		"mixedNone":  {c.mixedNone, false, "generated"},
		"assisted":   {c.assisted, false, "assisted"},
		"undeclared": {c.undeclared, false, ""},
	} {
		prov, pure := aiDerived(t, pool, want.id)
		gotProv := ""
		if prov != nil {
			gotProv = *prov
		}
		if pure != want.pure || gotProv != want.prov {
			t.Fatalf("fixture %s derived ai_pure=%v ai_provenance=%q, want %v / %q — "+
				"the 00060/00061 triggers did not produce the state these cases assume, "+
				"so a filter result here would prove nothing",
				name, pure, gotProv, want.pure, want.prov)
		}
	}

	// ⭐ The premise the whole feature rests on, stated once: the two
	// derived columns DISAGREE on three of the five posts. If they ever
	// agree everywhere, `ai_pure` has become a synonym for
	// `ai_provenance = 'generated'` and the owner's ruling has been
	// silently reverted — every case below would still pass.
	labelled := 0
	for _, id := range c.notPure() {
		if prov, _ := aiDerived(t, pool, id); prov != nil && *prov == "generated" {
			labelled++
		}
	}
	if labelled < 2 {
		t.Fatalf("only %d of the not-pure posts are LABELLED `generated`; the fixture no "+
			"longer distinguishes the labelling column from the purity column, so it "+
			"cannot detect a filter keyed on the wrong one", labelled)
	}

	return c
}

// ---------------------------------------------------------------------------
// ⭐ The owner's ruling
// ---------------------------------------------------------------------------

// TestAIFilter_HidesPureOnlyAndKeepsMixed is the case the feature exists
// for. Every not-pure post survives `ai=not_pure`, including the three a
// filter on the labelling column would have removed.
func TestAIFilter_HidesPureOnlyAndKeepsMixed(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	c := newAICorpus(t, pool)

	hidden := aiFeed(t, h, aiAuthor, facet.AINotPure, "")
	aiAssertAbsent(t, "ai=not_pure (the PURE post)", hidden, c.pure)
	aiAssertPresent(t, "⭐ ai=not_pure (MIXED: one generated, one undeclared)", hidden, c.mixed)
	aiAssertPresent(t, "⭐ ai=not_pure (MIXED: one generated, one `none`)", hidden, c.mixedNone)
	aiAssertPresent(t, "⭐ ai=not_pure (ALL-ASSISTED is human work made with AI help)", hidden, c.assisted)
	aiAssertPresent(t, "⭐ ai=not_pure (UNDECLARED — not-knowing must never hide work)", hidden, c.undeclared)
}

// TestAIFilter_PureSelectsOnlyThePureOne is the other half of the
// partition, and it is the direction that catches an inverted predicate:
// a `=` accidentally written as `<>` passes the case above by hiding
// everything except the pure post and fails here.
func TestAIFilter_PureSelectsOnlyThePureOne(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	c := newAICorpus(t, pool)

	only := aiFeed(t, h, aiAuthor, facet.AIPure, "")
	aiAssertPresent(t, "ai=pure", only, c.pure)
	for _, id := range c.notPure() {
		aiAssertAbsent(t, "ai=pure", only, id)
	}
}

// ---------------------------------------------------------------------------
// ⭐ The rule of two — arithmetic, not membership
// ---------------------------------------------------------------------------

// TestAIFilter_ComposesWithKind asserts the two dimensions AND rather
// than compete, with COUNTS rather than with a membership spot-check.
//
// `both <= min(a, b)` is the property an OR cannot satisfy and a
// membership assertion cannot detect: a selection that ORed its
// dimensions still contains every post either one returns, so "the video
// post is in the result" passes on the bug. #1165 and #1242 each found a
// shipped filter that ORed where it should have ANDed, both times behind
// a green membership test.
func TestAIFilter_ComposesWithKind(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	c := newAICorpus(t, pool)

	// Restricted to the fixture, so counts are about these five posts
	// and not about whatever else the database holds.
	count := func(got map[uuid.UUID]bool) int {
		n := 0
		for _, id := range c.all() {
			if got[id] {
				n++
			}
		}
		return n
	}

	notPure := aiFeed(t, h, aiAuthor, facet.AINotPure, "")
	video := aiFeed(t, h, aiAuthor, "", "video")
	both := aiFeed(t, h, aiAuthor, facet.AINotPure, "video")

	a, b, ab := count(notPure), count(video), count(both)
	if ab > a || ab > b {
		t.Errorf("ai=not_pure AND kind=video returned %d, which exceeds min(not_pure=%d, video=%d) — "+
			"the dimensions are not ANDing", ab, a, b)
	}
	// The fixture makes the intersection EMPTY on purpose: the only
	// post holding an .mp4 is the pure one, so "hide AI work, show me
	// videos" has nothing to return. That is the strongest form of the
	// AND — an OR would return five.
	if ab != 0 {
		t.Errorf("ai=not_pure + kind=video returned %d fixture posts, want 0: "+
			"the only video member belongs to the pure post", ab)
	}
	if b != 1 {
		t.Errorf("kind=video alone returned %d fixture posts, want 1 (the pure post); "+
			"the AND case above is only meaningful while this is 1", b)
	}
}

// TestAIFilter_NotPureIsTheFeedMinusThePureOnes writes the subtraction
// out, over the WHOLE readable feed rather than over the fixture — the
// arithmetic is what makes it an assertion about the filter instead of
// about these five rows.
func TestAIFilter_NotPureIsTheFeedMinusThePureOnes(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	c := newAICorpus(t, pool)

	all := aiFeedAuthored(t, h, aiAuthor, aiAuthor, "")
	notPure := aiFeedAuthored(t, h, aiAuthor, aiAuthor, facet.AINotPure)
	pure := aiFeedAuthored(t, h, aiAuthor, aiAuthor, facet.AIPure)

	// THE SUBTRACTION, written out, over a feed scoped to this file's
	// author so the three page sizes are about these five posts and
	// nothing else: 5 unfiltered = 1 pure + 4 not-pure, with
	// not_pure exactly the unfiltered set minus the pure one.
	if len(all) != 5 || len(pure) != 1 || len(notPure) != 4 || len(pure)+len(notPure) != len(all) {
		t.Fatalf("subtraction failed: unfiltered=%d, pure=%d, not_pure=%d (want 5 = 1 + 4)",
			len(all), len(pure), len(notPure))
	}
	aiAssertPresent(t, "the pure post is the one `ai=pure` returns", pure, c.pure)
	for _, id := range c.notPure() {
		aiAssertPresent(t, "ai=not_pure (author-scoped)", notPure, id)
		aiAssertAbsent(t, "ai=pure (author-scoped)", pure, id)
	}

	// The two values PARTITION: every readable post on the page is in
	// exactly one of them. Per id rather than by count, so it holds
	// whatever else is in the database and however the page is capped.
	for id := range all {
		inNot, inPure := notPure[id], pure[id]
		if inNot == inPure {
			t.Errorf("post %v is in %s and %s alike (not_pure=%v pure=%v) — the two values "+
				"must partition the corpus", id, facet.AINotPure, facet.AIPure, inNot, inPure)
		}
	}
	for id := range notPure {
		if !all[id] {
			t.Errorf("ai=not_pure returned %v, which the UNFILTERED feed does not — a filter "+
				"that WIDENS is the one direction this may never move", id)
		}
	}
	for id := range pure {
		if !all[id] {
			t.Errorf("ai=pure returned %v, which the unfiltered feed does not", id)
		}
	}
}

// ---------------------------------------------------------------------------
// ⭐ It composes with the read rule, in both directions
// ---------------------------------------------------------------------------

// TestAIFilter_ComposesWithReadRule plants another author's PRIVATE pure
// post. `ai=pure` is the interesting direction: it is the one value that
// SELECTS the rare row, so a conjunct that had drifted into a disjunct —
// or that ran before the rule — would hand over exactly the post nobody
// may read, and would look like the feature working.
func TestAIFilter_ComposesWithReadRule(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	gen := aiAsset(t, pool, "png", 1, "generated")
	undec := aiAsset(t, pool, "png", 1, "")

	minePure := aiPost(t, pool, aiOther, "public", gen)
	theirsPure := aiPost(t, pool, aiAuthor, "private", gen)
	theirsMixed := aiPost(t, pool, aiAuthor, "private", gen, undec)

	// aiOther reads their own public post and none of aiAuthor's private ones.
	pure := aiFeed(t, h, aiOther, facet.AIPure, "")
	aiAssertPresent(t, "ai=pure (own public post)", pure, minePure)
	aiAssertAbsent(t, "⭐ ai=pure (another author's PRIVATE pure post)", pure, theirsPure)

	notPure := aiFeed(t, h, aiOther, facet.AINotPure, "")
	aiAssertAbsent(t, "ai=not_pure (another author's private post)", notPure, theirsMixed)

	// And unfiltered, so the absences above are the rule's doing rather
	// than the fixture never having been readable.
	all := aiFeed(t, h, aiOther, "", "")
	aiAssertPresent(t, "unfiltered (own public post)", all, minePure)
	aiAssertAbsent(t, "unfiltered (another author's private post)", all, theirsPure)
}

// TestAIFilter_AnonymousGetsTheFilterAndNothingMore drives the same
// distinction as an anonymous caller under public mode, because a filter
// that consulted an identity it does not have is a filter that fails
// open exactly where nobody is watching.
func TestAIFilter_AnonymousGetsTheFilterAndNothingMore(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	c := newAICorpus(t, pool)

	// A private pure post the anonymous caller must never see, filter or
	// no filter.
	gen := aiAsset(t, pool, "png", 1, "generated")
	secret := aiPost(t, pool, aiAuthor, "private", gen)

	hidden := aiFeed(t, h, aiAnon, facet.AINotPure, "")
	aiAssertAbsent(t, "anon ai=not_pure (the pure post)", hidden, c.pure)
	aiAssertPresent(t, "anon ai=not_pure (the mixed post is public)", hidden, c.mixed)
	aiAssertAbsent(t, "anon ai=not_pure (a private post)", hidden, secret)

	only := aiFeed(t, h, aiAnon, facet.AIPure, "")
	aiAssertPresent(t, "anon ai=pure (the public pure post)", only, c.pure)
	aiAssertAbsent(t, "⭐ anon ai=pure (a PRIVATE pure post)", only, secret)
}

// ---------------------------------------------------------------------------
// ⛔ A filter, never a gate — ADR 0094 §4
// ---------------------------------------------------------------------------

// TestAIFilter_IsAFilterNeverAGate asserts the property that keeps this
// column free of the derived-copies obligation: a caller who does not
// ASK for the dimension has nothing subtracted.
//
// The pure post is present, in full, for every caller who may read it —
// signed in and anonymous — with no parameter and with an empty one. If
// this ever fails, the column has become a gate and #1066's whole list
// (search text, facets, suggest, thumbhash, embeddings, counts, covers)
// inherits the obligation.
func TestAIFilter_IsAFilterNeverAGate(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	c := newAICorpus(t, pool)

	for _, caller := range []int64{aiAuthor, aiOther, aiAnon} {
		all := aiFeed(t, h, caller, "", "")
		for _, id := range c.all() {
			aiAssertPresent(t, "unfiltered feed (nothing is withheld on this axis)", all, id)
		}
	}

	// An EMPTY `?ai=` is "no filter", not "a filter naming nothing".
	// Unlike `?kind=` and `?visibility=`, this parameter has no
	// requested-but-empty state to degrade to an empty page — the
	// handler validates the value and rejects anything outside the
	// vocabulary — so the empty string must behave exactly like absence.
	empty := aiFeedRaw(t, h, aiAuthor, "", "", nil)
	if _, is := empty.(openapi.ListPosts200JSONResponse); !is {
		t.Fatalf("an absent ?ai= returned %T, want 200", empty)
	}
}

// ---------------------------------------------------------------------------
// ⭐ Fail closed, and SAY SO
// ---------------------------------------------------------------------------

// TestAIFilter_UnknownValueIs400 is the one place this parameter answers
// a typo differently from its neighbours, and the divergence is the
// point rather than an accident to be tidied away.
//
// `?kind=nonsense` and `?visibility=nonsense` both return an EMPTY PAGE
// (viewkind.ParseList drops the name; a bad tier fails the value gate
// inside facet.Selection.SQL). Both are POSITIVE selections, where "only
// X" for an X nobody has is legibly answered by no rows.
//
// `ai` is an EXCLUSION over a closed two-value vocabulary. A tolerated
// `?ai=generated` would render a predicate matching nothing and hand a
// viewer who asked to hide AI work an EMPTY WALL — indistinguishable
// from the site being broken, and silent. So it is a 400, which is what
// /search already answers `filter=ai:junk`, from the same validator.
//
// All three fail CLOSED. None of them can widen. That is the invariant;
// the status code is how loudly each one says it.
func TestAIFilter_UnknownValueIs400(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	c := newAICorpus(t, pool)

	// ⚠️ NOT "PURE " — that CANONICALISES to the valid `pure`, because
	// the validator trims and lowercases. Putting it here would have
	// asserted a 400 on a legal request; the good list below is where
	// that spelling belongs.
	for _, bad := range []string{"junk", "generated", "assisted", "none", "false", "not-pure", "true", "0", "1"} {
		resp := aiFeedRaw(t, h, aiAuthor, bad, "", nil)
		if _, is := resp.(openapi.ListPosts400JSONResponse); !is {
			t.Errorf("?ai=%q returned %T, want ListPosts400JSONResponse — an unrecognised "+
				"value must not render a predicate that matches nothing", bad, resp)
		}
	}

	// ⚠️ The two GOOD values must still be accepted after all that, and
	// case/space folding is part of the vocabulary rather than an
	// accident of it — facet.FacetType.CanonicalValue lowercases and
	// trims, so the wire is forgiving about spelling and closed about
	// meaning.
	for _, good := range []string{"not_pure", "NOT_PURE", " not_pure "} {
		got := aiFeed(t, h, aiAuthor, good, "")
		aiAssertAbsent(t, "?ai="+good, got, c.pure)
		aiAssertPresent(t, "?ai="+good, got, c.mixed)
	}
}
