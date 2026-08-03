-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00031_asset_field_value_history_fks.sql
--
-- Give asset_field_value_history the two foreign keys its collection
-- sibling has always had (#821).
--
-- collection_field_value_history carries
-- collection_field_value_history_field_id_fkey and
-- collection_field_value_history_collection_id_fkey, both ON DELETE
-- CASCADE, so a deleted field or collection takes its history with it.
-- asset_field_value_history had only its primary key: no FK on field_id
-- or asset_id at all. Its rows therefore OUTLIVED the asset or field
-- they describe, becoming orphans pointing at ids that no longer exist,
-- and — being unreachable by TRUNCATE ... CASCADE — surviving
-- `aa seed --reset` too (the same "CASCADE cannot reach me" class as the
-- polymorphic tables in internal/seed/reset.go). This closes the gap so
-- the two history tables enforce identically.
--
-- ORPHAN DELETE COMES FIRST. Adding a FK to a table that already holds
-- rows violating it aborts the whole ALTER — and this table almost
-- certainly does hold such rows, because their existence IS the bug. So
-- the orphans are swept before the constraints go on. A fresh database
-- has no orphans and would not exercise this path, which is why the
-- up/down test runs against a SEEDED database.
--
-- With the CASCADE FKs in place the bespoke asset_field_value_history
-- sweep in internal/seed/reset.go is moot: TRUNCATE assets ... CASCADE
-- now reaches this table via asset_id, and the field_definition sweep
-- reaches it via field_id. That sweep is removed in the same change and
-- its test comments corrected — see reset.go.

-- +goose Up

-- Sweep rows whose asset or field is already gone, so the constraints
-- can be added without aborting on a pre-existing orphan.
DELETE FROM public.asset_field_value_history h
 WHERE NOT EXISTS (SELECT 1 FROM public.assets a WHERE a.id = h.asset_id)
    OR NOT EXISTS (SELECT 1 FROM public.field_definition f WHERE f.id = h.field_id);

ALTER TABLE ONLY public.asset_field_value_history
    ADD CONSTRAINT asset_field_value_history_field_id_fkey
    FOREIGN KEY (field_id) REFERENCES public.field_definition(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.asset_field_value_history
    ADD CONSTRAINT asset_field_value_history_asset_id_fkey
    FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE;

-- +goose Down

ALTER TABLE ONLY public.asset_field_value_history
    DROP CONSTRAINT IF EXISTS asset_field_value_history_asset_id_fkey;

ALTER TABLE ONLY public.asset_field_value_history
    DROP CONSTRAINT IF EXISTS asset_field_value_history_field_id_fkey;
