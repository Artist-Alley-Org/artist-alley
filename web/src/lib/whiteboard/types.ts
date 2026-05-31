// Whiteboard / annotation data model — vector source of truth.
//
// Mirrors the WhiteboardContent / WhiteboardCreate schemas in
// app/api/openapi.yaml (we don't import the generated openapi types
// here because those use `unknown` for the JSONB payload — these
// types let the frontend reason about the structure typecheck-first).
//
// The same shape powers both:
//   - Whiteboards (target = post, no frame anchor)
//   - Frame annotations (target = asset, video_frame anchor) — phase C-2
//
// Coordinates are stored in source-canvas pixel space (source_w ×
// source_h captured at save time). Rendering scales to viewport.

/** Single pointer sample. [x, y, pressure?] — pressure 0..1 when the
 *  device reports it (stylus); the engine simulates from velocity
 *  otherwise. perfect-freehand consumes this directly. */
export type Point = [number, number, number?];

/** Every tool the BrushCanvas understands. The schema enum is the
 *  source of truth; this mirrors it. Phase C-1 ships pen / marker /
 *  highlighter / eraser. The rest (line / arrow / rect / ellipse /
 *  text) are reserved enum values so saving a whiteboard today and
 *  re-opening it in a future build stays forward-compatible. */
export type Tool =
  | 'pen'
  | 'marker'
  | 'highlighter'
  | 'eraser'
  | 'line'
  | 'arrow'
  | 'rect'
  | 'ellipse'
  | 'text';

/** One stroke = one continuous pointer-down through pointer-up. */
export interface Stroke {
  tool: Tool;
  /** Hex color string (e.g. "#ff6b00") or "currentColor" to author-
   *  tint at render time. */
  color: string;
  /** Base width in source-canvas pixels; the renderer modulates by
   *  pressure / velocity. */
  width: number;
  /** 0..1, defaults to 1. Highlighter usually 0.4–0.6. */
  opacity?: number;
  /** Only for tool='text' — the label content. */
  text?: string;
  /** Pointer samples — perfect-freehand input. */
  points: Point[];
}

/** Stacked bottom-to-top. */
export interface Layer {
  id: string;
  name?: string;
  visible: boolean;
  opacity: number;
  strokes: Stroke[];
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
        strokes: [],
      },
    ],
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

/** Per-tool perfect-freehand parameters. The library exposes a
 *  StrokeOptions bag — these defaults shape each tool's feel:
 *    pen        → tight, pressure-responsive
 *    marker     → thicker, less pressure variance
 *    highlighter→ flat, wide, no taper
 *    eraser     → uses destination-out composite at render time;
 *                 same stroke shape, color is irrelevant
 */
export function strokeOptionsFor(tool: Tool) {
  switch (tool) {
    case 'highlighter':
      return {
        size: 1, // size is applied externally via width; this is just the multiplier
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
