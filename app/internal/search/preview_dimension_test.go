// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25a: `preview:missing` (`!nopreviews`) on real rows.
//
// # What "missing" means, driven as rows
//
// A preview exists when a servable `col` variant exists for the asset's
// file hash, and nothing else decides it. The fixture below is built
// to make every wrong definition fail:
//
//   - a `failed` png with no `col`         → MISSING (the plain case)
//   - a `ready` png WITH a `col`           → not missing
//   - a `pending` mp4 WITH a `col`         → not missing: the poster job
//     wrote the thumbnail and deliberately did not touch status, so a
//     status-keyed rule would call this row missing
//   - a `ready` txt with no `col`          → MISSING: the text handler
//     logs a fan failure, continues, and marks the row ready, so a
//     status-keyed rule would call this row fine
//   - a `ready` bin with no `col`          → not missing: nothing could
//     have rendered it, so nothing is missing
//   - a `restricted` png with no `col`     → MISSING for the owner and a
//     content.read.all holder; NOT for a team-scoped assets.admin
//     holder, who may read its fields and not its picture, and not for
//     a stranger
//
// # ⛔ The no-probe assertion is the one that matters
//
// The team-scoped holder is the caller ADR 0064 hands the FIELD plane
// to and withholds the BINARY plane from. Whether a picture exists is a
// binary-plane fact. A dimension gated only by the field plane every
// execution site already applies would answer that holder one bit of
// the picture per query, and this file's positive control, the same
// holder's UNFILTERED search returns the restricted row, is what
// separates "the plane is composed" from "the row is invisible".
//
// # Written against the surface
//
// The queries are DSL strings folded through the package's own bridge
// (foldDSL, which /search/facets and /search/contributors call, and
// which is the same reading /search's applyDSL makes) and run through
// the Engine, so on the commit before this sprint the file compiles and
// `!nopreviews` reaches Postgres as the free-text word "nopreviews":
// every positive assertion below goes red there.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	pvOwner    int64 = 25010101
	pvHolder   int64 = 25010102 // team-scoped assets.admin over pvTeam, not the owner
	pvStranger int64 = 25010103
	pvReader   int64 = 25010104 // content.read.all
)

// pvPhrase is in every fixture title and nowhere else, so a hit or a
// count is attributable to this fixture alone.
const pvPhrase = "quorvellnix"

type pvRow struct {
	id   uuid.UUID
	hash string
	ext  string
}

type pvFixture struct {
	team uuid.UUID
	// See the file comment for what each row is.
	failedNoCol, readyWithCol, pendingPosterCol, readySoftFail, nonPreviewable, restrictedNoCol pvRow
	post                                                                                        uuid.UUID
}

func pvSeed(t *testing.T, pool *pgxpool.Pool) pvFixture {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		pvOwner, "pv-owner-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { testdb.Purge(t, pool, pvOwner, `DELETE FROM "user" WHERE ref = $1`) })

	team := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO teams (id, name, slug) VALUES ($1, $2, $3)`,
		team, "pv-team-"+team.String()[:8], "pv-"+team.String()[:8]); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, team,
			`DELETE FROM team_memberships WHERE team_id = $1`,
			`DELETE FROM teams WHERE id = $1`)
	})

	seed := func(label, ext, status, sensitivity string, onTeam, withCol bool) pvRow {
		id := uuid.New()
		sum := sha256.Sum256([]byte("pv " + label + " " + id.String()))
		hash := hex.EncodeToString(sum[:])
		if _, err := pool.Exec(ctx,
			`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 16, 'fs')`, hash); err != nil {
			t.Fatalf("seed object %s: %v", label, err)
		}
		// The object row is what teardown targets: storage_variants
		// cascades from it, and assets.file_hash is SET NULL, so the
		// asset delete below is ordered after it only for tidiness.
		t.Cleanup(func() {
			testdb.Purge(t, pool, hash, `DELETE FROM storage_objects WHERE hash = $1`)
		})
		var teamID any
		if onTeam {
			teamID = team
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, team_id, file_extension, file_hash)
			VALUES ($1,$2,'fixture body',$3,(SELECT MIN(ref) FROM asset_types),'active',$4,$5,$6,$7,$8)`,
			id, pvPhrase+" "+label, pvOwner, sensitivity, status, teamID, ext, hash); err != nil {
			t.Fatalf("seed asset %s: %v", label, err)
		}
		t.Cleanup(func() { testdb.Purge(t, pool, id, `DELETE FROM assets WHERE id = $1`) })
		if withCol {
			if _, err := pool.Exec(ctx,
				`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 8)`,
				hash); err != nil {
				t.Fatalf("seed col %s: %v", label, err)
			}
		}
		return pvRow{id: id, hash: hash, ext: ext}
	}

	f := pvFixture{
		team:             team,
		failedNoCol:      seed("failed raster", "png", "failed", "public", false, false),
		readyWithCol:     seed("ready raster", "png", "ready", "public", false, true),
		pendingPosterCol: seed("pending video with poster", "mp4", "pending", "public", false, true),
		readySoftFail:    seed("ready text fan failed", "txt", "ready", "public", false, false),
		nonPreviewable:   seed("ready binary blob", "bin", "ready", "public", false, false),
		restrictedNoCol:  seed("restricted concept", "png", "ready", "restricted", true, false),
	}

	// A post carrying the phrase, so a mixed-type query has a post to
	// drop.
	f.post = uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`, f.post, pvOwner, pvPhrase+" post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() { testdb.Purge(t, pool, f.post, `DELETE FROM posts WHERE id = $1`) })
	return f
}

// pvCaller is one caller class.
type pvCaller struct {
	name string
	ref  *int64
	caps visibility.ContentCaps
	mut  visibility.AssetMutationCaps
}

func pvCallers(f pvFixture) []pvCaller {
	owner, holder, stranger, reader := pvOwner, pvHolder, pvStranger, pvReader
	return []pvCaller{
		{"the owner", &owner, visibility.ContentCaps{}, visibility.AssetMutationCaps{}},
		{"a team-scoped assets.admin", &holder, visibility.ContentCaps{},
			visibility.AssetMutationCaps{Teams: []uuid.UUID{f.team}}},
		{"a stranger", &stranger, visibility.ContentCaps{}, visibility.AssetMutationCaps{}},
		{"a content.read.all holder", &reader, visibility.ContentCaps{ContentReadAll: true}, visibility.AssetMutationCaps{}},
		{"anonymous", nil, visibility.ContentCaps{}, visibility.AssetMutationCaps{}},
	}
}

// pvWantMissing states, independently of any surface, which rows each
// caller's `preview:missing` must return.
func pvWantMissing(f pvFixture, c pvCaller) map[uuid.UUID]bool {
	switch c.name {
	case "the owner", "a content.read.all holder":
		return map[uuid.UUID]bool{f.failedNoCol.id: true, f.readySoftFail.id: true, f.restrictedNoCol.id: true}
	case "anonymous":
		// The picture plane's anonymous conjuncts require a ready row,
		// and so does the row plane: the failed png is not visible to an
		// anonymous caller on any surface.
		return map[uuid.UUID]bool{f.readySoftFail.id: true}
	default: // the holder (fields, not picture) and the stranger
		return map[uuid.UUID]bool{f.failedNoCol.id: true, f.readySoftFail.id: true}
	}
}

// pvRun folds a DSL string through the package's bridge and runs it as
// this caller, over these types.
func pvRun(t *testing.T, pool *pgxpool.Pool, c pvCaller, dslInput string, types []HitType) QueryResult {
	t.Helper()
	sel, text, err := foldDSL(dslInput, facet.Selection{}, "")
	if err != nil {
		t.Fatalf("fold %q: %v", dslInput, err)
	}
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          text,
		Types:         types,
		Limit:         100,
		CallerUserRef: c.ref,
		Caps:          c.caps,
		MutationCaps:  c.mut,
		Filters:       sel,
	})
	if err != nil {
		t.Fatalf("run %q as %s: %v", dslInput, c.name, err)
	}
	return res
}

func pvIDs(res QueryResult) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func pvKeys(m map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func pvEqual(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPreviewMissing_MatchesTheDefinition drives the whole fixture as
// every caller, on both spellings, and asserts the EXACT set and that
// the count equals the rows.
func TestPreviewMissing_MatchesTheDefinition(t *testing.T) {
	pool := coPool(t)
	f := pvSeed(t, pool)
	for _, c := range pvCallers(f) {
		t.Run(c.name, func(t *testing.T) {
			want := pvKeys(pvWantMissing(f, c))
			for _, spelling := range []string{pvPhrase + " AND !nopreviews", pvPhrase + " AND preview:missing"} {
				res := pvRun(t, pool, c, spelling, []HitType{HitTypeAsset})
				got := pvIDs(res)
				if !pvEqual(got, want) {
					t.Errorf("%q returned %v, want exactly %v", spelling, got, want)
				}
				if res.TotalCount != len(want) {
					t.Errorf("%q: total_count %d, want %d (the count must equal the rows)", spelling, res.TotalCount, len(want))
				}
			}
		})
	}
}

// TestPreviewMissing_EachCounterexampleIsNamed states the four
// definitional cases one at a time, so a failure names WHICH rule was
// wrong rather than reporting a set difference.
func TestPreviewMissing_EachCounterexampleIsNamed(t *testing.T) {
	pool := coPool(t)
	f := pvSeed(t, pool)
	owner := pvCallers(f)[0]
	got := map[uuid.UUID]bool{}
	for _, id := range pvIDs(pvRun(t, pool, owner, pvPhrase+" AND !nopreviews", []HitType{HitTypeAsset})) {
		got[id] = true
	}
	if !got[f.failedNoCol.id] {
		t.Error("a previewable asset whose preview FAILED (no `col`) is not reported missing")
	}
	if got[f.readyWithCol.id] {
		t.Error("a ready asset WITH a `col` is reported missing")
	}
	if got[f.pendingPosterCol.id] {
		t.Error("a `pending` video whose poster job wrote `col` is reported missing: the predicate " +
			"is keyed on processing_status, which the poster handler deliberately does not touch")
	}
	if !got[f.readySoftFail.id] {
		t.Error("a `ready` text asset whose fan soft-failed (no `col`) is not reported missing: the " +
			"predicate is keyed on processing_status, which the handler marks ready after logging the failure")
	}
	if got[f.nonPreviewable.id] {
		t.Error("a non-previewable .bin with no `col` is reported missing; nothing could have rendered it")
	}
}

// TestPreviewMissing_PicturePlaneNotFieldPlane is the no-probe witness,
// non-vacuous by construction: the holder's unfiltered search RETURNS
// the restricted row (positive control), and `preview:missing` does
// not; the owner gets it on both.
func TestPreviewMissing_PicturePlaneNotFieldPlane(t *testing.T) {
	pool := coPool(t)
	f := pvSeed(t, pool)
	callers := pvCallers(f)
	owner, holder := callers[0], callers[1]

	// Positive control: the holder can read the FIELDS of the restricted
	// asset they administer, so an unfiltered search returns it.
	unfiltered := pvRun(t, pool, holder, pvPhrase, []HitType{HitTypeAsset})
	seen := map[uuid.UUID]bool{}
	for _, id := range pvIDs(unfiltered) {
		seen[id] = true
	}
	if !seen[f.restrictedNoCol.id] {
		t.Fatal("the team-scoped holder's UNFILTERED search does not return the restricted row; " +
			"the no-probe assertion below would be vacuous")
	}

	for _, spelling := range []string{pvPhrase + " AND !nopreviews", pvPhrase + " AND preview:missing"} {
		asHolder := pvRun(t, pool, holder, spelling, []HitType{HitTypeAsset})
		for _, id := range pvIDs(asHolder) {
			if id == f.restrictedNoCol.id {
				t.Errorf("%q as the team-scoped holder returned the restricted row: the caller may read "+
					"its fields and NOT its picture, so answering whether the picture exists hands "+
					"them one bit of the binary plane per query", spelling)
			}
		}
		if asHolder.TotalCount != 2 {
			t.Errorf("%q as the holder: total_count %d, want 2 (the count must not move for the "+
				"row the plane withholds)", spelling, asHolder.TotalCount)
		}
		asOwner := pvRun(t, pool, owner, spelling, []HitType{HitTypeAsset})
		found := false
		for _, id := range pvIDs(asOwner) {
			found = found || id == f.restrictedNoCol.id
		}
		if !found || asOwner.TotalCount != 3 {
			t.Errorf("%q as the owner: restricted row returned=%v, total_count=%d; want true, 3",
				spelling, found, asOwner.TotalCount)
		}
	}
}

// TestPreviewMissing_RailCountEqualsFilteredResult asserts the count
// through the path the rail uses: an extension bucket under the active
// `preview:missing` selection equals the size of the set that ticking
// that bucket returns, per caller.
func TestPreviewMissing_RailCountEqualsFilteredResult(t *testing.T) {
	pool := coPool(t)
	f := pvSeed(t, pool)
	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, c := range pvCallers(f) {
		t.Run(c.name, func(t *testing.T) {
			sel, _, err := foldDSL("!nopreviews", facet.Selection{}, "")
			if err != nil {
				t.Fatal(err)
			}
			resp := d.Run(context.Background(), facet.Request{
				QueryText:    pvPhrase,
				Facets:       []facet.FacetType{facet.FacetExtension},
				Selection:    sel,
				Caller:       visibility.NewCaller(c.ref),
				Caps:         c.caps,
				MutationCaps: c.mut,
			})
			buckets := map[string]int64{}
			for _, b := range resp.Facets[facet.FacetExtension].Buckets {
				buckets[b.Value] = b.Count
			}
			// Every extension the fixture carries, including the ones that
			// must count ZERO under the selection.
			for _, ext := range []string{"png", "mp4", "txt", "bin"} {
				filtered := pvRun(t, pool, c, pvPhrase+" AND !nopreviews AND extension:"+ext, []HitType{HitTypeAsset})
				if int(buckets[ext]) != filtered.TotalCount {
					t.Errorf("extension:%s bucket says %d under preview:missing but ticking it returns %d",
						ext, buckets[ext], filtered.TotalCount)
				}
			}
			// And the sum of the buckets is the caller's missing set.
			var sum int64
			for _, n := range buckets {
				sum += n
			}
			if int(sum) != len(pvWantMissing(f, c)) {
				t.Errorf("buckets sum to %d, want %d", sum, len(pvWantMissing(f, c)))
			}
		})
	}
}

func TestPreviewMissing_PresentIsRefusedOnBothSpellings(t *testing.T) {
	if _, _, err := foldDSL("preview:present", facet.Selection{}, ""); err == nil {
		t.Error("dsl=preview:present accepted; the vocabulary is `missing` alone")
	}
	if _, err := facet.ParseSelection([]string{"preview:present"}); err == nil {
		t.Error("filter=preview:present accepted")
	}
	if _, _, err := foldDSL("!nopreviewspresent", facet.Selection{}, ""); err == nil {
		t.Error("!nopreviewspresent accepted; the verb takes no value")
	}
}

// TestPreviewMissing_PostsAndCollectionsDropOut: a post-only query
// returns zero with no error, and a mixed-type query returns assets
// only.
func TestPreviewMissing_PostsAndCollectionsDropOut(t *testing.T) {
	pool := coPool(t)
	f := pvSeed(t, pool)
	owner := pvCallers(f)[0]

	postOnly := pvRun(t, pool, owner, pvPhrase+" AND !nopreviews", []HitType{HitTypePost})
	if len(postOnly.Hits) != 0 || postOnly.TotalCount != 0 {
		t.Errorf("post-only preview:missing returned %d hits / count %d, want 0 / 0", len(postOnly.Hits), postOnly.TotalCount)
	}
	// The post IS there without the filter, so the zero above is the
	// dimension's doing.
	if n := len(pvRun(t, pool, owner, pvPhrase, []HitType{HitTypePost}).Hits); n != 1 {
		t.Fatalf("the fixture post is not searchable (%d hits); the zero above proves nothing", n)
	}
	mixed := pvRun(t, pool, owner, pvPhrase+" AND !nopreviews", []HitType{HitTypeAsset, HitTypePost, HitTypeCollection})
	for _, h := range mixed.Hits {
		if h.Type != HitTypeAsset {
			t.Errorf("mixed-type preview:missing returned a %s", h.Type)
		}
	}
	if mixed.TotalCount != 3 {
		t.Errorf("mixed-type total_count %d, want 3 (the three missing assets)", mixed.TotalCount)
	}
}
