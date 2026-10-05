// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// EbookSession — shared reactive state between EpubView (the
// canvas-area reader) and EbookTool (the side-panel toolbox).
// Mirrors the SpriteSession / WhiteboardSession pattern: object-
// literal $state holding every field both sides touch, plus a
// few helper methods bound on directly. Both EpubView and the
// EbookTool's Body bind to the same instance, so picking a TOC
// entry in the side panel flips the reader's currentIdx without
// an event bus.
//
// All UX-state lives here, including bookmarks (localStorage-
// persisted per-asset) and reading settings (per-tab so a user
// who likes 22px persists across asset swaps inside one session).

// #1255 — the shared web-storage accessor. This module used to hold a
// private `readLS`/`writeLS` pair, and four sibling sessions held
// byte-identical copies of it; one implementation now, imported by name.
import { readStoredJSON, writeStoredJSON } from '$lib/util/storage';

export interface EbookSpineEntry {
  idx: number;
  label: string;
  href: string;
  media_type: string;
}

export interface EbookBookmark {
  /** Spine index the user was on. */
  idx: number;
  /** Scroll offset inside the chapter iframe (px), 0 if unknown. */
  scroll: number;
  /** Optional user-typed note. */
  note: string;
  /** ISO timestamp. */
  createdAt: string;
}

export interface EbookSearchHit {
  /** Spine index of the chapter the hit lives in. */
  chapterIdx: number;
  /** Chapter label for display. */
  chapterLabel: string;
  /** ~120 char excerpt with the match in the middle. */
  snippet: string;
  /** Character offset inside the plain-text chapter — useful
   *  for jumping the iframe to the position once anchor support
   *  lands. Not used for navigation yet (we just goTo the chapter). */
  charOffset: number;
}

export type EbookTheme = 'light' | 'dark' | 'sepia';
export type EbookFontFamily = 'system' | 'serif' | 'mono';

export interface EbookSession {
  // ── Spine (chapter list) ───────────────────────────────────
  spine: EbookSpineEntry[];
  spineLoading: boolean;
  spineError: string | null;

  // ── Current chapter ────────────────────────────────────────
  currentIdx: number;
  chapterLoading: boolean;
  chapterError: string | null;

  // ── Reading settings ───────────────────────────────────────
  /** CSS px for the iframe's body font-size. Default 18. */
  fontSize: number;
  /** Theme — drives the iframe's CSS color tokens. */
  theme: EbookTheme;
  /** Body font stack. System (UI default) / serif (literary
   *  default) / mono (code-heavy texts). */
  fontFamily: EbookFontFamily;
  /** Reading column width in rem. Drives iframe body max-width.
   *  Range 32 (narrow paperback) → 90 (near-full bleed). */
  maxWidth: number;
  /** Per-chapter vertical scroll offset (px). EpubView writes
   *  this on scroll and restores it on chapter load so a user
   *  who reopens the asset lands exactly where they left off.
   *  Keyed by chapter idx. */
  scrollByChapter: Record<number, number>;

  // ── Bookmarks ──────────────────────────────────────────────
  bookmarks: EbookBookmark[];

  // ── Search ─────────────────────────────────────────────────
  searchQuery: string;
  searchResults: EbookSearchHit[];
  searchBusy: boolean;
  searchError: string | null;
}

export interface EbookSessionOpts { assetId: string; }

export interface EbookSessionMethods {
  /** Fetch the spine. Idempotent — bails when already populated. */
  loadSpine(): Promise<void>;
  goTo(idx: number): void;
  goNext(): void;
  goPrev(): void;
  addBookmark(note?: string): void;
  removeBookmark(idx: number, createdAt: string): void;
  setFontSize(px: number): void;
  setTheme(t: EbookTheme): void;
  setFontFamily(f: EbookFontFamily): void;
  setMaxWidth(rem: number): void;
  /** Record the user's scroll position within the active chapter.
   *  Throttling lives at the EpubView layer; this just persists. */
  setScroll(chapterIdx: number, px: number): void;
  runSearch(query: string): Promise<void>;
  clearSearch(): void;
}

export type EbookSessionInstance = EbookSession & EbookSessionMethods & { assetId: string };

// localStorage keys are scoped per asset id so each ebook keeps
// its own reading position + bookmarks. Reading settings are
// session-global (per tab) since users tend to apply the same
// font / theme to every book.
const POS_KEY        = (id: string) => `aa.ebook.${id}.chapter`;
const SCROLL_KEY     = (id: string) => `aa.ebook.${id}.scroll`;
const BOOKMARKS_KEY  = (id: string) => `aa.ebook.${id}.bookmarks`;
const FONT_KEY       = 'aa.ebook.fontSize';
const FONT_FAM_KEY   = 'aa.ebook.fontFamily';
const THEME_KEY      = 'aa.ebook.theme';
const WIDTH_KEY      = 'aa.ebook.maxWidth';

export function createEbookSession(opts: EbookSessionOpts): EbookSessionInstance {
  const state = $state<EbookSession>({
    spine: [],
    spineLoading: false,
    spineError: null,
    currentIdx: readStoredJSON<number>(POS_KEY(opts.assetId), 0),
    chapterLoading: false,
    chapterError: null,
    fontSize: readStoredJSON<number>(FONT_KEY, 18),
    theme: readStoredJSON<EbookTheme>(THEME_KEY, 'dark'),
    fontFamily: readStoredJSON<EbookFontFamily>(FONT_FAM_KEY, 'system'),
    maxWidth: readStoredJSON<number>(WIDTH_KEY, 56),
    scrollByChapter: readStoredJSON<Record<number, number>>(SCROLL_KEY(opts.assetId), {}),
    bookmarks: readStoredJSON<EbookBookmark[]>(BOOKMARKS_KEY(opts.assetId), []),
    searchQuery: '',
    searchResults: [],
    searchBusy: false,
    searchError: null,
  });

  async function loadSpine() {
    if (state.spineLoading) return;
    if (state.spine.length > 0) return;
    state.spineLoading = true;
    state.spineError = null;
    try {
      const r = await fetch(`/api/v1/assets/${opts.assetId}/epub/spine`, {
        credentials: 'include',
      });
      if (!r.ok) throw new Error(`spine HTTP ${r.status}`);
      state.spine = (await r.json()) as EbookSpineEntry[];
      // Clamp persisted position to the new spine length so a re-
      // upload that shrunk the chapter count doesn't trap the
      // user at an out-of-range idx.
      if (state.currentIdx < 0 || state.currentIdx >= state.spine.length) {
        state.currentIdx = 0;
      }
    } catch (e) {
      state.spineError = e instanceof Error ? e.message : String(e);
    } finally {
      state.spineLoading = false;
    }
  }

  function goTo(idx: number) {
    if (idx < 0 || idx >= state.spine.length) return;
    if (state.currentIdx === idx) return;
    state.currentIdx = idx;
    writeStoredJSON(POS_KEY(opts.assetId), idx);
  }
  function goNext() { goTo(state.currentIdx + 1); }
  function goPrev() { goTo(state.currentIdx - 1); }

  function addBookmark(note: string = '') {
    const bm: EbookBookmark = {
      idx: state.currentIdx,
      scroll: 0,
      note: note.trim(),
      createdAt: new Date().toISOString(),
    };
    state.bookmarks = [...state.bookmarks, bm];
    writeStoredJSON(BOOKMARKS_KEY(opts.assetId), state.bookmarks);
  }
  function removeBookmark(idx: number, createdAt: string) {
    state.bookmarks = state.bookmarks.filter(
      (b) => !(b.idx === idx && b.createdAt === createdAt),
    );
    writeStoredJSON(BOOKMARKS_KEY(opts.assetId), state.bookmarks);
  }

  function setFontSize(px: number) {
    const clamped = Math.max(12, Math.min(28, Math.round(px)));
    state.fontSize = clamped;
    writeStoredJSON(FONT_KEY, clamped);
  }
  function setTheme(t: EbookTheme) {
    state.theme = t;
    writeStoredJSON(THEME_KEY, t);
  }
  function setFontFamily(f: EbookFontFamily) {
    state.fontFamily = f;
    writeStoredJSON(FONT_FAM_KEY, f);
  }
  function setMaxWidth(rem: number) {
    // Clamp generously — 32rem is paperback narrow, 90rem is
    // near-full bleed on a 1440px panel. Going below 32 produces
    // a confetti of two-word lines; above 90 the eye can't sweep
    // a single line comfortably.
    const clamped = Math.max(32, Math.min(90, Math.round(rem)));
    state.maxWidth = clamped;
    writeStoredJSON(WIDTH_KEY, clamped);
  }
  function setScroll(chapterIdx: number, px: number) {
    if (px <= 0) {
      // Don't persist 0 — collapses the map. Treat top-of-chapter
      // as "no offset to restore."
      if (state.scrollByChapter[chapterIdx] != null) {
        const next = { ...state.scrollByChapter };
        delete next[chapterIdx];
        state.scrollByChapter = next;
        writeStoredJSON(SCROLL_KEY(opts.assetId), state.scrollByChapter);
      }
      return;
    }
    state.scrollByChapter = { ...state.scrollByChapter, [chapterIdx]: Math.round(px) };
    writeStoredJSON(SCROLL_KEY(opts.assetId), state.scrollByChapter);
  }

  async function runSearch(query: string) {
    const q = query.trim();
    state.searchQuery = q;
    if (q.length < 2) {
      state.searchResults = [];
      state.searchError = null;
      return;
    }
    state.searchBusy = true;
    state.searchError = null;
    try {
      const r = await fetch(
        `/api/v1/assets/${opts.assetId}/epub/search?q=${encodeURIComponent(q)}`,
        { credentials: 'include' },
      );
      if (!r.ok) throw new Error(`search HTTP ${r.status}`);
      const raw = (await r.json()) as Array<{
        chapter_idx: number;
        chapter_label: string;
        snippet: string;
        char_offset: number;
      }>;
      state.searchResults = raw.map((h) => ({
        chapterIdx: h.chapter_idx,
        chapterLabel: h.chapter_label,
        snippet: h.snippet,
        charOffset: h.char_offset,
      }));
    } catch (e) {
      state.searchError = e instanceof Error ? e.message : String(e);
      state.searchResults = [];
    } finally {
      state.searchBusy = false;
    }
  }
  function clearSearch() {
    state.searchQuery = '';
    state.searchResults = [];
    state.searchError = null;
  }

  return Object.assign(state as EbookSessionInstance, {
    assetId: opts.assetId,
    loadSpine,
    goTo,
    goNext,
    goPrev,
    addBookmark,
    removeBookmark,
    setFontSize,
    setTheme,
    setFontFamily,
    setMaxWidth,
    setScroll,
    runSearch,
    clearSearch,
  });
}
