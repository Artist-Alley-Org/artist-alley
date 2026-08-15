// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #651 / #747 — masonry is one explicitly-placed grid, not a balanced
// multi-column flow and no longer N sibling block flows.
//
// Some of this is only observable in a real browser with real scrolling
// (does the wall LOOK right, does a wide tile read as artwork rather
// than a sliver) and those numbers are in the PR. What IS assertable
// here — and what a refactor would otherwise silently drop — is the
// decisions the mechanism rests on:
//
//   1. `cardTileRatio` — the height prediction. It has to agree with
//      CardThumb's `declaredRatio` (same ladder precondition, same clamp,
//      same cover-asset resolution) or the placer sizes tiles against
//      heights nothing actually has. Two copies of that rule in two
//      files is exactly how it drifts, so it lives in one and this pins
//      it.
//   2. `masonryTileHeight` — the #652 floor, written by the renderer as
//      `min-height` and predicted here.
//   3. APPEND-STABILITY. The whole reason the implementation looks the
//      way it does. `placeInto` may never move a tile it has already
//      placed; the test below asserts that on the exact 36 → 72 append
//      #651 measured, and it fails if the placer is made to re-place
//      from the start.
//   4. Spanning placement, ADR 0079 §4 — closest-matched adjacent pair,
//      placed at the greater of the two, both columns resuming from the
//      slot's bottom edge — plus the threshold, the cap and the stepper
//      rule that decide which tiles get one (#1025).
//   5. The accessibility contract. DOM order matches feed order again
//      now that the column boxes are gone, so the list semantics are
//      simpler than they were — but `aria-posinset` / `aria-setsize`
//      stay explicit, because ADR 0079's sized slots will put positions
//      in the stream that are not one tile.

import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import MasonryColumns from './MasonryColumns.svelte';
import MasonryFeedHarness from '../../../vitest-stubs/MasonryFeedHarness.svelte';
import {
  cardTileRatio,
  masonryMinTilePx,
  masonryTileHeight,
  MASONRY_MIN_TILE_REM,
  RATIO_MAX,
  RATIO_MIN,
} from './cardAsset';
import {
  MAX_SPAN,
  RECONCILE_SLOP_ROWS,
  ROW_UNIT_PX,
  emptyState,
  placeAll,
  placeInto,
  reconcile,
  spanWidthPx,
  tileRows,
  tileSpan,
  type MasonryGeometry,
  type MasonryState,
  type PlacedTile,
  type PlaceableTile,
} from './masonryPlacement';
import { createRawSnippet } from 'svelte';

describe('cardTileRatio', () => {
  const assetRow = (over: Record<string, unknown> = {}) => ({
    id: 'a1',
    ladder_available: true,
    pixel_width: 1600,
    pixel_height: 300,
    ...over,
  });

  it('reads an asset row', () => {
    expect(cardTileRatio(assetRow(), true)).toBeCloseTo(1600 / 300);
  });

  it('resolves a post row through its cover asset', () => {
    const post = {
      id: 'p1',
      cover_asset_id: 'a2',
      members: [
        { asset_id: 'a1', asset: assetRow() },
        { asset_id: 'a2', asset: assetRow({ pixel_width: 800, pixel_height: 800 }) },
      ],
    };
    expect(cardTileRatio(post, true)).toBe(1);
  });

  it('falls back to the first member when no cover is set', () => {
    const post = { id: 'p1', members: [{ asset_id: 'a1', asset: assetRow() }] };
    expect(cardTileRatio(post, true)).toBeCloseTo(1600 / 300);
  });

  // The ladder preconditions, both of them. Without a responsive srcset
  // CardThumb can only request `col` — a 320x320 centre CROP — so the
  // SOURCE ratio is not the shape that will be on screen, and predicting
  // from it would balance the columns against a tile that never renders.
  it('declines without the per-asset ladder', () => {
    expect(cardTileRatio(assetRow({ ladder_available: false }), true)).toBeNull();
  });
  it('declines before the install ladder has loaded', () => {
    expect(cardTileRatio(assetRow(), false)).toBeNull();
  });

  it('declines with no recorded dimensions', () => {
    expect(cardTileRatio(assetRow({ pixel_width: null, pixel_height: null }), true)).toBeNull();
    expect(cardTileRatio(assetRow({ pixel_height: 0 }), true)).toBeNull();
  });

  it('declines on a row it cannot read at all', () => {
    expect(cardTileRatio({ id: 'c1', title: 'a collection' }, true)).toBeNull();
    expect(cardTileRatio(null, true)).toBeNull();
  });

  // Guards corrupt metadata, not a design choice — a 4000:1 would
  // compute a sub-pixel tile nobody can see or click.
  it('clamps values that could not be a picture', () => {
    expect(cardTileRatio(assetRow({ pixel_width: 4000, pixel_height: 1 }), true)).toBe(RATIO_MAX);
    expect(cardTileRatio(assetRow({ pixel_width: 1, pixel_height: 4000 }), true)).toBe(RATIO_MIN);
  });
});

// #652 — the tile floor. Same argument as cardTileRatio above: the
// renderer writes it as `min-height` and the placer has to predict the
// identical number, so it is one function and this pins it. A floor in
// CSS only would size tiles against heights they never have.
describe('masonryTileHeight (#652)', () => {
  const MIN = 60;

  it('follows the aspect ratio for any tile above the floor', () => {
    // 1.78:1 and 1:1 in a 300px column — the two #646 must keep exact.
    expect(masonryTileHeight(300, 16 / 9, MIN)).toBeCloseTo(300 / (16 / 9), 5);
    expect(masonryTileHeight(300, 1, MIN)).toBe(300);
  });

  it('floors the tiles a true ratio would make uninteractable', () => {
    // The measured worst case: a 5.33:1 waveform in a 129px column
    // wants 24px, which cannot hold a 44px control.
    expect(masonryTileHeight(129, 16 / 3, MIN)).toBe(MIN);
    // And the clamp ceiling, 12:1, in the widest measured column.
    expect(masonryTileHeight(400, RATIO_MAX, MIN)).toBe(MIN);
  });

  it('floors the no-ratio square reservation too', () => {
    // Not reachable at real column widths, but the branch must not be
    // the one place the floor is missing.
    expect(masonryTileHeight(20, null, MIN)).toBe(MIN);
    expect(masonryTileHeight(300, null, MIN)).toBe(300);
  });

  // The floor is stated in rem because the controls are (`h-11`,
  // `top-2`). A hardcoded 60 would put a 20px-root user's 55px controls
  // straight back outside the tile.
  it('derives the floor from the root font size, not from 16', () => {
    expect(MASONRY_MIN_TILE_REM).toBe(2.75 + 0.5 * 2);
    document.documentElement.style.fontSize = '';
    expect(masonryMinTilePx()).toBeCloseTo(MASONRY_MIN_TILE_REM * 16, 3);
    document.documentElement.style.fontSize = '20px';
    expect(masonryMinTilePx()).toBeCloseTo(MASONRY_MIN_TILE_REM * 20, 3);
    document.documentElement.style.fontSize = '';
  });
});

// ── The placer (#747 / #1025) ────────────────────────────────────────
//
// Every geometry below is MEASURED, not invented: 1440px with the 22rem
// default rung renders 5 columns of 269px with an 8px gap and a 60px
// floor, read out of Chromium on the dev feed.
const WALL_1440: MasonryGeometry = { colCount: 5, colWidth: 269, gapPx: 8, minTilePx: 60 };
/** The 10rem rung on 1920px: 11 columns of ~160px. */
const WALL_DENSE: MasonryGeometry = { colCount: 11, colWidth: 160, gapPx: 8, minTilePx: 60 };
/** The shapes the dev feed actually contains, in the proportion it
 *  contains them — 16:9 and 4:3 stills, portraits, squares, and the
 *  5.33:1 audio waveforms that are a third of the wall. */
const FEED_RATIOS = [16 / 9, 1, 0.75, 4 / 3, 5.33, 1.5, 0.66, 5.33, 2 / 3, 1, 5.33, 8.68];

function feed(count: number, from = 0, geo = WALL_1440): PlaceableTile[] {
  return Array.from({ length: count }, (_, i) => {
    const r = FEED_RATIOS[(from + i) % FEED_RATIOS.length];
    return { id: `t${from + i}`, span: tileSpan(r, geo), estimateRatio: r };
  });
}

/** The same feed with every fourth tile UNDECLARED — a collection card, a
 *  typed-doc plate, an asset whose preview predates #757. Those are the
 *  tiles whose shape is only knowable once they have rendered, so their
 *  `estimateRatio` is null on the first pass and the harvested measurement
 *  by the time the next page arrives (`snapshotTiles`).
 *
 *  This is what makes the append-stability test load-bearing. A
 *  shortest-column placer is prefix-deterministic: re-running it from
 *  scratch over the SAME inputs reproduces the same wall, so a test built
 *  on unchanging shapes passes even if the placer re-places everything on
 *  every pass. It is only when the inputs for an already-placed tile
 *  change under it — which they do, on every real feed — that skipping
 *  the already-placed prefix is the thing keeping tiles still. */
function mixedFeed(count: number, measured: boolean): PlaceableTile[] {
  return feed(count).map((t, i) =>
    i % 4 === 3
      ? { ...t, span: 1, estimateRatio: measured ? 0.6 + (i % 7) * 0.3 : null }
      : t,
  );
}

const areaOf = (p: { col: number; span: number; row: number; rows: number }) =>
  `${p.col}/${p.span}/${p.row}/${p.rows}`;

describe('masonry placement — append-stability (#651)', () => {
  // THE property. #651 exists because the previous multicol re-sorted 19
  // of 24 sampled tiles into other columns on this exact append, while
  // the user was looking at them.
  it('never moves a tile that is already placed, on the 36 → 72 append', () => {
    // The second pass sees UPDATED shapes for the tiles the first pass
    // could only guess at — see `mixedFeed`. Placing them again would
    // move them; not looking at them again is the mechanism.
    const first = placeAll(mixedFeed(36, false), WALL_1440);
    const grown = placeInto(first, mixedFeed(72, true), 36, WALL_1440);

    expect(grown.placements.length).toBe(72);
    let moved = 0;
    for (let i = 0; i < first.placements.length; i++) {
      expect(grown.placements[i].id).toBe(first.placements[i].id);
      if (areaOf(grown.placements[i]) !== areaOf(first.placements[i])) moved++;
    }
    // Quoted in the PR. The multicol implementation this replaced scored
    // 19 of 24; the acceptable answer here is zero, not "few".
    expect(moved).toBe(0);
  });

  it('stays stable across three successive appends', () => {
    let s = placeAll(mixedFeed(24, false), WALL_1440);
    const seen = new Map(s.placements.map((p) => [p.id, areaOf(p)]));
    for (const total of [48, 72, 96]) {
      s = placeInto(s, mixedFeed(total, true), seen.size, WALL_1440);
      for (const p of s.placements) {
        const before = seen.get(p.id);
        if (before !== undefined) expect(areaOf(p)).toBe(before);
        else seen.set(p.id, areaOf(p));
      }
      expect(seen.size).toBe(total);
    }
  });

  it('appends only ever grow a column downward', () => {
    const first = placeAll(feed(36), WALL_1440);
    const grown = placeInto(first, feed(72), 36, WALL_1440);
    for (let c = 0; c < WALL_1440.colCount; c++) {
      expect(grown.colRows[c]).toBeGreaterThanOrEqual(first.colRows[c]);
    }
  });

  // A column-count change is the case that DELIBERATELY re-places
  // everything: the user is resizing, already changing the layout, and
  // expects it to change.
  it('re-places from scratch when the column count changes', () => {
    const wide = placeAll(feed(36), WALL_1440);
    const narrowGeo = { ...WALL_1440, colCount: 3, colWidth: 460 };
    const narrow = placeAll(feed(36, 0, narrowGeo), narrowGeo);
    expect(narrow.colRows.length).toBe(3);
    expect(narrow.placements.some((p, i) => areaOf(p) !== areaOf(wide.placements[i]))).toBe(true);
  });
});

describe('masonry placement — spanning (ADR 0079 §4, #1025)', () => {
  const wide: PlaceableTile = { id: 'w', span: 2, estimateRatio: 8.68 };

  it('lands a 2-wide tile on the closest-matched ADJACENT PAIR', () => {
    // Columns 1 and 2 are the closest-matched pair (Δ2). Column 0 is the
    // shortest, so a shortest-column heuristic would put the tile there
    // and leave a 300-row hole beside it.
    const s = placeInto(
      { placements: [], colRows: [100, 400, 402, 900] },
      [wide],
      0,
      { ...WALL_1440, colCount: 4 },
    );
    const p = s.placements[0];
    expect(p.span).toBe(2);
    expect(p.col).toBe(1);
    // §4 step 2 — placed at the GREATER of the pair's two heights.
    expect(p.row).toBe(403);
    // §4 step 3 — BOTH columns resume from the slot's bottom edge, which
    // is what re-levels the pair.
    expect(s.colRows[1]).toBe(402 + p.rows);
    expect(s.colRows[2]).toBe(402 + p.rows);
    // Untouched columns stay where they were.
    expect(s.colRows[0]).toBe(100);
    expect(s.colRows[3]).toBe(900);
  });

  it('leaves a residual gap of exactly the pair difference, never more', () => {
    const s = placeInto({ placements: [], colRows: [100, 400, 402, 900] }, [wide], 0, {
      ...WALL_1440,
      colCount: 4,
    });
    // The slot's top minus the shorter column of the pair.
    expect(s.placements[0].row - 1 - 400).toBe(2);
  });

  it('breaks a tie towards the higher pair', () => {
    const s = placeInto({ placements: [], colRows: [700, 700, 100, 100] }, [wide], 0, {
      ...WALL_1440,
      colCount: 4,
    });
    expect(s.placements[0].col).toBe(2);
  });

  it('keeps a wall containing spanning tiles append-stable', () => {
    const spanning = (n: number) =>
      feed(n).map((t, i) => (i % 9 === 4 ? { ...t, span: 2 } : t));
    const first = placeAll(spanning(36), WALL_1440);
    expect(first.placements.filter((p) => p.span === 2).length).toBeGreaterThan(0);
    const grown = placeInto(first, spanning(72), 36, WALL_1440);
    for (let i = 0; i < first.placements.length; i++) {
      expect(areaOf(grown.placements[i])).toBe(areaOf(first.placements[i]));
    }
  });

  it('never lets a span run off the end of the wall', () => {
    const s = placeInto({ placements: [], colRows: [0, 0, 0] }, [{ ...wide, span: 2 }], 0, {
      ...WALL_1440,
      colCount: 3,
    });
    expect(s.placements[0].col + s.placements[0].span).toBeLessThanOrEqual(3);
    // Two columns is the whole row at two columns, so it is refused.
    const narrow = placeInto({ placements: [], colRows: [0, 0] }, [{ ...wide, span: 2 }], 0, {
      ...WALL_1440,
      colCount: 2,
    });
    expect(narrow.placements[0].span).toBe(1);
  });

  it('sizes a spanning tile against the width it actually gets', () => {
    // 2 columns is 2*269 + 8 = 546px, not 538 — the span eats the gap.
    expect(spanWidthPx(WALL_1440, 2)).toBe(546);
    const s = placeAll([wide], WALL_1440);
    const p = s.placements[0];
    // 546 / 8.68 = 62.9px, which clears the 60px floor — the whole
    // point of spanning. At one column it would have been 31px.
    expect(p.rows).toBe(tileRows(546 / 8.68, 8));
    expect(masonryTileHeight(269, 8.68, 60)).toBe(60); // floored, i.e. squashed
  });
});

describe('the span threshold (#1025)', () => {
  // The rule is against RENDERED HEIGHT, so the same ratio decides
  // differently at different rungs of the size stepper. An aspect-ratio
  // threshold would be scale-invariant and could not.
  it('spans exactly the ratios whose tile falls under the #652 floor', () => {
    // 269 / 60 = 4.48:1 is the break-even at the 22rem default.
    expect(tileSpan(4.4, WALL_1440)).toBe(1);
    expect(tileSpan(4.6, WALL_1440)).toBe(2);
    expect(tileSpan(5.33, WALL_1440)).toBe(2);
    expect(tileSpan(8.68, WALL_1440)).toBe(2);
  });

  it('moves the threshold with the stepper, not with the ratio', () => {
    // 160 / 60 = 2.67:1 at the dense rung — a 3:1 tile is a sliver there
    // and comfortable at the default.
    expect(tileSpan(3, WALL_1440)).toBe(1);
    expect(tileSpan(3, WALL_DENSE)).toBe(2);
  });

  it('takes the SMALLEST span that clears the floor, never more', () => {
    // Nothing between 1 and the cap to skip at MAX_SPAN 2, so this is
    // pinned against the cap growing later.
    expect(MAX_SPAN).toBe(2);
    expect(tileSpan(1, WALL_DENSE)).toBe(1);
    expect(tileSpan(2.6, WALL_DENSE)).toBe(1);
  });

  it('never spans more than two columns', () => {
    // 12:1, the clamp ceiling, still only gets two. Past that the tile
    // letterboxes onto the #652 floor — the ratio ceiling and that floor
    // are the same box — rather than eating the row.
    expect(tileSpan(RATIO_MAX, WALL_DENSE)).toBe(MAX_SPAN);
    expect(tileSpan(60, WALL_DENSE)).toBe(MAX_SPAN);
    expect(masonryTileHeight(spanWidthPx(WALL_DENSE, MAX_SPAN), RATIO_MAX, 60)).toBe(60);
  });

  it('never spans the whole row — a wide tile keeps a neighbour', () => {
    expect(tileSpan(8.68, { ...WALL_1440, colCount: 3 })).toBe(2);
    expect(tileSpan(8.68, { ...WALL_1440, colCount: 2 })).toBe(1);
    expect(tileSpan(8.68, { ...WALL_1440, colCount: 1 })).toBe(1);
  });

  it('does not span a tile whose shape is not declared', () => {
    // A measured ratio is the shape the tile RENDERS at, which for an
    // already-floored tile is the floored shape — it cannot tell us the
    // tile is being squashed. Span reads the declared ratio alone.
    expect(tileSpan(null, WALL_1440)).toBe(1);
    const s = placeAll([{ id: 'u', span: tileSpan(null, WALL_1440), estimateRatio: 4.3 }], WALL_1440);
    expect(s.placements[0].span).toBe(1);
  });

  it('keeps a wall of real feed shapes append-stable once they span', () => {
    const first = placeAll(feed(36), WALL_1440);
    expect(first.placements.filter((p) => p.span === 2).length).toBeGreaterThan(0);
    const grown = placeInto(first, feed(72), 36, WALL_1440);
    for (let i = 0; i < first.placements.length; i++) {
      expect(areaOf(grown.placements[i])).toBe(areaOf(first.placements[i]));
    }
  });
});

describe('masonry placement — the lattice', () => {
  it('reserves whole rows, gap included, and never fewer than one', () => {
    expect(tileRows(400, 8)).toBe(Math.ceil(408 / ROW_UNIT_PX));
    expect(tileRows(0, 0)).toBe(1);
  });

  it('gives a tile at least its own gap of slack under it', () => {
    // The reservation rounds UP past height + gap, so the next tile in
    // that column can never start inside this one.
    for (const h of [30, 61, 150.19, 269, 422, 1144]) {
      expect(tileRows(h, 8) * ROW_UNIT_PX).toBeGreaterThanOrEqual(h + 8);
    }
  });

  it('places every tile inside the wall', () => {
    const s = placeAll(feed(72), WALL_1440);
    for (const p of s.placements) {
      expect(p.col).toBeGreaterThanOrEqual(0);
      expect(p.col + p.span).toBeLessThanOrEqual(WALL_1440.colCount);
      expect(p.row).toBeGreaterThanOrEqual(1);
    }
  });

  it('never overlaps two tiles in the same column', () => {
    const s = placeAll(feed(72), WALL_1440);
    const bottom = new Array(WALL_1440.colCount).fill(0);
    for (const p of s.placements) {
      for (let c = p.col; c < p.col + p.span; c++) {
        expect(p.row).toBeGreaterThan(bottom[c]);
        bottom[c] = p.row - 1 + p.rows;
      }
    }
  });

  it('starts from an empty wall of the right width', () => {
    expect(emptyState(5).colRows).toEqual([0, 0, 0, 0, 0]);
    expect(emptyState(0).colRows.length).toBe(1);
  });
});

// ── The append band (#1095) ──────────────────────────────────────────
//
// The release-blocking regression in #747's wall, and the reason
// `reconcile` exists. Measured on the dev feed at 2560px / 14 columns:
// 126 inter-tile voids over 16px after ten appends, appearing as a band
// of empty space across ALL FOURTEEN columns at one depth, once per
// appended page, deepening as you scroll (525px of accumulated
// displacement by tile 360).
//
// The mechanism has two halves and only the first is in this file:
//
//   1. A tile whose asset has no recorded dimensions reserves a SQUARE
//      and then settles into the loaded image's own ratio when the bytes
//      arrive — CardThumb's resolution case 2, which `cardTileRatio`
//      cannot mirror because the number does not exist until the image
//      has loaded. 9 of 432 tiles on the dev feed rendered 57-113px
//      taller than they had reserved.
//   2. `grid-auto-rows: minmax(unit, auto)` then lets those tiles grow
//      the row tracks they span, and a grid's row tracks are shared by
//      every column — so a local misprediction displaces the whole wall
//      at that depth.
//
// Half 2 is CSS and only observable in a browser (the numbers are in the
// PR). Half 1 is arithmetic and is what these tests pin: A RESERVATION
// THAT DOES NOT COVER WHAT THE TILE RENDERS IS THE BUG, whatever the CSS
// then does with it. The oracle is the ACTUAL height, supplied
// independently of the prediction — not "does the placer agree with
// itself", which it always did.
describe('the append band (#1095)', () => {
  /** 2560px at the 10rem rung, read out of Chromium: 14 columns of
   *  163px, 8px gap, the 60px floor. The owner's reproduction. */
  const WALL_2560: MasonryGeometry = { colCount: 14, colWidth: 163, gapPx: 8, minTilePx: 60 };
  const PAGE = 36;
  const PAGES = 10;

  /** Which tiles are the ones that settle. Every twelfth, i.e. three per
   *  appended page — close to the dev feed's measured 9-in-432 once the
   *  wall is deep, and enough that a band would be unmissable. */
  const settles = (i: number) => i % 12 === 5;
  /** The shape such a tile turns out to have. Both directions on
   *  purpose: a portrait settles TALLER than the square it reserved
   *  (which is what grows the shared tracks) and an ultrawide settles
   *  SHORTER (which leaves a void the size of the square). */
  const settledRatio = (i: number) => (i % 24 === 5 ? 0.66 : 4.5);

  /** What the placer is told before anything renders. */
  function predicted(count: number): PlaceableTile[] {
    return Array.from({ length: count }, (_, i) => {
      if (settles(i)) return { id: `b${i}`, span: 1, estimateRatio: null };
      const r = FEED_RATIOS[i % FEED_RATIOS.length];
      return { id: `b${i}`, span: tileSpan(r, WALL_2560), estimateRatio: r };
    });
  }

  /** What the tile ACTUALLY renders at. The oracle — deliberately not
   *  derived from anything the placer produced. */
  function actualHeight(i: number): number {
    const r = settles(i) ? settledRatio(i) : FEED_RATIOS[i % FEED_RATIOS.length];
    const span = settles(i) ? 1 : tileSpan(FEED_RATIOS[i % FEED_RATIOS.length], WALL_2560);
    return masonryTileHeight(spanWidthPx(WALL_2560, span), r, WALL_2560.minTilePx);
  }

  /** The DOM's answer to "how many rows does this tile really need",
   *  for every tile placed so far — what `snapshotTiles` harvests. */
  function measure(state: MasonryState): Map<string, number> {
    const m = new Map<string, number>();
    for (const p of state.placements) {
      m.set(p.id, tileRows(actualHeight(p.index), WALL_2560.gapPx));
    }
    return m;
  }

  /** Scroll the feed, one appended page at a time, exactly as
   *  MasonryColumns does: harvest the rendered wall, reconcile against
   *  it, then place the new page onto the corrected column bottoms.
   *  `withReconcile: false` is the shipped-and-broken pipeline. */
  function scroll(withReconcile: boolean): MasonryState {
    let wall = emptyState(WALL_2560.colCount);
    for (let page = 1; page <= PAGES; page++) {
      const shapes = predicted(page * PAGE);
      if (withReconcile) wall = reconcile(wall, measure(wall));
      wall = placeInto(wall, shapes, (page - 1) * PAGE, WALL_2560);
      // The size observer fires as the page's images land.
      if (withReconcile) wall = reconcile(wall, measure(wall));
    }
    return wall;
  }

  /** Rendered geometry, in px: a tile's top is its row line, its bottom
   *  is that plus what it really renders. */
  const topPx = (p: { row: number }) => (p.row - 1) * ROW_UNIT_PX;
  const bottomPx = (p: { row: number; index: number }) => topPx(p) + actualHeight(p.index);

  /** Every inter-tile void in the rendered wall, per column. */
  function voids(state: MasonryState) {
    const cols = new Map<number, PlacedTile[]>();
    for (const p of state.placements) {
      for (let c = p.col; c < p.col + p.span; c++) {
        if (!cols.has(c)) cols.set(c, []);
        cols.get(c)!.push(p);
      }
    }
    const out: Array<{ col: number; gap: number; at: number; prev: number; next: number }> = [];
    for (const [col, list] of cols) {
      list.sort((a, b) => a.row - b.row);
      for (let i = 1; i < list.length; i++) {
        out.push({
          col,
          gap: topPx(list[i]) - bottomPx(list[i - 1]),
          at: bottomPx(list[i - 1]),
          prev: list[i - 1].index,
          next: list[i].index,
        });
      }
    }
    return out;
  }

  /** ADR 0079 §4: the only void the packing rule is allowed to leave is
   *  the one under a SPANNING tile, and it is exactly the placing pair's
   *  height difference. Recovered by replaying the column bottoms the
   *  final placement implies — the columns and spans come from the wall
   *  itself, so this reads the residuals out rather than re-deciding
   *  them. */
  function spanResidualPx(state: MasonryState): Map<string, number> {
    const colRows = new Array(WALL_2560.colCount).fill(0);
    const residual = new Map<string, number>();
    for (const p of state.placements) {
      let top = 0;
      for (let c = p.col; c < p.col + p.span; c++) top = Math.max(top, colRows[c]);
      let worst = 0;
      for (let c = p.col; c < p.col + p.span; c++) worst = Math.max(worst, top - colRows[c]);
      residual.set(p.id, worst * ROW_UNIT_PX);
      for (let c = p.col; c < p.col + p.span; c++) colRows[c] = top + p.rows;
    }
    return residual;
  }

  /** What a correct wall may leave under a tile: the gap itself, the
   *  lattice's round-up, and the one row of quantisation `reconcile`
   *  declines to chase. 16px at this geometry — which is why the
   *  browser-side measurement counts voids OVER 16px. */
  const SLACK_PX = WALL_2560.gapPx + (RECONCILE_SLOP_ROWS + 1) * ROW_UNIT_PX;

  // ⛔ THE FAILING TEST. Reverting `reconcile` to a no-op turns this red
  //    with ~30 uncovered reservations — the sabotage check.
  it('reserves at least what every tile renders, ten appends deep', () => {
    const wall = scroll(true);
    expect(wall.placements.length).toBe(PAGE * PAGES);
    const under = wall.placements.filter((p) => p.rows * ROW_UNIT_PX < actualHeight(p.index));
    expect(under.map((p) => p.index)).toEqual([]);
  });

  it('leaves no void a spanning tile does not account for', () => {
    const wall = scroll(true);
    const residual = spanResidualPx(wall);
    const byId = new Map(wall.placements.map((p) => [p.index, p]));
    const unexplained = voids(wall).filter(
      (v) => v.gap > SLACK_PX + (residual.get(byId.get(v.next)!.id) ?? 0),
    );
    expect(unexplained).toEqual([]);
  });

  // The signature the owner reported: not "some gaps", but a horizontal
  // stripe of nothing across the whole wall, once per appended page.
  it('never leaves a void at one depth in every column at once', () => {
    const wall = scroll(true);
    const wide = voids(wall).filter((v) => v.gap > SLACK_PX);
    for (const v of wide) {
      const mid = v.at + v.gap / 2;
      const spanned = new Set(
        wide.filter((o) => o.at <= mid && o.at + o.gap >= mid).map((o) => o.col),
      );
      expect(spanned.size).toBeLessThan(WALL_2560.colCount);
    }
  });

  // The wall is measured, not summed: a tile that renders taller than it
  // reserved must cost its OWN column that height and no other column
  // anything. Without the correction the CSS pays for it out of the
  // shared row tracks, which is every column at once.
  it('charges a settled tile to its own column, and the wall grows by it', () => {
    const before = scroll(false);
    const after = reconcile(before, measure(before));
    // Reconciliation is VERTICAL ONLY — same tile, same column, same
    // width, same feed position. It answers "how far down does this
    // column really go", never "which column".
    expect(after.placements.map((p) => `${p.id}/${p.col}/${p.span}/${p.index}`)).toEqual(
      before.placements.map((p) => `${p.id}/${p.col}/${p.span}/${p.index}`),
    );
    // The wall gets TALLER: the height the shared row tracks used to
    // absorb — and charge to every column — is now reserved where it
    // belongs. The tallest column grows by less than the total settled
    // overflow, because it lands in one column at a time.
    expect(Math.max(...after.colRows)).toBeGreaterThan(Math.max(...before.colRows));
    // And the reconciled wall is the one that covers its tiles.
    expect(before.placements.some((p) => p.rows * ROW_UNIT_PX < actualHeight(p.index))).toBe(true);
    expect(after.placements.every((p) => p.rows * ROW_UNIT_PX >= actualHeight(p.index))).toBe(true);
  });

  it('is idempotent once the wall agrees with the DOM', () => {
    const wall = scroll(true);
    expect(reconcile(wall, measure(wall))).toBe(wall);
  });

  // Reconciling before an append re-solves ROWS. It must not be able to
  // re-decide a packing, which is what would bring #651 back.
  it('never moves a tile sideways or reorders the feed', () => {
    const wall = placeAll(feed(144, 0, WALL_2560), WALL_2560);
    const measured = new Map(wall.placements.map((p, i) => [p.id, p.rows + (i % 5) * 3]));
    const next = reconcile(wall, measured);
    expect(next.placements.map((p) => `${p.id}/${p.col}/${p.span}/${p.index}`)).toEqual(
      wall.placements.map((p) => `${p.id}/${p.col}/${p.span}/${p.index}`),
    );
  });

  // A one-row disagreement is the lattice's own resolution, not an
  // error. Chasing it would rewrite a quarter of the wall's row lines
  // for 4px and make every append look like it moved tiles.
  it('ignores a disagreement inside the lattice unit', () => {
    const wall = placeAll(feed(72, 0, WALL_2560), WALL_2560);
    const noise = new Map(
      wall.placements.map((p, i) => [p.id, p.rows + (i % 3) - 1] as [string, number]),
    );
    expect(reconcile(wall, noise)).toBe(wall);
  });
});

describe('MasonryColumns', () => {
  const items = Array.from({ length: 5 }, (_, i) => ({ id: `i${i}` }));
  const card = createRawSnippet(() => ({ render: () => '<span>tile</span>' }));

  function wall() {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const { container } = render(MasonryColumns as any, {
      props: { items, tileMin: '22rem', card },
    });
    const el = container.querySelector('.posts-masonry');
    expect(el, 'renders a wall').toBeTruthy();
    return el as HTMLElement;
  }

  it('places every item', () => {
    const el = wall();
    const tiles = el.querySelectorAll<HTMLElement>('[data-tile-id]');
    expect(tiles.length).toBe(items.length);
    // Read the attribute, not the parsed CSSOM: jsdom does not implement
    // the `grid-column` shorthand and reports it empty.
    for (const tile of tiles) {
      expect(tile.getAttribute('style')).toMatch(/grid-column: \d+ \/ span \d+/);
      expect(tile.getAttribute('style')).toMatch(/grid-row: \d+ \/ span \d+/);
    }
  });

  // The win the column boxes cost us, taken back. #651 accepted
  // column-major traversal because a tile lived inside a column element;
  // there are no column elements now, so the reading order IS the feed
  // order and there is nothing to compensate for.
  it('renders in feed order', () => {
    const el = wall();
    const ids = [...el.querySelectorAll<HTMLElement>('[data-tile-id]')].map(
      (t) => t.dataset.tileId,
    );
    expect(ids).toEqual(items.map((i) => i.id));
  });

  // Kept explicit even though DOM order now agrees with it: ADR 0079's
  // sized slots will put positions in the stream that are not one tile,
  // and the announced position must not drift when they do.
  it("carries each tile's true feed position in list semantics", () => {
    const el = wall();
    expect(el.getAttribute('role')).toBe('list');
    // No `role="presentation"` column boxes to make transparent any more.
    expect(el.querySelectorAll('[data-masonry-col]').length).toBe(0);
    const tiles = [...el.querySelectorAll<HTMLElement>('[role="listitem"]')];
    expect(tiles.length).toBe(items.length);
    for (const tile of tiles) {
      const idx = items.findIndex((it) => it.id === tile.dataset.tileId);
      expect(tile.getAttribute('aria-posinset')).toBe(String(idx + 1));
      expect(tile.getAttribute('aria-setsize')).toBe(String(items.length));
    }
  });
});

// #1103 — SWITCHING THE FEED FILTER CORRUPTED THE WALL.
//
// `placements` and `items` are two independent reactive inputs to one
// `{#each}`: the block iterates the placements and renders
// `items[p.index]`. They agree only while the placement pass is up to
// date, and a feed swap breaks that on purpose — browse sets `items =
// []` and refills it a page at a time, so for several flushes the block
// was still iterating the PREVIOUS feed's 180 placements while `items`
// held 0, then 36, then 72.
//
// Every index past the end handed the card `undefined`. Real cards read
// their row (PostCard reads `post.members`), so the render effect threw,
// the flush aborted, and the user `$effect` that re-places the wall
// never ran — for the whole refill. The wall kept the old feed's rows
// under the new feed's posts, and the tile ResizeObserver's `reconcile`
// (a rAF, outside the effect graph) then re-solved those rows against
// heights measured from tiles showing DIFFERENT posts. Measured on the
// dev feed at 2560px: 0 overlapping pairs before the swap and 10 after,
// worst void 214px → 521px, with three uncaught `undefined.members`.
//
// The oracle is a fresh mount: a wall that has been swapped TO a feed
// must be indistinguishable from a wall that was mounted with it.
describe('MasonryColumns — feed swap (#1103)', () => {
  const feedOf = (ids: string[]) => ids.map((id) => ({ id }));

  function readWall(container: Element) {
    return [...container.querySelectorAll<HTMLElement>('[data-tile-id]')].map((t) => ({
      id: t.dataset.tileId,
      style: t.getAttribute('style'),
      pos: t.getAttribute('aria-posinset'),
    }));
  }

  const LATEST = feedOf(['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h']);
  // The same set in a different order. The account the owner reported
  // this on follows everything, so Latest → Following is a REORDER and
  // the tile count is identical before and after — which is what makes
  // it the case the placer must treat as a brand-new wall rather than
  // as an append.
  const FOLLOWING = feedOf(['e', 'a', 'g', 'c', 'h', 'b', 'f', 'd']);

  it('lands on the same wall a fresh mount would', async () => {
    const swapped = render(MasonryFeedHarness, { props: { items: LATEST } });
    // Browse empties the list before refetching (+page.svelte's feedKey
    // effect), then the infinite loader refills it a page at a time.
    for (const items of [[], FOLLOWING.slice(0, 4), FOLLOWING]) {
      await swapped.rerender({ items });
    }
    const fresh = render(MasonryFeedHarness, { props: { items: FOLLOWING } });

    expect(readWall(swapped.container)).toEqual(readWall(fresh.container));
  });

  it('never renders a tile the current feed has no item for', async () => {
    const { container, rerender } = render(MasonryFeedHarness, { props: { items: LATEST } });
    // Every intermediate state the refill passes through, including the
    // short ones the old code indexed past the end of.
    for (const items of [[], FOLLOWING.slice(0, 2), FOLLOWING.slice(0, 5), FOLLOWING]) {
      await rerender({ items });
      const tiles = readWall(container);
      expect(tiles.length).toBeLessThanOrEqual(items.length);
      for (const [i, tile] of tiles.entries()) expect(tile.id).toBe(items[i].id);
    }
    expect(readWall(container).length).toBe(FOLLOWING.length);
  });
});
