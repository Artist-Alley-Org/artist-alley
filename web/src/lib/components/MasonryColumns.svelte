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
  //                    DOM on every placement pass (`snapshotTiles`)
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
  // # `syncHeightsFromDom`, restored — and why deleting it was wrong
  //
  // The sibling-column version re-read every column's real
  // `offsetHeight` before each append. #747 dropped it on the argument
  // that the reservation IS the position, so a prediction that is a few
  // pixels short costs a few pixels of slack under that one tile and
  // contributes nothing to the next tile's row.
  //
  // That argument is true of POSITION and false of HEIGHT, and #1095 is
  // the bill. A tile whose asset has no recorded dimensions reserves a
  // SQUARE and then settles into the loaded image's own ratio when the
  // bytes arrive (CardThumb's resolution case 2, which `cardTileRatio`
  // cannot mirror because the number does not exist until the image
  // loads). Measured on the dev feed at 2560px / 14 columns, 9 of 432
  // tiles ended up 57-113px TALLER than they had reserved.
  //
  // On its own that is a local error. What made it a release blocker is
  // `grid-auto-rows: minmax(ROW_UNITpx, auto)`: a tile that outgrows its
  // reservation grows the row tracks it spans, and a grid's row tracks
  // are SHARED BY EVERY COLUMN. One mispredicted tile therefore pushes
  // ALL FOURTEEN columns down at that depth — the full-width band — and
  // the growth accumulates (525px of it by tile 360, measured), which is
  // why it got worse the further you scrolled. Forcing a re-place fixed
  // it only because the re-place bucketed on `measuredRatio`, i.e. on
  // the settled DOM.
  //
  // So the DOM is read back in, in the shape this layout needs:
  // `reconcile()` re-solves every tile's row line from its MEASURED
  // reservation while carrying `col` / `span` / order through untouched.
  // It runs before every append (so a new page anchors to the wall that
  // is really there) and whenever a tile changes size (so a settling
  // image is corrected in its own column instead of shoving the wall).
  //
  // `minmax(ROW_UNITpx, auto)` stays in the CSS, now as a one-frame net
  // rather than the mechanism: it keeps a tile that has just outgrown
  // its reservation from overlapping the one below while the reconcile
  // is scheduled. Removing it and reconciling was measured too — 126
  // voids → 10, but with 8 real overlaps in the frame before the
  // correction lands, which is worse than a band.
  //
  // # A placement may only render against the item it was made for
  //
  // `placements` and `items` are two independent reactive inputs to one
  // `{#each}` — the block iterates the placements and renders
  // `items[p.index]` — and they agree only while the placement pass is
  // up to date. An append never breaks that (the list grows at the end,
  // so every existing index still points at the same row). A FEED SWAP
  // breaks it on purpose: browse sets `items = []` and refills it a page
  // at a time, so the block was left iterating the previous feed's 180
  // placements while `items` held 0, then 36, then 72.
  //
  // Every index past the end handed the card `undefined`. Cards read
  // their row — PostCard reads `post.members` — so the render effect
  // threw, and a throw during a flush takes the REST OF THAT FLUSH with
  // it: the user `$effect` below never ran, for the whole refill. The
  // wall kept the old feed's rows under the new feed's posts, and the
  // tile ResizeObserver's `reconcile` (a rAF, outside the effect graph,
  // so nothing stopped it) then re-solved those rows against heights
  // measured from tiles showing DIFFERENT posts. Measured at 2560px on
  // the dev feed: 0 overlapping pairs before the swap and 10 after,
  // worst void 214px → 521px, plus three uncaught `undefined.members`
  // (#1103).
  //
  // The fix is `rendered` below, and it is deliberately a RENDER-TIME
  // gate rather than another thing to invalidate. Adding feed identity
  // to the epoch key was the obvious candidate and it would not have
  // helped: the epoch decides append-vs-re-place inside `place()`, and
  // `place()` was never reached. A `$derived` is evaluated when the
  // template reads it, so "a tile is only rendered when the current
  // `items` still stands behind its placement" holds in the same frame
  // the list changes, with no ordering to win.

  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import type { ViewMode } from '$stores/browseView.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { cardTileRatio, masonryMinTilePx, masonryTileHeight } from './cardAsset';
  import {
    ROW_UNIT_PX,
    emptyState,
    placeInto,
    reconcile,
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
  /** id → the lattice rows the tile ACTUALLY needs, harvested from the
   *  DOM in the same pass as `measuredRatio`. This is what `reconcile`
   *  reads; see its header for why the wall cannot be trusted to render
   *  at the height it reserved.
   *
   *  Cleared on a re-place, unlike `measuredRatio`: a ratio is
   *  dimensionless and survives a column-width change, a row count is
   *  the answer to "how tall at THIS width" and does not. */
  const measuredRows = new Map<string, number>();

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

  /** Harvest what every rendered tile currently measures — its shape,
   *  for the next re-place, and the rows it really occupies, for
   *  `reconcile`. Merged rather than rebuilt, so a tile stays remembered
   *  if it ever leaves the DOM.
   *
   *  One layout read per tile, and only on a placement pass or a size
   *  change — not per frame. `offsetWidth` / `offsetHeight` are the
   *  TILE's, and `align-items: start` keeps a tile at its content height
   *  even when its row tracks have been stretched, so this reads what the
   *  card is rather than what the grid grew to. */
  function snapshotTiles(g: MasonryGeometry): void {
    const el = containerEl;
    if (!el) return;
    for (const tile of el.querySelectorAll<HTMLElement>('[data-tile-id]')) {
      const id = tile.dataset.tileId;
      const w = tile.offsetWidth;
      const h = tile.offsetHeight;
      if (!id || w <= 0 || h <= 0) continue;
      measuredRatio.set(id, h / w);
      measuredRows.set(id, tileRows(h, g.gapPx));
    }
  }

  /** Re-solve the wall against the DOM. Returns whether anything moved.
   *
   *  ⚠️ Reads geometry then writes state, so it must never run from
   *  inside the placement effect's dependency graph — see the
   *  `untrack` at the call sites. */
  function reconcileFromDom(g: MasonryGeometry): boolean {
    if (wall.placements.length === 0) return false;
    snapshotTiles(g);
    const next = reconcile(wall, measuredRows);
    if (next === wall) return false;
    wall = next;
    placements = wall.placements;
    colRows = wall.colRows;
    return true;
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

    snapshotTiles(g);

    let from: number;
    if (append) {
      // Anchor the new page to the wall that is REALLY on screen, not to
      // the heights we predicted for it (#1095). A tile that settled
      // taller than its reservation has already been corrected by the
      // size observer below; this is the belt to that pair of braces —
      // whatever the cause, an append placed against reconciled column
      // bottoms cannot inherit an error from the page before it.
      wall = reconcile(wall, measuredRows);
      from = placedIds.length;
    } else {
      epoch = ep;
      wall = emptyState(g.colCount);
      // Row counts are answers at the OLD column width. The ratios are
      // dimensionless and stay.
      measuredRows.clear();
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

  /** Watch every tile for a height change and re-solve when one lands.
   *
   *  This is the trigger the prediction cannot supply: a tile with no
   *  recorded dimensions reserves a square and only learns its real
   *  shape when its bytes arrive, which is some arbitrary time after it
   *  was placed (#1095). Observing the tiles catches that — and every
   *  other cause of a height we could not compute ahead of layout,
   *  including the wrapped card chrome the `auto` track maximum was
   *  standing in for.
   *
   *  Coalesced to one rAF, so a page of 36 images landing together costs
   *  one re-solve rather than 36. It cannot loop: `align-items: start`
   *  makes a tile's height its content's, so moving its row line does
   *  not resize it, and `reconcile` returns the same state once it has
   *  agreed with the DOM.
   *
   *  No ResizeObserver (a test DOM) means the wall keeps its predicted
   *  reservations and the pre-append `reconcile` above never has
   *  measurements to act on — degraded, not broken, the same posture as
   *  the column-count observer. */
  let tileRo: ResizeObserver | undefined;
  let pendingReconcile = 0;
  const observed = new Set<Element>();

  $effect(() => {
    // Re-run on every change to the rendered set so newly rendered tiles
    // get watched. `rendered` and not `placements` because this observes
    // the DOM, and the DOM is the gated set (#1103).
    //
    // The observer is created once and only ever gains targets — calling
    // `observe` again on one it already holds would re-notify for every
    // tile in the wall on every append.
    void rendered;
    const el = containerEl;
    if (!el || typeof ResizeObserver === 'undefined') return;
    tileRo ??= new ResizeObserver(() => {
      if (pendingReconcile) return;
      pendingReconcile = requestAnimationFrame(() => {
        pendingReconcile = 0;
        untrack(() => reconcileFromDom(geo));
      });
    });
    // A feed swap destroys every tile element and builds new ones, so
    // without this the Set (and the observer) accumulate a whole wall of
    // detached nodes per swap — each of which reports a 0x0 resize on
    // its way out and buys a reconcile pass that measures nothing.
    for (const gone of observed) {
      if (gone.isConnected) continue;
      tileRo.unobserve(gone);
      observed.delete(gone);
    }
    for (const tile of el.querySelectorAll<HTMLElement>('[data-tile-id]')) {
      if (observed.has(tile)) continue;
      observed.add(tile);
      tileRo.observe(tile);
    }
  });

  $effect(() => () => {
    tileRo?.disconnect();
    tileRo = undefined;
    observed.clear();
    cancelAnimationFrame(pendingReconcile);
    pendingReconcile = 0;
  });

  /** The placements the CURRENT `items` still stands behind (#1103).
   *
   *  A placement carries the feed index it was made for, so the question
   *  is answerable exactly: `items[p.index]` must still be the row that
   *  produced `p`. The scan stops at the first disagreement rather than
   *  filtering, because a placement is only meaningful on top of the
   *  ones before it — rendering tile 90 while tiles 0-89 belong to
   *  another feed would put it in a column whose bottom edge no longer
   *  exists.
   *
   *  The common cases are both O(1)-ish in practice and neither
   *  allocates: an append agrees all the way to the old length and
   *  returns `placements` itself (so the keyed `{#each}` sees the same
   *  array identity it did before), and a swap disagrees at index 0.
   *  The full walk only happens on the pass that re-places, which is
   *  already O(n).
   *
   *  This is what keeps a tile out of the DOM rather than merely quiet:
   *  a tile that is not rendered is not measured by `snapshotTiles`
   *  either, so a stale placement cannot contribute a height to
   *  `measuredRows` under an id it no longer describes. */
  const rendered = $derived.by(() => {
    const list = placements;
    const src = items;
    let k = 0;
    while (k < list.length && src[list[k].index]?.id === list[k].id) k++;
    return k === list.length ? list : list.slice(0, k);
  });

  /** Skeletons are dealt to the shortest columns, which is where the
   *  next page will land. Derived rather than placed, because they are
   *  not feed positions — they never enter `wall`, so they cannot
   *  perturb where the page they are waiting for goes.
   *
   *  Withheld while the wall is mid-swap: `colRows` describes the whole
   *  of `placements`, so dealing skeletons against it while only a
   *  prefix is on screen would park them below a wall that is not
   *  there. One frame later `place()` has run and they land properly. */
  const skeletons = $derived.by(() => {
    if (!loading || colRows.length === 0 || rendered.length !== placements.length) return [];
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
  <!-- `rendered`, not `placements`: see its header. The gate is what
       makes `items[p.index]` below a total function. -->
  {#each rendered as p (p.id)}
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
