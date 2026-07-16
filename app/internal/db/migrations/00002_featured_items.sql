-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00002_featured_items.sql
--
-- Admin-curated "featured" list (GitHub #341, Phase 1.14 follow-up).
--
-- A single flat table of hand-picked spotlight entries a home/browse
-- surface can render as a curated rail. Each row points at either an
-- asset or a collection (subject_kind + subject_id) and carries an
-- explicit ordering position. Kept deliberately separate from the
-- per-collection `collections.featured` boolean, which is a browse
-- filter flag, not an ordered mixed-kind curation surface.
--
-- The (subject_kind, subject_id) unique constraint stops the same
-- subject being featured twice. No FK to assets/collections: a
-- subject can be hard-deleted out from under a featured row, and the
-- list read tolerates the dangling reference (resolves an empty
-- title) rather than cascading — an operator prunes stale entries by
-- hand. This also keeps the mixed-kind subject_id column FK-free,
-- which a two-table polymorphic FK could not express anyway.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE public.featured_items (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    subject_kind text NOT NULL,
    subject_id uuid NOT NULL,
    position integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_ref bigint,
    CONSTRAINT featured_items_pkey PRIMARY KEY (id),
    CONSTRAINT featured_items_subject_kind_check
        CHECK ((subject_kind = ANY (ARRAY['asset'::text, 'collection'::text]))),
    CONSTRAINT featured_items_subject_unique UNIQUE (subject_kind, subject_id)
);

CREATE INDEX featured_items_order_idx
    ON public.featured_items USING btree (position, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS public.featured_items;
-- +goose StatementEnd
