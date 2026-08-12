// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #907 — facets became filters. Every bucket on /search showed a real,
// correct count and did nothing; there was no way anywhere in the
// product to narrow a search by tag, asset type, owner, sensitivity or
// extension.
//
// The visibility half of the arc lives in hit_withholding_test.go, in
// TestFacets_CountOnlyWhatTheCallerCanOpen, which was EXTENDED rather
// than duplicated: it already held the judgement about what a count may
// disclose, and the filter has to make the same one.
//
// What is here:
//
//   - the pure-Go shape of a Selection (parsing, cache key, the
//     apply-to-others rule for counting) — no DB, always runs;
//   - ticking a bucket changes the result set, per dimension;
//   - the count equals the filtered total, for every bucket the
//     aggregators emit rather than for one hand-picked one;
//   - the cache does not serve one selection's results for another's;
//   - anonymous and authenticated callers both behave.
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

	appcache "github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// Shape — no database.
// ---------------------------------------------------------------------------

func TestParseSelection_WireShape(t *testing.T) {
	got, err := facet.ParseSelection([]string{"tag:sketch", "ext:PNG", "owner:alice", ""})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []facet.Term{
		{Type: facet.FacetTag, Value: "sketch"},
		{Type: facet.FacetExtension, Value: "PNG"},
		{Type: facet.FacetOwner, Value: "alice"},
	}
	terms := got.Terms()
	if len(terms) != len(want) {
		t.Fatalf("got %d terms, want %d: %+v", len(terms), len(want), terms)
	}
	for i := range want {
		if terms[i] != want[i] {
			t.Errorf("term %d = %+v, want %+v", i, terms[i], want[i])
		}
	}
	// A value may itself contain a colon — only the FIRST one separates.
	one, err := facet.ParseSelection([]string{"tag:a:b"})
	if err != nil {
		t.Fatalf("parse colon value: %v", err)
	}
	if v := one.Terms()[0].Value; v != "a:b" {
		t.Errorf("value = %q, want %q", v, "a:b")
	}
	// Unknown dimensions REJECT. A silently dropped filter renders a
	// page that looks narrowed and is not — the defect this issue is
	// about, reintroduced through the front door.
	for _, bad := range []string{"nonsense:x", "tag:", "tag", ":x"} {
		if _, err := facet.ParseSelection([]string{bad}); err == nil {
			t.Errorf("filter=%q was accepted; it must be a 400", bad)
		}
	}
}

func TestSelection_CacheKeyDistinguishesSelections(t *testing.T) {
	mk := func(raw ...string) string {
		s, err := facet.ParseSelection(raw)
		if err != nil {
			t.Fatalf("parse %v: %v", raw, err)
		}
		return s.CacheKey()
	}
	if mk("extension:png") == mk("extension:jpg") {
		t.Error("two different extensions share a cache key — they would serve each " +
			"other's results for the rest of the TTL")
	}
	if mk() == mk("extension:png") {
		t.Error("an empty selection shares a key with a filtered one")
	}
	// Order-insensitive: the rail can render buckets in any order and
	// still hit the cache.
	if mk("tag:a", "extension:png") != mk("extension:png", "tag:a") {
		t.Error("selection cache key depends on tick order")
	}
	// Separator-safe: a bucket value may contain a colon, so the key
	// must not be a naive join.
	if mk("tag:a:b") == mk("tag:a", "tag:b") {
		t.Error("`tag:a:b` collides with `tag:a`+`tag:b`")
	}
	// Duplicate ticks collapse rather than squaring the work.
	if mk("tag:a", "tag:a") != mk("tag:a") {
		t.Error("a duplicate term changed the key")
	}
}

// TestSelection_ForFacet pins the rule that lets a rail stay usable
// under an active filter, and it is the rule acceptance item 5 rests on:
// with nothing else selected, every facet counts against the unfiltered
// population, so a bucket's number is what ticking it returns.
func TestSelection_ForFacet(t *testing.T) {
	sel, err := facet.ParseSelection([]string{"extension:png", "tag:a", "sensitivity:public"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// An OR dimension drops its OWN terms: otherwise picking `png`
	// collapses the extension facet to one bucket and there is no way
	// back to `jpg` except clearing.
	for _, tm := range sel.ForFacet(facet.FacetExtension).Terms() {
		if tm.Type == facet.FacetExtension {
			t.Errorf("the extension facet still filters itself by %q", tm.Value)
		}
	}
	if n := len(sel.ForFacet(facet.FacetExtension).Terms()); n != 2 {
		t.Errorf("the extension facet kept %d of the other dimensions, want 2", n)
	}
	// The conjunctive dimension KEEPS its own: under `tag:a` the tag
	// facet should show what co-occurs with a, with the counts a second
	// tick will actually return.
	if n := len(sel.ForFacet(facet.FacetTag).Terms()); n != 3 {
		t.Errorf("the tag facet dropped its own term; it narrows on the next tick, so it "+
			"must keep it (kept %d of 3)", n)
	}
	// Nothing selected ⇒ nothing applied, for every dimension.
	for _, ft := range facet.AllFacets() {
		if !(facet.Selection{}).ForFacet(ft).Empty() {
			t.Errorf("%s: an empty selection produced a non-empty filter", ft)
		}
	}
}

// TestSelectionFromDSL_MatchesTheRail proves the two entry points land
// on one predicate set. `dsl=tag:x` and `filter=tag:x` must not come to
// mean different things.
func TestSelectionFromDSL_MatchesTheRail(t *testing.T) {
	parsed, err := dsl.Parse(`tag:sketch owner:alice type:Image sensitivity:public extension:png`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	compiled, err := dsl.Compile(parsed)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	fromDSL := selectionFromDSL(compiled.Filters, facet.Selection{})
	fromRail, err := facet.ParseSelection([]string{
		"tag:sketch", "owner:alice", "asset_type:Image", "sensitivity:public", "extension:png",
	})
	if err != nil {
		t.Fatalf("parse rail: %v", err)
	}
	if fromDSL.CacheKey() != fromRail.CacheKey() {
		t.Errorf("the DSL and the rail produced different selections:\n dsl  = %v\n rail = %v",
			fromDSL.Params(), fromRail.Params())
	}
	// The owner regression this conversion exists to close: `owner:alice`
	// used to be parsed with fmt.Sscanf("%d") into a *int64, fail, and
	// vanish into an unexported field — no filter at all.
	if compiled.Filters.Owner != "alice" {
		t.Errorf("owner filter = %q, want %q", compiled.Filters.Owner, "alice")
	}
}

// ---------------------------------------------------------------------------
// Behaviour — against the database.
// ---------------------------------------------------------------------------

const (
	ffOwner    int64 = 9071101
	ffStranger int64 = 9071102
)

// ffPhrase appears only in this fixture's titles.
const ffPhrase = "zomberulous"

type ffAsset struct {
	id        uuid.UUID
	extension string
	tags      []string
}

// ffSeed builds a small corpus with a deliberate SPREAD across every
// dimension, so a filter that silently does nothing cannot pass: each
// bucket holds a different number of rows.
func ffSeed(t *testing.T, pool *pgxpool.Pool) []ffAsset {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2), ($3, $4)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		ffOwner, "ff-owner-907", ffStranger, "ff-stranger-907"); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = ANY($1::BIGINT[])`,
			[]int64{ffOwner, ffStranger})
	})

	spec := []struct {
		title string
		ext   string
		tags  []string
	}{
		{ffPhrase + " alpha", "png", []string{"sketch", "wip"}},
		{ffPhrase + " beta", "png", []string{"sketch"}},
		{ffPhrase + " gamma", "png", []string{"wip"}},
		{ffPhrase + " delta", "jpg", []string{"sketch"}},
		{ffPhrase + " epsilon", "webm", nil},
	}
	out := make([]ffAsset, 0, len(spec))
	for _, s := range spec {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, file_extension)
			VALUES ($1, $2, '', $3, (SELECT MIN(ref) FROM asset_types), 'active', 'public',
			        'ready', $4)`,
			id, s.title, ffOwner, s.ext); err != nil {
			t.Fatalf("seed asset: %v", err)
		}
		for _, tag := range s.tags {
			if _, err := pool.Exec(ctx,
				`INSERT INTO asset_tag (asset_id, tag, source) VALUES ($1, $2, 'manual')`,
				id, tag); err != nil {
				t.Fatalf("seed tag: %v", err)
			}
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
		})
		out = append(out, ffAsset{id: id, extension: s.ext, tags: s.tags})
	}
	return out
}

func ffEngineRun(t *testing.T, e *Engine, ref *int64, sel facet.Selection) QueryResult {
	t.Helper()
	res, err := e.Run(context.Background(), Query{
		Text:          ffPhrase,
		Types:         AllHitTypes(),
		Limit:         50,
		CallerUserRef: ref,
		Filters:       sel,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return res
}

// TestFacetFilter_TickingABucketChangesTheResultSet is the headline:
// before #907, every one of these assertions would have seen the full
// five rows back.
func TestFacetFilter_TickingABucketChangesTheResultSet(t *testing.T) {
	pool := coPool(t)
	ffSeed(t, pool)
	e := NewEngine(pool)
	ref := ffOwner

	if n := len(ffEngineRun(t, e, &ref, facet.Selection{}).Hits); n != 5 {
		t.Fatalf("unfiltered search returned %d hits, want 5 — the fixture is wrong and "+
			"every assertion below is meaningless", n)
	}
	for _, c := range []struct {
		name string
		raw  []string
		want int
	}{
		{"one extension", []string{"extension:png"}, 3},
		{"another extension — a DIFFERENT number, so a no-op filter cannot pass",
			[]string{"extension:jpg"}, 1},
		{"case-insensitive extension", []string{"extension:PNG"}, 3},
		{"one tag", []string{"tag:sketch"}, 3},
		{"two tags AND, not OR — a tag is the one multi-valued dimension",
			[]string{"tag:sketch", "tag:wip"}, 1},
		{"two extensions OR, because AND is unsatisfiable on a single-valued column",
			[]string{"extension:png", "extension:jpg"}, 4},
		{"two dimensions AND", []string{"extension:png", "tag:wip"}, 2},
		{"owner by username", []string{"owner:ff-owner-907"}, 5},
		{"a value nothing carries", []string{"extension:tiff"}, 0},
		{"sensitivity", []string{"sensitivity:public"}, 5},
		{"sensitivity nothing carries", []string{"sensitivity:restricted"}, 0},
	} {
		sel, err := facet.ParseSelection(c.raw)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.name, err)
		}
		res := ffEngineRun(t, e, &ref, sel)
		if len(res.Hits) != c.want {
			t.Errorf("%s (%v): %d hits, want %d", c.name, c.raw, len(res.Hits), c.want)
		}
		// The count travels with the array or it is a second source of
		// truth for the same question.
		if res.TotalCount != c.want {
			t.Errorf("%s (%v): total_count %d, want %d", c.name, c.raw, res.TotalCount, c.want)
		}
	}
}

// TestFacetFilter_CountEqualsReality is acceptance item 5, asserted
// directly and across EVERY bucket the aggregators emit rather than one
// hand-picked one: for each (facet, value), the number on the rail must
// equal the number of results ticking it returns.
func TestFacetFilter_CountEqualsReality(t *testing.T) {
	pool := coPool(t)
	ffSeed(t, pool)
	e := NewEngine(pool)
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ref := ffOwner

	resp := d.Run(context.Background(), facet.Request{
		QueryText: ffPhrase,
		Caller:    visibility.NewCaller(&ref),
	})
	checked := 0
	for ft, res := range resp.Facets {
		for _, b := range res.Buckets {
			sel := facet.Selection{}.With(ft, b.Value)
			got := ffEngineRun(t, e, &ref, sel)
			if int64(got.TotalCount) != b.Count {
				t.Errorf("%s bucket %q says %d, ticking it returns %d",
					ft, b.Value, b.Count, got.TotalCount)
			}
			checked++
		}
	}
	// Guard against a green run that checked nothing — an empty facet
	// response would satisfy the loop above in silence.
	if checked < 6 {
		t.Fatalf("only %d buckets were checked; the fixture should produce at least 6 "+
			"(3 extensions + 2 tags + 1 owner + type + sensitivity)", checked)
	}
}

// TestFacetFilter_CountsReflectTheActiveSelection covers scope item 3:
// a count that ignores the active filter is the same bug one level up.
func TestFacetFilter_CountsReflectTheActiveSelection(t *testing.T) {
	pool := coPool(t)
	ffSeed(t, pool)
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ref := ffOwner

	ask := func(sel facet.Selection) map[facet.FacetType]map[string]int64 {
		resp := d.Run(context.Background(), facet.Request{
			QueryText: ffPhrase,
			Caller:    visibility.NewCaller(&ref),
			Selection: sel,
		})
		out := map[facet.FacetType]map[string]int64{}
		for name, f := range resp.Facets {
			m := map[string]int64{}
			for _, b := range f.Buckets {
				m[b.Value] = b.Count
			}
			out[name] = m
		}
		return out
	}

	base := ask(facet.Selection{})
	if base[facet.FacetTag]["sketch"] != 3 {
		t.Fatalf("unfiltered tag bucket sketch = %d, want 3", base[facet.FacetTag]["sketch"])
	}

	// A filter on ANOTHER dimension narrows this one. `extension:jpg`
	// leaves one asset, tagged `sketch` only.
	underJPG := ask(facet.Selection{}.With(facet.FacetExtension, "jpg"))
	if n := underJPG[facet.FacetTag]["sketch"]; n != 1 {
		t.Errorf("under extension:jpg the tag bucket sketch = %d, want 1 — the rail is "+
			"still describing the unfiltered corpus", n)
	}
	if _, present := underJPG[facet.FacetTag]["wip"]; present {
		t.Errorf("under extension:jpg the tag `wip` is still offered, but nothing that " +
			"survives the filter carries it — ticking it would return zero")
	}
	// …and the extension facet keeps offering the OTHER extensions,
	// because a single-valued dimension does not filter itself.
	if n := underJPG[facet.FacetExtension]["png"]; n != 3 {
		t.Errorf("under extension:jpg the extension facet lost `png` (got %d, want 3) — "+
			"there would be no way back to it except clearing the filter", n)
	}
	// The conjunctive dimension DOES narrow itself: under `tag:sketch`
	// the tag facet reports co-occurrence, which is what a second tick
	// returns.
	underTag := ask(facet.Selection{}.With(facet.FacetTag, "sketch"))
	if n := underTag[facet.FacetTag]["wip"]; n != 1 {
		t.Errorf("under tag:sketch the tag bucket wip = %d, want 1 (co-occurrence)", n)
	}
	if n := underTag[facet.FacetTag]["sketch"]; n != 3 {
		t.Errorf("under tag:sketch the tag bucket sketch = %d, want 3 (itself)", n)
	}
}

// TestFacetFilter_AnonymousAndAuthenticated — acceptance item 6. The
// facet endpoint has an anonymous path, and so does /search.
func TestFacetFilter_AnonymousAndAuthenticated(t *testing.T) {
	pool := coPool(t)
	assets := ffSeed(t, pool)
	// One of the five goes team-only, which an anonymous caller cannot
	// see AT ALL (the row predicate, not just the content plane).
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET sensitivity = 'team' WHERE id = $1`, assets[0].id); err != nil {
		t.Fatalf("update sensitivity: %v", err)
	}
	e := NewEngine(pool)
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ref := ffOwner

	sel := facet.Selection{}.With(facet.FacetExtension, "png")
	anon := ffEngineRun(t, e, nil, sel)
	auth := ffEngineRun(t, e, &ref, sel)
	if len(anon.Hits) != 2 {
		t.Errorf("anonymous got %d png hits, want 2 (the team row is not theirs to see)",
			len(anon.Hits))
	}
	if len(auth.Hits) != 3 {
		t.Errorf("the owner got %d png hits, want 3", len(auth.Hits))
	}
	// And the counts agree with each caller's own filtered page.
	for _, c := range []struct {
		name  string
		ref   *int64
		total int
	}{
		{"anonymous", nil, anon.TotalCount},
		{"owner", &ref, auth.TotalCount},
	} {
		resp := d.Run(context.Background(), facet.Request{
			QueryText: ffPhrase,
			Facets:    []facet.FacetType{facet.FacetExtension},
			Caller:    visibility.NewCaller(c.ref),
		})
		var count int64
		for _, b := range resp.Facets[facet.FacetExtension].Buckets {
			if b.Value == "png" {
				count = b.Count
			}
		}
		if int(count) != c.total {
			t.Errorf("%s: the png bucket says %d, ticking it returns %d",
				c.name, count, c.total)
		}
	}
}

// TestSearchCache_SelectionIsKeyed is the mutation-tested half of
// acceptance item 4.
//
// RUN IT BOTH WAYS. Remove `sb.WriteString(q.Filters.CacheKey())` from
// keyForQuery and this test must FAIL — the two selections then collide
// on one entry and the second Get returns the first's hits. A cache test
// that still passes without the key change is testing nothing, and this
// one was run in both configurations before the PR was opened.
func TestSearchCache_SelectionIsKeyed(t *testing.T) {
	c := NewCache(appcache.NewRegistry(nil, nil), 16, time.Minute, nil)
	base := Query{Text: "same query", CallerUserRef: ptrInt64(ffOwner), Limit: 25}

	png := base
	png.Filters = facet.Selection{}.With(facet.FacetExtension, "png")
	jpg := base
	jpg.Filters = facet.Selection{}.With(facet.FacetExtension, "jpg")

	pngResult := QueryResult{TotalCount: 3, Hits: []Hit{{Type: HitTypeAsset, Title: "png row"}}}
	c.Put(png, pngResult)

	if _, ok := c.Get(jpg); ok {
		t.Fatal("a DIFFERENT filter selection hit the cached entry for `extension: png` — " +
			"same q, same caller, two result sets, one key")
	}
	if _, ok := c.Get(base); ok {
		t.Fatal("the UNFILTERED query hit the cached entry for `extension: png` — the " +
			"whole page would come back narrowed")
	}
	got, ok := c.Get(png)
	if !ok {
		t.Fatal("the identical query missed its own cache entry")
	}
	if got.TotalCount != pngResult.TotalCount {
		t.Errorf("cached total = %d, want %d", got.TotalCount, pngResult.TotalCount)
	}
	// Tick order must not cost a hit.
	a := base
	a.Filters = facet.Selection{}.With(facet.FacetTag, "x").With(facet.FacetExtension, "png")
	b := base
	b.Filters = facet.Selection{}.With(facet.FacetExtension, "png").With(facet.FacetTag, "x")
	c.Put(a, QueryResult{TotalCount: 1})
	if _, ok := c.Get(b); !ok {
		t.Error("the same two ticks in the other order missed the cache")
	}
}

func ptrInt64(v int64) *int64 { return &v }
