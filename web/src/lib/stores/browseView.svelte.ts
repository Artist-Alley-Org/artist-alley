// Browse-feed view state: which layout the user picked and how many
// columns wide. Persists to localStorage so reloads + tab-restores
// honour the choice.
//
// Modes
//   grid       — square cards in a fixed-column grid
//   masonry    — CSS multi-column flow (variable card heights)
//   thumbnail  — same as grid but +3 cols (denser preview wall)
//   list       — single-column vertical stack
//
// The store hands the +page.svelte body two derived values:
//   layoutMode  — translates `mode` into the kind of layout to apply
//   cols        — concrete column count for the current mode + size
// keeping the math out of the template.

import { browser } from '$app/environment';

export type ViewMode = 'grid' | 'masonry' | 'thumbnail' | 'list';

const STORAGE_MODE = 'aa_browse_mode';
const STORAGE_SIZE = 'aa_browse_size';

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

class BrowseViewState {
  mode = $state<ViewMode>(DEFAULT_MODE);
  size = $state<number>(DEFAULT_SIZE);
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
    this.hydrated = true;
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
