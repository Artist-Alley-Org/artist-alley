// Browse-feed view state: which layout the user picked and how many
// columns wide. Persists to localStorage so reloads + tab-restores
// honour the choice.
//
// Modes
//   grid       — square cards in a fixed-column grid
//   masonry    — CSS multi-column flow (variable card heights)
//   thumbnail  — same as grid but +3 cols (denser preview wall)
//   list       — sortable spreadsheet-style table with toggleable
//                columns (see LIST_COLUMNS)
//
// The store hands the +page.svelte body two derived values:
//   layoutMode  — translates `mode` into the kind of layout to apply
//   cols        — concrete column count for the current mode + size
// keeping the math out of the template.

import { browser } from '$app/environment';

export type ViewMode = 'grid' | 'masonry' | 'thumbnail' | 'list';
export type SortDir = 'asc' | 'desc';
export type FeedFilter = 'team' | 'trending' | 'latest' | 'following';

const STORAGE_MODE = 'aa_browse_mode';
const STORAGE_SIZE = 'aa_browse_size';
const STORAGE_COLS = 'aa_browse_list_cols';
const STORAGE_SORT = 'aa_browse_list_sort';
const STORAGE_FILTER = 'aa_browse_filter';
const STORAGE_FEED_DIR = 'aa_browse_feed_dir';

// Size range. `size` is the user's slider position; the actual
// rendered cols depend on view mode (see resolveCols).
const SIZE_MIN = 1;
const SIZE_MAX = 7;
const DEFAULT_SIZE = 4;
const DEFAULT_MODE: ViewMode = 'grid';

function readMode(): ViewMode {
  if (!browser) return DEFAULT_MODE;
  const v = localStorage.getItem(STORAGE_MODE);
  if (v === 'grid' || v === 'masonry' || v === 'thumbnail' || v === 'list') return v;
  return DEFAULT_MODE;
}

function readSize(): number {
  if (!browser) return DEFAULT_SIZE;
  const raw = localStorage.getItem(STORAGE_SIZE);
  if (!raw) return DEFAULT_SIZE;
  const n = parseInt(raw, 10);
  if (Number.isNaN(n) || n < SIZE_MIN || n > SIZE_MAX) return DEFAULT_SIZE;
  return n;
}

function writeMode(v: ViewMode): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_MODE, v); } catch { /* quota / disabled */ }
}
function writeSize(n: number): void {
  if (!browser) return;
  try { localStorage.setItem(STORAGE_SIZE, String(n)); } catch { /* quota / disabled */ }
}

// Resolve `size` (1..7) + mode → concrete column count. Different
// modes prefer different defaults — thumbnail is denser, list is
// always one column.
function resolveCols(mode: ViewMode, size: number): number {
  if (mode === 'list') return 1;
  if (mode === 'thumbnail') return Math.min(SIZE_MAX + 3, size + 3); // 4..10
  // grid + masonry: size + 1 → 2..8
  return size + 1;
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
  size = $state<number>(DEFAULT_SIZE);
  /** Visible list-view columns, in the order they appear. */
  listColumns = $state<string[]>(DEFAULT_VISIBLE_COLS);
  /** Sort key + direction for the list view. */
  sort = $state<{ col: string; dir: SortDir }>({ col: 'posted_at', dir: 'desc' });
  /** Which feed segment is active (team / trending / latest / following). */
  filter = $state<FeedFilter>('latest');
  /** Sort direction for the feed itself (newest-first vs oldest-first). */
  feedDir = $state<SortDir>('desc');
  hydrated = $state(false);

  /** Concrete column count for the current mode + size. */
  get cols() {
    return resolveCols(this.mode, this.size);
  }

  /** Whether dec / inc are currently meaningful. List mode locks
   *  both — the column count is fixed at 1. */
  get canDec() {
    return this.mode !== 'list' && this.size > SIZE_MIN;
  }
  get canInc() {
    return this.mode !== 'list' && this.size < SIZE_MAX;
  }

  /** Hydrate from localStorage. Called once from +page.svelte. */
  init(): void {
    if (this.hydrated) return;
    this.mode = readMode();
    this.size = readSize();
    this.listColumns = readColumns();
    this.sort = readSort();
    this.filter = readFilter();
    this.feedDir = readFeedDir();
    this.hydrated = true;
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
  setSize(n: number): void {
    const clamped = Math.max(SIZE_MIN, Math.min(SIZE_MAX, n));
    this.size = clamped;
    writeSize(clamped);
  }
  decSize(): void {
    if (!this.canDec) return;
    this.setSize(this.size - 1);
  }
  incSize(): void {
    if (!this.canInc) return;
    this.setSize(this.size + 1);
  }
}

export const browseView = new BrowseViewState();
