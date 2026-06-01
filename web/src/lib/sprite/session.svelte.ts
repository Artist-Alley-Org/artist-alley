// SpriteSession — shared reactive state between SpriteCanvas (the
// view body that owns the canvas) and SpriteToolPanel (the right
// pane that owns the controls). AssetViewer instantiates one per
// sprite-mounted asset and threads it to both.
//
// Mirrors the WhiteboardSession pattern: object literal of $state
// fields + helper methods, returned from a factory. View body and
// tool panel both bind to the same instance, so when the panel
// tweaks cellW the canvas re-renders without an event bus.
//
// State is intentionally flat — no nested $state objects — because
// Svelte 5's proxy-based reactivity reads cleanest that way and
// the field count is small enough that flatness isn't a problem.

export interface SpriteFrameRect {
  name: string;
  sx: number; sy: number; sw: number; sh: number;
  /** Per-frame ms from the source format (Aseprite). Optional. */
  duration?: number;
}

export interface SpriteTagRange {
  name: string;
  from: number; to: number;
  direction?: 'forward' | 'reverse' | 'pingpong';
}

export interface SpriteCompanion {
  id: string;
  asset_id: string;
  path: string;
  content_type: string;
  size_bytes: number;
}

export type LoopMode = 'forward' | 'pingpong';
export type BgMode = 'checker' | 'transparent' | 'solid';

export interface SpriteSession {
  // ── Image (owner: canvas mounts the Image, writes back here) ──
  img: HTMLImageElement | null;
  imgW: number;
  imgH: number;

  // ── Manual slicer ────────────────────────────────────────────
  cellW: number;
  cellH: number;
  padX: number;
  padY: number;
  originX: number;
  originY: number;
  frameCountOverride: number | null;
  showGrid: boolean;

  // ── Metadata (from a companion JSON sidecar) ─────────────────
  metadataFrames: SpriteFrameRect[] | null;
  metadataTags: SpriteTagRange[];
  metadataCompanion: SpriteCompanion | null;
  metadataError: string | null;
  metadataLoading: boolean;
  activeTag: string | null;

  // ── Playback ─────────────────────────────────────────────────
  playing: boolean;
  currentFrame: number;
  fps: number;
  loopMode: LoopMode;
  /** Pingpong direction: 1 = forward, -1 = backward. Internal to
   *  the canvas's tick loop, kept on the session so a pause+resume
   *  doesn't reset the swing. */
  pingDir: number;

  // ── Display ──────────────────────────────────────────────────
  zoom: number;
  bg: BgMode;
  bgSolid: string;
  /** false = pixel-perfect (default), true = bilinear. */
  smoothing: boolean;
}

/** Asset id this session was created for. Lets the panel call the
 *  companion endpoints without threading the id separately. */
export interface SpriteSessionOpts { assetId: string; }

/** A namespace of helpers bound to a session instance — so panel
 *  buttons can call sess.detachMetadata() instead of importing the
 *  function + passing the session through. */
export interface SpriteSessionMethods {
  loadCompanions(): Promise<void>;
  detachMetadata(): Promise<void>;
  uploadMetadataFile(file: File): Promise<void>;
  pickFitZoom(): void;
  stepFrame(): void;
}

export type SpriteSessionInstance = SpriteSession & SpriteSessionMethods & { assetId: string };

export function createSpriteSession(opts: SpriteSessionOpts): SpriteSessionInstance {
  // Svelte 5's $state already wraps the object in a reactive proxy.
  // We bind helper methods directly onto the same object so callers
  // can do `session.detachMetadata()` and `session.metadataTags.length`
  // without an outer Proxy that loses Svelte's reactivity hooks.
  const state = $state<SpriteSession>({
    img: null,
    imgW: 0,
    imgH: 0,

    cellW: 0,
    cellH: 0,
    padX: 0,
    padY: 0,
    originX: 0,
    originY: 0,
    frameCountOverride: null,
    showGrid: false,

    metadataFrames: null,
    metadataTags: [],
    metadataCompanion: null,
    metadataError: null,
    metadataLoading: false,
    activeTag: null,

    playing: true,
    currentFrame: 0,
    fps: 10,
    loopMode: 'forward',
    pingDir: 1,

    zoom: 2,
    bg: 'checker',
    bgSolid: '#1a1a1a',
    smoothing: false,
  });

  // ── Companion / metadata helpers ─────────────────────────────
  // The natural-sort comparator keeps Hash-form keys in file-name
  // numeric order so "walk 10" lands after "walk 9".
  function naturalCompare(a: string, b: string): number {
    return a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' });
  }

  function pickMetadataCompanion(list: SpriteCompanion[]): SpriteCompanion | null {
    for (const c of list) {
      const p = c.path.toLowerCase();
      if (c.content_type.startsWith('application/json')) return c;
      if (p.endsWith('.json') || p.endsWith('.atlas')) return c;
    }
    return null;
  }

  function parseSpriteJSON(text: string): { frames: SpriteFrameRect[]; tags: SpriteTagRange[] } | null {
    let data: unknown;
    try { data = JSON.parse(text); }
    catch (e) {
      state.metadataError = 'Companion JSON failed to parse: ' + (e instanceof Error ? e.message : String(e));
      return null;
    }
    if (!data || typeof data !== 'object') {
      state.metadataError = 'Companion JSON is not an object.';
      return null;
    }
    const obj = data as Record<string, unknown>;
    const rawFrames = obj.frames;
    const out: SpriteFrameRect[] = [];
    if (Array.isArray(rawFrames)) {
      // Array form — JSON order is authoritative.
      for (const entry of rawFrames) {
        const ef = entry as Record<string, unknown>;
        const fr = ef.frame as { x?: number; y?: number; w?: number; h?: number } | undefined;
        if (!fr || typeof fr.x !== 'number') continue;
        out.push({
          name: String(ef.filename ?? ef.name ?? out.length),
          sx: fr.x, sy: fr.y ?? 0, sw: fr.w ?? 0, sh: fr.h ?? 0,
          duration: typeof ef.duration === 'number' ? ef.duration : undefined,
        });
      }
    } else if (rawFrames && typeof rawFrames === 'object') {
      const entries = Object.entries(rawFrames as Record<string, unknown>);
      entries.sort((a, b) => naturalCompare(a[0], b[0]));
      for (const [name, entry] of entries) {
        const ef = entry as Record<string, unknown>;
        const fr = ef.frame as { x?: number; y?: number; w?: number; h?: number } | undefined;
        if (!fr || typeof fr.x !== 'number') continue;
        out.push({
          name,
          sx: fr.x, sy: fr.y ?? 0, sw: fr.w ?? 0, sh: fr.h ?? 0,
          duration: typeof ef.duration === 'number' ? ef.duration : undefined,
        });
      }
    } else {
      state.metadataError = 'Companion JSON has no `frames` field; not a sprite atlas.';
      return null;
    }
    if (out.length === 0) {
      state.metadataError = 'Companion JSON had no valid frame rects.';
      return null;
    }
    const meta = (obj.meta ?? {}) as Record<string, unknown>;
    const rawTags = (meta.frameTags ?? []) as unknown[];
    const tags: SpriteTagRange[] = [];
    for (const t of rawTags) {
      const tg = t as Record<string, unknown>;
      if (typeof tg.from === 'number' && typeof tg.to === 'number') {
        tags.push({
          name: String(tg.name ?? `tag ${tags.length + 1}`),
          from: tg.from,
          to: tg.to,
          direction: tg.direction === 'reverse' || tg.direction === 'pingpong' ? tg.direction : 'forward',
        });
      }
    }
    return { frames: out, tags };
  }

  async function loadCompanions() {
    state.metadataError = null;
    try {
      const r = await fetch(`/api/v1/assets/${opts.assetId}/companions`, { credentials: 'include' });
      if (!r.ok) return;
      const list = (await r.json()) as SpriteCompanion[];
      const meta = pickMetadataCompanion(list);
      if (!meta) return;
      const rr = await fetch(`/api/v1/assets/${opts.assetId}/companions/${meta.id}`, { credentials: 'include' });
      if (!rr.ok) {
        state.metadataError = `Companion fetch failed: HTTP ${rr.status}`;
        return;
      }
      const text = await rr.text();
      const parsed = parseSpriteJSON(text);
      if (parsed) {
        state.metadataCompanion = meta;
        state.metadataFrames = parsed.frames;
        state.metadataTags = parsed.tags;
        const d = parsed.frames[0]?.duration;
        if (d && d > 0) state.fps = Math.max(0.5, Math.min(60, 1000 / d));
      }
    } catch (e) {
      state.metadataError = 'Companion load error: ' + (e instanceof Error ? e.message : String(e));
    }
  }

  async function detachMetadata() {
    if (!state.metadataCompanion) return;
    state.metadataLoading = true;
    try {
      await fetch(`/api/v1/assets/${opts.assetId}/companions/${state.metadataCompanion.id}`, {
        method: 'DELETE',
        credentials: 'include',
      });
    } finally {
      state.metadataCompanion = null;
      state.metadataFrames = null;
      state.metadataTags = [];
      state.activeTag = null;
      state.metadataLoading = false;
    }
  }

  async function uploadMetadataFile(file: File) {
    state.metadataLoading = true;
    state.metadataError = null;
    try {
      const r = await fetch(`/api/v1/assets/${opts.assetId}/companions`, {
        method: 'POST',
        credentials: 'include',
        headers: {
          'Content-Type': 'application/octet-stream',
          'X-Companion-Path': file.name,
          'X-Content-Type': file.type || 'application/json',
        },
        body: file,
      });
      if (!r.ok) {
        const j = await r.json().catch(() => ({ error: `HTTP ${r.status}` }));
        state.metadataError = (j as { error?: string }).error ?? `Upload failed (HTTP ${r.status})`;
        return;
      }
      await loadCompanions();
    } catch (e) {
      state.metadataError = 'Upload error: ' + (e instanceof Error ? e.message : String(e));
    } finally {
      state.metadataLoading = false;
    }
  }

  function pickFitZoom() {
    if (state.imgW <= 0 || state.imgH <= 0) return;
    const budget = 640;
    const z = Math.max(1, Math.floor(Math.min(budget / state.imgW, budget / state.imgH)));
    state.zoom = Math.max(1, Math.min(32, z));
  }

  // Public read of derived counts. We don't expose these as
  // $derived from inside the factory because consumers expect a
  // plain object shape; instead the components recompute them
  // (cheap — just integer math).
  function stepFrame() {
    const frames = state.metadataFrames;
    let from = 0;
    let to: number;
    if (frames && state.activeTag) {
      const t = state.metadataTags.find((x) => x.name === state.activeTag);
      if (t) { from = t.from; to = t.to; }
      else to = frames.length - 1;
    } else if (frames) {
      to = frames.length - 1;
    } else {
      const cols = state.cellW > 0
        ? Math.max(1, Math.floor((state.imgW - state.originX + state.padX) / (state.cellW + state.padX)))
        : 1;
      const rows = state.cellH > 0
        ? Math.max(1, Math.floor((state.imgH - state.originY + state.padY) / (state.cellH + state.padY)))
        : 1;
      const total = state.frameCountOverride ?? cols * rows;
      to = Math.max(0, total - 1);
    }
    const len = to - from + 1;
    if (len <= 1) return;
    if (state.loopMode === 'pingpong') {
      let next = state.currentFrame + state.pingDir;
      if (next >= len) {
        state.pingDir = -1;
        next = Math.max(0, len - 2);
      } else if (next < 0) {
        state.pingDir = 1;
        next = Math.min(len - 1, 1);
      }
      state.currentFrame = next;
    } else {
      state.currentFrame = (state.currentFrame + 1) % len;
    }
  }

  // Attach helpers + the constant asset id directly onto the
  // $state proxy. Object.assign uses default property descriptors
  // (writable + configurable + enumerable) which Svelte 5's $state
  // proxy requires — defineProperty with non-default flags trips
  // state_descriptors_fixed.
  return Object.assign(state as SpriteSessionInstance, {
    assetId: opts.assetId,
    loadCompanions,
    detachMetadata,
    uploadMetadataFile,
    pickFitZoom,
    stepFrame,
  });
}
