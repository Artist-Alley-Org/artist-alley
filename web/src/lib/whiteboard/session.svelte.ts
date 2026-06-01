// Whiteboard session — shared reactive state between the canvas
// overlay and the tool panel.
//
// Before C-1.5 the Whiteboard.svelte component owned everything in
// one file (its own tools/colors/sizes state + the canvas + the
// toolbar). With the toolbar moving to the side panel — a sibling
// component, not a child — we need a single mutable source of truth
// both sides can bind to without prop-drilling every field.
//
// Svelte 5 $state inside a factory function does this perfectly: the
// returned object is a reactive proxy; mutating fields on it triggers
// every component that reads them. Lives in a .svelte.ts file because
// $state can't be used in plain .ts modules.

import type {
  BrushContent,
  BrushStyle,
  BrushTool,
  Item,
  Layer,
  ShapeTool,
  TextItem,
  Tool,
} from './types';
import {
  DEFAULT_FONT_FAMILY,
  PALETTE,
  SIZES,
  defaultOpacityFor,
  emptyDoc,
  itemBBox,
  newLayer,
  resolveConnectorEndpoint,
} from './types';

/** Snapshot of the whole session — used by undo/redo. */
interface SessionSnapshot {
  doc: BrushContent;
  activeLayerId: string | null;
}

/** Owner-visible session API. The canvas component binds to `doc`
 *  + `activeLayerId` + tool/color/size; the tool panel mutates them.
 *  Methods are the only way to commit history snapshots so the undo
 *  stack stays accurate. */
export interface WhiteboardSession {
  // Document state
  doc: BrushContent;
  activeLayerId: string | null;

  // Tool state (driven by the tool panel)
  tool: Tool;
  color: string;
  width: number;
  opacity: number;
  fillShapes: boolean;

  // History
  canUndo: boolean;
  canRedo: boolean;
  undo: () => void;
  redo: () => void;

  // Item / layer mutations — each one pushes a history snapshot so
  // undo rewinds to the previous state cleanly.
  addItem: (layerId: string, item: Item) => void;
  /** Replace an item in a layer (used by the text editor on commit
   *  and the future selection/transform tool). */
  replaceItem: (layerId: string, index: number, item: Item) => void;
  /** Remove items by index from a layer. */
  removeItems: (layerId: string, indexes: number[]) => void;

  // Selection — single primary item via `selection`; the lasso
  // tool (C-1.9) populates `extraSelected` with additional items
  // when the user multi-picks via Shift+click or lasso. Move /
  // delete / copy operate on the union (primary ∪ extra).
  selection: { layerId: string; index: number } | null;
  /** Additional selected items (same shape as primary). Empty in
   *  the single-select common case. */
  extraSelected: Array<{ layerId: string; index: number }>;
  selectItem: (layerId: string, index: number) => void;
  /** Add to or remove from the extra-selected set (Shift+click /
   *  lasso). When `add` is false, removes if present. */
  toggleExtraSelected: (layerId: string, index: number) => void;
  /** Replace extra-selected with the given set. Used by the lasso
   *  tool on release. */
  setMultiSelection: (items: Array<{ layerId: string; index: number }>) => void;
  deselect: () => void;
  /** Pick every item on every visible layer. */
  selectAll: () => void;
  /** Pick every item that isn't currently selected. */
  invertSelection: () => void;

  // Secondary color (Paint's "Color 2"). Right-click paints with
  // this; X swaps primary ↔ secondary. Set via setter to keep the
  // interface symmetric with `color`.
  color2: string;
  /** Swap primary ↔ secondary colors. Bound to the X key. */
  swapColors: () => void;

  // Layer mutations
  addLayer: () => string;
  removeLayer: (layerId: string) => void;
  renameLayer: (layerId: string, name: string) => void;
  setLayerVisible: (layerId: string, visible: boolean) => void;
  setLayerOpacity: (layerId: string, opacity: number) => void;
  setLayerLocked: (layerId: string, locked: boolean) => void;
  moveLayer: (layerId: string, dir: 'up' | 'down') => void;

  // Typography (drives new TextItems + writes through to a selected
  // text item, if any). All four fields are reactive so the
  // TypographyPanel binds straight to them.
  fontFamily: string;
  fontSize: number;
  bold: boolean;
  italic: boolean;
  textAlign: 'left' | 'center' | 'right';

  // Brush style — sub-picker for the pen tool.
  brushStyle: BrushStyle;

  /** Phase 1.22 — default routing mode for new connectors. Bound
   *  to the routing-mode picker in the panel when connector tool
   *  is active. New ConnectorItems pick this up at commit. */
  connectorMode: 'straight' | 'orthogonal' | 'curve';

  /** Phase 1.21 — active stamp-brush id, or null when the user is
   *  drawing with a procedural brush style. When set, new pen
   *  strokes carry it as `stampId` so the renderer takes the stamp
   *  path. Setting a stamp clears `brushStyle` from the UI
   *  perspective (panel highlights the stamp button, not a style
   *  button) but the stored value is left alone so toggling back
   *  to a procedural brush restores the previous style choice. */
  stampId: string | null;

  // Whole-doc operations
  clearAll: () => void;
  /** Load a saved doc (e.g. user opened a previous whiteboard for
   *  edit). Resets history. */
  load: (doc: BrushContent) => void;

  /** Crop the doc to a sub-rectangle (in source-canvas coords).
   *  Every item is translated so the crop's top-left becomes the
   *  new origin; items entirely outside the crop are dropped.
   *  source_w / source_h shrink to the crop dimensions. */
  crop: (x: number, y: number, w: number, h: number) => void;

  // ── Image transforms (C-1.14) ────────────────────────────────
  /** Flip every item horizontally about the canvas vertical axis.
   *  source_w / source_h stay the same; strokes have every point's
   *  x mirrored, shapes / text / image have their x re-anchored. */
  flipHorizontal: () => void;
  /** Mirror vertical axis. */
  flipVertical: () => void;
  /** Rotate the whole doc 90° clockwise. source_w / source_h swap. */
  rotateClockwise: () => void;
  /** Rotate 90° counter-clockwise. */
  rotateCounterClockwise: () => void;
  /** Resize the source canvas + rescale every item to fit. Items
   *  retain their relative position + size on the new canvas. */
  resizeCanvas: (w: number, h: number) => void;
  /** Invert every item's color (hex → 1-hex). Strokes / shapes /
   *  text only; images are left as-is (CSS filter could invert at
   *  render time but not at the data level). */
  invertColors: () => void;

  /** Canvas background color (C-1.18). Defaults to '#ffffff'.
   *  Surfaced as a getter/setter so the Canvas section in the tool
   *  panel binds straight to it. Every write commits a history
   *  snapshot so undo rewinds the color change as a discrete step. */
  canvasColor: string;

  // ── Viewport (C-1.19, infinite-canvas mode) ─────────────────────
  // Per-user pan + zoom state. Lives on the session (not the doc)
  // because it's a view preference, not part of the saved sketch.
  // World coords = source-canvas coords; CSS coords = viewport
  // pixels in the canvas wrapper. The view transform maps world
  // to CSS via: cssX = worldX * zoom + viewX (analogous Y).
  /** Pan offset in CSS pixels — added after the zoom multiply. */
  viewX: number;
  viewY: number;
  /** Zoom factor. 1 = source pixel = CSS pixel; 0.1 = zoomed way
   *  out (Miro / Figma typical floor); 8 = max zoom in. Clamped on
   *  every setter so callers can't push beyond the useful range. */
  viewZoom: number;
  /** Zoom by `factor` around the viewport-space anchor (cssX, cssY)
   *  so the world point under the cursor stays under the cursor —
   *  Miro / Figma wheel-zoom behaviour. */
  zoomBy: (factor: number, anchorCssX: number, anchorCssY: number) => void;
  /** Pan in CSS pixels (positive dx → world moves right under the
   *  viewport). */
  panBy: (dx: number, dy: number) => void;
  /** Re-center on the source-canvas rect at zoom 1. */
  resetView: (viewportW: number, viewportH: number) => void;
  /** Fit the source-canvas rect inside the viewport with margin. */
  fitView: (viewportW: number, viewportH: number, margin?: number) => void;

  /** Total item count across every layer. Live; the panel renders a
   *  soft-cap warning banner when this passes ITEM_SOFT_CAP. */
  totalItems: number;
}

const HISTORY_MAX = 64;
// C-1.19 — item caps. Soft cap surfaces a banner so users see the
// warning before it becomes a problem; hard cap refuses new items
// to keep the JSONB blob + render loop from blowing past sane
// browser limits. Numbers picked from manual perf testing on a
// mid-range laptop (60 fps holds well under 5k items, starts to
// noticeably drop past ~15k even with viewport culling).
export const ITEM_SOFT_CAP = 5000;
export const ITEM_HARD_CAP = 20000;
/** Total item count across every layer. Hoisted so the banner UI
 *  + the cap check share one implementation. */
export function totalItemCount(doc: BrushContent): number {
  let n = 0;
  for (const l of doc.layers) n += l.items.length;
  return n;
}

export function createWhiteboardSession(
  sourceW: number,
  sourceH: number,
): WhiteboardSession {
  // Single reactive root the canvas + tool panel both bind to.
  const initialDoc = emptyDoc(sourceW, sourceH);
  interface ReactiveState {
    doc: BrushContent;
    activeLayerId: string | null;
    tool: Tool;
    color: string;
    width: number;
    opacity: number;
    fillShapes: boolean;
    selection: { layerId: string; index: number } | null;
    extraSelected: Array<{ layerId: string; index: number }>;
    // Secondary color — Paint's "Color 2". Right-click on the
    // canvas paints with this; X key swaps primary ↔ secondary.
    color2: string;
    // Typography state — used by the text tool for new items and
    // (when a text item is selected) reflected back from / written
    // through to that item.
    fontFamily: string;
    fontSize: number;
    bold: boolean;
    italic: boolean;
    textAlign: 'left' | 'center' | 'right';
    /** Active brush style — only meaningful when the pen tool is
     *  selected. The brushes sub-picker mutates this; new
     *  StrokeItems pick it up on commit. */
    brushStyle: BrushStyle;
    /** Phase 1.21 — active stamp brush id, or null when the user is
     *  drawing with a procedural style. */
    stampId: string | null;
    /** Phase 1.22 — default routing mode for new connectors. */
    connectorMode: 'straight' | 'orthogonal' | 'curve';
    // ── View transform (C-1.19, infinite canvas) ────────────────
    // The map is: cssX = worldX * viewZoom + viewX. Pan + zoom are
    // applied at render time via ctx.setTransform; pointer events
    // run the inverse to recover world coords.
    viewX: number;
    viewY: number;
    viewZoom: number;
  }
  const state = $state<ReactiveState>({
    doc: initialDoc,
    activeLayerId: initialDoc.layers[0]?.id ?? null,
    tool: 'pen',
    color: PALETTE[7],
    width: SIZES[1].width,
    opacity: defaultOpacityFor('pen'),
    fillShapes: false,
    selection: null,
    extraSelected: [],
    color2: '#ffffff',
    fontFamily: DEFAULT_FONT_FAMILY,
    fontSize: 24,
    bold: false,
    italic: false,
    textAlign: 'left',
    brushStyle: 'default',
    stampId: null,
    connectorMode: 'straight',
    viewX: 0,
    viewY: 0,
    viewZoom: 1,
  });

  // Zoom bounds. 0.05 = 5% (fits a huge sketch in a postcard
  // viewport — Miro's floor); 16 = 1600% (pixel-grid pixel-poking).
  const ZOOM_MIN = 0.05;
  const ZOOM_MAX = 16;
  function clampZoom(z: number): number {
    return Math.max(ZOOM_MIN, Math.min(ZOOM_MAX, z));
  }

  // Undo / redo. Snapshot-based — every mutating method commits the
  // post-mutation state. Simpler than a per-action log when the
  // mutation set is small + the document is bounded.
  const history: SessionSnapshot[] = $state([]);
  let historyIdx = $state(-1);

  function deepCopy<T>(v: T): T {
    return JSON.parse(JSON.stringify($state.snapshot(v))) as T;
  }

  function snapshot(): SessionSnapshot {
    return {
      doc: deepCopy(state.doc),
      activeLayerId: state.activeLayerId,
    };
  }

  function commit() {
    // Drop redo tail when branching off an undone state.
    history.splice(historyIdx + 1, history.length - (historyIdx + 1));
    history.push(snapshot());
    if (history.length > HISTORY_MAX) history.shift();
    historyIdx = history.length - 1;
  }

  // Seed history with the initial empty doc so undo can reach it.
  commit();

  function restore(snap: SessionSnapshot) {
    state.doc = deepCopy(snap.doc);
    state.activeLayerId = snap.activeLayerId;
  }

  function findLayer(id: string): Layer | undefined {
    return state.doc.layers.find((l) => l.id === id);
  }

  return {
    // Reactive proxies — these reads / writes are reactive because
    // state.* is a $state proxy. Getters because Svelte 5 forwards
    // get/set through proxies cleanly.
    get doc() { return state.doc; },
    set doc(v) { state.doc = v; },
    get activeLayerId() { return state.activeLayerId; },
    set activeLayerId(v) { state.activeLayerId = v; },
    get tool() { return state.tool; },
    set tool(v) {
      state.tool = v;
      // Auto-tune opacity to the tool's natural default — saves the
      // user from manually re-bumping the slider every tool switch.
      state.opacity = defaultOpacityFor(v);
      // Switching away from select clears any pending selection so
      // handles don't linger over the canvas after the user picks
      // a different tool.
      if (v !== 'select') state.selection = null;
    },
    get color() { return state.color; },
    set color(v) { state.color = v; },
    get color2() { return state.color2; },
    set color2(v) { state.color2 = v; },
    swapColors() {
      const tmp = state.color;
      state.color = state.color2;
      state.color2 = tmp;
    },
    get width() { return state.width; },
    set width(v) { state.width = v; },
    get opacity() { return state.opacity; },
    set opacity(v) { state.opacity = v; },
    get fillShapes() { return state.fillShapes; },
    set fillShapes(v) { state.fillShapes = v; },

    // Canvas background color. Live on the doc itself (not on the
    // session) so it round-trips through save/load like any other
    // doc property. Defaults to '#ffffff' when missing for back-
    // compat with C-1.0..C-1.17 saves that don't carry the field.
    get canvasColor() { return state.doc.canvas_color ?? '#ffffff'; },
    set canvasColor(v) {
      state.doc = { ...state.doc, canvas_color: v };
      commit();
    },

    get canUndo() { return historyIdx > 0; },
    get canRedo() { return historyIdx < history.length - 1; },

    undo() {
      if (historyIdx <= 0) return;
      historyIdx -= 1;
      restore(history[historyIdx]);
    },
    redo() {
      if (historyIdx >= history.length - 1) return;
      historyIdx += 1;
      restore(history[historyIdx]);
    },

    addItem(layerId, item) {
      const layer = findLayer(layerId);
      if (!layer || layer.locked) return;
      // Hard cap — refuse to push past the cap so neither the JSONB
      // blob nor the render loop blows up. The soft-cap banner in
      // WhiteboardToolPanel warns the user well before they hit
      // this limit so this branch firing is the "abuse / runaway
      // bug" failsafe, not the daily ceiling.
      if (totalItemCount(state.doc) >= ITEM_HARD_CAP) {
        // eslint-disable-next-line no-console
        console.warn(`[whiteboard] hard item cap reached (${ITEM_HARD_CAP}); refusing addItem`);
        return;
      }
      layer.items.push(item);
      commit();
    },

    replaceItem(layerId, index, item) {
      const layer = findLayer(layerId);
      if (!layer || layer.locked) return;
      if (index < 0 || index >= layer.items.length) return;
      layer.items[index] = item;
      commit();
    },

    removeItems(layerId, indexes) {
      const layer = findLayer(layerId);
      if (!layer || layer.locked) return;
      const set = new Set(indexes);
      layer.items = layer.items.filter((_, i) => !set.has(i));
      // Clear selection if the removed item was selected.
      if (state.selection && state.selection.layerId === layerId && set.has(state.selection.index)) {
        state.selection = null;
      }
      commit();
    },

    get selection() { return state.selection; },
    set selection(v) { state.selection = v; },
    get extraSelected() { return state.extraSelected; },
    toggleExtraSelected(layerId, index) {
      const existing = state.extraSelected.findIndex(
        (s) => s.layerId === layerId && s.index === index,
      );
      if (existing >= 0) {
        state.extraSelected.splice(existing, 1);
      } else {
        state.extraSelected.push({ layerId, index });
      }
    },
    setMultiSelection(items) {
      // First item becomes primary; the rest go into extraSelected.
      if (items.length === 0) {
        state.selection = null;
        state.extraSelected = [];
        return;
      }
      state.selection = { layerId: items[0].layerId, index: items[0].index };
      state.extraSelected = items.slice(1);
    },
    selectItem(layerId, index) {
      state.selection = { layerId, index };
      state.extraSelected = [];
      // When a text item is picked, pull its typography state into
      // the session so the TypographyPanel reflects its current
      // values (font, size, weight, italic, align).
      const layer = state.doc.layers.find((l) => l.id === layerId);
      const item = layer?.items[index];
      if (item && item.kind === 'text') {
        state.fontFamily = item.fontFamily ?? DEFAULT_FONT_FAMILY;
        state.fontSize = item.fontSize;
        state.bold = item.bold ?? false;
        state.italic = item.italic ?? false;
        state.textAlign = item.align ?? 'left';
      }
    },
    deselect() {
      state.selection = null;
      state.extraSelected = [];
    },

    selectAll() {
      const picks: Array<{ layerId: string; index: number }> = [];
      for (const layer of state.doc.layers) {
        if (!layer.visible) continue;
        for (let i = 0; i < layer.items.length; i++) {
          picks.push({ layerId: layer.id, index: i });
        }
      }
      if (picks.length === 0) return;
      state.selection = picks[0];
      state.extraSelected = picks.slice(1);
    },

    invertSelection() {
      // "Invert" = every currently-NOT-selected item across visible
      // layers becomes selected; previously-selected items become
      // unselected.
      const selectedSet = new Set<string>();
      if (state.selection) selectedSet.add(`${state.selection.layerId}:${state.selection.index}`);
      for (const s of state.extraSelected) selectedSet.add(`${s.layerId}:${s.index}`);
      const picks: Array<{ layerId: string; index: number }> = [];
      for (const layer of state.doc.layers) {
        if (!layer.visible) continue;
        for (let i = 0; i < layer.items.length; i++) {
          if (!selectedSet.has(`${layer.id}:${i}`)) {
            picks.push({ layerId: layer.id, index: i });
          }
        }
      }
      if (picks.length === 0) {
        state.selection = null;
        state.extraSelected = [];
      } else {
        state.selection = picks[0];
        state.extraSelected = picks.slice(1);
      }
    },

    // ── Typography — write through to a selected text item ────────
    // Setters mutate the selected text item too (if one exists) so
    // tweaking the font on the panel updates the canvas immediately.
    // Each write commits a history snapshot so undo rewinds the text
    // change as a discrete step.
    get fontFamily() { return state.fontFamily; },
    set fontFamily(v) {
      state.fontFamily = v;
      const sel = state.selection;
      if (!sel) return;
      const layer = state.doc.layers.find((l) => l.id === sel.layerId);
      const item = layer?.items[sel.index];
      if (item && item.kind === 'text' && !layer!.locked) {
        layer!.items[sel.index] = { ...item, fontFamily: v };
        commit();
      }
    },
    get fontSize() { return state.fontSize; },
    set fontSize(v) {
      state.fontSize = Math.max(8, Math.min(256, Math.round(v)));
      const sel = state.selection;
      if (!sel) return;
      const layer = state.doc.layers.find((l) => l.id === sel.layerId);
      const item = layer?.items[sel.index];
      if (item && item.kind === 'text' && !layer!.locked) {
        layer!.items[sel.index] = { ...item, fontSize: state.fontSize };
        commit();
      }
    },
    get bold() { return state.bold; },
    set bold(v) {
      state.bold = v;
      const sel = state.selection;
      if (!sel) return;
      const layer = state.doc.layers.find((l) => l.id === sel.layerId);
      const item = layer?.items[sel.index];
      if (item && item.kind === 'text' && !layer!.locked) {
        layer!.items[sel.index] = { ...item, bold: v };
        commit();
      }
    },
    get italic() { return state.italic; },
    set italic(v) {
      state.italic = v;
      const sel = state.selection;
      if (!sel) return;
      const layer = state.doc.layers.find((l) => l.id === sel.layerId);
      const item = layer?.items[sel.index];
      if (item && item.kind === 'text' && !layer!.locked) {
        layer!.items[sel.index] = { ...item, italic: v };
        commit();
      }
    },
    get brushStyle() { return state.brushStyle; },
    set brushStyle(v) {
      state.brushStyle = v;
      // Picking a procedural style clears any active stamp brush so
      // the panel UI + renderer don't get into a "both set" state.
      state.stampId = null;
    },
    get stampId() { return state.stampId; },
    set stampId(v) {
      state.stampId = v;
      // Picking a stamp also implies the pen tool (other brush tools
      // ignore stamps) — auto-switch so the user can paint right away.
      if (v && state.tool !== 'pen') state.tool = 'pen';
    },
    get connectorMode() { return state.connectorMode; },
    set connectorMode(v) { state.connectorMode = v; },

    get textAlign() { return state.textAlign; },
    set textAlign(v) {
      state.textAlign = v;
      const sel = state.selection;
      if (!sel) return;
      const layer = state.doc.layers.find((l) => l.id === sel.layerId);
      const item = layer?.items[sel.index];
      if (item && item.kind === 'text' && !layer!.locked) {
        layer!.items[sel.index] = { ...item, align: v };
        commit();
      }
    },

    addLayer() {
      const layer = newLayer(`Layer ${state.doc.layers.length + 1}`);
      state.doc.layers.push(layer);
      state.activeLayerId = layer.id;
      commit();
      return layer.id;
    },

    removeLayer(layerId) {
      if (state.doc.layers.length <= 1) return; // never zero
      const idx = state.doc.layers.findIndex((l) => l.id === layerId);
      if (idx === -1) return;
      state.doc.layers.splice(idx, 1);
      if (state.activeLayerId === layerId) {
        state.activeLayerId =
          state.doc.layers[Math.min(idx, state.doc.layers.length - 1)]?.id ?? null;
      }
      commit();
    },

    renameLayer(layerId, name) {
      const layer = findLayer(layerId);
      if (!layer) return;
      layer.name = name;
      commit();
    },

    setLayerVisible(layerId, visible) {
      const layer = findLayer(layerId);
      if (!layer) return;
      layer.visible = visible;
      commit();
    },

    setLayerOpacity(layerId, opacity) {
      const layer = findLayer(layerId);
      if (!layer) return;
      layer.opacity = Math.max(0, Math.min(1, opacity));
      commit();
    },

    setLayerLocked(layerId, locked) {
      const layer = findLayer(layerId);
      if (!layer) return;
      layer.locked = locked;
      commit();
    },

    moveLayer(layerId, dir) {
      const idx = state.doc.layers.findIndex((l) => l.id === layerId);
      if (idx === -1) return;
      const swap = dir === 'up' ? idx + 1 : idx - 1;
      if (swap < 0 || swap >= state.doc.layers.length) return;
      const [layer] = state.doc.layers.splice(idx, 1);
      state.doc.layers.splice(swap, 0, layer);
      commit();
    },

    clearAll() {
      state.doc = {
        ...state.doc,
        layers: state.doc.layers.map((l) => ({ ...l, items: [] })),
      };
      commit();
    },

    load(doc) {
      state.doc = deepCopy(doc);
      state.activeLayerId = state.doc.layers[0]?.id ?? null;
      history.length = 0;
      historyIdx = -1;
      commit();
    },

    flipHorizontal() {
      const w = state.doc.source_w;
      for (const layer of state.doc.layers) {
        layer.items = layer.items.map((item) => {
          if (item.kind === 'stroke') {
            return {
              ...item,
              points: item.points.map((p) => [w - p[0], p[1], p[2]] as [number, number, number?]),
            };
          }
          // Mirror around the canvas vertical axis. For shape we
          // also negate its own width so the drag-direction-coded
          // shape still renders the same on the mirrored side.
          if (item.kind === 'shape') {
            return { ...item, x: w - item.x - item.w };
          }
          if (item.kind === 'connector') {
            // Attached endpoints: target's bbox flips with it, so
            // (u, v) reads correctly on the flipped target — but
            // we mirror u so left ↔ right edge anchors swap to
            // match the visual mirror.
            const flip = (ep: typeof item.start) => {
              if (ep.attached) return { ...ep, u: 1 - (ep.u ?? 0.5) };
              return { ...ep, x: w - (ep.x ?? 0) };
            };
            return { ...item, start: flip(item.start), end: flip(item.end) };
          }
          if (item.kind === 'mindmap') {
            // Mindmap: just mirror the root x. Layout regenerates
            // from the root anchor + tree.
            return { ...item, x: w - item.x };
          }
          // text / image: anchor x flips so the box still lives on
          // the canvas; aligned-left text re-aligns relative to its
          // anchor visually, which is the expected mirror behaviour.
          return { ...item, x: w - item.x - item.w };
        });
      }
      state.selection = null;
      state.extraSelected = [];
      commit();
    },

    flipVertical() {
      const h = state.doc.source_h;
      for (const layer of state.doc.layers) {
        layer.items = layer.items.map((item) => {
          if (item.kind === 'stroke') {
            return {
              ...item,
              points: item.points.map((p) => [p[0], h - p[1], p[2]] as [number, number, number?]),
            };
          }
          if (item.kind === 'shape') {
            return { ...item, y: h - item.y - item.h };
          }
          if (item.kind === 'connector') {
            const flip = (ep: typeof item.start) => {
              if (ep.attached) return { ...ep, v: 1 - (ep.v ?? 0.5) };
              return { ...ep, y: h - (ep.y ?? 0) };
            };
            return { ...item, start: flip(item.start), end: flip(item.end) };
          }
          if (item.kind === 'mindmap') {
            return { ...item, y: h - item.y };
          }
          return { ...item, y: h - item.y - item.h };
        });
      }
      state.selection = null;
      state.extraSelected = [];
      commit();
    },

    rotateClockwise() {
      // Source (W, H) becomes (H, W). Every point (x, y) maps to
      // (H - y, x). For shapes / text / image we re-anchor the
      // top-left + swap w/h.
      const W = state.doc.source_w;
      const H = state.doc.source_h;
      for (const layer of state.doc.layers) {
        layer.items = layer.items.map((item) => {
          if (item.kind === 'stroke') {
            return {
              ...item,
              points: item.points.map((p) => [H - p[1], p[0], p[2]] as [number, number, number?]),
            };
          }
          if (item.kind === 'shape') {
            return { ...item, x: H - item.y - item.h, y: item.x, w: item.h, h: item.w };
          }
          if (item.kind === 'connector') {
            // 90° CW on (u, v) within a bbox: (u, v) → (1-v, u).
            // Free coords map (x, y) → (H - y, x) same as strokes.
            const rot = (ep: typeof item.start) => {
              if (ep.attached) return { ...ep, u: 1 - (ep.v ?? 0.5), v: ep.u ?? 0.5 };
              return { ...ep, x: H - (ep.y ?? 0), y: ep.x ?? 0 };
            };
            return { ...item, start: rot(item.start), end: rot(item.end) };
          }
          if (item.kind === 'mindmap') {
            return { ...item, x: H - item.y, y: item.x };
          }
          // text / image — keep dims, just re-anchor.
          return { ...item, x: H - item.y - item.h, y: item.x, w: item.h, h: item.w };
        });
      }
      state.doc.source_w = H;
      state.doc.source_h = W;
      state.selection = null;
      state.extraSelected = [];
      commit();
    },

    rotateCounterClockwise() {
      const W = state.doc.source_w;
      const H = state.doc.source_h;
      for (const layer of state.doc.layers) {
        layer.items = layer.items.map((item) => {
          if (item.kind === 'stroke') {
            return {
              ...item,
              points: item.points.map((p) => [p[1], W - p[0], p[2]] as [number, number, number?]),
            };
          }
          if (item.kind === 'shape') {
            return { ...item, x: item.y, y: W - item.x - item.w, w: item.h, h: item.w };
          }
          if (item.kind === 'connector') {
            // 90° CCW on (u, v): (u, v) → (v, 1-u). Inverse of CW.
            const rot = (ep: typeof item.start) => {
              if (ep.attached) return { ...ep, u: ep.v ?? 0.5, v: 1 - (ep.u ?? 0.5) };
              return { ...ep, x: ep.y ?? 0, y: W - (ep.x ?? 0) };
            };
            return { ...item, start: rot(item.start), end: rot(item.end) };
          }
          if (item.kind === 'mindmap') {
            return { ...item, x: item.y, y: W - item.x };
          }
          return { ...item, x: item.y, y: W - item.x - item.w, w: item.h, h: item.w };
        });
      }
      state.doc.source_w = H;
      state.doc.source_h = W;
      state.selection = null;
      state.extraSelected = [];
      commit();
    },

    resizeCanvas(newW, newH) {
      const sx = newW / state.doc.source_w;
      const sy = newH / state.doc.source_h;
      for (const layer of state.doc.layers) {
        layer.items = layer.items.map((item) => {
          if (item.kind === 'stroke') {
            return {
              ...item,
              points: item.points.map((p) => [p[0] * sx, p[1] * sy, p[2]] as [number, number, number?]),
              width: item.width * Math.min(sx, sy),
            };
          }
          if (item.kind === 'connector') {
            // Attached endpoints follow their target's scaled bbox
            // for free; only free endpoint coords need scaling.
            const scale = (ep: typeof item.start) => {
              if (ep.attached) return ep;
              return { ...ep, x: (ep.x ?? 0) * sx, y: (ep.y ?? 0) * sy };
            };
            return { ...item, start: scale(item.start), end: scale(item.end) };
          }
          if (item.kind === 'mindmap') {
            return { ...item, x: item.x * sx, y: item.y * sy };
          }
          return {
            ...item,
            x: item.x * sx,
            y: item.y * sy,
            w: item.w * sx,
            h: item.h * sy,
          };
        });
      }
      state.doc.source_w = Math.max(1, Math.round(newW));
      state.doc.source_h = Math.max(1, Math.round(newH));
      commit();
    },

    invertColors() {
      const invert = (hex: string): string => {
        const m = /^#?([0-9a-f]{6})$/i.exec(hex);
        if (!m) return hex;
        const n = parseInt(m[1], 16);
        const r = 255 - ((n >> 16) & 0xff);
        const g = 255 - ((n >> 8) & 0xff);
        const b = 255 - (n & 0xff);
        const p = (v: number) => v.toString(16).padStart(2, '0');
        return `#${p(r)}${p(g)}${p(b)}`;
      };
      for (const layer of state.doc.layers) {
        layer.items = layer.items.map((item) => {
          if (item.kind === 'image') return item;
          if (item.kind === 'shape') {
            // C-1.17 — invert every color the shape carries so the
            // outline + fill flip together. Legacy `color` stays in
            // sync so back-compat readers see consistent state.
            return {
              ...item,
              color: invert(item.color),
              strokeColor: item.strokeColor ? invert(item.strokeColor) : undefined,
              fillColor: item.fillColor ? invert(item.fillColor) : undefined,
            };
          }
          if (item.kind === 'frame') {
            return { ...item, color: item.color ? invert(item.color) : undefined };
          }
          if (item.kind === 'sticky') {
            return {
              ...item,
              background: item.background ? invert(item.background) : undefined,
              color: item.color ? invert(item.color) : undefined,
            };
          }
          if (item.kind === 'mindmap') {
            // Mindmap colors live per-node in the tree + per-branch
            // in branchColors[]. Invert both.
            const invertNode = (n: typeof item.root): typeof item.root => ({
              ...n,
              color: n.color ? invert(n.color) : undefined,
              children: n.children.map(invertNode),
            });
            return {
              ...item,
              root: invertNode(item.root),
              branchColors: item.branchColors?.map(invert),
            };
          }
          // stroke / text / connector all carry required `color`.
          return { ...item, color: invert(item.color) };
        });
      }
      commit();
    },

    crop(x, y, w, h) {
      // Drop items entirely outside the crop bbox; translate every
      // surviving item so the crop's top-left becomes the new
      // origin. Stroke points get shifted individually.
      const x2 = x + w;
      const y2 = y + h;
      state.doc.source_w = Math.max(1, Math.round(w));
      state.doc.source_h = Math.max(1, Math.round(h));
      for (const layer of state.doc.layers) {
        layer.items = layer.items
          .filter((item) => {
            // Cheap aabb test using itemBBox-like math inline.
            if (item.kind === 'stroke') {
              return item.points.some((p) => p[0] >= x && p[0] <= x2 && p[1] >= y && p[1] <= y2);
            }
            if (item.kind === 'connector') {
              // Cheap: check both resolved endpoints + keep if
              // either is inside the crop. Misses connectors that
              // only have their middle segment inside the crop;
              // good-enough for v1.
              const s = resolveConnectorEndpoint(item.start, state.doc);
              const e = resolveConnectorEndpoint(item.end, state.doc);
              return (s.x >= x && s.x <= x2 && s.y >= y && s.y <= y2)
                  || (e.x >= x && e.x <= x2 && e.y >= y && e.y <= y2);
            }
            if (item.kind === 'mindmap') {
              // Mindmap doesn't carry w/h on the type; use bbox.
              const bb = itemBBox(item, state.doc);
              return bb.x + bb.w >= x && bb.x <= x2 && bb.y + bb.h >= y && bb.y <= y2;
            }
            const ix = item.kind === 'shape' && item.w < 0 ? item.x + item.w : item.x;
            const iy = item.kind === 'shape' && item.h < 0 ? item.y + item.h : item.y;
            const iw = item.kind === 'shape' ? Math.abs(item.w) : item.w;
            const ih = item.kind === 'shape' ? Math.abs(item.h) : item.h;
            return ix + iw >= x && ix <= x2 && iy + ih >= y && iy <= y2;
          })
          .map((item) => {
            if (item.kind === 'stroke') {
              return {
                ...item,
                points: item.points.map((p) => [p[0] - x, p[1] - y, p[2]] as [number, number, number?]),
              };
            }
            if (item.kind === 'connector') {
              // Translate free endpoints by the crop offset;
              // attached endpoints follow their target (which the
              // outer crop loop also translates) for free.
              const shift = (ep: typeof item.start) => {
                if (ep.attached) return ep;
                return { ...ep, x: (ep.x ?? 0) - x, y: (ep.y ?? 0) - y };
              };
              return { ...item, start: shift(item.start), end: shift(item.end) };
            }
            return { ...item, x: item.x - x, y: item.y - y };
          });
      }
      state.selection = null;
      state.extraSelected = [];
      commit();
    },

    // ── Viewport (C-1.19) ─────────────────────────────────────────
    // View state is per-user / per-session, not persisted to the
    // saved doc. The host opt-in flag on BrushCanvas decides
    // whether the canvas reads view.* at render time (whiteboard
    // mode) or ignores it (annotation mode = always fit-to-source).
    get viewX() { return state.viewX; },
    set viewX(v) { state.viewX = v; },
    get viewY() { return state.viewY; },
    set viewY(v) { state.viewY = v; },
    get viewZoom() { return state.viewZoom; },
    set viewZoom(v) { state.viewZoom = clampZoom(v); },

    zoomBy(factor, anchorCssX, anchorCssY) {
      const z0 = state.viewZoom;
      const z1 = clampZoom(z0 * factor);
      if (z1 === z0) return;
      // Keep the world point under the anchor stationary in CSS
      // space. World point at the anchor pre-zoom:
      //   wx = (anchorCssX - viewX) / z0
      // Post-zoom, we want anchorCssX to still map to wx, so:
      //   anchorCssX = wx * z1 + viewX'
      //   viewX' = anchorCssX - wx * z1
      //          = anchorCssX - ((anchorCssX - viewX) / z0) * z1
      //          = anchorCssX - (anchorCssX - viewX) * (z1 / z0)
      const k = z1 / z0;
      state.viewX = anchorCssX - (anchorCssX - state.viewX) * k;
      state.viewY = anchorCssY - (anchorCssY - state.viewY) * k;
      state.viewZoom = z1;
    },

    panBy(dx, dy) {
      state.viewX += dx;
      state.viewY += dy;
    },

    resetView(viewportW, viewportH) {
      // Center the (0..source_w, 0..source_h) rect at zoom 1.
      state.viewZoom = 1;
      state.viewX = (viewportW - state.doc.source_w) / 2;
      state.viewY = (viewportH - state.doc.source_h) / 2;
    },

    get totalItems() { return totalItemCount(state.doc); },

    fitView(viewportW, viewportH, margin = 32) {
      const aw = Math.max(1, viewportW - margin * 2);
      const ah = Math.max(1, viewportH - margin * 2);
      const z = clampZoom(Math.min(aw / state.doc.source_w, ah / state.doc.source_h));
      state.viewZoom = z;
      state.viewX = (viewportW - state.doc.source_w * z) / 2;
      state.viewY = (viewportH - state.doc.source_h * z) / 2;
    },
  };
}
