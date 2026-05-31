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

/** Shape tools (click-drag-release rectangles defining the shape). */
export type ShapeTool = 'line' | 'arrow' | 'rect' | 'ellipse';

/** Other tools that aren't items themselves but mode-pickers. */
export type OtherTool = 'text' | 'select' | 'lasso' | 'crop' | 'clone';

/** Every tool the WhiteboardToolPanel surfaces. */
export type Tool = BrushTool | ShapeTool | OtherTool;

// ── Items (the polymorphic layer payload) ───────────────────────────

/** One brush stroke = one continuous pointer-down through pointer-up. */
export interface StrokeItem {
  kind: 'stroke';
  tool: BrushTool;
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
}

/** Geometric shape — defined by a rectangle (x, y, w, h) the user
 *  dragged out. Line + arrow use the rect's diagonal; rect + ellipse
 *  fit inside. Negative w/h are allowed (right-to-left drag). */
export interface ShapeItem {
  kind: 'shape';
  tool: ShapeTool;
  /** Top-left of the bounding box in source coords. */
  x: number;
  y: number;
  /** Signed dimensions — negatives indicate drag direction. */
  w: number;
  h: number;
  color: string;
  /** Stroke width in source-canvas px. */
  width: number;
  /** When set, the shape is filled with the same color at this
   *  opacity (0..1). Rect + ellipse only. */
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

/** Polymorphic discriminated union over every kind of layer item. */
export type Item = StrokeItem | ShapeItem | TextItem | ImageItem;

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

/** True when the tool produces a Shape item (click-drag-release). */
export function isShapeTool(tool: Tool): tool is ShapeTool {
  return tool === 'line' || tool === 'arrow' || tool === 'rect' || tool === 'ellipse';
}

/** Per-tool perfect-freehand parameters. The library exposes a
 *  StrokeOptions bag — these defaults shape each tool's feel. */
export function strokeOptionsFor(tool: BrushTool) {
  switch (tool) {
    case 'highlighter':
      return {
        size: 1,
        thinning: 0,
        smoothing: 0.6,
        streamline: 0.6,
        easing: (t: number) => t,
        simulatePressure: false,
        last: true,
      };
    case 'marker':
      return {
        size: 1,
        thinning: 0.15,
        smoothing: 0.5,
        streamline: 0.5,
        easing: (t: number) => t,
        simulatePressure: true,
        last: true,
      };
    case 'eraser':
      return {
        size: 1,
        thinning: 0.2,
        smoothing: 0.5,
        streamline: 0.5,
        easing: (t: number) => t,
        simulatePressure: true,
        last: true,
      };
    case 'pen':
    default:
      return {
        size: 1,
        thinning: 0.55,
        smoothing: 0.5,
        streamline: 0.35,
        easing: (t: number) => t * t,
        simulatePressure: true,
        last: true,
      };
  }
}

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
export function itemBBox(item: Item): BBox {
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
  }
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
 *  every point; the others bump x/y. */
export function translateItem(item: Item, dx: number, dy: number): Item {
  if (item.kind === 'stroke') {
    return { ...item, points: item.points.map((p) => [p[0] + dx, p[1] + dy, p[2]]) };
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
