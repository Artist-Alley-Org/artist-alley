-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00055_collection_featured_cover.sql
--
-- The featured rail gets its OWN cover, and a focal point for the crop
-- (#1207, absorbing #1200 and #1201).
--
-- # Why a second pointer rather than reusing cover_asset_id
--
-- 00046 added `cover_asset_id`: the picture a collection CARD shows.
-- A card is roughly square; a featured-rail card is locked to 890:500
-- (#1110). One picture cannot be the best answer for both shapes, and
-- the owner's finding is exactly that — "the featured collection rail
-- cover and regular collection cover should be different. Not force the
-- same image for both."
--
-- So this is a SECOND pointer, not a widening of the first, and it is
-- nullable for the reason that matters: NULL means "no separate choice
-- was made", and the rail then falls back to `cover_asset_id`, and then
-- to the derived hero-card cover. Three rungs, most-specific first. A
-- NOT NULL column defaulted to the regular cover would have had to be
-- kept in step by a trigger and would lose the difference between "the
-- curator wants these to match" and "the curator has not decided".
--
-- Same shape as `cover_asset_id` in every respect that is a decision:
-- a pointer at an ordinary asset (not a bespoke upload), ON DELETE SET
-- NULL so a hard-deleted asset drops the collection back through the
-- fallback chain rather than dangling, and NOT federated — a local
-- `assets.id` names something that exists only on this server (ADR
-- 0083's exclusion criterion, applied by analogy exactly as 00046
-- recorded it). Read 00046's header for the full argument; it is not
-- restated here, because two copies of a rationale is how one of them
-- goes stale.
--
-- # Why the focal point is a FRACTION, and why it is a pair of columns
--
-- The rail crops with `object-fit: cover`, which keeps the whole of one
-- axis and trims the other equally at both ends unless told otherwise.
-- `object-position` is what tells it otherwise, and it takes
-- percentages. A fraction in 0..1 maps to it directly, and — this is
-- the reason it is a fraction rather than pixels — it survives the
-- source changing resolution. The rail renders whichever preview rung
-- the ladder serves at the viewport it is drawn at; a pixel offset
-- measured against the original would be wrong at every rung, and a
-- fraction is right at all of them.
--
-- Two DOUBLE PRECISION columns rather than one composite/point type:
-- `point` has no useful operator here, arrives in Go as a bespoke type,
-- and would need a codec registration for a value that is two numbers.
-- DOUBLE PRECISION is what the sqlc mapping conventions give a *float64
-- without ceremony.
--
-- Both nullable, and NULL means CENTRE — the CSS default, and what
-- every collection has today. That keeps "the curator has not
-- positioned this" distinct from "the curator positioned it dead
-- centre", which matters because the second is a choice the editor's
-- Reset control has to be able to express as a clear.
--
-- The CHECK bounds them to 0..1 rather than trusting the handler. A
-- fraction outside that range is not a positioning the rail can honour;
-- it is a bug in whichever client sent it, and the constraint is where
-- that bug stops instead of quietly becoming an object-position of
-- -240%.
--
-- The two are constrained TOGETHER (both NULL or both set) because a
-- focal point is a point. Half of one is not a weaker positioning, it
-- is an unanswerable one — the read path would have to invent the
-- missing axis, and inventing it as 0.5 is the same as not having
-- stored the other half either.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.collections
    ADD COLUMN featured_cover_asset_id UUID NULL
        REFERENCES public.assets(id) ON DELETE SET NULL,
    ADD COLUMN featured_cover_focal_x DOUBLE PRECISION NULL,
    ADD COLUMN featured_cover_focal_y DOUBLE PRECISION NULL;

ALTER TABLE public.collections
    ADD CONSTRAINT collections_featured_cover_focal_check CHECK (
        (featured_cover_focal_x IS NULL AND featured_cover_focal_y IS NULL)
        OR (featured_cover_focal_x BETWEEN 0 AND 1
            AND featured_cover_focal_y BETWEEN 0 AND 1)
    );

COMMENT ON COLUMN public.collections.featured_cover_asset_id IS
    'Curator-chosen cover for the FEATURED RAIL specifically (#1207). The rail card is locked to 890:500 while a collection card is roughly square, so one picture is not the best answer for both. NULL means no separate choice: the rail falls back to cover_asset_id, then to the derived hero-card cover, each rung re-checked against the viewer''s picture plane so a withheld cover falls back rather than rendering blank. ON DELETE SET NULL, and does NOT federate — same reasoning as cover_asset_id (see migration 00046).';

COMMENT ON COLUMN public.collections.featured_cover_focal_x IS
    'Horizontal focal point for the featured rail''s 890:500 crop, as a FRACTION of the picture''s width (0 = left edge, 1 = right edge). Maps directly to CSS object-position, and is a fraction rather than a pixel offset so it stays correct across preview rungs and viewport sizes. NULL means centre (the CSS default), which is distinct from an explicit 0.5 so the editor''s reset is a clear rather than a re-set. Paired with featured_cover_focal_y by collections_featured_cover_focal_check: both NULL or both in 0..1.';

COMMENT ON COLUMN public.collections.featured_cover_focal_y IS
    'Vertical focal point for the featured rail''s 890:500 crop, as a FRACTION of the picture''s height (0 = top edge, 1 = bottom edge). See featured_cover_focal_x for why it is a fraction, why NULL means centre, and why the two are constrained together.';

-- The same partial-index argument 00046 made for cover_asset_id, for
-- the same access pattern: the rail joins FROM the collection TO the
-- asset by this column and never searches for the collections pointing
-- at a given asset, and the non-NULL rows are a small minority.
CREATE INDEX collections_featured_cover_asset_id_idx
    ON public.collections (featured_cover_asset_id)
    WHERE featured_cover_asset_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS public.collections_featured_cover_asset_id_idx;
ALTER TABLE public.collections
    DROP CONSTRAINT IF EXISTS collections_featured_cover_focal_check;
ALTER TABLE public.collections
    DROP COLUMN featured_cover_asset_id,
    DROP COLUMN featured_cover_focal_x,
    DROP COLUMN featured_cover_focal_y;
