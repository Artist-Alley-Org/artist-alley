// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package pixeldims

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// The WRITE half of the same two fields (#757).
//
// WHY THE WRITER LIVES BESIDE THE READER. SelectColumnsSQL above says
// what these columns mean to a browse page; this says what a producer
// has to put in them for that meaning to hold. Split across packages,
// the two halves drift — which is exactly how the feature shipped: the
// reader, the API field, the masonry estimator and the tile CSS were all
// built for #640/#651/#652 and merged green, while nothing anywhere
// wrote a value. Every tile in masonry fell to the 1:1 last resort and
// the mode was a grid wearing a masonry's name.
//
// WHAT THE VALUE IS. The dimensions of THE IMAGE THE CONTAIN LADDER IS
// BUILT FROM — not "the source file's pixels", which half the catalogue
// does not have. A 3D model, a font, an audio file and a plain-text
// document have no source pixels at all, yet every one of them produces
// exactly one image on its way through the preview pipeline (a
// turntable frame, a glyph specimen, an ffmpeg waveform, a rendered
// text plate) and fans it across the ladder. That image is what a card
// renders, so its shape is what the tile has to reserve.
//
// For a raster the two definitions coincide, with one deliberate
// refinement: the ladder source is the EXIF-ROTATED image, so an
// orientation=6 phone photo records the portrait shape the viewer
// actually sees rather than the landscape shape stored on disk. That is
// the right value for every consumer we have — masonry sizes the tile it
// draws, IIIF's info.json describes the rungs it serves, and both are
// post-rotation.
//
// THIS IS THE ONLY WRITER, as of #765. The EXIF extractor used to write
// the on-disk pair here too, from image.DecodeConfig, which reports the
// stored grid and so disagrees with this one for exactly the rotated
// photos the refinement above exists for. Nothing arbitrated: both
// definitions carried extraction_mode='replace', the applier's mode
// check reads presence and never set_by (ADR 0012's "skip if
// set_by='manual'" rule was written but never implemented), and on the
// upload path the extract job is enqueued after the preview job — so
// the stored grid was the last writer and won. Migration 00020 removed
// the definitions' extraction_source, so the route is gone as well as
// the caller.
//
// WHY NOT storage_variants.metadata. It is per-object-hash, and the
// quantity is not a property of any stored variant: `col` is 320x320
// for everything, and the ladder SOURCE — the thing whose shape this
// is — is never itself written to the backend. Recording it there would
// mean either a synthetic row for an object that does not exist or
// reconstructing the ratio from a rung, and nothing reads that column
// today, so pixeldims.SelectColumnsSQL would have had to be rewritten
// to serve a shape it already serves. asset_field_value is where the
// existing reader looks, the API already projects it, and ADR 0012
// provides set_by='computed' for precisely a value the system derived
// rather than a human typed.
// ---------------------------------------------------------------------------

// SetBy is the asset_field_value provenance these writes carry. Not
// 'exif' — nothing was read from a tag; the number was measured off a
// decoded image. The set_by CHECK constraint on asset_field_value
// admits 'computed' for exactly this.
const SetBy = "computed"

// recordSQL upserts both dimensions in one statement.
//
// The VALUES-to-field_definition join is what makes a missing
// definition a no-op instead of an error: an install that has not run
// migration 00017, or one whose `aa seed --reset` TRUNCATEd
// field_definition before re-inserting, simply matches zero rows. A
// preview job must not fail because a metadata field is absent.
//
// The `WHERE ... IS DISTINCT FROM` on the conflict target is load-
// bearing, not an optimisation. asset_field_value carries an AFTER
// trigger that rebuilds the asset's search text and pg_notifys a
// cache invalidation on every write. `aa rebuild-previews` re-renders
// the whole catalogue, and an unguarded upsert would fire two of those
// per asset per run for values that did not change.
const recordSQL = `
INSERT INTO asset_field_value (asset_id, field_id, value_num, set_by, set_at)
SELECT $1::UUID, fd.id, v.num, '` + SetBy + `', NOW()
  FROM (VALUES ('` + Width + `', $2::DOUBLE PRECISION),
               ('` + Height + `', $3::DOUBLE PRECISION)) AS v(code, num)
  JOIN field_definition fd ON fd.code = v.code
ON CONFLICT (asset_id, field_id) DO UPDATE
   SET value_num = EXCLUDED.value_num,
       set_by    = EXCLUDED.set_by,
       set_at    = NOW()
 WHERE asset_field_value.value_num IS DISTINCT FROM EXCLUDED.value_num`

// Record writes an asset's pixel dimensions.
//
// Returns nil for a nil pool, a nil asset id or a non-positive pair —
// callers hand it whatever they measured and this is the one place that
// decides what is worth storing, mirroring [Sane] on the read side. A
// zero or negative dimension is a decode that went wrong, and writing
// it would put a divide-by-zero in the browse payload.
func Record(ctx context.Context, pool *pgxpool.Pool, assetID uuid.UUID, w, h int) error {
	if pool == nil || assetID == uuid.Nil || w <= 0 || h <= 0 {
		return nil
	}
	_, err := pool.Exec(ctx, recordSQL, assetID, float64(w), float64(h))
	return err
}
