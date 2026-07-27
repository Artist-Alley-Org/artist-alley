<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Append-stable masonry (#651).
  //
  // # What was wrong
  //
  // This layout was `column-width` multicol. Multicol BALANCES: it fills
  // columns to equal height across the whole flow, so appending 36 tiles
  // at the end legitimately changes which column tile #4 belongs in.
  // Measured at 1440px on the 36 → 72 append, 19 of 24 sampled tiles
  // changed column and 13 moved. The user watches the thing they are
  // looking at slide sideways every time the infinite loader fires.
  //
  // The owner asked whether the render could be delayed until the
  // calculation settles. It should not be: the recalculation is neither
  // slow nor wrong. It is correct and unwanted. Delaying it only batches
  // the same jump into a bigger, later one and adds latency on top.
  //
  // # What this does instead
  //
  // N sibling columns, each an ordinary block flow. Each item is assigned
  // to the SHORTEST column at the moment it arrives, and that assignment
  // is never revisited while the column count holds. Appending therefore
  // only ever grows one column downward — there is no balancing pass, so
  // nothing above the append point can move. Pinterest, Unsplash and
  // Google Photos all land here for the same reason.
  //
  // The usual objection to hand-rolled column masonry is that you must
  // render, measure, then place — a double layout pass with a visible
  // reflow. That does not apply to us: #646 put `pixel_width` /
  // `pixel_height` on the card payload, so a tile's height is arithmetic
  // (`columnWidth / ratio`) BEFORE anything renders. No measurement pass,
  // no waiting on image load, no JS masonry library.
  //
  // # Three costs, taken deliberately
  //
  // 1. DOM ORDER STOPS MATCHING FEED ORDER. Item 2 can sit in column 3,
  //    so keyboard traversal reads down column 1, then down column 2 —
  //    column-major, not the feed's recency order. We ACCEPT that as the
  //    semantic rather than fight it: the alternatives (`aria-owns` over
  //    72 generated ids, or CSS `order` which cannot reorder across
  //    separate column boxes) are fragile in exactly the assistive tech
  //    they are meant to help. What we do NOT accept is losing the feed
  //    position, so the container is a `role="list"`, the column boxes
  //    are `role="presentation"` (transparent, so the list still owns its
  //    items), and every tile carries `aria-posinset` / `aria-setsize`
  //    with its TRUE index in the feed. A screen reader announces
  //    "item 37 of 72" correctly even though it is the 4th thing tabbed
  //    to — the traversal order is unusual, but the position never lies.
  //
  // 2. A COLUMN-COUNT CHANGE RE-BUCKETS EVERYTHING. That is a full
  //    reflow, and it is fine: it happens on resize, where the user is
  //    already changing the layout and expects it to change. Debounced
  //    (RESIZE_DEBOUNCE_MS) so a drag re-buckets once, not 60 times. The
  //    columns themselves are `flex: 1`, so width tracks continuously
  //    during the drag and only the COUNT snaps.
  //
  // 3. ASSETS WITH NO RECORDED DIMENSIONS need an estimated height.
  //    The fallback is 1:1, and not as a hedge: CardThumb reserves
  //    `aspect-square` for exactly the tiles that have no declared ratio
  //    (no dimensions, no ladder, or a generated doc/icon card that has
  //    no ratio to follow), so matching the renderer's own reservation
  //    makes the estimate EXACT at first paint rather than a guess.
  //
  //    It is only the LAST resort, though, because it goes stale for the
  //    subset that settles into a measured shape once its bytes arrive —
  //    and on this library that subset is not marginal. Measured on the
  //    dev feed, 24 of 72 tiles render at 5.33:1 or 8.8:1 from
  //    `naturalWidth` after load (audio waveforms, video posters), with
  //    nothing recorded server-side. Bucketing those as squares predicts
  //    a 269px tile where a 50px one lands, and a re-bucket built purely
  //    on that estimate left a 1144px height spread across five columns.
  //    So `estimate()` reads, in order:
  //      declared ratio → what CardThumb WILL render, authoritative and
  //                       known before the request
  //      measured cache → what this tile IS right now, harvested from
  //                       the DOM on every bucketing pass (`snapshotRatios`)
  //      1:1            → never rendered before, nothing recorded
  //    Declared beats measured deliberately: at the `ladderReady` flip
  //    the DOM still shows the square a declared tile is about to stop
  //    being, so trusting the measurement there would bake in the shape
  //    we are one frame away from replacing.
  //
  // # Height bookkeeping
  //
  // Predicted heights accumulate in `colHeights`, but before each append
  // we overwrite them with the columns' REAL `offsetHeight`. That is one
  // layout read per column per page — not per tile, not per frame — and
  // it is what stops estimator error accumulating over a long scroll:
  // whatever the last page actually did to the columns is the base the
  // next page is placed against.

  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import type { ViewMode } from '$stores/browseView.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { cardTileRatio, masonryMinTilePx, masonryTileHeight } from './cardAsset';

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

  /** Long enough that a drag re-buckets once; short enough that letting
   *  go feels immediate. */
  const RESIZE_DEBOUNCE_MS = 120;
  /** Fallback for a tile whose ratio isn't declared — see cost (3). */
  const SQUARE = 1;
  /** Skeleton tiles are square by construction (aspect-square below). */
  const SKELETONS = 8;

  interface Placed {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    item: any;
    /** Index in `items` — the feed position, for aria-posinset. */
    index: number;
  }

  let containerEl = $state<HTMLElement>();
  /** Zero-height probe whose width IS `--tile-min`. `tileMin` is a
   *  `clamp(…rem, …vw, …rem)` (browseView), so it cannot be parsed in JS
   *  — it has to be resolved by the engine. Reading a probe is how we
   *  ask CSS the same question multicol used to answer internally. */
  let probeEl = $state<HTMLElement>();

  /** Layout geometry, only ever written by `measure()`. */
  let colCount = $state(1);
  let colWidth = 0;
  let gapPx = 0;
  let lastWidth = -1;
  /** CardThumb's `min-height` floor (#652), resolved to px. Read here
   *  and not assumed, because it is declared in rem — see
   *  `masonryMinTilePx`. */
  let minTilePx = 0;

  let columns = $state<Placed[][]>([]);

  // Bucketer state. Deliberately plain (not $state): it is bookkeeping
  // for the effect that writes `columns`, and making it reactive would
  // re-enter that effect.
  let placedIds: string[] = [];
  let colHeights: number[] = [];
  let epoch = '';
  /** id → rendered height ÷ rendered width, harvested from the DOM. A
   *  dimensionless factor rather than an aspect ratio so it survives a
   *  column-width change AND carries whatever card chrome (borders, a
   *  CollectionCard footer) sits outside the thumb frame. */
  const measuredRatio = new Map<string, number>();

  /** Multicol's own column-count formula, so the wall keeps the density
   *  the `--tile-min` ladder was calibrated against (browseView's rungs
   *  are MEASURED against real column counts — see TILE_STEPS_REM). */
  function measure(): void {
    const el = containerEl;
    const probe = probeEl;
    if (!el || !probe) return;
    const width = el.clientWidth;
    if (width <= 0) return;
    const cs = getComputedStyle(el);
    gapPx = parseFloat(cs.columnGap) || 0;
    const min = probe.getBoundingClientRect().width;
    const n = min > 0 ? Math.max(1, Math.floor((width + gapPx) / (min + gapPx))) : 1;
    colWidth = (width - gapPx * (n - 1)) / n;
    minTilePx = masonryMinTilePx();
    lastWidth = width;
    colCount = n;
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
      // change can alter the column count, and re-bucketing on every
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

  /** Predicted height of `item`'s tile, in px, including the gap above
   *  it. See cost (3) for why the three sources are in this order.
   *
   *  Every branch goes through `masonryTileHeight`, which applies the
   *  #652 floor — the SAME rule CardThumb writes into `min-height`.
   *  This is the desynchronisation hazard the brief for that issue
   *  flagged: a floor that exists in CSS but not here means the
   *  bucketer places tiles against heights 30px shorter than they
   *  render, the columns drift apart over a long scroll, and the append
   *  instability #651 removed comes back through the side door.
   *  Measured ratios come back as height ÷ width, hence the invert. */
  function estimate(item: { id: string }, ladderReady: boolean): number {
    const declared = cardTileRatio(item, ladderReady);
    if (declared !== null) return masonryTileHeight(colWidth, declared, minTilePx) + gapPx;
    const measured = measuredRatio.get(item.id);
    if (measured !== undefined && measured > 0) {
      return masonryTileHeight(colWidth, 1 / measured, minTilePx) + gapPx;
    }
    return masonryTileHeight(colWidth, SQUARE, minTilePx) + gapPx;
  }

  /** Harvest what every rendered tile currently measures. Merged rather
   *  than rebuilt, so a tile stays remembered if it ever leaves the DOM.
   *  Runs in the same forced-layout batch as `syncHeightsFromDom`, so it
   *  costs reads, not extra reflows. */
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

  /** Replace predicted heights with what the columns actually measure. */
  function syncHeightsFromDom(n: number): void {
    const el = containerEl;
    if (!el) return;
    const cols = el.querySelectorAll<HTMLElement>('[data-masonry-col]');
    if (cols.length !== n) return;
    for (let i = 0; i < n; i++) colHeights[i] = cols[i].offsetHeight;
  }

  function shortest(): number {
    let best = 0;
    for (let i = 1; i < colHeights.length; i++) {
      if (colHeights[i] < colHeights[best]) best = i;
    }
    return best;
  }

  function bucket(list: Array<{ id: string }>, n: number, ladderReady: boolean): void {
    // The layout epoch. A change to either input invalidates every
    // existing placement, so we start over; within one epoch a placement
    // is permanent, which IS the append stability.
    //
    // `ladderReady` is in here because CardThumb only honours the
    // recorded dimensions once `GET /previews` has answered (see
    // cardTileRatio). Before that every tile is square and a wall bucketed
    // on those heights would stay permanently lopsided. That resolves
    // once, one RTT into the first page, well before any append.
    const ep = `${n}|${ladderReady}`;
    const append =
      ep === epoch &&
      list.length >= placedIds.length &&
      placedIds.every((id, i) => list[i]?.id === id);

    snapshotRatios();

    let next: Placed[][];
    let from: number;
    if (append) {
      syncHeightsFromDom(n);
      next = columns.map((c) => c.slice());
      from = placedIds.length;
    } else {
      epoch = ep;
      colHeights = new Array(n).fill(0);
      next = Array.from({ length: n }, () => [] as Placed[]);
      from = 0;
    }

    for (let i = from; i < list.length; i++) {
      const c = shortest();
      next[c].push({ item: list[i], index: i });
      colHeights[c] += estimate(list[i], ladderReady);
    }

    placedIds = list.map((it) => it.id);
    columns = next;
  }

  $effect(() => {
    const list = items;
    const n = colCount;
    const ladderReady = previewLadder.rungs.length > 0;
    untrack(() => bucket(list, n, ladderReady));
  });

  /** Skeletons are dealt round-robin so they appear at the foot of every
   *  column, which is where the next page will land. */
  const skeletonRows = $derived(
    Array.from({ length: colCount }, (_, c) =>
      Math.floor(SKELETONS / colCount) + (c < SKELETONS % colCount ? 1 : 0),
    ),
  );
</script>

<div
  bind:this={containerEl}
  class="posts-masonry"
  style="--tile-min: {tileMin}"
  role="list"
>
  <!-- See `probeEl`: absolutely positioned + zero height so it takes no
       part in the flex layout it is measuring. -->
  <div bind:this={probeEl} class="masonry-probe" aria-hidden="true"></div>
  {#each columns as col, ci (ci)}
    <div class="masonry-col" data-masonry-col role="presentation">
      {#each col as placed (placed.item.id)}
        <div
          role="listitem"
          data-tile-id={placed.item.id}
          aria-posinset={placed.index + 1}
          aria-setsize={items.length}
        >
          {@render card(placed.item, 'masonry')}
        </div>
      {/each}
      {#if loading}
        {#each Array(skeletonRows[ci] ?? 0) as _, i (i)}
          <div
            class="aspect-square rounded-lg bg-surface-elevated border border-border animate-pulse"
          ></div>
        {/each}
      {/if}
    </div>
  {/each}
</div>

<style>
  /* Sibling columns, not a balanced flow. `align-items: start` keeps a
     short column short instead of stretching it to the tallest — the
     columns are independent stacks, which is the whole mechanism. */
  .posts-masonry {
    position: relative;
    display: flex;
    align-items: flex-start;
    column-gap: 0.5rem;
  }
  .masonry-col {
    display: flex;
    flex-direction: column;
    /* `gap` rather than a margin on each tile: no trailing margin means
       a column's offsetHeight is exactly its content, which is what
       syncHeightsFromDom reads. */
    gap: 0.5rem;
    /* Equal share of the row. `min-width: 0` because a flex item's
       default `min-width: auto` floors it at its content's intrinsic
       width, which a wide tile would blow past. */
    flex: 1 1 0;
    min-width: 0;
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
