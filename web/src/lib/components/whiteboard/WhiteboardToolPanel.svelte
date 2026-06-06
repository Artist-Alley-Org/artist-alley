<script lang="ts">
  // WhiteboardToolPanel — the full toolbox, rendered into the
  // viewer's right pane (via PostHost's metadataSlot snippet swap).
  //
  // Sections, top-to-bottom:
  //   1. Tool picker   (4 brush + 4 shape + text)
  //   2. Color palette (8 swatches, click to set)
  //   3. Size slider   (continuous 1–48 px; brush + shape stroke width)
  //   4. Opacity       (continuous 0–1; tool-defaults itself when
  //                    tool changes, user can override)
  //   5. Shape fill toggle (when a shape tool is active)
  //   6. Layers panel  (add / delete / rename / reorder / visibility
  //                    / opacity / lock; active-layer highlight)
  //   7. Save / Cancel
  //
  // Everything binds straight to the session store — there's no
  // local state we have to forward back to the canvas.

  import type { Tool, MindmapItem, MindmapNode } from '$lib/whiteboard/types';
  import {
    FONT_SIZE_MAX,
    FONT_SIZE_MIN,
    FONT_SIZE_PRESETS,
    GOOGLE_FONTS,
    PALETTE,
    SIZES,
    isBrushTool,
    isShapeTool,
    addMindmapChild,
    removeMindmapNode,
    renameMindmapNode,
    walkMindmap,
  } from '$lib/whiteboard/types';
  import { loadFont } from '$lib/whiteboard/fonts.svelte';
  import { ITEM_SOFT_CAP, ITEM_HARD_CAP } from '$lib/whiteboard/session.svelte';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';
  import {
    listBrushPacks,
    registerPackFromAPI,
    unregisterPack,
    subscribeBrushPacks,
    type APIPack,
  } from '$lib/whiteboard/brushes';
  import { onMount } from 'svelte';
  import ColorPicker from './ColorPicker.svelte';

  interface Props {
    session: WhiteboardSession;
    saving?: boolean;
    saveError?: string | null;
    onSave: () => void;
    onClose: () => void;
    /** Compact-mode flag. When true, render the icon-strip layout
     *  (one icon per tool category, right-click to switch within
     *  the category). When false, render the full panel. Driven
     *  by the ToolPanelShell's paneCompact state — the shell's
     *  header chevron is the single toggle, no in-tool button. */
    compact?: boolean;
  }

  let { session, saving = false, saveError = null, onSave, onClose, compact = false }: Props = $props();

  // ── Brush-pack management (Phase 1.21d) ──────────────────────
  //
  // The panel renders `listBrushPacks()` directly, but the registry
  // is module-scope (not reactive). Subscribe to its change events
  // so a new import / delete re-renders the BRUSHES section without
  // requiring users to re-open the whiteboard.
  let packsBumper = $state(0); // bumped on subscribe-notify; reads force re-render
  onMount(() => {
    const unsub = subscribeBrushPacks(() => { packsBumper++; });
    void fetchInstalledPacks();
    return unsub;
  });
  const installedPacks = $derived.by(() => {
    void packsBumper; // create dependency
    return listBrushPacks();
  });

  // Load the user's previously-imported packs from the backend at
  // panel mount. Built-in pack is always present (registered at
  // module load); these append.
  async function fetchInstalledPacks() {
    try {
      const r = await fetch('/api/v1/brush-packs', { credentials: 'include' });
      if (!r.ok) return; // 401 on unauth is fine; built-ins still work
      const j = await r.json() as { packs: APIPack[] };
      for (const p of j.packs) registerPackFromAPI(p);
    } catch {
      // Network down → built-ins keep working; reload to retry.
    }
  }

  // Import-pack file picker. Triggered by the Import button in the
  // BRUSHES section; on file pick → POST multipart → register the
  // new pack from the server's response.
  let importInput: HTMLInputElement | undefined = $state();
  let importBusy = $state(false);
  let importError = $state<string | null>(null);
  async function onImportFiles(e: Event) {
    const t = e.currentTarget as HTMLInputElement;
    const file = t.files?.[0];
    t.value = ''; // reset so picking the same file twice works
    if (!file) return;
    importBusy = true;
    importError = null;
    try {
      const fd = new FormData();
      fd.append('file', file);
      fd.append('name', file.name.replace(/\.abr$/i, '') || 'Imported pack');
      const r = await fetch('/api/v1/brush-packs', {
        method: 'POST',
        credentials: 'include',
        body: fd,
      });
      if (!r.ok) {
        const err = await r.json().catch(() => ({ error: `HTTP ${r.status}` }));
        importError = err.error || 'Import failed';
        return;
      }
      const pack = await r.json() as APIPack;
      registerPackFromAPI(pack);
    } catch (e) {
      importError = (e instanceof Error ? e.message : 'Import failed');
    } finally {
      importBusy = false;
    }
  }

  async function deletePack(packId: string) {
    // Built-in pack is in-memory only; deletion just unregisters it
    // locally. User-imported packs round-trip through the backend.
    if (packId === 'builtin') {
      unregisterPack(packId);
      return;
    }
    if (!confirm('Delete this brush pack? This removes it from your account.')) return;
    try {
      const r = await fetch(`/api/v1/brush-packs/${packId}`, {
        method: 'DELETE',
        credentials: 'include',
      });
      if (r.ok || r.status === 404) unregisterPack(packId);
    } catch {
      // Leave the registry alone on failure so the user can retry.
    }
  }

  // Tool catalogue — split per-section so the sidebar maps each
  // tool to its right pane (matches Paint's Tools / Brushes /
  // Shapes / Image / Selection layout). Each section renders its
  // own grid; subsequent commits add more entries to each.
  interface ToolEntry { id: Tool; label: string; icon: string; }

  // Always-on utility tools. Drag selection + text input + recolor.
  const TOOLS_MAIN: ToolEntry[] = [
    { id: 'select',     label: 'Select (V)',      icon: 'M3 3l8 19 2-8 8-2z' },
    { id: 'eraser',     label: 'Eraser (E)',      icon: 'M3 19h18 M18 13L10 5l-7 7 6 6h8z' },
    { id: 'text',       label: 'Text (T)',        icon: 'M5 5h14 M12 5v14' },
    { id: 'bucket',     label: 'Fill bucket (B)', icon: 'M19 11l-7-7-8 8 7 7z M5 19h16 M16 4l3 7' },
    { id: 'eyedropper', label: 'Eyedropper (I)',  icon: 'M2 22l1-1h4l9-9-3-3-9 9v4z M14 7l3 3 M17 4l3 3-3 3-3-3z' },
    // Phase 1.22 — connector: click anchor → click anchor to link
    // shapes with a line. End sticks when the shape moves.
    { id: 'connector',  label: 'Connector', icon: 'M6 18a4 4 0 0 0 4-4 M14 10a4 4 0 0 0 4-4 M10 14L14 10' },
    // Phase 1.23 — frame: drag out a labelled boundary; items
    // inside move with it (Figma-style frames).
    { id: 'frame',      label: 'Frame', icon: 'M4 6h16v12H4z M4 9h16' },
    // Phase 1.23 — sticky note: drop a colored card with text.
    { id: 'sticky',     label: 'Sticky note', icon: 'M5 4h12l2 2v14H5z M19 6h-3v-2 M5 19h10' },
    // Label: a flat colored rectangle with centered text — a
    // chip/tag. Same item kind as sticky but with the
    // `style: 'label'` field for renderer-flipping.
    { id: 'label',      label: 'Label', icon: 'M3 7h14l4 5-4 5H3z M16 12h.01' },
    // Phase 1.24 — mindmap: drop a hierarchical tree with auto-
    // layout; double-click nodes to rename, panel buttons to add
    // / remove children.
    { id: 'mindmap',    label: 'Mindmap', icon: 'M4 12h6 M14 8h6 M14 16h6 M10 12c0 -2 2 -4 4 -4 M10 12c0 2 2 4 4 4' },
  ];

  // Brushes — same Tool ids today; C-1.13 will add a brush-style
  // sub-picker that mutates StrokeItem's per-stroke parameters
  // rather than adding more top-level tool ids.
  const TOOLS_BRUSHES: ToolEntry[] = [
    { id: 'pen',         label: 'Pen (P)',         icon: 'M14 4l6 6-10 10H4v-6z' },
    { id: 'marker',      label: 'Marker (M)',      icon: 'M16 2l6 6-12 12-4-4z' },
    { id: 'highlighter', label: 'Highlighter (H)', icon: 'M9 11l-6 6v4h4l6-6 M14 4l6 6-7 7-6-6z' },
  ];

  // Shapes — geometric primitives. C-1.12 expands this with
  // polygon / star / heart / rounded-rect / pentagon / hexagon /
  // right-triangle / diamond / callouts.
  const TOOLS_SHAPES: ToolEntry[] = [
    { id: 'line',    label: 'Line (L)',      icon: 'M5 19L19 5' },
    { id: 'arrow',   label: 'Arrow (A)',     icon: 'M5 19L19 5 M19 5h-6 M19 5v6' },
    { id: 'rect',    label: 'Rectangle (R)', icon: 'M4 6h16v12H4z' },
    { id: 'rounded-rect', label: 'Rounded rectangle', icon: 'M6 4h12a2 2 0 0 1 2 2v12a2 2 0 0 1 -2 2H6a2 2 0 0 1 -2 -2V6a2 2 0 0 1 2 -2z' },
    { id: 'ellipse', label: 'Ellipse (O)',   icon: 'M12 6c5 0 8 2.5 8 6s-3 6-8 6-8-2.5-8-6 3-6 8-6z' },
    { id: 'triangle', label: 'Triangle (G)', icon: 'M12 4l9 16H3z' },
    { id: 'right-triangle', label: 'Right triangle', icon: 'M4 4v16h16z' },
    { id: 'diamond', label: 'Diamond', icon: 'M12 3l9 9-9 9-9-9z' },
    { id: 'pentagon', label: 'Pentagon', icon: 'M12 3l9 6.5-3.5 10.5h-11L3 9.5z' },
    { id: 'hexagon',  label: 'Hexagon',  icon: 'M7 3h10l5 9-5 9H7l-5-9z' },
    { id: 'star',     label: 'Star',     icon: 'M12 3l2.6 6h6.4l-5.2 4 2 7-5.8-4-5.8 4 2-7-5.2-4h6.4z' },
    { id: 'heart',    label: 'Heart',    icon: 'M20.8 4.6a5.5 5.5 0 0 0-7.8 0L12 5.7l-1-1.1a5.5 5.5 0 0 0-7.8 7.8l1 1.1L12 21.2l7.8-7.7 1-1.1a5.5 5.5 0 0 0 0-7.8z' },
    { id: 'callout-rect', label: 'Callout (rectangle)', icon: 'M4 4h16v12H8l-3 4v-4H4z' },
    { id: 'callout-oval', label: 'Callout (oval)', icon: 'M12 4c5 0 8 3 8 6s-3 6-8 6c-1 0-2 0-3-.3l-4 2.3v-4C2.5 12.7 2 11.3 2 10c0-3 3-6 8-6z' },
  ];

  // Selection-mode tools. Both lasso (freehand polygon) + rect-
  // select (rectangle drag) ship; Select All / Invert are
  // one-shot ops rendered below.
  const TOOLS_SELECT: ToolEntry[] = [
    { id: 'rect-select', label: 'Rectangle select', icon: 'M4 4h16v16H4z M4 4l2 2 M20 4l-2 2 M4 20l2-2 M20 20l-2-2' },
    { id: 'lasso',       label: 'Lasso (Q)',         icon: 'M5 17c0-6 8-12 14-6s-2 12-7 9' },
  ];

  interface SelectOp { id: string; label: string; icon: string; run: () => void; }
  const SELECT_OPS: SelectOp[] = [
    { id: 'select-all',  label: 'Select all (Ctrl/⌘+A)', icon: 'M3 3h18v18H3z M7 7h10v10H7z', run: () => session.selectAll() },
    { id: 'invert-sel',  label: 'Invert selection',       icon: 'M3 3h12v12H3z M9 9h12v12H9z', run: () => session.invertSelection() },
    { id: 'deselect',    label: 'Deselect',               icon: 'M4 4h16v16H4z M2 2l20 20',     run: () => session.deselect() },
  ];

  // Image-transform tools. Crop is a mode (drag a rect to commit);
  // the rest are one-shot operations triggered by the IMAGE_OPS
  // buttons below. Keeping crop as a Tool entry (not an op button)
  // because its UX is drag-rectangle, not click-once.
  const TOOLS_IMAGE: ToolEntry[] = [
    { id: 'crop', label: 'Crop (C)', icon: 'M6 2v14h14 M2 6h14v14' },
  ];

  // One-shot image operations — clicking the button mutates the
  // doc immediately (no drag mode required). Render as
  // click-action buttons rather than tool toggles.
  interface ImageOp { id: string; label: string; icon: string; run: () => void; }
  const IMAGE_OPS: ImageOp[] = [
    { id: 'flip-h',   label: 'Flip horizontal',  icon: 'M3 6h7v12H3z M14 9h4v6h-4z M21 12l-3-3v6z', run: () => session.flipHorizontal() },
    { id: 'flip-v',   label: 'Flip vertical',    icon: 'M6 3v7h12V3z M9 14v4h6v-4z M12 21l3-3H9z', run: () => session.flipVertical() },
    { id: 'rotate-l', label: 'Rotate left 90°',  icon: 'M3 12a9 9 0 1 0 3-7 M3 3v6h6', run: () => session.rotateCounterClockwise() },
    { id: 'rotate-r', label: 'Rotate right 90°', icon: 'M21 12a9 9 0 1 1 -3-7 M21 3v6h-6', run: () => session.rotateClockwise() },
    { id: 'invert',   label: 'Invert colors',    icon: 'M12 4a8 8 0 0 0 0 16zM12 4v16a8 8 0 0 0 0-16z', run: () => session.invertColors() },
  ];

  // Canvas resize dropped in C-1.19 — the canvas is now infinite
  // (pan + zoom), so the "set source dims to N×M" affordance no
  // longer matches the mental model. source_w/h becomes a
  // rasterization frame hint only.

  // Image / Selection placeholder buttons — disabled until C-1.14 /
  // C-1.15 wire the actual mutators. They appear in their sections
  // so the user sees what's coming.
  interface PlaceholderEntry { id: string; label: string; icon: string; phase: string; }
  // Image-section placeholders still pending — skew + remove-bg.
  // Resize gets its own inline dialog (not a placeholder).
  const IMAGE_PLACEHOLDERS: PlaceholderEntry[] = [
    { id: 'skew',      label: 'Skew',              phase: 'C-1.14b', icon: 'M3 20l4-16h14L17 20z' },
    { id: 'remove-bg', label: 'Remove background', phase: 'C-1.14b (needs ML)', icon: 'M4 4h16v16H4z M2 2l20 20' },
  ];
  // All previous placeholders here either shipped (rect-select /
  // select-all / invert) or were retired (transparent selection
  // was Paint-specific lingo — we always render with a transparent
  // bbox already, so the toggle would be a no-op).
  const SELECT_PLACEHOLDERS: PlaceholderEntry[] = [];

  // Currently editing the name of which layer? Keyed by layer id.
  let editingLayerId = $state<string | null>(null);
  let editingLayerName = $state('');

  // Selection actions — only meaningful when session.selection is
  // set. The buttons are in their own section that appears between
  // tool controls and the layer panel.
  function moveSelectedToLayer(targetLayerId: string) {
    const sel = session.selection;
    if (!sel) return;
    if (sel.layerId === targetLayerId) return;
    const fromLayer = session.doc.layers.find((l) => l.id === sel.layerId);
    const toLayer = session.doc.layers.find((l) => l.id === targetLayerId);
    if (!fromLayer || !toLayer || toLayer.locked) return;
    const item = fromLayer.items[sel.index];
    if (!item) return;
    // Add to destination first, then remove from source. addItem
    // commits one history snapshot; removeItems commits another.
    // Selection re-targets the item in its new home so handles
    // stay glued.
    session.addItem(targetLayerId, item);
    session.removeItems(sel.layerId, [sel.index]);
    const newIdx = (session.doc.layers.find((l) => l.id === targetLayerId)?.items.length ?? 1) - 1;
    session.selectItem(targetLayerId, newIdx);
  }
  let showMoveMenu = $state(false);

  // Phase 1.27 — comment composer state. Pulls the current user's
  // ref from window.aaUserRef (set by the auth bootstrap); falls
  // back to 0 ("unknown author") so the post still works.
  let commentDraft = $state('');
  function currentUserRef(): number {
    return (window as { aaUserRef?: number }).aaUserRef ?? 0;
  }
  function postComment() {
    if (!session.selection) return;
    const body = commentDraft.trim();
    if (!body) return;
    session.addComment(session.selection.layerId, session.selection.index, body, currentUserRef());
    commentDraft = '';
  }
  let showColorPicker = $state(false);
  // Which color slot a palette / picker click writes into.
  //   'primary'  — session.color (Color 1)
  //   'secondary'— session.color2 (Color 2)
  //   'outline'  — selected shape's strokeColor
  //   'fill'     — selected shape's fillColor
  // User flips by clicking the slot's swatch in the Colors / Shape
  // style sections. The outline/fill targets only matter when a
  // shape is selected; otherwise we treat them as primary/secondary.
  type ColorTarget = 'primary' | 'secondary' | 'outline' | 'fill' | 'canvas';
  let colorTarget = $state<ColorTarget>('primary');
  // Canvas background quick-presets — common surfaces users reach
  // for. Custom picker handles anything else; this is just the
  // one-click row. Order matches Paint / Procreate's defaults.
  const CANVAS_PRESETS: ReadonlyArray<{ color: string; label: string }> = [
    { color: '#ffffff', label: 'White' },
    { color: '#f5f5f4', label: 'Off-white' },
    { color: '#fef9c3', label: 'Cream' },
    { color: '#1f2937', label: 'Slate' },
    { color: '#000000', label: 'Black' },
    { color: '#0c4a6e', label: 'Blueprint' },
    { color: '#052e16', label: 'Chalkboard' },
  ];
  // C-1.17 — currently-selected shape item, exposed for the Shape
  // style section (outline / fill swatches + fill toggle). null
  // when nothing's selected OR the selected item isn't a shape.
  const selectedShapeItem = $derived(() => {
    const sel = session.selection;
    if (!sel) return null;
    const layer = session.doc.layers.find((l) => l.id === sel.layerId);
    const item = layer?.items[sel.index];
    return item && item.kind === 'shape' ? { layerId: sel.layerId, index: sel.index, item } : null;
  });

  // Selected mindmap, if any — drives the Mindmap tree editor below.
  type MindmapSelection = { layerId: string; index: number; item: MindmapItem };
  const mindmapSelection = $derived(() => {
    const sel = session.selection;
    if (!sel) return null;
    const layer = session.doc.layers.find((l) => l.id === sel.layerId);
    const item = layer?.items[sel.index];
    if (!item || item.kind !== 'mindmap') return null;
    return { layerId: sel.layerId, index: sel.index, item } satisfies MindmapSelection;
  });
  // Flatten the mindmap into rows for the indented tree list.
  function flattenMindmap(item: MindmapItem): { node: MindmapNode; depth: number }[] {
    const rows: { node: MindmapNode; depth: number }[] = [];
    walkMindmap(item.root, (node, depth) => { rows.push({ node, depth }); });
    return rows;
  }
  function renameMindmapAt(sel: MindmapSelection, nodeId: string, label: string) {
    const trimmed = label.trim();
    if (!trimmed) return;
    session.replaceItem(sel.layerId, sel.index, renameMindmapNode(sel.item, nodeId, trimmed));
  }
  function addMindmapChildAt(sel: MindmapSelection, parentId: string) {
    session.replaceItem(sel.layerId, sel.index, addMindmapChild(sel.item, parentId));
  }
  function removeMindmapAt(sel: MindmapSelection, nodeId: string) {
    session.replaceItem(sel.layerId, sel.index, removeMindmapNode(sel.item, nodeId));
  }
  const activeColor = $derived.by(() => {
    if (colorTarget === 'secondary') return session.color2;
    if (colorTarget === 'outline') {
      const s = selectedShapeItem();
      return s ? (s.item.strokeColor ?? s.item.color) : session.color;
    }
    if (colorTarget === 'fill') {
      const s = selectedShapeItem();
      return s ? (s.item.fillColor ?? s.item.color) : session.color2;
    }
    if (colorTarget === 'canvas') return session.canvasColor;
    return session.color;
  });
  function setActiveColor(hex: string) {
    if (colorTarget === 'secondary') { session.color2 = hex; return; }
    if (colorTarget === 'outline') {
      const s = selectedShapeItem();
      if (s) {
        session.replaceItem(s.layerId, s.index, { ...s.item, strokeColor: hex, color: hex });
      } else {
        session.color = hex;
      }
      return;
    }
    if (colorTarget === 'fill') {
      const s = selectedShapeItem();
      if (s) {
        // Setting a fill color implies turning the fill on if it
        // wasn't already — otherwise the picker change wouldn't read
        // visually.
        session.replaceItem(s.layerId, s.index, {
          ...s.item,
          fillColor: hex,
          fill: Math.max(s.item.fill ?? 0, 1),
        });
      } else {
        session.color2 = hex;
      }
      return;
    }
    if (colorTarget === 'canvas') { session.canvasColor = hex; return; }
    session.color = hex;
  }
  // Open the color picker pre-targeted at the canvas slot so the
  // user's pick lands in session.canvasColor, not Color 1/2.
  function openCanvasColorPicker(e: MouseEvent) {
    colorTarget = 'canvas';
    openColorPicker(e);
  }
  // Outline / fill colors as currently displayed in the Shape style
  // section — pull from the selected shape when one's picked,
  // otherwise fall back to the session defaults (Color 1 + Color 2).
  const shapeOutlineColor = $derived(() => {
    const s = selectedShapeItem();
    return s ? (s.item.strokeColor ?? s.item.color) : session.color;
  });
  const shapeFillColor = $derived(() => {
    const s = selectedShapeItem();
    return s ? (s.item.fillColor ?? s.item.color) : session.color2;
  });
  const shapeHasFill = $derived(() => {
    const s = selectedShapeItem();
    return s ? (s.item.fill ?? 0) > 0 : session.fillShapes;
  });
  function toggleShapeFill() {
    const s = selectedShapeItem();
    if (s) {
      const next = (s.item.fill ?? 0) > 0 ? 0 : 1;
      session.replaceItem(s.layerId, s.index, { ...s.item, fill: next });
    } else {
      session.fillShapes = !session.fillShapes;
    }
  }
  const showShapeStyle = $derived(isShapeTool(session.tool) || !!selectedShapeItem());

  // ── Compact-mode categories ───────────────────────────────────
  // In compact mode the panel collapses to a vertical icon rail —
  // one icon per category, showing the *last-used* tool from that
  // category. Right-click on a category opens a flyout listing
  // every tool in it (single click switches). Click cycles back to
  // the category's last-used tool.
  interface ToolCategory { id: string; label: string; tools: ToolEntry[]; }
  const CATEGORIES: ToolCategory[] = $derived([
    { id: 'select',  label: 'Select',  tools: [
      { id: 'select',      label: 'Select (V)',         icon: 'M3 3l8 19 2-8 8-2z' },
      { id: 'lasso',       label: 'Lasso (Q)',          icon: 'M5 17c0-6 8-12 14-6s-2 12-7 9' },
      { id: 'rect-select', label: 'Rectangle select',   icon: 'M4 4h16v16H4z M4 4l2 2 M20 4l-2 2 M4 20l2-2 M20 20l-2-2' },
    ]},
    { id: 'brushes', label: 'Brushes', tools: [
      { id: 'pen',         label: 'Pen (P)',            icon: 'M14 4l6 6-10 10H4v-6z' },
      { id: 'marker',      label: 'Marker (M)',         icon: 'M16 2l6 6-12 12-4-4z' },
      { id: 'highlighter', label: 'Highlighter (H)',    icon: 'M9 11l-6 6v4h4l6-6 M14 4l6 6-7 7-6-6z' },
      { id: 'eraser',      label: 'Eraser (E)',         icon: 'M3 19h18 M18 13L10 5l-7 7 6 6h8z' },
    ]},
    { id: 'shapes',  label: 'Shapes',  tools: TOOLS_SHAPES },
    { id: 'text',    label: 'Text',    tools: [
      { id: 'text',        label: 'Text (T)',           icon: 'M5 5h14 M12 5v14' },
      { id: 'sticky',      label: 'Sticky note',        icon: 'M5 4h12l2 2v14H5z M19 6h-3v-2 M5 19h10' },
      { id: 'label',       label: 'Label',              icon: 'M3 7h14l4 5-4 5H3z M16 12h.01' },
    ]},
    { id: 'diagram', label: 'Diagram', tools: [
      { id: 'connector',   label: 'Connector',          icon: 'M6 18a4 4 0 0 0 4-4 M14 10a4 4 0 0 0 4-4 M10 14L14 10' },
      { id: 'frame',       label: 'Frame',              icon: 'M4 6h16v12H4z M4 9h16' },
      { id: 'mindmap',     label: 'Mindmap',            icon: 'M4 12h6 M14 8h6 M14 16h6 M10 12c0 -2 2 -4 4 -4 M10 12c0 2 2 4 4 4' },
    ]},
    { id: 'image',   label: 'Image',   tools: [
      { id: 'crop',        label: 'Crop (C)',           icon: 'M6 2v14h14 M2 6h14v14' },
      { id: 'bucket',      label: 'Fill bucket (B)',    icon: 'M19 11l-7-7-8 8 7 7z M5 19h16 M16 4l3 7' },
      { id: 'eyedropper',  label: 'Eyedropper (I)',     icon: 'M2 22l1-1h4l9-9-3-3-9 9v4z M14 7l3 3 M17 4l3 3-3 3-3-3z' },
    ]},
  ]);

  // Last-used tool per category, persisted. Records every time the
  // session tool changes so the compact rail always shows what the
  // user reached for most recently in each group.
  type LastUsedMap = Record<string, Tool>;
  const LAST_USED_KEY = 'aa.whiteboard.panel.lastUsedPerCategory';
  function loadLastUsed(): LastUsedMap {
    if (typeof localStorage === 'undefined') return {};
    try {
      const raw = localStorage.getItem(LAST_USED_KEY);
      return raw ? JSON.parse(raw) as LastUsedMap : {};
    } catch { return {}; }
  }
  let lastUsedPerCat = $state<LastUsedMap>(loadLastUsed());
  function categoryOf(t: Tool): string | null {
    for (const c of CATEGORIES) if (c.tools.some((x) => x.id === t)) return c.id;
    return null;
  }
  $effect(() => {
    const t = session.tool;
    const c = categoryOf(t);
    if (c && lastUsedPerCat[c] !== t) {
      lastUsedPerCat = { ...lastUsedPerCat, [c]: t };
      if (typeof localStorage !== 'undefined') {
        try { localStorage.setItem(LAST_USED_KEY, JSON.stringify(lastUsedPerCat)); } catch { /* quota / disabled */ }
      }
    }
  });
  function categoryIconFor(cat: ToolCategory): ToolEntry {
    const last = lastUsedPerCat[cat.id];
    if (last) {
      const found = cat.tools.find((t) => t.id === last);
      if (found) return found;
    }
    return cat.tools[0];
  }
  // Flyout state for the right-click "pick another tool in this
  // category" popup. Anchored to the icon's screen position.
  let categoryFlyout: { catId: string; x: number; y: number } | null = $state(null);
  function openCategoryFlyout(e: MouseEvent, catId: string) {
    e.preventDefault();
    const r = (e.currentTarget as HTMLElement).getBoundingClientRect();
    categoryFlyout = { catId, x: r.right + 6, y: r.top };
  }
  function pickFromFlyout(toolId: Tool) {
    session.tool = toolId;
    categoryFlyout = null;
  }

  // ── Collapsible sections (persisted) ─────────────────────────
  // Per-section open/closed state, keyed by the section id used in
  // the header. Persisted to localStorage so the user's panel
  // arrangement survives reloads (and HMR).
  type SectionId =
    | 'tools' | 'brushes' | 'stamps' | 'shapes' | 'selection' | 'image'
    | 'canvas' | 'color' | 'shape_style' | 'size' | 'typography'
    | 'selected' | 'mindmap' | 'comments' | 'layers' | 'history';
  const SECTION_STORAGE_KEY = 'aa.whiteboard.panel.sections';
  function loadSectionState(): Record<string, boolean> {
    if (typeof localStorage === 'undefined') return {};
    try {
      const raw = localStorage.getItem(SECTION_STORAGE_KEY);
      return raw ? JSON.parse(raw) as Record<string, boolean> : {};
    } catch {
      return {};
    }
  }
  // Default = open. Stored map only carries entries the user has
  // explicitly toggled, so adding a new section doesn't surprise
  // users by being closed.
  let sectionCollapsed = $state<Record<string, boolean>>(loadSectionState());
  function isCollapsed(id: SectionId): boolean {
    return sectionCollapsed[id] === true;
  }
  function toggleSection(id: SectionId) {
    sectionCollapsed = { ...sectionCollapsed, [id]: !sectionCollapsed[id] };
    try {
      localStorage.setItem(SECTION_STORAGE_KEY, JSON.stringify(sectionCollapsed));
    } catch { /* localStorage full / disabled — degrade silently */ }
  }
  // Viewport-relative coords for the color picker. Recomputed on
  // open so the dropdown sits below the swatch even when the sidebar
  // scrolls. fixed-positioned so it stacks above the canvas overlay
  // (which lives in a separate stacking context inside AssetViewer).
  let pickerLeft = $state(0);
  let pickerTop = $state(0);
  function openColorPicker(e: MouseEvent) {
    const btn = e.currentTarget as HTMLElement;
    const r = btn.getBoundingClientRect();
    // Anchor right-edge to the swatch's right edge, just below it.
    // 22rem = picker width; clamp left so it never spills off-screen.
    const pickerWidth = 22 * 16;
    pickerLeft = Math.max(8, Math.min(window.innerWidth - pickerWidth - 8, r.right - pickerWidth));
    pickerTop = Math.min(window.innerHeight - 360, r.bottom + 6);
    showColorPicker = true;
  }

  // Typography surface — visible when the text tool is active, OR
  // when a text item is the current selection. Switching the family
  // triggers a Google Fonts lazy-load so the canvas re-renders with
  // the real face as soon as the woff2 lands.
  const selectedTextItem = $derived(() => {
    const sel = session.selection;
    if (!sel) return null;
    const layer = session.doc.layers.find((l) => l.id === sel.layerId);
    const item = layer?.items[sel.index];
    return item && item.kind === 'text' ? item : null;
  });
  const showTypography = $derived(session.tool === 'text' || !!selectedTextItem());

  function pickFont(family: string) {
    session.fontFamily = family;
    void loadFont(family);
  }
  // Preload the default font + the currently-picked one so the
  // first text item renders in the right face immediately.
  $effect(() => {
    void loadFont(session.fontFamily);
  });

  function startRename(id: string, current: string) {
    editingLayerId = id;
    editingLayerName = current;
  }
  function commitRename() {
    if (editingLayerId) session.renameLayer(editingLayerId, editingLayerName.trim() || 'Untitled');
    editingLayerId = null;
  }
</script>

<div class="flex h-full min-h-0 flex-col text-fg">
  <!-- Both the "Whiteboard" title row and the in-tool compact
       toggle are retired. The shell owns both pieces of chrome:
       its header shows the tool name + the rail-toggle chevron,
       and selecting a different tool from the menubar fires the
       host's onClose. Compact mode is driven by ctx.shellState.
       paneCompact, which arrives here as the `compact` prop. -->

  {#if compact}
    <!-- Compact rail: one icon per category. Click cycles to last-
         used tool; right-click opens the flyout with every tool in
         the category. Save button at the bottom mirrors the full
         panel's bottom action. -->
    <nav class="flex shrink-0 flex-col items-center gap-1 border-b border-border bg-surface-elevated p-1">
      {#each CATEGORIES as cat (cat.id)}
        {@const icon = categoryIconFor(cat)}
        {@const active = categoryOf(session.tool) === cat.id}
        <button
          type="button"
          onclick={() => (session.tool = icon.id)}
          oncontextmenu={(e) => openCategoryFlyout(e, cat.id)}
          class="inline-flex h-10 w-10 items-center justify-center rounded transition-colors"
          class:bg-accent={active}
          class:text-on-accent={active}
          class:text-fg-muted={!active}
          class:hover:bg-state-hover={!active}
          title={`${cat.label} \u2014 ${icon.label} (right-click for more)`}
          aria-label={`${cat.label} category`}
          aria-pressed={active}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d={icon.icon} />
          </svg>
        </button>
      {/each}
    </nav>
    <div class="flex-1"></div>
    <footer class="flex shrink-0 flex-col items-center gap-1 border-t border-border bg-surface-elevated p-1">
      <button
        type="button"
        onclick={onSave}
        disabled={saving}
        class="inline-flex h-10 w-10 items-center justify-center rounded bg-accent text-on-accent hover:opacity-90 disabled:opacity-40"
        title={saving ? 'Saving\u2026' : 'Save whiteboard'}
        aria-label="Save whiteboard"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z M17 21v-8H7v8 M7 3v5h8" />
        </svg>
      </button>
    </footer>

    {#if categoryFlyout}
      <!-- Click-outside scrim closes the flyout. -->
      <div
        class="fixed inset-0 z-30"
        role="presentation"
        onpointerdown={(e) => { e.preventDefault(); categoryFlyout = null; }}
        oncontextmenu={(e) => { e.preventDefault(); categoryFlyout = null; }}
      ></div>
      {@const flyoutCat = CATEGORIES.find((c) => c.id === categoryFlyout!.catId)}
      {#if flyoutCat}
        <div
          class="fixed z-40 grid grid-cols-3 gap-1 rounded border border-border bg-surface-elevated p-2 shadow-lg"
          style:left={`${categoryFlyout.x}px`}
          style:top={`${categoryFlyout.y}px`}
          role="menu"
        >
          {#each flyoutCat.tools as t (t.id)}
            {@const active = session.tool === t.id}
            <button
              type="button"
              role="menuitem"
              onclick={() => pickFromFlyout(t.id)}
              class="inline-flex h-9 w-9 items-center justify-center rounded"
              class:bg-accent={active}
              class:text-on-accent={active}
              class:text-fg={!active}
              class:hover:bg-state-hover={!active}
              title={t.label}
              aria-label={t.label}
              aria-pressed={active}
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d={t.icon} />
              </svg>
            </button>
          {/each}
        </div>
      {/if}
    {/if}
  {:else}

  <div class="min-h-0 flex-1 overflow-y-auto">
    <!-- Soft-cap warning banner — surfaces when the doc passes
         5k items so users see the perf wall coming before it
         becomes a problem. Sticky at the top of the scroll area
         so a busy doc can't bury it. Once they hit the hard cap
         (20k), addItem refuses + logs to console; this banner is
         what tells them why. -->
    {#if session.totalItems >= ITEM_SOFT_CAP}
      <div
        class="sticky top-0 z-10 border-b border-warning/40 bg-warning/15 px-3 py-2 text-[11px] text-fg"
        role="status"
      >
        {#if session.totalItems >= ITEM_HARD_CAP}
          <span class="font-medium text-danger">Item cap reached</span> — new items won't be added until you delete some ({session.totalItems.toLocaleString()} / {ITEM_HARD_CAP.toLocaleString()}).
        {:else}
          <span class="font-medium">{session.totalItems.toLocaleString()} items</span> — performance may degrade past {ITEM_SOFT_CAP.toLocaleString()}; consider splitting or trimming.
        {/if}
      </div>
    {/if}
    <!-- ── Tools (utility / always-on) ─────────────────────────── -->
    {#snippet toolBtn(t: ToolEntry)}
      <button
        type="button"
        onclick={() => (session.tool = t.id)}
        class="inline-flex aspect-square items-center justify-center rounded transition-colors"
        class:bg-accent={session.tool === t.id}
        class:text-on-accent={session.tool === t.id}
        class:text-fg-muted={session.tool !== t.id}
        class:hover:bg-state-hover={session.tool !== t.id}
        title={t.label}
        aria-label={t.label}
        aria-pressed={session.tool === t.id}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d={t.icon} />
        </svg>
      </button>
    {/snippet}
    {#snippet placeholderBtn(p: PlaceholderEntry)}
      <button
        type="button"
        disabled
        class="inline-flex aspect-square items-center justify-center rounded text-fg-muted/40 opacity-60"
        title={`${p.label} — coming in ${p.phase}`}
        aria-label={`${p.label} (coming in ${p.phase})`}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d={p.icon} />
        </svg>
      </button>
    {/snippet}
    {#snippet sectionHeader(id: SectionId, label: string, badge?: string)}
      <!-- Click-to-collapse header. Chevron rotates 0° when open,
           -90° when closed (consistent with the rest of the app's
           details accordions). Wrapped in <button> so keyboard +
           screen-reader access work out of the box. -->
      <button
        type="button"
        onclick={() => toggleSection(id)}
        class="mb-2 flex w-full items-center justify-between gap-2 text-[11px] font-medium uppercase tracking-wide text-fg-muted/80 hover:text-fg"
        aria-expanded={!isCollapsed(id)}
        aria-controls={`wb-section-${id}`}
      >
        <span class="inline-flex items-center gap-1.5">
          <svg
            xmlns="http://www.w3.org/2000/svg"
            width="10" height="10" viewBox="0 0 24 24"
            fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"
            class="transition-transform"
            style:transform={isCollapsed(id) ? 'rotate(-90deg)' : 'rotate(0deg)'}
          >
            <polyline points="6 9 12 15 18 9"/>
          </svg>
          <span>{label}</span>
        </span>
        {#if badge}
          <span class="normal-case tracking-normal text-[10px] text-fg-muted">{badge}</span>
        {/if}
      </button>
    {/snippet}

    <section class="border-b border-border p-3">
      {@render sectionHeader('tools', 'Tools')}
      {#if !isCollapsed('tools')}
        <div id="wb-section-tools" class="grid grid-cols-5 gap-1">
          {#each TOOLS_MAIN as t (t.id)}{@render toolBtn(t)}{/each}
        </div>
        <!-- Connector routing-mode picker (Phase 1.22) — shown
             only when the connector tool is active. Three buttons:
             straight, orthogonal (one-elbow), curve (bezier with
             edge-tangent inference). New connectors pick this up
             at commit. -->
        {#if session.tool === 'connector'}
          <div class="mt-2 flex items-center gap-1">
            <span class="text-[10px] uppercase tracking-wide text-fg-muted/80">Route</span>
            {#each [
              { id: 'straight' as const,   label: 'Straight',   path: 'M4 12h16' },
              { id: 'orthogonal' as const, label: 'Orthogonal', path: 'M4 18v-6h8V6h8' },
              { id: 'curve' as const,      label: 'Curve',      path: 'M4 18c4-12 12-12 16 0' },
            ] as m (m.id)}
              {@const active = session.connectorMode === m.id}
              <button
                type="button"
                onclick={() => (session.connectorMode = m.id)}
                class="inline-flex h-6 w-8 items-center justify-center rounded border border-border"
                class:bg-accent={active}
                class:text-on-accent={active}
                class:text-fg-muted={!active}
                title={m.label}
                aria-label={`Connector routing: ${m.label}`}
                aria-pressed={active}
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <path d={m.path} />
                </svg>
              </button>
            {/each}
          </div>
        {/if}
      {/if}
    </section>

    <!-- ── Brushes ───────────────────────────────────────────────
         The three top-level brush *tools*: pen, marker, highlighter.
         Each has its own stroke math (taper, opacity blend mode,
         pressure handling) so they're not interchangeable styles —
         they're separate tools. For variety beyond the basics,
         users reach for the Stamps section below (built-ins plus
         imported .abr packs). -->
    <section class="border-b border-border p-3">
      {@render sectionHeader('brushes', 'Brushes')}
      {#if !isCollapsed('brushes')}
        <!-- 3 brush tools laid out as icon-plus-label tiles so the
             user can identify pen / marker / highlighter at a glance
             instead of guessing from icons. -->
        <div id="wb-section-brushes" class="grid grid-cols-3 gap-1">
          {#each TOOLS_BRUSHES as t (t.id)}
            {@const active = session.tool === t.id}
            <button
              type="button"
              onclick={() => (session.tool = t.id)}
              class="flex flex-col items-center justify-center gap-1 rounded p-2 transition-colors"
              class:bg-accent={active}
              class:text-on-accent={active}
              class:text-fg-muted={!active}
              class:hover:bg-state-hover={!active}
              title={t.label}
              aria-label={t.label}
              aria-pressed={active}
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d={t.icon} />
              </svg>
              <span class="text-[10px] leading-tight">{t.label.replace(/ \([A-Z]\)$/, '')}</span>
            </button>
          {/each}
        </div>
      {/if}
    </section>

    <!-- ── Stamps ────────────────────────────────────────────────
         Stamp-based brushes (Phase 1.21b/d). Built-ins ship with
         soft + hard round; users import additional .abr packs via
         the Import button. Each pack header strip carries a delete
         button (built-ins hide for the session only). Clicking a
         stamp sets session.stampId + auto-switches tool=pen. -->
    <section class="border-b border-border p-3">
      {@render sectionHeader('stamps', 'Stamps')}
      {#if !isCollapsed('stamps')}
        <div id="wb-section-stamps">
          <div class="flex items-center justify-between">
            <span class="text-[10px] uppercase tracking-wide text-fg-muted/70">Brush packs</span>
            <button
              type="button"
              onclick={() => importInput?.click()}
              disabled={importBusy}
              class="inline-flex h-6 items-center rounded border border-border px-2 text-[10px] text-fg hover:border-fg-muted disabled:opacity-50"
              title="Import a Photoshop .abr brush pack"
            >
              {importBusy ? 'Importing\u2026' : 'Import .abr\u2026'}
            </button>
            <input
              bind:this={importInput}
              type="file"
              accept=".abr,application/octet-stream"
              class="hidden"
              onchange={onImportFiles}
            />
          </div>
          {#if importError}
            <div class="mt-1 rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">
              {importError}
            </div>
          {/if}
        {#each installedPacks as pack (pack.id)}
          <div class="mt-2 flex items-center justify-between">
            <span class="text-[10px] uppercase tracking-wide text-fg-muted/70">
              {pack.name}
            </span>
            <button
              type="button"
              onclick={() => deletePack(pack.id)}
              class="text-[10px] text-fg-muted hover:text-danger"
              title={pack.id === 'builtin' ? 'Hide built-in stamps for this session' : 'Delete brush pack'}
            >×</button>
          </div>
          <div class="mt-1 grid grid-cols-5 gap-1">
            {#each pack.stamps as stamp (stamp.id)}
              {@const active = session.stampId === stamp.id}
              <button
                type="button"
                onclick={() => {
                  // Stamps only render on the pen tool — clicking a
                  // stamp without auto-switching left users staring at
                  // identical-looking marker / highlighter output. Flip
                  // to pen and assign in one step.
                  if (active) {
                    session.stampId = null;
                  } else {
                    session.tool = 'pen';
                    session.stampId = stamp.id;
                  }
                }}
                class="inline-flex aspect-square items-center justify-center rounded border border-border bg-surface transition-colors"
                class:border-accent={active}
                class:ring-1={active}
                class:ring-accent={active}
                class:hover:border-fg-muted={!active}
                title={`${stamp.label} \u2014 stamp brush (${pack.name})`}
                aria-label={stamp.label}
                aria-pressed={active}
              >
                {#if typeof stamp.source !== 'string'}
                  <!-- Preview the alpha-mask stamp as-is on a
                       white sub-tile inside the dark button so it
                       reads regardless of theme. Procedurally
                       generated stamps are white-alpha-on-
                       transparent, so they pop on white. -->
                  <span class="inline-flex h-6 w-6 items-center justify-center rounded-sm bg-white">
                    <img
                      src={stamp.source.src}
                      alt={stamp.label}
                      class="h-5 w-5 object-contain"
                      style:filter="invert(1)"
                    />
                  </span>
                {:else}
                  <!-- URL-sourced: source becomes an Image after the
                       preloader fires (registerPackFromAPI). Until
                       then, show a tiny placeholder so the layout
                       doesn't jump. -->
                  <span class="text-[9px] text-fg-muted">…</span>
                {/if}
              </button>
            {/each}
          </div>
        {/each}
        </div>
      {/if}
    </section>

    <!-- ── Shapes ──────────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      {@render sectionHeader('shapes', 'Shapes')}
      {#if !isCollapsed('shapes')}
        <div id="wb-section-shapes" class="grid grid-cols-5 gap-1">
          {#each TOOLS_SHAPES as t (t.id)}{@render toolBtn(t)}{/each}
        </div>
      {/if}
    </section>

    <!-- ── Selection ───────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      {@render sectionHeader('selection', 'Selection')}
      {#if !isCollapsed('selection')}
      <div id="wb-section-selection" class="grid grid-cols-5 gap-1">
        {#each TOOLS_SELECT as t (t.id)}{@render toolBtn(t)}{/each}
        {#each SELECT_OPS as op (op.id)}
          <button
            type="button"
            onclick={op.run}
            class="inline-flex aspect-square items-center justify-center rounded text-fg-muted hover:bg-state-hover hover:text-fg"
            title={op.label}
            aria-label={op.label}
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d={op.icon} />
            </svg>
          </button>
        {/each}
      </div>
      {/if}
    </section>

    <!-- ── Image ───────────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      {@render sectionHeader('image', 'Image')}
      {#if !isCollapsed('image')}
      <div id="wb-section-image" class="grid grid-cols-5 gap-1">
        {#each TOOLS_IMAGE as t (t.id)}{@render toolBtn(t)}{/each}
        {#each IMAGE_OPS as op (op.id)}
          <button
            type="button"
            onclick={op.run}
            class="inline-flex aspect-square items-center justify-center rounded text-fg-muted hover:bg-state-hover hover:text-fg"
            title={op.label}
            aria-label={op.label}
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d={op.icon} />
            </svg>
          </button>
        {/each}
        {#each IMAGE_PLACEHOLDERS as p (p.id)}{@render placeholderBtn(p)}{/each}
      </div>
      {/if}
    </section>

    <!-- ── Canvas ──────────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      {@render sectionHeader('canvas', 'Canvas')}
      {#if !isCollapsed('canvas')}
        <div class="flex items-center gap-3">
          <button
            type="button"
            onclick={openCanvasColorPicker}
            class="h-9 w-9 rounded border-2 border-border hover:border-accent"
            style:background-color={session.canvasColor}
            title={`Canvas color — ${session.canvasColor}`}
            aria-label="Pick canvas background color"
          ></button>
          <div class="flex flex-wrap gap-1">
            {#each CANVAS_PRESETS as p (p.color)}
              {@const active = session.canvasColor.toLowerCase() === p.color.toLowerCase()}
              <button
                type="button"
                onclick={() => (session.canvasColor = p.color)}
                class="h-6 w-6 rounded ring-1 ring-border hover:ring-accent"
                class:ring-2={active}
                class:ring-accent={active}
                style:background-color={p.color}
                title={p.label}
                aria-label={p.label}
              ></button>
            {/each}
          </div>
        </div>
      {/if}
    </section>

    <!-- ── Color ────────────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      {@render sectionHeader('color', 'Color', 'Right-click = #2 · X swaps')}
      {#if !isCollapsed('color')}
      <!-- Color 1 / Color 2 swatches + swap arrow. Click the
           swatch to make it the "target" — clicking any palette
           color or the custom picker writes into the targeted slot. -->
      <div class="mb-2 flex items-center gap-2">
        <div class="flex items-center gap-1">
          <button
            type="button"
            onclick={() => (colorTarget = 'primary')}
            class="h-9 w-9 rounded border-2"
            class:border-accent={colorTarget === 'primary'}
            class:border-border={colorTarget !== 'primary'}
            style:background-color={session.color}
            title={`Color 1 (primary) — ${session.color}`}
            aria-label="Select primary color slot"
            aria-pressed={colorTarget === 'primary'}
          ></button>
          <button
            type="button"
            onclick={() => (colorTarget = 'secondary')}
            class="h-9 w-9 rounded border-2"
            class:border-accent={colorTarget === 'secondary'}
            class:border-border={colorTarget !== 'secondary'}
            style:background-color={session.color2}
            title={`Color 2 (secondary) — ${session.color2}`}
            aria-label="Select secondary color slot"
            aria-pressed={colorTarget === 'secondary'}
          ></button>
          <button
            type="button"
            onclick={() => session.swapColors()}
            class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-state-hover hover:text-fg"
            title="Swap colors (X)"
            aria-label="Swap primary and secondary colors"
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M16 3l4 4-4 4 M20 7H8 M8 21l-4-4 4-4 M4 17h12" />
            </svg>
          </button>
        </div>
      </div>
      <div class="flex flex-wrap items-center gap-1.5">
        {#each PALETTE as c (c)}
          <button
            type="button"
            onclick={() => setActiveColor(c)}
            class="h-6 w-6 rounded-full ring-1 ring-border transition-transform hover:scale-110"
            class:ring-2={activeColor === c}
            class:ring-accent={activeColor === c}
            class:scale-110={activeColor === c}
            style:background-color={c}
            title={c}
            aria-label={`Color ${c}`}
            aria-pressed={activeColor === c}
          ></button>
        {/each}
        <!-- Custom color picker — clicking the swatch opens a
             dropdown with HSV field + hue slider + RGB/Hex inputs
             + EyeDropper API + recent-colors strip. Positioned
             with position:fixed so the picker stacks above the
             whiteboard canvas overlay (which sits in its own
             stacking context inside AssetViewer). -->
        <button
          type="button"
          onclick={openColorPicker}
          class="inline-flex h-6 w-6 items-center justify-center rounded border border-border ring-1 ring-fg-muted/30 hover:ring-accent"
          style:background-color={activeColor}
          title={`Custom color picker (writes to ${colorTarget === 'secondary' ? 'Color 2' : colorTarget === 'outline' ? 'Outline' : colorTarget === 'fill' ? 'Fill' : 'Color 1'})`}
          aria-label="Open custom color picker"
          aria-expanded={showColorPicker}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke={session.color === '#ffffff' || session.color === '#0f172a' ? 'currentColor' : 'white'} stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
            <polyline points="6 9 12 15 18 9" />
          </svg>
        </button>
      </div>
      {/if}
    </section>

    <!-- ── Shape style ───────────────────────────────────────────
         Outline + fill color slots, independent of Color 1/2.
         Visible whenever a shape tool is active OR a shape item is
         selected. For a selected shape, swatches read & write the
         item's own strokeColor / fillColor; otherwise they preview
         the defaults that get applied when a new shape is dragged
         out (left-drag → Color 1 outline + Color 2 fill, right-
         drag swaps). Clicking a swatch makes it the target slot
         for the palette + custom color picker. -->
    {#if showShapeStyle}
      <section class="space-y-2 border-b border-border p-3">
        {@render sectionHeader('shape_style', 'Shape style', selectedShapeItem() ? 'editing selection' : 'defaults for new shapes')}
        {#if !isCollapsed('shape_style')}
        <div class="flex items-center gap-3">
          <!-- Outline swatch -->
          <button
            type="button"
            onclick={() => (colorTarget = 'outline')}
            class="flex flex-col items-center gap-1"
            title="Click to make outline the color-picker target"
            aria-pressed={colorTarget === 'outline'}
          >
            <div class="relative h-9 w-9 rounded border-2"
              class:border-accent={colorTarget === 'outline'}
              class:border-border={colorTarget !== 'outline'}
            >
              <!-- Outline preview = ring of color over a white core. -->
              <div class="absolute inset-0 rounded-sm" style:background-color={shapeOutlineColor()}></div>
              <div class="absolute inset-1.5 rounded-sm bg-white"></div>
            </div>
            <span class="text-[10px] text-fg-muted">Outline</span>
          </button>
          <!-- Fill swatch -->
          <button
            type="button"
            onclick={() => (colorTarget = 'fill')}
            class="flex flex-col items-center gap-1"
            title="Click to make fill the color-picker target"
            aria-pressed={colorTarget === 'fill'}
          >
            <div class="relative h-9 w-9 rounded border-2"
              class:border-accent={colorTarget === 'fill'}
              class:border-border={colorTarget !== 'fill'}
            >
              <div class="absolute inset-0 rounded-sm" style:background-color={shapeFillColor()}></div>
              {#if !shapeHasFill()}
                <!-- Visual "no fill" hash through the swatch. -->
                <svg xmlns="http://www.w3.org/2000/svg" class="absolute inset-0 h-full w-full" viewBox="0 0 24 24" preserveAspectRatio="none">
                  <line x1="3" y1="21" x2="21" y2="3" stroke="#ef4444" stroke-width="2.5" />
                </svg>
              {/if}
            </div>
            <span class="text-[10px] text-fg-muted">Fill</span>
          </button>
          <div class="flex-1"></div>
          <label class="inline-flex items-center gap-1 text-xs">
            <input
              type="checkbox"
              checked={shapeHasFill()}
              onchange={toggleShapeFill}
              class="accent-accent"
            />
            <span>Filled</span>
          </label>
        </div>
        {#if !selectedShapeItem()}
          <p class="text-[10px] text-fg-muted">
            Drag = outline Color 1, fill Color 2. Right-drag swaps.
          </p>
        {/if}
        {/if}
      </section>
    {/if}

    <!-- ── Size + Opacity ──────────────────────────────────────── -->
    <section class="space-y-3 border-b border-border p-3">
      {@render sectionHeader('size', 'Size & opacity')}
      {#if !isCollapsed('size')}
      <div>
        <div class="mb-1 flex items-center justify-between text-[10px] text-fg-muted">
          <span>Brush size</span>
          <span class="font-mono text-fg">{session.width}px</span>
        </div>
        <input
          type="range"
          min={1} max={48} step={1}
          value={session.width}
          oninput={(e) => (session.width = +(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
          aria-label="Brush size"
        />
        <div class="mt-1 flex justify-between gap-1">
          {#each SIZES as s (s.label)}
            <button
              type="button"
              onclick={() => (session.width = s.width)}
              class="flex-1 rounded border border-border px-1 py-0.5 text-[10px] hover:border-fg-muted/60"
              class:border-accent={session.width === s.width}
              class:text-accent={session.width === s.width}
              title={`${s.label} (${s.width}px)`}
            >{s.label}</button>
          {/each}
        </div>
      </div>
      <div>
        <div class="mb-1 flex items-center justify-between text-[10px] text-fg-muted">
          <span>Opacity</span>
          <span class="font-mono text-fg">{Math.round(session.opacity * 100)}%</span>
        </div>
        <input
          type="range"
          min={0} max={1} step={0.05}
          value={session.opacity}
          oninput={(e) => (session.opacity = +(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
          aria-label="Opacity"
        />
      </div>
      {/if}
    </section>

    <!-- ── Typography ───────────────────────────────────────────
         Visible whenever the text tool is active OR a text item is
         selected. Writes through to the selected text item so users
         can restyle existing labels without re-typing them. -->
    {#if showTypography}
      <section class="space-y-3 border-b border-border p-3">
        {@render sectionHeader('typography', 'Typography')}
        {#if !isCollapsed('typography')}
        <!-- Font family -->
        <label class="block">
          <span class="mb-1 block text-[10px] text-fg-muted">Font</span>
          <select
            value={session.fontFamily}
            onchange={(e) => pickFont((e.currentTarget as HTMLSelectElement).value)}
            class="w-full rounded border border-border bg-surface px-2 py-1 text-xs"
          >
            {#each GOOGLE_FONTS as f (f.family)}
              <option value={f.family} style:font-family={`"${f.family}", system-ui`}>
                {f.label}
              </option>
            {/each}
          </select>
        </label>
        <!-- Live preview of the picked font at the current size +
             weight + style. Renders the font name so users see the
             face apply before they create a text item. -->
        <div
          class="overflow-hidden rounded border border-border bg-white px-2 py-1 text-black"
          style:font-family={`"${session.fontFamily}", system-ui, sans-serif`}
          style:font-weight={session.bold ? 700 : 400}
          style:font-style={session.italic ? 'italic' : 'normal'}
          style:font-size={`${Math.min(28, session.fontSize)}px`}
        >
          {session.fontFamily}
        </div>
        <!-- Size slider 8-96 with preset chips. -->
        <div>
          <div class="mb-1 flex items-center justify-between text-[10px] text-fg-muted">
            <span>Size</span>
            <span class="font-mono text-fg">{session.fontSize}px</span>
          </div>
          <input
            type="range"
            min={FONT_SIZE_MIN}
            max={FONT_SIZE_MAX}
            step={1}
            value={session.fontSize}
            oninput={(e) => (session.fontSize = +(e.currentTarget as HTMLInputElement).value)}
            class="w-full accent-accent"
          />
          <div class="mt-1 flex flex-wrap gap-1">
            {#each FONT_SIZE_PRESETS as s (s)}
              <button
                type="button"
                onclick={() => (session.fontSize = s)}
                class="rounded border border-border px-1.5 py-0.5 text-[10px] hover:border-fg-muted/60"
                class:border-accent={session.fontSize === s}
                class:text-accent={session.fontSize === s}
              >{s}</button>
            {/each}
          </div>
        </div>
        <!-- Weight / style / align toggles. Native buttons; bold +
             italic write through to the selection. Align is
             three-state pill. -->
        <div class="flex flex-wrap items-center gap-1">
          <button
            type="button"
            onclick={() => (session.bold = !session.bold)}
            class="inline-flex h-7 w-7 items-center justify-center rounded border border-border text-xs"
            class:border-accent={session.bold}
            class:bg-accent={session.bold}
            class:text-on-accent={session.bold}
            title="Bold"
            aria-pressed={session.bold}
          ><span class="font-bold">B</span></button>
          <button
            type="button"
            onclick={() => (session.italic = !session.italic)}
            class="inline-flex h-7 w-7 items-center justify-center rounded border border-border text-xs"
            class:border-accent={session.italic}
            class:bg-accent={session.italic}
            class:text-on-accent={session.italic}
            title="Italic"
            aria-pressed={session.italic}
          ><span class="italic">I</span></button>
          <span class="mx-1 h-5 w-px bg-border"></span>
          {#each [
            { id: 'left' as const,   path: 'M3 5h18 M3 12h12 M3 19h18' },
            { id: 'center' as const, path: 'M3 5h18 M6 12h12 M3 19h18' },
            { id: 'right' as const,  path: 'M3 5h18 M9 12h12 M3 19h18' },
          ] as a (a.id)}
            <button
              type="button"
              onclick={() => (session.textAlign = a.id)}
              class="inline-flex h-7 w-7 items-center justify-center rounded border border-border"
              class:border-accent={session.textAlign === a.id}
              class:bg-accent={session.textAlign === a.id}
              class:text-on-accent={session.textAlign === a.id}
              title={`Align ${a.id}`}
              aria-pressed={session.textAlign === a.id}
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d={a.path} /></svg>
            </button>
          {/each}
        </div>
        {/if}
      </section>
    {/if}

    <!-- ── Selection actions ────────────────────────────────────
         Only meaningful when an item is picked (select tool + click).
         When nothing's selected we hint at the workflow so users
         find the tool. -->
    {#if session.selection}
      <section class="border-b border-border p-3">
        {@render sectionHeader('selected', 'Selected item')}
        {#if !isCollapsed('selected')}
        <div class="mb-2 text-[10px] text-fg-muted">
          Drag = move · handles = resize / rotate · Delete · Ctrl/⌘ C / X / V.
        </div>
        <div class="flex flex-wrap gap-1">
          <button
            type="button"
            onclick={() => session.removeItems(session.selection!.layerId, [session.selection!.index])}
            class="inline-flex h-7 items-center rounded border border-border px-2 text-xs text-fg hover:border-danger hover:text-danger"
            title="Delete (Del / Backspace)"
          >Delete</button>
          <div class="relative">
            <button
              type="button"
              onclick={() => (showMoveMenu = !showMoveMenu)}
              class="inline-flex h-7 items-center rounded border border-border px-2 text-xs text-fg hover:border-fg-muted/60"
              title="Move selected item to another layer"
            >Move to layer ▾</button>
            {#if showMoveMenu}
              <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
              <div
                class="absolute left-0 top-full z-50 mt-1 min-w-[10rem] rounded border border-border bg-surface-elevated shadow-lg"
                onclick={() => (showMoveMenu = false)}
              >
                {#each session.doc.layers as l (l.id)}
                  <button
                    type="button"
                    onclick={() => moveSelectedToLayer(l.id)}
                    disabled={l.id === session.selection!.layerId || l.locked}
                    class="block w-full truncate px-3 py-1.5 text-left text-xs text-fg hover:bg-state-hover disabled:opacity-40 disabled:hover:bg-transparent"
                  >
                    {l.name || 'Untitled'}{l.id === session.selection!.layerId ? '  ✓' : ''}{l.locked ? '  🔒' : ''}
                  </button>
                {/each}
              </div>
            {/if}
          </div>
        </div>
        {/if}
      </section>
    {/if}

    <!-- ── Mindmap tree editor ─────────────────────────────────
         Shown only when a mindmap item is selected. Lists every
         node as an indented row with: rename (click), add child
         (+), delete (trash). Double-click on the canvas also
         opens an inline rename overlay on the clicked bubble. -->
    {#if mindmapSelection()}
      {@const sel = mindmapSelection()!}
      <section class="border-b border-border p-3">
        {@render sectionHeader('mindmap', 'Mindmap')}
        {#if !isCollapsed('mindmap')}
          <div class="mb-2 text-[10px] text-fg-muted">
            Edit each node's label inline below. Use + to add a child, × to delete (root protected).
          </div>
          <ul class="space-y-0.5 text-[11px]">
            {#each flattenMindmap(sel.item) as row (row.node.id)}
              <li
                class="flex items-center gap-1 rounded px-1 py-0.5 hover:bg-state-hover"
                style:padding-left={`${row.depth * 12 + 4}px`}
              >
                <input
                  type="text"
                  value={row.node.label}
                  onchange={(e) => renameMindmapAt(sel, row.node.id, (e.currentTarget as HTMLInputElement).value)}
                  class="min-w-0 flex-1 rounded bg-transparent px-1 py-0.5 text-fg focus:bg-surface focus:outline-none focus:ring-1 focus:ring-accent"
                />
                <button
                  type="button"
                  onclick={() => addMindmapChildAt(sel, row.node.id)}
                  class="inline-flex h-5 w-5 items-center justify-center rounded text-fg-muted hover:bg-state-hover hover:text-fg"
                  title="Add child"
                  aria-label="Add child node"
                >+</button>
                {#if row.node.id !== sel.item.root.id}
                  <button
                    type="button"
                    onclick={() => removeMindmapAt(sel, row.node.id)}
                    class="inline-flex h-5 w-5 items-center justify-center rounded text-fg-muted hover:bg-state-hover hover:text-danger"
                    title="Delete node + subtree"
                    aria-label="Delete node"
                  >×</button>
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
      </section>
    {/if}

    <!-- ── Comments on selected element (Phase 1.27) ───────────
         Element-typed pattern: comments live on individual canvas
         elements. Shown only when an item is selected. Same
         engine will power asset annotations in the future
         (annotation surface = same shape with an additional
         doc.anchor = {assetId, frameRange} field). -->
    {#if session.selection}
      <section class="border-b border-border p-3">
        {@render sectionHeader('comments', 'Comments')}
        {#if !isCollapsed('comments')}
          {@const threads = session.commentsForItem(session.selection.layerId, session.selection.index)}
          {#if threads.length === 0}
            <div class="mb-2 text-[10px] text-fg-muted">No comments yet.</div>
          {/if}
          {#each threads as t (t.id)}
            <div class="mb-2 rounded border border-border p-2 text-[11px]"
              class:opacity-60={t.resolved}
            >
              {#each t.messages as msg (msg.id)}
                <div class="mb-1.5">
                  <div class="mb-0.5 flex items-center gap-2 text-[10px] text-fg-muted">
                    <span class="font-medium">@{msg.author_ref}</span>
                    <span>{new Date(msg.created_at).toLocaleString()}</span>
                    <button
                      type="button"
                      class="ml-auto text-fg-muted hover:text-danger"
                      title="Delete message"
                      aria-label="Delete message"
                      onclick={() => session.deleteComment(t.id, msg.id)}
                    >×</button>
                  </div>
                  <div class="whitespace-pre-wrap text-fg">{msg.body}</div>
                </div>
              {/each}
              <div class="mt-1 flex justify-end">
                <button
                  type="button"
                  onclick={() => session.setCommentResolved(t.id, !t.resolved)}
                  class="text-[10px] text-fg-muted hover:text-fg"
                >
                  {t.resolved ? 'Reopen' : 'Resolve'}
                </button>
              </div>
            </div>
          {/each}
          <!-- New-comment input. Author ref pulled from
               window.aaUserRef (populated by the auth-bootstrap
               hook); falls back to 0 if missing so the comment
               still records but with an "unknown" attribution
               that the rendering layer can flag. -->
          <textarea
            bind:value={commentDraft}
            placeholder="Add a comment\u2026"
            class="mb-1 block w-full rounded border border-border bg-surface px-2 py-1 text-xs"
            rows="2"
          ></textarea>
          <button
            type="button"
            onclick={postComment}
            disabled={!commentDraft.trim()}
            class="inline-flex h-7 items-center rounded bg-accent px-2 text-xs font-medium text-on-accent disabled:opacity-40"
          >Post</button>
        {/if}
      </section>
    {/if}

    <!-- ── Layers ──────────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      <div class="mb-2 flex items-center justify-between">
        {@render sectionHeader('layers', 'Layers')}
        <button
          type="button"
          onclick={() => session.addLayer()}
          class="-mt-2 inline-flex h-5 w-5 items-center justify-center rounded text-fg-muted hover:bg-state-hover hover:text-fg"
          title="Add layer"
          aria-label="Add layer"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
        </button>
      </div>
      {#if !isCollapsed('layers')}
      <!-- Render newest-on-top to match Photoshop / Procreate's
           layer panel orientation (rendering order is reversed). -->
      <ul class="space-y-1">
        {#each [...session.doc.layers].reverse() as layer (layer.id)}
          {@const isActive = session.activeLayerId === layer.id}
          <li
            class="group rounded border p-1.5 transition-colors"
            class:border-accent={isActive}
            class:bg-state-selected={isActive}
            class:border-border={!isActive}
            class:hover:bg-state-hover={!isActive}
          >
            <div class="flex items-center gap-1.5">
              <!-- Visibility toggle -->
              <button
                type="button"
                onclick={() => session.setLayerVisible(layer.id, !layer.visible)}
                class="inline-flex h-5 w-5 shrink-0 items-center justify-center rounded text-fg-muted hover:text-fg"
                title={layer.visible ? 'Hide layer' : 'Show layer'}
                aria-pressed={layer.visible}
              >
                {#if layer.visible}
                  <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>
                {:else}
                  <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/></svg>
                {/if}
              </button>
              <!-- Name (click to activate; dbl-click to rename) -->
              {#if editingLayerId === layer.id}
                <input
                  type="text"
                  bind:value={editingLayerName}
                  onblur={commitRename}
                  onkeydown={(e) => { if (e.key === 'Enter') commitRename(); if (e.key === 'Escape') editingLayerId = null; }}
                  class="min-w-0 flex-1 rounded border border-border bg-surface px-1 text-xs"
                  autofocus
                />
              {:else}
                <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
                <span
                  onclick={() => (session.activeLayerId = layer.id)}
                  ondblclick={() => startRename(layer.id, layer.name ?? '')}
                  class="min-w-0 flex-1 cursor-pointer truncate text-xs"
                  title="Click to activate · double-click to rename"
                >
                  {layer.name || 'Untitled'}
                </span>
              {/if}
              <!-- Reorder -->
              <button
                type="button"
                onclick={() => session.moveLayer(layer.id, 'up')}
                class="opacity-0 group-hover:opacity-100 text-fg-muted hover:text-fg"
                title="Move layer up"
                aria-label="Move layer up"
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="18 15 12 9 6 15"/></svg>
              </button>
              <button
                type="button"
                onclick={() => session.moveLayer(layer.id, 'down')}
                class="opacity-0 group-hover:opacity-100 text-fg-muted hover:text-fg"
                title="Move layer down"
                aria-label="Move layer down"
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
              </button>
              <!-- Lock -->
              <button
                type="button"
                onclick={() => session.setLayerLocked(layer.id, !layer.locked)}
                class="text-fg-muted hover:text-fg"
                class:text-warning={layer.locked}
                title={layer.locked ? 'Unlock layer' : 'Lock layer'}
                aria-pressed={layer.locked}
              >
                {#if layer.locked}
                  <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
                {:else}
                  <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 9.9-1"/></svg>
                {/if}
              </button>
              <!-- Delete -->
              {#if session.doc.layers.length > 1}
                <button
                  type="button"
                  onclick={() => session.removeLayer(layer.id)}
                  class="opacity-0 group-hover:opacity-100 text-fg-muted hover:text-danger"
                  title="Delete layer"
                  aria-label="Delete layer"
                >
                  <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/></svg>
                </button>
              {/if}
            </div>
            <!-- Per-layer opacity (compact slider; only shown for the
                 active layer to keep the panel tight). -->
            {#if isActive}
              <div class="mt-1.5 flex items-center gap-2">
                <span class="text-[10px] text-fg-muted">α</span>
                <input
                  type="range"
                  min={0} max={1} step={0.05}
                  value={layer.opacity}
                  oninput={(e) => session.setLayerOpacity(layer.id, +(e.currentTarget as HTMLInputElement).value)}
                  class="min-w-0 flex-1 accent-accent"
                  aria-label="Layer opacity"
                />
                <span class="w-8 text-right font-mono text-[10px] text-fg-muted">{Math.round(layer.opacity * 100)}%</span>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
      {/if}
    </section>

    <!-- ── History + Clear ─────────────────────────────────────── -->
    <section class="flex items-center justify-between border-b border-border p-3">
      <div class="flex gap-1">
        <button
          type="button"
          onclick={() => session.undo()}
          disabled={!session.canUndo}
          class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-state-hover disabled:opacity-30 disabled:hover:bg-transparent"
          title="Undo (Ctrl/⌘+Z)"
          aria-label="Undo"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 7 3 13 9 13"/><path d="M3 13a9 9 0 1 0 3-7"/></svg>
        </button>
        <button
          type="button"
          onclick={() => session.redo()}
          disabled={!session.canRedo}
          class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-state-hover disabled:opacity-30 disabled:hover:bg-transparent"
          title="Redo (Ctrl/⌘+Shift+Z)"
          aria-label="Redo"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="21 7 21 13 15 13"/><path d="M21 13a9 9 0 1 1 -3-7"/></svg>
        </button>
      </div>
      <button
        type="button"
        onclick={() => { if (confirm('Clear all strokes on this whiteboard?')) session.clearAll(); }}
        class="inline-flex h-7 items-center rounded px-2 text-xs text-fg-muted hover:bg-state-hover"
        title="Clear all strokes"
      >
        Clear all
      </button>
    </section>

    <!-- Tips moved out of the panel in C-1.18 — they now live in
         the AssetPlaylist's bottom hotkeys accordion ("Tips" when
         a host extends it) and switch contents based on the active
         tool, so the panel itself stays focused on controls. -->
  </div>

  <!-- Color picker — rendered as a top-level fixed-positioned
       overlay so its stacking context isn't trapped by the
       sidebar's scroll container. Backdrop catches outside-clicks
       to close; both layers use very high z-index so the picker
       sits above the whiteboard canvas overlay (z-25) and the
       Tools dropdown menu portal. -->
  {#if showColorPicker}
    <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
    <div
      class="fixed inset-0 z-[1000]"
      onclick={() => (showColorPicker = false)}
    ></div>
    <div
      class="fixed z-[1001]"
      style:left={`${pickerLeft}px`}
      style:top={`${pickerTop}px`}
    >
      <ColorPicker
        value={activeColor}
        oninput={(hex) => setActiveColor(hex)}
        onclose={() => (showColorPicker = false)}
      />
    </div>
  {/if}

  <!-- ── Save / cancel — sticky footer (full mode only; compact
       mode has its own icon-only save footer above). -->
  <footer class="flex shrink-0 items-center gap-2 border-t border-border bg-surface-elevated px-3 py-2">
    <span class="flex-1 truncate text-xs text-danger">{saveError ?? ''}</span>
    <button
      type="button"
      onclick={onClose}
      class="inline-flex h-7 items-center rounded border border-border px-3 text-xs text-fg hover:bg-state-hover"
    >
      Cancel
    </button>
    <button
      type="button"
      onclick={onSave}
      disabled={saving}
      class="inline-flex h-7 items-center rounded bg-accent px-3 text-xs font-medium text-on-accent transition-colors hover:opacity-90 disabled:opacity-50"
    >
      {saving ? 'Saving…' : 'Save'}
    </button>
  </footer>
  {/if}
</div>
