// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #910 — you could find a collection and then the trail went cold:
// there was no way to search INSIDE one.
//
// The feature is a `collection` dimension on the #907 filter vocabulary
// (`filter=collection:<uuid>`), so most of what needs asserting is that
// it behaves like the dimensions beside it — it narrows, it composes
// with free text and with other dimensions, and an unparseable value is
// a 400 rather than a silent no-op.
//
// TWO THINGS HERE ARE NOT ROUTINE, and they are why this file exists
// rather than a handful of extra rows in facet_filter_test.go:
//
//  1. MEMBERSHIP MUST NOT WIDEN VISIBILITY (ADR 0009 §3). We diverge
//     deliberately from the prior art that cascades a container's
//     permissions to its contents: a collection may contain a member the
//     reader cannot open, and scoping a search to that collection must
//     not be the door that hands it over. Asserted as a POSITIVE
//     CONTROL — one row, two callers, OPPOSITE verdicts. A test where
//     both callers get the same answer proves only that the fixture is
//     uniform.
//
//  2. THE PARENT GATE. `collection:<id>` is the first dimension whose
//     value names another entity with a read rule of its own, so a
//     caller holding the id of a collection they cannot open must not be
//     able to enumerate which readable assets are curated into it. Same
//     shape of assertion, one level up: same collection, two callers,
//     opposite verdicts.
//
// Skips the DB half without AA_DB_PASSWORD.

package search

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// Shape — no database.
// ---------------------------------------------------------------------------

// TestParseSelection_CollectionValueIsTyped pins the one way this
// dimension differs at the parse layer: its value is a UUID, not opaque
// text, so a malformed one has to be rejected at the edge. Left
// unchecked it reaches a `::UUID` cast and becomes a Postgres 22P02 —
// a 500 for what is a caller mistake.
func TestParseSelection_CollectionValueIsTyped(t *testing.T) {
	id := uuid.New()
	sel, err := facet.ParseSelection([]string{"collection:" + id.String()})
	if err != nil {
		t.Fatalf("a well-formed collection filter was rejected: %v", err)
	}
	terms := sel.Terms()
	if len(terms) != 1 || terms[0].Type != facet.FacetCollection || terms[0].Value != id.String() {
		t.Fatalf("terms = %+v, want one collection term for %s", terms, id)
	}

	for _, bad := range []string{
		"collection:not-a-uuid",
		"collection:12345",
		"collection:" + id.String() + "x",
		// A tag value that would be fine on any other dimension.
		"collection:sketch",
	} {
		if _, err := facet.ParseSelection([]string{bad}); err == nil {
			t.Errorf("filter=%q was accepted; an unparseable value must be a 400, "+
				"not a query that errors at the database", bad)
		}
	}

	// The other dimensions are unaffected: their values stay opaque
	// text, and nonsense there still means "matches nothing" rather than
	// a rejection. Tightening them would break `extension:` values that
	// are legitimately odd.
	if _, err := facet.ParseSelection([]string{"extension:!!!", "tag:???"}); err != nil {
		t.Errorf("value validation leaked onto the text dimensions: %v", err)
	}
}

// TestParseSelection_CollectionValueCanonicalises — google/uuid accepts
// braced and hyphenless spellings; Postgres accepts a different subset.
// One spelling reaches the SQL, and two spellings of the same id share a
// cache key instead of running the same search twice.
func TestParseSelection_CollectionValueCanonicalises(t *testing.T) {
	id := uuid.New()
	canonical, err := facet.ParseSelection([]string{"collection:" + id.String()})
	if err != nil {
		t.Fatalf("parse canonical: %v", err)
	}
	for _, spelling := range []string{
		"collection:{" + id.String() + "}",
		"collection:" + id.String(),
	} {
		got, err := facet.ParseSelection([]string{spelling})
		if err != nil {
			t.Fatalf("parse %q: %v", spelling, err)
		}
		if got.CacheKey() != canonical.CacheKey() {
			t.Errorf("%q produced a different selection than the canonical spelling", spelling)
		}
	}
}

// TestCollectionFacet_HasNoAggregator states the deliberate omission so
// a later "the set looks incomplete" edit has to argue with a test. A
// bucket list here would enumerate every collection beside every search
// and cost a COUNT per collection, to answer a question nobody asked:
// scoping arrives from a collection page carrying an id.
func TestCollectionFacet_HasNoAggregator(t *testing.T) {
	for _, ft := range facet.AllFacets() {
		if ft == facet.FacetCollection {
			t.Fatal("FacetCollection is in AllFacets — the dispatcher will look for an " +
				"aggregator that does not exist, and /search/facets would silently " +
				"return a missing facet on every request")
		}
	}
	if _, ok := facet.ParseFacetType("collection"); !ok {
		t.Error("ParseFacetType rejects `collection`, so filter=collection:<id> is a 400")
	}
}

// ---------------------------------------------------------------------------
// Behaviour — against the database.
// ---------------------------------------------------------------------------

const (
	cfOwner    int64 = 9101101
	cfStranger int64 = 9101102
)

// cfPhrase appears in every title in this fixture and nowhere else, so
// the unfiltered search is a known quantity and any narrowing is
// attributable to the filter.
const cfPhrase = "quibblesnort"

type cfFixture struct {
	// Members of the scoped collection.
	memberPNG        uuid.UUID
	memberJPG        uuid.UUID
	memberRestricted uuid.UUID
	memberExpired    uuid.UUID
	memberPost       uuid.UUID
	// Outside it.
	loosePNG  uuid.UUID
	loosePost uuid.UUID
	// The containers.
	publicCollection  uuid.UUID
	privateCollection uuid.UUID
}

func cfSeed(t *testing.T, pool *pgxpool.Pool) cfFixture {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2), ($3, $4)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		cfOwner, "cf-owner-910", cfStranger, "cf-stranger-910"); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM "user" WHERE ref = ANY($1::BIGINT[])`,
			[]int64{cfOwner, cfStranger})
	})

	asset := func(title, ext, sensitivity string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, file_extension)
			VALUES ($1, $2, '', $3, (SELECT MIN(ref) FROM asset_types), 'active', $4,
			        'ready', $5)`,
			id, title, cfOwner, sensitivity, ext); err != nil {
			t.Fatalf("seed asset %q: %v", title, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
		})
		return id
	}
	post := func(title string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO posts (id, author_user_ref, title, visibility)
			 VALUES ($1, $2, $3, 'public')`,
			id, cfOwner, title); err != nil {
			t.Fatalf("seed post %q: %v", title, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id)
		})
		return id
	}
	collection := func(name, vis string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO collections (id, owner_user_ref, name, visibility)
			 VALUES ($1, $2, $3, $4)`,
			id, cfOwner, name, vis); err != nil {
			t.Fatalf("seed collection %q: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id)
		})
		return id
	}

	f := cfFixture{
		memberPNG:        asset(cfPhrase+" member png", "png", "public"),
		memberJPG:        asset(cfPhrase+" member jpg", "jpg", "public"),
		memberRestricted: asset(cfPhrase+" member sealed", "png", "restricted"),
		memberExpired:    asset(cfPhrase+" member lapsed", "png", "public"),
		loosePNG:         asset(cfPhrase+" loose png", "png", "public"),
		memberPost:       post(cfPhrase + " member post"),
		loosePost:        post(cfPhrase + " loose post"),
		// The collection NAMES avoid cfPhrase deliberately: a collection
		// carries none of the facet dimensions, so it drops out of any
		// filtered search — but it would still appear in the UNFILTERED
		// baseline below and make that number harder to reason about.
		publicCollection:  collection("cf public scope 910", "public"),
		privateCollection: collection("cf private scope 910", "private"),
	}

	// Membership. `pinned` and `expires_at` are left at their defaults
	// except where the row exists to test them.
	member := func(collectionID, assetID uuid.UUID, expired bool) {
		var expires any
		if expired {
			expires = time.Now().Add(-24 * time.Hour)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO collection_resources (collection_id, asset_id, sort_order, expires_at)
			VALUES ($1, $2, 0, $3::TIMESTAMPTZ)`,
			collectionID, assetID, expires); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	member(f.publicCollection, f.memberPNG, false)
	member(f.publicCollection, f.memberJPG, false)
	member(f.publicCollection, f.memberRestricted, false)
	member(f.publicCollection, f.memberExpired, true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO collection_posts (collection_id, post_id, sort_order) VALUES ($1, $2, 0)`,
		f.publicCollection, f.memberPost); err != nil {
		t.Fatalf("seed post membership: %v", err)
	}
	// The private collection holds the SAME readable rows. That is what
	// makes the parent-gate assertion a control rather than a tautology:
	// every row it scopes to is one the stranger can already find by
	// searching, so a difference in the answer is the gate and nothing
	// else.
	member(f.privateCollection, f.memberPNG, false)
	member(f.privateCollection, f.memberJPG, false)

	return f
}

// cfRun executes one search for a caller. Capabilities are resolved from
// nothing — these callers hold no content-plane capability, which is the
// point of the restricted-member control.
func cfRun(t *testing.T, e *Engine, ref *int64, raw ...string) QueryResult {
	t.Helper()
	sel, err := facet.ParseSelection(raw)
	if err != nil {
		t.Fatalf("parse %v: %v", raw, err)
	}
	res, err := e.Run(context.Background(), Query{
		Text:          cfPhrase,
		Types:         AllHitTypes(),
		Limit:         50,
		CallerUserRef: ref,
		Filters:       sel,
	})
	if err != nil {
		t.Fatalf("engine %v: %v", raw, err)
	}
	return res
}

func cfHas(res QueryResult, id uuid.UUID) bool {
	for _, h := range res.Hits {
		if h.ID == id {
			return true
		}
	}
	return false
}

func cfIDs(res QueryResult) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.ID)
	}
	return out
}

// TestCollectionFilter_NarrowsToMembers is the headline: assets AND
// posts, in one filter, through the same predicate set as every other
// dimension.
func TestCollectionFilter_NarrowsToMembers(t *testing.T) {
	pool := coPool(t)
	f := cfSeed(t, pool)
	e := NewEngine(pool)
	ref := cfOwner
	scope := "collection:" + f.publicCollection.String()

	// Baseline: unscoped, the owner sees everything in the fixture.
	base := cfRun(t, e, &ref)
	for _, id := range []uuid.UUID{f.memberPNG, f.loosePNG, f.memberPost, f.loosePost} {
		if !cfHas(base, id) {
			t.Fatalf("unfiltered search is missing %s — the fixture is wrong and every "+
				"assertion below is meaningless (got %v)", id, cfIDs(base))
		}
	}

	scoped := cfRun(t, e, &ref, scope)
	for _, c := range []struct {
		id   uuid.UUID
		want bool
		why  string
	}{
		{f.memberPNG, true, "an asset member"},
		{f.memberJPG, true, "another asset member"},
		{f.memberPost, true, "a POST member — collection_posts is the other half of the dimension"},
		{f.loosePNG, false, "an asset outside the collection"},
		{f.loosePost, false, "a post outside the collection"},
		{f.memberExpired, false, "a member whose membership has EXPIRED; the contents page " +
			"stopped listing it, so searching inside must not reach it either"},
	} {
		if got := cfHas(scoped, c.id); got != c.want {
			t.Errorf("scoped search: %s (%s) present=%v, want %v", c.why, c.id, got, c.want)
		}
	}
	// The count travels with the array, as it does for every other
	// dimension.
	if scoped.TotalCount != len(scoped.Hits) {
		t.Errorf("total_count %d != %d hits", scoped.TotalCount, len(scoped.Hits))
	}
	// A collection is not a member of itself, and scoping to one must
	// not return it (or any other collection) beside its contents.
	for _, h := range scoped.Hits {
		if h.Type == HitTypeCollection {
			t.Errorf("a collection (%s) came back inside a collection-scoped search", h.ID)
		}
	}
}

// TestCollectionFilter_Composes — a predicate that only works alone is
// not a predicate.
func TestCollectionFilter_Composes(t *testing.T) {
	pool := coPool(t)
	f := cfSeed(t, pool)
	e := NewEngine(pool)
	ref := cfOwner
	scope := "collection:" + f.publicCollection.String()

	// With another dimension. png ∩ members = memberPNG (+ the
	// restricted member, which the owner may open).
	withExt := cfRun(t, e, &ref, scope, "extension:png")
	if !cfHas(withExt, f.memberPNG) {
		t.Errorf("collection + extension:png lost the png member")
	}
	if cfHas(withExt, f.memberJPG) {
		t.Errorf("collection + extension:png returned the JPG member — the two predicates " +
			"are not being ANDed")
	}
	if cfHas(withExt, f.loosePNG) {
		t.Errorf("collection + extension:png returned a png from OUTSIDE the collection")
	}

	// With a free-text query. The scoped search above matched every
	// fixture title; narrowing the text to one of them must intersect,
	// not replace.
	res, err := e.Run(context.Background(), Query{
		Text:          cfPhrase + " sealed",
		Types:         AllHitTypes(),
		Limit:         50,
		CallerUserRef: &ref,
		Filters:       mustSelection(t, scope),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if !cfHas(res, f.memberRestricted) {
		t.Errorf("q + collection scope lost the row matching both")
	}

	// And with the OTHER container: two collection terms are an OR
	// (only `tag` is conjunctive), so scoping to both returns the union.
	union := cfRun(t, e, &ref, scope, "collection:"+f.privateCollection.String())
	if !cfHas(union, f.memberRestricted) || !cfHas(union, f.memberPNG) {
		t.Errorf("two collection terms did not OR: got %v", cfIDs(union))
	}
}

func mustSelection(t *testing.T, raw ...string) facet.Selection {
	t.Helper()
	sel, err := facet.ParseSelection(raw)
	if err != nil {
		t.Fatalf("parse %v: %v", raw, err)
	}
	return sel
}

// TestCollectionFilter_MembershipNeverWidensVisibility is acceptance
// item 3, as a POSITIVE CONTROL: one row, two callers, opposite
// verdicts.
//
// The restricted member sits in a PUBLIC collection. ADR 0009 §3 records
// this as a deliberate divergence from the prior art that cascades a
// container's permissions to its contents — here the container's tier
// buys the member nothing, and the collection-contents page renders it
// as a placeholder carrying only its owner's name (#883/#892/#894).
//
// The stranger CAN see the row unfiltered: the authenticated asset
// predicate is soft-delete only (ADR 0064), so a restricted asset is
// listed as a placeholder and counted. That is what makes the filtered
// verdict meaningful rather than an artefact of the row being invisible
// everywhere.
func TestCollectionFilter_MembershipNeverWidensVisibility(t *testing.T) {
	pool := coPool(t)
	f := cfSeed(t, pool)
	e := NewEngine(pool)
	owner, stranger := cfOwner, cfStranger
	scope := "collection:" + f.publicCollection.String()

	if !cfHas(cfRun(t, e, &stranger), f.memberRestricted) {
		t.Fatalf("the stranger cannot see the restricted member even UNFILTERED, so the " +
			"assertion below would pass on a build with no filter at all")
	}

	ownerScoped := cfRun(t, e, &owner, scope)
	strangerScoped := cfRun(t, e, &stranger, scope)

	if !cfHas(ownerScoped, f.memberRestricted) {
		t.Errorf("the OWNER lost their own restricted asset when scoping to a collection " +
			"it belongs to — the filter is over-narrowing")
	}
	if cfHas(strangerScoped, f.memberRestricted) {
		t.Errorf("a caller who cannot open the restricted member reached it by filtering " +
			"to the public collection containing it. Membership widened visibility, " +
			"which ADR 0009 §3 forbids: the collection's public tier is not the " +
			"member's.")
	}
	// The control on the control: the stranger still gets the PUBLIC
	// members. Without this, the assertion above passes on a build where
	// the collection filter returns nothing to anybody.
	if !cfHas(strangerScoped, f.memberPNG) {
		t.Errorf("the stranger got none of the public members either — the filter is "+
			"broken rather than correctly narrow (got %v)", cfIDs(strangerScoped))
	}
}

// TestCollectionFilter_AnonymousScopesToPublicMembers is the same rule
// at the other end of the caller range, where the asset predicate itself
// (not just the content plane) does the excluding.
func TestCollectionFilter_AnonymousScopesToPublicMembers(t *testing.T) {
	pool := coPool(t)
	f := cfSeed(t, pool)
	e := NewEngine(pool)
	scope := "collection:" + f.publicCollection.String()

	anon := cfRun(t, e, nil, scope)
	if !cfHas(anon, f.memberPNG) {
		t.Errorf("anonymous scoping to a PUBLIC collection got none of its public members")
	}
	if cfHas(anon, f.memberRestricted) {
		t.Errorf("anonymous reached a restricted member by scoping to the public " +
			"collection containing it")
	}
	// A private collection is not readable anonymously, so the parent
	// gate closes and the page is empty — including for the two public
	// assets inside it, which anonymous CAN find by searching normally.
	if n := len(cfRun(t, e, nil, "collection:"+f.privateCollection.String()).Hits); n != 0 {
		t.Errorf("anonymous scoped to a PRIVATE collection and got %d hits", n)
	}
}

// TestCollectionFilter_ParentGate — the second non-routine assertion.
//
// Every row in the private collection is one the stranger can already
// find by searching without a filter, so nothing here is about the
// members' own readability. What must not leak is the MEMBERSHIP: which
// of the assets I can read are curated into a collection I cannot open.
// The contents endpoint states the rule this borrows ("the parent gate
// and the member gate answer different questions and both are
// required"); scoping a search is a read of the same membership by
// another door.
func TestCollectionFilter_ParentGate(t *testing.T) {
	pool := coPool(t)
	f := cfSeed(t, pool)
	e := NewEngine(pool)
	owner, stranger := cfOwner, cfStranger
	scope := "collection:" + f.privateCollection.String()

	// Both callers can see the members themselves, unfiltered.
	if !cfHas(cfRun(t, e, &stranger), f.memberPNG) {
		t.Fatalf("the stranger cannot see the member unfiltered; the assertion below " +
			"would pass for the wrong reason")
	}

	ownerScoped := cfRun(t, e, &owner, scope)
	strangerScoped := cfRun(t, e, &stranger, scope)

	if !cfHas(ownerScoped, f.memberPNG) {
		t.Errorf("the collection's OWNER cannot search inside their own private collection")
	}
	if len(strangerScoped.Hits) != 0 || strangerScoped.TotalCount != 0 {
		t.Errorf("a caller who cannot open the collection enumerated its membership by "+
			"scoping to its id: %d hits, total %d (%v). Holding an id is not a grant.",
			len(strangerScoped.Hits), strangerScoped.TotalCount, cfIDs(strangerScoped))
	}

	// The ONE exception, and it is not invented here: GetCollection
	// (collections/handler.go) lets a system.admin holder through where
	// CanSee refuses, because the collection read rule carries no admin
	// disjunct. Matching it is what stops the "Search in this collection"
	// button returning an empty page from a collection page the same
	// caller just opened.
	admin, err := e.Run(context.Background(), Query{
		Text:          cfPhrase,
		Types:         AllHitTypes(),
		Limit:         50,
		CallerUserRef: &stranger,
		Filters:       mustSelection(t, scope),
		Caps:          visibility.ContentCaps{SystemAdmin: true},
	})
	if err != nil {
		t.Fatalf("engine (admin): %v", err)
	}
	if !cfHas(admin, f.memberPNG) {
		t.Errorf("a system.admin holder — who can open this collection through " +
			"GET /collections/{id} — got nothing when scoping a search to it")
	}
}

// TestCollectionFacets_CountWhatTheScopedPageShows — the aggregators
// take the same Selection the Engine does (#907), so a collection scope
// reaches the rail for free. Asserted rather than assumed: this is the
// property that makes a bucket's number equal what ticking it returns,
// and it is the first dimension the rail cannot itself produce.
func TestCollectionFacets_CountWhatTheScopedPageShows(t *testing.T) {
	pool := coPool(t)
	f := cfSeed(t, pool)
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	owner := cfOwner
	scope := "collection:" + f.publicCollection.String()

	count := func(caller *int64, raw ...string) map[string]int64 {
		req := facet.Request{
			QueryText: cfPhrase,
			Selection: mustSelection(t, raw...),
			Caller:    visibility.NewCaller(caller),
			Facets:    []facet.FacetType{facet.FacetExtension},
		}
		out := map[string]int64{}
		for _, b := range d.Run(context.Background(), req).Facets[facet.FacetExtension].Buckets {
			out[b.Value] = b.Count
		}
		return out
	}

	unscoped := count(&owner)
	scoped := count(&owner, scope)
	if scoped["png"] >= unscoped["png"] {
		t.Errorf("the extension rail did not narrow inside the collection: png %d scoped "+
			"vs %d unscoped", scoped["png"], unscoped["png"])
	}
	// memberPNG + memberRestricted (the owner may open both);
	// memberExpired's membership has lapsed and loosePNG is outside.
	if scoped["png"] != 2 {
		t.Errorf("scoped png bucket = %d, want 2 (the two live png members)", scoped["png"])
	}
	if scoped["jpg"] != 1 {
		t.Errorf("scoped jpg bucket = %d, want 1", scoped["jpg"])
	}

	// The rail obeys the parent gate too — a count computed inside a
	// collection the caller may not open answers "how many pngs are in
	// it" without ever listing a row.
	stranger := cfStranger
	if n := count(&stranger, "collection:"+f.privateCollection.String())["png"]; n != 0 {
		t.Errorf("the facet rail counted %d pngs inside a collection the caller cannot "+
			"open — the count is the oracle the results were gated against", n)
	}
}
