// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The masonry wall's RENDERED column width, published for the cards
// inside it (#1047).
//
// # Why a card needs to know
//
// The owner's amendment to masonry's identity: at large tile scales the
// wall shows the full #1111 overlay (kind, title, artist, ⋯); at small
// scales it stays art-only with the two minimal hover affordances. A
// masonry tile is as wide as its column, so "large" is a question about
// the column and only MasonryColumns can answer it.
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
// So the rule is a measured number, and this store is how the measured
// number reaches the card. The rest of the geometry stays private to
// MasonryColumns — only the width crosses, because only the width is a
// question something outside the layout asks.
//
// # Why a module singleton rather than a prop
//
// ContentGrid renders the card through a `Snippet<[item, mode]>` the
// PAGE declares, so a value produced inside MasonryColumns cannot reach
// the card without widening that contract at all seven call sites — for
// one number that exactly one of the five layouts produces. There is one
// masonry wall per page (it is a whole-page browse layout, not a
// component that repeats), so a singleton says something true.
//
// It is deliberately WRITE-ONE-READ-MANY and carries no behaviour: the
// card reads a number and decides its own posture. Nothing here knows
// what an overlay is.

/** Live rendered column width of the mounted masonry wall, in CSS px.
 *  0 before the first measure — which is the honest answer, and the one
 *  that resolves to "minimal", so a tile never flashes a full overlay
 *  before the wall has been measured. */
class MasonryLayout {
  colWidth = $state(0);

  /** Called by MasonryColumns' `measure()`. Idempotent. */
  set(colWidth: number): void {
    if (this.colWidth !== colWidth) this.colWidth = colWidth;
  }

  /** Called when the wall unmounts, so a card rendered by some OTHER
   *  surface can never read a width left behind by a masonry that is no
   *  longer on screen. */
  clear(): void {
    this.colWidth = 0;
  }
}

export const masonryLayout = new MasonryLayout();

/**
 * The column width at or above which a masonry tile shows the FULL
 * #1111 overlay instead of the minimal hover affordances.
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
 * Two consequences worth knowing before changing it. The default rung
 * lands at 365px at 1920, so the wall's ordinary appearance IS the full
 * overlay and the art-only posture is what a reader gets by deliberately
 * stepping down. And because this is a width and not a rung, the SAME
 * number puts the transition at a different rung on a different viewport
 * — which is the whole point (#1025): at 2560 the rungs land elsewhere
 * and the overlay appears at whichever of them first crosses 280.
 */
export const MASONRY_OVERLAY_MIN_COL_PX = 280;
