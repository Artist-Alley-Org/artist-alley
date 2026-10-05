-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00063_collection_cover_focal_comment.sql
--
-- The collection cover's focal point does NOT describe a square (#1334).
--
-- ## What is actually wrong
--
-- No column, constraint, index or stored value changes here. What
-- changes is a SENTENCE, and the sentence is load-bearing: sqlc lifts
-- Postgres column comments into the generated Go doc comments, so the
-- claim written into 00055 was reprinted verbatim into every
-- `models.go` in the tree and read back by everyone who went looking
-- for the destination shape.
--
-- 00055 said the destination is a square, "because `col` is fit=cover
-- at 320px, a 320x320 centre-crop, and that rendition is what every
-- small collection thumbnail is made of". Every clause of that is true
-- and the conclusion does not follow. `col` is a SOURCE. The
-- destination is whatever paints the pixels, and what paints a chosen
-- collection cover is CollectionCard's tile, which is
-- `aspect-[4/3]` (web/src/lib/components/CollectionCard.svelte) on the
-- hub, on a profile and in search results.
--
-- The frontend already had this right: `COLLECTION_CARD_ASPECT = 4 / 3`
-- in web/src/lib/util/featuredCrop.ts, with the correction written out
-- beside it, and the cover editor's marquee has been locking to 4:3
-- since #1207 shipped. So the shape a curator drags against and the
-- shape the card renders have always agreed with each other; the
-- database comment was the only place still teaching the square, and
-- an out-of-date explanation next to a correct implementation is worse
-- than none, because the next reader believes it and "fixes" the code.
--
-- The general rule, worth keeping: A CROP MARQUEE LOCKS TO THE
-- DIMENSIONS OF THE THING THAT RENDERS IT. A rendition that happens to
-- be square somewhere upstream is not that thing.
--
-- ## Why the posts pair is untouched
--
-- 00062 says the POST cover's destination is a square, and it is
-- right: it names the surface (`PostCard` sets CardThumb's `fill` in
-- grid alone, giving `object-fit: cover` on an `aspect-square` frame)
-- and enumerates what the other modes do instead. Same reasoning, a
-- different answer, because posts and collections render on different
-- cards. That comment is the model this one now follows.
--
-- ## Why a migration rather than an edit to 00055
--
-- 00055 has been applied everywhere. Rewriting its text would change
-- what a FRESH database is told and leave every existing one still
-- carrying the wrong sentence, which is the divergence the comment
-- exists to prevent. So the correction ships forward like any other
-- schema change, and Down restores 00055's exact wording so the
-- sequence stays reversible.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

COMMENT ON COLUMN public.collections.cover_focal_x IS
    'Horizontal focal point for the collection cover''s 4:3 crop, as a FRACTION of the picture''s width (0 = left edge, 1 = right edge, #1207). THE DESTINATION IS 4:3, NOT A SQUARE (#1334): CollectionCard paints a chosen cover inside an `aspect-[4/3]` tile on the hub, on a profile and in search, and that tile is the only collection surface that crops this picture. The square is the tempting wrong answer because `col` IS one (fit=cover at 320px, a 320x320 centre-crop, the rendition every small collection thumbnail is made of), but `col` is a SOURCE and not a destination; a curator positioned against it would be shown a region the card never displays. A crop marquee locks to the dimensions of the thing that renders it. Separate from featured_cover_focal_x because the rail card is 890:500, and one fraction cannot be right for two shapes. Maps directly to CSS object-position, and is a fraction rather than a pixel offset so it stays correct across preview rungs and viewport sizes. NULL means centre (the CSS default), distinct from an explicit 0.5 so the editor''s reset is a clear rather than a re-set. Paired with cover_focal_y by collections_cover_focal_check: both NULL or both in 0..1. Cleared when the cover picture is swapped or removed and no new framing is supplied (#1333), because a fraction chosen against one photograph means nothing on the next. ⚠️ Chosen against the ORIGINAL picture, so a consumer honours it by rendering a `contain` rung with object-position; applying it to `col` crops an already-centre-cropped square and lands somewhere nobody picked.';

COMMENT ON COLUMN public.collections.cover_focal_y IS
    'Vertical focal point for the collection cover''s 4:3 crop, as a FRACTION of the picture''s height (0 = top edge, 1 = bottom edge, #1207). See cover_focal_x for the destination shape (4:3, not a square, #1334), why it is a fraction, why NULL means centre, why the two are constrained together, when a cover swap clears them, and why it must be painted from a contain rung.';

-- The neighbouring column carries the same misapprehension in
-- passing ("a collection card is roughly square"), which is where
-- the square came from in the first place. Corrected here so the
-- two columns cannot disagree.
COMMENT ON COLUMN public.collections.featured_cover_asset_id IS
    'Curator-chosen cover for the FEATURED RAIL specifically (#1207). The rail card is locked to 890:500 while a collection card is 4:3 (#1334), so one picture is not the best answer for both. NULL means no separate choice: the rail falls back to cover_asset_id, then to the derived hero-card cover, each rung re-checked against the viewer''s picture plane so a withheld cover falls back rather than rendering blank. ON DELETE SET NULL, and does NOT federate — same reasoning as cover_asset_id (see migration 00046).';

-- +goose Down

COMMENT ON COLUMN public.collections.cover_focal_x IS
    'Horizontal focal point for the collection cover''s SQUARE crop, as a FRACTION of the picture''s width (#1207). The square is the destination shape because `col` is fit=cover at 320px — a 320x320 centre-crop — and that rendition is what every small collection thumbnail is made of. Separate from featured_cover_focal_x because the two destinations are different shapes and one fraction cannot be right for both. NULL means centre. ⚠️ Chosen against the ORIGINAL picture, so a consumer honours it by rendering a `contain` rung with object-position; applying it to `col` crops an already-centre-cropped square and is wrong.';

COMMENT ON COLUMN public.collections.cover_focal_y IS
    'Vertical focal point for the collection cover''s square crop (#1207). See cover_focal_x.';

COMMENT ON COLUMN public.collections.featured_cover_asset_id IS
    'Curator-chosen cover for the FEATURED RAIL specifically (#1207). The rail card is locked to 890:500 while a collection card is roughly square, so one picture is not the best answer for both. NULL means no separate choice: the rail falls back to cover_asset_id, then to the derived hero-card cover, each rung re-checked against the viewer''s picture plane so a withheld cover falls back rather than rendering blank. ON DELETE SET NULL, and does NOT federate — same reasoning as cover_asset_id (see migration 00046).';
