-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00025_wire_extraction_and_drop_source.sql
--
-- Carry the fold-era extraction INTENT into the column the pipeline
-- actually routes on, then drop the column that held the intent
-- (#813). Three things have been called "source" and only one has ever
-- been read:
--
--   * field_definition.source (jsonb) — populated on six shipped rows
--     with {"tag": "DateTimeOriginal", "type": "exif"} and the like.
--     Read by NOTHING. It is a note describing where a value ought to
--     come from, in a vocabulary (EXIF tag names) that no code speaks.
--   * field_definition.extraction_source (text) — what the pipeline
--     routes on, holding a metadata.CanonicalField name. Empty on
--     every row in the database.
--   * the API's PUT …/extraction {source} — a plain string carrying
--     the extraction_source semantic. Unrelated to the jsonb column;
--     keeps its name.
--
-- So the extract pipeline has run on every upload since 1.18.A-2 and
-- landed nowhere, and /admin/fields has shown "— wire —" on every row,
-- while six rows recorded an intent nobody could act on. This migration
-- turns four of those intents into wiring and deletes the record of all
-- six, because an intent that has been carried out is not worth keeping
-- in a shape nothing can read.
--
-- ===========================================================================
-- What each intent becomes
-- ===========================================================================
--
-- extraction_source must hold a CanonicalField name (see
-- app/internal/asset/metadata/interfaces.go) or the definition routes
-- nothing at all — the applier looks the string up in Result.Fields,
-- which is keyed by CanonicalField. The tag names in the jsonb are
-- EXIF/IPTC/XMP wire names and are not those.
--
--   code          | recorded intent                       | canonical
--   --------------+---------------------------------------+-----------------
--   capture_date  | DateTimeOriginal / exif               | capture_datetime
--   copyright     | dc:rights / xmp                       | xmp_rights
--   credit        | Credit / iptc                         | iptc_credit
--   country       | Country-PrimaryLocationName / iptc    | iptc_country
--
-- `copyright` resolves to xmp_rights rather than the EXIF `copyright`
-- canonical, because the recorded intent names dc:rights, which is XMP.
-- The two are different tags meaning different things — EXIF Copyright
-- is a camera-written notice, dc:rights is a rights statement — and
-- CanonicalField is deliberately per-source rather than per-semantic so
-- an operator can choose. The intent chose. It is also the one that
-- routes: four of the dataset's images carry dc:rights and none carries
-- an EXIF Copyright.
--
-- ===========================================================================
-- Two intents are deliberately NOT wired
-- ===========================================================================
--
-- `keywords` ({"tag": "Keywords", "type": "iptc"}) stays unwired
-- because the applier cannot write it. FieldValueSnapshot and
-- WriteAssetFieldValueParams carry value_text / value_num / value_date;
-- a multi_select value lives in value_options, and there is no path
-- from extraction to that column. The mapping itself is right — IPTC
-- 2:25 is the keyword set, and it is what ResourceSpace maps its own
-- keywords field to — so this is a missing capability, not a missing
-- decision. It belongs to #789 (dynamic keyword lists), which gives the
-- applier somewhere to put a multi-value result. Wiring it now would
-- write a comma-joined string into a field whose reader looks at
-- value_options: a field that stays visibly empty while every log line
-- says it was set. The applier refuses the wiring outright rather than
-- trusting this comment (see writableFieldTypes in vocabulary.go).
--
-- `title` ({"tag": "ObjectName", "type": "iptc"}) stays unwired because
-- its target mirrors assets.title (#822). Wiring it would make
-- extraction populate the FIELD copy of a concept whose real home is
-- the column, and the two would then disagree the moment anyone edited
-- either. The mapping is fine; the destination is the open question,
-- and #822 owns it.
--
-- ===========================================================================
-- Why `country` can be wired at all
-- ===========================================================================
--
-- IPTC 2:101 delivers a LABEL ("United Kingdom"); a tree value is a
-- leaf SLUG ("gb", per 00024 and ADR 0012's tree-storage amendment).
-- Wiring the two directly would store the label in the slug column —
-- a value that renders plausibly, resolves to nothing, matches no
-- filter, and means nothing to a federated peer, with nothing to
-- object because slug validation does not exist yet (#824).
--
-- So the wiring is only half of it: the applier resolves an extracted
-- value against the field's own vocabulary (label or slug,
-- case-insensitive) and stores the slug, or stores NOTHING and records
-- a failure row naming the value that did not resolve. This migration
-- is safe to run because that resolution shipped with it.
--
-- ===========================================================================
-- Mode
-- ===========================================================================
--
-- extraction_mode is left exactly as it is — all four rows already read
-- skip_if_set, which since #793 means "do not overwrite a value someone
-- CHOSE" rather than "do not overwrite anything present". The seeded
-- values on this instance carry set_by='import', which is a choice, so
-- extraction fills gaps and never clobbers a seed. That is ADR 0081 §3
-- and it is asserted both directions in apply_test.go.
--
-- Plain DML + DDL, so no StatementBegin/End markers — those exist for
-- plpgsql bodies whose semicolons goose would otherwise split on.

-- +goose Up

UPDATE public.field_definition
   SET extraction_source = 'capture_datetime', updated_at = now()
 WHERE code = 'capture_date' AND extraction_source = '';

UPDATE public.field_definition
   SET extraction_source = 'xmp_rights', updated_at = now()
 WHERE code = 'copyright' AND extraction_source = '';

UPDATE public.field_definition
   SET extraction_source = 'iptc_credit', updated_at = now()
 WHERE code = 'credit' AND extraction_source = '';

UPDATE public.field_definition
   SET extraction_source = 'iptc_country', updated_at = now()
 WHERE code = 'country' AND extraction_source = '';

-- The intent has been carried out (or consciously deferred, above), so
-- the column recording it goes. Nothing reads it: the only Go that
-- touched it was the CRUD passthrough removed alongside this migration.
ALTER TABLE public.field_definition DROP COLUMN source;

-- +goose Down
--
-- A real round trip: the column comes back and all six intents are
-- restored verbatim, including the two that were never wired.
ALTER TABLE public.field_definition ADD COLUMN source jsonb;

UPDATE public.field_definition SET source = '{"tag": "DateTimeOriginal", "type": "exif"}'::jsonb            WHERE code = 'capture_date';
UPDATE public.field_definition SET source = '{"tag": "dc:rights", "type": "xmp"}'::jsonb                    WHERE code = 'copyright';
UPDATE public.field_definition SET source = '{"tag": "Country-PrimaryLocationName", "type": "iptc"}'::jsonb WHERE code = 'country';
UPDATE public.field_definition SET source = '{"tag": "Credit", "type": "iptc"}'::jsonb                      WHERE code = 'credit';
UPDATE public.field_definition SET source = '{"tag": "Keywords", "type": "iptc"}'::jsonb                    WHERE code = 'keywords';
UPDATE public.field_definition SET source = '{"tag": "ObjectName", "type": "iptc"}'::jsonb                  WHERE code = 'title';

-- Unwire only what the Up wired, and only where it still holds the
-- value the Up set. An operator who re-pointed `credit` at a different
-- canonical made a choice, and a down migration that discarded it would
-- silently un-wire a field they had deliberately configured.
UPDATE public.field_definition
   SET extraction_source = '', updated_at = now()
 WHERE (code = 'capture_date' AND extraction_source = 'capture_datetime')
    OR (code = 'copyright'    AND extraction_source = 'xmp_rights')
    OR (code = 'credit'       AND extraction_source = 'iptc_credit')
    OR (code = 'country'      AND extraction_source = 'iptc_country');
