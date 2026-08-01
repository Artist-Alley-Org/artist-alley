-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00023_drop_folded_field_definition_fixtures.sql
--
-- Remove the six test fixtures the v0.1 baseline fold captured into
-- field_definition (#812).
--
-- The baseline inserts 13 field definitions unconditionally, so every
-- fresh install has them. Seven of those are the PRODUCT — title,
-- description, credit, copyright, capture_date, keywords, country —
-- and they are a considered default catalogue, not a dump: their
-- `source` mappings are the same ones ResourceSpace ships in
-- dbstruct/data_resource_type_field.txt (ObjectName/iptc for title,
-- Keywords/iptc for keywords, Country-PrimaryLocationName/iptc for
-- country). Shipping an opinionated default catalogue is normal and
-- those seven stay. Migration 00017 adds two more — pixel_width /
-- pixel_height — so a fresh install's catalogue after this migration
-- is NINE rows.
--
-- The other six are not a catalogue at all. Every one of them carries
-- created_by_user_ref = 420000, a test-fixture user ref that does not
-- exist in any install, and they are named after the tests that made
-- them: mcoltest_fedguard / ctest_fedguard are federation-guard probes,
-- mtv_text / mtv_due / mtv_score / mtv_tags are metadata type-validation
-- probes ("mtv"). They came out of the same dump already known to have
-- captured dev secrets into system_config, and they have shipped to
-- every operator as "Text Field", "Score", "Due", "Tags" and two copies
-- of "Fed Guard" in the general group ever since.
--
-- Deleted by CODE, not by id: an operator who already removed one by
-- hand, or a federation peer that minted its own row for the same
-- natural key, must not turn this migration into a failed boot. Every
-- statement is a set-based DELETE/UPDATE that matches zero rows
-- harmlessly.
--
-- WHY THE THREE STATEMENTS, IN THIS ORDER. The dependants of a
-- field_definition row do NOT all cascade, and assuming they did would
-- leave rows behind:
--
--   * asset_field_value            FK, ON DELETE CASCADE  → automatic
--   * collection_field_value       FK, ON DELETE CASCADE  → automatic
--   * collection_field_value_history FK, ON DELETE CASCADE → automatic
--   * field_default_override       FK, ON DELETE CASCADE  → automatic
--   * asset_field_value_history    NO FOREIGN KEY AT ALL  → manual
--   * field_definition.deprecated_replacement_id
--                                  FK, ON DELETE RESTRICT → manual
--
-- asset_field_value_history is the one that bites: it names field_id
-- (and asset_id) as bare uuid columns with no constraint, so nothing
-- reaches it and its rows would survive pointing at a definition that
-- no longer exists. RESTRICT on deprecated_replacement_id is the other:
-- if anything was ever pointed at one of the six as its replacement,
-- the DELETE would abort the migration outright. Both are handled
-- before the delete rather than hoped away.
--
-- Plain DML, so no StatementBegin/End markers — those exist for
-- plpgsql bodies whose semicolons goose would otherwise split on.

-- +goose Up

-- No FK, so nothing else will ever clean these up.
DELETE FROM public.asset_field_value_history
 WHERE field_id IN (
     SELECT id FROM public.field_definition
      WHERE code IN ('mcoltest_fedguard', 'ctest_fedguard',
                     'mtv_text', 'mtv_due', 'mtv_score', 'mtv_tags'));

-- ON DELETE RESTRICT: a surviving definition that names one of the six
-- as its replacement would block the delete below.
UPDATE public.field_definition
   SET deprecated_replacement_id = NULL,
       updated_at = now()
 WHERE deprecated_replacement_id IN (
     SELECT id FROM public.field_definition
      WHERE code IN ('mcoltest_fedguard', 'ctest_fedguard',
                     'mtv_text', 'mtv_due', 'mtv_score', 'mtv_tags'));

DELETE FROM public.field_definition
 WHERE code IN ('mcoltest_fedguard', 'ctest_fedguard',
                'mtv_text', 'mtv_due', 'mtv_score', 'mtv_tags');

-- +goose Down
-- Restores the six rows exactly as the baseline shipped them, ids
-- included, so an up/down/up cycle is a genuine round trip and the
-- version can be rolled back without leaving a half-state.
--
-- field_set_id is deliberately absent from the column list: goose rolls
-- back in reverse order, so 00022 (which dropped that column) is still
-- applied when this runs and the column does not exist yet.
--
-- The asset_field_value / asset_field_value_history rows that pointed at
-- these definitions are NOT restorable and are not restored. They were
-- values against fixture fields; nothing reads them, and a down
-- migration that invents data is worse than one that admits the loss
-- (same position migration 00020 takes on the exif pixel rows).
INSERT INTO public.field_definition (
    id, code, label, description, type, options, required, searchable,
    applies_to, read_capability, write_capability, display_order,
    display_group, source, status, deprecated_replacement_id,
    origin_server_id, created_at, updated_at, created_by_user_ref,
    updated_by_user_ref, subject_kind, extraction_source, extraction_mode
) VALUES
    ('a90b9209-2ce8-4485-98d8-5b178f8a448a', 'mcoltest_fedguard', 'Fed Guard', '', 'text', '{}', false, true, '{}', NULL, NULL, 100, 'general', NULL, 'active', NULL, NULL, '2026-07-08 14:19:28.004452+00', '2026-07-08 14:19:28.004452+00', 420000, 420000, 'collection', '', 'skip_if_set'),
    ('ddc173f4-0822-4255-9881-ca665eab4d06', 'mtv_text', 'Text Field', '', 'text', '{}', false, true, '{}', NULL, NULL, 100, 'general', NULL, 'active', NULL, NULL, '2026-07-08 14:19:28.078738+00', '2026-07-08 14:19:28.078738+00', 420000, 420000, 'asset', '', 'skip_if_set'),
    ('8ac32f2b-1748-4bae-af75-018e607cda02', 'mtv_score', 'Score', '', 'number', '{}', false, true, '{}', NULL, NULL, 100, 'general', NULL, 'active', NULL, NULL, '2026-07-08 14:19:28.080315+00', '2026-07-08 14:19:28.080315+00', 420000, 420000, 'asset', '', 'skip_if_set'),
    ('790f1232-454a-4dba-803d-bd9397647d3c', 'mtv_due', 'Due', '', 'datetime', '{}', false, true, '{}', NULL, NULL, 100, 'general', NULL, 'active', NULL, NULL, '2026-07-08 14:19:28.08179+00', '2026-07-08 14:19:28.08179+00', 420000, 420000, 'asset', '', 'skip_if_set'),
    ('d09c2ddc-1d47-4f3d-b85a-9d932298123f', 'mtv_tags', 'Tags', '', 'multi_select', '{}', false, true, '{}', NULL, NULL, 100, 'general', NULL, 'active', NULL, NULL, '2026-07-08 14:19:28.083461+00', '2026-07-08 14:19:28.083461+00', 420000, 420000, 'asset', '', 'skip_if_set'),
    ('9bfd1538-6987-44aa-a796-e167a90ddfca', 'ctest_fedguard', 'Fed Guard', '', 'text', '{}', false, true, '{}', NULL, NULL, 100, 'general', NULL, 'archived', NULL, NULL, '2026-06-20 04:29:09.363034+00', '2026-06-20 04:47:26.872961+00', 420000, 1, 'collection', '', 'skip_if_set')
ON CONFLICT (code) DO NOTHING;
