// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1333: changing a collection's cover kept the crop chosen for the
// OLD picture, on BOTH slots.
//
// # What was wrong
//
// A focal pair is a fraction of one particular picture's width and
// height, and a zoom is a multiplier on the window that picture fits
// into. UpdateCollection wrote all six columns through
// `COALESCE(narg, column)`, so "the caller said nothing about framing"
// meant "keep what is stored" regardless of what else the same request
// did. Swap the picture, keep the framing, and the card is cropped on a
// point nobody chose. Nothing errors and nothing logs.
//
// This is NOT new in #1210. Collections have carried it since the
// featured cover's focal point shipped in #1207; finding it on posts is
// what exposed it here.
//
// # What makes this file worth reading
//
// THREE things that a fix shaped "clear whenever a cover id is present"
// gets wrong, each of which this file separates:
//
//  1. THE SUPPLIED-FRAMING CASE. The cover editor saves the new picture
//     AND its new focal point AND its new zoom in ONE PATCH. Clearing on
//     any cover change discards exactly the value the curator just
//     chose, while still passing a one-field test for the reported bug.
//  2. SLOT INDEPENDENCE. `featured_cover_*` and `cover_*` are different
//     destinations (890:500 and 4:3). Swapping the rail's picture must
//     not disturb how the collection card is framed, and vice versa. A
//     fix keyed on "any cover column changed" collapses the two.
//  3. ZOOM TRAVELS WITH FOCAL. Clearing the focal alone would leave the
//     new picture centred but still tightened to a multiple chosen for a
//     different photograph, which is the same silent wrongness one
//     column over.
//
// Every assertion reads the COLUMNS BACK OUT OF POSTGRES, for the reason
// expiry_clear_test states: the handler echoes the row it just wrote, so
// a body assertion is derived from the same statement under test.
//
// Skips without AA_DB_PASSWORD.

package collections_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// csCrop is one slot's three crop columns as persisted.
type csCrop struct {
	X, Y, Zoom *float64
}

func (c csCrop) String() string {
	return "focal=" + fmtF(c.X) + "/" + fmtF(c.Y) + " zoom=" + fmtF(c.Zoom)
}

func fmtF(v *float64) string {
	if v == nil {
		return "NULL"
	}
	// Only ever 0.1 / 0.9 / 0.25 / 0.75 / 2 / 3 in this file, so a
	// coarse rendering is enough to read a failure.
	switch *v {
	case 0.1:
		return "0.1"
	case 0.9:
		return "0.9"
	case 0.25:
		return "0.25"
	case 0.75:
		return "0.75"
	case 2:
		return "2"
	case 3:
		return "3"
	}
	return "other"
}

// csRead pulls both slots straight off the row.
func csRead(t *testing.T, pool *pgxpool.Pool, colID string) (featured, collection csCrop) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT featured_cover_focal_x, featured_cover_focal_y, featured_cover_zoom,
		       cover_focal_x, cover_focal_y, cover_zoom
		  FROM collections WHERE id = $1`, colID).Scan(
		&featured.X, &featured.Y, &featured.Zoom,
		&collection.X, &collection.Y, &collection.Zoom); err != nil {
		t.Fatalf("read crop columns: %v", err)
	}
	return
}

func csWant(t *testing.T, got csCrop, x, y, zoom float64, what, why string) {
	t.Helper()
	if got.X == nil || got.Y == nil || got.Zoom == nil ||
		*got.X != x || *got.Y != y || *got.Zoom != zoom {
		t.Errorf("%s: %s, want focal=%v/%v zoom=%v\n  %s", what, got, x, y, zoom, why)
	}
}

func csWantCleared(t *testing.T, got csCrop, what, why string) {
	t.Helper()
	if got.X != nil || got.Y != nil || got.Zoom != nil {
		t.Errorf("%s: %s, want everything NULL\n  %s", what, got, why)
	}
}

// csSeedFramed plants a collection with BOTH slots pointed at their own
// picture and BOTH fully framed, through the real PATCH surface. Returns
// the collection id and the two cover asset ids.
func csSeedFramed(t *testing.T, pool *pgxpool.Pool, router chi.Router, name string) (colID, featuredAsset, coverAsset string) {
	t.Helper()
	colID = mustCreate(t, router, map[string]any{"name": name, "visibility": "private"})
	featuredAsset = ccRenderableAsset(t, pool, ccOwner, name+"_feat", "public")
	coverAsset = ccRenderableAsset(t, pool, ccOwner, name+"_cov", "public")

	if rr := patchJSON(t, router, "/collections/"+colID, map[string]any{
		"featured_cover_asset_id": featuredAsset,
		"featured_cover_focal_x":  0.1,
		"featured_cover_focal_y":  0.9,
		"featured_cover_zoom":     2,
		"cover_asset_id":          coverAsset,
		"cover_focal_x":           0.1,
		"cover_focal_y":           0.9,
		"cover_zoom":              3,
	}); rr.Code != http.StatusOK {
		t.Fatalf("seed framed collection: %d body=%s", rr.Code, rr.Body.String())
	}

	// The precondition. Without it every "cleared" assertion below would
	// pass on a collection that was never framed in the first place.
	f, c := csRead(t, pool, colID)
	csWant(t, f, 0.1, 0.9, 2, "precondition featured slot",
		"the fixture must START framed, or the swap tests prove nothing")
	csWant(t, c, 0.1, 0.9, 3, "precondition collection slot",
		"the fixture must START framed, or the swap tests prove nothing")
	return colID, featuredAsset, coverAsset
}

func csPatch(t *testing.T, router chi.Router, colID string, body map[string]any) {
	t.Helper()
	if rr := patchJSON(t, router, "/collections/"+colID, body); rr.Code != http.StatusOK {
		t.Fatalf("patch %v: %d body=%s", body, rr.Code, rr.Body.String())
	}
}

// TestCollectionCoverSwap_ClearsStaleCrop is the bug, on both slots
// independently. FAILS on dev at 9e850ef9.
func TestCollectionCoverSwap_ClearsStaleCrop(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	t.Run("collection slot", func(t *testing.T) {
		colID, _, _ := csSeedFramed(t, pool, router, "ct_swap_col")
		next := ccRenderableAsset(t, pool, ccOwner, "ct_swap_col_next", "public")
		csPatch(t, router, colID, map[string]any{"cover_asset_id": next})

		f, c := csRead(t, pool, colID)
		csWantCleared(t, c, "collection slot after its own cover swap",
			"#1333: the picture changed and the request said nothing about framing, so "+
				"the framing goes. Keeping it crops the new picture on a fraction "+
				"measured against a different one")
		csWant(t, f, 0.1, 0.9, 2, "featured slot after the COLLECTION cover swap",
			"slot independence: the rail's picture did not change, so nothing about how "+
				"the rail is framed did either")
	})

	t.Run("featured slot", func(t *testing.T) {
		colID, _, _ := csSeedFramed(t, pool, router, "ct_swap_feat")
		next := ccRenderableAsset(t, pool, ccOwner, "ct_swap_feat_next", "public")
		csPatch(t, router, colID, map[string]any{"featured_cover_asset_id": next})

		f, c := csRead(t, pool, colID)
		csWantCleared(t, f, "featured slot after its own cover swap",
			"the 890:500 crop was measured against the picture that just left")
		csWant(t, c, 0.1, 0.9, 3, "collection slot after the FEATURED cover swap",
			"slot independence, in the other direction")
	})
}

// TestCollectionCoverSwap_SuppliedCropIsKept is the case a careless fix
// breaks. This is exactly what the cover editor sends.
func TestCollectionCoverSwap_SuppliedCropIsKept(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	t.Run("collection slot", func(t *testing.T) {
		colID, _, _ := csSeedFramed(t, pool, router, "ct_keep_col")
		next := ccRenderableAsset(t, pool, ccOwner, "ct_keep_col_next", "public")
		csPatch(t, router, colID, map[string]any{
			"cover_asset_id": next,
			"cover_focal_x":  0.25, "cover_focal_y": 0.75, "cover_zoom": 2,
		})
		_, c := csRead(t, pool, colID)
		csWant(t, c, 0.25, 0.75, 2, "collection slot",
			"a new picture AND its new framing arrive in ONE PATCH, which is how the "+
				"cover editor saves. A rule that cleared on any cover change would "+
				"discard the value the curator just chose and still pass the swap test")
	})

	t.Run("featured slot", func(t *testing.T) {
		colID, _, _ := csSeedFramed(t, pool, router, "ct_keep_feat")
		next := ccRenderableAsset(t, pool, ccOwner, "ct_keep_feat_next", "public")
		csPatch(t, router, colID, map[string]any{
			"featured_cover_asset_id": next,
			"featured_cover_focal_x":  0.25, "featured_cover_focal_y": 0.75,
			"featured_cover_zoom": 3,
		})
		f, _ := csRead(t, pool, colID)
		csWant(t, f, 0.25, 0.75, 3, "featured slot", "same rule, other slot")
	})
}

// TestCollectionCoverSwap_SameCoverIdIsNotASwap. Clients round-trip
// whole objects, and re-sending the stored id is not a change.
func TestCollectionCoverSwap_SameCoverIdIsNotASwap(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	colID, featuredAsset, coverAsset := csSeedFramed(t, pool, router, "ct_same")
	csPatch(t, router, colID, map[string]any{
		"cover_asset_id": coverAsset, "featured_cover_asset_id": featuredAsset,
	})

	f, c := csRead(t, pool, colID)
	csWant(t, c, 0.1, 0.9, 3, "collection slot",
		"IS DISTINCT FROM, not IS NOT NULL: neither cover changed, so neither crop did")
	csWant(t, f, 0.1, 0.9, 2, "featured slot", "same")
}

// TestCollectionCoverSwap_UnrelatedPatchKeepsCrop is the regression
// control. A fix that nulled the columns on every PATCH would pass every
// test above.
func TestCollectionCoverSwap_UnrelatedPatchKeepsCrop(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	colID, _, _ := csSeedFramed(t, pool, router, "ct_unrelated")
	csPatch(t, router, colID, map[string]any{"description": "renamed, nothing else"})

	f, c := csRead(t, pool, colID)
	csWant(t, c, 0.1, 0.9, 3, "collection slot",
		"the request never mentioned a cover, so the framing is untouched")
	csWant(t, f, 0.1, 0.9, 2, "featured slot", "same")
}

// TestCollectionCoverSwap_ClearCoverAlsoClearsItsCrop: removing the
// picture removes the framing with it. There is nothing left to frame,
// and leaving the columns behind would silently re-apply them to
// whatever picture is chosen next.
func TestCollectionCoverSwap_ClearCoverAlsoClearsItsCrop(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	colID, _, _ := csSeedFramed(t, pool, router, "ct_clearcover")
	csPatch(t, router, colID, map[string]any{"clear_cover": true})

	f, c := csRead(t, pool, colID)
	csWantCleared(t, c, "collection slot after clear_cover",
		"the picture is gone, so a fraction of its width means nothing")
	csWant(t, f, 0.1, 0.9, 2, "featured slot after clear_cover",
		"clear_cover names ONE slot; the rail keeps its own picture and its own framing")
}
