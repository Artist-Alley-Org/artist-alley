<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Append-stable masonry, on one grid (#651, #747).
  //
  // # Where this started
  //
  // The layout was `column-width` multicol. Multicol BALANCES: it fills
  // columns to equal height across the whole flow, so appending 36 tiles
  // at the end legitimately changes which column tile #4 belongs in.
  // Measured at 1440px on the 36 → 72 append, 19 of 24 sampled tiles
  // changed column and 13 moved. The user watches the thing they are
  // looking at slide sideways every time the infinite loader fires.
  //
  // #651 replaced that with N sibling column elements, each an ordinary
  // block flow, each item assigned to the shortest column on arrival and
  // never reassigned. That fixed the movement and cost two things: a
  // tile lived INSIDE one column element, so there was no shared
  // coordinate space and nothing could straddle two columns (ADR 0079
  // §4); and DOM order was column-major rather than feed order.
  //
  // # What this is now
  //
  // ONE CSS grid. `grid-template-columns: repeat(n, minmax(0, 1fr))`,
  // a fine `grid-auto-rows` lattice, and every tile explicitly placed
  // with `grid-column` / `grid-row` computed in `masonryPlacement.ts`.
  // Both costs go away:
  //
  //   * A tile can span columns, because the columns are tracks in one
  //     coordinate space rather than separate boxes. That is #747, and
  //     ADR 0079's sized slots inherit it.
  //   * DOM ORDER IS FEED ORDER AGAIN. Tiles render in one flat keyed
  //     list and the grid puts them where the placer said, so keyboard
  //     traversal and the reading order follow the feed. The #651
  //     compensation — column boxes marked `role="presentation"` so the
  //     list still owned its items — is retired with the column boxes.
  //     `aria-posinset` / `aria-setsize` stay, because ADR 0079 will put
  //     positions in the stream that are not one tile.
  //
  // # Append-stability, restated
  //
  // #651 held this with a cache key: `epoch = n|ladderReady`, and within
  // one epoch a placement was permanent. The epoch is still here for
  // invalidation, but the property itself is now structural — `placeInto`
  // only reads column bottoms and pushes onto the placement list, so it
  // has no way to revisit an earlier tile, and appending renders as a
  // pure push onto a keyed `{#each}`. "Placement is permanent" became
  // "the DOM never reorders", which is the stronger statement.
  //
  // ⛔ `grid-auto-flow: dense` is forbidden (ADR 0079 §3). It backfills
  // holes, which reorders items visually — the exact instability #651
  // removed. Nothing here relies on auto-flow at all; every tile is
  // explicitly placed.
  //
  // `ladderReady` stays in the epoch key and still earns its place:
  // CardThumb only honours recorded dimensions once `GET /previews` has
  // answered (see `cardTileRatio`), so before that flip every tile is a
  // square — and now it also means no tile knows it is wide enough to
  // span. A wall bucketed on pre-ladder squares would stay permanently
  // lopsided AND permanently unspanned. It resolves once, one RTT into
  // the first page, well before any append.
  //
  // # Heights are arithmetic, and the lattice is authoritative
  //
  // The usual objection to hand-rolled masonry is the double pass:
  // render, measure, then position. That does not apply — #646 put
  // `pixel_width` / `pixel_height` on the card payload, so a tile's
  // height is `columnWidth / ratio` before anything renders.
  //
  // `estimate()` reads three sources IN THIS ORDER:
  //   declared ratio → what CardThumb WILL render, authoritative and
  //                    known before the request
  //   measured cache → what this tile IS right now, harvested from the
  //                    DOM on every placement pass (`snapshotRatios`)
  //   square         → never rendered before, nothing recorded. Not a
  //                    hedge: CardThumb reserves `aspect-square` for
  //                    exactly these tiles, so matching its own
  //                    reservation makes the estimate EXACT at first
  //                    paint.
  //
  // Declared beats measured deliberately: at the `ladderReady` flip the
  // DOM still shows the square a declared tile is about to stop being,
  // so trusting the measurement there bakes in the shape we are one
  // frame away from replacing. Bucketing the ultra-wide tiles as squares
  // predicted a 269px tile where a 50px one landed, and a re-bucket
  // built purely on that estimate left a 1144px height spread across
  // five columns.
  //
  // # What replaced `syncHeightsFromDom`
  //
  // The sibling-column version re-read every column's real
  // `offsetHeight` before each append, because `colHeights` was a running
  // sum of PREDICTIONS while the wall rendered at its REAL heights — two
  // numbers that diverge, with the divergence compounding down a long
  // scroll.
  //
  // There is no such pair now. A tile renders exactly where its reserved
  // rows put it, so the reservation IS the position: a prediction that is
  // a few pixels short shows as a few pixels of slack under that one
  // tile and contributes nothing to the next tile's row. Nothing sums the
  // error, so nothing can drift. Measured in Chromium at 1440px, the
  // reservation for every declared-ratio card on the dev feed is within
  // 2px of what the card renders (the card's 1px border top and bottom),
  // against a 4px lattice unit inside an 8px gap.
  //
  // The guard for what we CANNOT predict is in the CSS rather than in
  // JS: `grid-auto-rows: minmax(ROW_UNITpx, auto)`. A card carrying flow
  // chrome we have no way to compute ahead of time — a CollectionCard's
  // title and footer wrap at a width we only learn at layout — grows its
  // own tracks instead of overlapping the tile below it. When the
  // reservation is right, which is the ordinary case, `auto` never
  // engages and the lattice is exactly `ROW_UNIT` throughout.

  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import type { ViewMode } from '$stores/browseView.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { cardTileRatio, masonryMinTilePx, masonryTileHeight } from './cardAsset';
  import {
    ROW_UNIT_PX,
    emptyState,
    placeInto,
    tileRows,
    tileSpan,
    type MasonryGeometry,
    type MasonryState,
    type PlaceableTile,
  } from './masonryPlacement';

  interface Props {
    items: Array<{ id: string }>;
    /** Minimum column width as a CSS length (browseView.tileMin). */
    tileMin: string;
    loading?: boolean;
    /** Renders one card. See ContentGrid — `item` is loosely typed
     *  because the row shape differs per surface. */
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    card: Snippet<[any, ViewMode]>;
  }

  let { items, tileMin, loading = false, card }: Props = $props();

  /** Long enough that a drag re-places once; short enough that letting
   *  go feels immediate. */
  const RESIZE_DEBOUNCE_MS = 120;
  /** Skeleton tiles are square by construction (aspect-square below). */
  const SKELETONS = 8;

  let containerEl = $state<HTMLElement>();
  /** Zero-height probe whose width IS `--tile-min`. `tileMin` is a
   *  `clamp(…rem, …vw, …rem)` (browseView), so it cannot be parsed in JS
   *  — it has to be resolved by the engine. Reading a probe is how we
   *  ask CSS the same question multicol used to answer internally. */
  let probeEl = $state<HTMLElement>();

  /** Layout geometry, only ever written by `measure()`. Reactive because
   *  the template needs the column count and the placement effect has to
   *  re-run when the wall is re-measured. */
  let geo = $state<MasonryGeometry>({ colCount: 1, colWidth: 0, gapPx: 0, minTilePx: 0 });
  let lastWidth = -1;

  let placements = $state<MasonryState['placements']>([]);
  /** Column bottoms after the last placement pass, in lattice rows.
   *  Reactive only so the skeleton row can sit under the real wall. */
  let colRows = $state<number[]>([]);

  // Placer state. Deliberately plain (not $state): it is bookkeeping for
  // the effect that writes `placements`, and making it reactive would
  // re-enter that effect.
  let placedIds: string[] = [];
  let wall: MasonryState = emptyState(1);
  let epoch = '';
  /** id → rendered height ÷ rendered width, harvested from the DOM. A
   *  dimensionless factor rather than an aspect ratio so it survives a
   *  column-width change AND carries whatever card chrome (borders, a
   *  CollectionCard footer) sits outside the thumb frame. */
  const measuredRatio = new Map<string, number>();

  /** Multicol's own column-count formula, so the wall keeps the density
   *  the `--tile-min` ladder was calibrated against (browseView's rungs
   *  are MEASURED against real column counts — see TILE_STEPS_REM).
   *
   *  The stepper picks a SIZE and the count falls out of the width
   *  (#556). This is where it falls out; it is a live value. */
  function measure(): void {
    const el = containerEl;
    const probe = probeEl;
    if (!el || !probe) return;
    const width = el.clientWidth;
    if (width <= 0) return;
    const cs = getComputedStyle(el);
    const gapPx = parseFloat(cs.columnGap) || 0;
    const min = probe.getBoundingClientRect().width;
    const colCount = min > 0 ? Math.max(1, Math.floor((width + gapPx) / (min + gapPx))) : 1;
    lastWidth = width;
    geo = {
      colCount,
      colWidth: (width - gapPx * (colCount - 1)) / colCount,
      gapPx,
      // CardThumb's `min-height` floor (#652), resolved to px. Read here
      // and not assumed, because it is declared in rem.
      minTilePx: masonryMinTilePx(),
    };
  }

  $effect(() => {
    const el = containerEl;
    if (!el) return;
    // Re-measuring when `tileMin` changes is the size stepper; the probe
    // resolves the new clamp on the next frame, so read it here.
    void tileMin;
    measure();
    // No ResizeObserver (older engine, or a test DOM) just means the
    // count is fixed at whatever the first measure found — degraded, not
    // broken, same posture as previewLadder's col-only fallback.
    if (typeof ResizeObserver === 'undefined') return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const ro = new ResizeObserver(() => {
      // The observer also fires as the wall GROWS taller. Only a width
      // change can alter the column count, and re-placing on every
      // append would reintroduce the exact bug this file removes.
      if (el.clientWidth === lastWidth) return;
      clearTimeout(timer);
      timer = setTimeout(measure, RESIZE_DEBOUNCE_MS);
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      clearTimeout(timer);
    };
  });

  /** What the placer needs: how wide this tile should be, and the ratio
   *  its height comes from.
   *
   *  ⭐ SPAN READS THE DECLARED RATIO AND ONLY THE DECLARED RATIO.
   *  A measured ratio is the shape the tile currently RENDERS at, which
   *  for a tile already squashed onto the #652 floor is the FLOORED
   *  shape — a 267x62 box measures 4.3:1, not the 8.68:1 it truly is.
   *  Asking "is this too thin to read?" of a number the thinness has
   *  already been clamped out of cannot work. The consequence is that an
   *  asset whose preview predates #757, and which therefore has no
   *  recorded dimensions, does not span until `aa rebuild-previews` has
   *  given it some. */
  function shape(item: { id: string }, ladderReady: boolean, g: MasonryGeometry): PlaceableTile {
    const declared = cardTileRatio(item, ladderReady);
    const span = tileSpan(declared, g);
    if (declared !== null) return { id: item.id, span, estimateRatio: declared };
    // Measured ratios come back as height ÷ width, hence the invert.
    const measured = measuredRatio.get(item.id);
    const estimateRatio = measured !== undefined && measured > 0 ? 1 / measured : null;
    return { id: item.id, span, estimateRatio };
  }

  /** Harvest what every rendered tile currently measures. Merged rather
   *  than rebuilt, so a tile stays remembered if it ever leaves the DOM.
   *  One layout read per tile per PAGE — not per frame. */
  function snapshotRatios(): void {
    const el = containerEl;
    if (!el) return;
    for (const tile of el.querySelectorAll<HTMLElement>('[data-tile-id]')) {
      const id = tile.dataset.tileId;
      const w = tile.offsetWidth;
      const h = tile.offsetHeight;
      if (id && w > 0 && h > 0) measuredRatio.set(id, h / w);
    }
  }

  function place(list: Array<{ id: string }>, g: MasonryGeometry, ladderReady: boolean): void {
    // The layout epoch. A change to either input invalidates every
    // existing placement, so we start over. An APPEND is the case where
    // neither changed and the list only grew at the end — the only case
    // that must not move anything.
    const ep = `${g.colCount}|${ladderReady}`;
    const append =
      ep === epoch &&
      list.length >= placedIds.length &&
      placedIds.every((id, i) => list[i]?.id === id);

    snapshotRatios();

    let from: number;
    if (append) {
      from = placedIds.length;
    } else {
      epoch = ep;
      wall = emptyState(g.colCount);
      from = 0;
    }

    const shapes = list.map((it) => shape(it, ladderReady, g));
    wall = placeInto(wall, shapes, from, g);
    placedIds = list.map((it) => it.id);
    placements = wall.placements;
    colRows = wall.colRows;
  }

  $effect(() => {
    const list = items;
    const g = geo;
    const ladderReady = previewLadder.rungs.length > 0;
    untrack(() => place(list, g, ladderReady));
  });

  /** Skeletons are dealt to the shortest columns, which is where the
   *  next page will land. Derived rather than placed, because they are
   *  not feed positions — they never enter `wall`, so they cannot
   *  perturb where the page they are waiting for goes. */
  const skeletons = $derived.by(() => {
    if (!loading || colRows.length === 0) return [];
    const rows = colRows.slice();
    const height = masonryTileHeight(geo.colWidth, 1, geo.minTilePx);
    const span = tileRows(height, geo.gapPx);
    return Array.from({ length: SKELETONS }, () => {
      let col = 0;
      for (let i = 1; i < rows.length; i++) if (rows[i] < rows[col]) col = i;
      const row = rows[col] + 1;
      rows[col] += span;
      return { col, row, rows: span };
    });
  });
</script>

<div
  bind:this={containerEl}
  class="posts-masonry"
  style="--tile-min: {tileMin}; --masonry-row-unit: {ROW_UNIT_PX}px; grid-template-columns: repeat({geo.colCount}, minmax(0, 1fr));"
  role="list"
>
  <!-- See `probeEl`: absolutely positioned + zero height so it is not a
       grid item and takes no part in the layout it is measuring. -->
  <div bind:this={probeEl} class="masonry-probe" aria-hidden="true"></div>
  {#each placements as p (p.id)}
    <div
      role="listitem"
      data-tile-id={p.id}
      data-tile-col={p.col}
      data-tile-span={p.span}
      aria-posinset={p.index + 1}
      aria-setsize={items.length}
      style="grid-column: {p.col + 1} / span {p.span}; grid-row: {p.row} / span {p.rows};"
    >
      {@render card(items[p.index], 'masonry')}
    </div>
  {/each}
  {#each skeletons as s, i (i)}
    <div
      class="aspect-square rounded-lg bg-surface-elevated border border-border animate-pulse"
      style="grid-column: {s.col + 1} / span 1; grid-row: {s.row} / span {s.rows};"
    ></div>
  {/each}
</div>

<style>
  /* One grid, not N block flows. Every tile is explicitly placed, so
     auto-flow never runs — and `dense` in particular is neither used
     nor useful here (ADR 0079 §3). */
  .posts-masonry {
    position: relative;
    display: grid;
    column-gap: 0.5rem;
    /* The inter-tile gap is inside each tile's row reservation (see
       `tileRows`), so a row gap here would double it — and it would
       apply between EVERY lattice row, not between tiles. */
    row-gap: 0;
    /* The lattice. `auto` as the maximum is the safety net for cards
       whose flow chrome we cannot predict: those grow their own tracks
       rather than overlapping the tile below. When the reservation is
       right it never engages. */
    grid-auto-rows: minmax(var(--masonry-row-unit, 4px), auto);
    /* Tiles keep their own height inside a reservation that rounds up to
       the lattice; stretching them would hand the slack to the card. */
    align-items: start;
  }
  .masonry-probe {
    position: absolute;
    top: 0;
    left: 0;
    height: 0;
    width: min(var(--tile-min, 22rem), 100%);
    pointer-events: none;
    visibility: hidden;
  }
</style>
