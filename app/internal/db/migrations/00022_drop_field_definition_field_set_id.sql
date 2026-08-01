-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00022_drop_field_definition_field_set_id.sql
--
-- Drop field_definition.field_set_id (#738, ADR 0012 amended).
--
-- The column was declared in ADR 0012 as "for bundling (export/import)"
-- and shipped in the baseline. It never acquired a producer, a consumer,
-- a foreign key, an index, or a referent — there has never been a
-- `field_set` table for it to point at. Live: 15 of 15 rows NULL, and
-- the only FK on the table is deprecated_replacement_id.
--
-- Its stated purpose was federation schema exchange: "operators publish
-- a field_set JSON to share with peers; peers import to adopt identical
-- field schemas" (ADR 0012 § Federation model). That consumer was never
-- designed, let alone built. Federation moves no metadata at all — the
-- activity catalogue in app/internal/federation/vocab.go has no field-
-- definition or field-value verb, and the outbox resolver projects no
-- metadata onto an object. None of the four federation ADRs written
-- after 0012 (0007, 0042, 0043, 0049) mentions field sets. Neither does
-- the bulk import/export epic (#521) or ADR 0019, both of which are
-- about ingesting and dumping ASSETS, not schemas.
--
-- The grouping the column is often mistaken for is already served, twice
-- over and by populated columns: display_group (UI grouping) and
-- applies_to (asset-type scoping). A persisted set would be a third
-- grouping axis that has to be kept consistent with the other two while
-- granting no capability neither of them already grants.
--
-- The deeper reason to remove rather than complete it: an export unit
-- does not need to be persisted state. Exporting N field definitions to
-- JSON needs an endpoint that accepts a list of field codes, not a row
-- that fields point at. Keeping the column encodes the wrong shape and
-- actively misleads — issue #738 read the dangling column and concluded
-- "build the table", which would have moved the dangling reference up
-- one level and left it just as unwritten. See ADR 0012's amendment for
-- the envelope shape to use if schema exchange is ever built.
--
-- Same class as #579 / migration 00016 (assets.has_image): a column that
-- never had a producer, kept because it looked like intent. That one had
-- four consumers silently reading a lie. This one got lucky and had
-- none — dropping it now is what keeps it that way.
--
-- NOT a data migration: every value is NULL, so nothing is lost and
-- nothing needs backfilling before or after.
--
-- Plain DDL, so no StatementBegin/End markers — those exist for plpgsql
-- bodies, whose semicolons goose would otherwise split on.

-- +goose Up
ALTER TABLE public.field_definition
    DROP COLUMN field_set_id;

-- +goose Down
-- Restores the original baseline definition exactly: a nullable uuid
-- with no FK, no index and no default, which is what it always was.
ALTER TABLE public.field_definition
    ADD COLUMN field_set_id uuid;
