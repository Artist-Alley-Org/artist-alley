// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package featured

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// #1207 (absorbing #1200) — the rail's collection tile resolves its
// picture in PREFERENCE ORDER, and every rung is gated for the caller
// who is looking.
//
// The defect these tests exist to keep closed is not "the rail picked
// the wrong picture". It is that the rail read NEITHER chosen column:
// every other collection surface — the covers endpoint, the collection
// card, the edit modal's crop preview — rendered the curator's choice
// while the strip rendered a derived one, under a comment claiming the
// override "is not in the schema yet" that had been untrue since
// migration 00046.
//
// The fixtures below give one collection THREE DISTINCT covers, because
// a test where two rungs share an asset cannot tell "preferred the
// featured cover" from "fell through to the regular one and got lucky".

// railStageCover makes an asset invisible to an ANONYMOUS caller while
// leaving it visible to a signed-in one, without touching its tier.
//
// This is the lever the fallback tests need and the reason it is
// processing_status rather than sensitivity. The cover lateral applies
// TWO independent bars: the caller's ADR 0063 predicate, and a flat
// `sensitivity = 'public'` that no caller is exempt from. Gating a
// fixture by sensitivity would trip the flat bar for everybody, so both
// callers would see the same fallback and the test would pass without
// the PREDICATE splice existing at all — the shape of green that #1207's
// whole point is to avoid. `processing_status = 'pending'` trips only
// the anonymous arm of the predicate, so the two callers genuinely
// disagree and the splice is what makes them.
func railStageCover(t *testing.T, pool *pgxpool.Pool, asset uuid.UUID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET processing_status=$2 WHERE id=$1`, asset, status); err != nil {
		t.Fatalf("stage cover %v as %s: %v", asset, status, err)
	}
}

// railChooseCovers writes the curator's choices onto a seeded
// collection: the featured-rail cover, the ordinary cover, and the
// focal point. nil leaves the column NULL, which is the "no choice"
// the fallback chain reads.
func railChooseCovers(
	t *testing.T,
	pool *pgxpool.Pool,
	coll uuid.UUID,
	featured, regular *uuid.UUID,
	focalX, focalY *float64,
) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE collections
		   SET featured_cover_asset_id = $2,
		       cover_asset_id          = $3,
		       featured_cover_focal_x  = $4,
		       featured_cover_focal_y  = $5
		 WHERE id = $1`, coll, featured, regular, focalX, focalY); err != nil {
		t.Fatalf("choose covers on %v: %v", coll, err)
	}
}

func f64(v float64) *float64 { return &v }

// TestRail_CollectionCoverPrefersFeaturedThenRegularThenDerived is the
// order itself, walked one rung at a time on ONE collection whose three
// candidate covers are three different assets.
//
// Every rung is fully public here: this test is about PREFERENCE, and
// the fallback-under-gating case is the one below it. Keeping them
// apart is deliberate — a single test that removed a cover AND gated it
// could not say which of the two moved the answer.
func TestRail_CollectionCoverPrefersFeaturedThenRegularThenDerived(t *testing.T) {
	pool := railPool(t)
	anon := visibility.NewCaller(nil)

	coll := railCollection(t, pool, "rail-1207-order", "public")
	place(t, pool, "collection", coll, "public", 0)

	featured := railStoredAsset(t, pool, "rail-1207-featured", "public", true)
	regular := railStoredAsset(t, pool, "rail-1207-regular", "public", true)
	derived := railStoredAsset(t, pool, "rail-1207-derived", "public", true)
	railPostInCollection(t, pool, coll, derived, time.Hour)

	// Rung 0. The focal point rides with it.
	railChooseCovers(t, pool, coll, &featured, &regular, f64(0.25), f64(0.75))
	row, ok := railRowFor(t, pool, anon, coll)
	if !ok {
		t.Fatal("featured collection missing from the rail")
	}
	if got := uuid.UUID(row.CoverAssetID.Bytes); got != featured {
		t.Errorf("cover = %v, want the FEATURED cover %v — the rail is still deriving (#1200)", got, featured)
	}
	if row.CoverFocalX == nil || row.CoverFocalY == nil {
		t.Fatal("focal point dropped on the featured rung; the rail would centre a crop the curator moved")
	}
	if *row.CoverFocalX != 0.25 || *row.CoverFocalY != 0.75 {
		t.Errorf("focal = (%v, %v), want (0.25, 0.75)", *row.CoverFocalX, *row.CoverFocalY)
	}

	// Rung 1. No featured choice, so the ordinary cover answers — and
	// the focal point still applies, because the picture the curator
	// positioned in the editor is whichever one occupies the featured
	// slot, and with no separate choice that is this one.
	railChooseCovers(t, pool, coll, nil, &regular, f64(0.25), f64(0.75))
	row, ok = railRowFor(t, pool, anon, coll)
	if !ok {
		t.Fatal("collection vanished after clearing the featured cover")
	}
	if got := uuid.UUID(row.CoverAssetID.Bytes); got != regular {
		t.Errorf("cover = %v, want the REGULAR cover %v", got, regular)
	}
	if row.CoverFocalX == nil || *row.CoverFocalX != 0.25 {
		t.Error("focal point dropped on the regular rung")
	}

	// Rung 2. No chosen cover at all: ADR 0027's derived hero card, and
	// NO focal point — the curator never saw this picture in the editor,
	// so a crop chosen for a different one is not transplanted onto it.
	railChooseCovers(t, pool, coll, nil, nil, f64(0.25), f64(0.75))
	row, ok = railRowFor(t, pool, anon, coll)
	if !ok {
		t.Fatal("collection vanished after clearing both chosen covers")
	}
	if got := uuid.UUID(row.CoverAssetID.Bytes); got != derived {
		t.Errorf("cover = %v, want the DERIVED cover %v (ADR 0027)", got, derived)
	}
	if row.CoverFocalX != nil || row.CoverFocalY != nil {
		t.Errorf("focal (%v, %v) carried onto the derived cover — that crop was chosen for a "+
			"picture this tile is not showing", row.CoverFocalX, row.CoverFocalY)
	}
}

// TestRail_ChosenCoverFallsBackForAViewerWhoMayNotSeeIt is the security
// half, and it is per-VIEWER on purpose.
//
// The same three rows produce different answers for different callers,
// which is the only way to demonstrate that the fallback is driven by
// the predicate splice rather than by the fixture. Three cases, each
// naming its caller:
//
//	sees-featured                       — a signed-in caller gets rung 0
//	featured-restricted → regular       — anon falls to rung 1
//	both-restricted     → derived       — anon falls to rung 2
//
// AND THE WITHHELD ID IS NEVER RETURNED. Falling back is only half the
// requirement: a rail that fell back to the regular cover while still
// naming the gated asset in cover_asset_id would leak exactly what the
// gate withholds, and the client would fire a byte request for it.
func TestRail_ChosenCoverFallsBackForAViewerWhoMayNotSeeIt(t *testing.T) {
	pool := railPool(t)
	anon := visibility.NewCaller(nil)
	signedIn := visibility.NewCaller(func() *int64 { r := railOwner; return &r }())

	// Fixture A: the featured cover is staged (anon-invisible), the
	// regular cover is public.
	collA := railCollection(t, pool, "rail-1207-gated-featured", "public")
	place(t, pool, "collection", collA, "public", 0)
	gatedFeatured := railStoredAsset(t, pool, "rail-1207-gated-featured-asset", "public", true)
	publicRegular := railStoredAsset(t, pool, "rail-1207-public-regular", "public", true)
	derivedA := railStoredAsset(t, pool, "rail-1207-derived-a", "public", true)
	railPostInCollection(t, pool, collA, derivedA, time.Hour)
	railChooseCovers(t, pool, collA, &gatedFeatured, &publicRegular, f64(0.1), f64(0.9))
	railStageCover(t, pool, gatedFeatured, "pending")

	// Fixture B: BOTH chosen covers are staged; only the derived one is
	// reachable by a stranger.
	collB := railCollection(t, pool, "rail-1207-gated-both", "public")
	place(t, pool, "collection", collB, "public", 0)
	gatedFeaturedB := railStoredAsset(t, pool, "rail-1207-gated-featured-b", "public", true)
	gatedRegularB := railStoredAsset(t, pool, "rail-1207-gated-regular-b", "public", true)
	derivedB := railStoredAsset(t, pool, "rail-1207-derived-b", "public", true)
	railPostInCollection(t, pool, collB, derivedB, time.Hour)
	railChooseCovers(t, pool, collB, &gatedFeaturedB, &gatedRegularB, f64(0.1), f64(0.9))
	railStageCover(t, pool, gatedFeaturedB, "pending")
	railStageCover(t, pool, gatedRegularB, "pending")

	for _, tc := range []struct {
		name string
		// caller is the load-bearing input: the same rows, read by two
		// different people, must produce two different pictures.
		caller     visibility.Caller
		coll       uuid.UUID
		want       uuid.UUID
		wantFocal  bool
		mustNotSee []uuid.UUID
	}{
		{
			name:      "sees-featured: the caller who may see the chosen cover gets it",
			caller:    signedIn,
			coll:      collA,
			want:      gatedFeatured,
			wantFocal: true,
		},
		{
			name:       "featured-restricted falls to the regular cover, without naming the withheld one",
			caller:     anon,
			coll:       collA,
			want:       publicRegular,
			wantFocal:  true,
			mustNotSee: []uuid.UUID{gatedFeatured},
		},
		{
			name:       "both-restricted falls to the derived cover, and drops the focal point with them",
			caller:     anon,
			coll:       collB,
			want:       derivedB,
			wantFocal:  false,
			mustNotSee: []uuid.UUID{gatedFeaturedB, gatedRegularB},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := railRowFor(t, pool, tc.caller, tc.coll)
			if !ok {
				t.Fatal("collection missing from the rail; a gated cover must not remove the tile")
			}
			if !row.CoverAssetID.Valid {
				t.Fatal("no cover at all — the chain should have fallen through to a rung this caller can see")
			}
			got := uuid.UUID(row.CoverAssetID.Bytes)
			if got != tc.want {
				t.Errorf("cover = %v, want %v", got, tc.want)
			}
			for _, forbidden := range tc.mustNotSee {
				if got == forbidden {
					t.Errorf("LEAK: rail named %v, an asset this caller may not see", forbidden)
				}
			}
			if row.AssetFileHash == nil {
				t.Error("no file hash beside the resolved cover; the tile would render blank")
			}
			if !row.AssetPreviewAvailable {
				t.Error("preview_available false for a resolved, servable cover")
			}
			if tc.wantFocal && (row.CoverFocalX == nil || *row.CoverFocalX != 0.1) {
				t.Errorf("focal x = %v, want 0.1 on a chosen rung", row.CoverFocalX)
			}
			if !tc.wantFocal && row.CoverFocalX != nil {
				t.Errorf("focal x = %v on the derived rung, want nil (centre)", *row.CoverFocalX)
			}
		})
	}
}

// A chosen cover with no servable `col` variant is not a cover, and the
// chain must keep walking rather than advertise a tile that 404s.
//
// This is the #471 zero-console-404 property arriving at a new rung. The
// derived rung has required a servable variant since #559; a chosen
// cover skipping that check would have the front page fire a byte
// request for a rendition that was never produced — which is the state
// a curator reaches by picking a cover the moment they upload it.
func TestRail_ChosenCoverWithoutAVariantFallsThrough(t *testing.T) {
	pool := railPool(t)
	anon := visibility.NewCaller(nil)

	coll := railCollection(t, pool, "rail-1207-nocol", "public")
	place(t, pool, "collection", coll, "public", 0)

	noCol := railStoredAsset(t, pool, "rail-1207-nocol-asset", "public", false)
	derived := railStoredAsset(t, pool, "rail-1207-nocol-derived", "public", true)
	railPostInCollection(t, pool, coll, derived, time.Hour)
	railChooseCovers(t, pool, coll, &noCol, nil, nil, nil)

	row, ok := railRowFor(t, pool, anon, coll)
	if !ok {
		t.Fatal("collection missing from the rail")
	}
	if got := uuid.UUID(row.CoverAssetID.Bytes); got != derived {
		t.Errorf("cover = %v, want the derived cover %v — a chosen cover with no col variant is "+
			"not servable and must not win the rung", got, derived)
	}
	if !row.AssetPreviewAvailable {
		t.Error("preview_available false after falling through to a servable cover")
	}
}

// An ASSET subject carries no focal point. The columns live on
// `collections`, so there is nowhere for one to come from — this pins
// that the lateral does not accidentally resolve for the asset arm.
func TestRail_AssetSubjectHasNoFocalPoint(t *testing.T) {
	pool := railPool(t)
	anon := visibility.NewCaller(nil)

	asset := railStoredAsset(t, pool, "rail-1207-asset-subject", "public", true)
	place(t, pool, "asset", asset, "public", 0)

	row, ok := railRowFor(t, pool, anon, asset)
	if !ok {
		t.Fatal("public asset missing from the rail")
	}
	if row.CoverFocalX != nil || row.CoverFocalY != nil {
		t.Errorf("asset subject carried a focal point (%v, %v)", row.CoverFocalX, row.CoverFocalY)
	}
}
