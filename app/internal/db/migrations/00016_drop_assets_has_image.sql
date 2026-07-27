-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00016_drop_assets_has_image.sql
--
-- Drop assets.has_image (#579, ADR 0011 amendment pending).
--
-- The column was declared in the baseline as `boolean DEFAULT false NOT
-- NULL` and NOTHING EVER WROTE IT. Not the upload path, not the preview
-- worker, not any backfill — live, 1007 of 1007 rows were false. It was
-- not stale data; it was a field that never had a producer.
--
-- That made it worse than merely useless, because four separate
-- consumers read it as though it meant something, and each one silently
-- did nothing:
--
--   * IIIF gated both its endpoints on it, so the entire Image API
--     returned 404 for every asset for a full release (#614).
--   * The EXIF metadata backfill gated eligibility on it, so it
--     enqueued zero work while reporting success — which in turn
--     starved pixel_width/pixel_height and kept IIIF's info.json
--     unbuildable even after the gate above was fixed (#579 step 1).
--   * The CLIP visual-embedding backlog + backfill queue gated on it,
--     so the admin dashboard reported perfect coverage of nothing and
--     the worker rejected every asset that reached it (#615).
--   * The featured rail and the admin featured list keyed thumbnails on
--     its API projection, so asset tiles rendered placeholders forever
--     (#619, #559).
--
-- Every one of those is now keyed on something real — stored variants,
-- or the file extension — and the column has no readers left. Dropping
-- it is what makes the class of bug unrepeatable: a future gate cannot
-- reach for a field that does not exist, which is a stronger guarantee
-- than a comment asking it not to.
--
-- NOT a data migration: there is no information here to preserve. Every
-- value is the default, so nothing is lost and nothing needs backfilling
-- before or after.
--
-- Plain DDL, so no StatementBegin/End markers — those exist for plpgsql
-- bodies, whose semicolons goose would otherwise split on.
--
-- Deliberately NOT touched: assets.is_transcoding is also writerless
-- today, but it causes no bug and may be intended for the video
-- pipeline. Removing it is a product call, not cleanup to fold in here.

-- +goose Up
ALTER TABLE public.assets
    DROP COLUMN has_image;

-- +goose Down
-- Restores the original baseline definition exactly, so a rollback puts
-- the schema back where it was rather than approximately there. The
-- column comes back empty of meaning — as it always was — which is
-- correct: nothing wrote it before and nothing would after.
ALTER TABLE public.assets
    ADD COLUMN has_image boolean DEFAULT false NOT NULL;
