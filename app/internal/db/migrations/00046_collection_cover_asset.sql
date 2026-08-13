-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00046_collection_cover_asset.sql
--
-- A curator can now CHOOSE a collection's cover picture (#1027).
--
-- # What it is, and what it deliberately is not
--
-- A pointer at an ordinary asset — not a bespoke upload, and not a
-- pointer restricted to the collection's own members.
--
-- `CollectionCover` is `{asset_id}` and the client fetches
-- /assets/{asset_id}/variants/col. ONE representation. A dedicated
-- cover upload would make a cover sometimes an asset id and sometimes a
-- storage hash, forcing every client to branch — two expressions of one
-- concept, which is the defect ADR 0063 exists to prevent.
--
-- The in-repo upload precedent argues the same way. POST
-- /admin/system/appearance/logo had to grow content-type-by-decoding,
-- raster-only with SVG refused, a 2 MiB / 16-1024px bound, an
-- `appearance:logo` content-addressed pin and a 5-deep MRU history that
-- releases evicted pins to GC. All of that exists because the logo has
-- no asset to point at. Pointing at an asset inherits storage, the `col`
-- rendition, permissions, federation and GC for free — and "upload a
-- dedicated banner" still works: upload it as an ordinary asset, then
-- pick it.
--
-- Not member-restricted because the failure the issue itself names is a
-- cover that dies when its member is removed. A free pointer survives
-- that.
--
-- # ON DELETE SET NULL is the behaviour, not the cheap option
--
-- If the asset is hard-deleted the collection REVERTS to the derived
-- mosaic composed from its members. The alternative, RESTRICT, would
-- make an unrelated collection's curation choice block an asset's
-- deletion; CASCADE would delete the collection, which is absurd. SET
-- NULL says "the chosen picture is gone, fall back to the composed one",
-- which is exactly the fallback the read path already runs for a cover
-- the viewer may not picture.
--
-- Soft-delete needs no help here: ComposeCovers already requires
-- `assets.deleted_at IS NULL`, so a soft-deleted cover falls back
-- through the same door while the pointer stays put and returns if the
-- asset is restored.
--
-- # Federation: the pointer does NOT travel
--
-- ADR 0083 excludes a property that "names something that exists only on
-- the sender", and a local `assets.id` is precisely that — a peer has no
-- row under that id, so an exported pointer would either dangle or, far
-- worse, resolve to an unrelated local asset. A receiving peer composes
-- the derived mosaic from whatever members it holds, which is the same
-- fallback every other unrenderable-cover path takes.
--
-- 0083 governs FIELD-schema export and this is a collection column, so
-- this is its criterion applied by analogy rather than an implementation
-- of it. Recorded here so the exchange, when it is built, does not
-- relitigate the question. Nothing is exported by this migration.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.collections
    ADD COLUMN cover_asset_id UUID NULL
        REFERENCES public.assets(id) ON DELETE SET NULL;

COMMENT ON COLUMN public.collections.cover_asset_id IS
    'Curator-chosen cover picture (#1027): any asset the curator may PICTURE, not necessarily a member. NULL means compose the derived mosaic from members instead. Read path (collections.ComposeCovers) re-checks the viewer''s picture plane and falls back to the mosaic when the override is unrenderable for them — a withheld cover must never render blank. ON DELETE SET NULL so a hard-deleted asset reverts the collection to its mosaic rather than dangling. Does NOT federate: a local asset id names something that exists only on this server (ADR 0083''s exclusion criterion, applied by analogy).';

-- The read path asks "does THIS collection have an override, and is its
-- asset renderable" for a page of collections at a time; it joins from
-- collections to assets by this column and never searches for the
-- collections pointing AT a given asset. So the index that earns its
-- keep is a partial one over the non-NULL pointers, which is a small
-- minority of rows and keeps the join off a sequential scan.
CREATE INDEX collections_cover_asset_id_idx
    ON public.collections (cover_asset_id)
    WHERE cover_asset_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS public.collections_cover_asset_id_idx;
ALTER TABLE public.collections DROP COLUMN cover_asset_id;
