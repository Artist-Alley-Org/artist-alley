// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The masonry wall's RENDERED TILE BOXES, published for the cards
// inside it (#1047, corrected in #1139).
//
// # Why a card needs to know
//
// The owner's amendment to masonry's identity: at large tile scales the
// wall shows the full #1111 overlay (kind, title, artist, ⋯); at small
// scales it stays art-only with the two minimal hover affordances. Only
// the wall knows how big a tile came out, so only the wall can answer.
//
// # Why the threshold is in PIXELS and not in rungs
//
// The obvious spelling — "the top three rungs of the stepper" — is
// wrong, and #1025 is where that was learned: a rung names a
// `--tile-min` CLAMP, and the column width it produces depends on the
// viewport. 22rem yields 5 columns of ~370px at 1920 and 7 of ~355px at
// 2560; 38rem yields ~620px at 1920 and ~640px at 2560, and at 1280 the
// same rung can collapse to one column of 1240px. A rung index is
// therefore not a width, and a rule written in rungs shows the overlay
// at sizes it does not fit and hides it at sizes where it would.
//
// # Why the whole BOX and not just the column width (#1139)
//
// The first cut of this store published one number — the column width —
// and the card asked "am I at least 280px wide?". Two things were wrong
// with that, and the owner found both in one screenshot:
//
//  1. A WIDE TILE IS SHORT BY CONSTRUCTION. A 5.33:1 piece spanning two
//     269px columns is 550px wide and ~103px tall. It cleared the width
//     gate with room to spare and could not contain the overlay stack,
//     so the identity row — avatar and artist name — was cut off at the
//     tile's bottom edge. Measured before the fix, at the default rung
//     on a 1920 viewport: a 738×89 span-2 tile whose identity block ran
//     to 106px, i.e. 17px past the picture. Height is not a proxy for
//     width on a wall whose entire premise is unequal heights.
//  2. The column width is not the TILE's width once spans exist
//     (ADR 0079). A span-2 tile in a 250px column is 512px wide and was
//     being judged as 250 — refused an overlay it had ample room for.
//
// So the store carries the tile's measured BOX, keyed by tile id, and
// the card asks a question about itself rather than about its column.
//
// # Measured, not predicted
//
// The wall already predicts each tile's height to place it
// (`tileHeightPx`), and publishing that prediction would have been less
// code. It would also have been wrong exactly where it matters: the
// prediction is only as good as the ratio it has, and a tile with no
// recorded dimensions is predicted SQUARE — tall — while rendering at
// whatever shape its bytes turn out to be. That is precisely the tile
// that ends up short with an overlay sized for a square. `snapshotTiles`
// already reads every tile's real box on each placement pass, for the
// reconciler; this rides the same read and cannot be wrong about
// whether the overlay fits.
//
// The measured box is the TILE wrapper, which in masonry is the card
// root — the picture frame plus the 2px matte inset on each side
// (`p-[2px]`, #596). The thresholds below are calibrated against that
// same measured number, so the 4px is already inside them; do not
// "correct" for it.
//
// # Why a module singleton rather than a prop
//
// ContentGrid renders the card through a `Snippet<[item, mode]>` the
// PAGE declares, so a value produced inside MasonryColumns cannot reach
// the card without widening that contract at all seven call sites.
//
// ⚠️ "ONE WALL PER PAGE" STOPPED BEING TRUE IN #1118 and the singleton
// survived it — but only because `clear()` learned to count. The promo
// band splits the browse feed into two walls with the band between them,
// so two MasonryColumns are mounted at once. The MAP is fine either way
// (the key is a tile id and the two walls hold disjoint slices), but the
// unmount hook was not: whichever wall tore down first would empty the
// boxes of the one still on screen, and every card on it would drop to
// the `minimal` posture until something resized. See `acquire`/`release`.
//
// It is deliberately WRITE-ONE-READ-MANY and carries no behaviour: the
// card reads its own box and decides its own posture. Nothing here knows
// what an overlay is beyond the tier function at the bottom, which is
// here so the two card components cannot answer the question differently.

import { SvelteMap } from 'svelte/reactivity';

/** One tile's rendered box, in CSS px. */
export interface MasonryTileBox {
  w: number;
  h: number;
}

class MasonryLayout {
  /** Rendered box per tile id. A `SvelteMap` rather than a `$state`
   *  object so a card re-derives only when ITS OWN entry changes — a
   *  placement pass rewrites every entry, and a plain reassignment
   *  would re-run the posture derivation on all ~70 mounted cards for
   *  the benefit of the handful whose size actually moved. */
  private boxes = new SvelteMap<string, MasonryTileBox>();

  /** Called by MasonryColumns' `snapshotTiles()`, once per tile per
   *  placement pass. Idempotent: an unchanged box writes nothing, so a
   *  re-place that moves tiles around without resizing them wakes no
   *  card. */
  set(id: string, w: number, h: number): void {
    const prev = this.boxes.get(id);
    if (prev && prev.w === w && prev.h === h) return;
    this.boxes.set(id, { w, h });
  }

  /** This tile's measured box, or null before the wall has measured it.
   *  Null is the honest answer and it resolves to the MINIMAL posture,
   *  so a tile never flashes a full overlay before it has been sized. */
  box(id: string): MasonryTileBox | null {
    return this.boxes.get(id) ?? null;
  }

  /** How many masonry walls are mounted right now.
   *
   *  A plain number, not `$state`: nothing renders from it, and making
   *  it reactive would wake every card on a mount. */
  private walls = 0;

  /** Called by a wall as it mounts. Pairs with `release`. */
  acquire(): void {
    this.walls++;
  }

  /** Called when a wall unmounts. The boxes are dropped only when the
   *  LAST one goes, which is what preserves #1047's invariant — a card
   *  on some other surface must never read a box left behind by a
   *  masonry that is no longer on screen — now that a page can hold two
   *  walls at once (#1118's promo band splits the feed).
   *
   *  Clearing on every unmount would have been the smaller diff and is
   *  the bug: the surviving wall's cards would read `null` boxes and
   *  drop to the `minimal` posture, silently, until the next resize or
   *  placement pass happened to re-measure them.
   *
   *  The counter floors at zero rather than trusting the pairing. A
   *  negative count would make the next `release` a no-op and leak the
   *  map into the next surface — a stale box being exactly what this
   *  method exists to prevent. */
  release(): void {
    this.walls = Math.max(0, this.walls - 1);
    if (this.walls === 0) this.boxes.clear();
  }
}

export const masonryLayout = new MasonryLayout();

/**
 * The tile width at or above which a masonry tile may show the #1111
 * overlay at all.
 *
 * CALIBRATED AGAINST SCREENSHOTS, not chosen. The overlay's bottom-left
 * identity block is the constraint, and most of it is fixed pixels:
 *
 *     10px  padding                     (the overlay's `p-2.5`)
 *     40px  avatar disc                 (`h-10 w-10`, #1111's measurement)
 *      8px  gap                         (`gap-2`)
 *      ??   the artist's name
 *     44px  the ⋯ menu's reserved lane   (`pr-11`)
 *     10px  padding
 *
 * — 112px of chrome before a single character of the name, and the title
 * on the line above truncates against the same reserved lane. So the
 * question "does the overlay read" is really "how much is left after
 * 112px", and it was answered by forcing the overlay on at every rung of
 * the stepper and looking. Measured on the dev library at a 1920
 * viewport, hovering the first tile:
 *
 *     199px  overlay not offered at this rung; at this width it would be
 *            chrome with a two-character name beside it
 *     225px  "Pexels 12546959 C…" — the title breaks mid-token and the
 *            band covers most of a short tile. NOT legible.
 *     258px  "Pexels 12546959 Color…" — the artist reads, the title
 *            still breaks mid-token. Borderline, and the band is about
 *            half the tile.
 *     303px  "Pexels 12546959 Colors Mot…" — both read; the band is a
 *            caption on a picture rather than a panel over one. LEGIBLE.
 *     365px  the default rung at 1920 — comfortable.
 *
 * The boundary is therefore between 258 and 303, and 280 is the round
 * number inside it: 168px of text lane, roughly twenty characters at the
 * overlay's 12px name / 14px title. Below it the overlay is not merely
 * tight, it is UNTRUE — a title clipped to "Pexels 12546…" beside a name
 * clipped to "Ma…" states less than the hover tooltip it replaced, while
 * covering the art the density exists to show.
 *
 * ⚠️ Since #1139 this is measured against the TILE's width, not its
 * column's. The number did not move — it was always a statement about
 * how much lane the overlay has, and a span-2 tile has two columns of
 * it. What changed is that a spanning tile is now judged on what it
 * actually got.
 */
export const MASONRY_OVERLAY_MIN_W_PX = 280;

/**
 * The tile height at or above which the overlay may show its full
 * IDENTITY ROW (avatar + artist name under the title).
 *
 * CALIBRATED THE SAME WAY, against the same wall (#1139). The overlay is
 * a `justify-between` column, so its two blocks are pinned to the two
 * ends and the height question is whether they meet. Measured at 1920,
 * 16px root, overlays forced visible:
 *
 *     10px  top padding
 *     29px  the kind-badge block          (bottom edge measured at 39px)
 *     67px  the identity block            (title 20 + mt-1 4 + avatar 40,
 *                                          + the ⋯ menu's 44px lane inside it)
 *     10px  bottom padding
 *
 * — so the two blocks TOUCH at exactly 116px and overlap below it. The
 * candidates, on real tiles:
 *
 *      89px  the owner's reported case (a 738×89 span-2 tile). The
 *           identity block runs to 106px — 17px of avatar and artist
 *           name cut off by the tile's bottom edge. This is the bug.
 *     106px  the two blocks abut with ZERO gap: the title sits directly
 *           against the kind badge and the pair reads as one smudge.
 *           Nothing is clipped, and it is still not legible.
 *     116px  touching exactly. Same objection.
 *     150px  34px of clear picture between the badge and the title. The
 *           overlay reads as a caption with the art visible through the
 *           middle. LEGIBLE — this is the floor.
 *     194px  the shortest ORDINARY (span-1) tile on the wall at the
 *           default rung; comfortable.
 *
 * 150 is the round number at the first height that reads. It is not a
 * derivation of the 116px touching point plus a guess: below 150 the
 * band occupies more than 80% of the tile, which on a density whose
 * whole premise is "maximum art per page" is a panel with a picture
 * behind it rather than a picture with a caption on it.
 */
export const MASONRY_OVERLAY_MIN_H_PX = 150;

/**
 * The tile height at or above which the overlay shows a COMPRESSED
 * identity — the title alone, with no avatar and no artist name.
 *
 * The tier exists because the alternative at these heights is a choice
 * between two bad answers: clip the identity row (the bug), or drop
 * straight to art-only and lose the title on a tile that plainly has
 * room for one line. A wide tile is the one shape where a caption is
 * both wanted and cheap.
 *
 * The arithmetic, same wall, same method:
 *
 *     10px  top padding
 *     29px  the kind-badge block
 *     44px  the compressed identity block — the ⋯ menu's own tap target
 *           (`h-11`) is the floor here, NOT the 20px title line. The menu
 *           is `absolute` inside that block and reserves no flex space,
 *           so a block sized to the title alone would let a 44px control
 *           hang 32px past the bottom of the picture. That is the same
 *           clipping bug in a smaller costume, and it is why the
 *           compressed block carries `min-h-11`.
 *     10px  bottom padding
 *
 * — 93px touching, and the same "must not abut" argument puts the floor
 * at 110px: 17px of clear picture between the badge and the caption,
 * which at this scale is the same visual separation 150 buys the full
 * overlay. Below it the tile is heading for the 60px control floor and
 * art-only is the honest posture.
 */
export const MASONRY_OVERLAY_COMPRESSED_MIN_H_PX = 110;

/**
 * How much overlay a masonry tile of this box can carry.
 *
 *   `full`        kind badge, title, avatar + artist name, ⋯
 *   `compressed`  kind badge, title, ⋯ — no identity row
 *   `minimal`     art only, with the checkbox and ⋯ affordances
 *
 * Width qualifies the tile for an overlay AT ALL (there is no narrow
 * tier — a 200px-wide tile has no text lane to compress INTO), and
 * height then decides how much of the stack fits. Both gates must pass,
 * which is the correction #1139 makes: before it, width passed alone.
 *
 * A null box is a tile the wall has not measured yet and resolves to
 * `minimal` — see `box()`.
 */
export type MasonryOverlayTier = 'full' | 'compressed' | 'minimal';

export function masonryOverlayTier(box: MasonryTileBox | null): MasonryOverlayTier {
  if (!box || box.w < MASONRY_OVERLAY_MIN_W_PX) return 'minimal';
  if (box.h >= MASONRY_OVERLAY_MIN_H_PX) return 'full';
  if (box.h >= MASONRY_OVERLAY_COMPRESSED_MIN_H_PX) return 'compressed';
  return 'minimal';
}
