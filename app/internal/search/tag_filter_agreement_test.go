// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1251 — THE GUARD THE DUPLICATION LACKED.
//
// The epic's first acceptance bullet: "One definition per filter. A test
// asserts feed and search return the SAME set for the same filter — the
// guard the current duplication lacks."
//
// Until slice 2 the tag predicate existed TWICE — the feed built
// `EXISTS (SELECT 1 FROM post_tags pt WHERE pt.post_id = posts.id AND
// pt.tag = $5)` and facet.dimensionSQL built the byte-identical
// expression for EntityPost. The issue is explicit that this was
// duplication rather than a bug ("checked precisely, they are the same
// rule"), and equally explicit about the risk: "two implementations that
// agree today and have no test asserting they must keep agreeing."
//
// ⚠️ AND A SAMENESS TEST BETWEEN TWO COPIES CANNOT DETECT A SHARED WRONG
// RULE — that is what makes this test worth writing only NOW. After the
// convergence there is one expression, so agreement is structural and
// this asserts that the WIRING reaches it: the feed's `?tag=`, the
// Engine's `filter=tag:` and the rail's bucket all select the same posts
// for the same caller. A regression here means somebody re-introduced a
// second implementation, which is the thing ADR 0093 forbids.
//
// The independent oracle beside it is the FIXTURE: each post's tag
// membership is stated in the table below, per caller, independently of
// every surface — so all three surfaces being wrong in the same
// direction still fails.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/posts"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	tfaAuthor   int64 = 12511201
	tfaStranger int64 = 12511202
)

// tfaPhrase is in every fixture post's title, so /search has text to
// rank on while the FILTER is what decides membership.
const tfaPhrase = "brelthaquon"

const (
	tfaAlpha = "tfa-alpha"
	tfaBeta  = "tfa-beta"
)

// tfaFixture is the intent oracle: what each post carries and who may
// read it, stated once and read by every surface.
var tfaFixture = []struct {
	name     string
	tier     string
	tags     []string
	readable bool // by the STRANGER (the author reads all of them)
}{
	{"public-both", "public", []string{tfaAlpha, tfaBeta}, true},
	{"public-alpha", "public", []string{tfaAlpha}, true},
	{"public-beta", "public", []string{tfaBeta}, true},
	{"public-none", "public", nil, true},
	// ⭐ The private post carrying BOTH tags. It is what makes the
	// agreement worth asserting: a surface that composed the tag filter
	// without its read rule would return it to the stranger, and it is
	// the only fixture row that separates "the filter ran" from "the
	// filter ran beside the gate".
	{"private-both", "private", []string{tfaAlpha, tfaBeta}, false},
}

func tfaSeed(t *testing.T, pool *pgxpool.Pool) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ids := map[string]uuid.UUID{}
	for _, p := range tfaFixture {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO posts (id, author_user_ref, title, description, visibility)
			VALUES ($1,$2,$3,'fixture body',$4)`,
			id, tfaAuthor, tfaPhrase+" "+p.name, p.tier); err != nil {
			t.Fatalf("seed %s: %v", p.name, err)
		}
		for _, tag := range p.tags {
			if _, err := pool.Exec(ctx,
				`INSERT INTO post_tags (post_id, tag) VALUES ($1,$2)`, id, tag); err != nil {
				t.Fatalf("seed %s tag: %v", p.name, err)
			}
		}
		ids[p.name] = id
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM post_tags WHERE post_id=$1`, id)
			_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, id)
		})
	}
	return ids
}

// tfaBrowsed is the FEED: posts.ListPostsPageGated, the browse query
// itself rather than a restatement of it.
func tfaBrowsed(t *testing.T, pool *pgxpool.Pool, id *auth.Identity, tags ...string) map[uuid.UUID]bool {
	t.Helper()
	h := &posts.Handler{Pool: pool}
	rows, err := h.ListPostsPageGated(context.Background(), id, posts.ListPostsPageParams{
		Tags: tags, RowLimit: 500,
	})
	if err != nil {
		t.Fatalf("browse (tags=%v): %v", tags, err)
	}
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[uuid.UUID(r.ID.Bytes)] = true
	}
	return out
}

// tfaSearched is /search restricted to posts, with the same tags applied
// through the `filter=tag:` grammar.
func tfaSearched(t *testing.T, pool *pgxpool.Pool, ref *int64, tags ...string) map[uuid.UUID]bool {
	t.Helper()
	sel := facet.Selection{}
	for _, tag := range tags {
		sel = sel.With(facet.FacetTag, tag)
	}
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          tfaPhrase,
		Types:         []HitType{HitTypePost},
		Limit:         MaxLimit,
		CallerUserRef: ref,
		Filters:       sel,
	})
	if err != nil {
		t.Fatalf("search (tags=%v): %v", tags, err)
	}
	out := map[uuid.UUID]bool{}
	for _, h := range res.Hits {
		out[h.ID] = true
	}
	return out
}

// TestTagFilter_FeedAndSearchSelectTheSameSet is the epic's guard.
//
// Every cardinality the grammar admits — none, one, two — for a caller
// who reads everything and one who reads only the public tier. The
// per-surface answers must be EQUAL to each other and equal to the
// fixture's own statement of what should be there.
func TestTagFilter_FeedAndSearchSelectTheSameSet(t *testing.T) {
	pool := coPool(t)
	ids := tfaSeed(t, pool)

	stranger := tfaStranger
	author := tfaAuthor
	for _, c := range []struct {
		name string
		id   *auth.Identity
		ref  *int64
		all  bool // reads every tier of this fixture
	}{
		{"the author", &auth.Identity{UserRef: tfaAuthor, AuthMethod: "session"}, &author, true},
		{"a stranger", &auth.Identity{UserRef: tfaStranger, AuthMethod: "session"}, &stranger, false},
		{"anonymous", nil, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, sel := range [][]string{
				nil,
				{tfaAlpha},
				{tfaBeta},
				{tfaAlpha, tfaBeta},
				{tfaAlpha, "tfa-nobody-has-this"},
			} {
				browsed := tfaBrowsed(t, pool, c.id, sel...)
				searched := tfaSearched(t, pool, c.ref, sel...)

				for _, p := range tfaFixture {
					id := ids[p.name]
					// The INTENT ORACLE, computed from the fixture and
					// from nothing either surface returned.
					want := (c.all || p.readable) && tfaCarriesAll(p.tags, sel)

					if browsed[id] != want {
						t.Errorf("tags=%v: the FEED %s %q, want %v (tier=%s tags=%v)",
							sel, tfaVerb(browsed[id]), p.name, want, p.tier, p.tags)
					}
					if searched[id] != want {
						t.Errorf("tags=%v: /SEARCH %s %q, want %v (tier=%s tags=%v)",
							sel, tfaVerb(searched[id]), p.name, want, p.tier, p.tags)
					}
					if browsed[id] != searched[id] {
						t.Errorf("tags=%v: the feed and /search DISAGREE about %q "+
							"(feed=%v search=%v). One definition per filter — a "+
							"divergence here means a second implementation is back",
							sel, p.name, browsed[id], searched[id])
					}
				}
			}
		})
	}
}

// TestTagFilter_RailCountEqualsWhatTickingItReturns is #907's invariant
// re-asserted for the dimension this slice moved: a bucket says "N rows
// carry this value", and ticking it must return those N.
//
// It is asserted on the FIXTURE'S OWN CONTRIBUTION rather than on the
// bucket's absolute count, because a developer's seeded corpus may carry
// the same tag — which is why the tags are nonsense strings and why the
// comparison is between the two surfaces rather than against a literal.
func TestTagFilter_RailCountEqualsWhatTickingItReturns(t *testing.T) {
	pool := coPool(t)
	ids := tfaSeed(t, pool)

	stranger := tfaStranger
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	resp := d.Run(context.Background(), facet.Request{
		QueryText: tfaPhrase,
		Facets:    []facet.FacetType{facet.FacetTag},
		Caller:    visibility.NewCaller(&stranger),
	})
	counts := map[string]int64{}
	for _, b := range resp.Facets[facet.FacetTag].Buckets {
		counts[b.Value] = b.Count
	}
	if counts[tfaAlpha] == 0 {
		t.Fatalf("the rail counted no %q bucket for the stranger — the comparison "+
			"below would be vacuous", tfaAlpha)
	}

	// The stranger reads three public posts in this fixture; two carry
	// alpha and two carry beta, and the private post carrying BOTH is
	// invisible to them. So the bucket and the result set must agree at
	// 2 for each tag, not at 3.
	for _, tag := range []string{tfaAlpha, tfaBeta} {
		got := tfaSearched(t, pool, &stranger, tag)
		n := 0
		for _, p := range tfaFixture {
			if got[ids[p.name]] {
				n++
			}
		}
		if int64(n) != counts[tag] {
			t.Errorf("the rail says %s=%d and ticking it returns %d of this "+
				"fixture's posts. #907's invariant is that those are one number",
				tag, counts[tag], n)
		}
		if n != 2 {
			t.Errorf("tag=%s returned %d fixture posts for the stranger, want 2 — "+
				"the private post carrying both tags must be in neither", tag, n)
		}
	}
}

func tfaCarriesAll(has, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range has {
			if h == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func tfaVerb(present bool) string {
	if present {
		return "RETURNED"
	}
	return "OMITTED"
}
