// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1117 — every derived copy of a mature asset is withheld from a
// viewer who does not qualify (ADR 0090 §5).
//
// # Why a WITHHELD-VS-CONTROL PAIR per channel, and not an assertion
//
// A single assertion — "the disqualified viewer does not get the mature
// row" — passes on a gate that refuses everyone, which is the failure
// mode of every conjunct that folds to FALSE by accident. It also passes
// on a channel that returns nothing at all because the fixture never
// reached it, which is the failure mode that matters more here: five of
// these channels are separate SQL statements in four packages, and a
// test that only ever asserts absence cannot tell "correctly withheld"
// from "never ran".
//
// So each channel gets TWO rows that are identical in every respect the
// channel can see — same owner, same tier, same status, same distinctive
// token in the title, same tag — and differ only in `mature`. The
// disqualified viewer must receive exactly the control; the qualified
// viewer must receive both. The control is what proves the query ran and
// the token matched; the pair is what proves the gate discriminates on
// the axis under test and not on something incidental.
//
// # Why the arms are (disqualified, qualified) and not (anon, signed-in)
//
// Because the interesting failure is the one that survives being signed
// in. Anonymous is disqualified by the FIRST conjunct and would pass
// even a gate that had only implemented that one; opted-out-signed-in
// and opted-in-signed-in differ by the SECOND, which is the conjunct
// every channel here composes through MatureFilterSQL. The anonymous and
// instance-off arms are covered by the predicate's own table test
// (visibility.TestMatureFilterSQL_MatchesGo) driving all six inputs.
//
// Skips without AA_DB_PASSWORD, like every other integration test here.

package search

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Synthetic refs, outside any seeded range. Nothing here needs a row in
// "user": assets.owner_user_ref and posts.author_user_ref carry no FK
// (federation-friendly by design), which is also what keeps this fixture
// out of every other suite's way.
const (
	mcOwner    int64 = 11170001 // owns both assets — so ownership cannot explain a verdict
	mcStranger int64 = 11170002 // signed in, no relationship, no capabilities
)

// mcToken is a nonsense word that occurs nowhere else in the corpus, so
// a hit can only have come from this fixture. `similarity()` needs it to
// be a plausible word rather than a hash, which is why it reads like one.
const mcToken = "zarquonfluxweave"

// The two arms. Both are SIGNED IN and both are on an instance that
// ALLOWS mature content; they differ only in the opt-in, which is the
// conjunct every channel under test composes.
var (
	mcDisqualified = visibility.MatureViewer{SignedIn: true, OptedIn: false, InstanceAllows: true}
	mcQualified    = visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}
)

func mcPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mcFixture plants the pair: two public, active, non-restricted assets
// carrying the same token and the same tag, differing only in `mature`,
// each wrapped in its own public post so the post-plane channels get a
// pair too.
//
// PUBLIC and ACTIVE deliberately. The mature axis has to be shown
// working on content the viewer is fully ENTITLED to — that is ADR 0090
// §1's whole point, that a public artwork can be mature. Seeding the
// mature one as `restricted` would let the row predicate do the hiding
// and the test would pass with no mature conjunct anywhere.
type mcFixture struct {
	matureAsset, controlAsset uuid.UUID
	maturePost, controlPost   uuid.UUID
}

func mcSeed(t *testing.T, pool *pgxpool.Pool) mcFixture {
	t.Helper()
	ctx := context.Background()
	f := mcFixture{
		matureAsset: uuid.New(), controlAsset: uuid.New(),
		maturePost: uuid.New(), controlPost: uuid.New(),
	}
	type row struct {
		asset, post uuid.UUID
		mature      bool
		label       string
	}
	rows := []row{
		{f.matureAsset, f.maturePost, true, "mature"},
		{f.controlAsset, f.controlPost, false, "control"},
	}
	for _, r := range rows {
		title := mcToken + " " + r.label
		if _, err := pool.Exec(ctx,
			`INSERT INTO assets (id, title, description, asset_type, owner_user_ref,
			                     status, sensitivity, processing_status, mature)
			 VALUES ($1, $2, $2, 1, $3, 'active', 'public', 'ready', $4)`,
			r.asset, title, mcOwner, r.mature); err != nil {
			t.Fatalf("seed asset (%s): %v", r.label, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO asset_tag (asset_id, tag) VALUES ($1, $2)`,
			r.asset, mcToken+"-"+r.label); err != nil {
			t.Fatalf("seed asset tag (%s): %v", r.label, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO posts (id, author_user_ref, title, description, visibility)
			 VALUES ($1, $2, $3, $3, 'public')`,
			r.post, mcOwner, title); err != nil {
			t.Fatalf("seed post (%s): %v", r.label, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_tags (post_id, tag) VALUES ($1, $2)`,
			r.post, mcToken+"-"+r.label); err != nil {
			t.Fatalf("seed post tag (%s): %v", r.label, err)
		}
		// The membership link is what makes posts.mature DERIVE — the
		// trigger, not this test, decides the post's flag. Asserted
		// below rather than assumed, because a fixture that set the post
		// flag by hand would prove nothing about the derivation.
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, 0)`,
			r.post, r.asset); err != nil {
			t.Fatalf("link (%s): %v", r.label, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, r := range rows {
			_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, r.post)
			_, _ = pool.Exec(c, `DELETE FROM post_tags WHERE post_id = $1`, r.post)
			_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, r.post)
			_, _ = pool.Exec(c, `DELETE FROM asset_tag WHERE asset_id = $1`, r.asset)
			_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, r.asset)
		}
	})

	// The PERSISTED derived value, straight out of Postgres (#946). If
	// the trigger did not fire, every post-plane arm below would pass for
	// the wrong reason — the post would simply not be flagged.
	var maturePostFlag, controlPostFlag bool
	if err := pool.QueryRow(ctx, `SELECT mature FROM posts WHERE id = $1`,
		f.maturePost).Scan(&maturePostFlag); err != nil {
		t.Fatalf("read maturePost.mature: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT mature FROM posts WHERE id = $1`,
		f.controlPost).Scan(&controlPostFlag); err != nil {
		t.Fatalf("read controlPost.mature: %v", err)
	}
	if !maturePostFlag {
		t.Fatal("the post holding the mature asset is not flagged mature: the derivation " +
			"trigger did not fire, so every post-plane arm below would pass vacuously")
	}
	if controlPostFlag {
		t.Fatal("the control post is flagged mature: the derivation is over-firing")
	}
	return f
}

// mcQuery builds a Query for one arm. Everything except the mature axis
// is identical between the two, which is what makes the difference in
// the results attributable.
func mcQuery(v visibility.MatureViewer) Query {
	ref := mcStranger
	return Query{
		Text:          mcToken,
		Types:         AllHitTypes(),
		Limit:         50,
		CallerUserRef: &ref,
		Mature:        v,
	}
}

// mcHitTitles collects the hit titles for one arm.
func mcHitTitles(t *testing.T, e *Engine, v visibility.MatureViewer) []string {
	t.Helper()
	res, err := e.Run(context.Background(), mcQuery(v))
	if err != nil {
		t.Fatalf("engine run: %v", err)
	}
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.Title)
	}
	return out
}

func mcHasLabel(titles []string, label string) bool {
	for _, s := range titles {
		if strings.Contains(s, mcToken) && strings.Contains(s, label) {
			return true
		}
	}
	return false
}

// mcAssertPair is the shared shape of every channel arm below.
//
// It takes the two result sets and asserts the four facts that together
// mean "this channel gates on the mature axis and on nothing else":
// the control is present for BOTH viewers (so the channel ran and the
// token matched), and the mature row is present for the qualified viewer
// only.
func mcAssertPair(t *testing.T, channel string, disq, qual []string) {
	t.Helper()
	if !mcHasLabel(disq, "control") {
		t.Errorf("%s: the CONTROL row is missing for the disqualified viewer. "+
			"The channel is either not running or is gating on something other "+
			"than mature — an absence-only assertion would have called this a pass", channel)
	}
	if !mcHasLabel(qual, "control") {
		t.Errorf("%s: the CONTROL row is missing for the qualified viewer", channel)
	}
	if mcHasLabel(disq, "mature") {
		t.Errorf("%s: LEAK — the mature row answered a disqualified viewer. "+
			"Its words are a derived copy of content this reader did not opt into "+
			"(ADR 0090 §5)", channel)
	}
	if !mcHasLabel(qual, "mature") {
		t.Errorf("%s: the mature row is missing for a QUALIFIED viewer. The conjunct "+
			"is refusing everyone, which every absence-only test would pass", channel)
	}
}

// TestMatureChannels_SearchText covers the /search text channel for both
// entity planes in one pass — the Engine runs assets, posts and
// collections together, so the hit list carries both pairs and the
// assertion is over the union.
func TestMatureChannels_SearchText(t *testing.T) {
	pool := mcPool(t)
	mcSeed(t, pool)
	e := NewEngine(pool)
	mcAssertPair(t, "/search search_text",
		mcHitTitles(t, e, mcDisqualified),
		mcHitTitles(t, e, mcQualified))
}

// TestMatureChannels_TotalCount is the half a hits-only test misses.
//
// `total_count` is its own SQL statement. If the conjunct reaches the
// hits query and not the count, a disqualified viewer sees the number
// move 0→1 for a phrase that occurs only in a mature asset's title and
// has recovered the fact of its existence without ever being handed the
// row — #902's oracle, on a second axis.
func TestMatureChannels_TotalCount(t *testing.T) {
	pool := mcPool(t)
	mcSeed(t, pool)
	e := NewEngine(pool)

	count := func(v visibility.MatureViewer) int {
		res, err := e.Run(context.Background(), mcQuery(v))
		if err != nil {
			t.Fatalf("engine run: %v", err)
		}
		return res.TotalCount
	}
	disq, qual := count(mcDisqualified), count(mcQualified)
	if disq == 0 {
		t.Fatal("total_count is 0 for the disqualified viewer: the control did not " +
			"match, so this test proves nothing about the mature rows")
	}
	if qual <= disq {
		t.Errorf("total_count did not grow for the qualified viewer (disqualified=%d, "+
			"qualified=%d): the count statement is not composing the mature conjunct, "+
			"or is composing it for everyone", disq, qual)
	}
	if got, want := qual-disq, 2; got != want {
		t.Errorf("total_count grew by %d, want %d (the mature asset and its post). "+
			"A different delta means the hits query and the count query disagree "+
			"about what the mature conjunct excludes", got, want)
	}
}

// TestMatureChannels_CacheKey is the leak that needs two callers and a
// warm cache to exist at all, so no single-caller test can see it.
//
// Without the axis in the key, the first arm's page is served verbatim
// to the second and the gate is bypassed entirely for the TTL.
func TestMatureChannels_CacheKey(t *testing.T) {
	a := keyForQuery(mcQuery(mcDisqualified))
	b := keyForQuery(mcQuery(mcQualified))
	if a == b {
		t.Fatal("two viewers who differ ONLY on the mature axis produce the same cache " +
			"key: an opted-in reader's cached page will be served to an opted-out one, " +
			"and every result below is decided by whoever asked first")
	}
	// The instance switch is the operator's, so it moves for every caller
	// at once — a cache that ignored it would keep serving mature rows on
	// an install whose operator has just switched the feature off.
	off := mcQualified
	off.InstanceAllows = false
	if keyForQuery(mcQuery(off)) == b {
		t.Error("the instance switch is not in the cache key: turning mature content " +
			"off would leave every warm entry serving it until the TTL expired")
	}
}

// TestMatureChannels_Facets — the rail's counts.
//
// A facet bucket is a derived copy in the purest form: `zarquon-mature 1`
// tells a disqualified viewer both that the tag exists and how much is
// filed under it, without handing over a single row.
func TestMatureChannels_Facets(t *testing.T) {
	pool := mcPool(t)
	mcSeed(t, pool)
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	tags := func(v visibility.MatureViewer) []string {
		resp := d.Run(context.Background(), facet.Request{
			QueryText: mcToken,
			Facets:    []facet.FacetType{facet.FacetTag},
			Caller:    visibility.NewCaller(&[]int64{mcStranger}[0]),
			Mature:    v,
		})
		out := []string{}
		for _, b := range resp.Facets[facet.FacetTag].Buckets {
			out = append(out, b.Value)
		}
		return out
	}
	mcAssertPair(t, "/search/facets tag buckets", tags(mcDisqualified), tags(mcQualified))
}

// TestMatureChannels_Suggest — three of the four sources.
//
// `collections` is deliberately absent and that is a decision, not a
// gap: a collection name is typed by a curator rather than derived from
// any member's content, and `collections` carries no mature column
// because there is nothing to derive one from. See suggest.Request.Mature.
func TestMatureChannels_Suggest(t *testing.T) {
	pool := mcPool(t)
	mcSeed(t, pool)
	svc := suggest.NewService(pool)

	values := func(v visibility.MatureViewer) []string {
		resp, err := svc.Suggest(context.Background(), suggest.Request{
			Prefix: mcToken,
			Caller: visibility.NewCaller(&[]int64{mcStranger}[0]),
			Mature: v,
			Limit:  50,
		})
		if err != nil {
			t.Fatalf("suggest: %v", err)
		}
		out := make([]string, 0, len(resp.Suggestions))
		for _, s := range resp.Suggestions {
			out = append(out, s.Value)
		}
		return out
	}
	mcAssertPair(t, "/search/suggest (tags + post titles + asset titles)",
		values(mcDisqualified), values(mcQualified))
}
