-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00019_storage_variants_updated_at.sql
--
-- Record WHEN a variant's bytes were last written (#760).
--
-- storage_variants has only created_at, and the upsert that runs on
-- every render is `ON CONFLICT … DO UPDATE SET size_bytes, content_type,
-- metadata` — so re-rendering an existing variant leaves created_at
-- pointing at the FIRST render, forever. The table cannot answer "were
-- these bytes rewritten?", which is the only question that matters when
-- a renderer changes.
--
-- That gap is not academic. The diagnosis in #760 was made by reading
-- created_at: all 590 3D `col` variants dated 2026-07-16, predating both
-- the three.js worker and Blender's removal, which is what proved the
-- renders were stale. The same reading AFTER a successful forced
-- re-render would have shown 2026-07-16 as well — the operator would
-- have concluded the fix had failed. Shipping a force path without this
-- column would replace one silent control with an unfalsifiable one.
--
-- Backfill is created_at, not now(): for a variant nobody has rewritten,
-- "last written" IS its creation time. Stamping now() would assert that
-- 40k variants were just re-rendered, which is a lie the whole change is
-- meant to make impossible.
--
-- No trigger. The one writer (storage.UpsertVariant, used by every
-- preview handler) sets the column explicitly in the same statement that
-- writes the size — a trigger would be a second mechanism for a
-- single-writer column.
--
-- Plain DDL, so no StatementBegin/End markers — those exist for plpgsql
-- bodies whose semicolons goose would otherwise split on.

-- +goose Up
ALTER TABLE public.storage_variants
    ADD COLUMN updated_at timestamp with time zone DEFAULT now() NOT NULL;

UPDATE public.storage_variants SET updated_at = created_at;

-- An operator asking "what did the last rebuild actually touch?" scans
-- by recency across the whole table; without an index that is a seq scan
-- over every variant of every object.
CREATE INDEX IF NOT EXISTS storage_variants_updated_at_idx
    ON public.storage_variants (updated_at DESC);

-- +goose Down
DROP INDEX IF EXISTS storage_variants_updated_at_idx;

ALTER TABLE public.storage_variants
    DROP COLUMN updated_at;
