// DocSession — shared reactive state between DocView (the canvas-
// area CodeMirror renderer) and DocTool (the side-panel toolbox).
// Mirrors the EbookSession / SpriteSession / ModelSession pattern:
// object-literal $state holding every field both sides touch, plus
// helper methods bound on directly. Both ends bind the same instance
// so changing font-size in the panel updates the editor without an
// event bus.
//
// Persistence split:
//   * Per-asset (localStorage):
//       bookmarks       — line + optional note, user-local
//       scrollLine      — last viewed line (for resume-where-I-was)
//   * Per-tab (localStorage, global key):
//       reading prefs the user wants applied to every doc:
//       theme, font family, font size, line height, wrap, line
//       numbers, tab width, render-as-markdown (when applicable).
//
// Viewer-populated, not persisted:
//   languageId, outline, stats (lines/words/chars). The viewer
//   pushes these on load + on content change so the panel can
//   render them; the panel never writes them back.
//
// Annotations land in Phase B — backend table + REST + decorations
// (highlight / strikethrough / underline / comment / note). The
// fields are sketched here so the contract stays stable; the panel
// renders the section as "coming soon" until B ships.

export type DocTheme = 'light' | 'sepia' | 'dark';
export type DocFontFamily = 'sans' | 'serif' | 'mono';

export interface DocBookmark {
  /** 1-based line number the user was on. */
  line: number;
  /** Optional user-typed note. */
  note: string;
  /** ISO timestamp. */
  createdAt: string;
}

export interface DocOutlineEntry {
  /** Display label — heading text for markdown, symbol for code. */
  label: string;
  /** 1-based line to jump to. */
  line: number;
  /** Depth (0–6); drives left-pad in the panel. */
  depth: number;
}

export interface DocStats {
  lines: number;
  words: number;
  characters: number;
  /** Bytes of the source file. */
  fileSize: number | null;
}

/** Text-range annotation — Phase B. Mirrors the backend
 *  `comments` row with `annotation_type='text-range'`; the visual
 *  style + color + anchor + resolved flag live in `annotation_data`
 *  on the wire and as direct fields here for ergonomic access. */
export interface DocAnnotation {
  id: string;
  style: 'highlight' | 'strikethrough' | 'underline' | 'comment' | 'note';
  color: string;
  startLine: number;
  startCol: number;
  endLine: number;
  endCol: number;
  resolved: boolean;
  body: string;
  authorRef: number | null;
  createdAt: string;
  updatedAt: string;
}

/** Selection the editor reports to the panel when the user
 *  highlights a range. Used by the floating selection toolbar +
 *  the side-panel "Annotate selection" button. */
export interface DocSelection {
  startLine: number;
  startCol: number;
  endLine: number;
  endCol: number;
  /** True when start === end (no characters selected). The toolbar
   *  uses this to gate which annotation styles make sense — a
   *  zero-width range can only carry a sticky note. */
  empty: boolean;
}

export interface DocSession {
  // ── Reading settings (global per-tab) ────────────────────────
  theme: DocTheme;
  fontFamily: DocFontFamily;
  /** CSS px. Default 14 (smaller than ebook since code reads
   *  better dense). Range 10–24. */
  fontSize: number;
  /** Line-height multiplier. Range 1.0–2.2. */
  lineHeight: number;
  /** Soft-wrap long lines. */
  wrap: boolean;
  /** Render the gutter with line numbers. */
  lineNumbers: boolean;
  /** Tab width in columns. 2 / 4 / 8 are the common picks. */
  tabSize: number;
  /** Show whitespace markers (· for space, → for tab). */
  showWhitespace: boolean;
  /** When the file is markdown, render a preview pane instead of
   *  the source. Toggled from the panel; the view body owns the
   *  rendering. Ignored for non-markdown. */
  renderMarkdown: boolean;

  // ── Viewer-populated (read-only from panel) ──────────────────
  /** CodeMirror language id detected from the file extension —
   *  the panel reads this for the Stats badge. */
  languageId: string;
  /** Hierarchical outline — markdown headings or code symbols. */
  outline: DocOutlineEntry[];
  stats: DocStats | null;
  loading: boolean;
  loadError: string | null;
  /** Last line the viewer scrolled to, mirrored back so the
   *  bookmark "Add at current line" knows where the user is.
   *  Updated on each scroll-stop (throttled). */
  currentLine: number;

  // ── Bookmarks (per-asset, user-local) ────────────────────────
  bookmarks: DocBookmark[];

  // ── Search ───────────────────────────────────────────────────
  /** The user's last query — surfaced in the panel so the user
   *  can re-run it without re-typing. The actual search runs in
   *  CodeMirror's search extension; the panel reads/writes this
   *  field and the view body bridges it into the editor. */
  searchQuery: string;
  /** Case-sensitive flag — fed into CodeMirror's SearchQuery. */
  searchCaseSensitive: boolean;
  /** Regex flag. */
  searchRegex: boolean;
  /** Whole-word match. */
  searchWholeWord: boolean;
  /** Replace-with field — only used when the user opts in via
   *  the Find panel's Replace toggle. */
  replaceWith: string;
  /** Total matches the editor reports for the current query. */
  searchMatchCount: number;

  // ── Annotations (Phase B) ────────────────────────────────────
  /** Live annotation list — fetched on mount + mutated by the
   *  selection toolbar / panel. The viewer's decoration extension
   *  re-reads on every change and re-paints the editor. */
  annotations: DocAnnotation[];
  /** True while the initial GET is in flight. */
  annotationsLoading: boolean;
  annotationsError: string | null;
  /** Last error from a write (create/update/delete). Cleared on
   *  the next successful write. */
  annotationsWriteError: string | null;
  /** Current editor selection — pushed by DocView on selectionSet.
   *  null when nothing is selected. Drives the floating toolbar. */
  selection: DocSelection | null;
  /** Pixel coordinates the selection toolbar should anchor to.
   *  Viewport-relative (clientX/clientY) so the panel doesn't have
   *  to know about CodeMirror's coordinate system. */
  selectionAnchor: { x: number; y: number } | null;
  /** Panel filter — which annotation kinds to show. `null` = all. */
  annotationsFilter: DocAnnotation['style'] | null;
  /** Hide resolved entries by default; user can flip the toggle. */
  annotationsShowResolved: boolean;
}

export interface DocSessionOpts { assetId: string; }

export interface DocSessionMethods {
  // Reading prefs
  setTheme(t: DocTheme): void;
  setFontFamily(f: DocFontFamily): void;
  setFontSize(px: number): void;
  setLineHeight(v: number): void;
  toggleWrap(): void;
  toggleLineNumbers(): void;
  setTabSize(n: number): void;
  toggleShowWhitespace(): void;
  toggleRenderMarkdown(): void;

  // Bookmarks
  addBookmark(note?: string): void;
  removeBookmark(line: number, createdAt: string): void;

  // Navigation
  /** Panel-emitted jump request. The viewer's $effect on
   *  jumpTrigger watches the (line, counter) tuple and animates
   *  CodeMirror to that line. */
  goToLine(line: number): void;

  // Search
  setSearchQuery(q: string): void;
  setSearchCaseSensitive(v: boolean): void;
  setSearchRegex(v: boolean): void;
  setSearchWholeWord(v: boolean): void;
  setReplaceWith(s: string): void;
  /** Trigger "next match" / "prev match" — same trigger-counter
   *  idiom used by goToLine. The viewer handles the actual seek. */
  findNext(): void;
  findPrev(): void;
  /** Replace just the current match. */
  replaceCurrent(): void;
  /** Replace every match for the current query. */
  replaceAll(): void;

  // Annotations (Phase B)
  /** Initial fetch — called once after mount. Idempotent. */
  loadAnnotations(): Promise<void>;
  /** Persist a new annotation. Optimistically inserts into
   *  `annotations` on success. */
  createAnnotation(input: {
    style: DocAnnotation['style'];
    color: string;
    body?: string;
    anchor: { startLine: number; startCol: number; endLine: number; endCol: number };
  }): Promise<DocAnnotation | null>;
  /** Patch body / style / color / range / resolved. The patch is
   *  shallow-merged with the existing annotation before the
   *  PATCH /text-annotations/{id} call. */
  updateAnnotation(id: string, patch: Partial<Pick<DocAnnotation,
    'body' | 'style' | 'color' | 'startLine' | 'startCol' | 'endLine' | 'endCol' | 'resolved'>>): Promise<void>;
  /** DELETE /comments/{id} — annotations are comments under the hood. */
  deleteAnnotation(id: string): Promise<void>;
  /** Push selection state from the viewer to the panel + toolbar. */
  setSelection(sel: DocSelection | null, anchor?: { x: number; y: number } | null): void;
  setAnnotationsFilter(f: DocAnnotation['style'] | null): void;
  toggleAnnotationsShowResolved(): void;
}

export type DocSessionInstance =
  DocSession & DocSessionMethods & { assetId: string };

const BOOKMARKS_KEY = (id: string) => `aa.doc.${id}.bookmarks`;
const POS_KEY       = (id: string) => `aa.doc.${id}.line`;

const G_THEME       = 'aa.doc.theme';
const G_FONT_FAM    = 'aa.doc.fontFamily';
const G_FONT_SIZE   = 'aa.doc.fontSize';
const G_LINE_HT     = 'aa.doc.lineHeight';
const G_WRAP        = 'aa.doc.wrap';
const G_LINE_NUMS   = 'aa.doc.lineNumbers';
const G_TAB_SIZE    = 'aa.doc.tabSize';
const G_WHITESPACE  = 'aa.doc.showWhitespace';
const G_RENDER_MD   = 'aa.doc.renderMarkdown';

const DEFAULTS = {
  theme: 'dark' as DocTheme,
  fontFamily: 'mono' as DocFontFamily,
  fontSize: 14,
  lineHeight: 1.5,
  wrap: true,
  lineNumbers: true,
  tabSize: 4,
  showWhitespace: false,
  renderMarkdown: false,
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

export function createDocSession(opts: DocSessionOpts): DocSessionInstance {
  const state = $state<DocSession>({
    theme: readLS<DocTheme>(G_THEME, DEFAULTS.theme),
    fontFamily: readLS<DocFontFamily>(G_FONT_FAM, DEFAULTS.fontFamily),
    fontSize: readLS<number>(G_FONT_SIZE, DEFAULTS.fontSize),
    lineHeight: readLS<number>(G_LINE_HT, DEFAULTS.lineHeight),
    wrap: readLS<boolean>(G_WRAP, DEFAULTS.wrap),
    lineNumbers: readLS<boolean>(G_LINE_NUMS, DEFAULTS.lineNumbers),
    tabSize: readLS<number>(G_TAB_SIZE, DEFAULTS.tabSize),
    showWhitespace: readLS<boolean>(G_WHITESPACE, DEFAULTS.showWhitespace),
    renderMarkdown: readLS<boolean>(G_RENDER_MD, DEFAULTS.renderMarkdown),

    languageId: 'plain',
    outline: [],
    stats: null,
    loading: true,
    loadError: null,
    currentLine: readLS<number>(POS_KEY(opts.assetId), 1),

    bookmarks: readLS<DocBookmark[]>(BOOKMARKS_KEY(opts.assetId), []),

    searchQuery: '',
    searchCaseSensitive: false,
    searchRegex: false,
    searchWholeWord: false,
    replaceWith: '',
    searchMatchCount: 0,

    annotations: [],
    annotationsLoading: false,
    annotationsError: null,
    annotationsWriteError: null,
    selection: null,
    selectionAnchor: null,
    annotationsFilter: null,
    annotationsShowResolved: false,
  });

  // Trigger counters — the panel calls a method, the counter
  // increments, the viewer's $effect on the counter responds. Same
  // pattern the ModelSession uses for Frame all / Reset.
  const triggers = $state({
    jumpLine: 0,
    findNext: 0,
    findPrev: 0,
    replaceCurrent: 0,
    replaceAll: 0,
  });
  // Sticky-on-jump value the viewer reads after the trigger fires.
  let pendingJumpLine = 1;

  // ─── Reading prefs ───────────────────────────────────────────
  function setTheme(t: DocTheme) { state.theme = t; writeLS(G_THEME, t); }
  function setFontFamily(f: DocFontFamily) { state.fontFamily = f; writeLS(G_FONT_FAM, f); }
  function setFontSize(px: number) {
    const c = Math.max(10, Math.min(24, Math.round(px)));
    state.fontSize = c; writeLS(G_FONT_SIZE, c);
  }
  function setLineHeight(v: number) {
    const c = Math.max(1.0, Math.min(2.2, Math.round(v * 10) / 10));
    state.lineHeight = c; writeLS(G_LINE_HT, c);
  }
  function toggleWrap() { state.wrap = !state.wrap; writeLS(G_WRAP, state.wrap); }
  function toggleLineNumbers() { state.lineNumbers = !state.lineNumbers; writeLS(G_LINE_NUMS, state.lineNumbers); }
  function setTabSize(n: number) {
    const c = Math.max(1, Math.min(8, Math.round(n)));
    state.tabSize = c; writeLS(G_TAB_SIZE, c);
  }
  function toggleShowWhitespace() { state.showWhitespace = !state.showWhitespace; writeLS(G_WHITESPACE, state.showWhitespace); }
  function toggleRenderMarkdown() { state.renderMarkdown = !state.renderMarkdown; writeLS(G_RENDER_MD, state.renderMarkdown); }

  // ─── Bookmarks ───────────────────────────────────────────────
  function addBookmark(note: string = '') {
    const bm: DocBookmark = {
      line: state.currentLine || 1,
      note: note.trim(),
      createdAt: new Date().toISOString(),
    };
    state.bookmarks = [...state.bookmarks, bm];
    writeLS(BOOKMARKS_KEY(opts.assetId), state.bookmarks);
  }
  function removeBookmark(line: number, createdAt: string) {
    state.bookmarks = state.bookmarks.filter(
      (b) => !(b.line === line && b.createdAt === createdAt),
    );
    writeLS(BOOKMARKS_KEY(opts.assetId), state.bookmarks);
  }

  // ─── Navigation ──────────────────────────────────────────────
  function goToLine(line: number) {
    pendingJumpLine = Math.max(1, Math.floor(line));
    triggers.jumpLine++;
  }

  // ─── Search ──────────────────────────────────────────────────
  function setSearchQuery(q: string) { state.searchQuery = q; }
  function setSearchCaseSensitive(v: boolean) { state.searchCaseSensitive = v; }
  function setSearchRegex(v: boolean) { state.searchRegex = v; }
  function setSearchWholeWord(v: boolean) { state.searchWholeWord = v; }
  function setReplaceWith(s: string) { state.replaceWith = s; }
  function findNext() { triggers.findNext++; }
  function findPrev() { triggers.findPrev++; }
  function replaceCurrent() { triggers.replaceCurrent++; }
  function replaceAll() { triggers.replaceAll++; }

  // ─── Annotations (Phase B) ───────────────────────────────────
  // Server returns the comments-shaped row; we flatten its
  // annotation_data blob onto the DocAnnotation shape for ergonomic
  // panel access. The reverse direction packs the same fields back
  // into the `anchor` payload the API expects.
  type CommentRow = {
    id: string;
    author_user_ref: number | null;
    body: string;
    annotation_data?: Record<string, unknown> | null;
    created_at: string;
    updated_at: string;
  };
  function rowToAnnotation(c: CommentRow): DocAnnotation | null {
    const a = c.annotation_data ?? {};
    const style = String(a.style ?? 'highlight') as DocAnnotation['style'];
    return {
      id: c.id,
      style,
      color: String(a.color ?? '#fef08a'),
      startLine: Number(a.start_line ?? 1),
      startCol: Number(a.start_col ?? 0),
      endLine: Number(a.end_line ?? 1),
      endCol: Number(a.end_col ?? 0),
      resolved: Boolean(a.resolved ?? false),
      body: c.body ?? '',
      authorRef: c.author_user_ref,
      createdAt: c.created_at,
      updatedAt: c.updated_at,
    };
  }
  function annotationToAnchor(a: {
    style: DocAnnotation['style']; color: string; startLine: number;
    startCol: number; endLine: number; endCol: number; resolved?: boolean;
  }) {
    return {
      style: a.style,
      color: a.color,
      start_line: a.startLine,
      start_col: a.startCol,
      end_line: a.endLine,
      end_col: a.endCol,
      resolved: a.resolved ?? false,
    };
  }

  async function loadAnnotations() {
    state.annotationsLoading = true;
    state.annotationsError = null;
    try {
      const r = await fetch(`/api/v1/assets/${opts.assetId}/text-annotations`, {
        credentials: 'include',
      });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const rows = (await r.json()) as CommentRow[];
      state.annotations = rows
        .map(rowToAnnotation)
        .filter((x): x is DocAnnotation => !!x);
    } catch (e) {
      state.annotationsError = e instanceof Error ? e.message : String(e);
    } finally {
      state.annotationsLoading = false;
    }
  }

  async function createAnnotation(input: {
    style: DocAnnotation['style'];
    color: string;
    body?: string;
    anchor: { startLine: number; startCol: number; endLine: number; endCol: number };
  }): Promise<DocAnnotation | null> {
    state.annotationsWriteError = null;
    try {
      const r = await fetch(`/api/v1/assets/${opts.assetId}/text-annotations`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          body: input.body ?? '',
          anchor: annotationToAnchor({
            style: input.style, color: input.color,
            startLine: input.anchor.startLine, startCol: input.anchor.startCol,
            endLine: input.anchor.endLine, endCol: input.anchor.endCol,
            resolved: false,
          }),
        }),
      });
      if (!r.ok) throw new Error(`HTTP ${r.status}: ${(await r.text()).slice(0, 200)}`);
      const row = (await r.json()) as CommentRow;
      const ann = rowToAnnotation(row);
      if (ann) state.annotations = [...state.annotations, ann];
      return ann;
    } catch (e) {
      state.annotationsWriteError = e instanceof Error ? e.message : String(e);
      return null;
    }
  }

  async function updateAnnotation(id: string, patch: Partial<Pick<DocAnnotation,
    'body' | 'style' | 'color' | 'startLine' | 'startCol' | 'endLine' | 'endCol' | 'resolved'>>) {
    state.annotationsWriteError = null;
    const existing = state.annotations.find((a) => a.id === id);
    if (!existing) return;
    const merged: DocAnnotation = { ...existing, ...patch };
    try {
      // Build the PATCH body. Only include `anchor` if any range /
      // style / color / resolved changed; only include `body` if the
      // commentary changed.
      const wantsAnchor =
        'style' in patch || 'color' in patch || 'startLine' in patch
        || 'startCol' in patch || 'endLine' in patch || 'endCol' in patch
        || 'resolved' in patch;
      const payload: { body?: string; anchor?: ReturnType<typeof annotationToAnchor> } = {};
      if ('body' in patch) payload.body = merged.body;
      if (wantsAnchor) payload.anchor = annotationToAnchor(merged);
      const r = await fetch(`/api/v1/text-annotations/${id}`, {
        method: 'PATCH',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (!r.ok) throw new Error(`HTTP ${r.status}: ${(await r.text()).slice(0, 200)}`);
      const row = (await r.json()) as CommentRow;
      const ann = rowToAnnotation(row);
      if (!ann) return;
      state.annotations = state.annotations.map((a) => (a.id === id ? ann : a));
    } catch (e) {
      state.annotationsWriteError = e instanceof Error ? e.message : String(e);
    }
  }

  async function deleteAnnotation(id: string) {
    state.annotationsWriteError = null;
    try {
      const r = await fetch(`/api/v1/comments/${id}`, {
        method: 'DELETE',
        credentials: 'include',
      });
      if (!r.ok && r.status !== 204) throw new Error(`HTTP ${r.status}`);
      state.annotations = state.annotations.filter((a) => a.id !== id);
    } catch (e) {
      state.annotationsWriteError = e instanceof Error ? e.message : String(e);
    }
  }

  function setSelection(sel: DocSelection | null, anchor?: { x: number; y: number } | null) {
    state.selection = sel;
    state.selectionAnchor = anchor ?? null;
  }
  function setAnnotationsFilter(f: DocAnnotation['style'] | null) { state.annotationsFilter = f; }
  function toggleAnnotationsShowResolved() { state.annotationsShowResolved = !state.annotationsShowResolved; }

  return Object.assign(state as DocSessionInstance, {
    assetId: opts.assetId,
    // Trigger getters exposed so view-body $effects can read them
    // as reactive deps without exposing the inner `triggers` map.
    get _jumpLineTrigger() { return triggers.jumpLine; },
    get _findNextTrigger() { return triggers.findNext; },
    get _findPrevTrigger() { return triggers.findPrev; },
    get _replaceCurrentTrigger() { return triggers.replaceCurrent; },
    get _replaceAllTrigger() { return triggers.replaceAll; },
    get _pendingJumpLine() { return pendingJumpLine; },
    setTheme, setFontFamily, setFontSize, setLineHeight,
    toggleWrap, toggleLineNumbers, setTabSize, toggleShowWhitespace,
    toggleRenderMarkdown,
    addBookmark, removeBookmark,
    goToLine,
    setSearchQuery, setSearchCaseSensitive, setSearchRegex,
    setSearchWholeWord, setReplaceWith,
    findNext, findPrev, replaceCurrent, replaceAll,
    loadAnnotations, createAnnotation, updateAnnotation, deleteAnnotation,
    setSelection, setAnnotationsFilter, toggleAnnotationsShowResolved,
  });
}

// Persist scroll position whenever the panel/view writes session
// .currentLine. Exposed as a free helper so the DocView's scroll
// listener doesn't have to import POS_KEY directly.
export function persistDocScroll(assetId: string, line: number) {
  writeLS(POS_KEY(assetId), Math.max(1, Math.floor(line)));
}
