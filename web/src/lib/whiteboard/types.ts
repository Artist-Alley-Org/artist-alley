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
export type OtherTool = 'text' | 'select';

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
  /** 'left' | 'center' | 'right' — basic text alignment. */
  align?: 'left' | 'center' | 'right';
  /** Bold / italic toggles. Italic skipped in C-1.5 toolbar; field
   *  reserved so a future toolbar gets to it without schema change. */
  bold?: boolean;
  italic?: boolean;
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
