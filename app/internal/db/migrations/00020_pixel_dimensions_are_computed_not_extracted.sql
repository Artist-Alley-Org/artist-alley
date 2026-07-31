-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00020_pixel_dimensions_are_computed_not_extracted.sql
--
-- pixel_width / pixel_height are COMPUTED, never extracted (#765).
--
-- Two writers aimed at these two rows. The EXIF extractor sniffed
-- image.DecodeConfig and wrote the STORED pixel grid; the preview
-- pipeline (asset/pixeldims.Record, set_by='computed') writes the shape
-- of the image the contain rungs are built from. For an unrotated JPEG
-- the two coincide, which is why it went unnoticed. For an
-- orientation=6 phone photo they are transposed, and there was no
-- precedence rule to break the tie:
--
--   * extraction_mode on both definitions is 'replace', so the extractor
--     overwrites unconditionally;
--   * the applier's only mode check is "is a value present", never
--     set_by — ADR 0012's "skip if set_by = 'manual'" rule was written
--     but never implemented;
--   * on the upload path the preview job is enqueued FIRST and the
--     extract job second, so the extractor is the last writer and the
--     wrong value wins.
--
-- A portrait phone photo would have tiled as landscape. The extractor's
-- write is gone as of #765; this migration removes its ROUTE, so the
-- defect cannot return by way of some future extractor emitting the same
-- canonical field name.
--
-- WHY UNWIRING AND NOT JUST DELETING THE GO CODE. extraction_source is
-- the mapping table between a canonical extractor field and a
-- field_definition:
--
--     SELECT id, extraction_source, extraction_mode
--       FROM field_definition WHERE extraction_source != '';
--
-- Leaving 'pixel_width' in that table says "any extractor that reports a
-- pixel_width may write this row". That is precisely the sentence #765
-- exists to retract: per ADR 0071 §6 the quantity is not the source
-- file's pixels at all. Half the catalogue has no source pixels — a 3D
-- model, a font, an audio file and a plain-text document have none, yet
-- each produces exactly one image on its way through the preview
-- pipeline whose shape is what a tile reserves. A 2048x384 waveform is a
-- 5.33:1 tile and nothing in the .ogg says so. No extractor can answer
-- that, so no extractor should be routed here.
--
-- The definitions THEMSELVES stay. pixeldims.SelectColumnsSQL and the
-- IIIF dimension join both resolve them by `code`, so dropping them
-- would 404 every info.json again (#618) and blank every masonry tile
-- (#757). Only the extraction wiring goes.
--
-- The stale rows go too. Six asset_field_value rows carried
-- set_by='exif' on this install — 200px test fixtures, so no live tile
-- is wrong today — but a row that says "read off a tag" about a number
-- measured from a decode is a lie in the data whether or not anything
-- currently reads it. DELETE rather than re-label to 'computed': the
-- VALUE is suspect too (it is pre-rotation by construction), and
-- pixeldims documents NULL as the honest "unknown, decide client-side".
-- The next preview render stamps the true pair; `aa rebuild-previews`
-- (#763) is the vehicle for doing that catalogue-wide.
--
-- Plain DML, so no StatementBegin/End markers — those exist for plpgsql
-- bodies whose semicolons goose would otherwise split on.

-- +goose Up
UPDATE public.field_definition
   SET extraction_source = '',
       description = CASE code
           WHEN 'pixel_width' THEN
               'Width in pixels of the image the preview ladder is built from. Computed by the preview pipeline; feeds the masonry tile shape and the IIIF Image API.'
           WHEN 'pixel_height' THEN
               'Height in pixels of the image the preview ladder is built from. Computed by the preview pipeline; feeds the masonry tile shape and the IIIF Image API.'
           ELSE description
       END,
       updated_at = now()
 WHERE code IN ('pixel_width', 'pixel_height')
   AND extraction_source <> '';

DELETE FROM public.asset_field_value
 WHERE set_by = 'exif'
   AND field_id IN (SELECT id FROM public.field_definition
                     WHERE code IN ('pixel_width', 'pixel_height'));

-- +goose Down
-- Restores the routing only. The deleted values are not restorable and
-- do not need to be: a preview render re-derives them, correctly.
UPDATE public.field_definition
   SET extraction_source = code,
       updated_at = now()
 WHERE code IN ('pixel_width', 'pixel_height');
