// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1242 — THE `ai:` FILTER DIMENSION.
//
// A viewer may ask not to see AI work. The owner ruled on what that
// actually excludes:
//
//	> If someone filters out AI content it should still show a post that
//	> has mixed AI/non-AI content — only exclude posts with pure AI. AI
//	> could be used as part of an ideation phase and the final project
//	> might be pure human made.
//
// Recorded as ADR 0094's fourth amendment. The derivation half is held
// down in assets/ai_purity_derivation_test.go; what is here is the
// GRAMMAR half — the dimension, its value vocabulary, and how it
// composes.
//
// # Four things need holding down by a test rather than by a comment
//
//  1. THE MIXED POST SURVIVES. `{generated, none}` must come back from a
//     search that excludes AI work, and `{generated, generated}` must
//     not. This is the assertion that regresses silently: both posts
//     read `ai_provenance = 'generated'`, so any implementation that
//     reaches for the labelling column passes every other test in this
//     file and fails this one.
//
//  2. IT FAILS TOWARD SHOWING. An UNDECLARED asset survives
//     `ai:not_pure`. The natural SQL — `ai_provenance <> 'generated'` —
//     is NULL for an undeclared asset and a NULL conjunct drops the row,
//     so every work nobody was asked about would vanish from the page of
//     a viewer who asked to see non-AI work. Wrongly hiding human work
//     is the error this whole design refuses to make.
//
//  3. IT COMBINES BY AND. Sprint 6 shipped a live bug where two filter
//     terms on different fields ORed and adding a filter made the result
//     set BIGGER (907 + 596 → 1191, the exact union, where the answer
//     was 312). So the fixture is built so a union and an intersection
//     give different numbers, and the assertion is `both < min(a, b)` —
//     `both > 0` passes on the union.
//
//  4. ⛔ IT IS A FILTER, NEVER A GATE (ADR 0094 §4). A caller who does
//     not ask for this dimension must see pure-AI work in their hits,
//     their total, their facet buckets and their suggestions exactly as
//     before. Nothing is withheld, so no derived copy inherits a
//     withholding obligation — the #1066 list stays out of this — and
//     that property is what keeps the column cheap. It disappears the
//     moment something subtracts on it for a caller who never asked.
//
// Skips the DB half without AA_DB_PASSWORD.

package search

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// Shape — no database.
// ---------------------------------------------------------------------------

// TestParseSelection_AIValueVocabularyIsClosed pins the parse layer.
//
// `ai:` is the third dimension with a value grammar and the first whose
// vocabulary is a closed set of two. Tolerating an unknown value is
// worse here than on an opaque-text dimension: `extension:!!!` matches
// nothing and that is the honest answer to what was asked, whereas a
// tolerated `ai:generated` would render a predicate matching nothing and
// hand an EMPTY page to someone who asked to hide AI work. A 400 is a
// mistake the client can see.
func TestParseSelection_AIValueVocabularyIsClosed(t *testing.T) {
	for _, good := range []struct{ raw, want string }{
		{"ai:pure", facet.AIPure},
		{"ai:not_pure", facet.AINotPure},
		// Canonicalised, so a differently-cased tick shares a cache key
		// with its twin instead of paying for the same query twice.
		{"ai:PURE", facet.AIPure},
		{"ai:Not_Pure", facet.AINotPure},
	} {
		sel, err := facet.ParseSelection([]string{good.raw})
		if err != nil {
			t.Fatalf("filter=%q was rejected: %v", good.raw, err)
		}
		terms := sel.Terms()
		if len(terms) != 1 || terms[0].Type != facet.FacetAI || terms[0].Value != good.want {
			t.Errorf("filter=%q parsed to %+v, want one ai term with value %q",
				good.raw, terms, good.want)
		}
	}
	for _, bad := range []string{
		"ai:",
		"ai:true",
		"ai:none",
		// ⭐ The three values of the LABELLING column. Accepting any of
		// them would be the bug this dimension exists to avoid, arriving
		// through the parser.
		"ai:generated",
		"ai:assisted",
		"ai:mixed",
		"ai:pure_ai",
		"ai:notpure",
	} {
		if _, err := facet.ParseSelection([]string{bad}); err == nil {
			t.Errorf("filter=%q was accepted; an unknown value must be a 400, not a "+
				"predicate that silently matches nothing", bad)
		}
	}
}

// TestSelection_AITermsRoundTripAndKey — the two values must not share a
// cache key, or one would be served to the other for the rest of the
// TTL, which on this dimension means showing AI work to the caller who
// asked to hide it.
func TestSelection_AITermsRoundTripAndKey(t *testing.T) {
	mk := func(raw string) facet.Selection {
		s, err := facet.ParseSelection([]string{raw})
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return s
	}
	pure, notPure := mk("ai:pure"), mk("ai:not_pure")
	if pure.CacheKey() == notPure.CacheKey() {
		t.Error("ai:pure and ai:not_pure share a cache key")
	}
	if (facet.Selection{}).CacheKey() == notPure.CacheKey() {
		t.Error("an unfiltered search shares a cache key with a filtered one")
	}
	if got := notPure.Params(); len(got) != 1 || got[0] != "ai:not_pure" {
		t.Errorf("Params() = %v, want [ai:not_pure] — a saved search and a "+
			"save-as-collection both round-trip through this", got)
	}
}

// TestFacetAI_IsFilterOnly pins two decisions that are otherwise
// invisible and would rot silently.
func TestFacetAI_IsFilterOnly(t *testing.T) {
	// No bucket list. A two-bucket rail reading "not_pure 1,946 /
	// pure 1" is not a discovery surface, and #907's invariant — a
	// bucket's number equals what ticking it returns — makes no promise
	// a dimension without buckets can break.
	for _, ft := range facet.AllFacets() {
		if ft == facet.FacetAI {
			t.Error("FacetAI is in AllFacets(); it is a filter-only dimension like " +
				"collection and field, and adding it there costs a COUNT per search")
		}
	}
	// …but it still PARSES, which is what makes `filter=ai:…` reach the
	// engine. FacetCollection has had exactly this shape since #910.
	if got, ok := facet.ParseFacetType("ai"); !ok || got != facet.FacetAI {
		t.Errorf("ParseFacetType(\"ai\") = (%q, %v), want (ai, true)", got, ok)
	}
	// It is NOT the field dimension, so it does not poison the cache.
	// `field:` is uncacheable because read_capability names a capability
	// from an open set; `ai:` names a maintained column and every
	// component of its answer is already in the key.
	sel, err := facet.ParseSelection([]string{"ai:not_pure"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sel.NamesFieldDimension() {
		t.Error("an ai selection reported itself as a field selection, which would " +
			"make every AI-filtered search uncacheable for no reason")
	}
}

// ---------------------------------------------------------------------------
// Behaviour — against the database.
// ---------------------------------------------------------------------------

const aiOwner int64 = 12420101

// aiPhrase appears in the title of every fixture row and nowhere else in
// any developer's database, so every count below is attributable to this
// fixture alone.
const aiPhrase = "quibblewotsit"

// aiTag is carried by three of the five posts and by NO asset, so a
// second dimension is available for the AND arithmetic without changing
// the asset numbers.
const aiTag = "quibbletag"

// aiFixture is the corpus. It is built so that EVERY wrong rule produces
// a different, wrong number:
//
//   - keying the filter on `ai_provenance` instead of purity would drop
//     the mixed and undeclared posts, so `ai:not_pure` would return 2
//     posts rather than 4;
//   - `<> 'generated'` instead of `IS DISTINCT FROM` would drop the one
//     undeclared asset, so `ai:not_pure` would return 5 assets rather
//     than 6;
//   - ORing two dimensions instead of ANDing them returns 12 where the
//     answer is 2;
//   - dropping collections from an exclusion returns 10 where the answer
//     is 11.
//
// No single count can be confused with another.
type aiFixture struct {
	purePost, mixedPost, undeclaredPost, assistedPost, humanPost uuid.UUID
	collection                                                   uuid.UUID
	// pureTitle is the title of the pure-AI post, used to prove that
	// suggest still completes it (the filter-not-gate assertion).
	pureTitle string
}

func aiSeed(t *testing.T, pool *pgxpool.Pool) aiFixture {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		aiOwner, "ai-owner-1242"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, aiOwner, `DELETE FROM "user" WHERE ref = $1`)
	})

	spec := []struct {
		label   string
		members []string
		tagged  bool
	}{
		{"pure", []string{"generated", "generated"}, true},
		{"mixed", []string{"generated", "none"}, true},
		{"undeclared", []string{"generated", ""}, false},
		{"assisted", []string{"assisted", "assisted"}, false},
		{"human", []string{"none", "none"}, true},
	}
	byLabel := map[string]uuid.UUID{}
	titleByLabel := map[string]string{}
	for _, s := range spec {
		postID := uuid.New()
		title := aiPhrase + " " + s.label + " post"
		if _, err := pool.Exec(ctx, `
			INSERT INTO posts (id, author_user_ref, title, description, visibility)
			VALUES ($1, $2, $3, $3, 'public')`, postID, aiOwner, title); err != nil {
			t.Fatalf("seed post %s: %v", s.label, err)
		}
		t.Cleanup(func() {
			testdb.Purge(t, pool, postID, `DELETE FROM posts WHERE id = $1`)
		})
		if s.tagged {
			if _, err := pool.Exec(ctx,
				`INSERT INTO post_tags (post_id, tag) VALUES ($1, $2)`,
				postID, aiTag); err != nil {
				t.Fatalf("seed post tag %s: %v", s.label, err)
			}
		}
		for i, decl := range s.members {
			assetID := uuid.New()
			var v any
			if decl != "" {
				v = decl
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO assets (id, title, description, owner_user_ref, asset_type,
				                    status, sensitivity, processing_status, file_extension,
				                    ai_provenance)
				VALUES ($1, $2, '', $3, (SELECT MIN(ref) FROM asset_types),
				        'active', 'public', 'ready', 'png', $4)`,
				assetID, aiPhrase+" "+s.label+" member", aiOwner, v); err != nil {
				t.Fatalf("seed asset %s/%d: %v", s.label, i, err)
			}
			t.Cleanup(func() {
				testdb.Purge(t, pool, assetID, `DELETE FROM assets WHERE id = $1`)
			})
			if _, err := pool.Exec(ctx,
				`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, $3)`,
				postID, assetID, i); err != nil {
				t.Fatalf("seed membership %s/%d: %v", s.label, i, err)
			}
		}
		byLabel[s.label] = postID
		titleByLabel[s.label] = title
	}

	// One collection, so the entity that carries NO purity of its own is
	// represented. See TestAIFilter_AnExclusionDoesNotDeleteCollections.
	collectionID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`,
		collectionID, aiOwner, aiPhrase+" collection"); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, collectionID, `DELETE FROM collections WHERE id = $1`)
	})

	// The fixture is only meaningful if the DERIVATION agrees with the
	// table above, so it is checked here rather than assumed. A silent
	// disagreement would turn every count below into a test of the
	// wrong thing.
	for label, want := range map[string]bool{
		"pure": true, "mixed": false, "undeclared": false,
		"assisted": false, "human": false,
	} {
		var got bool
		if err := pool.QueryRow(ctx,
			`SELECT ai_pure FROM posts WHERE id = $1`, byLabel[label]).Scan(&got); err != nil {
			t.Fatalf("read ai_pure for %s: %v", label, err)
		}
		if got != want {
			t.Fatalf("fixture post %q derived ai_pure = %v, want %v — the corpus is "+
				"wrong and every assertion below is meaningless", label, got, want)
		}
	}

	return aiFixture{
		purePost:       byLabel["pure"],
		mixedPost:      byLabel["mixed"],
		undeclaredPost: byLabel["undeclared"],
		assistedPost:   byLabel["assisted"],
		humanPost:      byLabel["human"],
		collection:     collectionID,
		pureTitle:      titleByLabel["pure"],
	}
}

// aiRun executes one search over ALL three entity types and returns the
// hits. Types matter here: this dimension behaves differently on each,
// and a run pinned to assets would hide the post arm — the one the
// owner's ruling is actually about.
func aiRun(t *testing.T, pool *pgxpool.Pool, filters ...string) []Hit {
	t.Helper()
	sel, err := facet.ParseSelection(filters)
	if err != nil {
		t.Fatalf("parse %v: %v", filters, err)
	}
	ref := aiOwner
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          aiPhrase,
		Types:         AllHitTypes(),
		Limit:         50,
		Filters:       sel,
		CallerUserRef: &ref,
	})
	if err != nil {
		t.Fatalf("run %v: %v", filters, err)
	}
	// The count travels with the array or it is a second source of truth
	// for the same question — asserted on EVERY run through this helper,
	// so no future arm of this dimension can be added without it being
	// checked.
	if res.TotalCount != len(res.Hits) {
		t.Errorf("total_count %d but %d hits for %v — a filter that narrows one and "+
			"not the other turns the count into an oracle the hits are not",
			res.TotalCount, len(res.Hits), filters)
	}
	return res.Hits
}

// aiCount buckets a hit slice by entity type.
func aiCount(hits []Hit) (assets, posts, collections int) {
	for _, h := range hits {
		switch h.Type {
		case HitTypeAsset:
			assets++
		case HitTypePost:
			posts++
		case HitTypeCollection:
			collections++
		}
	}
	return
}

func aiHas(hits []Hit, id uuid.UUID) bool {
	for _, h := range hits {
		if h.ID == id {
			return true
		}
	}
	return false
}

// TestAIFilter_TheMixedPostSurvives is the owner's ruling, asserted
// directly. If one assertion in this file is read, it is this one.
//
// All four of `{generated, generated}`, `{generated, none}`,
// `{generated, undeclared}` and `{generated, assisted}` derive
// `ai_provenance = 'generated'`. An implementation that filters on that
// column excludes all four; the ruling says it must exclude exactly the
// first. That is the whole reason a second derived fact exists, and the
// failure is SILENT — the filter would look like it worked, and the only
// visible symptom would be an artist's mixed-media post quietly missing
// from other people's feeds.
func TestAIFilter_TheMixedPostSurvives(t *testing.T) {
	pool := coPool(t)
	fx := aiSeed(t, pool)

	hidden := aiRun(t, pool, "ai:not_pure")

	if !aiHas(hidden, fx.mixedPost) {
		t.Error("⭐ a {generated, none} post was EXCLUDED by ai:not_pure.\n" +
			"  AI in an ideation phase and a final piece painted by hand is HUMAN\n" +
			"  work, and excluding it punishes the honest declaration on the other\n" +
			"  member — the thing ADR 0094's whole design depends on being cheap.\n" +
			"  This is what a filter keyed on posts.ai_provenance does.")
	}
	if aiHas(hidden, fx.purePost) {
		t.Error("a {generated, generated} post SURVIVED ai:not_pure — the filter is " +
			"not excluding anything, so the assertion above proves nothing")
	}
	if !aiHas(hidden, fx.undeclaredPost) {
		t.Error("a {generated, undeclared} post was excluded. An undeclared member is " +
			"one nobody was asked about, and not-knowing must never hide an artist's work")
	}
	if !aiHas(hidden, fx.assistedPost) {
		t.Error("an all-`assisted` post was excluded. Assisted work is human work made " +
			"with AI help — exactly what the ruling protects — and `assisted` never " +
			"contributes to purity")
	}
	if !aiHas(hidden, fx.humanPost) {
		t.Error("a wholly disclaimed post was excluded by an AI filter")
	}

	// The counterweight. A filter that excluded nothing would satisfy
	// every assertion above and ship a dead feature.
	only := aiRun(t, pool, "ai:pure")
	if !aiHas(only, fx.purePost) {
		t.Error("ai:pure did not return the one purely-AI post, so the dimension is " +
			"not selecting either — this is a gate on nothing, not a filter")
	}
	for _, id := range []uuid.UUID{fx.mixedPost, fx.undeclaredPost, fx.assistedPost, fx.humanPost} {
		if aiHas(only, id) {
			t.Errorf("ai:pure returned post %s, which is not purely AI", id)
		}
	}
}

// TestAIFilter_PerEntityCounts is the arithmetic, per entity, with the
// numbers chosen so no wrong rule lands on a right total.
func TestAIFilter_PerEntityCounts(t *testing.T) {
	pool := coPool(t)
	aiSeed(t, pool)

	a, p, c := aiCount(aiRun(t, pool))
	if a != 10 || p != 5 || c != 1 {
		t.Fatalf("unfiltered search returned %d assets / %d posts / %d collections, "+
			"want 10 / 5 / 1 — the fixture is wrong and every assertion below is "+
			"meaningless", a, p, c)
	}

	// ⭐ THE UNDECLARED ASSET IS THE NULL TRAP. Six assets are not
	// `generated`: three `none`, two `assisted` and ONE undeclared.
	// `ai_provenance <> 'generated'` evaluates to NULL for that last one
	// and a NULL conjunct drops the row, so the obvious spelling returns
	// 5 here — every work nobody was asked about vanishing from the page
	// of a viewer who asked to see non-AI work.
	a, p, c = aiCount(aiRun(t, pool, "ai:not_pure"))
	if a != 6 {
		t.Errorf("ai:not_pure returned %d assets, want 6. Five means the UNDECLARED "+
			"asset was dropped by NULL semantics — use IS DISTINCT FROM", a)
	}
	if p != 4 {
		t.Errorf("ai:not_pure returned %d posts, want 4. Two means the filter is keyed "+
			"on the labelling column and the mixed and undeclared posts went with the "+
			"pure one", p)
	}
	if c != 1 {
		t.Errorf("ai:not_pure returned %d collections, want 1", c)
	}

	a, p, c = aiCount(aiRun(t, pool, "ai:pure"))
	if a != 4 || p != 1 || c != 0 {
		t.Errorf("ai:pure returned %d assets / %d posts / %d collections, "+
			"want 4 / 1 / 0", a, p, c)
	}

	// ⭐ THE PARTITION. Every row is in exactly one of the two values, so
	// the halves must sum to the whole. A rule that double-counted or
	// dropped a row — an asset whose declaration is neither matched nor
	// unmatched — shows up here and nowhere else.
	pureHits := len(aiRun(t, pool, "ai:pure"))
	notPureHits := len(aiRun(t, pool, "ai:not_pure"))
	if all := len(aiRun(t, pool)); pureHits+notPureHits != all {
		t.Errorf("ai:pure (%d) + ai:not_pure (%d) = %d, but the unfiltered search "+
			"returns %d — the two values must PARTITION the corpus",
			pureHits, notPureHits, pureHits+notPureHits, all)
	}
}

// TestAIFilter_TwoAITermsMeanEither is the same-dimension rule, decided
// deliberately rather than inherited.
//
// A post has exactly ONE purity state, so AND across two values of this
// dimension is unsatisfiable — it would return nothing forever, which is
// a filter that looks applied and is not. OR is the honest reading of
// "show me pure work or non-pure work", and because the two values
// partition the corpus it is equivalent to no constraint at all. That
// equivalence is the assertion: it is what makes the choice checkable
// instead of a comment.
func TestAIFilter_TwoAITermsMeanEither(t *testing.T) {
	pool := coPool(t)
	aiSeed(t, pool)

	both := len(aiRun(t, pool, "ai:pure", "ai:not_pure"))
	none := len(aiRun(t, pool))
	if both != none {
		t.Errorf("ai:pure + ai:not_pure returned %d hits and an unfiltered search "+
			"returns %d. Zero means the dimension became conjunctive and now returns "+
			"nothing forever; anything else means the two values do not partition "+
			"the corpus", both, none)
	}
	// A repeated identical term collapses rather than squaring the work.
	if twice := len(aiRun(t, pool, "ai:pure", "ai:pure")); twice != len(aiRun(t, pool, "ai:pure")) {
		t.Errorf("a duplicated ai:pure term changed the result set (%d)", twice)
	}
}

// TestAIFilter_CombinesWithAnotherDimensionByAND is the plural rule
// across dimensions.
//
// ⚠️ The assertion is `both < min(a, b)`, not `both > 0`. Sprint 6
// shipped a live bug where two terms on different fields ORed — adding a
// filter made the page BIGGER, 907 + 596 → 1191, the exact union, where
// the answer was 312 — and every "the filter returned some rows"
// assertion passed on it.
func TestAIFilter_CombinesWithAnotherDimensionByAND(t *testing.T) {
	pool := coPool(t)
	fx := aiSeed(t, pool)

	// `tag:` is carried by three POSTS and by no asset, so it is a
	// genuinely independent dimension over the same corpus.
	tagOnly := len(aiRun(t, pool, "tag:"+aiTag))
	aiOnly := len(aiRun(t, pool, "ai:not_pure"))
	both := aiRun(t, pool, "tag:"+aiTag, "ai:not_pure")

	if tagOnly != 3 {
		t.Fatalf("tag:%s alone returned %d hits, want 3 — the fixture is wrong",
			aiTag, tagOnly)
	}
	if aiOnly != 11 {
		t.Fatalf("ai:not_pure alone returned %d hits, want 11 — the fixture is wrong", aiOnly)
	}

	min := tagOnly
	if aiOnly < min {
		min = aiOnly
	}
	if len(both) >= min {
		t.Errorf("tag:%s AND ai:not_pure returned %d hits, but each term alone returns "+
			"%d and %d. A conjunction must be SMALLER than either half; %d is the "+
			"union, which is the shape of the bug sprint 6 shipped",
			aiTag, len(both), tagOnly, aiOnly, tagOnly+aiOnly-1)
	}
	if len(both) != 2 {
		t.Errorf("tag:%s AND ai:not_pure returned %d hits, want exactly 2 (the mixed "+
			"post and the human post — the pure post carries the tag but is excluded, "+
			"and the untagged non-pure posts are excluded the other way)",
			aiTag, len(both))
	}
	if !aiHas(both, fx.mixedPost) || !aiHas(both, fx.humanPost) {
		t.Error("the intersection is the wrong TWO rows — a count can be right by accident")
	}
}

// TestAIFilter_AnExclusionDoesNotDeleteCollections.
//
// Every other dimension is a POSITIVE narrowing, and a collection
// dropping out of `extension:png` is the answer rather than a loss:
// "which files are pngs" is a question about files. `ai:not_pure` is an
// EXCLUSION wearing a value's clothes — the caller is saying "not that"
// — and silently removing every collection from that page would hide
// curated human work from someone who asked to see LESS AI work. Same
// fails-toward-showing rule as the derivation, applied to the mechanism.
//
// A collection is never a pure-AI WORK, so it is a member of `not_pure`
// and never of `pure`.
func TestAIFilter_AnExclusionDoesNotDeleteCollections(t *testing.T) {
	pool := coPool(t)
	fx := aiSeed(t, pool)

	if !aiHas(aiRun(t, pool, "ai:not_pure"), fx.collection) {
		t.Error("asking to hide AI work removed a COLLECTION from the results. A " +
			"collection is not a pure-AI work; it is a container we derive no purity " +
			"for, and an exclusion must not delete an entity it cannot describe")
	}
	if aiHas(aiRun(t, pool, "ai:pure"), fx.collection) {
		t.Error("ai:pure returned a collection — we derive no purity for a container, " +
			"so it can never satisfy the positive value")
	}
	// The OTHER dimensions are unchanged: a collection still drops out
	// of a search filtered by a dimension it genuinely does not carry.
	if _, _, c := aiCount(aiRun(t, pool, "extension:png")); c != 0 {
		t.Errorf("extension:png returned %d collections, want 0 — #1242 must not have "+
			"widened the satisfiability rule for the dimensions that describe a FILE", c)
	}
	if _, _, c := aiCount(aiRun(t, pool, "ai:not_pure", "extension:png")); c != 0 {
		t.Errorf("ai:not_pure + extension:png returned %d collections, want 0 — one "+
			"unsatisfiable dimension still removes the entity", c)
	}
}

// TestAIFilter_IsAFilterAndNotAGate is ADR 0094 §4, asserted rather than
// asserted-about.
//
// The distinction is not "does it narrow" — every filter narrows. It is
// WHO it narrows for. A gate subtracts the work from everybody: it
// disappears from counts, from facet buckets, from suggestions, and
// every derived copy then inherits a withholding obligation (search
// text, facets, suggest, thumbhash, embeddings, counts, covers — the
// #1066 list). A filter subtracts only from the caller who asked, and
// that is what keeps this column free of all of it.
//
// So the assertion is about the caller who did NOT ask: pure-AI work
// must be present in their hits, their total, their facet buckets and
// their suggestions, exactly as before.
func TestAIFilter_IsAFilterAndNotAGate(t *testing.T) {
	pool := coPool(t)
	fx := aiSeed(t, pool)
	ctx := context.Background()
	ref := aiOwner

	// 1. HITS AND COUNTS. The pure post and its two AI assets are in the
	//    unfiltered result set of a caller who never mentioned `ai`.
	unfiltered := aiRun(t, pool)
	if !aiHas(unfiltered, fx.purePost) {
		t.Error("a purely-AI post is missing from an UNFILTERED search. That is a " +
			"gate, and it drags every derived copy into the withholding discipline")
	}
	if a, p, c := aiCount(unfiltered); a != 10 || p != 5 || c != 1 {
		t.Errorf("the unfiltered corpus is %d/%d/%d, want 10/5/1 — something is "+
			"subtracting on provenance for a caller who did not ask", a, p, c)
	}

	// 2. FACETS. The rail counts the pure-AI rows for the same caller.
	//    All ten fixture assets are `png`, so the bucket is 10 or
	//    something is being withheld from the count.
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	resp := d.Run(ctx, facet.Request{
		QueryText: aiPhrase,
		Caller:    visibility.NewCaller(&ref),
	})
	var pngBucket int64
	for _, b := range resp.Facets[facet.FacetExtension].Buckets {
		if strings.EqualFold(b.Value, "png") {
			pngBucket = b.Count
		}
	}
	if pngBucket != 10 {
		t.Errorf("the extension facet counts %d pngs, want 10 — the four AI-generated "+
			"assets must be COUNTED for a caller who did not ask to hide them", pngBucket)
	}
	// The tag bucket carries the pure post too: three posts are tagged,
	// one of them the pure one.
	var tagBucket int64
	for _, b := range resp.Facets[facet.FacetTag].Buckets {
		if b.Value == aiTag {
			tagBucket = b.Count
		}
	}
	if tagBucket != 3 {
		t.Errorf("the tag facet counts %d posts under %q, want 3 — the purely-AI post "+
			"is one of them and is not withheld from a count", tagBucket, aiTag)
	}

	// 3. SUGGEST. `suggest.Request` carries no Selection at all, so this
	//    dimension structurally cannot reach it — but "cannot reach it"
	//    is exactly the claim a future refactor breaks quietly, so the
	//    behaviour is pinned: the pure-AI post's own title completes.
	sug, err := suggest.NewService(pool).Suggest(ctx, suggest.Request{
		Prefix: fx.pureTitle,
		Caller: visibility.NewCaller(&ref),
	})
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	found := false
	for _, s := range sug.Suggestions {
		if s.Value == fx.pureTitle {
			found = true
		}
	}
	if !found {
		t.Errorf("suggest did not complete the purely-AI post's own title (%q). A "+
			"completion IS the title, so withholding one here would be a derived copy "+
			"withheld — and ADR 0094 §4 says nothing on this axis withholds",
			fx.pureTitle)
	}
}

// TestAIFilter_NarrowsTheRailInLockstep is the OTHER half of the
// filter/gate distinction, and it is not the same assertion.
//
// For the caller who DID ask, the rail must describe the page they are
// looking at. #907's invariant is that a bucket's number equals what
// ticking it returns, so a count that ignored the active `ai:` term
// would report the unfiltered corpus beside a narrowed grid and every
// number on it would be a lie about the page beside it.
//
// Read together with the test above: a filter narrows for the caller who
// asked and for nobody else. A gate would narrow for everybody; a broken
// filter would narrow for nobody.
func TestAIFilter_NarrowsTheRailInLockstep(t *testing.T) {
	pool := coPool(t)
	aiSeed(t, pool)
	ref := aiOwner

	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sel, err := facet.ParseSelection([]string{"ai:not_pure"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resp := d.Run(context.Background(), facet.Request{
		QueryText: aiPhrase,
		Caller:    visibility.NewCaller(&ref),
		Selection: sel,
	})
	var pngBucket int64
	for _, b := range resp.Facets[facet.FacetExtension].Buckets {
		if strings.EqualFold(b.Value, "png") {
			pngBucket = b.Count
		}
	}
	if pngBucket != 6 {
		t.Errorf("under ai:not_pure the extension facet counts %d pngs, want 6 — the "+
			"same number ticking that bucket returns. 10 means the rail is still "+
			"describing the unfiltered corpus", pngBucket)
	}
	// And ticking it returns exactly that many.
	if a, _, _ := aiCount(aiRun(t, pool, "ai:not_pure", "extension:png")); int64(a) != pngBucket {
		t.Errorf("the bucket says %d but ticking it returns %d assets", pngBucket, a)
	}
}
