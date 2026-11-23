-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00097_rights_baseline_fields.sql
--
-- Two well-known field definitions for the archival-1 rights baseline
-- (#1577, spec v1 §17.2.5, ADR 0083's 2026-11-17 amendment): a rights URI
-- and a required statement, in the existing `rights` display group beside
-- `copyright` and `credit`.
--
-- # Fixed identities
--
-- Each row has a fixed id. The sender maps a value onto `rights` or
-- `requiredStatement` ONLY from the row with that id and that code
-- (app/internal/federation/assetsend/descriptive.go). An install that
-- already has a field with either code, or a row with either id, keeps it
-- untouched: the insert does nothing (ON CONFLICT DO NOTHING covers the
-- primary key and field_definition_code_key), and the sender then omits
-- that member. An operator's own field is never adopted, renamed or
-- changed by this migration.
--
-- # Receiver-local defaults
--
-- Ungated (no read or write capability), active, for every asset type,
-- plain `text`, no default, no extraction, not shown on cards. Every
-- other participation flag is the column default, as for every shipped
-- field (offered on upload and in advanced search, searchable), so the
-- two rows behave like `copyright` and `credit` beside them. An operator
-- may relabel, gate or retire either row; the sender respects that every
-- time it records and every time it delivers. No value is written and no existing value is
-- touched. No write validation is added: the sender omits a stored value
-- the wire allowlist refuses, rather than this server refusing the write.
--
-- # Down
--
-- Removes a seeded row only when it is still exactly the seeded row and
-- nothing depends on it:
--
--   - same id and code;
--   - its whole configuration still equals the template Up inserted: every
--     column except the created_at / updated_at housekeeping, compared as
--     JSON, so a relabelled, regated, retired or otherwise configured row
--     counts as the operator's (even with no value) and is kept exactly.
--     A column a later migration adds makes the row differ from the
--     template, so such a row is kept too (the conservative direction);
--   - no asset or collection value or history, no default override, no
--     batch preview, and no definition naming it as a replacement.
--
-- A row an operator has configured or started to use stays, byte for
-- byte, and a later Up leaves it as it is (ON CONFLICT DO NOTHING).

-- +goose Up
INSERT INTO public.field_definition
    (id, code, label, description, type, options, required, searchable, applies_to,
     read_capability, write_capability, display_order, display_group, status, subject_kind,
     extraction_source, extraction_mode, show_on_card)
VALUES
    ('56d71791-189d-429c-b7e4-e837baf91ddb', 'rights_uri', 'Rights URI',
     'A Creative Commons licence URI or a RightsStatements.org rights statement URI describing the rights in this asset. Shared with archival peers only when ungated; it never grants anyone access to the files.',
     'text', '{}', false, true, '{}', NULL, NULL, 30, 'rights', 'active', 'asset',
     '', 'skip_if_set', false),
    ('6c3e25be-83ed-4762-b636-a1e37cdbb74c', 'required_statement', 'Required statement',
     'Attribution text that must accompany this asset wherever it is shown. Plain text. Shared with archival peers only when ungated; it never grants anyone access to the files.',
     'text', '{}', false, true, '{}', NULL, NULL, 40, 'rights', 'active', 'asset',
     '', 'skip_if_set', false)
ON CONFLICT DO NOTHING;

-- +goose Down
WITH seeded(template) AS (VALUES
    ('{"id": "56d71791-189d-429c-b7e4-e837baf91ddb", "code": "rights_uri", "label": "Rights URI", "description": "A Creative Commons licence URI or a RightsStatements.org rights statement URI describing the rights in this asset. Shared with archival peers only when ungated; it never grants anyone access to the files.", "type": "text", "options": {}, "required": false, "searchable": true, "applies_to": [], "read_capability": null, "write_capability": null, "display_order": 30, "display_group": "rights", "status": "active", "deprecated_replacement_id": null, "origin_server_id": null, "created_by_user_ref": null, "updated_by_user_ref": null, "subject_kind": "asset", "extraction_source": "", "extraction_mode": "skip_if_set", "default_value": null, "open_vocabulary": false, "mirrors_column": null, "show_on_card": false, "show_in_advanced_search": true, "show_on_upload": true, "edit_tab": null, "read_only": false, "regexp_filter": null, "display_condition": null}'::jsonb),
    ('{"id": "6c3e25be-83ed-4762-b636-a1e37cdbb74c", "code": "required_statement", "label": "Required statement", "description": "Attribution text that must accompany this asset wherever it is shown. Plain text. Shared with archival peers only when ungated; it never grants anyone access to the files.", "type": "text", "options": {}, "required": false, "searchable": true, "applies_to": [], "read_capability": null, "write_capability": null, "display_order": 40, "display_group": "rights", "status": "active", "deprecated_replacement_id": null, "origin_server_id": null, "created_by_user_ref": null, "updated_by_user_ref": null, "subject_kind": "asset", "extraction_source": "", "extraction_mode": "skip_if_set", "default_value": null, "open_vocabulary": false, "mirrors_column": null, "show_on_card": false, "show_in_advanced_search": true, "show_on_upload": true, "edit_tab": null, "read_only": false, "regexp_filter": null, "display_condition": null}'::jsonb))
DELETE FROM public.field_definition d
 USING seeded
 WHERE (to_jsonb(d) - 'created_at' - 'updated_at') = seeded.template
   AND (d.id, d.code) IN (('56d71791-189d-429c-b7e4-e837baf91ddb'::uuid, 'rights_uri'),
                          ('6c3e25be-83ed-4762-b636-a1e37cdbb74c'::uuid, 'required_statement'))
   AND NOT EXISTS (SELECT 1 FROM public.asset_field_value v WHERE v.field_id = d.id)
   AND NOT EXISTS (SELECT 1 FROM public.asset_field_value_history h WHERE h.field_id = d.id)
   AND NOT EXISTS (SELECT 1 FROM public.collection_field_value v WHERE v.field_id = d.id)
   AND NOT EXISTS (SELECT 1 FROM public.collection_field_value_history h WHERE h.field_id = d.id)
   AND NOT EXISTS (SELECT 1 FROM public.field_default_override o WHERE o.field_id = d.id)
   AND NOT EXISTS (SELECT 1 FROM public.metadata_batch_preview p WHERE p.field_id = d.id)
   AND NOT EXISTS (SELECT 1 FROM public.field_definition r WHERE r.deprecated_replacement_id = d.id);
