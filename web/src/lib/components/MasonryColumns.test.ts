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
  ROW_UNIT_PX,
  emptyState,
  placeAll,
  placeInto,
  spanWidthPx,
  tileRows,
  tileSpan,
  type MasonryGeometry,
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
 *  by the time the next page arrives (`snapshotRatios`).
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
