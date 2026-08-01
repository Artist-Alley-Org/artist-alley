-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00017_seed_pixel_dimension_fields.sql
--
-- Seed pixel_width / pixel_height field definitions, WIRED for
-- extraction (#618).
--
-- IIIF's info.json reads an asset's pixel dimensions out of
-- asset_field_value via field_definition codes 'pixel_width' /
-- 'pixel_height' (iiif/http.go, PoolLookup). Neither definition existed
-- anywhere: the baseline's 13 are editorial/rights fields and the
-- dataset seed's 12 are studio-fiction — no technical/EXIF field among
-- either set. So the join could not match even in principle, and every
-- info.json 404ed with "no recorded pixel dimensions" (#618) even after
-- #621 made the EXIF backfill actually run.
--
-- WIRED IS THE POINT, NOT A DETAIL. The extractor routes an extracted
-- tag to a field through:
--
--     SELECT id, extraction_source, extraction_mode
--       FROM field_definition WHERE extraction_source != '';
--
-- so a definition with the default '' extraction_source routes NOTHING —
-- the backfill reports success and asset_field_value stays empty,
-- indistinguishable from today. extraction_source must equal the
-- extractor's own CanonicalField constant (metadata.FieldPixelWidth =
-- 'pixel_width', FieldPixelHeight = 'pixel_height' — the codes and the
-- canonical names coincide by design here).
--
-- extraction_mode is 'replace', deliberately not the default
-- 'skip_if_set': dimensions are a fact about the stored bytes, not
-- operator prose. skip_if_set exists so extraction never clobbers
-- operator work; freezing a stale width after a replace-file is the
-- opposite of that intent.
--
-- ON CONFLICT heals rather than skips: if a row with the code already
-- exists but is UNWIRED (extraction_source = ''), the wiring is applied
-- — an unwired definition is the precise defect class this migration
-- exists to remove. A row an operator deliberately pointed somewhere
-- else is left alone (the WHERE guard).
--
-- searchable = false: these feed IIIF and the metadata panel, not the
-- text index — a number in a tsvector is noise.
--
-- NOTE: `aa seed --reset` TRUNCATEs field_definition, so this migration
-- alone keeps only non-reseeded installs correct. The dataset seed
-- carries the same two definitions (dataset.field_definitions.json +
-- seed.SeedInsertField's extraction columns) so a reseed reinstates
-- them wired. Both halves land together in #618.
--
-- CORRECTION 2026-08-01 (#812): the NOTE above is no longer true and is
-- kept only so the reasoning reads in order. `aa seed --reset` no longer
-- truncates field_definition — it SWEEPS it against the shipped-code
-- registry in app/internal/db/shippedfields.go, and pixel_width /
-- pixel_height are on that registry, so these two rows now survive a
-- reset. The dataset catalogue still lists them; since #812 those
-- entries BIND to the rows this migration inserted rather than creating
-- new ones. (The extraction wiring this migration applies was
-- subsequently withdrawn by 00020 — these are computed, not extracted.)

-- +goose Up
INSERT INTO public.field_definition
    (code, label, description, type, searchable,
     display_order, display_group, subject_kind,
     extraction_source, extraction_mode)
VALUES
    ('pixel_width', 'Pixel width',
     'Width of the stored image in pixels. Extracted from the file; feeds the IIIF Image API.',
     'number', false, 200, 'technical', 'asset', 'pixel_width', 'replace'),
    ('pixel_height', 'Pixel height',
     'Height of the stored image in pixels. Extracted from the file; feeds the IIIF Image API.',
     'number', false, 201, 'technical', 'asset', 'pixel_height', 'replace')
ON CONFLICT (code) DO UPDATE SET
    extraction_source = EXCLUDED.extraction_source,
    extraction_mode   = EXCLUDED.extraction_mode
WHERE field_definition.extraction_source = '';

-- +goose Down
DELETE FROM public.field_definition
 WHERE code IN ('pixel_width', 'pixel_height');
