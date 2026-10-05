-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00062_post_cover_focal.sql
--
-- A post's cover picture gets a focal point (#1210).
--
-- ## Why a post needs one, and why exactly one
--
-- The browse GRID is the only post surface that CROPS its cover.
-- PostCard passes `fill={mode === 'grid'}` and
-- `variableAspect={mode === 'masonry'}`, and CardThumb turns `fill`
-- into `object-fit: cover` on an `aspect-square` frame. Masonry takes
-- the picture's own shape, and feed, thumbnail, band and list letterbox
-- it whole onto the matte. A focal point says nothing about a picture
-- that is being shown whole, so grid is the only consumer there can
-- be.
--
-- So there is ONE destination shape (a square) and therefore ONE focal
-- pair, unlike `collections`, which carries two because the featured
-- rail's 890:500 card and the collection tile are different shapes and
-- one fraction cannot be right for both.
--
-- ## Which of the two cover columns it belongs to
--
-- A post has `cover_asset_id` and `cover_thumbnail_asset_id`, and the
-- pair below attaches to WHATEVER THE CARD RESOLVES, which today is
-- `cover_asset_id` falling back to the first member (PostCard's
-- `coverAssetId`). `cover_thumbnail_asset_id` renders nowhere that
-- crops: the one surface reading it is the collection cover mosaic
-- (`cover_thumbnail_asset_id ?? cover_asset_id`, ADR 0088 / #1236),
-- and a mosaic tile is deliberately never positioned, because a focal
-- point is a statement about ONE picture filling a tile and there is no
-- meaningful place to apply it across four. A collection whose mosaic
-- collapses to a single cover positions it with the COLLECTION's own
-- `cover_focal_*`, not the post's.
--
-- Adding a second pair for a column nothing crops would be storage for
-- a rendering that does not exist, so it is deliberately absent.
--
-- ## The value's meaning
--
-- Identical to `collections.cover_focal_*`: a FRACTION of the ORIGINAL
-- picture, mapping straight onto CSS `object-position`, NULL meaning
-- centre. Fractions rather than pixel offsets so the value stays
-- correct across preview rungs and viewport sizes, and NULL rather than
-- an explicit 0.5 so the editor's reset is a clear rather than a
-- re-set.
--
-- The two are constrained TOGETHER, as 00055 constrains the collection
-- pairs: a focal point is a point, and half of one is unanswerable
-- rather than weaker.
--
-- No zoom column. `collections` has `cover_zoom` (00056) because a
-- curator framing a 2.4:1 panorama into an 890:500 band wanted to
-- tighten it; a post cover has one destination and no rail, and adding
-- a slider nobody asked for would be a second unshipped control to keep
-- in step with the crop stage. If it is wanted later it is one nullable
-- column and one CHECK, exactly as 00056 was.
--
-- No backfill: every existing post is centred, which is what NULL
-- means, so the migration writes no rows.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.posts
    ADD COLUMN cover_focal_x DOUBLE PRECISION NULL,
    ADD COLUMN cover_focal_y DOUBLE PRECISION NULL;

ALTER TABLE public.posts
    ADD CONSTRAINT posts_cover_focal_check CHECK (
        (cover_focal_x IS NULL AND cover_focal_y IS NULL)
        OR (cover_focal_x BETWEEN 0 AND 1 AND cover_focal_y BETWEEN 0 AND 1)
    );

COMMENT ON COLUMN public.posts.cover_focal_x IS
    'Horizontal focal point for the post cover''s SQUARE crop, as a FRACTION of the picture''s width (0 = left edge, 1 = right edge, #1210). The square is the destination shape because the browse GRID tile is the only post surface that crops: PostCard sets CardThumb''s `fill` in grid alone, and that is `object-fit: cover` on an `aspect-square` frame. Masonry takes the picture''s own shape and feed, thumbnail, band and list letterbox it whole, so none of them can act on this. Maps directly to CSS object-position, and is a fraction rather than a pixel offset so it stays correct across preview rungs and viewport sizes. NULL means centre (the CSS default), distinct from an explicit 0.5 so the editor''s reset is a clear rather than a re-set. Paired with cover_focal_y by posts_cover_focal_check: both NULL or both in 0..1. ⚠️ Chosen against the ORIGINAL picture, so a consumer honours it by rendering a `contain` rung with object-position; applying it to `col` crops an already-centre-cropped square and lands somewhere nobody picked.';

COMMENT ON COLUMN public.posts.cover_focal_y IS
    'Vertical focal point for the post cover''s square crop, as a FRACTION of the picture''s height (0 = top edge, 1 = bottom edge, #1210). See cover_focal_x for the destination shape, why it is a fraction, why NULL means centre, why the two are constrained together, and why it must be painted from a contain rung.';

-- +goose Down

ALTER TABLE public.posts
    DROP CONSTRAINT IF EXISTS posts_cover_focal_check;
ALTER TABLE public.posts
    DROP COLUMN cover_focal_x,
    DROP COLUMN cover_focal_y;
