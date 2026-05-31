<script lang="ts">
  // BrushCanvas — the shared brush primitive used by Whiteboard (post-
  // anchored) and the future Annotation surface (asset-frame-anchored).
  //
  // What it owns
  // ------------
  //   - The <canvas> element + 2D context
  //   - Pointer event handling (mouse / touch / stylus)
  //   - perfect-freehand stroke rendering (smooth, pressure-aware
  //     polygons from raw pointer samples)
  //   - The vector document (BrushContent — strokes + layers)
  //   - An undo / redo history stack
  //   - Tool / color / size / opacity state — exposed as bindable
  //     props so a host toolbar can drive them
  //   - DPR (devicePixelRatio) aware re-rasterization so the canvas
  //     stays crisp on retina + window-resize
  //
  // What it does NOT own
  // --------------------
  //   - Save / load (host's job — host passes `value` in, listens to
  //     `oncommit` for the final document on save)
  //   - The toolbar UI (host renders its own toolbar bound to these
  //     props — letting Whiteboard + Annotation pick which tools to
  //     surface and where to position the bar)
  //   - The layer panel (host responsibility; multi-layer UI ships
  //     in C-1.5; this commit's single visible layer is enough to
  //     prove the engine)
  //   - The cover-it overlay chrome (Whiteboard.svelte handles that)
  //
  // Why share the primitive: Whiteboard + frame-Annotation share 100%
  // of the brush math, the undo stack, the DPR handling, the pointer
  // capture. They only differ in (a) what they're drawing OVER and
  // (b) where the result gets stored.

  import { onMount, onDestroy } from 'svelte';
  import { getStroke } from 'perfect-freehand';
  import type { BrushContent, Layer, Point, Stroke, Tool } from '$lib/whiteboard/types';
  import { defaultOpacityFor, strokeOptionsFor } from '$lib/whiteboard/types';

  interface Props {
    /** The doc being edited. Two-way bound — the host reads it back to
        save. We keep it `$bindable` so the host can also reset / load
        a different doc without a full remount. */
    value: BrushContent;
    /** Active tool. Bindable so the host's tool picker drives it. */
    tool?: Tool;
    /** Active brush color (CSS color). Ignored for the eraser. */
    color?: string;
    /** Active brush width in source-canvas pixels. */
    width?: number;
    /** Per-stroke opacity (0..1). Highlighter defaults to 0.45;
        everything else 1. */
    opacity?: number;
    /** Index into value.layers — the layer new strokes land in. */
    activeLayer?: number;
    /** Read-only mode — render the doc but don't accept input. Used
        when previewing somebody else's whiteboard in the sidebar. */
    readOnly?: boolean;
    /** Optional asset reference image — when present, drawn beneath
        the strokes (annotation use case: sketch over a video frame).
        Whiteboards don't pass this; their background is whatever the
        host renders behind the canvas. */
    backgroundUrl?: string;
  }

  let {
    value = $bindable(),
    tool = $bindable('pen'),
    color = $bindable('#ef4444'),
    width = $bindable(6),
    opacity = $bindable(1),
    activeLayer = $bindable(0),
    readOnly = false,
    backgroundUrl,
  }: Props = $props();

  // ── DOM refs ──────────────────────────────────────────────────────

  let canvasEl: HTMLCanvasElement | undefined = $state();
  let wrapperEl: HTMLDivElement | undefined = $state();
  let ctx: CanvasRenderingContext2D | null = null;

  // ── Live state ────────────────────────────────────────────────────

  // The stroke currently being drawn (pointer-down through pointer-
  // up). Re-rendered every pointer-move; promoted into the layer's
  // strokes array on pointer-up.
  let livePoints: Point[] = $state([]);
  let drawing = $state(false);

  // History — undo / redo. Each entry is a full BrushContent snapshot.
  // Bounded at 64 so a long session doesn't eat memory. Snapshot-based
  // instead of action-log because strokes are immutable once committed
  // and the layer structure can change in non-stroke ways (visibility,
  // opacity, reorder) — a single snapshot covers all of it.
  let history: BrushContent[] = $state([]);
  let historyIdx = $state(-1);
  const HISTORY_MAX = 64;

  // Optional background image (annotation surface). Loaded async; we
  // re-render once it lands.
  let backgroundImg: HTMLImageElement | null = $state(null);

  // ── Canvas sizing (DPR-aware) ─────────────────────────────────────

  function fitToWrapper() {
    if (!canvasEl || !wrapperEl || !ctx) return;
    const dpr = window.devicePixelRatio || 1;
    const w = wrapperEl.clientWidth;
    const h = wrapperEl.clientHeight;
    // The DRAWING surface (logical pixels) stays at source_w × source_h
    // so saved strokes remain coordinate-stable across viewport sizes.
    // The DISPLAY surface (CSS pixels) fills the wrapper.
    canvasEl.style.width = `${w}px`;
    canvasEl.style.height = `${h}px`;
    canvasEl.width = Math.round(value.source_w * dpr);
    canvasEl.height = Math.round(value.source_h * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    render();
  }

  // ── Pointer → source-canvas coords ────────────────────────────────

  function pointFromEvent(e: PointerEvent): Point {
    if (!canvasEl) return [0, 0];
    const rect = canvasEl.getBoundingClientRect();
    // Map from display (CSS) px to source-canvas px so strokes are
    // resolution-independent.
    const x = ((e.clientX - rect.left) / rect.width) * value.source_w;
    const y = ((e.clientY - rect.top) / rect.height) * value.source_h;
    const p = e.pressure > 0 && e.pressure !== 0.5 ? e.pressure : 0.5;
    return [x, y, p];
  }

  // ── Render ────────────────────────────────────────────────────────

  function render() {
    if (!ctx || !canvasEl) return;
    ctx.clearRect(0, 0, value.source_w, value.source_h);
    if (backgroundImg) {
      // Fit-contain: maintain aspect ratio inside the source-canvas
      // rect. Image annotations always want the picture centered, not
      // stretched.
      const iw = backgroundImg.naturalWidth;
      const ih = backgroundImg.naturalHeight;
      if (iw > 0 && ih > 0) {
        const r = Math.min(value.source_w / iw, value.source_h / ih);
        const w = iw * r;
        const h = ih * r;
        ctx.drawImage(
          backgroundImg,
          (value.source_w - w) / 2,
          (value.source_h - h) / 2,
          w,
          h,
        );
      }
    }
    for (const layer of value.layers) {
      if (!layer.visible) continue;
      ctx.save();
      ctx.globalAlpha = layer.opacity;
      for (const stroke of layer.strokes) {
        drawStroke(stroke);
      }
      ctx.restore();
    }
    // Live stroke — preview the one being drawn right now, on top of
    // the active layer's existing strokes.
    if (drawing && livePoints.length > 0) {
      const liveStroke: Stroke = {
        tool,
        color,
        width,
        opacity,
        points: livePoints,
      };
      drawStroke(liveStroke);
    }
  }

  function drawStroke(stroke: Stroke) {
    if (!ctx) return;
    if (stroke.points.length === 0) return;
    const opts = strokeOptionsFor(stroke.tool);
    // perfect-freehand size multiplier — we pass the stroke's base
    // width through here, and the library handles pressure / velocity
    // modulation inside the polygon math.
    const outline = getStroke(
      stroke.points.map((p) => [p[0], p[1], p[2] ?? 0.5]) as number[][],
      { ...opts, size: stroke.width },
    );
    if (outline.length === 0) return;

    ctx.save();
    if (stroke.tool === 'eraser') {
      // destination-out cuts what we draw out of every existing pixel
      // below. Color is irrelevant for the eraser; we use opaque
      // black so the cutout is full-strength.
      ctx.globalCompositeOperation = 'destination-out';
      ctx.fillStyle = '#000';
    } else {
      ctx.globalCompositeOperation = 'source-over';
      ctx.fillStyle = stroke.color;
      ctx.globalAlpha = (ctx.globalAlpha ?? 1) * (stroke.opacity ?? 1);
    }

    // perfect-freehand returns the stroke as an outline polygon —
    // a closed Path2D rendered as a fill gives us the rich variable-
    // width look that's the whole point of the library.
    const path = new Path2D();
    path.moveTo(outline[0][0], outline[0][1]);
    for (let i = 1; i < outline.length; i++) {
      path.lineTo(outline[i][0], outline[i][1]);
    }
    path.closePath();
    ctx.fill(path);
    ctx.restore();
  }

  // ── Stroke commit + history ───────────────────────────────────────

  function pushHistory(snapshot: BrushContent) {
    // Drop the redo tail when we branch off an undone state.
    history = history.slice(0, historyIdx + 1);
    history.push(JSON.parse(JSON.stringify($state.snapshot(snapshot))));
    if (history.length > HISTORY_MAX) {
      history = history.slice(history.length - HISTORY_MAX);
    }
    historyIdx = history.length - 1;
  }

  export function undo() {
    if (historyIdx <= 0) return;
    historyIdx -= 1;
    value = (JSON.parse(JSON.stringify(history[historyIdx])) as BrushContent);
    render();
  }
  export function redo() {
    if (historyIdx >= history.length - 1) return;
    historyIdx += 1;
    value = (JSON.parse(JSON.stringify(history[historyIdx])) as BrushContent);
    render();
  }
  export function canUndo() { return historyIdx > 0; }
  export function canRedo() { return historyIdx < history.length - 1; }
  export function clearAll() {
    const next: BrushContent = {
      ...value,
      layers: value.layers.map((l) => ({ ...l, strokes: [] })),
    };
    value = next;
    pushHistory(next);
    render();
  }
  /** Get a rasterized PNG snapshot of the current state (excludes
   *  the backgroundImg — we want the strokes alone for OCR / AI /
   *  PDF). Returns a base64-encoded data URL the host can POST. */
  export function snapshotPng(): string {
    if (!canvasEl) return '';
    // Temporarily strip the background and re-render. Cheaper than
    // a second hidden canvas because we already have one.
    const savedBg = backgroundImg;
    backgroundImg = null;
    render();
    const out = canvasEl.toDataURL('image/png');
    backgroundImg = savedBg;
    render();
    return out;
  }

  // ── Pointer handlers ──────────────────────────────────────────────

  function onPointerDown(e: PointerEvent) {
    if (readOnly) return;
    if (e.button !== 0 && e.pointerType === 'mouse') return; // left only
    if (!canvasEl) return;
    canvasEl.setPointerCapture(e.pointerId);
    drawing = true;
    livePoints = [pointFromEvent(e)];
    render();
    e.preventDefault();
  }
  function onPointerMove(e: PointerEvent) {
    if (!drawing) return;
    livePoints = [...livePoints, pointFromEvent(e)];
    render();
  }
  function onPointerUp(e: PointerEvent) {
    if (!drawing) return;
    if (canvasEl?.hasPointerCapture(e.pointerId)) {
      canvasEl.releasePointerCapture(e.pointerId);
    }
    drawing = false;
    if (livePoints.length === 0) return;
    // Tap (single point) — keep it as a dot, perfect-freehand handles
    // single-sample strokes as a small filled disc.
    const newStroke: Stroke = {
      tool,
      color,
      width,
      opacity: opacity ?? defaultOpacityFor(tool),
      points: livePoints,
    };
    const next: BrushContent = {
      ...value,
      layers: value.layers.map((l, i) =>
        i === activeLayer ? { ...l, strokes: [...l.strokes, newStroke] } : l,
      ),
    };
    value = next;
    pushHistory(next);
    livePoints = [];
    render();
  }

  // ── Lifecycle ─────────────────────────────────────────────────────

  onMount(() => {
    if (!canvasEl) return;
    ctx = canvasEl.getContext('2d');
    if (!ctx) return;
    pushHistory(value); // seed history with the initial state
    fitToWrapper();

    const ro = new ResizeObserver(fitToWrapper);
    if (wrapperEl) ro.observe(wrapperEl);

    if (backgroundUrl) {
      const img = new Image();
      img.crossOrigin = 'anonymous';
      img.onload = () => {
        backgroundImg = img;
        render();
      };
      img.src = backgroundUrl;
    }

    return () => ro.disconnect();
  });

  onDestroy(() => {
    ctx = null;
  });

  // Re-render when value changes externally (host loaded a different
  // doc, undo/redo, etc).
  $effect(() => {
    void value; // subscribe
    render();
  });

  // Re-render when the background URL changes.
  $effect(() => {
    const url = backgroundUrl;
    if (!url) {
      backgroundImg = null;
      return;
    }
    const img = new Image();
    img.crossOrigin = 'anonymous';
    img.onload = () => {
      backgroundImg = img;
      render();
    };
    img.src = url;
  });

  // Re-fit when source dimensions change (rare, but the host can swap
  // a doc with different source_w/h).
  $effect(() => {
    void value.source_w;
    void value.source_h;
    fitToWrapper();
  });

  // The pointer-up handler also needs to fire when the pointer leaves
  // the canvas mid-stroke (drag-out + release elsewhere). Listening on
  // window during a stroke covers that without the full pointer-
  // capture dance failing on touch devices.
  function handleWindowPointerUp(e: PointerEvent) {
    if (drawing) onPointerUp(e);
  }
</script>

<svelte:window onpointerup={handleWindowPointerUp} onpointercancel={handleWindowPointerUp} />

<div
  bind:this={wrapperEl}
  class="relative h-full w-full select-none"
  class:cursor-crosshair={!readOnly}
>
  <canvas
    bind:this={canvasEl}
    class="absolute inset-0 h-full w-full touch-none"
    style:cursor={readOnly ? 'default' : 'crosshair'}
    onpointerdown={onPointerDown}
    onpointermove={onPointerMove}
    onpointerup={onPointerUp}
  ></canvas>
</div>
