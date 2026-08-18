-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00056_collection_cover_zoom.sql
--
-- The cover crop box gains ZOOM, so a subject sitting off to one side
-- can be framed (#1212).
--
-- # What 00055 could not express
--
-- 00055 gave both cover slots a focal POINT, and the marquee that
-- writes it is the largest rectangle of the destination's aspect that
-- fits inside the picture. That rectangle always has one axis equal to
-- the whole picture — `object-fit: cover` keeps one axis whole and
-- trims the other — so exactly one axis can travel. A portrait picture
-- in the rail's 890:500 window is pinned to the full width: the crop
-- can slide up and down and never left or right, and a subject in the
-- left half can never be brought to the middle. The owner's finding was
-- exactly that: "I can't center items that are left aligned."
--
-- The missing capability is not a second focal point or a wider
-- marquee. It is a SMALLER one. Once the crop window is smaller than
-- the fitting rectangle, BOTH axes gain travel, and the focal pair
-- 00055 already stores addresses the whole picture instead of one line
-- through it. Zoom is the single number that shrinks it.
--
-- # Why a number per SLOT, not one per collection
--
-- Same argument 00055 made for two focal pairs, and it lands the same
-- way: a zoom factor is only meaningful against a destination shape.
-- The framing that centres a face in the rail's 890:500 band is not the
-- framing that centres it in the collection card's 4:3 tile, and the
-- amount you have to tighten to get there differs with the shape. One
-- column shared between the two slots would be right for one of them by
-- accident.
--
--   featured_cover_zoom  the rail's card, locked to 890:500 (#1110)
--   cover_zoom           the collection card's 4:3 tile
--
-- # Why NULL rather than a DEFAULT of 1
--
-- NULL means FIT — the largest rectangle that fits, which is exactly
-- what 00055's marquee does and therefore exactly what every existing
-- collection renders today. Not defaulting to 1 is what makes that
-- guarantee structural rather than a claim: a client that has never
-- heard of zoom sends nothing, reads nothing, and paints what it always
-- painted, and the regression that matters here — an existing
-- collection changing appearance — cannot happen because no row's value
-- changed.
--
-- NULL and an explicit 1 render identically and are stored differently
-- on purpose, for 00055's reason: the editor's reset is a CLEAR, so
-- "the curator never zoomed" has to stay distinguishable from "the
-- curator zoomed and came back to fit". Every read path therefore tests
-- `IS NOT NULL` and never truthiness — a zoom of 1 is a value, and the
-- COALESCE trap #1081 closed on this very table is what a truthiness
-- test would walk back into.
--
-- # Why DOUBLE PRECISION, and why the CHECK is 1..4
--
-- DOUBLE PRECISION for the same reason the focal columns are: it is
-- what the sqlc mapping conventions give a *float64 without ceremony,
-- and the value is genuinely continuous — a slider writes 1.37.
--
-- The lower bound is a hard geometric fact, not a preference. Window =
-- fit-window ÷ zoom, so a zoom below 1 asks for a window LARGER than
-- the picture: there are no pixels there and no way to render it. 1 is
-- the fit itself.
--
-- THE UPPER BOUND COMES FROM THE PREVIEW LADDER'S REAL RUNGS, not from
-- taste. A cover carrying a crop is painted from a CONTAIN rung — `col`
-- is `fit: cover` at 320px, a square already cropped at the centre, so
-- positioning applied to it crops a crop (00055's warning). The contain
-- rungs this install ships are `preview` 1024, `screen` 1920 and
-- `hires` 4096 (sysconfig/previews.go), and `preview` is the one a
-- cover is GUARANTEED to have — it is what `CollectionCover.
-- preview_available` reports. Zooming to z feeds the card 1/z of the
-- picture's fitted width, so it demands z times the source pixels per
-- CSS pixel; the browser answers that by climbing the srcset, and the
-- top rung is 4096 — exactly four times `preview`. At 4 the ladder has
-- one more rung to give; past 4 it has none, and every further step is
-- upscaling bytes the server never made. So: 4.
--
-- The CHECK is where a client bug stops, exactly as the focal range
-- check is. A zoom of 40 is not a stronger framing, it is a request for
-- a 25-pixel window rendered at card size, and the constraint refuses
-- it here instead of letting it become a smeared tile nobody chose.
--
-- # Why NOT paired with the focal columns
--
-- The focal pair is constrained both-or-neither because half a point is
-- unanswerable. Zoom is not half of anything: "tightened, still
-- centred" is a complete and ordinary framing, and so is "positioned at
-- fit". The two settings are independent, each with its own NULL, and
-- the API gives each its own clear flag for the same reason.
--
-- Plain DDL, so no StatementBegin/End markers.

-- +goose Up

ALTER TABLE public.collections
    ADD COLUMN featured_cover_zoom DOUBLE PRECISION NULL,
    ADD COLUMN cover_zoom DOUBLE PRECISION NULL;

ALTER TABLE public.collections
    ADD CONSTRAINT collections_featured_cover_zoom_check CHECK (
        featured_cover_zoom IS NULL OR featured_cover_zoom BETWEEN 1 AND 4
    );

ALTER TABLE public.collections
    ADD CONSTRAINT collections_cover_zoom_check CHECK (
        cover_zoom IS NULL OR cover_zoom BETWEEN 1 AND 4
    );

COMMENT ON COLUMN public.collections.featured_cover_zoom IS
    'How far the featured rail''s 890:500 crop is tightened, as a multiplier on the fitting rectangle (#1212). The crop window is the fit window divided by this, so 1 is the fit itself and 2 shows a quarter of the area. NULL means fit — what every collection rendered before this column existed, and what a client that has never heard of zoom keeps rendering. NULL and an explicit 1 paint the same picture and are stored differently on purpose, so the editor''s reset stays a clear. Bounded 1..4 by collections_featured_cover_zoom_check: below 1 the window would exceed the picture, and above 4 the preview ladder has no further contain rung to climb to (`hires` 4096 is 4x `preview` 1024, the rung a cover is guaranteed).';

COMMENT ON COLUMN public.collections.cover_zoom IS
    'How far the collection card''s 4:3 crop is tightened (#1212). Separate from featured_cover_zoom because the two destinations are different shapes and the tightening that frames a subject in a wide band is not the one that frames it in a 4:3 tile. See featured_cover_zoom for why NULL means fit, why NULL differs from 1, and where the 1..4 bound comes from. ⚠️ Chosen against the ORIGINAL picture, like the focal pair it travels with, so a consumer honours it by painting a `contain` rung.';

-- +goose Down

ALTER TABLE public.collections
    DROP CONSTRAINT IF EXISTS collections_featured_cover_zoom_check;
ALTER TABLE public.collections
    DROP CONSTRAINT IF EXISTS collections_cover_zoom_check;
ALTER TABLE public.collections
    DROP COLUMN featured_cover_zoom,
    DROP COLUMN cover_zoom;
