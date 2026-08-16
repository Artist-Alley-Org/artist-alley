// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Masonry placement, as arithmetic (#747).
//
// MasonryColumns used to be N sibling column elements, each an ordinary
// block flow. A tile lived INSIDE one column element, so there was no
// shared coordinate space and nothing could straddle two columns — the
// structural blocker ADR 0079 §4 records. The wall is now one CSS grid
// with every tile explicitly placed, and this module is the placer: it
// takes tile shapes and a geometry, and returns a grid area per tile.
// No DOM, no framework — so the properties the layout rests on are
// assertable rather than only observable in a browser.
//
// # Why the placement is explicit and not `grid-auto-flow`
//
// Sparse auto-flow looks like it would do this for free: it scans
// row-major from a cursor that only moves forward, and while every
// column's occupied cells form a prefix that scan lands on exactly the
// shortest column. It stops being equivalent the moment a tile spans.
// After a 2-wide tile is placed deep in the wall the cursor is left at
// that row, and every later tile is forced to start at or below it —
// so a column that was 400px shorter never gets filled again. Measured
// against a hand-run of the CSS Grid §8.5 sparse algorithm, one wide
// tile placed at row 505 with a neighbouring column at row 120 strands
// a 385px hole that nothing can ever occupy. `dense` would fix the hole
// by backfilling, which reorders items visually and is forbidden by
// ADR 0079 §3 for exactly the reason #651 exists.
//
// Explicit placement keeps the whole packing decision here, where it is
// one function and one set of tests.
//
// # The lattice
//
// Row heights are not content-derived: the grid declares a fine
// `grid-auto-rows` unit and each tile reserves `ceil(height / unit)` of
// them. That makes a tile's position arithmetic from the recorded
// `pixel_width` / `pixel_height` (#646) BEFORE anything renders — no
// measure-then-place pass, no JS masonry library, no waiting on image
// bytes.
//
// The lattice is authoritative for POSITION — a tile renders where its
// reserved rows put it — but it is not authoritative for HEIGHT, and
// #1095 is what happens when the two are confused. See `reconcile`.
//
// The unit is a tradeoff, not a magic number: every tile rounds UP to
// it, so a coarse unit shows as slack under tiles, and a fine one
// multiplies the implicit tracks the engine has to size (a 72-tile wall
// at 1440px is ~4300px of column, i.e. ~1100 tracks at 4px and ~4300 at
// 1px). 4px is under the 8px inter-tile gap, so the slack is never
// larger than the gap already there.

import { masonryTileHeight } from './cardAsset';

/** Height of one `grid-auto-rows` track, in px. See the header. */
export const ROW_UNIT_PX = 4;

/** A tile may span at most this many columns.
 *
 *  Owner's call, and the reason is legibility in both directions: a tile
 *  spanning the full row stops reading as a card and reads as a section
 *  break. Paired with `MIN_COLS_FOR_SPAN` and the `colCount - 1` cap in
 *  `tileSpan`, so a wide tile always has at least one ordinary column
 *  beside it. */
export const MAX_SPAN = 2;

/** No spanning below this many columns. At two columns a 2-wide tile IS
 *  the full row; at one it is meaningless. */
export const MIN_COLS_FOR_SPAN = 3;

/** The wall's measured geometry. All px, all read from the DOM by the
 *  component — never assumed here. */
export interface MasonryGeometry {
  /** Live column count. The size stepper picks `--tile-min` and the
   *  count falls out of the available width (browseView, #556), so this
   *  is an outcome to be read, never a setting. */
  colCount: number;
  colWidth: number;
  gapPx: number;
  /** CardThumb's `min-height` floor (#652) resolved to px. */
  minTilePx: number;
}

/** One tile's shape, as the placer needs it. The component resolves both
 *  fields (see MasonryColumns' `shape`); this module never touches a
 *  card row.
 *
 *  How WIDE a tile should be is the caller's decision, not the placer's:
 *  a span may come from the tile's own content being too wide to read in
 *  one column (#1025) or, later, from ADR 0079's sized slots,
 *  where the size is configuration rather than a property of the
 *  artwork. The placer only has to honour it. */
export interface PlaceableTile {
  id: string;
  /** Columns this tile occupies. Clamped to what the wall can give it. */
  span: number;
  /** The ratio the HEIGHT prediction uses — declared, else the measured
   *  cache, else null for a square. */
  estimateRatio: number | null;
}

/** A tile's grid area. `col` / `row` are zero- and one-based
 *  respectively to match how they are consumed: `col` indexes
 *  `colRows`, `row` is a CSS grid line. */
export interface PlacedTile {
  id: string;
  /** Position in the feed, for `aria-posinset`. Kept explicit even
   *  though DOM order now matches feed order, because ADR 0079's sized
   *  slots will put positions in the stream that are not tiles. */
  index: number;
  /** Zero-based column index of the tile's left edge. */
  col: number;
  span: number;
  /** One-based CSS grid row line. */
  row: number;
  rows: number;
}

export interface MasonryState {
  placements: PlacedTile[];
  /** Each column's running bottom edge, in lattice rows. Integers we
   *  assigned, so this is exact — see the header on drift. */
  colRows: number[];
}

export function emptyState(colCount: number): MasonryState {
  return { placements: [], colRows: new Array(Math.max(1, colCount)).fill(0) };
}

/** Rendered width of a tile spanning `span` columns. A span eats the
 *  gaps it covers, which is why this is not `span * colWidth`. */
export function spanWidthPx(geo: MasonryGeometry, span: number): number {
  return span * geo.colWidth + (span - 1) * geo.gapPx;
}

/** How many columns `declaredRatio` needs to stay legible.
 *
 *  THE RULE: a tile spans the smallest number of columns that lifts its
 *  RENDERED HEIGHT back above the #652 floor — the same floor CardThumb
 *  writes into `min-height` and the same one the height prediction
 *  applies, not a second constant.
 *
 *  It is stated against rendered pixels and not against an aspect ratio
 *  on purpose. An aspect-ratio threshold is scale-invariant by
 *  construction, so it would say the same thing at every rung of the
 *  size stepper — but the stepper changes `--tile-min`, which changes
 *  the column width, which is exactly what decides whether a given ratio
 *  still has 60px of height to render into. At the 22rem default on a
 *  1440px viewport (5 columns, 269px each) the rule fires above 4.48:1;
 *  at the 10rem rung on 1920px (11 columns, ~160px) it fires above
 *  2.67:1. One rule, two thresholds, because the pixels differ. */
export function tileSpan(declaredRatio: number | null, geo: MasonryGeometry): number {
  if (declaredRatio === null || declaredRatio <= 0) return 1;
  if (geo.colCount < MIN_COLS_FOR_SPAN) return 1;
  // Never the whole row: a wide tile keeps at least one ordinary column
  // beside it, or it is a section break rather than a card.
  const cap = Math.min(MAX_SPAN, geo.colCount - 1);
  if (cap < 2) return 1;
  for (let span = 1; span < cap; span++) {
    if (spanWidthPx(geo, span) / declaredRatio >= geo.minTilePx) return span;
  }
  // Still under the floor at the cap. It renders letterboxed onto the
  // floor by the SAME `min-height` that produced this decision — the
  // ratio ceiling and the #652 floor are the same box, so there is no
  // second mechanism to apply. See `masonryTileHeight`.
  return cap;
}

/** Lattice rows a tile of `heightPx` reserves, gap included. */
export function tileRows(heightPx: number, gapPx: number): number {
  return Math.max(1, Math.ceil((heightPx + gapPx) / ROW_UNIT_PX));
}

/** Predicted height of `tile` at `span` columns wide, in px. Goes
 *  through `masonryTileHeight` so the #652 floor is applied by the one
 *  function CardThumb's `min-height` also reads. */
export function tileHeightPx(tile: PlaceableTile, geo: MasonryGeometry, span: number): number {
  return masonryTileHeight(spanWidthPx(geo, span), tile.estimateRatio, geo.minTilePx);
}

/** Shortest column, ties to the left. */
function shortest(colRows: number[]): number {
  let best = 0;
  for (let i = 1; i < colRows.length; i++) if (colRows[i] < colRows[best]) best = i;
  return best;
}

/** ADR 0079 §4 step 1: the ADJACENT PAIR with the smallest height
 *  difference — not simply the shortest column. The residual gap under a
 *  wide tile is exactly that difference, so minimising it is the whole
 *  point.
 *
 *  Ties break to the HIGHER placement and then to the left. Two pairs
 *  level with each other are equally good by §4's measure and there is
 *  no reason to take the lower one. */
function closestPair(colRows: number[]): number {
  let best = 0;
  let bestDiff = Infinity;
  let bestTop = Infinity;
  for (let c = 0; c + 1 < colRows.length; c++) {
    const diff = Math.abs(colRows[c] - colRows[c + 1]);
    const top = Math.max(colRows[c], colRows[c + 1]);
    if (diff < bestDiff || (diff === bestDiff && top < bestTop)) {
      best = c;
      bestDiff = diff;
      bestTop = top;
    }
  }
  return best;
}

/** Place `tiles[from..]` into `state`, in feed order.
 *
 *  APPEND-STABILITY LIVES HERE, and it is now a property of the loop
 *  rather than of a cache key: this only ever reads `colRows` and pushes
 *  onto `placements`. It cannot revisit an existing tile's `col` / `row`
 *  because it never looks at one. A page appended at the end therefore
 *  cannot move anything already on screen — and because the wall renders
 *  in feed order, the DOM does not reorder either.
 *
 *  Returns a NEW state; the caller decides whether to keep it. */
export function placeInto(
  state: MasonryState,
  tiles: PlaceableTile[],
  from: number,
  geo: MasonryGeometry,
): MasonryState {
  const placements = state.placements.slice();
  const colRows = state.colRows.slice();
  // `colRows` is authoritative for the count, not `geo.colCount`: a
  // caller that hands us a stale state must not be able to place a tile
  // into a column that does not exist.
  const n = colRows.length;
  for (let i = from; i < tiles.length; i++) {
    const tile = tiles[i];
    let span = n < MIN_COLS_FOR_SPAN ? 1 : Math.max(1, Math.min(tile.span, MAX_SPAN));
    const col = span === 1 ? shortest(colRows) : closestPair(colRows);
    span = Math.max(1, Math.min(span, n - col));
    // §4 step 2 — the slot sits at the GREATER of the pair's heights.
    let top = colRows[col];
    for (let c = col; c < col + span; c++) top = Math.max(top, colRows[c]);
    const rows = tileRows(tileHeightPx(tile, geo, span), geo.gapPx);
    placements.push({ id: tile.id, index: i, col, span, row: top + 1, rows });
    // §4 step 3 — BOTH columns resume from the slot's bottom edge, which
    // is what re-levels the pair.
    for (let c = col; c < col + span; c++) colRows[c] = top + rows;
  }
  return { placements, colRows };
}

/** Place every tile from scratch. */
export function placeAll(tiles: PlaceableTile[], geo: MasonryGeometry): MasonryState {
  return placeInto(emptyState(geo.colCount), tiles, 0, geo);
}

/** How far a reservation may disagree with the rendered height before
 *  the measurement wins, in lattice rows.
 *
 *  One unit, because one unit is the coordinate system's own resolution:
 *  `tileRows` rounds UP to it, and `offsetHeight` rounds to whole pixels,
 *  so a reservation that is one row out is quantisation and not a
 *  disagreement. Adopting those would rewrite a quarter of the wall's
 *  rows for 4px — under the 8px gap, invisible, and it would make an
 *  ordinary append look like it moved tiles. */
export const RECONCILE_SLOP_ROWS = 1;

/** Re-solve the wall's VERTICAL positions against what the tiles
 *  actually render, keeping every tile in the column it was placed in.
 *
 *  # Why this exists (#1095)
 *
 *  A tile's height is a PREDICTION. Most of the time it is exact — #646
 *  put the source dimensions on the card payload — but CardThumb has a
 *  second resolution case the prediction cannot mirror: an asset with no
 *  recorded dimensions renders as a square until its bytes arrive and
 *  then takes the loaded image's own ratio. Measured on the dev feed at
 *  14 columns, 9 of 432 tiles settled 57-113px TALLER than the square
 *  they had reserved.
 *
 *  On its own that is a local cosmetic error. What made it #1095 is the
 *  CSS: `grid-auto-rows: minmax(unit, auto)` lets a tile that outgrows
 *  its reservation grow the row tracks it spans — and a grid's row
 *  tracks are SHARED BY EVERY COLUMN. So one mispredicted tile in one
 *  column pushes all fourteen columns down at that depth, which is the
 *  full-width band the owner reported, and the growth accumulates down
 *  the wall (525px by tile 360, measured) which is why it got worse the
 *  further you scrolled.
 *
 *  So the rendered DOM is the source of truth for HEIGHT, and this is
 *  where it is read back in. It is the anti-drift mechanism the
 *  sibling-column implementation had as `syncHeightsFromDom`, restored
 *  in the shape the one-grid layout needs: not a re-measure of column
 *  bottoms (there are no column boxes to measure) but a re-solve of
 *  every tile's row line from its measured reservation.
 *
 *  # What it may and may not change
 *
 *  `col`, `span`, `index` and the order are carried through UNTOUCHED.
 *  This cannot move a tile sideways, cannot reorder the feed, and cannot
 *  revisit a packing decision — so #651's append-stability survives it.
 *  It only ever answers "how far down does this column actually go",
 *  which is a question the wall was already getting wrong.
 *
 *  Returns `state` ITSELF when nothing moved, so a caller can skip the
 *  write and a settled wall costs one pass and no re-render. */
export function reconcile(
  state: MasonryState,
  measuredRows: ReadonlyMap<string, number>,
): MasonryState {
  const colRows = new Array(state.colRows.length).fill(0);
  const placements: PlacedTile[] = [];
  let changed = false;
  for (const p of state.placements) {
    const m = measuredRows.get(p.id);
    const rows = m !== undefined && Math.abs(m - p.rows) > RECONCILE_SLOP_ROWS ? m : p.rows;
    let top = 0;
    for (let c = p.col; c < p.col + p.span; c++) top = Math.max(top, colRows[c]);
    for (let c = p.col; c < p.col + p.span; c++) colRows[c] = top + rows;
    if (rows === p.rows && top + 1 === p.row) {
      placements.push(p);
      continue;
    }
    changed = true;
    placements.push({ ...p, row: top + 1, rows });
  }
  return changed ? { placements, colRows } : state;
}
