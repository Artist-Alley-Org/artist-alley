// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #640 — do an asset's recorded pixel dimensions reach the browse
// payload?
//
// They did not, for as long as they have existed. #618 seeded the
// `pixel_width` / `pixel_height` field definitions and wired the EXIF
// pass to write them, and the only reader in the tree was IIIF
// info.json. Every card surface therefore shipped with no idea how tall
// a tile should be, which is why masonry rendered a wall of identical
// squares: CSS was the only thing deciding the shape, because CSS was
// the only thing that knew anything.
//
// The bug class is the one #591 was filed for — the server holds the
// answer and the wire format has no field for it — so the test is
// written against the WIRE-SHAPED row rather than against the SQL. A
// test that queried `asset_field_value` directly would have passed
// happily throughout the entire period the client could not see the
// values.
//
// Skips without AA_DB_PASSWORD, same convention as the sibling suites.

package assets

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const pixelDimsOwner int64 = 4290778

// seedPixelDimsAsset plants an asset and writes whichever of the two
// dimension field values the caller asked for — nil means "this install
// never measured that one", which is the case the pair rule exists for.
func seedPixelDimsAsset(t *testing.T, w, h *int) uuid.UUID {
	t.Helper()
	pool := listPagePool(t)
	ctx := context.Background()
	id := uuid.New()

	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, created_at)
		VALUES ($1,'#640 pixel dims probe',$2,(SELECT MIN(ref) FROM asset_types),
		        'active','public','ready',NOW())`,
		id, pixelDimsOwner); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM asset_field_value WHERE asset_id = $1`, id)
		_, _ = pool.Exec(bg, `DELETE FROM assets WHERE id = $1`, id)
	})

	write := func(code string, v *int) {
		if v == nil {
			return
		}
		// The field definitions are seeded by migration 00017, not by
		// this fixture: a test that created its own would pass against a
		// database where the real ones are missing, which is the failure
		// it is supposed to catch.
		ct, err := pool.Exec(ctx, `
			INSERT INTO asset_field_value (asset_id, field_id, value_num, set_by)
			SELECT $1, fd.id, $2::DOUBLE PRECISION, 'exif'
			  FROM field_definition fd WHERE fd.code = $3`,
			id, float64(*v), code)
		if err != nil {
			t.Fatalf("write %s: %v", code, err)
		}
		if ct.RowsAffected() != 1 {
			t.Fatalf("no field_definition with code %q — migration 00017 "+
				"seeds it, and without it nothing can record dimensions", code)
		}
	}
	write("pixel_width", w)
	write("pixel_height", h)
	return id
}

func pixelDimsRow(t *testing.T, id uuid.UUID) (ListAssetsPageGatedRow, bool) {
	t.Helper()
	rows, err := ListAssetsPageGated(context.Background(), listPagePool(t),
		visibility.NewCaller(ptrTo(pixelDimsOwner)), nil,
		ListAssetsPageGatedParams{OwnerUserRef: ptrTo(pixelDimsOwner), RowLimit: 50})
	if err != nil {
		t.Fatalf("gated list: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.ID.Bytes) == id {
			return r, true
		}
	}
	return ListAssetsPageGatedRow{}, false
}

// TestPixelDims_ReachTheBrowseRow is the invariant that was never true:
// a measured asset arrives at the client carrying its measurements.
func TestPixelDims_ReachTheBrowseRow(t *testing.T) {
	w, h := 1024, 2048
	id := seedPixelDimsAsset(t, &w, &h)

	row, ok := pixelDimsRow(t, id)
	if !ok {
		t.Fatal("seeded asset missing from the gated browse page")
	}
	if row.PixelWidth == nil || row.PixelHeight == nil {
		t.Fatalf("recorded dimensions did not reach the browse row "+
			"(w=%v h=%v). The values are in asset_field_value; if this "+
			"fails the projection is gone again and every masonry tile "+
			"is back to guessing (#640)", row.PixelWidth, row.PixelHeight)
	}
	if *row.PixelWidth != 1024 || *row.PixelHeight != 2048 {
		t.Fatalf("got %dx%d, want 1024x2048", *row.PixelWidth, *row.PixelHeight)
	}
}

// TestPixelDims_AbsentIsNullNotZero pins the shape of "unknown".
//
// Most of the library is here: every non-raster kind, and every draft
// raster (the EXIF backfill selects status = 'active'). A zero would be
// indistinguishable from a real measurement of nothing and would make
// the client divide by it.
func TestPixelDims_AbsentIsNullNotZero(t *testing.T) {
	id := seedPixelDimsAsset(t, nil, nil)

	row, ok := pixelDimsRow(t, id)
	if !ok {
		t.Fatal("seeded asset missing from the gated browse page")
	}
	if row.PixelWidth != nil || row.PixelHeight != nil {
		t.Fatalf("an unmeasured asset reported dimensions (w=%v h=%v); "+
			"unknown must stay null", row.PixelWidth, row.PixelHeight)
	}
}

// TestPixelDims_HalfPairIsDropped: a width with no height cannot make an
// aspect ratio. Projecting it would push the check onto every consumer,
// and one of them would forget.
func TestPixelDims_HalfPairIsDropped(t *testing.T) {
	w := 1600
	id := seedPixelDimsAsset(t, &w, nil)

	row, ok := pixelDimsRow(t, id)
	if !ok {
		t.Fatal("seeded asset missing from the gated browse page")
	}
	if row.PixelWidth != nil || row.PixelHeight != nil {
		t.Fatalf("half a pair was projected (w=%v h=%v) — the API promises "+
			"a pair or neither", row.PixelWidth, row.PixelHeight)
	}
}
