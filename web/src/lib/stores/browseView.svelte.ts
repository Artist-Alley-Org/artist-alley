// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Browse-feed view state: which layout the user picked and how big the
// tiles are. Persists to localStorage so reloads + tab-restores honour
// the choice.
//
// Modes
//   grid       — cards in an auto-filling grid
//   masonry    — CSS multi-column flow (variable card heights)
//   thumbnail  — same as grid, two rungs denser (preview wall)
//   list       — sortable spreadsheet-style table with toggleable
//                columns (see LIST_COLUMNS)
//   feed       — single column, image at full width (phone default)
//
// # Size is the lever; column count is the outcome
//
// This store used to own a `resolveCols(mode, size)` that turned the
// stepper into a column COUNT, which the grid then hard-coded via
// `repeat(var(--cols), 1fr)`. That cannot work across a 390px phone
// and a 3840px 32:9 display at the same time: any fixed count is
// simultaneously too many for one and too few for the other, and the
// only fix is to enumerate widths with breakpoints — a new bug for
// every aspect ratio nobody enumerated.
//
// So it's inverted. The stepper picks `tileMin` (a SIZE), the grid
// declares `repeat(auto-fill, minmax(var(--tile-min), 1fr))`, and the
// column count falls out of the available width — continuously, with
// no breakpoints, no JS, and no resize listeners. Flickr's
// justified-layout reaches the same conclusion from the other side:
// it has no columns knob at all, only a target row height.
//
// The payoff is that one number satisfies both ends. The 22rem default
// yields exactly 1 column at 390px and 5 at 1920px — the counts the old
// mapping produced at those widths — without either being written down.

import { browser } from '$app/environment';

export type ViewMode = 'grid' | 'masonry' | 'thumbnail' | 'list' | 'feed';
export type SortDir = 'asc' | 'desc';
export type FeedFilter = 'team' | 'trending' | 'latest' | 'following';

const STORAGE_MODE = 'aa_browse_mode';
/** Legacy 1..7 column-count stepper. Read once, migrated, never written. */
const STORAGE_SIZE_LEGACY = 'aa_browse_size';
const STORAGE_TILE = 'aa_browse_tile_min';
const STORAGE_COLS = 'aa_browse_list_cols';
const STORAGE_SORT = 'aa_browse_list_sort';
const STORAGE_FILTER = 'aa_browse_filter';
const STORAGE_FEED_DIR = 'aa_browse_feed_dir';

// ── The tile-size ladder, in rem. The stepper walks these rungs; the
//    value lands in `--tile-min` and the grid does the rest.
//
//    The rungs are spaced roughly geometrically because column count is
//    inversely proportional to tile width — even rem steps would bunch
//    up all the interesting column changes at the small end.
//
//    Calibrated against the MEASURED content width at 1920px — 1844px,
//    not the 1872px the shell's padding suggests — so each rung lands
//    on a whole column count there:
//      10rem→11 cols · 12→9 · 13.5→8 · 16→7 · 18→6 · 22→5 · 28→4 · 38→3 · 57→2
//    The odd-looking rungs are why this list is measured rather than
//    derived. 23rem is 368px — six pixels over the 362px that fits five
//    columns in 1844px — so the obvious round number silently rendered
//    four. 14rem is 224px, half a pixel over the 223.5px that fits
//    eight. Both round numbers are wrong by less than a pixel and by a
//    whole column. Re-measure if the shell's padding changes.
const TILE_STEPS_REM = [10, 12, 13.5, 16, 18, 22, 28, 38, 57] as const;
const TILE_MIN_IDX = 0;
const TILE_MAX_IDX = TILE_STEPS_REM.length - 1;

/** Index 5 → 22rem → 1 column at 390px, 5 at 1920px, 10 at 3840px.
 *  Both ends fall out of the one number: 5-at-1920 preserves exactly
 *  what the old default rendered (no regression on the width that
 *  matters most), and 1-at-390 is the explicit product call. Neither
 *  is written down as a rule — they're consequences. */
const DEFAULT_TILE_IDX = 5;

/** thumbnail is the same ladder, one rung ROOMIER than grid — at the
 *  default that is 28rem → 4 columns at 1920px, against grid's 22rem → 5.
 *
 *  This inverts the previous -2 ("a dense preview wall", 16rem → 7
 *  columns), and the inversion is the point (#556). thumbnail is the
 *  DETAILS view: it carries a title header, the thumb, and a metadata
 *  footer, and the owner's ask is "info at a glance". A details tile
 *  that is denser than the plain grid tile is a contradiction — it was
 *  why the metadata read as a cramped caption strip. Roomier than grid
 *  is the product intent now; do not "restore" the dense wall without
 *  re-reading #556. */
const THUMBNAIL_RUNG_OFFSET = 1;

// ── The tile-size clamp, in one place (#639) ─────────────────────────
//
// `tileMin` is a three-zone clamp and `tileSizes` is the SAME three
// zones spelled as an `<img sizes>` list. They have to agree — a `sizes`
// that advertises a different width than the layout gives the tile makes
// the browser pick the wrong rung — so both read the two constants
// below and neither restates the arithmetic.
//
// CEIL_AT_PX is the viewport width at which the vw zone reaches the rem
// ceiling: `R rem` and `R·(100·16/CEIL_AT_PX) vw` are the same length
// there, by construction.
// FLOOR_RATIO then places the floor handoff at FLOOR_RATIO·CEIL_AT_PX
// (0.4 · 1920 = 768px), because `FLOOR_RATIO·R rem` is what the vw zone
// resolves to at that width. Change either number and BOTH the clamp and
// the sizes list move together.
const CEIL_AT_PX = 1920;
const FLOOR_RATIO = 0.4;
/** px per rem, for converting the ceiling into a vw percentage. Matches
 *  the browser default root size; a user with a larger root gets a
 *  proportionally larger rem floor/ceiling anyway, so the vw zone stays
 *  the only part expressed in absolute viewport units. */
const ROOT_PX = 16;
/** The viewport width below which the clamp sits on its floor. */
const FLOOR_BELOW_PX = CEIL_AT_PX * FLOOR_RATIO;

/** The clamp's three zones for a rung, as numbers. */
function clampZones(rem: number): { floorRem: number; vw: number; ceilRem: number } {
  return {
    floorRem: +(rem * FLOOR_RATIO).toFixed(2),
    vw: +((rem * ROOT_PX * 100) / CEIL_AT_PX).toFixed(2),
    ceilRem: rem,
  };
}

/** `--tile-min` for a rung: the CSS lever the layout actually uses. */
export function tileMinFor(rem: number): string {
  const { floorRem, vw, ceilRem } = clampZones(rem);
  return `clamp(${floorRem}rem, ${vw}vw, ${ceilRem}rem)`;
}

/** The `<img sizes>` list for a rung — see `BrowseViewState.tileSizes`
 *  for the full argument. Exported so a card that renders outside a
 *  browse surface has one honest default instead of a literal `22rem`. */
export function tileSizesFor(rem: number): string {
  const { floorRem, vw, ceilRem } = clampZones(rem);
  return (
    `auto, (max-width: ${FLOOR_BELOW_PX}px) ${floorRem}rem, ` +
    `(max-width: ${CEIL_AT_PX}px) ${vw}vw, ${ceilRem}rem`
  );
}

/** The hint for a card whose caller passed none. Derived from the
 *  default rung rather than written out, so it cannot drift from the
 *  ladder the way the literal `'22rem'` it replaces did — that literal
 *  survived two rung recalibrations without moving. */
export const DEFAULT_TILE_SIZES = tileSizesFor(TILE_STEPS_REM[DEFAULT_TILE_IDX]);

const DEFAULT_MODE: ViewMode = 'grid';
/** Phones default to `feed` — but only when nothing is stored, and only
 *  once at hydration. See `init()`. */
const COARSE_DEFAULT_MODE: ViewMode = 'feed';

const VALID_MODES: ReadonlyArray<ViewMode> = ['grid', 'masonry', 'thumbnail', 'list', 'feed'];

/** Modes whose column count is fixed at 1, so the size stepper is inert. */
const SINGLE_COLUMN_MODES: ReadonlyArray<ViewMode> = ['list', 'feed'];

function readMode(): ViewMode | null {
  if (!browser) return null;
  const v = localStorage.getItem(STORAGE_MODE);
  return (VALID_MODES as ReadonlyArray<string>).includes(v ?? '') ? (v as ViewMode) : null;
}

/** Read the tile-size rung, migrating the legacy column-count stepper.
 *
 *  Legacy `size` (1..7) mapped to grid columns 2..8, so bigger `size`
 *  meant MORE columns and therefore SMALLER tiles — the opposite of
 *  what its own +/- labels claimed. The ladder is indexed by tile size,
 *  so the migration inverts: `idx = 9 - size` reproduces the same
 *  column count at 1920px that the stored value used to produce.
 *  Default 4 maps to index 5, which is also the new default, so a user
 *  who never touched the stepper sees no change at all. */
function readTileIdx(): number {
  if (!browser) return DEFAULT_TILE_IDX;
  const raw = localStorage.getItem(STORAGE_TILE);
  if (raw !== null) {
    const n = parseInt(raw, 10);
    if (!Number.isNaN(n) && n >= TILE_MIN_IDX && n <= TILE_MAX_IDX) return n;
    return DEFAULT_TILE_IDX;
  }
  const legacy = localStorage.getItem(STORAGE_SIZE_LEGACY);
  if (legacy !== null) {
    const s = parseInt(legacy, 10);
    if (!Number.isNaN(s) && s >= 1 && s <= 7) {
      return Math.max(TILE_MIN_IDX, Math.min(TILE_MAX_IDX, 9 - s));
    }
  }
  return DEFAULT_TILE_IDX;
}

function writeMode(v: ViewMode): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_MODE, v); } catch { /* quota / disabled */ }
}
function writeTileIdx(n: number): void {
  if (!browser) return;
  try {
    localStorage.setItem(STORAGE_TILE, String(n));
    // The legacy key is a column count and can't round-trip a tile
    // size. Drop it so a later read can't resurrect a stale density.
    localStorage.removeItem(STORAGE_SIZE_LEGACY);
  } catch { /* quota / disabled */ }
}

// ── List-view column registry. Each column id is a stable string the
//    UI uses for visibility + sort persistence. Adding a column here
//    plus the matching i18n key under browse.col.* is enough for it to
//    appear in the picker. Default visibility keeps the initial table
//    readable on smaller widths.
export interface ListColumnDef {
  id: string;
  labelKey: string;
  defaultVisible: boolean;
  sortable: boolean;
  align?: 'left' | 'right' | 'center';
  /** CSS width for the <col>. Concrete widths give the table a
   *  predictable rhythm without enabling drag-to-resize yet. */
  width?: string;
}

export const LIST_COLUMNS: ListColumnDef[] = [
  { id: 'thumbnail',    labelKey: 'browse.col.thumbnail', defaultVisible: true,  sortable: false, align: 'center', width: '3.5rem' },
  { id: 'title',        labelKey: 'browse.col.title',     defaultVisible: true,  sortable: true,  align: 'left',  width: 'minmax(16rem, 2fr)' },
  { id: 'author',       labelKey: 'browse.col.author',    defaultVisible: true,  sortable: true,  align: 'left',  width: '10rem' },
  { id: 'visibility',   labelKey: 'browse.col.visibility',defaultVisible: false, sortable: true,  align: 'left',  width: '7rem' },
  { id: 'tags',         labelKey: 'browse.col.tags',      defaultVisible: true,  sortable: false, align: 'left',  width: 'minmax(10rem, 1fr)' },
  { id: 'members',      labelKey: 'browse.col.members',   defaultVisible: true,  sortable: true,  align: 'right', width: '5rem' },
  { id: 'likes',        labelKey: 'browse.col.likes',     defaultVisible: true,  sortable: true,  align: 'right', width: '5rem' },
  { id: 'comments',     labelKey: 'browse.col.comments',  defaultVisible: false, sortable: true,  align: 'right', width: '5rem' },
  { id: 'posted_at',    labelKey: 'browse.col.posted_at', defaultVisible: true,  sortable: true,  align: 'right', width: '9rem' },
  { id: 'description',  labelKey: 'browse.col.description', defaultVisible: false, sortable: false, align: 'left', width: 'minmax(12rem, 2fr)' },
];

const DEFAULT_VISIBLE_COLS = LIST_COLUMNS.filter((c) => c.defaultVisible).map((c) => c.id);

function readColumns(): string[] {
  if (!browser) return DEFAULT_VISIBLE_COLS;
  try {
    const raw = localStorage.getItem(STORAGE_COLS);
    if (!raw) return DEFAULT_VISIBLE_COLS;
    const arr = JSON.parse(raw);
    if (!Array.isArray(arr)) return DEFAULT_VISIBLE_COLS;
    const known = new Set(LIST_COLUMNS.map((c) => c.id));
    const kept = arr.filter((id): id is string => typeof id === 'string' && known.has(id));
    return kept.length > 0 ? kept : DEFAULT_VISIBLE_COLS;
  } catch {
    return DEFAULT_VISIBLE_COLS;
  }
}
function writeColumns(ids: string[]): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_COLS, JSON.stringify(ids)); } catch { /* */ }
}

function readSort(): { col: string; dir: SortDir } {
  if (!browser) return { col: 'posted_at', dir: 'desc' };
  try {
    const raw = localStorage.getItem(STORAGE_SORT);
    if (!raw) return { col: 'posted_at', dir: 'desc' };
    const parsed = JSON.parse(raw);
    if (typeof parsed?.col === 'string' && (parsed?.dir === 'asc' || parsed?.dir === 'desc')) {
      return { col: parsed.col, dir: parsed.dir };
    }
  } catch { /* */ }
  return { col: 'posted_at', dir: 'desc' };
}
function writeSort(s: { col: string; dir: SortDir }): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_SORT, JSON.stringify(s)); } catch { /* */ }
}

const VALID_FILTERS: ReadonlyArray<FeedFilter> = ['team', 'trending', 'latest', 'following'];
function readFilter(): FeedFilter {
  if (!browser) return 'latest';
  const v = localStorage.getItem(STORAGE_FILTER);
  return (VALID_FILTERS as ReadonlyArray<string>).includes(v ?? '') ? (v as FeedFilter) : 'latest';
}
function writeFilter(v: FeedFilter): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_FILTER, v); } catch { /* */ }
}

function readFeedDir(): SortDir {
  if (!browser) return 'desc';
  const v = localStorage.getItem(STORAGE_FEED_DIR);
  return v === 'asc' || v === 'desc' ? v : 'desc';
}
function writeFeedDir(v: SortDir): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_FEED_DIR, v); } catch { /* */ }
}

class BrowseViewState {
  mode = $state<ViewMode>(DEFAULT_MODE);
  /** Rung on TILE_STEPS_REM, not a column count. */
  tileIdx = $state<number>(DEFAULT_TILE_IDX);
  /** Visible list-view columns, in the order they appear. */
  listColumns = $state<string[]>(DEFAULT_VISIBLE_COLS);
  /** Sort key + direction for the list view. */
  sort = $state<{ col: string; dir: SortDir }>({ col: 'posted_at', dir: 'desc' });
  /** Which feed segment is active (team / trending / latest / following). */
  filter = $state<FeedFilter>('latest');
  /** Sort direction for the feed itself (newest-first vs oldest-first). */
  feedDir = $state<SortDir>('desc');
  hydrated = $state(false);

  /** The active rung in rem, after the thumbnail density offset. */
  private get activeRem(): number {
    const offset = this.mode === 'thumbnail' ? THUMBNAIL_RUNG_OFFSET : 0;
    const idx = Math.max(TILE_MIN_IDX, Math.min(TILE_MAX_IDX, this.tileIdx + offset));
    return TILE_STEPS_REM[idx];
  }

  /** Minimum tile width for the current mode, as a CSS length. Feeds
   *  `--tile-min`; the browser derives the column count from it.
   *
   *  Not a bare `${R}rem`. A single absolute length can't span the
   *  useful column range at both ends: 22rem is 5 columns at 1920px but
   *  ONE column at 390px, and worse, every rung ≥ 10rem is one column
   *  at 390px — so on a phone the stepper did nothing and grid looked
   *  identical to feed. (An earlier fix capped the floor at a flat 40vw,
   *  which just pinned every rung to 2 columns instead of 1: same dead
   *  stepper, different number.)
   *
   *  So the rung drives a three-zone clamp, all three parts scaled by R
   *  so stepping moves the ACTIVE part at every width:
   *    floor  0.4·R rem — governs < 768px. Scales with the rung, so the
   *                       phone stepper spans 1–4 columns instead of
   *                       flattening.
   *    vw     R·(16/19.2) vw — governs 768–1920px. Equals the rem
   *                       ceiling exactly at 1920 and the floor exactly
   *                       at 768, so both handoffs are seamless.
   *    ceil   R rem — governs > 1920px. This IS the old ladder, so
   *                       desktop is untouched: default still 5 cols at
   *                       1920 and 10 at 3840.
   *  The grid wraps this in min(…, 100%) for overflow safety.
   *
   *  There is deliberately no `cols` getter. Nothing in the app knows
   *  the column count — it's a property of the viewport, and the only
   *  thing qualified to compute it is the layout engine. */
  get tileMin(): string {
    return tileMinFor(this.activeRem);
  }

  /** The slot width to advertise in `<img sizes>`.
   *
   *  This used to be the bare rung `${R}rem` — the clamp's CEILING —
   *  which is the width the tile occupies only above 1920px. Everywhere
   *  else it over-stated (#639). Measured on the dev library at the
   *  default rung, advertised against the painted image box:
   *    masonry 1440px  355px vs 255px   1.33x
   *    masonry  390px  352px vs 155px   2.13x   (DPR 2)
   *  Over-stating can only over-FETCH, never upscale, so on the default
   *  1024/1920/4096 ladder it changed nothing at either width — every
   *  column lands on `preview` either way. It bites where a phone at
   *  DPR 2 advertises 705 device px of need against 310 of real slot,
   *  which crosses a rung on any ladder with a smaller bottom step, and
   *  ladders are operator-configured (ADR 0071).
   *
   *  Two answers, in preference order, because `sizes` accepts a list:
   *
   *  1. `auto` — the browser uses the image's OWN laid-out width, which
   *     is the exact slot including the part no static value can express:
   *     `auto-fill` and the masonry bucketer resolve a column COUNT, and
   *     a column is `container/n`, never the bare minimum. Any static
   *     hint is wrong by up to a factor of two on that alone. Measured in
   *     Chromium: a lazy `<img>` in a 200px slot resolved `auto` to
   *     exactly 200px.
   *
   *     REQUIRES `loading="lazy"` — CardThumb sets it, and it is load-
   *     bearing, not incidental. On an eagerly-loaded image `auto`
   *     resolves to 100vw per spec, and the rest of the list is NOT
   *     consulted: measured, the same probe made eager advertised the
   *     full 1440px viewport. Dropping that attribute turns this hint
   *     into its own worst case.
   *
   *  2. The clamp, restated as media-query-conditioned lengths, for UAs
   *     that do not implement `auto` — they fail to parse it as a
   *     source-size-value and fall through to the rest of the list, which
   *     is what the list form is for. Still not exact, but wrong by the
   *     column-count remainder instead of by the whole floor-to-ceiling
   *     span.
   *
   *  NOT `var(--tile-min)`: `sizes` is not CSS and cannot see custom
   *  properties. Measured — `sizes: var(--slot)` is discarded outright
   *  and the 100vw default applies, so it is not a smaller improvement,
   *  it is silently no hint at all.
   *
   *  `min()` / `clamp()` are a different story than #639 assumed: they DO
   *  parse here. Measured in Chromium, `clamp(8.8rem, 18.33vw, 22rem)`
   *  resolved to 265px at a 1440px viewport — the correct answer — so
   *  "sizes rejects clamp()" is not why this is a media-query list. The
   *  reason is that a CSS math function inside a non-CSS attribute is a
   *  per-engine bet, while media-conditioned lengths are the original
   *  `sizes` grammar and resolve to the identical number. */
  get tileSizes(): string {
    return tileSizesFor(this.activeRem);
  }

  /** Whether dec / inc are currently meaningful. list + feed lock both:
   *  they're single-column by definition, so tile size isn't a knob. */
  get canDec() {
    return !SINGLE_COLUMN_MODES.includes(this.mode) && this.tileIdx > TILE_MIN_IDX;
  }
  get canInc() {
    return !SINGLE_COLUMN_MODES.includes(this.mode) && this.tileIdx < TILE_MAX_IDX;
  }

  /** Hydrate from localStorage. Called once from +page.svelte.
   *
   *  The default mode is resolved HERE, once, and only when nothing is
   *  stored — never in a $derived or an $effect keyed on a media query.
   *  A reactive default would fight the user: pick `grid` on a phone,
   *  rotate to landscape, and it would yank you back to `feed`. An
   *  explicit choice always wins, at every width, forever.
   *
   *  `pointer: coarse` rather than a width query on purpose — the
   *  question "is this a touch device that wants a one-handed feed?" is
   *  about input modality, not how many pixels are available. A
   *  touchscreen laptop at 1920px is coarse; a 390px browser window on
   *  a desktop is not. */
  init(): void {
    if (this.hydrated) return;
    const stored = readMode();
    this.mode = stored ?? this.defaultModeForDevice();
    this.tileIdx = readTileIdx();
    this.listColumns = readColumns();
    this.sort = readSort();
    this.filter = readFilter();
    this.feedDir = readFeedDir();
    this.hydrated = true;
  }

  private defaultModeForDevice(): ViewMode {
    if (!browser) return DEFAULT_MODE;
    return window.matchMedia?.('(pointer: coarse)').matches ? COARSE_DEFAULT_MODE : DEFAULT_MODE;
  }

  setFilter(v: FeedFilter): void {
    this.filter = v;
    writeFilter(v);
  }

  toggleFeedDir(): void {
    this.feedDir = this.feedDir === 'asc' ? 'desc' : 'asc';
    writeFeedDir(this.feedDir);
  }

  /** Resolve visible column defs in the user's chosen order. */
  get visibleColumns(): ListColumnDef[] {
    const byId = new Map(LIST_COLUMNS.map((c) => [c.id, c]));
    return this.listColumns
      .map((id) => byId.get(id))
      .filter((c): c is ListColumnDef => !!c);
  }

  toggleColumn(id: string): void {
    if (this.listColumns.includes(id)) {
      this.listColumns = this.listColumns.filter((c) => c !== id);
    } else {
      // Insert in the catalogue's canonical order so toggling on a
      // column doesn't dump it at the right edge of the table.
      const order = LIST_COLUMNS.map((c) => c.id);
      const set = new Set([...this.listColumns, id]);
      this.listColumns = order.filter((cid) => set.has(cid));
    }
    writeColumns(this.listColumns);
  }

  resetColumns(): void {
    this.listColumns = DEFAULT_VISIBLE_COLS;
    writeColumns(this.listColumns);
  }

  /** Cycle the sort for a column: asc → desc → keep desc. Clicking a
   *  different column starts fresh on asc. */
  cycleSort(col: string): void {
    if (this.sort.col === col) {
      this.sort = { col, dir: this.sort.dir === 'asc' ? 'desc' : 'asc' };
    } else {
      this.sort = { col, dir: 'asc' };
    }
    writeSort(this.sort);
  }

  setMode(m: ViewMode): void {
    this.mode = m;
    writeMode(m);
  }
  setTileIdx(n: number): void {
    const clamped = Math.max(TILE_MIN_IDX, Math.min(TILE_MAX_IDX, n));
    this.tileIdx = clamped;
    writeTileIdx(clamped);
  }
  /** `-` shrinks the tiles (so more of them fit). Note this is the
   *  opposite rung direction from the legacy stepper, which counted
   *  columns up while tiles got smaller — the labels said "size" and
   *  meant "count". Now `size` means size. */
  decSize(): void {
    if (!this.canDec) return;
    this.setTileIdx(this.tileIdx - 1);
  }
  incSize(): void {
    if (!this.canInc) return;
    this.setTileIdx(this.tileIdx + 1);
  }
}

export const browseView = new BrowseViewState();
