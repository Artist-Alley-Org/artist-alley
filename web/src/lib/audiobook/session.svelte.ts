// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// AudiobookSession — shared reactive state between AudiobookView
// (the canvas-area player) and AudiobookTool (the side-panel
// toolbox). Mirrors the Ebook / Doc / Model session pattern:
// object-literal $state holding every field both ends touch, plus
// helper methods bound on directly.
//
// Persistence split:
//   * Per-asset (localStorage):
//       resumePos     — last playback time (seconds) so re-opening
//                       picks up where the user left off
//       bookmarks     — time-anchored markers with optional notes
//   * Per-tab (localStorage, global key):
//       playback speed, skip-back / skip-fwd intervals, theme,
//       auto-rewind seconds. Audiobook listeners use the same speed
//       across every book they reach for so a global pref makes
//       sense.
//
// Viewer-populated (not persisted): chapters list, current time +
// duration (mirrored from controller every render frame), current
// chapter index (derived), cover URL.

export interface AudiobookChapter {
  id: number;
  /** Start time in seconds. */
  start: number;
  /** End time in seconds (or duration when the container puts it
   *  at the very end). */
  end: number;
  /** Display label — "Chapter 1" / "Opening Credits" / etc.
   *  Falls back to "Chapter N" client-side when blank. */
  title: string;
}

/** Album / series projection from a .nfo companion (Kodi-style).
 *  Surfaces in the "Album" panel section + the now-playing strip
 *  ("Track 3 of 6 · The Dark Tower V · by Stephen King"). */
export interface AudiobookAlbum {
  title: string;
  artist: string;
  albumArtist: string;
  genre: string;
  year: string;
  summary: string;
  runtimeS: number;
  mbAlbumId: string;
  tracks: AudiobookAlbumTrack[];
}
export interface AudiobookAlbumTrack {
  position: number;
  title: string;
  durationS: number;
}

/** Sibling track in a multi-file audiobook. Populated by the host
 *  (PostHost) when the asset lives inside a post with multiple
 *  audio members — the view treats the post as a playlist and
 *  auto-advances at end-of-track. */
export interface AudiobookSibling {
  /** Asset id of the sibling track. */
  assetId: string;
  /** Display title. */
  title: string;
  /** 1-based index in the playlist (mirrors the post.members
   *  sort_order). */
  position: number;
}

export interface AudiobookBookmark {
  /** Time position in seconds. */
  time: number;
  /** Chapter id at the position (for display, not navigation). */
  chapterId: number | null;
  /** Optional user note. */
  note: string;
  /** ISO timestamp. */
  createdAt: string;
}

export type SleepTimerMode =
  | 'off'
  | '5min'
  | '15min'
  | '30min'
  | '45min'
  | '60min'
  | 'end-of-chapter';

export interface AudiobookSession {
  // ── Source (populated by the view on mount) ─────────────────
  chapters: AudiobookChapter[];
  /** Where the chapter list came from — "container" (m4b atom
   *  table), "cue" (parsed companion .cue), "album" (synthesised
   *  from a .nfo track listing for multi-file books), or empty
   *  when no chapters exist. Surfaced in the stats panel. */
  chapterSource: string;
  durationS: number;
  coverUrl: string | null;
  title: string;
  author: string;
  narrator: string;
  /** Album metadata folded from the .nfo companion. Null when the
   *  asset has no .nfo attached or the file failed to parse. */
  album: AudiobookAlbum | null;
  /** Sibling-track playlist when the asset lives inside a
   *  multi-member post (Dark Tower style: one MP3 per disk). The
   *  view's "Next track" button + auto-advance fire when this
   *  is populated and a sibling follows the current asset. Empty
   *  for single-file audiobooks. */
  siblings: AudiobookSibling[];
  /** Index of the currently-playing asset within `siblings`.
   *  -1 when there are no siblings. */
  currentSiblingIndex: number;
  /** When true, the view automatically loads the next sibling
   *  track on end-of-audio. Persisted globally so users keep the
   *  Audible-like "continuous play" behaviour across books. */
  autoAdvance: boolean;

  // ── Live playback state (mirrored from <audio>) ─────────────
  currentTime: number;
  playing: boolean;
  loading: boolean;
  loadError: string | null;

  // ── Reading prefs (per-tab global) ──────────────────────────
  /** Playback speed (1.0 = normal). Audiobookshelf-style range
   *  goes 0.5 → 5.0; we cap a bit lower since the browser's
   *  preservesPitch struggles past 4×. */
  speed: number;
  /** Auto-rewind on resume — when the user pauses and comes back,
   *  jump this many seconds back before resuming. Industry
   *  default is 5–10s; 0 disables. */
  autoRewindS: number;
  /** Skip-back button delta (seconds). Audiobookshelf default 10. */
  skipBackS: number;
  /** Skip-forward button delta. Audiobookshelf default 30. */
  skipFwdS: number;
  /** Sleep timer mode. 'end-of-chapter' fires at the next chapter
   *  boundary; numeric modes fire after the wall-clock minutes
   *  elapse. */
  sleepTimer: SleepTimerMode;
  /** Seconds remaining on the active sleep timer — the view ticks
   *  this down from a setTimeout while playing. null when no timer
   *  is running. */
  sleepRemaining: number | null;

  // ── Per-asset state ─────────────────────────────────────────
  /** Last persisted resume position. Different from currentTime —
   *  resumePos updates on a 5s throttle while currentTime mirrors
   *  every frame. */
  resumePos: number;
  bookmarks: AudiobookBookmark[];
}

export interface AudiobookSessionMethods {
  // Reading prefs
  setSpeed(v: number): void;
  toggleAutoAdvance(): void;
  setAutoRewindS(v: number): void;
  setSkipBackS(v: number): void;
  setSkipFwdS(v: number): void;
  setSleepTimer(m: SleepTimerMode): void;
  cancelSleepTimer(): void;

  // Bookmarks
  addBookmark(note?: string): void;
  removeBookmark(time: number, createdAt: string): void;
  setCurrentBookmarkNote(time: number, createdAt: string, note: string): void;

  // Navigation (viewer-published callbacks — set on mount)
  /** Seek to a time in seconds (the view assigns this when its
   *  <audio> element mounts). */
  seekTo?: (seconds: number) => void;
  /** Toggle play/pause. */
  togglePlay?: () => void;
  /** Skip relative — positive forward, negative back. */
  skipRelative?: (deltaS: number) => void;
  /** Jump to a specific chapter by index. */
  goToChapter?: (idx: number) => void;
  /** Host-published navigation: jump to a sibling asset (multi-
   *  file audiobook). The PostHost asset playlist takes over and
   *  swaps AssetViewer's asset prop, which rebuilds this session
   *  for the new track. */
  goToSibling?: (assetId: string) => void;
}

export type AudiobookSessionInstance =
  AudiobookSession & AudiobookSessionMethods & { assetId: string };

export interface AudiobookSessionOpts { assetId: string; }

// localStorage keys
const RESUME_KEY    = (id: string) => `aa.audiobook.${id}.resume`;
const BOOKMARKS_KEY = (id: string) => `aa.audiobook.${id}.bookmarks`;

const G_SPEED         = 'aa.audiobook.speed';
const G_AUTO_REW      = 'aa.audiobook.autoRewindS';
const G_SKIP_BACK     = 'aa.audiobook.skipBackS';
const G_SKIP_FWD      = 'aa.audiobook.skipFwdS';
const G_AUTO_ADVANCE  = 'aa.audiobook.autoAdvance';

const DEFAULTS = {
  speed: 1.0,
  autoRewindS: 5,
  skipBackS: 10,
  skipFwdS: 30,
  autoAdvance: true,
};

function readLS<T>(key: string, fallback: T): T {
  try {
    const v = localStorage.getItem(key);
    if (v == null) return fallback;
    return JSON.parse(v) as T;
  } catch {
    return fallback;
  }
}
function writeLS(key: string, value: unknown): void {
  try { localStorage.setItem(key, JSON.stringify(value)); } catch { /* ignore */ }
}

/** Format seconds → H:MM:SS or M:SS for display. */
export function fmtClock(s: number): string {
  if (!Number.isFinite(s) || s < 0) return '0:00';
  const total = Math.floor(s);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const sec = total % 60;
  const mPad = h > 0 ? String(m).padStart(2, '0') : String(m);
  const sPad = String(sec).padStart(2, '0');
  return h > 0 ? `${h}:${mPad}:${sPad}` : `${mPad}:${sPad}`;
}

/** Format seconds → "3h 45m" / "12m 5s" for longer durations. */
export function fmtSpan(s: number): string {
  if (!Number.isFinite(s) || s <= 0) return '0s';
  const total = Math.floor(s);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const sec = total % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${sec}s`;
  return `${sec}s`;
}

export function createAudiobookSession(opts: AudiobookSessionOpts): AudiobookSessionInstance {
  const state = $state<AudiobookSession>({
    chapters: [],
    chapterSource: '',
    durationS: 0,
    coverUrl: null,
    title: '',
    author: '',
    narrator: '',
    album: null,
    siblings: [],
    currentSiblingIndex: -1,
    autoAdvance: readLS<boolean>(G_AUTO_ADVANCE, DEFAULTS.autoAdvance),

    currentTime: 0,
    playing: false,
    loading: true,
    loadError: null,

    speed: readLS<number>(G_SPEED, DEFAULTS.speed),
    autoRewindS: readLS<number>(G_AUTO_REW, DEFAULTS.autoRewindS),
    skipBackS: readLS<number>(G_SKIP_BACK, DEFAULTS.skipBackS),
    skipFwdS: readLS<number>(G_SKIP_FWD, DEFAULTS.skipFwdS),
    sleepTimer: 'off',
    sleepRemaining: null,

    resumePos: readLS<number>(RESUME_KEY(opts.assetId), 0),
    bookmarks: readLS<AudiobookBookmark[]>(BOOKMARKS_KEY(opts.assetId), []),
  });

  function setSpeed(v: number) {
    const c = Math.max(0.5, Math.min(4.0, Math.round(v * 100) / 100));
    state.speed = c;
    writeLS(G_SPEED, c);
  }
  function toggleAutoAdvance() {
    state.autoAdvance = !state.autoAdvance;
    writeLS(G_AUTO_ADVANCE, state.autoAdvance);
  }
  function setAutoRewindS(v: number) {
    const c = Math.max(0, Math.min(30, Math.round(v)));
    state.autoRewindS = c;
    writeLS(G_AUTO_REW, c);
  }
  function setSkipBackS(v: number) {
    const c = Math.max(5, Math.min(60, Math.round(v)));
    state.skipBackS = c;
    writeLS(G_SKIP_BACK, c);
  }
  function setSkipFwdS(v: number) {
    const c = Math.max(5, Math.min(120, Math.round(v)));
    state.skipFwdS = c;
    writeLS(G_SKIP_FWD, c);
  }
  function setSleepTimer(m: SleepTimerMode) {
    state.sleepTimer = m;
    if (m === 'off') state.sleepRemaining = null;
    else if (m === 'end-of-chapter') state.sleepRemaining = null;
    else {
      const mins = m === '5min' ? 5 : m === '15min' ? 15 : m === '30min' ? 30 : m === '45min' ? 45 : 60;
      state.sleepRemaining = mins * 60;
    }
  }
  function cancelSleepTimer() {
    state.sleepTimer = 'off';
    state.sleepRemaining = null;
  }

  function chapterAtTime(t: number): AudiobookChapter | null {
    for (const c of state.chapters) {
      if (t >= c.start && t < c.end) return c;
    }
    return state.chapters[state.chapters.length - 1] ?? null;
  }

  function addBookmark(note: string = '') {
    const t = state.currentTime;
    const ch = chapterAtTime(t);
    const bm: AudiobookBookmark = {
      time: Math.round(t * 100) / 100,
      chapterId: ch?.id ?? null,
      note: note.trim(),
      createdAt: new Date().toISOString(),
    };
    state.bookmarks = [...state.bookmarks, bm].sort((a, b) => a.time - b.time);
    writeLS(BOOKMARKS_KEY(opts.assetId), state.bookmarks);
  }
  function removeBookmark(time: number, createdAt: string) {
    state.bookmarks = state.bookmarks.filter(
      (b) => !(b.time === time && b.createdAt === createdAt),
    );
    writeLS(BOOKMARKS_KEY(opts.assetId), state.bookmarks);
  }
  function setCurrentBookmarkNote(time: number, createdAt: string, note: string) {
    state.bookmarks = state.bookmarks.map((b) =>
      (b.time === time && b.createdAt === createdAt) ? { ...b, note } : b,
    );
    writeLS(BOOKMARKS_KEY(opts.assetId), state.bookmarks);
  }

  return Object.assign(state as AudiobookSessionInstance, {
    assetId: opts.assetId,
    setSpeed, setAutoRewindS, setSkipBackS, setSkipFwdS,
    setSleepTimer, cancelSleepTimer, toggleAutoAdvance,
    addBookmark, removeBookmark, setCurrentBookmarkNote,
  });
}

/** Persist helper for the view to call on a 5s throttle as
 *  playback advances — keeps `resumePos` fresh without writing
 *  every frame. */
export function persistAudiobookResume(assetId: string, seconds: number) {
  writeLS(RESUME_KEY(assetId), Math.max(0, Math.round(seconds * 100) / 100));
}
