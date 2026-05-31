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
  newLayer,
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
}

const HISTORY_MAX = 64;

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
    // Typography state — used by the text tool for new items and
    // (when a text item is selected) reflected back from / written
    // through to that item.
    fontFamily: string;
    fontSize: number;
    bold: boolean;
    italic: boolean;
    textAlign: 'left' | 'center' | 'right';
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
    fontFamily: DEFAULT_FONT_FAMILY,
    fontSize: 24,
    bold: false,
    italic: false,
    textAlign: 'left',
  });

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
    get width() { return state.width; },
    set width(v) { state.width = v; },
    get opacity() { return state.opacity; },
    set opacity(v) { state.opacity = v; },
    get fillShapes() { return state.fillShapes; },
    set fillShapes(v) { state.fillShapes = v; },

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
            return { ...item, x: item.x - x, y: item.y - y };
          });
      }
      state.selection = null;
      state.extraSelected = [];
      commit();
    },
  };
}
