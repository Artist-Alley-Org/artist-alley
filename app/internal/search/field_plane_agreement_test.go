// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1064 + #1056 — ONE CALLER, ONE PLANE, THREE SURFACES.
//
// ADR 0064: *"a capability that permits mutation confers FIELD-plane
// readability for the objects it governs. It never confers the binary
// plane."* A team-scoped `assets.admin` holder may therefore read the
// TITLE, the EXTENSION and the SENSITIVITY of the assets they
// administer — and may not download them.
//
// #902 taught /search that rule: the match conjunct is
// visibility.AssetSearchMatchSQL, which is `search_text @@ q AND
// FieldsReadableSQL`. Two neighbouring surfaces were left on the
// CONTENT plane — the bytes plane — and so disagreed with it:
//
//   - #1064, /search/suggest: the asset-title source composed
//     ContentReadableSQL, so the holder typed six letters of a title
//     they can read on the asset page, got nothing, pressed Enter, and
//     /search returned the row.
//   - #1056, /search/facets AND the Engine's own filter conjunct: the
//     aggregators composed ContentReadableSQL, and runAssets was pinned
//     to match them on purpose (its comment said so) rather than
//     diverge. So the holder was counted out of the rail.
//
// THE INVARIANT THAT FORCED THEM INTO ONE CHANGE: the number on the rail
// must equal the size of the result set that ticking it returns. Widen
// the Engine alone and `png 1` returns 2; widen the aggregators alone
// and `ogg 2` returns 1. Both are #907's defect in a form that looks
// plausible from either side, which is why the equality is asserted
// here PER CALLER rather than in aggregate.
//
// Every assertion is driven as a positive control: the holder and a
// stranger get OPPOSITE verdicts on the SAME row, in the same call. A
// test where both get the same answer proves only that the query ran.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	fpOwner    int64 = 10641101
	fpHolder   int64 = 10641102 // team-scoped assets.admin over fpTeam
	fpStranger int64 = 10641103 // no relationship, no capability
)

// fpPhrase appears only in this fixture's titles, so a hit, a count or a
// completion is attributable to these rows and to nothing else in any
// developer's database.
const fpPhrase = "brindlequoth"

// fpFixture is the corpus. Two extensions so the facet filter actually
// NARROWS — an equality asserted against a filter that removes nothing
// is satisfied by ignoring the filter.
type fpAssets struct {
	team uuid.UUID
	// restrictedOgg and restrictedPng are on the team the holder
	// administers; publicOgg is readable by everyone.
	restrictedOgg, restrictedPng, publicOgg uuid.UUID
}

func fpSeed(t *testing.T, pool *pgxpool.Pool) fpAssets {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		fpOwner, "fp-owner-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, fpOwner)
	})

	team := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, name, slug) VALUES ($1, $2, $3)`,
		team, "fp-team-"+team.String()[:8], "fp-"+team.String()[:8]); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	seed := func(title, sensitivity, ext string, onTeam bool) uuid.UUID {
		id := uuid.New()
		var teamID any
		if onTeam {
			teamID = team
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, team_id, file_extension)
			VALUES ($1,$2,$3,$4,(SELECT MIN(ref) FROM asset_types),'active',$5,'ready',$6,$7)`,
			id, title, "fixture body", fpOwner, sensitivity, teamID, ext); err != nil {
			t.Fatalf("seed asset %q: %v", title, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
		})
		return id
	}

	f := fpAssets{
		team:          team,
		restrictedOgg: seed(fpPhrase+" embargoed boss theme", "restricted", "ogg", true),
		restrictedPng: seed(fpPhrase+" embargoed concept sheet", "restricted", "png", true),
		publicOgg:     seed(fpPhrase+" published splash cue", "public", "ogg", false),
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM team_memberships WHERE team_id = $1`, team)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id = $1`, team)
	})
	return f
}

// fpCaller is one caller class: a ref plus the two capability structs
// that decide the plane.
type fpCaller struct {
	name string
	ref  *int64
	caps visibility.ContentCaps
	mut  visibility.AssetMutationCaps
}

func fpCallers(f fpAssets) []fpCaller {
	holder, stranger, owner := fpHolder, fpStranger, fpOwner
	return []fpCaller{
		// THE CALLER THIS SPRINT IS ABOUT. No content-plane capability
		// at all — only `assets.admin` scoped to this team.
		{"team-scoped assets.admin", &holder, visibility.ContentCaps{},
			visibility.AssetMutationCaps{Teams: []uuid.UUID{f.team}}},
		// The negative control, identical in every other respect.
		{"a stranger", &stranger, visibility.ContentCaps{}, visibility.AssetMutationCaps{}},
		// The owner, who has always been on the readable side and whose
		// answers must not move.
		{"the owner", &owner, visibility.ContentCaps{}, visibility.AssetMutationCaps{}},
	}
}

// fpWantReadable states, independently of any surface, which fixture
// rows each caller may read the FIELDS of. Stated here rather than
// derived, so "every surface is wrong in the same direction" fails.
func fpWantReadable(f fpAssets, c fpCaller) map[uuid.UUID]bool {
	switch c.name {
	case "a stranger":
		return map[uuid.UUID]bool{f.restrictedOgg: false, f.restrictedPng: false, f.publicOgg: true}
	default: // the holder (via ADR 0064) and the owner (via ownership)
		return map[uuid.UUID]bool{f.restrictedOgg: true, f.restrictedPng: true, f.publicOgg: true}
	}
}

// ── the three surfaces ────────────────────────────────────────────────

func fpSearched(t *testing.T, pool *pgxpool.Pool, c fpCaller, sel facet.Selection) QueryResult {
	t.Helper()
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          fpPhrase,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		CallerUserRef: c.ref,
		Caps:          c.caps,
		MutationCaps:  c.mut,
		Filters:       sel,
	})
	if err != nil {
		t.Fatalf("search as %s: %v", c.name, err)
	}
	return res
}

func fpHitSet(res QueryResult) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, h := range res.Hits {
		out[h.ID] = true
	}
	return out
}

// fpCompleted is /search/suggest, asset-title source only.
func fpCompleted(t *testing.T, pool *pgxpool.Pool, c fpCaller) []string {
	t.Helper()
	resp, err := suggest.NewService(pool).Suggest(context.Background(), suggest.Request{
		Prefix:       fpPhrase,
		Caller:       visibility.NewCaller(c.ref),
		Caps:         c.caps,
		MutationCaps: c.mut,
		Limit:        suggest.MaxResults,
	})
	if err != nil {
		t.Fatalf("suggest as %s: %v", c.name, err)
	}
	out := []string{}
	for _, s := range resp.Suggestions {
		if s.Kind == suggest.KindAssetTitle {
			out = append(out, s.Value)
		}
	}
	sort.Strings(out)
	return out
}

// fpFaceted is /search/facets, as facet type → value → count.
func fpFaceted(t *testing.T, pool *pgxpool.Pool, c fpCaller, sel facet.Selection) map[string]map[string]int64 {
	t.Helper()
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	resp := d.Run(context.Background(), facet.Request{
		QueryText:    fpPhrase,
		Selection:    sel,
		Caller:       visibility.NewCaller(c.ref),
		Caps:         c.caps,
		MutationCaps: c.mut,
	})
	out := map[string]map[string]int64{}
	for name, res := range resp.Facets {
		m := map[string]int64{}
		for _, b := range res.Buckets {
			m[b.Value] = b.Count
		}
		out[string(name)] = m
	}
	return out
}

// TestFieldPlane_SuggestAgreesWithSearch is #1064 stated as the bug: the
// disagreement itself, not a list of expectations.
//
// For every caller, a title completes iff /search returns the row it
// belongs to. The `want` map sits beside both so the two surfaces
// agreeing WRONGLY still fails.
func TestFieldPlane_SuggestAgreesWithSearch(t *testing.T) {
	pool := coPool(t)
	f := fpSeed(t, pool)

	titles := map[uuid.UUID]string{
		f.restrictedOgg: "boss theme",
		f.restrictedPng: "concept sheet",
		f.publicOgg:     "splash cue",
	}

	for _, c := range fpCallers(f) {
		t.Run(c.name, func(t *testing.T) {
			want := fpWantReadable(f, c)
			hits := fpHitSet(fpSearched(t, pool, c, facet.Selection{}))
			completions := strings.Join(fpCompleted(t, pool, c), " | ")

			for id, fragment := range titles {
				completed := strings.Contains(completions, fragment)

				if hits[id] != want[id] {
					t.Errorf("SEARCH: %q returned=%v, want %v — the #902 match gate itself "+
						"disagrees with ADR 0064, so the agreement below proves nothing",
						fragment, hits[id], want[id])
				}
				if completed != want[id] {
					if want[id] {
						t.Errorf("SUGGEST: %q did not complete for a caller who may read that "+
							"title (search returned it: %v). This is #1064 — the completion "+
							"was on the CONTENT plane while the title it completes is a "+
							"FIELD, so typing the name of an asset you administer offered "+
							"nothing and pressing Enter found it",
							fragment, hits[id])
					} else {
						t.Errorf("SUGGEST: %q completed for a caller who may NOT read it — "+
							"a prefix endpoint that completes a withheld title hands it "+
							"back letter by letter (#899)", fragment)
					}
				}
			}
		})
	}
}

// TestFieldPlane_FacetCountEqualsFilteredResult is #1056's invariant,
// asserted per caller because the callers legitimately see different
// numbers and an equality that held for only one of them would hide
// exactly the drift this sprint exists to prevent.
//
// Three properties, in order of what they catch:
//
//  1. the filtered set is the unfiltered set NARROWED — never widened,
//     and never narrowed by readability for rows the caller can read;
//  2. the rail's count equals that filtered set's size;
//  3. the holder and the stranger disagree about the same bucket.
func TestFieldPlane_FacetCountEqualsFilteredResult(t *testing.T) {
	pool := coPool(t)
	f := fpSeed(t, pool)

	for _, c := range fpCallers(f) {
		t.Run(c.name, func(t *testing.T) {
			want := fpWantReadable(f, c)
			unfiltered := fpHitSet(fpSearched(t, pool, c, facet.Selection{}))
			counts := fpFaceted(t, pool, c, facet.Selection{})

			for _, ext := range []string{"ogg", "png"} {
				sel := facet.Selection{}.With(facet.FacetExtension, ext)
				filtered := fpSearched(t, pool, c, sel)
				got := fpHitSet(filtered)

				// (1) The filter narrows the caller's OWN result set. Any
				// row it returns must already have been in the unfiltered
				// set — a filter that admits a row the unfiltered search
				// withheld is a readability hole with a query string in
				// front of it.
				for id := range got {
					if !unfiltered[id] {
						t.Errorf("extension:%s returned a row the caller's UNFILTERED search "+
							"did not: %v", ext, id)
					}
				}
				// ...and every readable row of that extension must survive
				// it, or the filter is narrowing by readability twice.
				for id, readable := range want {
					inExt := (id == f.restrictedOgg || id == f.publicOgg) == (ext == "ogg")
					if readable && inExt && !got[id] {
						t.Errorf("extension:%s LOST %v, which this caller may read and which "+
							"their unfiltered search returned (%v)", ext, id, unfiltered[id])
					}
				}

				// (2) THE #907 INVARIANT. The rail says N; ticking it
				// returns N.
				if int(counts["extension"][ext]) != filtered.TotalCount {
					t.Errorf("the `extension: %s` bucket says %d but ticking it returns %d — "+
						"the facet count and the filter are on different readability "+
						"planes, which is #907 restored in the subtle form #1056 "+
						"existed to prevent",
						ext, counts["extension"][ext], filtered.TotalCount)
				}
			}

			// (3) The sensitivity rail, which is the dimension that forced
			// the #899 judgement and the sharpest statement of the plane:
			// `restricted 2` tells the caller these items' TIER.
			wantRestricted := int64(2)
			if c.name == "a stranger" {
				wantRestricted = 0
			}
			if n := counts["sensitivity"]["restricted"]; n != wantRestricted {
				t.Errorf("sensitivity:restricted counted %d, want %d — the rail must count "+
					"exactly the rows whose fields this caller may read",
					n, wantRestricted)
			}
		})
	}
}

// TestFieldPlane_MutationScopeIsScoped is the fence around the widening.
//
// #1056 hands a capability holder rows they could not see before, so the
// test that matters most is the one proving the grant is BOUNDED. An
// `assets.admin` scoped to team X must not read team Y's restricted
// assets — if it did, "team-scoped" would be a comment rather than a
// gate, and the sprint would have shipped an escalation.
func TestFieldPlane_MutationScopeIsScoped(t *testing.T) {
	pool := coPool(t)
	f := fpSeed(t, pool)

	holder := fpHolder
	// Scoped to a team that exists but is NOT this fixture's team.
	elsewhere := fpCaller{
		name: "assets.admin scoped elsewhere",
		ref:  &holder,
		mut:  visibility.AssetMutationCaps{Teams: []uuid.UUID{uuid.New()}},
	}

	hits := fpHitSet(fpSearched(t, pool, elsewhere, facet.Selection{}))
	if hits[f.restrictedOgg] || hits[f.restrictedPng] {
		t.Error("SEARCH: an assets.admin scoped to ANOTHER team matched this team's " +
			"restricted assets — the mutation disjunct is not comparing team_id")
	}
	if got := fpCompleted(t, pool, elsewhere); strings.Contains(strings.Join(got, " | "), "boss theme") {
		t.Errorf("SUGGEST: an assets.admin scoped to another team completed this team's "+
			"restricted title: %v", got)
	}
	if n := fpFaceted(t, pool, elsewhere, facet.Selection{})["sensitivity"]["restricted"]; n != 0 {
		t.Errorf("FACET: an assets.admin scoped to another team counted %d restricted "+
			"rows, want 0", n)
	}

	// And the same caller, scoped to the RIGHT team, sees all three —
	// so the assertions above are about the SCOPE and not about the
	// capability being inert.
	right := fpCaller{
		name: "assets.admin scoped here",
		ref:  &holder,
		mut:  visibility.AssetMutationCaps{Teams: []uuid.UUID{f.team}},
	}
	if !fpHitSet(fpSearched(t, pool, right, facet.Selection{}))[f.restrictedOgg] {
		t.Error("the correctly-scoped holder does not match the asset either, so the " +
			"negative assertions above prove nothing")
	}
}
