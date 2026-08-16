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
import { api } from '$api/client';
import { auth, type AccountViewDefaults } from '$stores/auth.svelte';

export type ViewMode = 'grid' | 'masonry' | 'thumbnail' | 'list' | 'feed';
export type SortDir = 'asc' | 'desc';
/** The feed segments the SERVER can actually serve.
 *
 *  This list is the client mirror of the `feed` enum on `GET /posts`
 *  (`app/api/openapi.yaml`). It used to also carry `team` and
 *  `trending`, which were never in that enum — clicking either sent an
 *  undeclared query param, the server ignored it, and the user got the
 *  plain latest feed under a label that promised otherwise (#691).
 *
 *  Neither is coming back by widening this union alone:
 *    - `trending` needs a ranking model (recency vs engagement, decay)
 *      chosen first; a guessed one is worse than none.
 *    - `team` returns with the teams browse surface (#684), where the
 *      team-scoped query gets designed once.
 *  Add a member here only when `feed` accepts it. */
export type FeedFilter = 'latest' | 'following';

const STORAGE_MODE = 'aa_browse_mode';
/** Legacy 1..7 column-count stepper. Read once, migrated, never written. */
const STORAGE_SIZE_LEGACY = 'aa_browse_size';
const STORAGE_TILE = 'aa_browse_tile_min';
const STORAGE_COLS = 'aa_browse_list_cols';
const STORAGE_COL_WIDTHS = 'aa_browse_list_col_widths';
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

/** Index 3 → 16rem (#1140, owner direction with reference DOM).
 *
 *  MEASURED, NOT CHOSEN. The owner's target is "the reference discovery
 *  wall's compact card — ~240-260px tiles at 1920, ≈7 columns". Driving
 *  every rung at 1920 and reading the rendered tile:
 *
 *      rung 2 (13.5rem)  232px   8 columns
 *      rung 3 (16rem)    265px   7 columns   ← this one
 *      rung 4 (18rem)    306px   6 columns
 *      rung 5 (22rem)    367px   5 columns   ← the old default
 *
 *  ⚠️ NO RUNG PRODUCES 250px, AND NONE CAN. A 1920 viewport's content
 *  row is ~1844px, so seven columns ARE 265px and eight ARE 232px —
 *  the two constraints in the target ("240-260px" and "≈7 columns") name
 *  different rungs, because a column count is an integer. Adding a rung
 *  was checked and does nothing: 15rem also resolves to 7 columns of
 *  265px, since the clamp is a MINIMUM and the columns divide what is
 *  left. So the column count is the half that was honoured — it is the
 *  one the owner can see — and the tile lands 5px over the stated band.
 *
 *  At 2560 the same rung gives 273px across 9 columns.
 *
 *  Stored preferences are untouched: `readTileIdx` returns the stored
 *  rung when there is one and only falls back to this constant when
 *  there is not (#709's rule — preserve, never overwrite). */
const DEFAULT_TILE_IDX = 3;

/** thumbnail is the same ladder, two rungs TIGHTER than grid — at the
 *  default that is 12rem → ~197px previews across 9 columns at 1920,
 *  against grid's 16rem → 265px across 7.
 *
 *  ⚠️ THIS REVERSES #556'S SIGN, DELIBERATELY AND ON OWNER DIRECTION
 *  (#1140, with reference DOM). The note that stood here said the
 *  opposite in as many words — "roomier than grid is the product intent
 *  now; do not restore the dense wall without re-reading #556" — so the
 *  reversal is recorded rather than quietly applied.
 *
 *  What changed is not the reasoning, it is the SHAPE the reasoning was
 *  about. #556's argument was that a details tile denser than a grid
 *  tile is a contradiction, "which was why the metadata read as a
 *  cramped caption strip". The caption strip is gone: #1136 rebuilt this
 *  density as an info PANEL — a format band above the preview, the
 *  metadata stacked one fact per row below it, a control band under
 *  that. A panel states its facts in rows and does not need the width a
 *  caption needed to avoid truncating; what it needs is to be small
 *  enough that a shelf of them fits on a screen. The owner's reference
 *  panel is exactly that: a ~200px preview in a ~336px-tall card.
 *
 *  MEASURED at 1920 (the offset resolves the stored rung 3 to effective
 *  rung 1): 197px preview, 199px card, 378px tall, 9 columns. At 2560:
 *  199px preview, 12 columns. Both on the ~200px target; our card runs
 *  ~40px taller than the reference's 336 because our stack carries the
 *  artist row the reference panel does not.
 *
 *  The offset is CLAMPED at both ends by `activeRem`, so the two lowest
 *  grid rungs resolve to the same 10rem floor in thumbnail rather than
 *  underflowing — the stepper's bottom two steps do nothing in this
 *  mode, which is the cost of one shared index and is preferable to a
 *  second stored preference for the same control. */
const THUMBNAIL_RUNG_OFFSET = -2;

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

/** The small-viewport floor for THUMBNAIL mode (#1140 rider).
 *
 *  MEASURED, and it exists because the shared floor is wrong for this
 *  mode alone. `FLOOR_RATIO · R` is calibrated for a mode whose tile is
 *  a picture: at the thumbnail default (effective rung 1 = 12rem) it
 *  resolves to 4.8rem = 77px, and 390px fits FOUR of those. A 77px
 *  "preview" in a panel that also carries a format band, a stacked
 *  metadata list and a control band is not a small card — it is a card
 *  whose picture has become an icon, with three bands of text under it.
 *
 *  9.5rem = 152px puts TWO columns across a 390px phone, which is the
 *  planning call recorded when #1140 shipped.
 *
 *  ⚠️ MEASURED, AND THE OBVIOUS NUMBER WAS WRONG BY A WHOLE COLUMN —
 *  the trap TILE_STEPS_REM's own comment warns about, walked into again
 *  on the first attempt. The browse grid at a 390px viewport is 328px
 *  wide with an 8px gap, so two columns need a floor of at most
 *  (328 - 8) / 2 = 160px. 11rem = 176px was the round number that
 *  "obviously" gives two, and it gives ONE — a 326px full-width tile,
 *  which is worse than the four it replaced and the opposite of what
 *  was asked for. Driven in a real browser at 390 and read off the
 *  computed grid, not derived.
 *
 *  9.5rem rather than the exact 10rem the arithmetic permits, because
 *  10rem lands on 328px EXACTLY — two columns with zero slack, one
 *  rounding change in the shell's padding away from collapsing to one.
 *  152px leaves 16px of room and still cannot reach three (three would
 *  need 3 x 152 + 16 = 472px, well over 328).
 *
 *  ⚠️ IT IS A FLOOR, NOT A PIN, and that distinction keeps the stepper
 *  alive. The clamp still takes the LARGER of this and the rung's own
 *  vw zone, so stepping up from the default still widens the tile on a
 *  phone; what it can no longer do is go below two columns' worth. Only
 *  the rungs at or under the default are affected, which is exactly the
 *  band the complaint was about.
 *
 *  ⚠️ OVERRIDABLE — this is a planning call, not an owner ruling. If the
 *  owner wants 3 columns at 390px, this constant is the single knob:
 *  ~7.3rem gives 3, ~5.5rem gives 4 (today's behaviour). Nothing else
 *  moves, because `tileSizesFor` reads the same number. */
const THUMBNAIL_FLOOR_REM = 9.5;

/** The clamp's three zones for a rung, as numbers.
 *
 *  `floorMinRem` raises the floor without touching the other two zones,
 *  so a mode can set a small-viewport minimum and leave desktop exactly
 *  as it was. It is a MAX rather than a replacement: a rung whose own
 *  floor is already higher keeps it, which is what stops the override
 *  from SHRINKING a large rung on a phone. */
function clampZones(
  rem: number,
  floorMinRem = 0,
): { floorRem: number; vw: number; ceilRem: number } {
  return {
    floorRem: +Math.max(rem * FLOOR_RATIO, floorMinRem).toFixed(2),
    vw: +((rem * ROOT_PX * 100) / CEIL_AT_PX).toFixed(2),
    ceilRem: rem,
  };
}

/** `--tile-min` for a rung: the CSS lever the layout actually uses. */
export function tileMinFor(rem: number, floorMinRem = 0): string {
  const { floorRem, vw, ceilRem } = clampZones(rem, floorMinRem);
  return `clamp(${floorRem}rem, ${vw}vw, ${ceilRem}rem)`;
}

/** The `<img sizes>` list for a rung — see `BrowseViewState.tileSizes`
 *  for the full argument. Exported so a card that renders outside a
 *  browse surface has one honest default instead of a literal `22rem`. */
export function tileSizesFor(rem: number, floorMinRem = 0): string {
  const { floorRem, vw, ceilRem } = clampZones(rem, floorMinRem);
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

/** `--tile-min`'s value at the default rung — the width a surface uses
 *  when it wants the app's standard tile and NOT the browse stepper's
 *  current one (#1098's featured strip).
 *
 *  The matched other half of `DEFAULT_TILE_SIZES`, and derived from the
 *  same rung for the same reason: a fixed size written as a literal
 *  `clamp(8.8rem, 18.33vw, 22rem)` would silently stop being "the
 *  default tile" the first time the ladder is recalibrated, and the
 *  drift would show up as the strip and the grid disagreeing at the
 *  default — the exact complaint #909 was filed about. */
export const DEFAULT_TILE_MIN = tileMinFor(TILE_STEPS_REM[DEFAULT_TILE_IDX]);

const DEFAULT_MODE: ViewMode = 'grid';
/** Phones default to `feed` — but only when nothing is stored, and only
 *  once at hydration. See `init()`. */
const COARSE_DEFAULT_MODE: ViewMode = 'feed';

/** Every mode this build can render. NOT the same question as "which
 *  modes may be offered" — that is the operator's, and it lives in
 *  `BrowseViewState.enabledModes` (#709). This list is what the code
 *  knows how to draw; that list is a subset the operator has chosen. */
const VALID_MODES: ReadonlyArray<ViewMode> = ['grid', 'masonry', 'thumbnail', 'list', 'feed'];

/** localStorage mirror of the operator's enabled set (#709).
 *
 *  Cached for first paint, exactly as `appearance` and `lang` cache
 *  their public boot payloads: the switcher renders before the network
 *  answers, and without a cache it would paint all five buttons and
 *  then drop the disabled ones a moment later. */
const STORAGE_ENABLED = 'aa_browse_enabled_modes';

function readEnabledCache(): ViewMode[] | null {
  if (!browser) return null;
  try {
    const raw = localStorage.getItem(STORAGE_ENABLED);
    if (!raw) return null;
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return null;
    const modes = parsed.filter((m): m is ViewMode =>
      (VALID_MODES as ReadonlyArray<unknown>).includes(m));
    return modes.length > 0 ? modes : null;
  } catch {
    return null;
  }
}

function writeEnabledCache(modes: ViewMode[]): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_ENABLED, JSON.stringify(modes)); } catch { /* quota / disabled */ }
}

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
  /** CSS width for the column's grid track — the DEFAULT the table
   *  starts from and the value double-clicking a resize handle returns
   *  to (#1100). A user's dragged width overrides it per column; see
   *  `columnWidths`. */
  width?: string;
  /** Floor for a dragged width, in px. Below this the column stops
   *  being a column: its header label clips to nothing and the cell
   *  content underneath has no room to ellipsize into.
   *
   *  Per-column rather than one global number because the columns are
   *  not the same kind of thing. `thumbnail` renders a 32px square and
   *  nothing else, so 48px is a legitimate size for it and a global
   *  floor of 80 would make the narrowest column in the table the one
   *  you cannot narrow. Defaults to COLUMN_MIN_PX. */
  minPx?: number;
  /** May the reader drag this column's trailing edge (#1127)? Defaults
   *  to true, which is every column #1100 shipped.
   *
   *  FALSE FOR THE FIXED-CONTROL COLUMNS, and the rule is one rule: a
   *  column whose cell renders a control or a preview at a size of its
   *  own is not a column of DATA. There is nothing inside it that more
   *  width would reveal and nothing that less width would ellipsize, so
   *  a handle there offers a gesture whose only possible outcome is
   *  whitespace — and it sits 8px from a tab stop, so every near-miss
   *  lands on a drag target instead.
   *
   *  Two columns qualify and they share this ONE flag rather than each
   *  getting a special case (#1047, owner's list amendment):
   *
   *    select     one 24px checkbox (#1127)
   *    thumbnail  one 32px preview square (#1047)
   *
   *  Both already had the other half of the symptom — a `labelKey` that
   *  resolves to an empty string, because there is no field name to put
   *  over a control. */
  resizable?: boolean;
  /** Can the reader turn this column off in the ColumnPicker? Defaults
   *  to true.
   *
   *  False for the selection column: it is the list view's only
   *  selection affordance, and a picker entry that removes the ability
   *  to select is a setting whose "off" state breaks a feature rather
   *  than hiding a field. The other four views have no equivalent
   *  because their checkbox lives on the card and was never optional. */
  hideable?: boolean;
}

/** The floor a column may be dragged to when its def names no other.
 *  Roughly a header label plus its padding — narrower and the column
 *  reads as a rendering fault rather than a choice. */
export const COLUMN_MIN_PX = 80;

export const LIST_COLUMNS: ListColumnDef[] = [
  // The selection column (#1127). FIRST, fixed, unresizable, unhideable
  // — the desktop-list idiom, and the one column whose width is decided
  // by the control inside it rather than by its content.
  { id: 'select',       labelKey: 'browse.col.select',    defaultVisible: true,  sortable: false, align: 'center', width: '2.75rem', minPx: 44, resizable: false, hideable: false },
  // The preview column (#1047). FIXED, like `select` above it and for
  // the same stated reason: its cell is a 32px square whatever the track
  // is, so dragging it only ever padded a picture with whitespace. Still
  // HIDEABLE — unlike selection, a reader who wants a denser text table
  // can turn the pictures off without losing a capability.
  { id: 'thumbnail',    labelKey: 'browse.col.thumbnail', defaultVisible: true,  sortable: false, align: 'center', width: '3.5rem', minPx: 48, resizable: false },
  { id: 'title',        labelKey: 'browse.col.title',     defaultVisible: true,  sortable: true,  align: 'left',  width: 'minmax(16rem, 2fr)' },
  { id: 'author',       labelKey: 'browse.col.author',    defaultVisible: true,  sortable: true,  align: 'left',  width: '10rem' },
  { id: 'visibility',   labelKey: 'browse.col.visibility',defaultVisible: false, sortable: true,  align: 'left',  width: '7rem' },
  { id: 'tags',         labelKey: 'browse.col.tags',      defaultVisible: true,  sortable: false, align: 'left',  width: 'minmax(10rem, 1fr)' },
  { id: 'members',      labelKey: 'browse.col.members',   defaultVisible: true,  sortable: true,  align: 'right', width: '5rem',  minPx: 64 },
  { id: 'likes',        labelKey: 'browse.col.likes',     defaultVisible: true,  sortable: true,  align: 'right', width: '5rem',  minPx: 64 },
  { id: 'comments',     labelKey: 'browse.col.comments',  defaultVisible: false, sortable: true,  align: 'right', width: '5rem',  minPx: 64 },
  { id: 'posted_at',    labelKey: 'browse.col.posted_at', defaultVisible: true,  sortable: true,  align: 'right', width: '9rem' },
  { id: 'description',  labelKey: 'browse.col.description', defaultVisible: false, sortable: false, align: 'left', width: 'minmax(12rem, 2fr)' },
];

/** The floor for one column, resolved. */
export function columnMinPx(id: string): number {
  return LIST_COLUMNS.find((c) => c.id === id)?.minPx ?? COLUMN_MIN_PX;
}

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

/** Dragged column widths in px, keyed by column id (#1100).
 *
 *  A SEPARATE key from the visible-column list, not a richer value in
 *  it, and that split is deliberate: visibility and width are set by
 *  different controls and answer different questions, and a column
 *  hidden through the picker has to come back at the width its owner
 *  left it at. Two records means "hide, re-show" is not a reset. It
 *  also means a width for a column this build no longer has is inert
 *  rather than corrupting the visible set.
 *
 *  Unknown ids and non-finite / sub-floor numbers are dropped on read
 *  rather than repaired. A width that cannot be honoured is not a
 *  preference, and the alternative — clamping it up to the floor — puts
 *  a value on screen that nobody chose. */
function readColumnWidths(): Record<string, number> {
  if (!browser) return {};
  try {
    const raw = localStorage.getItem(STORAGE_COL_WIDTHS);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const known = new Set(LIST_COLUMNS.map((c) => c.id));
    const out: Record<string, number> = {};
    for (const [id, px] of Object.entries(parsed as Record<string, unknown>)) {
      if (!known.has(id)) continue;
      if (typeof px !== 'number' || !Number.isFinite(px)) continue;
      if (px < columnMinPx(id)) continue;
      out[id] = Math.round(px);
    }
    return out;
  } catch {
    return {};
  }
}
function writeColumnWidths(w: Record<string, number>): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_COL_WIDTHS, JSON.stringify(w)); } catch { /* */ }
}

/** The stored list-view sort, or null when this device has never set
 *  one. Null is the load-bearing part: it is what `init()` reads as
 *  "no local choice here", which is the only condition under which the
 *  account preference gets to seed the value (#706). */
function readSort(): { col: string; dir: SortDir } | null {
  if (!browser) return null;
  try {
    const raw = localStorage.getItem(STORAGE_SORT);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    if (typeof parsed?.col === 'string' && (parsed?.dir === 'asc' || parsed?.dir === 'desc')) {
      return { col: parsed.col, dir: parsed.dir };
    }
  } catch { /* */ }
  return null;
}
function writeSort(s: { col: string; dir: SortDir }): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_SORT, JSON.stringify(s)); } catch { /* */ }
}

const VALID_FILTERS: ReadonlyArray<FeedFilter> = ['latest', 'following'];
/** The persisted segment, or null when this device has no valid local
 *  choice — either because it never made one or because the one it
 *  made no longer exists.
 *
 *  The allow-list is what makes shrinking `FeedFilter` safe: a browser
 *  still holding `team` or `trending` from before #691 fails the
 *  `includes`, so no removed value can reach the segmented control
 *  (which would render no active segment) or the fetch params. The
 *  stale key is also CLEARED, not just ignored — otherwise it sits in
 *  localStorage forever, silently reactivating if that string ever
 *  becomes valid again for a different reason.
 *
 *  Clearing rather than rewriting to 'latest' is the #706 amendment.
 *  Both get the dead string out, but a rewrite leaves behind a key
 *  that looks like a deliberate local choice, which would then outrank
 *  the account's `home_tab` forever on that device. A value the build
 *  cannot serve is not a choice; it is the absence of one. */
function readFilter(): FeedFilter | null {
  if (!browser) return null;
  const v = localStorage.getItem(STORAGE_FILTER);
  if ((VALID_FILTERS as ReadonlyArray<string>).includes(v ?? '')) return v as FeedFilter;
  if (v !== null) {
    try { localStorage.removeItem(STORAGE_FILTER); } catch { /* */ }
  }
  return null;
}
function writeFilter(v: FeedFilter): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_FILTER, v); } catch { /* */ }
}

/** The persisted feed direction, or null when unset — same null-means-
 *  no-local-choice contract as readMode / readFilter / readSort. */
function readFeedDir(): SortDir | null {
  if (!browser) return null;
  const v = localStorage.getItem(STORAGE_FEED_DIR);
  return v === 'asc' || v === 'desc' ? v : null;
}
function writeFeedDir(v: SortDir): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_FEED_DIR, v); } catch { /* */ }
}

// ── Account defaults (#706) ─────────────────────────────────────────
//
// The three knobs on /account/preferences → the store fields they
// seed. `GET /auth/me` carries them (see CurrentUser.default_views in
// openapi.yaml), and the root layout's load awaits that call before
// any page renders, so they are already in hand when a page's onMount
// calls init() — no second round-trip, no re-seed after paint.

function accountMode(v: AccountViewDefaults | null | undefined): ViewMode | null {
  const raw = v?.browse_layout ?? '';
  return (VALID_MODES as ReadonlyArray<string>).includes(raw) ? (raw as ViewMode) : null;
}

function accountFilter(v: AccountViewDefaults | null | undefined): FeedFilter | null {
  const raw = v?.home_tab ?? '';
  return (VALID_FILTERS as ReadonlyArray<string>).includes(raw) ? (raw as FeedFilter) : null;
}

/** `browse_sort` is a direction, not a column.
 *
 *  There is no column to choose: `GET /posts` takes no ordering
 *  parameter at all, so the feed is always `posted_at DESC, id DESC`
 *  and every ordering the client can offer is a reversal of that one
 *  sequence. `newest` is therefore the server order and `oldest` is
 *  the same rows read backwards — which is exactly what the existing
 *  newest/oldest control in ViewControls already does. The preference
 *  seeds that control; it does not add a capability behind it.
 *
 *  This is also why the vocabulary stops at two. `popular` and
 *  `trending` were offered here before #706 and could not be honoured
 *  by anything, because ranking is a server capability nobody has
 *  built — see the FeedFilter comment above for the same story one
 *  field over. */
function accountDir(v: AccountViewDefaults | null | undefined): SortDir | null {
  switch (v?.browse_sort ?? '') {
    case 'newest': return 'desc';
    case 'oldest': return 'asc';
    default:       return null;
  }
}

class BrowseViewState {
  mode = $state<ViewMode>(DEFAULT_MODE);
  /** The layouts the operator offers on this install (#709).
   *
   *  Defaults to everything this build renders, so an install that has
   *  never configured it — and a boot where the fetch has not landed or
   *  was refused — behaves exactly as it did before the setting
   *  existed. Never empty: `setEnabledModes` refuses that, because a
   *  switcher with no buttons is a browse page nobody can use. */
  enabledModes = $state<ViewMode[]>([...VALID_MODES]);
  /** Rung on TILE_STEPS_REM, not a column count. */
  tileIdx = $state<number>(DEFAULT_TILE_IDX);
  /** Visible list-view columns, in the order they appear. */
  listColumns = $state<string[]>(DEFAULT_VISIBLE_COLS);
  /** Dragged list-view column widths in px, keyed by column id (#1100).
   *  A column absent from here is on its registry default. */
  columnWidths = $state<Record<string, number>>({});
  /** Sort key + direction for the list view. */
  sort = $state<{ col: string; dir: SortDir }>({ col: 'posted_at', dir: 'desc' });
  /** Which feed segment is active (latest / following). */
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
    return tileMinFor(this.activeRem, this.floorMinRem);
  }

  /** The small-viewport floor for the ACTIVE mode (#1140 rider).
   *
   *  Thumbnail only. It is read by BOTH `tileMin` and `tileSizes` — the
   *  two have to agree or the browser picks a preview rung for a width
   *  the layout never gives the tile, which is the defect #639's shared
   *  `clampZones` exists to prevent, and a mode-specific floor applied
   *  to one of them would reintroduce it. */
  private get floorMinRem(): number {
    return this.mode === 'thumbnail' ? THUMBNAIL_FLOOR_REM : 0;
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
    return tileSizesFor(this.activeRem, this.floorMinRem);
  }

  /** Whether dec / inc are currently meaningful. list + feed lock both:
   *  they're single-column by definition, so tile size isn't a knob. */
  get canDec() {
    return !SINGLE_COLUMN_MODES.includes(this.mode) && this.tileIdx > TILE_MIN_IDX;
  }
  get canInc() {
    return !SINGLE_COLUMN_MODES.includes(this.mode) && this.tileIdx < TILE_MAX_IDX;
  }

  /** Hydrate from localStorage, then from the account, then from the
   *  built-ins. Called once from +page.svelte.
   *
   *  # Precedence: explicit local choice > account preference > default
   *
   *  Every line below reads the same way — `local ?? account ?? built-in`
   *  — and the order is the whole design of #706, not an implementation
   *  detail. The account preference is a SEED for a device that has not
   *  been set up by hand; it is not an override. Pick masonry on this
   *  laptop and that survives a reload even though the account default
   *  says grid, because the laptop now has an opinion and the account
   *  only ever spoke for laptops that didn't.
   *
   *  Which is why the seed is deliberately NOT written to localStorage.
   *  Persisting it would turn "the account says grid" into "this device
   *  chose grid", and the device would then ignore the account forever
   *  — including the next time the user changes it. Re-seeding on every
   *  hydration costs nothing and keeps the two levels distinguishable.
   *  The reverse direction is equally deliberate: `setMode` and friends
   *  write to localStorage only, so changing the view while browsing
   *  never rewrites the account preference. That is a separate, explicit
   *  act on /account/preferences.
   *
   *  `defaults` is optional so the four call sites don't each have to
   *  reach for the auth store; it falls back to what `/auth/me` already
   *  put there. Tests pass it explicitly.
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
   *  a desktop is not. An account that names a layout outranks that
   *  guess: the heuristic exists to stand in for an answer nobody gave. */
  init(defaults?: AccountViewDefaults | null): void {
    if (this.hydrated) return;
    this.tileIdx = readTileIdx();
    this.listColumns = readColumns();
    this.columnWidths = readColumnWidths();
    this.applyAccountDefaults(defaults);
    this.hydrated = true;
  }

  /** Resolve the four seedable fields as `local ?? account ?? built-in`.
   *
   *  Split out of init() because it has to run a second time in one
   *  case init() cannot reach: a guest browsing a public install signs
   *  in, and the store hydrated before there was an account to consult.
   *  The root layout re-runs this whenever the session identity
   *  changes.
   *
   *  Safe to call repeatedly by construction — it only ever reads
   *  localStorage, never writes it, so a field this device has an
   *  opinion about resolves to that same opinion every time. */
  applyAccountDefaults(defaults?: AccountViewDefaults | null): void {
    const acct = defaults !== undefined
      ? defaults
      : (auth.user?.defaultViews ?? null);

    // ONE resolver for all three rungs (#709). Written as a list rather
    // than a `??` chain because every rung has to be filtered through
    // the operator's enabled set, and a chain filtered rung-by-rung is
    // how the rungs drift apart: the next person to add a rung adds it
    // to the chain and not to the filter.
    this.mode = this.resolveMode([readMode(), accountMode(acct), this.defaultModeForDevice()]);
    this.filter = readFilter() ?? accountFilter(acct) ?? 'latest';

    // One account value, two store fields, because the app has two
    // places the same intent shows up: `feedDir` reverses the card
    // feeds, `sort` drives the list-view table header. Seeding only one
    // would make "oldest first" true in grid and false in list on the
    // same device.
    const dir = accountDir(acct);
    this.feedDir = readFeedDir() ?? dir ?? 'desc';
    this.sort = readSort() ?? { col: 'posted_at', dir: dir ?? 'desc' };
  }

  private defaultModeForDevice(): ViewMode {
    if (!browser) return DEFAULT_MODE;
    return window.matchMedia?.('(pointer: coarse)').matches ? COARSE_DEFAULT_MODE : DEFAULT_MODE;
  }

  /** Is this layout offered on this install? */
  isEnabled(m: ViewMode): boolean {
    return this.enabledModes.includes(m);
  }

  /** The one place a mode value becomes THE mode (#709).
   *
   *  Takes the rungs in precedence order and returns the first one the
   *  operator still offers, falling through the rest rather than
   *  accepting a disabled layout. Every rung goes through here — this
   *  device's stored choice, the signed-in account's default, the
   *  coarse-pointer guess — so none of them can quietly start accepting
   *  a mode the switcher no longer draws.
   *
   *  The final fallback is the first ENABLED mode, not `DEFAULT_MODE`:
   *  an operator who disabled `grid` would otherwise land every user
   *  who has no stored choice on the one layout the install refuses to
   *  render. `enabledModes` is never empty, so this always returns
   *  something real. */
  private resolveMode(rungs: Array<ViewMode | null | undefined>): ViewMode {
    for (const rung of rungs) {
      if (rung && this.isEnabled(rung)) return rung;
    }
    return this.enabledModes[0] ?? DEFAULT_MODE;
  }

  /** Apply the operator's enabled set, then re-resolve the active mode.
   *
   *  Re-resolving is the point. The set usually arrives AFTER the store
   *  hydrated — it comes off the network, the pages call `init()` on
   *  mount — so a user whose localStorage names a since-disabled layout
   *  has already had it applied by the time this runs. Without the
   *  re-resolve they would sit on a mode the switcher does not offer,
   *  which is the empty-page symptom this whole feature is about.
   *
   *  An empty or all-unknown set is IGNORED rather than applied. The
   *  server refuses to store one, so seeing one here means a stale
   *  cache or a mangled response, and honouring it would black out
   *  browse over what is at worst a display setting. */
  setEnabledModes(modes: ViewMode[]): void {
    const next = VALID_MODES.filter((m) => modes.includes(m));
    if (next.length === 0) return;
    this.enabledModes = [...next];
    this.applyAccountDefaults();
  }

  /** Read the operator's enabled set from the public boot endpoint.
   *
   *  Called once from the root layout, alongside `appearance.init()`
   *  and `lang.init()`. The cached set is applied synchronously first
   *  so the switcher's first paint is already correct.
   *
   *  A failure leaves whatever is in place — the cache, or all five.
   *  That includes the 401 a PRIVATE install returns to an anonymous
   *  caller: the endpoint is public-mode governed, and a logged-out
   *  visitor there has no browse page to configure anyway. Every mode
   *  is a valid render, so there is nothing to report to the user. */
  async loadEnabledModes(): Promise<void> {
    const cached = readEnabledCache();
    if (cached) this.setEnabledModes(cached);
    try {
      const { data } = await api.GET('/browse-views');
      if (!data?.enabled?.length) return;
      const modes = data.enabled.filter((m): m is ViewMode =>
        (VALID_MODES as ReadonlyArray<string>).includes(m));
      if (modes.length === 0) return;
      this.setEnabledModes(modes);
      writeEnabledCache(modes);
    } catch {
      // Network / parse failure — keep the cached set.
    }
  }

  setFilter(v: FeedFilter): void {
    this.filter = v;
    writeFilter(v);
  }

  toggleFeedDir(): void {
    this.feedDir = this.feedDir === 'asc' ? 'desc' : 'asc';
    writeFeedDir(this.feedDir);
  }

  /** Resolve visible column defs in the user's chosen order.
   *
   *  UNHIDEABLE COLUMNS ARE FORCED IN, in the catalogue's canonical
   *  position, whatever the stored list says. Without this the selection
   *  column (#1127) would be missing for every reader who has ever
   *  opened the ColumnPicker: their `listColumns` was written before the
   *  column existed, `readColumns` keeps only ids it recognises, and the
   *  new one is simply not in their array. The alternative — a
   *  migration that rewrites everyone's localStorage on first load — has
   *  to run exactly once and be right, and re-derives on every read what
   *  this expresses as a rule. */
  get visibleColumns(): ListColumnDef[] {
    const byId = new Map(LIST_COLUMNS.map((c) => [c.id, c]));
    const chosen = new Set(this.listColumns);
    for (const c of LIST_COLUMNS) {
      if (c.hideable === false) chosen.add(c.id);
    }
    // Ordered by the reader's list, then anything forced in slotted back
    // into catalogue order — which for `select` means first, where the
    // idiom puts it.
    const ordered = LIST_COLUMNS.filter((c) => chosen.has(c.id) && c.hideable === false).map(
      (c) => c.id,
    );
    for (const id of this.listColumns) if (!ordered.includes(id)) ordered.push(id);
    return ordered.map((id) => byId.get(id)).filter((c): c is ListColumnDef => !!c);
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

  /** The grid track for one column: the user's dragged width if there
   *  is one, otherwise the registry default (#1100).
   *
   *  A dragged width is a FIXED `px` track, and it replaces a flexible
   *  default (`minmax(16rem, 2fr)`) rather than being clamped inside
   *  it. That is what dragging means: the moment a column is given a
   *  size by hand, it stops taking a share of the leftover. Columns
   *  nobody has touched keep their `fr` and absorb the remainder, so
   *  the table still fills its container. */
  columnTrack(c: ListColumnDef): string {
    const px = this.columnWidths[c.id];
    return px ? `${px}px` : (c.width ?? '1fr');
  }

  /** Set one column's width, clamped up to its floor. Clamping rather
   *  than refusing: a drag past the floor should PARK the column at the
   *  minimum and keep tracking the pointer back out, not freeze the
   *  handle and lose the gesture. */
  setColumnWidth(id: string, px: number): void {
    if (!Number.isFinite(px)) return;
    const next = Math.max(columnMinPx(id), Math.round(px));
    if (this.columnWidths[id] === next) return;
    this.columnWidths = { ...this.columnWidths, [id]: next };
    writeColumnWidths(this.columnWidths);
  }

  /** Drop one column's width so it falls back to the registry default. */
  resetColumnWidth(id: string): void {
    if (!(id in this.columnWidths)) return;
    const next = { ...this.columnWidths };
    delete next[id];
    this.columnWidths = next;
    writeColumnWidths(this.columnWidths);
  }

  /** Put the table back the way it shipped — which is BOTH halves.
   *
   *  Widths go too, deliberately. "Reset columns" that restored the
   *  default set of columns at whatever widths the last drag left them
   *  is a half reset, and the state it produces — the default columns,
   *  none of them the default size — is one the user cannot get out of
   *  except by dragging each one back by hand. */
  resetColumns(): void {
    this.listColumns = DEFAULT_VISIBLE_COLS;
    writeColumns(this.listColumns);
    this.columnWidths = {};
    writeColumnWidths(this.columnWidths);
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

  /** Switch layout. Refuses a mode the operator does not offer (#709).
   *
   *  The switcher already hides those buttons, so this guard is for the
   *  paths that are not the switcher: a stale component holding an old
   *  list, a keyboard shortcut, a future deep link carrying a mode. It
   *  also stops a disabled mode reaching localStorage, which would
   *  otherwise outlive the session and have to be filtered out on every
   *  subsequent boot. */
  setMode(m: ViewMode): void {
    if (!this.isEnabled(m)) return;
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
