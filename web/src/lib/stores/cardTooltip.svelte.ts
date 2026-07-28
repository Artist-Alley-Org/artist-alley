// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Hover tooltip for masonry tiles (#652) — a singleton, mounted once in
// the root layout.
//
// # Why the info left the tile
//
// Every other mode paints the title in a gradient overlay across the
// bottom of the artwork. Masonry is the one mode where that is
// self-defeating: since #646 its tiles are the shape of their images,
// so a 5.33:1 audio waveform is ~50px tall and a three-line gradient
// covers the entire work it is describing. Masonry exists to scan a lot
// of things at once; covering each thing to caption it removes the
// reason to be in the mode. The owner's call: strip the overlay to the
// two controls (checkbox + ⋮) and put the description next to the
// cursor instead.
//
// # Why a singleton and not a tooltip per card
//
// Flicker. With per-card state, moving from tile A to tile B unmounts
// A's tooltip and starts B's show-delay from cold — so a slow drag
// across a dense wall strobes. One shared tooltip can distinguish the
// two cases that actually differ:
//
//   COLD (nothing showing) — wait SHOW_DELAY_MS. A tooltip that appears
//     instantly on a wall this dense is a strobe light; you cross a
//     dozen tiles just reaching for the scrollbar.
//   WARM (already showing, or just was) — SWITCH_DELAY_MS. The tooltip
//     never hides, it just swaps its contents, which is what stops the
//     flicker. Still debounced, or a fast sweep animates every title in
//     the row.
//
// Leaving schedules a hide on a short grace (HIDE_DELAY_MS) so the 1px
// gutter between two tiles doesn't blink it off and on, and the warm
// window outlives the hide by WARM_MS so a pause-move-pause reads as
// one gesture.
//
// # Positioning: cursor for x, tile for y
//
// Cursor-following in both axes is the lighter read but it jitters, and
// on a 60px tile it would sit ON the artwork it describes. Anchoring to
// the tile in both axes is calm but ignores where the user is actually
// looking on a 2000px-wide wall. So: x follows the cursor (the owner
// asked for "next to the mouse"), y is pinned just outside the tile's
// bottom edge — flipping above when there is no room below. The
// tooltip therefore never covers the tile it is about to describe, and
// it does not jitter vertically while you move within one tile.
//
// The tooltip is `pointer-events: none` and `aria-hidden`: it must not
// trap the pointer over the card's click target, and it must not steal
// focus. It is deliberately NOT wired as an `aria-describedby` tooltip —
// it appears on hover only, and the same title is already the card
// link's accessible name, so announcing it twice would be noise. See
// CardTooltip.svelte for the keyboard/touch reasoning.

/** Cold start. Long enough that crossing tiles on the way somewhere
 *  shows nothing; short enough that a deliberate pause feels answered.
 *  Verified by hand on the dev wall — 200ms still strobed on a sweep,
 *  500ms felt broken on a deliberate hover. */
const SHOW_DELAY_MS = 350;
/** Warm swap between adjacent tiles. */
const SWITCH_DELAY_MS = 90;
/** Grace on leave, so a gutter crossing doesn't blink. */
const HIDE_DELAY_MS = 90;
/** How long after hiding the tooltip still counts as warm. */
const WARM_MS = 500;

export interface CardTooltipContent {
  title: string;
  /** Short supporting facts, rendered dot-separated on one line. Keep
   *  it to the scan-level facts — this is not the details card. */
  meta: string[];
}

interface Pending extends CardTooltipContent {
  key: string;
  x: number;
  anchorTop: number;
  anchorBottom: number;
}

class CardTooltipStore {
  visible = $state(false);
  title = $state('');
  meta = $state<string[]>([]);
  /** Cursor clientX at the moment the tooltip committed. */
  x = $state(0);
  /** The hovered tile's viewport rect edges — the tooltip sits outside
   *  one of them. */
  anchorTop = $state(0);
  anchorBottom = $state(0);

  #pending: Pending | null = null;
  #key: string | null = null;
  #showTimer: ReturnType<typeof setTimeout> | null = null;
  #hideTimer: ReturnType<typeof setTimeout> | null = null;
  #warmTimer: ReturnType<typeof setTimeout> | null = null;
  #warm = false;

  /** Fine pointers only. A touch device has no hover state to hang this
   *  off — and per #578 it already shows the ⋮ menu at rest, which is
   *  the affordance that matters there. Evaluated per call rather than
   *  cached: a 2-in-1 can change primary pointer mid-session. */
  get enabled(): boolean {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false;
    return window.matchMedia('(hover: hover) and (pointer: fine)').matches;
  }

  #clearShow() {
    if (this.#showTimer) clearTimeout(this.#showTimer);
    this.#showTimer = null;
  }
  #clearHide() {
    if (this.#hideTimer) clearTimeout(this.#hideTimer);
    this.#hideTimer = null;
  }

  #commit() {
    const p = this.#pending;
    if (!p || p.key !== this.#key) return;
    this.title = p.title;
    this.meta = p.meta;
    this.x = p.x;
    this.anchorTop = p.anchorTop;
    this.anchorBottom = p.anchorBottom;
    this.visible = true;
    this.#warm = true;
    if (this.#warmTimer) clearTimeout(this.#warmTimer);
    this.#warmTimer = null;
  }

  /** Pointer entered a tile. `key` identifies the tile so a stale timer
   *  from a tile the pointer already left can't commit. */
  enter(key: string, content: CardTooltipContent, e: MouseEvent) {
    if (!this.enabled) return;
    this.#clearHide();
    this.#clearShow();
    this.#key = key;
    this.#pending = { key, ...content, ...rectOf(e) };
    this.#showTimer = setTimeout(() => this.#commit(), this.#warm ? SWITCH_DELAY_MS : SHOW_DELAY_MS);
  }

  /** Pointer moved within a tile. Only tracks while the tooltip for that
   *  tile has not committed yet — once it is up, it stays put rather
   *  than chasing the cursor. */
  move(key: string, e: MouseEvent) {
    const p = this.#pending;
    if (!p || p.key !== key || this.#showTimer === null) return;
    Object.assign(p, rectOf(e));
  }

  /** Pointer left a tile. Scheduled, not immediate: entering the next
   *  tile cancels this, which is what makes a swap not a flicker. */
  leave(key: string) {
    if (this.#key !== key) return;
    this.#clearShow();
    this.#key = null;
    this.#pending = null;
    this.#clearHide();
    this.#hideTimer = setTimeout(() => this.hide(), HIDE_DELAY_MS);
  }

  /** Hide now and start the warm window running down. Also the escape
   *  hatch for scroll / navigation, where the anchor rect goes stale. */
  hide() {
    this.#clearShow();
    this.#clearHide();
    this.#key = null;
    this.#pending = null;
    if (!this.visible) return;
    this.visible = false;
    if (this.#warmTimer) clearTimeout(this.#warmTimer);
    this.#warmTimer = setTimeout(() => {
      this.#warm = false;
      this.#warmTimer = null;
    }, WARM_MS);
  }
}

function rectOf(e: MouseEvent): { x: number; anchorTop: number; anchorBottom: number } {
  const el = e.currentTarget as HTMLElement | null;
  const r = el?.getBoundingClientRect();
  return {
    x: e.clientX,
    anchorTop: r?.top ?? e.clientY,
    anchorBottom: r?.bottom ?? e.clientY,
  };
}

export const cardTooltip = new CardTooltipStore();
