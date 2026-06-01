<script lang="ts">
  // BrushCanvas — the polymorphic-item drawing primitive used by
  // WhiteboardCanvas (post-anchored) and the future Annotation
  // surface (asset-frame-anchored).
  //
  // Responsibilities
  // ----------------
  //   - The <canvas> element + 2D context (DPR-aware sizing)
  //   - Pointer event handling (mouse / touch / stylus)
  //   - Renderers for every Item kind:
  //       Stroke (perfect-freehand)
  //       Shape  (line / arrow / rect / ellipse)
  //       Text   (canvas fillText)
  //       Image  (drawImage from data URL or remote)
  //   - Per-tool input gestures:
  //       brush  → continuous pointer-down through pointer-up
  //       shape  → click-drag-release (Shift constrains)
  //       text   → click → contenteditable overlay → commit on blur
  //       (select / transform lands in C-1.6 — out of scope here)
  //   - Paste handler: image / text / URL → push the right item kind
  //
  // What it does NOT own
  // --------------------
  //   - Tool / color / size state (lives in the session store passed
  //     in via the `session` prop)
  //   - Toolbar UI (the WhiteboardToolPanel sibling owns it)
  //   - Save / load (the host POSTs the session's doc on demand)

  import { onMount, onDestroy } from 'svelte';
  import { getStroke } from 'perfect-freehand';
  import type {
    BBox, BrushContent, BrushStamp, ConnectorEndpoint, ConnectorItem, FrameItem, ImageItem, Item, Layer, MindmapItem, MindmapNode, ShapeItem, StickyNoteItem, StrokeItem, TextItem,
  } from '$lib/whiteboard/types';
  import {
    MAX_PASTED_IMAGE_BYTES,
    DEFAULT_FONT_FAMILY,
    isBrushTool, isShapeTool, strokeOptionsFor,
    itemBBox, pointInItem, translateItem, resizeItemToBBox,
    itemInPolygon,
    anchorsForItem, resolveConnectorEndpoint,
    layoutMindmap, walkMindmap, addMindmapChild,
  } from '$lib/whiteboard/types';
  import { getStamp, getTintedStamp } from '$lib/whiteboard/brushes';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';

  // Per-session local clipboard for copy/cut/paste of items. Lives
  // in module scope so it survives canvas remounts within the same
  // tab. Shared across whiteboard sessions in the same tab — useful
  // when the user copies from one sketch and pastes into another.
  let sessionClipboard: Item | null = null;

  // CSS cursor per active tool. Uses inline SVG data URLs for the
  // tools whose action a stock cursor doesn't describe well
  // (eraser / eyedropper / bucket / clone) so users always see
  // "what's in my hand" — matches Paint / Photoshop's affordance.
  // The hotspot anchor in `<svg> H Hy auto` (the 'H Hy auto' suffix)
  // tells the browser the click point + fallback. Tools with a clear
  // native equivalent stay on the native cursor so the OS theme +
  // accessibility scaling apply.
  function svgCursor(svg: string, hotX: number, hotY: number): string {
    const url = `url("data:image/svg+xml;utf8,${encodeURIComponent(svg)}") ${hotX} ${hotY}, crosshair`;
    return url;
  }
  function cursorFor(tool: typeof session.tool, ro: boolean): string {
    if (ro) return 'default';
    switch (tool) {
      case 'select':       return 'default';
      case 'text':         return 'text';
      case 'crop':         return 'crosshair';
      case 'rect-select':  return 'crosshair';
      case 'lasso':        return 'crosshair';
      case 'eraser':       return svgCursor(
        '<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"22\" height=\"22\" viewBox=\"0 0 24 24\" fill=\"white\" stroke=\"black\" stroke-width=\"1.5\" stroke-linejoin=\"round\"><path d=\"M3 19h18 M18 13l-7-7-7 7 6 6h8z\" stroke-linecap=\"round\" fill=\"white\"/></svg>',
        4, 18,
      );
      case 'eyedropper':   return svgCursor(
        '<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"22\" height=\"22\" viewBox=\"0 0 24 24\" fill=\"white\" stroke=\"black\" stroke-width=\"1.5\" stroke-linecap=\"round\" stroke-linejoin=\"round\"><path d=\"M2 22l1-1h4l9-9-3-3-9 9v4z M14 7l3 3 M17 4l3 3-3 3-3-3z\"/></svg>',
        2, 20,
      );
      case 'bucket':       return svgCursor(
        '<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"22\" height=\"22\" viewBox=\"0 0 24 24\" fill=\"white\" stroke=\"black\" stroke-width=\"1.5\" stroke-linecap=\"round\" stroke-linejoin=\"round\"><path d=\"M19 11l-7-7-8 8 7 7z M16 4l3 7\"/><circle cx=\"21\" cy=\"15\" r=\"2\" fill=\"white\"/></svg>',
        4, 18,
      );
      case 'clone':        return svgCursor(
        '<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"22\" height=\"22\" viewBox=\"0 0 24 24\" fill=\"white\" stroke=\"black\" stroke-width=\"1.5\" stroke-linecap=\"round\" stroke-linejoin=\"round\"><rect x=\"3\" y=\"3\" width=\"10\" height=\"10\" fill=\"white\"/><rect x=\"11\" y=\"11\" width=\"10\" height=\"10\" fill=\"white\"/></svg>',
        11, 11,
      );
      // Brush + shape tools → crosshair as a neutral default; the
      // panel button is the source-of-truth indicator for which one
      // is active.
      default:             return 'crosshair';
    }
  }

  interface Props {
    /** Shared reactive session. Owns doc, active layer, tools. */
    session: WhiteboardSession;
    /** Read-only mode — render but don't accept input (sidebar
        preview of a saved whiteboard). */
    readOnly?: boolean;
    /** Optional asset reference image — when present, drawn beneath
        the items (annotation use case). Whiteboards don't pass this. */
    backgroundUrl?: string;
    /** Infinite-canvas mode (C-1.19). When true, the canvas matches
        the wrapper dimensions and applies session.viewX / viewY /
        viewZoom at render time so the user can pan + zoom Miro-style.
        When false (default — annotation use case), the canvas matches
        source dimensions and renders fixed-fit. The whiteboard host
        opts in; the annotation host doesn't, so the two surfaces
        keep their separate UX. */
    infinite?: boolean;
  }

  let { session, readOnly = false, backgroundUrl, infinite = false }: Props = $props();

  // ── DOM refs ──────────────────────────────────────────────────────

  let canvasEl: HTMLCanvasElement | undefined = $state();
  let wrapperEl: HTMLDivElement | undefined = $state();
  let ctx: CanvasRenderingContext2D | null = null;

  // ── Live gesture state ────────────────────────────────────────────

  // Brush stroke being drawn this frame.
  let liveStroke: StrokeItem | null = $state(null);
  // Shape being dragged out this frame.
  let liveShape: ShapeItem | null = $state(null);
  let dragStart: { x: number; y: number } | null = null;
  // Lasso polygon being drawn this frame. List of [x, y] vertices
  // in source coords; closed on mouseup.
  let liveLasso: number[][] | null = $state(null);
  // Crop rectangle being dragged out this frame. (x, y, w, h) in
  // source coords, signed.
  let liveCrop: { x: number; y: number; w: number; h: number } | null = $state(null);
  // Rectangle-select drag — same shape as crop but commits to a
  // multi-selection instead of trimming the canvas.
  let liveRectSelect: { x: number; y: number; w: number; h: number } | null = $state(null);
  // Connector tool state (Phase 1.22) — two-click gesture: first
  // click pins the start endpoint, mouse-move tracks the cursor as
  // the live end, second click commits. Carries the full draft
  // connector minus the end attachment (which is resolved on
  // commit), so the render path can `drawConnector(liveConnector
  // as ConnectorItem)` directly for the preview.
  let liveConnector: Omit<ConnectorItem, 'kind'> | null = $state(null);
  // Frame + sticky drag-out previews (Phase 1.23). Live items are
  // the full FrameItem / StickyNoteItem so the same renderer paths
  // handle the preview as the committed item.
  let liveFrame: FrameItem | null = $state(null);
  let liveSticky: StickyNoteItem | null = $state(null);
  // Currently-hovered anchor (under the cursor when connector tool
  // is active). Drives the visual snap hint that draws a small ring
  // around the anchor point.
  let hoverAnchor: { layerId: string; index: number; anchor: { u: number; v: number; x: number; y: number } } | null = $state(null);
  // Text being typed (overlay div positioned over the canvas).
  let textEdit: { x: number; y: number; w: number; h: number } | null = $state(null);
  let textEditBody = $state('');
  let textEditRef: HTMLDivElement | undefined = $state();

  // Background image cache (annotation use case).
  let backgroundImg: HTMLImageElement | null = $state(null);

  // Image item cache — drawImage requires an HTMLImageElement, so
  // we lazy-load + memoize each Image item's src once. Keyed by src
  // (data URL or remote URL) so identical pastes share one Image.
  const imageCache = new Map<string, HTMLImageElement>();
  function loadImage(src: string): HTMLImageElement {
    const cached = imageCache.get(src);
    if (cached) return cached;
    const img = new Image();
    img.crossOrigin = 'anonymous';
    img.src = src;
    img.onload = () => render();
    imageCache.set(src, img);
    return img;
  }

  // ── Canvas sizing (DPR-aware) ─────────────────────────────────────
  //
  // Two modes (C-1.19):
  //   - infinite=false: canvas matches the source-doc dimensions in
  //     CSS px, stretched to wrapper via `width:100%`. Identity
  //     transform; world coords == source coords == CSS coords.
  //     Same shape as before C-1.19. Used by the annotation host.
  //   - infinite=true:  canvas matches the wrapper dimensions in
  //     CSS px (Miro-style). Render-time setTransform applies
  //     session.view{X,Y,Zoom} so the user can pan + zoom freely.
  //     Source-doc dims become a "rasterization frame" hint only.

  function fitToWrapper() {
    if (!canvasEl || !wrapperEl || !ctx) return;
    const dpr = window.devicePixelRatio || 1;
    const wW = wrapperEl.clientWidth;
    const wH = wrapperEl.clientHeight;
    canvasEl.style.width = `${wW}px`;
    canvasEl.style.height = `${wH}px`;
    if (infinite) {
      // Backing store matches viewport in CSS px (× DPR for sharp
      // lines on hidpi). Items aren't clipped by source bounds
      // anymore; the pan + zoom transform decides what's on-screen.
      canvasEl.width = Math.round(wW * dpr);
      canvasEl.height = Math.round(wH * dpr);
      // Auto-fit on first mount so the source-doc rect lands
      // centered + visible — same as opening a fresh sketch in Miro.
      if (session.viewZoom === 1 && session.viewX === 0 && session.viewY === 0) {
        session.fitView(wW, wH);
      }
    } else {
      canvasEl.width = Math.round(session.doc.source_w * dpr);
      canvasEl.height = Math.round(session.doc.source_h * dpr);
    }
    render();
  }

  // ── World-coord conversion ────────────────────────────────────────
  //
  // "world" = source-canvas coords (same as before C-1.19; we kept
  // the name for storage compat). "css" = viewport CSS pixels in
  // the canvas wrapper. In infinite mode the mapping is
  //   cssX = worldX * zoom + viewX
  // so eventToSource runs the inverse + sourceToCss runs the
  // forward map. In fixed mode it's a simple proportional fit.

  function eventToSource(e: PointerEvent | MouseEvent): { x: number; y: number; p?: number } {
    if (!canvasEl) return { x: 0, y: 0 };
    const rect = canvasEl.getBoundingClientRect();
    const cssX = e.clientX - rect.left;
    const cssY = e.clientY - rect.top;
    let x: number, y: number;
    if (infinite) {
      x = (cssX - session.viewX) / session.viewZoom;
      y = (cssY - session.viewY) / session.viewZoom;
    } else {
      x = (cssX / rect.width) * session.doc.source_w;
      y = (cssY / rect.height) * session.doc.source_h;
    }
    const p = (e as PointerEvent).pressure;
    return { x, y, p: p > 0 && p !== 0.5 ? p : undefined };
  }

  function sourceToCss(x: number, y: number): { left: number; top: number } {
    if (!canvasEl) return { left: 0, top: 0 };
    if (infinite) {
      return { left: x * session.viewZoom + session.viewX, top: y * session.viewZoom + session.viewY };
    }
    const rect = canvasEl.getBoundingClientRect();
    return {
      left: (x / session.doc.source_w) * rect.width,
      top: (y / session.doc.source_h) * rect.height,
    };
  }

  // ── Render ────────────────────────────────────────────────────────

  function render() {
    if (!ctx || !canvasEl) return;
    const dpr = window.devicePixelRatio || 1;
    // ── Transform setup ─────────────────────────────────────────
    // Two paths: infinite mode applies pan + zoom; fixed mode
    // keeps the C-1.0 identity transform. Reset → identity →
    // clear viewport → re-apply the per-frame transform so the
    // clearRect call uses backing-store coords (not world coords
    // multiplied by zoom which would underflow).
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, canvasEl.width, canvasEl.height);
    if (infinite) {
      // CSS coords scaled to backing store (DPR) then pan/zoom.
      // Order of multiplication: backing = view * dpr.
      ctx.setTransform(
        dpr * session.viewZoom, 0,
        0, dpr * session.viewZoom,
        dpr * session.viewX, dpr * session.viewY,
      );
    } else {
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    }

    // Infinite-mode backdrop: paint a giant world-space rect so
    // the canvas color fills the viewport at any zoom. World-space
    // viewport bounds = inverse map of (0, 0) → (cssW, cssH).
    const bg = session.doc.canvas_color ?? '#ffffff';
    if (infinite) {
      const rect = canvasEl.getBoundingClientRect();
      const w0 = -session.viewX / session.viewZoom;
      const h0 = -session.viewY / session.viewZoom;
      const w1 = (rect.width - session.viewX) / session.viewZoom;
      const h1 = (rect.height - session.viewY) / session.viewZoom;
      if (bg && bg !== 'transparent') {
        ctx.fillStyle = bg;
        ctx.fillRect(w0, h0, w1 - w0, h1 - h0);
      }
      // Faint frame around the source-doc rect — a subtle Miro-
      // style "page" boundary so users know where the export
      // / snapshot region sits without it feeling like a wall.
      ctx.save();
      ctx.lineWidth = 1 / session.viewZoom;
      ctx.strokeStyle = 'rgba(100, 116, 139, 0.35)';
      ctx.setLineDash([8 / session.viewZoom, 6 / session.viewZoom]);
      ctx.strokeRect(0, 0, session.doc.source_w, session.doc.source_h);
      ctx.restore();
    } else if (bg && bg !== 'transparent') {
      ctx.fillStyle = bg;
      ctx.fillRect(0, 0, session.doc.source_w, session.doc.source_h);
    }

    // Background image (annotation surface)
    if (backgroundImg) {
      const iw = backgroundImg.naturalWidth;
      const ih = backgroundImg.naturalHeight;
      if (iw > 0 && ih > 0) {
        const r = Math.min(session.doc.source_w / iw, session.doc.source_h / ih);
        const w = iw * r;
        const h = ih * r;
        ctx.drawImage(
          backgroundImg,
          (session.doc.source_w - w) / 2,
          (session.doc.source_h - h) / 2,
          w, h,
        );
      }
    }

    // Viewport-culling bbox in world coords. In infinite mode this
    // is the inverse map of the canvas's CSS viewport — items whose
    // bbox is entirely outside this box can be skipped per-frame.
    // Pads by 64 world px so off-screen-by-a-hair items still render
    // (matters for strokes whose stroke width pokes into the view).
    // In fixed mode the cull box is the whole source-doc rect, so
    // the gate effectively no-ops.
    let cullX0 = -Infinity, cullY0 = -Infinity, cullX1 = Infinity, cullY1 = Infinity;
    if (infinite && canvasEl) {
      const rect = canvasEl.getBoundingClientRect();
      const pad = 64;
      cullX0 = -session.viewX / session.viewZoom - pad;
      cullY0 = -session.viewY / session.viewZoom - pad;
      cullX1 = (rect.width - session.viewX) / session.viewZoom + pad;
      cullY1 = (rect.height - session.viewY) / session.viewZoom + pad;
    }
    function visible(item: Item): boolean {
      if (!infinite) return true;
      const bb = itemBBox(item);
      return bb.x + bb.w >= cullX0 && bb.x <= cullX1 && bb.y + bb.h >= cullY0 && bb.y <= cullY1;
    }

    // Layers bottom-to-top
    for (const layer of session.doc.layers) {
      if (!layer.visible) continue;
      ctx.save();
      ctx.globalAlpha = layer.opacity;
      for (const item of layer.items) {
        if (!visible(item)) continue;
        drawItem(item);
      }
      ctx.restore();
    }

    // Live previews on top
    if (liveStroke) drawItem(liveStroke);
    if (liveShape) drawItem(liveShape);
    if (liveFrame) drawItem(liveFrame);
    if (liveSticky) drawItem(liveSticky);
    if (liveConnector) drawConnector({ kind: 'connector', ...liveConnector });
    if (liveLasso && liveLasso.length >= 2) drawLassoPreview(liveLasso);
    if (liveCrop) drawCropPreview(liveCrop);
    if (liveRectSelect) drawRectSelectPreview(liveRectSelect);
    // Snap hint — render a small ring at the hover anchor when
    // connector tool is active. Helps the user understand where
    // their next click will attach.
    if (hoverAnchor && session.tool === 'connector') {
      const z = infinite ? session.viewZoom : 1;
      const r = 6 / z;
      ctx.save();
      ctx.strokeStyle = '#3b82f6';
      ctx.fillStyle = 'rgba(59,130,246,0.25)';
      ctx.lineWidth = 2 / z;
      ctx.beginPath();
      ctx.arc(hoverAnchor.anchor.x, hoverAnchor.anchor.y, r, 0, Math.PI * 2);
      ctx.fill();
      ctx.stroke();
      ctx.restore();
    }
  }

  function drawRectSelectPreview(r: { x: number; y: number; w: number; h: number }) {
    if (!ctx) return;
    const x = Math.min(r.x, r.x + r.w);
    const y = Math.min(r.y, r.y + r.h);
    const w = Math.abs(r.w);
    const h = Math.abs(r.h);
    ctx.save();
    ctx.strokeStyle = 'rgba(59, 130, 246, 0.9)';
    ctx.fillStyle = 'rgba(59, 130, 246, 0.08)';
    ctx.lineWidth = 1.5;
    ctx.setLineDash([6, 4]);
    ctx.fillRect(x, y, w, h);
    ctx.strokeRect(x, y, w, h);
    ctx.restore();
  }

  function drawLassoPreview(poly: number[][]) {
    if (!ctx) return;
    ctx.save();
    ctx.strokeStyle = 'rgba(59, 130, 246, 0.9)';  // accent blue
    ctx.fillStyle = 'rgba(59, 130, 246, 0.10)';
    ctx.lineWidth = 1.5;
    ctx.setLineDash([6, 4]);
    ctx.beginPath();
    ctx.moveTo(poly[0][0], poly[0][1]);
    for (let i = 1; i < poly.length; i++) ctx.lineTo(poly[i][0], poly[i][1]);
    ctx.closePath();
    ctx.fill();
    ctx.stroke();
    ctx.restore();
  }

  function drawCropPreview(c: { x: number; y: number; w: number; h: number }) {
    if (!ctx) return;
    const x = Math.min(c.x, c.x + c.w);
    const y = Math.min(c.y, c.y + c.h);
    const w = Math.abs(c.w);
    const h = Math.abs(c.h);
    ctx.save();
    // Dim everything outside the crop rect — four bars around it.
    ctx.fillStyle = 'rgba(0, 0, 0, 0.4)';
    ctx.fillRect(0, 0, session.doc.source_w, y);
    ctx.fillRect(0, y, x, h);
    ctx.fillRect(x + w, y, session.doc.source_w - (x + w), h);
    ctx.fillRect(0, y + h, session.doc.source_w, session.doc.source_h - (y + h));
    // Dashed outline of the crop itself.
    ctx.strokeStyle = '#ffffff';
    ctx.lineWidth = 2;
    ctx.setLineDash([8, 6]);
    ctx.strokeRect(x, y, w, h);
    ctx.restore();
  }

  function drawItem(item: Item) {
    if (!ctx) return;
    switch (item.kind) {
      case 'stroke': return drawStroke(item);
      case 'shape': return drawShape(item);
      case 'text': return drawText(item);
      case 'image': return drawImageItem(item);
      case 'connector': return drawConnector(item);
      case 'frame': return drawFrame(item);
      case 'sticky': return drawSticky(item);
      case 'mindmap': return drawMindmap(item);
    }
  }

  function drawStroke(stroke: StrokeItem) {
    if (!ctx) return;
    if (stroke.points.length === 0) return;

    // Stamp-based brush path (Phase 1.21). When the stroke carries a
    // `stampId` we walk the centerline + stamp a tinted bitmap at
    // every spacing interval. Falls back to perfect-freehand below
    // if the stamp isn't loaded yet (Image still decoding) so the
    // user sees *something* while the stamp resolves.
    if (stroke.stampId) {
      const stamp = getStamp(stroke.stampId);
      if (stamp) {
        const tinted = getTintedStamp(stamp, stroke.color);
        if (tinted) {
          drawStampedStroke(stroke, stamp, tinted);
          return;
        }
      }
      // Stamp missing / not loaded — fall through to PF outline so
      // the stroke is still visible. Will re-render with the right
      // visual on the next $effect tick once the image loads.
    }

    const style = stroke.brushStyle ?? 'default';
    const opts = strokeOptionsFor(stroke.tool, style);
    const outline = getStroke(
      stroke.points.map((p) => [p[0], p[1], p[2] ?? 0.5]) as number[][],
      { ...opts, size: stroke.width },
    );
    if (outline.length === 0) return;
    ctx.save();
    if (stroke.tool === 'eraser') {
      ctx.globalCompositeOperation = 'destination-out';
      ctx.fillStyle = '#000';
    } else {
      ctx.globalCompositeOperation = 'source-over';
      ctx.fillStyle = stroke.color;
      const baseAlpha = (stroke.opacity ?? 1);
      // Watercolor builds up via low per-stroke alpha — overlapping
      // strokes accumulate without going opaque immediately.
      const effectiveAlpha = stroke.tool === 'pen' && style === 'watercolor' ? baseAlpha * 0.35 : baseAlpha;
      ctx.globalAlpha = (ctx.globalAlpha ?? 1) * effectiveAlpha;
    }
    const path = new Path2D();
    path.moveTo(outline[0][0], outline[0][1]);
    for (let i = 1; i < outline.length; i++) path.lineTo(outline[i][0], outline[i][1]);
    path.closePath();
    ctx.fill(path);

    // ── Per-style overlay effects ──────────────────────────────
    if (stroke.tool === 'pen' && stroke.points.length > 1) {
      if (style === 'airbrush') drawAirbrushScatter(stroke);
      else if (style === 'crayon') drawCrayonNoise(stroke);
      else if (style === 'pencil') drawPencilGrain(stroke);
    }
    ctx.restore();
  }

  /** Phase 1.21b — Stamp-based stroke rendering. Walks the raw
   *  sample path at fixed intervals (spacing × stamp diameter,
   *  GIMP / Photoshop convention) and draws the pre-tinted stamp
   *  at each step. The math:
   *
   *    1. Resample the path into a list of {x, y, pressure, t}
   *       where t is cumulative arc length in source-canvas px.
   *    2. Walk from t=0 to total length, stepping by `step` where
   *       step = max(1, spacing × stamp diameter × pressure).
   *    3. At each step, interpolate (x, y, pressure) from the
   *       resampled path, optionally compute the tangent angle,
   *       optionally apply jitter, draw the stamp.
   *
   *  Performance: linear in number of stamps. Pre-tinted offscreen
   *  canvas means each stamp is one drawImage. For a 200-stamp
   *  stroke at 60 fps that's 12k drawImage/sec — well within
   *  modern canvas budgets. */
  function drawStampedStroke(stroke: StrokeItem, stamp: BrushStamp, tinted: HTMLCanvasElement) {
    if (!ctx) return;
    const pts = stroke.points;
    if (pts.length === 0) return;
    const baseAlpha = stroke.opacity ?? 1;
    const stampW = tinted.width;
    // Min step of 1 source-px so degenerate (spacing=0) stamps
    // don't spin in a infinite-density loop. Max step capped at
    // stamp diameter so very-sparse spacing still lays down at
    // least the perimeter density.
    const baseStep = Math.max(1, Math.min(stampW, (stamp.spacing || 0.1) * stroke.width));

    ctx.save();
    ctx.globalAlpha = (ctx.globalAlpha ?? 1) * baseAlpha;

    // Walk segments and stamp at each interval. `leftover` carries
    // sub-step distance from the previous segment so spacing stays
    // even across path joins (rather than re-counting from each
    // segment start, which produces visible clumping at corners).
    let leftover = 0;
    // Always stamp at the first point so single-tap strokes leave
    // a dot rather than nothing.
    stampAt(stroke, stamp, tinted, pts[0][0], pts[0][1], 0, pts[0][2] ?? 0.5);
    for (let i = 1; i < pts.length; i++) {
      const a = pts[i - 1];
      const b = pts[i];
      const dx = b[0] - a[0];
      const dy = b[1] - a[1];
      const segLen = Math.hypot(dx, dy);
      if (segLen === 0) continue;
      const ang = Math.atan2(dy, dx);
      let t = baseStep - leftover;
      while (t < segLen) {
        const u = t / segLen;
        const px = a[0] + dx * u;
        const py = a[1] + dy * u;
        const pp = ((a[2] ?? 0.5) * (1 - u)) + ((b[2] ?? 0.5) * u);
        stampAt(stroke, stamp, tinted, px, py, ang, pp);
        t += baseStep;
      }
      leftover = segLen - (t - baseStep);
    }
    ctx.restore();
  }

  /** One stamp placement. Size + opacity scale with pressure; jitter
   *  (when configured on the stamp) perturbs each placement. */
  function stampAt(stroke: StrokeItem, stamp: BrushStamp, tinted: HTMLCanvasElement, x: number, y: number, ang: number, pressure: number) {
    if (!ctx) return;
    const stampW = tinted.width;
    const stampH = tinted.height;
    // Effective stamp size in source-canvas px: scale the source
    // bitmap so its width matches the stroke's brush width × pressure.
    // (Photoshop's "Size" dynamic — bigger pressure = bigger stamp.)
    const sizeJ = stamp.sizeJitter ? (Math.random() * 2 - 1) * stamp.sizeJitter : 0;
    const scale = (stroke.width / stampW) * (0.5 + 0.5 * pressure) * (1 + sizeJ);
    const drawW = stampW * scale;
    const drawH = stampH * scale;
    if (drawW < 0.5 || drawH < 0.5) return;
    const opJ = stamp.opacityJitter ? Math.random() * stamp.opacityJitter : 0;
    const prevAlpha = ctx.globalAlpha;
    ctx.globalAlpha = prevAlpha * (1 - opJ);
    let rot = stamp.alignToPath ? ang : 0;
    if (stamp.angleJitter) {
      rot += (Math.random() * 2 - 1) * (stamp.angleJitter * Math.PI / 180);
    }
    if (rot !== 0) {
      ctx.save();
      ctx.translate(x, y);
      ctx.rotate(rot);
      ctx.drawImage(tinted, -drawW / 2, -drawH / 2, drawW, drawH);
      ctx.restore();
    } else {
      ctx.drawImage(tinted, x - drawW / 2, y - drawH / 2, drawW, drawH);
    }
    ctx.globalAlpha = prevAlpha;
  }

  /** Airbrush — scatter soft circles along the stroke. Sparser at
   *  the center of the stroke than the edge so the "spray" reads
   *  outward. Density scales with width so big brushes look puffier. */
  function drawAirbrushScatter(stroke: StrokeItem) {
    if (!ctx) return;
    ctx.save();
    ctx.fillStyle = stroke.color;
    ctx.globalAlpha = (stroke.opacity ?? 1) * 0.18;
    const r = stroke.width * 0.8;
    const dotR = Math.max(1, stroke.width * 0.08);
    // Deterministic-ish PRNG seeded by point coords so re-renders
    // produce the same scatter pattern.
    function rng(seed: number) { let x = Math.sin(seed) * 10000; return x - Math.floor(x); }
    for (let i = 1; i < stroke.points.length; i++) {
      const [px, py] = stroke.points[i];
      const dotCount = Math.max(3, Math.round(stroke.width / 4));
      for (let d = 0; d < dotCount; d++) {
        const a = rng(px * 13 + py * 17 + d) * Math.PI * 2;
        const dist = rng(px * 7 + py * 23 + d * 3) * r;
        ctx.beginPath();
        ctx.arc(px + Math.cos(a) * dist, py + Math.sin(a) * dist, dotR, 0, Math.PI * 2);
        ctx.fill();
      }
    }
    ctx.restore();
  }

  /** Crayon — broken edge texture: scatter low-alpha dots perpendicular
   *  to the stroke direction, denser near the edges. Reads as wax-on-
   *  paper rough texture. */
  function drawCrayonNoise(stroke: StrokeItem) {
    if (!ctx) return;
    ctx.save();
    ctx.fillStyle = stroke.color;
    ctx.globalAlpha = (stroke.opacity ?? 1) * 0.45;
    const dotR = Math.max(0.6, stroke.width * 0.06);
    function rng(seed: number) { let x = Math.sin(seed) * 10000; return x - Math.floor(x); }
    for (let i = 1; i < stroke.points.length; i++) {
      const [px, py] = stroke.points[i];
      const [ppx, ppy] = stroke.points[i - 1];
      const dx = px - ppx, dy = py - ppy;
      const len = Math.hypot(dx, dy) || 1;
      const nx = -dy / len, ny = dx / len;
      for (let d = 0; d < 4; d++) {
        const t = rng(px * 11 + py * 19 + d * 5) * 2 - 1;
        const off = t * stroke.width * 0.45;
        ctx.beginPath();
        ctx.arc(px + nx * off, py + ny * off, dotR, 0, Math.PI * 2);
        ctx.fill();
      }
    }
    ctx.restore();
  }

  /** Pencil — thin streaks along the stroke at varied opacity to
   *  fake graphite grain. Lighter than crayon. */
  function drawPencilGrain(stroke: StrokeItem) {
    if (!ctx) return;
    ctx.save();
    ctx.strokeStyle = stroke.color;
    ctx.lineWidth = Math.max(0.3, stroke.width * 0.12);
    ctx.lineCap = 'round';
    function rng(seed: number) { let x = Math.sin(seed) * 10000; return x - Math.floor(x); }
    for (let i = 1; i < stroke.points.length; i++) {
      const [px, py] = stroke.points[i];
      const [ppx, ppy] = stroke.points[i - 1];
      const dx = px - ppx, dy = py - ppy;
      const len = Math.hypot(dx, dy) || 1;
      const nx = -dy / len, ny = dx / len;
      ctx.globalAlpha = (stroke.opacity ?? 1) * (0.2 + rng(px * 7 + py * 13) * 0.4);
      const off = (rng(px * 5 + py * 31) - 0.5) * stroke.width * 0.6;
      ctx.beginPath();
      ctx.moveTo(ppx + nx * off, ppy + ny * off);
      ctx.lineTo(px + nx * off, py + ny * off);
      ctx.stroke();
    }
    ctx.restore();
  }

  function drawShape(s: ShapeItem) {
    if (!ctx) return;
    // Normalize so negative w/h still draw correctly.
    const x = s.w >= 0 ? s.x : s.x + s.w;
    const y = s.h >= 0 ? s.y : s.y + s.h;
    const w = Math.abs(s.w);
    const h = Math.abs(s.h);
    // C-1.17 — outline + fill colors are independent. Legacy saves
    // only have `color`; fall back to it when the explicit field is
    // missing so old whiteboards render the same.
    const strokeC = s.strokeColor ?? s.color;
    const fillC = s.fillColor ?? s.color;
    ctx.save();
    if (s.rotation) {
      const cx = x + w / 2;
      const cy = y + h / 2;
      ctx.translate(cx, cy);
      ctx.rotate((s.rotation * Math.PI) / 180);
      ctx.translate(-cx, -cy);
    }
    ctx.strokeStyle = strokeC;
    ctx.lineWidth = s.width;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    ctx.globalAlpha = (ctx.globalAlpha ?? 1) * (s.opacity ?? 1);
    if (s.tool === 'line') {
      ctx.beginPath();
      ctx.moveTo(s.x, s.y);
      ctx.lineTo(s.x + s.w, s.y + s.h);
      ctx.stroke();
    } else if (s.tool === 'arrow') {
      drawArrow(s.x, s.y, s.x + s.w, s.y + s.h, s.width);
    } else if (s.tool === 'rect') {
      if (s.fill && s.fill > 0) {
        ctx.fillStyle = fillC;
        const prevAlpha = ctx.globalAlpha;
        ctx.globalAlpha = prevAlpha * s.fill;
        ctx.fillRect(x, y, w, h);
        ctx.globalAlpha = prevAlpha;
      }
      if (s.width > 0) ctx.strokeRect(x, y, w, h);
    } else if (s.tool === 'ellipse') {
      ctx.beginPath();
      ctx.ellipse(x + w / 2, y + h / 2, w / 2, h / 2, 0, 0, Math.PI * 2);
      if (s.fill && s.fill > 0) {
        ctx.fillStyle = fillC;
        const prevAlpha = ctx.globalAlpha;
        ctx.globalAlpha = prevAlpha * s.fill;
        ctx.fill();
        ctx.globalAlpha = prevAlpha;
      }
      if (s.width > 0) ctx.stroke();
    } else if (s.tool === 'triangle') {
      // Isoceles triangle: apex at top-center, base across bottom.
      ctx.beginPath();
      ctx.moveTo(x + w / 2, y);
      ctx.lineTo(x + w, y + h);
      ctx.lineTo(x, y + h);
      ctx.closePath();
      fillStrokeShape(s);
    } else if (s.tool === 'right-triangle') {
      // Right angle at bottom-left.
      ctx.beginPath();
      ctx.moveTo(x, y);
      ctx.lineTo(x, y + h);
      ctx.lineTo(x + w, y + h);
      ctx.closePath();
      fillStrokeShape(s);
    } else if (s.tool === 'rounded-rect') {
      const r = Math.min(w, h) * 0.18;
      ctx.beginPath();
      ctx.moveTo(x + r, y);
      ctx.arcTo(x + w, y,     x + w, y + h, r);
      ctx.arcTo(x + w, y + h, x,     y + h, r);
      ctx.arcTo(x,     y + h, x,     y,     r);
      ctx.arcTo(x,     y,     x + w, y,     r);
      ctx.closePath();
      fillStrokeShape(s);
    } else if (s.tool === 'diamond') {
      ctx.beginPath();
      ctx.moveTo(x + w / 2, y);
      ctx.lineTo(x + w, y + h / 2);
      ctx.lineTo(x + w / 2, y + h);
      ctx.lineTo(x, y + h / 2);
      ctx.closePath();
      fillStrokeShape(s);
    } else if (s.tool === 'pentagon' || s.tool === 'hexagon') {
      // Regular n-gon centered in the bbox, oriented apex-up.
      const n = s.tool === 'pentagon' ? 5 : 6;
      const cx = x + w / 2;
      const cy = y + h / 2;
      const rx = w / 2;
      const ry = h / 2;
      ctx.beginPath();
      for (let i = 0; i < n; i++) {
        const a = -Math.PI / 2 + (i * 2 * Math.PI) / n;
        const px = cx + Math.cos(a) * rx;
        const py = cy + Math.sin(a) * ry;
        if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
      }
      ctx.closePath();
      fillStrokeShape(s);
    } else if (s.tool === 'star') {
      // 5-point star. Inner radius is the standard 0.382 × outer
      // (golden-ratio derived; matches what PowerPoint / Paint draw).
      const points = 5;
      const cx = x + w / 2;
      const cy = y + h / 2;
      const rxOut = w / 2;
      const ryOut = h / 2;
      const inn = 0.382;
      ctx.beginPath();
      for (let i = 0; i < points * 2; i++) {
        const a = -Math.PI / 2 + (i * Math.PI) / points;
        const r = i % 2 === 0 ? 1 : inn;
        const px = cx + Math.cos(a) * rxOut * r;
        const py = cy + Math.sin(a) * ryOut * r;
        if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
      }
      ctx.closePath();
      fillStrokeShape(s);
    } else if (s.tool === 'heart') {
      // Classic two-arc + V-tip heart. Math: two semicircles at
      // the top, lines converging to the bottom point. Looks right
      // at any aspect ratio because we scale the control points.
      const cx = x + w / 2;
      const top = y;
      const bottom = y + h;
      const r = w * 0.25;
      ctx.beginPath();
      ctx.moveTo(cx, bottom);
      ctx.bezierCurveTo(cx + w * 0.5, y + h * 0.7, x + w, y + h * 0.25, cx, top + h * 0.25);
      ctx.bezierCurveTo(x, y + h * 0.25, cx - w * 0.5, y + h * 0.7, cx, bottom);
      ctx.closePath();
      fillStrokeShape(s);
    } else if (s.tool === 'callout-rect' || s.tool === 'callout-oval') {
      // Speech bubble — main body + a triangular tail at the bottom-
      // left pointing down-and-left. Tail size scales with the body
      // so it reads as a callout at any drag size.
      const tailW = Math.min(w * 0.15, 40);
      const tailH = Math.min(h * 0.2, 40);
      const tailBaseX = x + w * 0.25;
      const tipX = tailBaseX - tailW;
      const tipY = y + h + tailH;
      // Body
      ctx.beginPath();
      if (s.tool === 'callout-rect') {
        const r = Math.min(w, h) * 0.12;
        ctx.moveTo(x + r, y);
        ctx.arcTo(x + w, y,     x + w, y + h, r);
        ctx.arcTo(x + w, y + h, x,     y + h, r);
        ctx.arcTo(x,     y + h, x,     y,     r);
        ctx.arcTo(x,     y,     x + w, y,     r);
        ctx.closePath();
      } else {
        ctx.ellipse(x + w / 2, y + h / 2, w / 2, h / 2, 0, 0, Math.PI * 2);
      }
      // Tail — drawn as a separate triangle on top of the body so
      // the stroke around the body still reads cleanly behind it.
      // Fill-first ordering keeps the join clean.
      const tailPath = new Path2D();
      tailPath.moveTo(tailBaseX, y + h - 1);
      tailPath.lineTo(tipX, tipY);
      tailPath.lineTo(tailBaseX + tailW, y + h - 1);
      tailPath.closePath();
      // First fill body + tail together, then stroke both.
      if (s.fill && s.fill > 0) {
        ctx.fillStyle = fillC;
        const prevAlpha = ctx.globalAlpha;
        ctx.globalAlpha = prevAlpha * s.fill;
        ctx.fill();
        ctx.fill(tailPath);
        ctx.globalAlpha = prevAlpha;
      }
      if (s.width > 0) {
        ctx.stroke();
        ctx.stroke(tailPath);
      }
    }
    ctx.restore();
  }

  /** Shared fill+stroke for the simple-path shapes above. Pull-out
   *  helper because every new shape branch was repeating the same
   *  six lines. Uses strokeColor / fillColor with legacy `color`
   *  fallback (C-1.17). */
  function fillStrokeShape(s: ShapeItem) {
    if (!ctx) return;
    if (s.fill && s.fill > 0) {
      ctx.fillStyle = s.fillColor ?? s.color;
      const prevAlpha = ctx.globalAlpha;
      ctx.globalAlpha = prevAlpha * s.fill;
      ctx.fill();
      ctx.globalAlpha = prevAlpha;
    }
    if (s.width > 0) ctx.stroke();
  }

  function drawArrow(x1: number, y1: number, x2: number, y2: number, w: number) {
    if (!ctx) return;
    const dx = x2 - x1;
    const dy = y2 - y1;
    const len = Math.hypot(dx, dy);
    if (len < 1) return;
    // Triangular head scales with line width, capped so giant strokes
    // don't get cartoon arrows.
    const head = Math.min(len * 0.25, Math.max(12, w * 4));
    const ang = Math.atan2(dy, dx);
    // Line stops short of the tip so the head sits clean on the end.
    const lx = x2 - Math.cos(ang) * head * 0.6;
    const ly = y2 - Math.sin(ang) * head * 0.6;
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    ctx.lineTo(lx, ly);
    ctx.stroke();
    // Filled arrowhead.
    ctx.beginPath();
    ctx.moveTo(x2, y2);
    ctx.lineTo(
      x2 - Math.cos(ang - Math.PI / 6) * head,
      y2 - Math.sin(ang - Math.PI / 6) * head,
    );
    ctx.lineTo(
      x2 - Math.cos(ang + Math.PI / 6) * head,
      y2 - Math.sin(ang + Math.PI / 6) * head,
    );
    ctx.closePath();
    ctx.fillStyle = ctx.strokeStyle;
    ctx.fill();
  }

  /** Phase 1.22 — render a connector. Resolves both endpoints to
   *  absolute world coords (handles attached-anchor follow), then
   *  paints the path according to `mode`. End-tangent for curve
   *  mode is the perpendicular-to-edge of the attached shape side
   *  the anchor sits on (top/right/bottom/left) so the curve enters
   *  the shape cleanly rather than parallel to its edge. */
  function drawConnector(c: ConnectorItem) {
    if (!ctx) return;
    const s = resolveConnectorEndpoint(c.start, session.doc);
    const e = resolveConnectorEndpoint(c.end, session.doc);
    if (Math.hypot(e.x - s.x, e.y - s.y) < 0.5) return;
    ctx.save();
    ctx.strokeStyle = c.color;
    ctx.fillStyle = c.color;
    ctx.lineWidth = c.width;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    ctx.globalAlpha = (ctx.globalAlpha ?? 1) * (c.opacity ?? 1);
    // Pick the path geometry per mode.
    ctx.beginPath();
    if (c.mode === 'straight') {
      ctx.moveTo(s.x, s.y);
      ctx.lineTo(e.x, e.y);
    } else if (c.mode === 'orthogonal') {
      // Single-elbow heuristic: bend the corner along whichever
      // axis is longer. Two-elbow A* pathfinding around obstacles
      // is a future polish — single-elbow already covers most
      // real diagram cases (boxes to boxes, no overlapping items).
      ctx.moveTo(s.x, s.y);
      const dx = e.x - s.x;
      const dy = e.y - s.y;
      if (Math.abs(dx) > Math.abs(dy)) {
        // Horizontal-first: out to half-way x, then drop to e.y,
        // then over to e.x. Reads like a step staircase.
        const midX = s.x + dx / 2;
        ctx.lineTo(midX, s.y);
        ctx.lineTo(midX, e.y);
        ctx.lineTo(e.x, e.y);
      } else {
        const midY = s.y + dy / 2;
        ctx.lineTo(s.x, midY);
        ctx.lineTo(e.x, midY);
        ctx.lineTo(e.x, e.y);
      }
    } else {
      // Curve: cubic bezier with tangents derived from end-of-edge
      // when attached, or 1/3-of-distance when free. Tangent length
      // scales with endpoint separation so short connectors stay
      // tight + long ones flow.
      const len = Math.hypot(e.x - s.x, e.y - s.y);
      const t = Math.min(160, len * 0.4);
      const sT = endpointTangent(c.start, s, e, t);
      const eT = endpointTangent(c.end, e, s, t);
      ctx.moveTo(s.x, s.y);
      ctx.bezierCurveTo(s.x + sT.x, s.y + sT.y, e.x + eT.x, e.y + eT.y, e.x, e.y);
    }
    ctx.stroke();
    // Arrow heads.
    const startArrow = c.startArrow ?? 'none';
    const endArrow = c.endArrow ?? 'arrow';
    if (startArrow !== 'none') drawConnectorHead(s, e, c.width, startArrow);
    if (endArrow !== 'none') drawConnectorHead(e, s, c.width, endArrow);
    ctx.restore();
  }

  /** Endpoint tangent for curve mode. When the endpoint is attached
   *  to a shape, the tangent points away from the shape (out of the
   *  edge the anchor sits on); when free, it points toward the
   *  other endpoint. `len` is the desired tangent magnitude. */
  function endpointTangent(ep: ConnectorEndpoint, me: { x: number; y: number }, other: { x: number; y: number }, len: number): { x: number; y: number } {
    if (ep.attached) {
      const u = ep.u ?? 0.5;
      const v = ep.v ?? 0.5;
      // Determine which edge of the bbox the anchor sits on by the
      // (u, v) values: 0 / 1 = on an edge; 0.5 / 0.5 = center.
      // Point the tangent outward through that edge.
      let nx = 0, ny = 0;
      if (u <= 0.01) nx = -1;
      else if (u >= 0.99) nx = 1;
      if (v <= 0.01) ny = -1;
      else if (v >= 0.99) ny = 1;
      if (nx === 0 && ny === 0) {
        // Center anchor — fall back to "toward other endpoint".
        const dx = other.x - me.x;
        const dy = other.y - me.y;
        const d = Math.hypot(dx, dy) || 1;
        return { x: (dx / d) * len, y: (dy / d) * len };
      }
      const m = Math.hypot(nx, ny) || 1;
      return { x: (nx / m) * len, y: (ny / m) * len };
    }
    const dx = other.x - me.x;
    const dy = other.y - me.y;
    const d = Math.hypot(dx, dy) || 1;
    return { x: (dx / d) * len, y: (dy / d) * len };
  }

  /** Filled arrow / dot at one end. `from` is the head position,
   *  `toward` is the other endpoint (defines the arrow's angle). */
  function drawConnectorHead(from: { x: number; y: number }, toward: { x: number; y: number }, w: number, kind: 'arrow' | 'dot') {
    if (!ctx) return;
    if (kind === 'dot') {
      const r = Math.max(2.5, w * 1.6);
      ctx.beginPath();
      ctx.arc(from.x, from.y, r, 0, Math.PI * 2);
      ctx.fill();
      return;
    }
    // Arrow head: triangle pointing from `toward` to `from`.
    const dx = from.x - toward.x;
    const dy = from.y - toward.y;
    const ang = Math.atan2(dy, dx);
    const head = Math.max(10, w * 4);
    ctx.beginPath();
    ctx.moveTo(from.x, from.y);
    ctx.lineTo(
      from.x - Math.cos(ang - Math.PI / 6) * head,
      from.y - Math.sin(ang - Math.PI / 6) * head,
    );
    ctx.lineTo(
      from.x - Math.cos(ang + Math.PI / 6) * head,
      from.y - Math.sin(ang + Math.PI / 6) * head,
    );
    ctx.closePath();
    ctx.fill();
  }

  /** Phase 1.23 — Frame: a labelled boundary rectangle. Used to
   *  visually group items + as a slide-like region for export
   *  later. We draw a thin rounded-rect border + an optional
   *  title bar above. NOT filled: items inside show through. */
  function drawFrame(f: FrameItem) {
    if (!ctx) return;
    const x = f.w >= 0 ? f.x : f.x + f.w;
    const y = f.h >= 0 ? f.y : f.y + f.h;
    const w = Math.abs(f.w);
    const h = Math.abs(f.h);
    if (w < 1 || h < 1) return;
    ctx.save();
    if (f.rotation) {
      const cx = x + w / 2;
      const cy = y + h / 2;
      ctx.translate(cx, cy);
      ctx.rotate((f.rotation * Math.PI) / 180);
      ctx.translate(-cx, -cy);
    }
    const color = f.color ?? '#94a3b8'; // slate-400 default
    const r = 6;
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.5;
    // Border
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y,     x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x,     y + h, r);
    ctx.arcTo(x,     y + h, x,     y,     r);
    ctx.arcTo(x,     y,     x + w, y,     r);
    ctx.closePath();
    ctx.stroke();
    // Title bar above the frame — bg pill + text. Font scales
    // mildly with viewport so labels stay readable at all zooms
    // without becoming dominant.
    if (f.title) {
      const fontSize = 14;
      ctx.font = `500 ${fontSize}px system-ui, sans-serif`;
      const textMetrics = ctx.measureText(f.title);
      const padX = 8;
      const padY = 4;
      const labelW = textMetrics.width + padX * 2;
      const labelH = fontSize + padY * 2;
      ctx.fillStyle = color;
      // Pill background, just above the frame.
      ctx.beginPath();
      const lr = labelH / 2;
      const lx = x;
      const ly = y - labelH - 4;
      ctx.moveTo(lx + lr, ly);
      ctx.arcTo(lx + labelW, ly,            lx + labelW, ly + labelH, lr);
      ctx.arcTo(lx + labelW, ly + labelH,   lx,          ly + labelH, lr);
      ctx.arcTo(lx,          ly + labelH,   lx,          ly,          lr);
      ctx.arcTo(lx,          ly,            lx + labelW, ly,          lr);
      ctx.closePath();
      ctx.fill();
      // Title text in white on the pill.
      ctx.fillStyle = '#fff';
      ctx.textBaseline = 'middle';
      ctx.fillText(f.title, lx + padX, ly + labelH / 2);
    }
    ctx.restore();
  }

  /** Phase 1.23 — StickyNote: a colored card with editable text.
   *  Background uses a slight gradient + drop shadow to read as
   *  a paper post-it. Text wraps automatically inside the card
   *  bounds. Foreground color auto-derives from background
   *  luminance for legibility. */
  function drawSticky(s: StickyNoteItem) {
    if (!ctx) return;
    const w = Math.abs(s.w);
    const h = Math.abs(s.h);
    if (w < 1 || h < 1) return;
    ctx.save();
    if (s.rotation) {
      const cx = s.x + w / 2;
      const cy = s.y + h / 2;
      ctx.translate(cx, cy);
      ctx.rotate((s.rotation * Math.PI) / 180);
      ctx.translate(-cx, -cy);
    }
    const bg = s.background ?? '#fef08a';
    // Drop shadow — short + soft so the note feels resting on the
    // canvas without dominating. ctx.shadowOffset works in-canvas;
    // we draw the rect, save the shadow, then clear shadow before
    // drawing the text so the text doesn't double-blur.
    ctx.shadowColor = 'rgba(0,0,0,0.18)';
    ctx.shadowBlur = 8;
    ctx.shadowOffsetX = 0;
    ctx.shadowOffsetY = 2;
    ctx.fillStyle = bg;
    ctx.fillRect(s.x, s.y, w, h);
    ctx.shadowColor = 'transparent';
    ctx.shadowBlur = 0;
    ctx.shadowOffsetX = 0;
    ctx.shadowOffsetY = 0;
    if (s.body) {
      const fg = s.color ?? autoTextColor(bg);
      const fontSize = s.fontSize ?? 18;
      const family = s.fontFamily ? `"${s.fontFamily}", system-ui` : 'system-ui, sans-serif';
      ctx.fillStyle = fg;
      ctx.font = `500 ${fontSize}px ${family}`;
      ctx.textBaseline = 'top';
      ctx.textAlign = 'left';
      const padding = 12;
      wrapTextInside(s.body, s.x + padding, s.y + padding, w - padding * 2, fontSize * 1.3);
    }
    ctx.restore();
  }

  /** Pick a readable text color (black or white) for a given hex
   *  background. Uses the WCAG-recommended sRGB-relative-luminance
   *  threshold. Returns #000 for light bgs, #fff for dark ones —
   *  the most legible for sticky-note paper feel. */
  /** Phase 1.24 — Mindmap renderer. Lays out the tree (horizontal,
   *  root on left), draws curved connector lines from each parent
   *  to its children, then draws node bubbles on top. Branch color
   *  cycles per top-level subtree so the tree reads as branches.
   *
   *  The node bubble is a pill with text inside. Collapsed nodes
   *  show a small +N circle indicating hidden child count. */
  function drawMindmap(m: MindmapItem) {
    if (!ctx) return;
    // Capture ctx into a non-nullable local so the closures below
    // (walkMindmap callbacks) inherit the narrowing — TS can't
    // prove `ctx` stayed non-null through the walk function call.
    const c2d = ctx;
    const layout = layoutMindmap(m);
    const defaultPalette = ['#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#a855f7', '#06b6d4', '#ec4899', '#84cc16'];
    const palette = m.branchColors ?? defaultPalette;
    // Walk to draw edges, tagging branch index by top-level child.
    c2d.save();
    if (m.rotation) {
      const cx = (layout.minX + layout.maxX) / 2;
      const cy = (layout.minY + layout.maxY) / 2;
      ctx.translate(cx, cy);
      ctx.rotate((m.rotation * Math.PI) / 180);
      ctx.translate(-cx, -cy);
    }
    // Determine branch index for every node: same as the index of
    // its top-level ancestor (or -1 for the root itself).
    const branchOf = new Map<string, number>();
    branchOf.set(m.root.id, -1);
    m.root.children.forEach((c, i) => {
      walkMindmap(c, (n) => branchOf.set(n.id, i));
    });
    // Edges first so node bubbles paint on top of the line ends.
    walkMindmap(m.root, (n, _depth, parent) => {
      if (!parent) return;
      const pp = layout.positions.get(parent.id);
      const cp = layout.positions.get(n.id);
      if (!pp || !cp) return;
      const branchIdx = branchOf.get(n.id) ?? 0;
      const color = n.color ?? palette[((branchIdx >= 0 ? branchIdx : 0)) % palette.length];
      // Cubic bezier from parent's right edge to child's left edge,
      // tangents horizontal — gives the classic mindmap "branch
      // curve" look.
      const px = pp.x + pp.w;
      const py = pp.y + pp.h / 2;
      const cx = cp.x;
      const cy = cp.y + cp.h / 2;
      const t = Math.max(20, (cx - px) * 0.5);
      c2d.strokeStyle = color;
      c2d.lineWidth = 2;
      c2d.beginPath();
      c2d.moveTo(px, py);
      c2d.bezierCurveTo(px + t, py, cx - t, cy, cx, cy);
      c2d.stroke();
    });
    // Nodes second.
    walkMindmap(m.root, (n) => {
      const pos = layout.positions.get(n.id);
      if (!pos) return;
      const isRoot = n.id === m.root.id;
      const branchIdx = branchOf.get(n.id) ?? -1;
      const color = n.color ?? (isRoot ? '#0f172a' : palette[((branchIdx >= 0 ? branchIdx : 0)) % palette.length]);
      // Pill bubble.
      const r = pos.h / 2;
      c2d.fillStyle = isRoot ? color : '#ffffff';
      c2d.strokeStyle = color;
      c2d.lineWidth = isRoot ? 0 : 2;
      c2d.beginPath();
      c2d.moveTo(pos.x + r, pos.y);
      c2d.arcTo(pos.x + pos.w, pos.y,            pos.x + pos.w, pos.y + pos.h, r);
      c2d.arcTo(pos.x + pos.w, pos.y + pos.h,    pos.x,         pos.y + pos.h, r);
      c2d.arcTo(pos.x,         pos.y + pos.h,    pos.x,         pos.y,         r);
      c2d.arcTo(pos.x,         pos.y,            pos.x + pos.w, pos.y,         r);
      c2d.closePath();
      c2d.fill();
      if (!isRoot) c2d.stroke();
      // Label.
      c2d.fillStyle = isRoot ? '#ffffff' : '#0f172a';
      c2d.font = '500 14px system-ui, sans-serif';
      c2d.textBaseline = 'middle';
      c2d.textAlign = 'center';
      c2d.fillText(n.label, pos.x + pos.w / 2, pos.y + pos.h / 2);
      // Collapsed indicator — small filled circle with the hidden
      // child count, sitting at the right edge.
      if (n.collapsed && n.children.length > 0) {
        const ix = pos.x + pos.w + 4;
        const iy = pos.y + pos.h / 2;
        c2d.beginPath();
        c2d.arc(ix + 8, iy, 8, 0, Math.PI * 2);
        c2d.fillStyle = color;
        c2d.fill();
        c2d.fillStyle = '#ffffff';
        c2d.font = 'bold 10px system-ui, sans-serif';
        c2d.fillText(String(n.children.length), ix + 8, iy);
      }
    });
    c2d.restore();
  }

  function autoTextColor(hex: string): string {
    const m = /^#?([0-9a-f]{6})$/i.exec(hex);
    if (!m) return '#000';
    const n = parseInt(m[1], 16);
    const r = (n >> 16) & 0xff;
    const g = (n >> 8) & 0xff;
    const b = n & 0xff;
    // sRGB → linear → relative luminance. Threshold 140 picks dark
    // text for the post-it yellow + light text for slate/navy
    // backgrounds.
    const lum = 0.299 * r + 0.587 * g + 0.114 * b;
    return lum > 140 ? '#0f172a' : '#ffffff';
  }

  /** Word-wrap text within a fixed pixel width on the canvas.
   *  Splits on whitespace + measures cumulative width per token,
   *  starting a new line when the next token would overflow.
   *  Newlines in the source are respected. */
  function wrapTextInside(body: string, x: number, y: number, maxW: number, lineH: number) {
    if (!ctx) return;
    const paragraphs = body.split('\n');
    let curY = y;
    for (const para of paragraphs) {
      const words = para.split(/\s+/).filter(Boolean);
      if (words.length === 0) { curY += lineH; continue; }
      let line = '';
      for (const word of words) {
        const candidate = line ? line + ' ' + word : word;
        if (ctx.measureText(candidate).width > maxW && line) {
          ctx.fillText(line, x, curY);
          curY += lineH;
          line = word;
        } else {
          line = candidate;
        }
      }
      if (line) {
        ctx.fillText(line, x, curY);
        curY += lineH;
      }
    }
  }

  function drawText(t: TextItem) {
    if (!ctx) return;
    if (!t.body) return;
    ctx.save();
    if (t.rotation) {
      const cx = t.x + t.w / 2;
      const cy = t.y + t.h / 2;
      ctx.translate(cx, cy);
      ctx.rotate((t.rotation * Math.PI) / 180);
      ctx.translate(-cx, -cy);
    }
    ctx.fillStyle = t.color;
    ctx.globalAlpha = (ctx.globalAlpha ?? 1);
    const weight = t.bold ? '700' : '500';
    const style = t.italic ? 'italic ' : '';
    // Quote the font-family for safety when it contains spaces
    // ("Open Sans", "Permanent Marker", etc).
    const family = t.fontFamily ? `"${t.fontFamily}", system-ui, sans-serif` : 'system-ui, sans-serif';
    ctx.font = `${style}${weight} ${t.fontSize}px ${family}`;
    ctx.textBaseline = 'top';
    ctx.textAlign = t.align ?? 'left';
    const x = t.align === 'center' ? t.x + t.w / 2 : (t.align === 'right' ? t.x + t.w : t.x);
    const lines = t.body.split('\n');
    const lineH = t.fontSize * 1.25;
    for (let i = 0; i < lines.length; i++) {
      ctx.fillText(lines[i], x, t.y + i * lineH);
    }
    ctx.restore();
  }

  function drawImageItem(item: ImageItem) {
    if (!ctx) return;
    const img = loadImage(item.src);
    if (!img.complete || img.naturalWidth === 0) return; // re-renders onload
    ctx.save();
    if (item.rotation) {
      const cx = item.x + item.w / 2;
      const cy = item.y + item.h / 2;
      ctx.translate(cx, cy);
      ctx.rotate((item.rotation * Math.PI) / 180);
      ctx.translate(-cx, -cy);
    }
    ctx.drawImage(img, item.x, item.y, item.w, item.h);
    ctx.restore();
  }

  // ── Pan + zoom (infinite mode) ────────────────────────────────────
  //
  // Three pan gestures, mirroring Figma / Miro conventions:
  //   - Middle-mouse drag — always pans regardless of active tool
  //   - Space + left-drag — Photoshop's hand-tool toggle
  //   - Two-finger trackpad drag without ctrl — handled via wheel
  //     deltaX/deltaY when ctrlKey is false (browser convention)
  //
  // Wheel zoom-at-cursor: ctrl/cmd + wheel OR pinch-zoom (which
  // browsers report as wheel + ctrlKey). Anchor the zoom on the
  // cursor's CSS position so the world point under it stays put —
  // Miro / Figma behaviour.
  //
  // The gesture state lives outside selectGesture so it doesn't
  // tangle with item-resize / rotate transforms.
  let panGesture: { startCssX: number; startCssY: number; startViewX: number; startViewY: number } | null = null;
  let spaceHeld = $state(false);

  function onWheel(e: WheelEvent) {
    if (!infinite || readOnly) return;
    if (!canvasEl) return;
    const rect = canvasEl.getBoundingClientRect();
    const ax = e.clientX - rect.left;
    const ay = e.clientY - rect.top;
    if (e.ctrlKey || e.metaKey) {
      // Zoom. deltaY > 0 → zoom out. Step is tuned so a single
      // detent on a typical mouse wheel = ~1.15× change (gentle but
      // visible); trackpad pinch arrives as many small deltas so it
      // still feels smooth.
      e.preventDefault();
      const factor = Math.pow(1.0015, -e.deltaY);
      session.zoomBy(factor, ax, ay);
      return;
    }
    // Plain wheel = pan. Two-finger trackpad scroll fires the
    // same shape with both deltaX and deltaY; mouse wheel only
    // fires deltaY (vertical), shift+wheel fires deltaX. We honor
    // both axes either way.
    e.preventDefault();
    session.panBy(-e.deltaX, -e.deltaY);
  }

  function onPanPointerDown(e: PointerEvent): boolean {
    // Returns true if the pan handler claimed the event so the
    // tool's normal pointerdown branch should skip.
    if (!infinite || readOnly || !canvasEl) return false;
    const wantPan = e.button === 1 || (e.button === 0 && spaceHeld);
    if (!wantPan) return false;
    e.preventDefault();
    canvasEl.setPointerCapture(e.pointerId);
    panGesture = {
      startCssX: e.clientX,
      startCssY: e.clientY,
      startViewX: session.viewX,
      startViewY: session.viewY,
    };
    return true;
  }
  function onPanPointerMove(e: PointerEvent): boolean {
    if (!panGesture) return false;
    session.viewX = panGesture.startViewX + (e.clientX - panGesture.startCssX);
    session.viewY = panGesture.startViewY + (e.clientY - panGesture.startCssY);
    return true;
  }
  function onPanPointerUp(_e: PointerEvent): boolean {
    if (!panGesture) return false;
    panGesture = null;
    return true;
  }

  function onSpaceKeyDown(e: KeyboardEvent) {
    if (!infinite) return;
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
    if (e.code === 'Space' && !spaceHeld) { spaceHeld = true; e.preventDefault(); }
  }
  function onSpaceKeyUp(e: KeyboardEvent) {
    if (e.code === 'Space') spaceHeld = false;
  }

  // ── Pointer handlers ──────────────────────────────────────────────

  // ── Selection drag state ─────────────────────────────────────────
  // Active transform gesture started by mousedown on the selected
  // item's bbox or one of its handles. The mousemove handler reads
  // this to know what to update; mouseup clears it + commits.
  type SelectGesture =
    // Phase 1.23 — `containedAtStart` carries a snapshot of the
    // items inside a frame at gesture start, so they move with the
    // frame at the same dx/dy. Empty array when the moved item
    // isn't a frame.
    | { kind: 'move'; startX: number; startY: number; original: Item; containedAtStart: { layerId: string; index: number; original: Item }[] }
    | { kind: 'resize'; handle: HandleId; startX: number; startY: number; original: Item; originalBBox: BBox }
    | { kind: 'rotate'; cx: number; cy: number; startAngle: number; originalRotation: number; original: Item };
  type HandleId = 'nw' | 'n' | 'ne' | 'e' | 'se' | 's' | 'sw' | 'w';
  let selectGesture: SelectGesture | null = null;

  function selectedItem(): { layerId: string; index: number; item: Item } | null {
    const sel = session.selection;
    if (!sel) return null;
    const layer = session.doc.layers.find((l) => l.id === sel.layerId);
    if (!layer) return null;
    const item = layer.items[sel.index];
    if (!item) return null;
    return { layerId: sel.layerId, index: sel.index, item };
  }

  /** Hit-test items in z-order (newest first within a layer; topmost
   *  layer first). Returns the first item the point falls inside. */
  function pickItem(px: number, py: number): { layerId: string; index: number } | null {
    for (let li = session.doc.layers.length - 1; li >= 0; li--) {
      const layer = session.doc.layers[li];
      if (!layer.visible) continue;
      for (let i = layer.items.length - 1; i >= 0; i--) {
        if (pointInItem(px, py, layer.items[i])) {
          return { layerId: layer.id, index: i };
        }
      }
    }
    return null;
  }

  /** Phase 1.22 — find the closest anchor point within snap range
   *  of (px, py). Iterates every non-connector item on every
   *  visible layer (connectors don't have anchors themselves —
   *  prevents cycle in resolveConnectorEndpoint). Returns the
   *  hit's layer + item index + the anchor's (u, v, x, y) so the
   *  caller can store the attachment without re-deriving. Snap
   *  range scales with viewport zoom so it stays reachable at any
   *  zoom level. */
  function snapToAnchor(px: number, py: number) {
    const z = infinite ? session.viewZoom : 1;
    const snapRange = 12 / z; // 12 CSS px in world coords
    let best: { layerId: string; index: number; anchor: { u: number; v: number; x: number; y: number } } | null = null;
    let bestD = snapRange;
    for (let li = session.doc.layers.length - 1; li >= 0; li--) {
      const layer = session.doc.layers[li];
      if (!layer.visible) continue;
      for (let i = 0; i < layer.items.length; i++) {
        const it = layer.items[i];
        if (it.kind === 'connector') continue;
        for (const a of anchorsForItem(it, session.doc)) {
          const d = Math.hypot(px - a.x, py - a.y);
          if (d < bestD) {
            bestD = d;
            best = { layerId: layer.id, index: i, anchor: a };
          }
        }
      }
    }
    return best;
  }

  function onPointerDown(e: PointerEvent) {
    if (readOnly) return;
    // Pan first (middle-click or Space+drag in infinite mode) — must
    // run before the brush/shape branches so those don't start
    // gestures of their own with the wrong button.
    if (onPanPointerDown(e)) return;
    // Mouse buttons: 0 = left = paint with primary color; 2 = right
    // = paint with secondary (Paint's "Color 2"). Anything else is
    // ignored (middle / back / forward) so existing pan etc on
    // those mice don't bleed into the drawing surface.
    if (e.pointerType === 'mouse' && e.button !== 0 && e.button !== 2) return;
    if (!canvasEl) return;
    const usingColor2 = e.button === 2;
    // Effective color for this gesture. Held in a local so subsequent
    // tool branches don't have to know about the right-click toggle.
    const gestureColor = usingColor2 ? session.color2 : session.color;

    // Select tool — hit-test and either pick or deselect. Drag-to-
    // move happens in onPointerMove if the user drags after picking.
    if (session.tool === 'select') {
      const p = eventToSource(e);
      const hit = pickItem(p.x, p.y);
      if (hit) {
        session.selectItem(hit.layerId, hit.index);
        // Start a move gesture; mousemove + mouseup decide whether
        // it's a click or a drag.
        const layer = session.doc.layers.find((l) => l.id === hit.layerId)!;
        const item = layer.items[hit.index];
        if (!layer.locked) {
          canvasEl.setPointerCapture(e.pointerId);
          // Phase 1.23 — frame containment: if we just picked a
          // frame, snapshot every item whose bbox-center falls
          // inside the frame's bbox at gesture start. They get
          // translated along with the frame on every mousemove.
          // We use center-in-frame (not bbox-fully-inside) so an
          // item that overflows the frame slightly still moves
          // with it, matching Figma's behaviour.
          const contained: { layerId: string; index: number; original: Item }[] = [];
          if (item.kind === 'frame') {
            const fbb = itemBBox(item);
            for (const l of session.doc.layers) {
              if (l.locked) continue;
              l.items.forEach((other, oi) => {
                if (l.id === hit.layerId && oi === hit.index) return; // skip self
                if (other.kind === 'frame') return; // don't nest frames in the move set
                const obb = itemBBox(other, session.doc);
                const cx = obb.x + obb.w / 2;
                const cy = obb.y + obb.h / 2;
                if (cx >= fbb.x && cx <= fbb.x + fbb.w && cy >= fbb.y && cy <= fbb.y + fbb.h) {
                  contained.push({ layerId: l.id, index: oi, original: JSON.parse(JSON.stringify(other)) });
                }
              });
            }
          }
          selectGesture = {
            kind: 'move',
            startX: p.x,
            startY: p.y,
            original: JSON.parse(JSON.stringify(item)),
            containedAtStart: contained,
          };
        }
      } else {
        session.deselect();
      }
      e.preventDefault();
      return;
    }

    // Block input on the active layer when it's locked.
    const layer = session.doc.layers.find((l) => l.id === session.activeLayerId);
    if (!layer || layer.locked) return;

    canvasEl.setPointerCapture(e.pointerId);
    const p = eventToSource(e);

    // Eyedropper — click an item to pull its color into session.color.
    // Works on Stroke / Shape / Text; Image is skipped (no single
    // representative color). After picking we drop back to the
    // previous brush tool so the user can paint with the picked
    // color immediately — same UX as Paint / Photoshop.
    if (session.tool === 'eyedropper') {
      const p3 = eventToSource(e);
      const hit = pickItem(p3.x, p3.y);
      if (hit) {
        const layer = session.doc.layers.find((l) => l.id === hit.layerId);
        const item = layer?.items[hit.index];
        if (item && (item.kind === 'stroke' || item.kind === 'shape' || item.kind === 'text')) {
          session.color = item.color;
          session.tool = 'pen';
        }
      }
      e.preventDefault();
      return;
    }
    // Connector — two-click gesture (click anchor → click anchor).
    // First click: snap to the nearest anchor in pick range and
    // pin the start; mouse-move shows a live preview line. Second
    // click: snap end to nearest anchor (or place free at click
    // position) and commit the ConnectorItem.
    if (session.tool === 'connector') {
      const p4 = eventToSource(e);
      const snapped = snapToAnchor(p4.x, p4.y);
      if (!liveConnector) {
        // First click — pin the start.
        const startEp: ConnectorEndpoint = snapped
          ? { attached: { layerId: snapped.layerId, itemIndex: snapped.index }, u: snapped.anchor.u, v: snapped.anchor.v }
          : { x: p4.x, y: p4.y };
        liveConnector = {
          start: startEp,
          end: { x: p4.x, y: p4.y },
          mode: session.connectorMode,
          color: gestureColor,
          width: session.width,
          opacity: session.opacity,
          endArrow: 'arrow',
        };
        render();
      } else {
        // Second click — commit.
        const endEp: ConnectorEndpoint = snapped
          ? { attached: { layerId: snapped.layerId, itemIndex: snapped.index }, u: snapped.anchor.u, v: snapped.anchor.v }
          : { x: p4.x, y: p4.y };
        const item: ConnectorItem = {
          kind: 'connector',
          start: liveConnector.start,
          end: endEp,
          mode: liveConnector.mode,
          color: liveConnector.color,
          width: liveConnector.width,
          opacity: liveConnector.opacity,
          endArrow: liveConnector.endArrow,
        };
        session.addItem(layer.id, item);
        liveConnector = null;
        render();
      }
      e.preventDefault();
      return;
    }
    // Bucket — click a shape to recolor + fill it with the current
    // color at full opacity. Click on a stroke / text / image just
    // recolors (no fill semantics on those kinds). Click on empty
    // canvas → no-op (we're vector-first, no flood-fill of pixels).
    if (session.tool === 'bucket') {
      const p2 = eventToSource(e);
      const hit = pickItem(p2.x, p2.y);
      if (hit) {
        const layer = session.doc.layers.find((l) => l.id === hit.layerId);
        const item = layer?.items[hit.index];
        if (item && layer && !layer.locked) {
          let next: Item = item;
          if (item.kind === 'shape') {
            // C-1.17 — bucket recolors the FILL only. Outline stays
            // put (Paint's behaviour). Forces fill > 0 so the click
            // actually turns the shape filled.
            next = {
              ...item,
              fillColor: session.color,
              color: session.color, // legacy
              fill: Math.max(item.fill ?? 0, 1),
            };
          } else if (item.kind === 'stroke' || item.kind === 'text') {
            next = { ...item, color: session.color };
          }
          if (next !== item) session.replaceItem(hit.layerId, hit.index, next);
        }
      }
      e.preventDefault();
      return;
    }
    // Lasso — start a freehand polygon. Points captured each
    // mousemove, closed on mouseup. The pointer drag itself never
    // alters the doc; the commit happens in onPointerUp by
    // running every item through pointInPolygon and replacing
    // session.selection + extraSelected.
    if (session.tool === 'lasso') {
      canvasEl.setPointerCapture(e.pointerId);
      liveLasso = [[p.x, p.y]];
      render();
      e.preventDefault();
      return;
    }
    // Rectangle select — drag a rect; commit picks every item
    // whose bbox intersects. Auto-switches to select on release
    // so handles + Delete work right after.
    if (session.tool === 'rect-select') {
      canvasEl.setPointerCapture(e.pointerId);
      liveRectSelect = { x: p.x, y: p.y, w: 0, h: 0 };
      dragStart = { x: p.x, y: p.y };
      render();
      e.preventDefault();
      return;
    }
    // Crop — drag out a rectangle that becomes the new source
    // bounds on release. Live preview is a dashed white rectangle
    // overlaid on the canvas with the outside dimmed so the user
    // sees what gets discarded.
    if (session.tool === 'crop') {
      canvasEl.setPointerCapture(e.pointerId);
      liveCrop = { x: p.x, y: p.y, w: 0, h: 0 };
      dragStart = { x: p.x, y: p.y };
      render();
      e.preventDefault();
      return;
    }
    if (isBrushTool(session.tool)) {
      liveStroke = {
        kind: 'stroke',
        tool: session.tool,
        brushStyle: session.tool === 'pen' ? session.brushStyle : 'default',
        // Phase 1.21 — when the user has picked a stamp brush, every
        // new stroke carries the stamp id so the renderer takes the
        // stamp path. Eraser / highlighter / marker stay procedural
        // (they're alpha-mask operators, not stroke styles).
        stampId: session.tool === 'pen' && session.stampId ? session.stampId : undefined,
        color: gestureColor,
        width: session.width,
        opacity: session.opacity,
        points: [[p.x, p.y, p.p ?? 0.5]],
      };
      render();
      e.preventDefault();
    } else if (isShapeTool(session.tool)) {
      dragStart = { x: p.x, y: p.y };
      // C-1.17 — outline and fill are independent. Default mapping
      // matches Paint: left-drag → outline=Color 1, fill=Color 2;
      // right-drag swaps them. Legacy `color` stays set so back-
      // compat readers keep working.
      const strokeC = usingColor2 ? session.color2 : session.color;
      const fillC = usingColor2 ? session.color : session.color2;
      liveShape = {
        kind: 'shape',
        tool: session.tool,
        x: p.x, y: p.y, w: 0, h: 0,
        color: gestureColor,
        strokeColor: strokeC,
        fillColor: fillC,
        width: session.width,
        fill: session.fillShapes ? 1 : 0,
        opacity: session.opacity,
      };
      render();
      e.preventDefault();
    } else if (session.tool === 'frame') {
      // Frame: drag out a labeled boundary. Live preview is the
      // FrameItem itself; bbox commits on pointerup.
      dragStart = { x: p.x, y: p.y };
      liveFrame = {
        kind: 'frame',
        x: p.x, y: p.y, w: 0, h: 0,
        title: 'Frame',
      };
      render();
      e.preventDefault();
    } else if (session.tool === 'sticky') {
      // Sticky note: drag out a card (or single-click for a sane
      // default size). The text is empty at creation; double-click
      // (or click again with select tool) opens the inline editor.
      // Default to a fixed 240×180 card on single-tap so most
      // users get the expected post-it size without dragging.
      dragStart = { x: p.x, y: p.y };
      liveSticky = {
        kind: 'sticky',
        x: p.x, y: p.y, w: 240, h: 180,
        body: '',
      };
      render();
      e.preventDefault();
    } else if (session.tool === 'mindmap') {
      // Mindmap: single-click drops a fresh mindmap with a root +
      // three starter "Idea" children at the click point. Users
      // double-click a node to rename, or click-add-child via the
      // selection panel (Phase 1.24 follow-up).
      const root: MindmapNode = {
        id: crypto.randomUUID(),
        label: 'Central idea',
        children: [
          { id: crypto.randomUUID(), label: 'Idea 1', children: [] },
          { id: crypto.randomUUID(), label: 'Idea 2', children: [] },
          { id: crypto.randomUUID(), label: 'Idea 3', children: [] },
        ],
      };
      const item: MindmapItem = { kind: 'mindmap', x: p.x, y: p.y, root };
      session.addItem(layer.id, item);
      // Auto-select so the typography / properties panel can show
      // mindmap-specific actions next.
      const idx = layer.items.length - 1;
      session.selectItem(layer.id, idx);
      // Switch to select so the user can interact with the mindmap
      // immediately rather than keep dropping new ones.
      session.tool = 'select';
      e.preventDefault();
      return;
    } else if (session.tool === 'text') {
      // Begin a text-edit at the click point. The contenteditable
      // overlay below gets focused; commit on blur or Enter (without
      // Shift, which inserts newlines).
      textEdit = { x: p.x, y: p.y, w: 320, h: 60 };
      textEditBody = '';
      // Defer focus so the overlay mounts first.
      queueMicrotask(() => textEditRef?.focus());
      e.preventDefault();
    }
  }

  function onPointerMove(e: PointerEvent) {
    // Pan claims the pointer before any tool gesture so the brush /
    // shape branches don't see middle-click / space-drag deltas.
    if (onPanPointerMove(e)) return;
    const p = eventToSource(e);
    // Connector tool — track hover anchor + drive the live preview
    // end when the gesture is mid-flight. Snap end to anchors so
    // the user sees "this end will attach here" before clicking.
    if (!readOnly && session.tool === 'connector') {
      const snap = snapToAnchor(p.x, p.y);
      const newHover = snap;
      if ((hoverAnchor?.layerId !== newHover?.layerId) || (hoverAnchor?.index !== newHover?.index)
          || (hoverAnchor?.anchor.u !== newHover?.anchor.u) || (hoverAnchor?.anchor.v !== newHover?.anchor.v)) {
        hoverAnchor = newHover;
      }
      if (liveConnector) {
        if (snap) {
          liveConnector = {
            ...liveConnector,
            end: { attached: { layerId: snap.layerId, itemIndex: snap.index }, u: snap.anchor.u, v: snap.anchor.v },
          };
        } else {
          liveConnector = { ...liveConnector, end: { x: p.x, y: p.y } };
        }
        render();
      } else if (snap || hoverAnchor) {
        // Hover state changed but no gesture in flight; still need
        // a redraw to show / hide the snap ring.
        render();
      }
      return;
    }
    // Selection-gesture transforms — mutate the selected item in
    // place without committing to history (we commit on mouseup so
    // a single drag becomes one undo step).
    if (selectGesture) {
      const sel = selectedItem();
      if (!sel) { selectGesture = null; return; }
      const layer = session.doc.layers.find((l) => l.id === sel.layerId);
      if (!layer) return;
      const g = selectGesture;
      if (g.kind === 'move') {
        const dx = p.x - g.startX;
        const dy = p.y - g.startY;
        layer.items[sel.index] = translateItem(g.original, dx, dy);
        // Phase 1.23 — translate every snapshotted contained item
        // by the same delta. Reading from the original snapshot
        // (not the live item) keeps deltas stable across the drag.
        for (const c of g.containedAtStart) {
          const cLayer = session.doc.layers.find((l) => l.id === c.layerId);
          if (!cLayer) continue;
          cLayer.items[c.index] = translateItem(c.original, dx, dy);
        }
      } else if (g.kind === 'resize') {
        const b = g.originalBBox;
        let nx = b.x, ny = b.y, nw = b.w, nh = b.h;
        // Edge / corner ID drives which sides move.
        const h = g.handle;
        if (h.includes('w')) { const dx = p.x - g.startX; nx = b.x + dx; nw = b.w - dx; }
        if (h.includes('e')) { nw = b.w + (p.x - g.startX); }
        if (h.includes('n')) { const dy = p.y - g.startY; ny = b.y + dy; nh = b.h - dy; }
        if (h.includes('s')) { nh = b.h + (p.y - g.startY); }
        // Shift = uniform scale (corner handles only).
        if (e.shiftKey && (h === 'nw' || h === 'ne' || h === 'sw' || h === 'se') && b.w > 0 && b.h > 0) {
          const ratio = b.h / b.w;
          if (h === 'nw' || h === 'se') {
            nh = nw * ratio;
            if (h === 'nw') ny = b.y + b.h - nh;
          } else {
            nh = nw * ratio;
            if (h === 'ne') ny = b.y + b.h - nh;
          }
        }
        // Don't allow zero/negative dimensions — clamp to 4 px.
        if (nw < 4) { nw = 4; if (h.includes('w')) nx = b.x + b.w - 4; }
        if (nh < 4) { nh = 4; if (h.includes('n')) ny = b.y + b.h - 4; }
        layer.items[sel.index] = resizeItemToBBox(g.original, nx, ny, nw, nh);
      } else if (g.kind === 'rotate') {
        const ang = (Math.atan2(p.y - g.cy, p.x - g.cx) * 180) / Math.PI;
        let next = g.originalRotation + (ang - g.startAngle);
        // Shift snaps to 15° increments.
        if (e.shiftKey) next = Math.round(next / 15) * 15;
        const item = g.original;
        if (item.kind === 'stroke') {
          // Strokes don't carry a rotation field — rotation reshapes
          // the points instead. Build the rotated copy.
          const rad = ((next - (g.originalRotation || 0)) * Math.PI) / 180;
          const c = Math.cos(rad);
          const s = Math.sin(rad);
          layer.items[sel.index] = {
            ...item,
            points: item.points.map((pt) => {
              const dx = pt[0] - g.cx;
              const dy = pt[1] - g.cy;
              return [g.cx + dx * c - dy * s, g.cy + dx * s + dy * c, pt[2]];
            }),
          };
        } else {
          layer.items[sel.index] = { ...item, rotation: next } as Item;
        }
      }
      render();
      return;
    }
    // Lasso live-add to polygon. Drop near-duplicate points to
    // keep the array bounded; the renderer handles the rest.
    if (liveLasso) {
      const last = liveLasso[liveLasso.length - 1];
      const dx = p.x - last[0], dy = p.y - last[1];
      if (Math.hypot(dx, dy) > 2) {
        liveLasso = [...liveLasso, [p.x, p.y]];
        render();
      }
      return;
    }
    // Crop live-resize.
    if (liveCrop && dragStart) {
      liveCrop = {
        x: dragStart.x,
        y: dragStart.y,
        w: p.x - dragStart.x,
        h: p.y - dragStart.y,
      };
      render();
      return;
    }
    // Rect-select live-resize.
    if (liveRectSelect && dragStart) {
      liveRectSelect = {
        x: dragStart.x,
        y: dragStart.y,
        w: p.x - dragStart.x,
        h: p.y - dragStart.y,
      };
      render();
      return;
    }
    if (liveStroke) {
      liveStroke = {
        ...liveStroke,
        points: [...liveStroke.points, [p.x, p.y, p.p ?? 0.5]],
      };
      render();
    } else if (liveShape && dragStart) {
      let dx = p.x - dragStart.x;
      let dy = p.y - dragStart.y;
      // Shift constrains: line/arrow → 45° increments; rect/ellipse → 1:1
      if (e.shiftKey) {
        if (liveShape.tool === 'line' || liveShape.tool === 'arrow') {
          const ang = Math.atan2(dy, dx);
          const snap = Math.round(ang / (Math.PI / 4)) * (Math.PI / 4);
          const len = Math.hypot(dx, dy);
          dx = Math.cos(snap) * len;
          dy = Math.sin(snap) * len;
        } else {
          const s = Math.max(Math.abs(dx), Math.abs(dy));
          dx = Math.sign(dx || 1) * s;
          dy = Math.sign(dy || 1) * s;
        }
      }
      liveShape = { ...liveShape, w: dx, h: dy };
      render();
    } else if (liveFrame && dragStart) {
      liveFrame = { ...liveFrame, w: p.x - dragStart.x, h: p.y - dragStart.y };
      render();
    } else if (liveSticky && dragStart) {
      // Drag changes the card size; if dx/dy are tiny (single tap),
      // the pointerup commit uses a default 240×180 instead.
      const dx = p.x - dragStart.x;
      const dy = p.y - dragStart.y;
      if (Math.abs(dx) > 4 || Math.abs(dy) > 4) {
        liveSticky = { ...liveSticky, w: dx, h: dy };
      }
      render();
    }
  }

  function onPointerUp(e: PointerEvent) {
    if (canvasEl?.hasPointerCapture(e.pointerId)) {
      canvasEl.releasePointerCapture(e.pointerId);
    }
    // Pan release runs first + short-circuits so the tool branches
    // don't commit a 0-distance brush stroke / 0-px shape.
    if (onPanPointerUp(e)) return;
    // Lasso commit — pick every item whose bbox falls in the
    // polygon and set the multi-selection. Auto-switch to the
    // select tool so handles + delete + move-to-layer immediately
    // operate on the picked items.
    if (liveLasso) {
      const poly = liveLasso;
      liveLasso = null;
      if (poly.length >= 3) {
        const picks: Array<{ layerId: string; index: number }> = [];
        for (const layer of session.doc.layers) {
          if (!layer.visible) continue;
          layer.items.forEach((item, idx) => {
            if (itemInPolygon(item, poly)) picks.push({ layerId: layer.id, index: idx });
          });
        }
        session.setMultiSelection(picks);
        if (picks.length > 0) session.tool = 'select';
      }
      render();
      return;
    }
    // Rect-select commit — every item whose bbox intersects the
    // drag-rect gets multi-selected.
    if (liveRectSelect) {
      const r = liveRectSelect;
      liveRectSelect = null;
      dragStart = null;
      const x = Math.min(r.x, r.x + r.w);
      const y = Math.min(r.y, r.y + r.h);
      const w = Math.abs(r.w);
      const h = Math.abs(r.h);
      if (w >= 4 && h >= 4) {
        const picks: Array<{ layerId: string; index: number }> = [];
        for (const layer of session.doc.layers) {
          if (!layer.visible) continue;
          layer.items.forEach((item, idx) => {
            const bb = itemBBox(item);
            // Intersection test on axis-aligned bboxes.
            if (bb.x + bb.w >= x && bb.x <= x + w &&
                bb.y + bb.h >= y && bb.y <= y + h) {
              picks.push({ layerId: layer.id, index: idx });
            }
          });
        }
        session.setMultiSelection(picks);
        if (picks.length > 0) session.tool = 'select';
      }
      render();
      return;
    }
    // Crop commit — apply if the rect is non-tiny.
    if (liveCrop) {
      const c = liveCrop;
      liveCrop = null;
      dragStart = null;
      const x = Math.min(c.x, c.x + c.w);
      const y = Math.min(c.y, c.y + c.h);
      const w = Math.abs(c.w);
      const h = Math.abs(c.h);
      if (w >= 16 && h >= 16) {
        session.crop(x, y, w, h);
        session.tool = 'select';
      }
      render();
      return;
    }
    // Selection gesture commit — push one history snapshot for the
    // whole drag so undo rewinds to pre-drag in one step.
    if (selectGesture) {
      const sel = selectedItem();
      if (sel) {
        const layer = session.doc.layers.find((l) => l.id === sel.layerId);
        const finalItem = layer?.items[sel.index];
        if (finalItem) {
          // Replace via session method so history snapshots cleanly.
          session.replaceItem(sel.layerId, sel.index, finalItem);
        }
        // Phase 1.23 — also commit every contained item that moved
        // with this frame so they each get a history-clean
        // replaceItem call. Single-undo rewinds the whole drag.
        if (selectGesture.kind === 'move' && selectGesture.containedAtStart.length > 0) {
          for (const c of selectGesture.containedAtStart) {
            const cLayer = session.doc.layers.find((l) => l.id === c.layerId);
            const cFinal = cLayer?.items[c.index];
            if (cFinal) session.replaceItem(c.layerId, c.index, cFinal);
          }
        }
      }
      selectGesture = null;
      render();
      return;
    }
    const layerId = session.activeLayerId;
    if (liveStroke && layerId) {
      // Single-tap → keep as a one-point stroke; perfect-freehand
      // renders it as a small filled disc.
      session.addItem(layerId, liveStroke);
      liveStroke = null;
    } else if (liveShape && layerId) {
      // Drop shapes that didn't actually drag (< 4px in both axes).
      if (Math.abs(liveShape.w) >= 4 || Math.abs(liveShape.h) >= 4) {
        session.addItem(layerId, liveShape);
      }
      liveShape = null;
      dragStart = null;
    } else if (liveFrame && layerId) {
      // Frame commit — drop tiny accidental drags (default min 48
      // px each way so the frame is at least usable).
      if (Math.abs(liveFrame.w) >= 48 && Math.abs(liveFrame.h) >= 48) {
        // Normalize to positive w/h for storage so contained-item
        // calculations are simpler.
        const x = liveFrame.w >= 0 ? liveFrame.x : liveFrame.x + liveFrame.w;
        const y = liveFrame.h >= 0 ? liveFrame.y : liveFrame.y + liveFrame.h;
        session.addItem(layerId, { ...liveFrame, x, y, w: Math.abs(liveFrame.w), h: Math.abs(liveFrame.h) });
      }
      liveFrame = null;
      dragStart = null;
    } else if (liveSticky && layerId) {
      // Sticky commit — drag or no-drag both make a sticky. The
      // size on no-drag is the default 240×180 set at pointerdown.
      const x = liveSticky.w >= 0 ? liveSticky.x : liveSticky.x + liveSticky.w;
      const y = liveSticky.h >= 0 ? liveSticky.y : liveSticky.y + liveSticky.h;
      const item: StickyNoteItem = { ...liveSticky, x, y, w: Math.max(120, Math.abs(liveSticky.w)), h: Math.max(80, Math.abs(liveSticky.h)) };
      session.addItem(layerId, item);
      liveSticky = null;
      dragStart = null;
      // Open the inline editor on the freshly-added sticky so the
      // user can type right away — matches "double-click to edit"
      // muscle memory without the extra click.
      queueMicrotask(() => {
        const layer = session.doc.layers.find((l) => l.id === layerId);
        if (!layer) return;
        const idx = layer.items.length - 1;
        editingStickyRef = { layerId, index: idx };
        stickyEditBody = '';
        queueMicrotask(() => textEditRef?.focus());
      });
    }
    render();
  }

  function handleWindowPointerUp(e: PointerEvent) {
    if (liveStroke || liveShape || liveFrame || liveSticky) onPointerUp(e);
  }

  // ── Text edit commit ─────────────────────────────────────────────

  function commitTextEdit() {
    if (!textEdit) return;
    const body = textEditBody.trim();
    const layerId = session.activeLayerId;
    if (body && layerId) {
      // Use the typography state from the tool panel so the new
      // item inherits the user's current font / size / weight.
      const item: TextItem = {
        kind: 'text',
        x: textEdit.x,
        y: textEdit.y,
        w: textEdit.w,
        h: textEdit.h,
        body,
        fontSize: session.fontSize,
        fontFamily: session.fontFamily,
        color: session.color,
        align: session.textAlign,
        bold: session.bold,
        italic: session.italic,
      };
      session.addItem(layerId, item);
    }
    textEdit = null;
    textEditBody = '';
  }

  function onTextEditKey(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      textEdit = null;
      textEditBody = '';
    } else if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      commitTextEdit();
    }
  }

  // ── Paste handler ────────────────────────────────────────────────

  async function onPaste(e: ClipboardEvent) {
    if (readOnly) return;
    const layerId = session.activeLayerId;
    if (!layerId) return;
    const cd = e.clipboardData;
    if (!cd) return;
    // Look for an image first — paste-from-screenshot is the
    // common case and beats text in priority.
    for (const item of Array.from(cd.items)) {
      if (item.type.startsWith('image/')) {
        const file = item.getAsFile();
        if (!file) continue;
        if (file.size > MAX_PASTED_IMAGE_BYTES) {
          // eslint-disable-next-line no-alert
          alert(`Pasted image is ${(file.size / (1024 * 1024)).toFixed(1)} MB; cap is ${(MAX_PASTED_IMAGE_BYTES / (1024 * 1024)).toFixed(0)} MB for inline storage.`);
          return;
        }
        e.preventDefault();
        const dataUrl = await new Promise<string>((resolve) => {
          const r = new FileReader();
          r.onload = () => resolve(String(r.result ?? ''));
          r.readAsDataURL(file);
        });
        // Probe natural dimensions so the dropped item has a sensible
        // aspect-correct size on the canvas.
        const img = new Image();
        img.src = dataUrl;
        await img.decode().catch(() => {});
        const naturalW = img.naturalWidth || 800;
        const naturalH = img.naturalHeight || 600;
        // Fit to half the source canvas so the image lands visible
        // without dominating; the user can resize via C-1.6's
        // selection tool.
        const maxW = session.doc.source_w * 0.5;
        const r = Math.min(1, maxW / naturalW);
        const w = naturalW * r;
        const h = naturalH * r;
        // Center the drop point in the source canvas.
        const x = (session.doc.source_w - w) / 2;
        const y = (session.doc.source_h - h) / 2;
        session.addItem(layerId, { kind: 'image', x, y, w, h, src: dataUrl });
        render();
        return;
      }
    }
    // No image — fall back to text.
    const text = cd.getData('text/plain').trim();
    if (text) {
      e.preventDefault();
      const fontSize = Math.max(14, session.width * 2.5);
      session.addItem(layerId, {
        kind: 'text',
        x: session.doc.source_w * 0.2,
        y: session.doc.source_h * 0.2,
        w: session.doc.source_w * 0.6,
        h: 80,
        body: text,
        fontSize,
        color: session.color,
        align: 'left',
      });
      render();
    }
  }

  // ── Selection overlay derived values ─────────────────────────────

  // Computed in the template; exposed via a getter for clarity.
  const selectionBBox = $derived(() => {
    const sel = session.selection;
    if (!sel) return null;
    const layer = session.doc.layers.find((l) => l.id === sel.layerId);
    const item = layer?.items[sel.index];
    if (!item) return null;
    return { bbox: itemBBox(item), item, layerId: sel.layerId, index: sel.index };
  });

  // Convert source-canvas bbox to CSS-pixel rect for handle layout.
  // sourceToCss handles both infinite (zoom + pan) and fixed mode, so
  // computing from two opposing corners gives the correct on-screen
  // rect at any zoom.
  function bboxToCss(bb: BBox): { left: number; top: number; width: number; height: number } {
    const a = sourceToCss(bb.x, bb.y);
    const b = sourceToCss(bb.x + bb.w, bb.y + bb.h);
    return { left: a.left, top: a.top, width: b.left - a.left, height: b.top - a.top };
  }

  // CSS-px font size for the text-edit overlay. In infinite mode
  // the size scales with zoom so what you type matches what gets
  // drawn at the current view; in fixed mode it scales with the
  // wrapper width like before.
  function textCssFontSize(worldFontSize: number): number {
    if (infinite) return worldFontSize * session.viewZoom;
    if (!wrapperEl) return worldFontSize;
    return worldFontSize * (wrapperEl.clientWidth / session.doc.source_w);
  }

  function startHandleDrag(e: PointerEvent, handle: HandleId) {
    e.stopPropagation();
    e.preventDefault();
    const sel = selectedItem();
    if (!sel) return;
    const p = eventToSource(e);
    selectGesture = {
      kind: 'resize',
      handle,
      startX: p.x,
      startY: p.y,
      original: JSON.parse(JSON.stringify(sel.item)),
      originalBBox: itemBBox(sel.item),
    };
    if (canvasEl) canvasEl.setPointerCapture(e.pointerId);
  }

  function startRotateDrag(e: PointerEvent) {
    e.stopPropagation();
    e.preventDefault();
    const sel = selectedItem();
    if (!sel) return;
    const bb = itemBBox(sel.item);
    const cx = bb.x + bb.w / 2;
    const cy = bb.y + bb.h / 2;
    const p = eventToSource(e);
    selectGesture = {
      kind: 'rotate',
      cx, cy,
      startAngle: (Math.atan2(p.y - cy, p.x - cx) * 180) / Math.PI,
      // Connectors + strokes don't carry rotation; rotation handle
      // on them rotates the points / endpoints instead. We start
      // from 0 in those cases.
      originalRotation: (sel.item.kind === 'stroke' || sel.item.kind === 'connector') ? 0 : (sel.item.rotation ?? 0),
      original: JSON.parse(JSON.stringify(sel.item)),
    };
    if (canvasEl) canvasEl.setPointerCapture(e.pointerId);
  }

  // Double-click a text item with the select tool active → re-enter
  // edit mode at that item. The contenteditable overlay reuses the
  // text-input UI so commit-on-blur/Enter works the same way.
  let editingTextRef: { layerId: string; index: number } | null = $state(null);
  // Sticky-note text-edit gesture (Phase 1.23). Same DOM overlay
  // as the text item editor, just targets a StickyNoteItem.body
  // on commit instead of TextItem.body.
  let editingStickyRef: { layerId: string; index: number } | null = $state(null);
  let stickyEditBody = $state('');

  function onCanvasDblClick(e: MouseEvent) {
    if (readOnly) return;
    if (session.tool !== 'select') return;
    const p = eventToSource(e);
    const hit = pickItem(p.x, p.y);
    if (!hit) return;
    const layer = session.doc.layers.find((l) => l.id === hit.layerId);
    const item = layer?.items[hit.index];
    if (!item) return;
    if (item.kind === 'text') {
      editingTextRef = { layerId: hit.layerId, index: hit.index };
      textEditBody = item.body;
      queueMicrotask(() => textEditRef?.focus());
      e.preventDefault();
      return;
    }
    if (item.kind === 'sticky') {
      editingStickyRef = { layerId: hit.layerId, index: hit.index };
      stickyEditBody = item.body;
      queueMicrotask(() => textEditRef?.focus());
      e.preventDefault();
      return;
    }
  }

  /** Commit the sticky-note body edit. Mirror of commitTextEdit2
   *  for text items; just targets the StickyNoteItem.body field. */
  function commitStickyEdit() {
    if (!editingStickyRef) return;
    const layer = session.doc.layers.find((l) => l.id === editingStickyRef!.layerId);
    const item = layer?.items[editingStickyRef.index];
    if (item && item.kind === 'sticky') {
      const next: StickyNoteItem = { ...item, body: stickyEditBody };
      session.replaceItem(editingStickyRef.layerId, editingStickyRef.index, next);
    }
    editingStickyRef = null;
    stickyEditBody = '';
  }

  function commitTextEdit2() {
    if (!editingTextRef) return;
    const layer = session.doc.layers.find((l) => l.id === editingTextRef!.layerId);
    const item = layer?.items[editingTextRef.index];
    if (item && item.kind === 'text') {
      const next: TextItem = { ...item, body: textEditBody.trim() || item.body };
      session.replaceItem(editingTextRef.layerId, editingTextRef.index, next);
    }
    editingTextRef = null;
    textEditBody = '';
  }

  // ── Clipboard (copy / cut / paste of selected items) ─────────────
  // Per-session clipboard. Lives in module scope so paste works
  // even when the canvas remounts (Vite HMR, layer changes, etc).
  // We don't go through the OS clipboard because items are JSON, not
  // text/HTML/image — local clipboard is simpler + reliable.

  function copySelection(): Item | null {
    const sel = selectedItem();
    if (!sel) return null;
    sessionClipboard = JSON.parse(JSON.stringify(sel.item)) as Item;
    return sessionClipboard;
  }

  function cutSelection() {
    const sel = selectedItem();
    if (!sel) return;
    copySelection();
    session.removeItems(sel.layerId, [sel.index]);
  }

  function pasteFromClipboard() {
    if (!sessionClipboard) return;
    const targetLayer = session.activeLayerId;
    if (!targetLayer) return;
    // Offset the paste by 20 px so the user sees it land beside the
    // original instead of stacking exactly on top.
    const offsetItem = translateItem(JSON.parse(JSON.stringify(sessionClipboard)) as Item, 20, 20);
    session.addItem(targetLayer, offsetItem);
    // Auto-select the newly pasted item.
    const layer = session.doc.layers.find((l) => l.id === targetLayer);
    if (layer) session.selectItem(targetLayer, layer.items.length - 1);
  }

  // Document-level keyboard for selection-related shortcuts.
  function onDocKey(e: KeyboardEvent) {
    if (readOnly) return;
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
    // Delete / Backspace = remove every selected item (primary +
    // any lasso-multi-picked extras). Group indexes per-layer so
    // one removeItems call per layer is enough.
    if ((e.key === 'Delete' || e.key === 'Backspace') && session.selection) {
      e.preventDefault();
      const all = [session.selection, ...session.extraSelected];
      const byLayer = new Map<string, number[]>();
      for (const s of all) {
        const arr = byLayer.get(s.layerId) ?? [];
        arr.push(s.index);
        byLayer.set(s.layerId, arr);
      }
      for (const [layerId, indexes] of byLayer) {
        session.removeItems(layerId, indexes);
      }
      session.deselect();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'c') {
      e.preventDefault();
      copySelection();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'x') {
      e.preventDefault();
      cutSelection();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'v') {
      e.preventDefault();
      pasteFromClipboard();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
      e.preventDefault();
      session.selectAll();
      // Switch to the select tool so handles render immediately on
      // the picked items — matches Paint / Photoshop.
      if (session.tool !== 'select') session.tool = 'select';
      return;
    }
  }

  // ── Public helper for the host: rasterized PNG snapshot ───────────

  export function snapshotPng(): string {
    if (!canvasEl) return '';
    const savedBg = backgroundImg;
    backgroundImg = null;
    render();
    const out = canvasEl.toDataURL('image/png');
    backgroundImg = savedBg;
    render();
    return out;
  }

  // ── Lifecycle ────────────────────────────────────────────────────

  onMount(() => {
    if (!canvasEl) return;
    ctx = canvasEl.getContext('2d');
    if (!ctx) return;
    fitToWrapper();
    const ro = new ResizeObserver(fitToWrapper);
    if (wrapperEl) ro.observe(wrapperEl);
    if (backgroundUrl) {
      const img = new Image();
      img.crossOrigin = 'anonymous';
      img.onload = () => { backgroundImg = img; render(); };
      img.src = backgroundUrl;
    }
    document.addEventListener('keydown', onDocKey);
    return () => ro.disconnect();
  });

  onDestroy(() => {
    ctx = null;
    document.removeEventListener('keydown', onDocKey);
  });

  // Re-render reactively when session doc changes (undo/redo, items
  // added by paste, layer toggles, etc).
  $effect(() => {
    void session.doc;
    render();
  });

  // Re-render on view (pan / zoom) changes too — infinite mode only.
  $effect(() => {
    if (!infinite) return;
    void session.viewX;
    void session.viewY;
    void session.viewZoom;
    render();
  });

  // Re-fit on source dimension change.
  $effect(() => {
    void session.doc.source_w;
    void session.doc.source_h;
    fitToWrapper();
  });

  // Background URL prop change (annotation host swaps backgrounds
  // when the cursor moves to a new frame).
  $effect(() => {
    const url = backgroundUrl;
    if (!url) { backgroundImg = null; return; }
    const img = new Image();
    img.crossOrigin = 'anonymous';
    img.onload = () => { backgroundImg = img; render(); };
    img.src = url;
  });

  // textEditBody changes are not committed yet — Just track input.
  function onTextInput(e: Event) {
    const t = e.currentTarget as HTMLDivElement;
    textEditBody = t.innerText;
  }
</script>

<svelte:window
  onpointerup={handleWindowPointerUp}
  onpointercancel={handleWindowPointerUp}
  onkeydown={onSpaceKeyDown}
  onkeyup={onSpaceKeyUp}
/>

<div
  bind:this={wrapperEl}
  class="relative h-full w-full select-none"
  onpaste={onPaste}
  tabindex="-1"
>
  <canvas
    bind:this={canvasEl}
    class="absolute inset-0 h-full w-full touch-none"
    style:cursor={cursorFor(session.tool, readOnly)}
    onpointerdown={onPointerDown}
    onpointermove={onPointerMove}
    onpointerup={onPointerUp}
    ondblclick={onCanvasDblClick}
    onwheel={onWheel}
    oncontextmenu={(e) => e.preventDefault()}
  ></canvas>

  {#if selectionBBox() && session.tool === 'select'}
    {@const sb = selectionBBox()!}
    {@const css = bboxToCss(sb.bbox)}
    <!-- Selection overlay: bbox outline + 8 resize handles + rotate
         handle. Positioned absolute over the canvas in CSS pixels so
         handle size stays constant regardless of source-canvas
         dimensions. Rotated via transform so the user sees the
         rotation live. -->
    <div
      class="pointer-events-none absolute"
      style:left={`${css.left}px`}
      style:top={`${css.top}px`}
      style:width={`${css.width}px`}
      style:height={`${css.height}px`}
      style:transform={sb.bbox.rotation ? `rotate(${sb.bbox.rotation}deg)` : 'none'}
      style:transform-origin="center"
    >
      <div class="absolute inset-0 border-2 border-accent shadow-[0_0_0_1px_rgba(0,0,0,0.3)]"></div>
      <!-- 8 resize handles. pointer-events-auto so they receive the
           drag; the wrapping div is pointer-events-none so clicks
           inside the bbox (but outside handles) fall through to the
           canvas for re-pick. -->
      {#each [
        { id: 'nw', pos: 'left-0 top-0 -translate-x-1/2 -translate-y-1/2', cursor: 'nwse-resize' },
        { id: 'n',  pos: 'left-1/2 top-0 -translate-x-1/2 -translate-y-1/2', cursor: 'ns-resize' },
        { id: 'ne', pos: 'right-0 top-0 translate-x-1/2 -translate-y-1/2', cursor: 'nesw-resize' },
        { id: 'e',  pos: 'right-0 top-1/2 translate-x-1/2 -translate-y-1/2', cursor: 'ew-resize' },
        { id: 'se', pos: 'right-0 bottom-0 translate-x-1/2 translate-y-1/2', cursor: 'nwse-resize' },
        { id: 's',  pos: 'left-1/2 bottom-0 -translate-x-1/2 translate-y-1/2', cursor: 'ns-resize' },
        { id: 'sw', pos: 'left-0 bottom-0 -translate-x-1/2 translate-y-1/2', cursor: 'nesw-resize' },
        { id: 'w',  pos: 'left-0 top-1/2 -translate-x-1/2 -translate-y-1/2', cursor: 'ew-resize' },
      ] as h (h.id)}
        <button
          type="button"
          class={`pointer-events-auto absolute h-3 w-3 rounded-sm border-2 border-accent bg-white shadow-sm ${h.pos}`}
          style:cursor={h.cursor}
          onpointerdown={(e) => startHandleDrag(e, h.id as HandleId)}
          aria-label={`Resize ${h.id}`}
        ></button>
      {/each}
      <!-- Rotate handle — sits above the bbox top-center, connected
           by a short stem so it reads as "the rotate one" not "extra
           resize". -->
      <div class="pointer-events-none absolute left-1/2 top-0 h-5 w-px -translate-x-1/2 -translate-y-full bg-accent"></div>
      <button
        type="button"
        class="pointer-events-auto absolute left-1/2 top-0 h-3.5 w-3.5 -translate-x-1/2 -translate-y-[calc(100%+0.5rem)] rounded-full border-2 border-accent bg-white shadow-sm"
        style:cursor="grab"
        onpointerdown={startRotateDrag}
        title="Rotate (Shift = snap 15°)"
        aria-label="Rotate"
      ></button>
    </div>
  {/if}

  {#if editingTextRef && !readOnly}
    {@const layer = session.doc.layers.find((l) => l.id === editingTextRef!.layerId)}
    {@const item = layer?.items[editingTextRef!.index] as TextItem | undefined}
    {#if item}
      {@const css = sourceToCss(item.x, item.y)}
      <!-- svelte-ignore a11y_autofocus -->
      <div
        bind:this={textEditRef}
        contenteditable="true"
        class="absolute z-10 min-h-[1.5em] cursor-text rounded border border-accent bg-white/95 px-1 text-black outline-none focus:ring-2 focus:ring-accent"
        style:left={`${css.left}px`}
        style:top={`${css.top}px`}
        style:font-size={`${textCssFontSize(item.fontSize)}px`}
        style:font-family={item.fontFamily ?? 'system-ui'}
        style:color={item.color}
        oninput={(e) => textEditBody = (e.currentTarget as HTMLDivElement).innerText}
        onkeydown={(e) => { if (e.key === 'Escape' || (e.key === 'Enter' && !e.shiftKey)) { e.preventDefault(); commitTextEdit2(); } }}
        onblur={commitTextEdit2}
      >{item.body}</div>
    {/if}
  {/if}

  {#if editingStickyRef && !readOnly}
    {@const layer = session.doc.layers.find((l) => l.id === editingStickyRef!.layerId)}
    {@const item = layer?.items[editingStickyRef!.index] as StickyNoteItem | undefined}
    {#if item}
      {@const css = sourceToCss(item.x, item.y)}
      {@const wCss = (item.w) * (infinite ? session.viewZoom : (canvasEl ? canvasEl.getBoundingClientRect().width / session.doc.source_w : 1))}
      {@const hCss = (item.h) * (infinite ? session.viewZoom : (canvasEl ? canvasEl.getBoundingClientRect().height / session.doc.source_h : 1))}
      <!-- svelte-ignore a11y_autofocus -->
      <div
        bind:this={textEditRef}
        contenteditable="true"
        class="absolute z-10 cursor-text overflow-hidden rounded p-3 outline-none focus:ring-2 focus:ring-accent"
        style:left={`${css.left}px`}
        style:top={`${css.top}px`}
        style:width={`${wCss}px`}
        style:height={`${hCss}px`}
        style:background-color={item.background ?? '#fef08a'}
        style:font-size={`${textCssFontSize(item.fontSize ?? 18)}px`}
        style:font-family={item.fontFamily ? `"${item.fontFamily}", system-ui` : 'system-ui'}
        style:color={item.color ?? '#0f172a'}
        oninput={(e) => stickyEditBody = (e.currentTarget as HTMLDivElement).innerText}
        onkeydown={(e) => { if (e.key === 'Escape') { e.preventDefault(); commitStickyEdit(); } }}
        onblur={commitStickyEdit}
      >{item.body}</div>
    {/if}
  {/if}

  {#if textEdit && !readOnly}
    {@const css = sourceToCss(textEdit.x, textEdit.y)}
    <div
      bind:this={textEditRef}
      contenteditable="true"
      class="absolute z-10 min-h-[1.5em] min-w-[120px] cursor-text rounded border border-accent bg-white/95 px-1 text-black outline-none focus:ring-2 focus:ring-accent"
      style:left={`${css.left}px`}
      style:top={`${css.top}px`}
      style:font-size={`${textCssFontSize(Math.max(14, session.width * 2.5))}px`}
      style:color={session.color}
      oninput={onTextInput}
      onkeydown={onTextEditKey}
      onblur={commitTextEdit}
    ></div>
  {/if}
</div>
