// Whiteboard / annotation data model — vector source of truth.
//
// Mirrors the WhiteboardContent / WhiteboardCreate schemas in
// app/api/openapi.yaml.
//
// The same shape powers both:
//   - Whiteboards (target = post, no frame anchor)
//   - Frame annotations (target = asset, video_frame anchor) — phase C-2
//
// Coordinates are stored in source-canvas pixel space (source_w ×
// source_h captured at save time). Rendering scales to viewport.
//
// Items are polymorphic — a layer carries `items[]` rather than just
// `strokes[]` so the same renderer + undo stack + storage carries
// brush strokes, geometric shapes, text labels, and pasted images.
// This is the "Photoshop-lite" extension; the OpenAPI WhiteboardItem
// schema is a discriminated union keyed on `kind`.

/** Single pointer sample. [x, y, pressure?] — pressure 0..1 when the
 *  device reports it (stylus); the engine simulates from velocity
 *  otherwise. perfect-freehand consumes this directly. */
export type Point = [number, number, number?];

/** Brush tools (free-form pointer input). */
export type BrushTool = 'pen' | 'marker' | 'highlighter' | 'eraser';

/** Sub-style for brush strokes. Pen ships variations within the same
 *  tool (`pen` + brushStyle='calligraphy' etc.) rather than adding
 *  more top-level Tool enum entries — keeps the tool count bounded
 *  while letting the user pick from a Photoshop / Paint -style brush
 *  picker. Each style maps to a perfect-freehand parameter preset
 *  (see `strokeOptionsFor`) + optional render-time effects
 *  (airbrush scatter dots, crayon noise overlay, etc).
 *
 *  Eraser + highlighter ignore brushStyle — their stroke math is
 *  fixed (destination-out / flat low-flow).
 */
export type BrushStyle =
  | 'default'      // Pen as it draws today — pressure-responsive smooth
  | 'calligraphy'  // Velocity-modulated thick taper, oblique-feel
  | 'pen-tip'      // Thin steady-width fountain pen
  | 'pencil'       // Fine, low-pressure, high-contrast edges
  | 'airbrush'     // Soft-edge spray; scatter dots on top of the stroke
  | 'oil'          // Heavy paint with smooth taper
  | 'crayon'       // Textured stroke via noise overlay
  | 'watercolor';  // Soft, semi-transparent buildup

/** Shape tools (click-drag-release rectangles defining the shape).
 *  Most are parametric off the (x, y, w, h) bbox; star + polygon
 *  carry a `points` count saved per-item so re-render matches what
 *  the user picked. */
export type ShapeTool =
  | 'line' | 'arrow' | 'rect' | 'ellipse' | 'triangle'
  | 'rounded-rect' | 'right-triangle' | 'diamond'
  | 'pentagon' | 'hexagon'
  | 'star' | 'heart'
  | 'callout-rect' | 'callout-oval';

/** Other tools that aren't items themselves but mode-pickers. */
export type OtherTool =
  | 'text' | 'select' | 'lasso' | 'rect-select'
  | 'crop' | 'clone' | 'bucket' | 'eyedropper'
  | 'connector';

/** Every tool the WhiteboardToolPanel surfaces. */
export type Tool = BrushTool | ShapeTool | OtherTool;

// ── Items (the polymorphic layer payload) ───────────────────────────

/** One brush stroke = one continuous pointer-down through pointer-up. */
export interface StrokeItem {
  kind: 'stroke';
  tool: BrushTool;
  /** Sub-style for the brush tool (calligraphy / airbrush / oil /
   *  etc). Only meaningful when tool='pen'; eraser + marker +
   *  highlighter ignore this. Optional + defaults to 'default' for
   *  back-compat with C-1.0 → C-1.12 saves that didn't carry the
   *  field. */
  brushStyle?: BrushStyle;
  /** Hex color string (e.g. "#ff6b00") or "currentColor" to author-
   *  tint at render time. */
  color: string;
  /** Base width in source-canvas pixels; the renderer modulates by
   *  pressure / velocity. */
  width: number;
  /** 0..1, defaults to 1. Highlighter usually 0.4–0.6. */
  opacity?: number;
  /** Pointer samples — perfect-freehand input. */
  points: Point[];
  /** Phase 1.21 — when set, the stroke is rendered by stamping a
   *  bitmap brush (looked up in the session's brush-pack registry)
   *  along the path at the brush's spacing × pressure × jitter,
   *  instead of via perfect-freehand's outline fill. The id is the
   *  `stamp.id` from a BrushStamp registered in the session. When
   *  the id is missing or the stamp isn't loaded, the renderer
   *  falls back to perfect-freehand so old saves still render. */
  stampId?: string;
}

// ── Brush packs (stamp-based brushes, Phase 1.21) ────────────────
//
// A brush pack is a set of bitmap stamps + per-stamp dynamics
// (spacing, jitter, etc). At least one pack ships built-in; users
// import ABR (Photoshop) packs in Phase 1.21d. Packs are *display*
// state — they don't round-trip through the save format. Strokes
// reference stamps by `stampId` and the host loads the necessary
// packs when opening a saved whiteboard.

/** One stamp = one alpha-mask bitmap + how to lay it down. */
export interface BrushStamp {
  /** Globally unique id. ABR-sourced stamps use the original UUID;
   *  built-in stamps use stable hand-authored ids like
   *  `builtin:soft-round`. */
  id: string;
  /** Human-readable name (panel button tooltip). */
  label: string;
  /** Either an HTMLImageElement (already loaded) or a URL to fetch
   *  on first use. The renderer expects a grayscale alpha mask —
   *  255 = fully solid stamp, 0 = transparent. */
  source: HTMLImageElement | string;
  /** Spacing between successive stamps as a fraction of the
   *  effective stamp diameter. 0.1 (GIMP default) = dense; 1.0 =
   *  stamps just touch; >1 = sparse / stippled. */
  spacing: number;
  /** When true, the stamp rotates to follow the stroke direction
   *  (good for stroke-stamps + ribbon-style brushes). When false,
   *  the stamp keeps its original orientation (good for tip-stamps
   *  + texture brushes). */
  alignToPath?: boolean;
  /** 0..1 of stamp diameter — random size perturbation per stamp.
   *  0 = no jitter; 0.5 = stamps vary up to ±50%. */
  sizeJitter?: number;
  /** 0..1 — random opacity perturbation per stamp. */
  opacityJitter?: number;
  /** 0..360° — random angle perturbation per stamp. Only meaningful
   *  when alignToPath = false (otherwise the path tangent dominates). */
  angleJitter?: number;
}

/** A named collection of stamps. */
export interface BrushPack {
  id: string;
  name: string;
  stamps: BrushStamp[];
}

/** Geometric shape — defined by a rectangle (x, y, w, h) the user
 *  dragged out. Line + arrow use the rect's diagonal; rect + ellipse
 *  fit inside. Negative w/h are allowed (right-to-left drag).
 *
 *  Color model (C-1.17): outline and fill are independent. `color`
 *  was the only field through C-1.16 and is kept for back-compat
 *  reads — `strokeColor` falls back to `color` and `fillColor`
 *  falls back to `color` when missing, so old saves render the
 *  same. New saves emit the explicit fields. */
export interface ShapeItem {
  kind: 'shape';
  tool: ShapeTool;
  /** Top-left of the bounding box in source coords. */
  x: number;
  y: number;
  /** Signed dimensions — negatives indicate drag direction. */
  w: number;
  h: number;
  /** Legacy single color (C-1.0..C-1.16 saves). Kept on the type
   *  so old docs read cleanly; new saves write strokeColor +
   *  fillColor and leave this set for compatibility. */
  color: string;
  /** Independent outline color. Defaults to `color` when missing
   *  (legacy doc read path). */
  strokeColor?: string;
  /** Independent fill color. Defaults to `color` when missing. */
  fillColor?: string;
  /** Stroke width in source-canvas px. 0 = no outline. */
  width: number;
  /** When > 0, the shape is filled with `fillColor` at this opacity
   *  (0..1). Rect / ellipse / triangle / polygon-style shapes only;
   *  line / arrow ignore fill. */
  fill?: number;
  opacity?: number;
  /** Rotation in degrees around the bbox center. C-1.6 selection
   *  tool sets this. */
  rotation?: number;
}

/** Text label placed at a point and editable inline. */
export interface TextItem {
  kind: 'text';
  /** Top-left anchor in source coords. */
  x: number;
  y: number;
  /** Width / height of the text box. Auto-grows on edit; saved
   *  with the value at commit time so re-renders match. */
  w: number;
  h: number;
  body: string;
  fontSize: number;
  color: string;
  /** CSS font-family. Whiteboard ships a curated Google Fonts list
   *  (see GOOGLE_FONTS in types) — picking a face lazy-loads its
   *  webfont. Falls back to system-ui if missing / loading. */
  fontFamily?: string;
  /** 'left' | 'center' | 'right' — basic text alignment. */
  align?: 'left' | 'center' | 'right';
  bold?: boolean;
  italic?: boolean;
  /** Rotation in degrees around the bbox center. */
  rotation?: number;
}

/** Pasted (or dropped) image. The bytes are stored as a base64
 *  data: URL inside the JSON document for C-1.5 — simple, no extra
 *  upload pipeline, lives entirely in annotation_data JSONB. A
 *  later commit can swap to content-addressed storage + image_hash
 *  for large pastes (when we add `imageHash` here, the renderer
 *  prefers it over the data URL). 5 MB cap enforced client-side. */
export interface ImageItem {
  kind: 'image';
  /** Top-left in source coords. */
  x: number;
  y: number;
  /** Display dimensions (may differ from the source image's natural
   *  size when the user resized the item). */
  w: number;
  h: number;
  /** Base64 data: URL OR a remote URL (CORS willing). */
  src: string;
  /** Optional rotation in degrees — reserved for the selection /
   *  transform tool in C-1.6. */
  rotation?: number;
}

/** A connector endpoint — either pinned to a free world point OR
 *  attached to another item's anchor. The AFFiNE pattern: store
 *  the anchor as a fraction (u, v) of the target's bbox so the
 *  endpoint follows when the target moves/resizes, without any
 *  listener wiring. resolveEndpoint() recomputes the absolute
 *  point at render time. */
export interface ConnectorEndpoint {
  /** When set, the endpoint follows this item's bbox. */
  attached?: { layerId: string; itemIndex: number };
  /** Anchor as a fraction of the attached item's bbox (0..1).
   *  {u: 0, v: 0.5} = left middle, {u: 1, v: 0.5} = right middle,
   *  {u: 0.5, v: 0} = top middle, etc. Ignored when not attached. */
  u?: number;
  v?: number;
  /** Free world coords. Only used when `attached` is unset. */
  x?: number;
  y?: number;
}

/** Connector — line linking two endpoints. Either / both endpoints
 *  may be attached to another item (the AFFiNE blueprint feature
 *  for Phase 1.22). The line auto-reroutes when the attached item
 *  moves because the endpoint is stored relative; the existing
 *  render-on-doc-change loop picks up the new position for free. */
export interface ConnectorItem {
  kind: 'connector';
  start: ConnectorEndpoint;
  end: ConnectorEndpoint;
  /** Routing strategy: straight = M-L line; orthogonal = one-elbow
   *  axis-aligned path; curve = cubic bezier with end-tangents
   *  inferred from the anchor side (for attached endpoints) or
   *  from the connector direction (for free endpoints). */
  mode: 'straight' | 'orthogonal' | 'curve';
  color: string;
  /** Stroke width in source-canvas px. */
  width: number;
  opacity?: number;
  /** Arrow heads. 'arrow' = filled triangle pointing outward;
   *  'dot' = small filled disc; 'none' = nothing. Defaults to
   *  endArrow='arrow', startArrow='none' on new connectors so the
   *  natural "draw from source to target" gesture produces a
   *  pointing arrow. */
  startArrow?: 'none' | 'arrow' | 'dot';
  endArrow?: 'none' | 'arrow' | 'dot';
}

/** Polymorphic discriminated union over every kind of layer item. */
export type Item = StrokeItem | ShapeItem | TextItem | ImageItem | ConnectorItem;

/** Stacked bottom-to-top. */
export interface Layer {
  id: string;
  name?: string;
  visible: boolean;
  /** 0..1, multiplies every item's own opacity. */
  opacity: number;
  /** When true, the layer is read-only — items can't be added,
   *  edited, or removed. Visibility stays toggleable. */
  locked?: boolean;
  /** Polymorphic item list. Previously `strokes[]`; renamed to
   *  `items[]` in C-1.5 when shapes / text / images arrived. */
  items: Item[];
}

/** The full vector document. Stored verbatim in comments.annotation_
 *  data (JSONB) and re-rendered on view. */
export interface BrushContent {
  source_w: number;
  source_h: number;
  /** Optional content-addressed hash of a rasterized PNG snapshot in
   *  object storage. Convenience output for OCR / AI / PDF later. */
  image_hash?: string | null;
  /** Canvas background color (C-1.18). Hex string; defaults to
   *  '#ffffff' when missing so old saves render the same. Painted
   *  behind every layer in BrushCanvas.render. Surfaced to the
   *  user via the Canvas section in the tool panel. */
  canvas_color?: string;
  layers: Layer[];
}

/** Helper — make an empty document with one starter layer. */
export function emptyDoc(w: number, h: number): BrushContent {
  return {
    source_w: w,
    source_h: h,
    layers: [
      {
        id: crypto.randomUUID(),
        name: 'Layer 1',
        visible: true,
        opacity: 1,
        items: [],
      },
    ],
  };
}

/** Make a fresh empty layer (used by the "+" button in the layer
 *  panel). Names auto-increment from the existing count. */
export function newLayer(name?: string): Layer {
  return {
    id: crypto.randomUUID(),
    name,
    visible: true,
    opacity: 1,
    items: [],
  };
}

/** Backward-compat normalizer — older saves (pre-C-1.5) carried
 *  `strokes[]` on each layer instead of polymorphic `items[]`. Read
 *  path runs every loaded doc through this so old whiteboards keep
 *  rendering after the schema bumped. New saves emit `items[]` only. */
export function normalizeDoc(doc: BrushContent): BrushContent {
  return {
    ...doc,
    layers: doc.layers.map((l) => {
      const raw = l as Layer & { strokes?: Array<Omit<StrokeItem, 'kind'>> };
      if (Array.isArray(raw.items)) return l;
      if (Array.isArray(raw.strokes)) {
        return {
          ...l,
          items: raw.strokes.map((s) => ({ kind: 'stroke', ...s }) as StrokeItem),
        };
      }
      return { ...l, items: [] };
    }),
  };
}

/** Color palette — 8-step hand-picked for visibility on both light
 *  and dark canvas backdrops. White is omitted so the eraser is the
 *  only "remove" operation; users don't accidentally paint over
 *  themselves with white. */
export const PALETTE: readonly string[] = [
  '#ef4444', // red
  '#f97316', // orange
  '#facc15', // yellow
  '#22c55e', // green
  '#06b6d4', // cyan
  '#3b82f6', // blue
  '#a855f7', // purple
  '#0f172a', // near-black
];

/** Brush size presets in source-canvas pixels. Three steps is enough
 *  for whiteboard UX without a continuous slider; matches Apple
 *  Notes / Procreate's coarse-mode default. */
export const SIZES = [
  { label: 'S', width: 3 },
  { label: 'M', width: 6 },
  { label: 'L', width: 12 },
  { label: 'XL', width: 24 },
] as const;

/** Tool-specific default opacity. Highlighter is the only one that's
 *  semi-transparent by design. */
export function defaultOpacityFor(tool: Tool): number {
  return tool === 'highlighter' ? 0.45 : 1;
}

/** True when the tool produces a Stroke item (free-form brush). */
export function isBrushTool(tool: Tool): tool is BrushTool {
  return tool === 'pen' || tool === 'marker' || tool === 'highlighter' || tool === 'eraser';
}

/** Every ShapeTool id, exposed both for runtime predicates and for
 *  iteration (the WhiteboardToolPanel renders one button per id).
 *  Order matters here — it controls the Shapes section layout. */
export const SHAPE_TOOLS: readonly ShapeTool[] = [
  'line', 'arrow', 'rect', 'rounded-rect', 'ellipse',
  'triangle', 'right-triangle', 'diamond',
  'pentagon', 'hexagon', 'star', 'heart',
  'callout-rect', 'callout-oval',
] as const;
const SHAPE_TOOL_SET = new Set<string>(SHAPE_TOOLS);

/** True when the tool produces a Shape item (click-drag-release).
 *  Exhaustive over `ShapeTool` — prior to C-1.18 the predicate only
 *  matched the original 4 shapes (line/arrow/rect/ellipse) and the
 *  new ones (triangle/diamond/star/heart/callouts/etc) couldn't
 *  start a drag gesture in BrushCanvas because the branch never
 *  fired. */
export function isShapeTool(tool: Tool): tool is ShapeTool {
  return SHAPE_TOOL_SET.has(tool);
}

/** Per-tool perfect-freehand parameters. For pen we further switch
 *  on `brushStyle` so the user's brush picker maps to a real change
 *  in the stroke math. */
export function strokeOptionsFor(tool: BrushTool, style: BrushStyle = 'default') {
  if (tool === 'highlighter') {
    return { size: 1, thinning: 0, smoothing: 0.6, streamline: 0.6, easing: (t: number) => t, simulatePressure: false, last: true };
  }
  if (tool === 'marker') {
    return { size: 1, thinning: 0.15, smoothing: 0.5, streamline: 0.5, easing: (t: number) => t, simulatePressure: true, last: true };
  }
  if (tool === 'eraser') {
    return { size: 1, thinning: 0.2, smoothing: 0.5, streamline: 0.5, easing: (t: number) => t, simulatePressure: true, last: true };
  }
  // Pen — branch on brush style.
  switch (style) {
    case 'calligraphy':
      // Velocity-modulated thick taper. High thinning + delayed
      // easing = the swooping width changes that read as calligraphy.
      return { size: 1, thinning: 0.8, smoothing: 0.4, streamline: 0.4, easing: (t: number) => t * t * t, simulatePressure: true, last: true };
    case 'pen-tip':
      // Thin steady fountain-pen. Almost no thinning so the line
      // reads as a constant ink trail.
      return { size: 1, thinning: 0.05, smoothing: 0.5, streamline: 0.3, easing: (t: number) => t, simulatePressure: false, last: true };
    case 'pencil':
      // Sharp, low-flow, slightly broken edges. Low smoothing so
      // we keep the wobble; the noise overlay (drawStroke effects)
      // adds the texture.
      return { size: 1, thinning: 0.5, smoothing: 0.25, streamline: 0.2, easing: (t: number) => t, simulatePressure: true, last: true };
    case 'airbrush':
      // Soft fat stroke; scatter dots layered in drawStroke add
      // the "spray" texture.
      return { size: 1, thinning: 0.1, smoothing: 0.7, streamline: 0.7, easing: (t: number) => t, simulatePressure: false, last: true };
    case 'oil':
      // Heavy paint feel — wide, low thinning, very smooth.
      return { size: 1, thinning: 0.25, smoothing: 0.7, streamline: 0.6, easing: (t: number) => t, simulatePressure: true, last: true };
    case 'crayon':
      // Same baseline as pen but the noise overlay in drawStroke
      // gives the wax-on-paper roughness.
      return { size: 1, thinning: 0.4, smoothing: 0.4, streamline: 0.3, easing: (t: number) => t, simulatePressure: true, last: true };
    case 'watercolor':
      // Wide, soft, semi-transparent — the layer-opacity halving
      // in drawStroke gives the buildup look.
      return { size: 1, thinning: 0.15, smoothing: 0.7, streamline: 0.8, easing: (t: number) => t, simulatePressure: false, last: true };
    case 'default':
    default:
      return { size: 1, thinning: 0.55, smoothing: 0.5, streamline: 0.35, easing: (t: number) => t * t, simulatePressure: true, last: true };
  }
}

/** UI catalogue of brush styles for the BRUSHES section's sub-picker.
 *  Order matches how a user typically thinks about brushes (Paint /
 *  Procreate ordering). */
export interface BrushStyleEntry {
  id: BrushStyle;
  label: string;
}
export const BRUSH_STYLES: readonly BrushStyleEntry[] = [
  { id: 'default',     label: 'Brush' },
  { id: 'calligraphy', label: 'Calligraphy brush' },
  { id: 'pen-tip',     label: 'Calligraphy pen' },
  { id: 'pencil',      label: 'Natural pencil' },
  { id: 'airbrush',    label: 'Airbrush' },
  { id: 'oil',         label: 'Oil brush' },
  { id: 'crayon',      label: 'Crayon' },
  { id: 'watercolor',  label: 'Watercolor brush' },
] as const;

/** Max bytes for a pasted/dropped image before we reject it. 5 MB —
 *  generous for screenshots, blocks accidentally pasting in a
 *  high-res photo that would balloon the JSON document. */
export const MAX_PASTED_IMAGE_BYTES = 5 * 1024 * 1024;

// ── Bounding boxes + hit-testing (used by selection tool) ────────

export interface BBox { x: number; y: number; w: number; h: number; rotation: number; }

/** Axis-aligned bbox of an item in source-canvas coords. Strokes
 *  scan their points; shapes / text / image read their own x/y/w/h
 *  (after normalizing negative shape sizes). The rotation field is
 *  the item's own rotation around the bbox center; the bbox values
 *  themselves are still axis-aligned. */
export function itemBBox(item: Item, doc?: BrushContent): BBox {
  switch (item.kind) {
    case 'stroke': {
      let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
      for (const p of item.points) {
        if (p[0] < minX) minX = p[0];
        if (p[1] < minY) minY = p[1];
        if (p[0] > maxX) maxX = p[0];
        if (p[1] > maxY) maxY = p[1];
      }
      const pad = (item.width ?? 6) * 0.6;
      if (!Number.isFinite(minX)) return { x: 0, y: 0, w: 0, h: 0, rotation: 0 };
      return { x: minX - pad, y: minY - pad, w: (maxX - minX) + pad * 2, h: (maxY - minY) + pad * 2, rotation: 0 };
    }
    case 'shape': {
      const x = item.w >= 0 ? item.x : item.x + item.w;
      const y = item.h >= 0 ? item.y : item.y + item.h;
      return { x, y, w: Math.abs(item.w), h: Math.abs(item.h), rotation: item.rotation ?? 0 };
    }
    case 'text':
      return { x: item.x, y: item.y, w: item.w, h: item.h, rotation: item.rotation ?? 0 };
    case 'image':
      return { x: item.x, y: item.y, w: item.w, h: item.h, rotation: item.rotation ?? 0 };
    case 'connector': {
      // The bbox spans the two resolved endpoints. doc is needed
      // to resolve attached endpoints — without it we fall back to
      // the free coords (which is what older docs without a doc
      // argument get; safe because connectors weren't picked by
      // selection/lasso before this phase).
      const s = resolveConnectorEndpoint(item.start, doc);
      const e = resolveConnectorEndpoint(item.end, doc);
      const pad = (item.width ?? 2) * 0.6;
      return {
        x: Math.min(s.x, e.x) - pad,
        y: Math.min(s.y, e.y) - pad,
        w: Math.abs(e.x - s.x) + pad * 2,
        h: Math.abs(e.y - s.y) + pad * 2,
        rotation: 0,
      };
    }
  }
}

/** Resolve an endpoint to absolute (x, y) world coords. When the
 *  endpoint is `attached`, looks up the target item in the doc and
 *  multiplies the (u, v) anchor fractions through its bbox. When
 *  the target is missing (deleted while connector remained), falls
 *  back to the free coords so the connector doesn't visually
 *  collapse into the origin. */
export function resolveConnectorEndpoint(ep: ConnectorEndpoint, doc?: BrushContent): { x: number; y: number } {
  if (ep.attached && doc) {
    const layer = doc.layers.find((l) => l.id === ep.attached!.layerId);
    const target = layer?.items[ep.attached.itemIndex];
    if (target && target.kind !== 'connector') {
      // No connector→connector attachments: prevents cycles in
      // resolveConnectorEndpoint and keeps the bbox stable.
      const bb = itemBBox(target);
      const u = ep.u ?? 0.5;
      const v = ep.v ?? 0.5;
      return { x: bb.x + u * bb.w, y: bb.y + v * bb.h };
    }
  }
  return { x: ep.x ?? 0, y: ep.y ?? 0 };
}

/** Five canonical anchor points on an item's bbox — the four edge
 *  midpoints + the center. Used by the connector tool to (a) snap
 *  the user's click to the nearest anchor, (b) hint hover targets
 *  when the connector tool is active. Returns absolute world coords
 *  alongside the (u, v) fractions for storing in the endpoint. */
export interface AnchorPoint { u: number; v: number; x: number; y: number; }
export function anchorsForItem(item: Item, doc?: BrushContent): AnchorPoint[] {
  const bb = itemBBox(item, doc);
  if (bb.w === 0 || bb.h === 0) return [];
  // Order matters cosmetically: top, right, bottom, left, center.
  // Matches the order users expect when tabbing through anchors.
  return [
    { u: 0.5, v: 0,   x: bb.x + bb.w / 2, y: bb.y },
    { u: 1,   v: 0.5, x: bb.x + bb.w,     y: bb.y + bb.h / 2 },
    { u: 0.5, v: 1,   x: bb.x + bb.w / 2, y: bb.y + bb.h },
    { u: 0,   v: 0.5, x: bb.x,            y: bb.y + bb.h / 2 },
    { u: 0.5, v: 0.5, x: bb.x + bb.w / 2, y: bb.y + bb.h / 2 },
  ];
}

/** Test whether a point is inside an item's bbox (rotation-aware:
 *  un-rotates the point into the bbox's local space first). Used by
 *  the select tool's pick gesture. */
export function pointInItem(px: number, py: number, item: Item): boolean {
  const bb = itemBBox(item);
  if (bb.w === 0 || bb.h === 0) return false;
  let lx = px, ly = py;
  if (bb.rotation) {
    const cx = bb.x + bb.w / 2;
    const cy = bb.y + bb.h / 2;
    const ang = (-bb.rotation * Math.PI) / 180;
    const dx = px - cx;
    const dy = py - cy;
    lx = cx + dx * Math.cos(ang) - dy * Math.sin(ang);
    ly = cy + dx * Math.sin(ang) + dy * Math.cos(ang);
  }
  return lx >= bb.x && lx <= bb.x + bb.w && ly >= bb.y && ly <= bb.y + bb.h;
}

/** Standard ray-casting point-in-polygon test for the lasso tool.
 *  poly is an array of [x, y] vertices in source-canvas coords. */
export function pointInPolygon(px: number, py: number, poly: number[][]): boolean {
  let inside = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i++) {
    const xi = poly[i][0], yi = poly[i][1];
    const xj = poly[j][0], yj = poly[j][1];
    const intersect = ((yi > py) !== (yj > py)) &&
      (px < ((xj - xi) * (py - yi)) / ((yj - yi) || 1e-9) + xi);
    if (intersect) inside = !inside;
  }
  return inside;
}

/** True if any of an item's bbox corners falls inside the polygon —
 *  good-enough lasso hit-test. Catches strokes / shapes / text /
 *  images alike without per-kind logic. */
export function itemInPolygon(item: Item, poly: number[][]): boolean {
  const bb = itemBBox(item);
  const corners = [
    [bb.x, bb.y],
    [bb.x + bb.w, bb.y],
    [bb.x, bb.y + bb.h],
    [bb.x + bb.w, bb.y + bb.h],
    [bb.x + bb.w / 2, bb.y + bb.h / 2], // center
  ];
  return corners.some((c) => pointInPolygon(c[0], c[1], poly));
}

// ── Item mutation helpers (selection / move / resize) ─────────────

/** Translate an item by (dx, dy) in source coords. Strokes shift
 *  every point; connectors shift only the *free* endpoint coords
 *  (attached endpoints stay anchored to their target); the others
 *  bump x/y. */
export function translateItem(item: Item, dx: number, dy: number): Item {
  if (item.kind === 'stroke') {
    return { ...item, points: item.points.map((p) => [p[0] + dx, p[1] + dy, p[2]]) };
  }
  if (item.kind === 'connector') {
    const shift = (ep: ConnectorEndpoint): ConnectorEndpoint => {
      if (ep.attached) return ep; // anchor stays glued to target
      return { ...ep, x: (ep.x ?? 0) + dx, y: (ep.y ?? 0) + dy };
    };
    return { ...item, start: shift(item.start), end: shift(item.end) };
  }
  return { ...item, x: item.x + dx, y: item.y + dy };
}

/** Scale an item to a new bbox. Strokes re-map every point; shapes /
 *  text / image just re-set x/y/w/h. fontSize on text scales with
 *  height so re-sizing the box re-sizes the text proportionally. */
export function resizeItemToBBox(item: Item, x: number, y: number, w: number, h: number): Item {
  const old = itemBBox(item);
  if (item.kind === 'stroke') {
    if (old.w === 0 || old.h === 0) return item;
    const sx = w / old.w;
    const sy = h / old.h;
    return {
      ...item,
      points: item.points.map((p) => [
        x + (p[0] - old.x) * sx,
        y + (p[1] - old.y) * sy,
        p[2],
      ]),
    };
  }
  if (item.kind === 'shape') {
    return { ...item, x, y, w, h };
  }
  if (item.kind === 'text') {
    const scaleY = old.h > 0 ? h / old.h : 1;
    return { ...item, x, y, w, h, fontSize: Math.max(8, Math.min(256, item.fontSize * scaleY)) };
  }
  if (item.kind === 'connector') {
    // Connectors don't resize via the bbox tool — they're defined
    // by their endpoints, not a w/h rect. Drop-through: just
    // return the item unchanged. The user moves endpoints via a
    // future endpoint-handle gesture (Phase 1.22 follow-up).
    return item;
  }
  // image
  return { ...item, x, y, w, h };
}

// ── Google Fonts (curated, lazy-loaded) ──────────────────────────

export interface FontEntry {
  family: string;          // CSS font-family value (must match Google's API name)
  label: string;           // Human-friendly label in the picker
  category: 'sans' | 'serif' | 'display' | 'handwriting' | 'mono';
  /** CSS weight options to request via Google Fonts. Pickers only
   *  expose 400 + 700 — bold toggle picks 700, otherwise 400. */
  weights: number[];
}

/** Curated set. Limelight is the user's brand display face (per
 *  account.preferences.themes config). The rest cover the common
 *  family categories users reach for: clean sans, serious serif,
 *  display, hand, monospace. */
export const GOOGLE_FONTS: readonly FontEntry[] = [
  { family: 'Limelight',           label: 'Limelight (brand)', category: 'display', weights: [400] },
  { family: 'Inter',               label: 'Inter',             category: 'sans',    weights: [400, 700] },
  { family: 'Roboto',              label: 'Roboto',            category: 'sans',    weights: [400, 700] },
  { family: 'Open Sans',           label: 'Open Sans',         category: 'sans',    weights: [400, 700] },
  { family: 'Merriweather',        label: 'Merriweather',      category: 'serif',   weights: [400, 700] },
  { family: 'Lora',                label: 'Lora',              category: 'serif',   weights: [400, 700] },
  { family: 'Playfair Display',    label: 'Playfair Display',  category: 'serif',   weights: [400, 700] },
  { family: 'Bebas Neue',          label: 'Bebas Neue',        category: 'display', weights: [400] },
  { family: 'Pacifico',            label: 'Pacifico',          category: 'handwriting', weights: [400] },
  { family: 'Caveat',              label: 'Caveat',            category: 'handwriting', weights: [400, 700] },
  { family: 'Permanent Marker',    label: 'Permanent Marker',  category: 'handwriting', weights: [400] },
  { family: 'JetBrains Mono',      label: 'JetBrains Mono',    category: 'mono',    weights: [400, 700] },
] as const;

/** Default font family for new text items. Inter is a safe, neutral
 *  sans that reads well at every size and is widely cached on the
 *  Google Fonts CDN. */
export const DEFAULT_FONT_FAMILY = 'Inter';

/** Min/max font-size for the typography slider. */
export const FONT_SIZE_MIN = 8;
export const FONT_SIZE_MAX = 96;
export const FONT_SIZE_PRESETS = [14, 18, 24, 36, 48, 72, 96] as const;
