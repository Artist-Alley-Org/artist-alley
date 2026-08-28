// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1333: changing a post's cover kept the framing chosen for the OLD
// picture.
//
// # What was wrong
//
// `cover_focal_x` / `cover_focal_y` are fractions of ONE particular
// picture's width and height. UpdatePost wrote them through
// `COALESCE(narg, column)`, so "the caller said nothing about framing"
// meant "keep what is stored" no matter what else the same request did.
// Swap the cover and the old pair stayed, and the new picture was
// framed on a point nobody picked. Nothing fails, nothing logs, and the
// card just looks wrong forever.
//
// # What makes this file worth reading
//
// THE SECOND CASE IS THE ONE THAT BREAKS A CARELESS FIX. Both cover
// editors save the new picture AND its new focal point in a SINGLE
// PATCH, so a rule shaped "clear whenever cover_asset_id is present"
// throws away the value the curator just chose, while passing a
// one-field test for the reported bug. TestPostCoverSwap_SuppliedFocal
// IsKept is the working path, and it is the reason the query's CASE
// arms are ordered rather than merely present.
//
// The other two are controls in the opposite direction: a PATCH that
// re-sends the SAME cover id is not a swap (clients round-trip whole
// objects), and a PATCH that mentions no cover at all must leave the
// framing entirely alone.
//
// Every assertion reads the COLUMNS BACK OUT OF POSTGRES. The handler
// echoes the row it just wrote, so a body assertion is derived from the
// same statement under test and would pass on the bug.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	csAuthor int64 = 13330001
)

// csFocalOf reads the PERSISTED pair. Two nils mean centred.
func csFocalOf(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID) (*float64, *float64) {
	t.Helper()
	var x, y *float64
	if err := pool.QueryRow(context.Background(),
		`SELECT cover_focal_x, cover_focal_y FROM posts WHERE id = $1`, postID).Scan(&x, &y); err != nil {
		t.Fatalf("read cover focal pair: %v", err)
	}
	return x, y
}

func csWantFocal(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID, wx, wy float64, why string) {
	t.Helper()
	x, y := csFocalOf(t, pool, postID)
	if x == nil || y == nil || *x != wx || *y != wy {
		t.Errorf("persisted focal pair = %v/%v, want %v/%v\n  %s", fmtF(x), fmtF(y), wx, wy, why)
	}
}

func csWantCleared(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID, why string) {
	t.Helper()
	x, y := csFocalOf(t, pool, postID)
	if x != nil || y != nil {
		t.Errorf("persisted focal pair = %v/%v, want NULL/NULL\n  %s", fmtF(x), fmtF(y), why)
	}
}

func fmtF(v *float64) any {
	if v == nil {
		return "NULL"
	}
	return *v
}

// csSeedFramedPost plants a post whose cover is `cover` and whose focal
// pair is 0.1/0.9, straight through the real create + update path so
// the fixture is a state the API can actually produce.
func csSeedFramedPost(t *testing.T, h *Handler, pool *pgxpool.Pool, cover uuid.UUID) uuid.UUID {
	t.Helper()
	postID := seedTierPost(t, pool, csAuthor, "public")
	x, y := 0.1, 0.9
	c := openapi_types.UUID(cover)
	resp, err := h.UpdatePost(ctxAs(csAuthor), openapi.UpdatePostRequestObject{
		Id:   openapi_types.UUID(postID),
		Body: &openapi.PostUpdate{CoverAssetId: &c, CoverFocalX: &x, CoverFocalY: &y},
	})
	if err != nil {
		t.Fatalf("seed framed post: %v", err)
	}
	if _, ok := resp.(openapi.UpdatePost200JSONResponse); !ok {
		t.Fatalf("seed framed post: response is %T, want 200", resp)
	}
	// The precondition. Without it every "cleared" assertion below would
	// pass on a post that was never framed in the first place.
	csWantFocal(t, pool, postID, 0.1, 0.9,
		"precondition: the fixture must START framed, or the swap tests prove nothing")
	return postID
}

func csPatch(t *testing.T, h *Handler, postID uuid.UUID, body *openapi.PostUpdate) {
	t.Helper()
	resp, err := h.UpdatePost(ctxAs(csAuthor), openapi.UpdatePostRequestObject{
		Id:   openapi_types.UUID(postID),
		Body: body,
	})
	if err != nil {
		t.Fatalf("UpdatePost: %v", err)
	}
	if _, ok := resp.(openapi.UpdatePost200JSONResponse); !ok {
		t.Fatalf("UpdatePost: response is %T, want 200", resp)
	}
}

// TestPostCoverSwap_ClearsStaleFocal is the bug. FAILS on dev at
// 9e850ef9, where the COALESCE keeps 0.1/0.9 on a picture those
// fractions were never measured against.
func TestPostCoverSwap_ClearsStaleFocal(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	first := seedPreviewAssetOwned(t, pool, "public", false, csAuthor)
	second := seedPreviewAssetOwned(t, pool, "public", false, csAuthor)

	postID := csSeedFramedPost(t, h, pool, first)
	c := openapi_types.UUID(second)
	csPatch(t, h, postID, &openapi.PostUpdate{CoverAssetId: &c})

	csWantCleared(t, pool, postID,
		"#1333: the cover changed and the request said nothing about framing, so the "+
			"framing must go. Keeping it points the new picture at a fraction measured "+
			"on a different one")
}

// TestPostCoverSwap_SuppliedFocalIsKept is the case a careless fix
// breaks. This is what the cover editor actually sends.
func TestPostCoverSwap_SuppliedFocalIsKept(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	first := seedPreviewAssetOwned(t, pool, "public", false, csAuthor)
	second := seedPreviewAssetOwned(t, pool, "public", false, csAuthor)

	postID := csSeedFramedPost(t, h, pool, first)
	c := openapi_types.UUID(second)
	x, y := 0.25, 0.75
	csPatch(t, h, postID, &openapi.PostUpdate{CoverAssetId: &c, CoverFocalX: &x, CoverFocalY: &y})

	csWantFocal(t, pool, postID, 0.25, 0.75,
		"a new cover AND its new framing arrive in ONE PATCH, which is how both editors "+
			"save. A rule that cleared on any cover change would discard the value the "+
			"curator just chose and still pass the swap test above")
}

// TestPostCoverSwap_SameCoverIdIsNotASwap is the control on the other
// side. Clients round-trip whole objects; re-sending the stored id is
// not a change and must not throw the framing away.
func TestPostCoverSwap_SameCoverIdIsNotASwap(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	first := seedPreviewAssetOwned(t, pool, "public", false, csAuthor)

	postID := csSeedFramedPost(t, h, pool, first)
	c := openapi_types.UUID(first)
	csPatch(t, h, postID, &openapi.PostUpdate{CoverAssetId: &c})

	csWantFocal(t, pool, postID, 0.1, 0.9,
		"IS DISTINCT FROM, not IS NOT NULL: the cover did not change, so nothing about "+
			"the framing did either")
}

// TestPostCoverSwap_UnrelatedPatchKeepsFocal is the regression control.
// A fix that nulled the pair on every PATCH would pass both swap tests.
func TestPostCoverSwap_UnrelatedPatchKeepsFocal(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	first := seedPreviewAssetOwned(t, pool, "public", false, csAuthor)

	postID := csSeedFramedPost(t, h, pool, first)
	title := "cs retitled"
	csPatch(t, h, postID, &openapi.PostUpdate{Title: &title})

	csWantFocal(t, pool, postID, 0.1, 0.9,
		"the request never mentioned the cover, so the framing is untouched")
}

// TestPostCoverSwap_ClearFlagStillWins pins arm ORDER at the top. The
// explicit clear must beat a supplied value being present in the same
// body only insofar as the handler allows it; here it is sent alone
// with a cover swap, and the pair must end up NULL either way.
func TestPostCoverSwap_ClearFlagStillWins(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	first := seedPreviewAssetOwned(t, pool, "public", false, csAuthor)
	second := seedPreviewAssetOwned(t, pool, "public", false, csAuthor)

	postID := csSeedFramedPost(t, h, pool, first)
	c := openapi_types.UUID(second)
	clear := true
	csPatch(t, h, postID, &openapi.PostUpdate{CoverAssetId: &c, ClearCoverFocal: &clear})

	csWantCleared(t, pool, postID, "clear_cover_focal is the first arm and remains absolute")
}
