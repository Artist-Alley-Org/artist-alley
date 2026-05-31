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
    BrushContent, ImageItem, Item, Layer, ShapeItem, StrokeItem, TextItem,
  } from '$lib/whiteboard/types';
  import {
    MAX_PASTED_IMAGE_BYTES,
    isBrushTool, isShapeTool, strokeOptionsFor,
  } from '$lib/whiteboard/types';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';

  interface Props {
    /** Shared reactive session. Owns doc, active layer, tools. */
    session: WhiteboardSession;
    /** Read-only mode — render but don't accept input (sidebar
        preview of a saved whiteboard). */
    readOnly?: boolean;
    /** Optional asset reference image — when present, drawn beneath
        the items (annotation use case). Whiteboards don't pass this. */
    backgroundUrl?: string;
  }

  let { session, readOnly = false, backgroundUrl }: Props = $props();

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

  function fitToWrapper() {
    if (!canvasEl || !wrapperEl || !ctx) return;
    const dpr = window.devicePixelRatio || 1;
    const wW = wrapperEl.clientWidth;
    const wH = wrapperEl.clientHeight;
    canvasEl.style.width = `${wW}px`;
    canvasEl.style.height = `${wH}px`;
    canvasEl.width = Math.round(session.doc.source_w * dpr);
    canvasEl.height = Math.round(session.doc.source_h * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    render();
  }

  // ── Source-canvas coord conversion ────────────────────────────────

  function eventToSource(e: PointerEvent | MouseEvent): { x: number; y: number; p?: number } {
    if (!canvasEl) return { x: 0, y: 0 };
    const rect = canvasEl.getBoundingClientRect();
    const x = ((e.clientX - rect.left) / rect.width) * session.doc.source_w;
    const y = ((e.clientY - rect.top) / rect.height) * session.doc.source_h;
    const p = (e as PointerEvent).pressure;
    return { x, y, p: p > 0 && p !== 0.5 ? p : undefined };
  }

  function sourceToCss(x: number, y: number): { left: number; top: number } {
    if (!canvasEl) return { left: 0, top: 0 };
    const rect = canvasEl.getBoundingClientRect();
    return {
      left: (x / session.doc.source_w) * rect.width,
      top: (y / session.doc.source_h) * rect.height,
    };
  }

  // ── Render ────────────────────────────────────────────────────────

  function render() {
    if (!ctx || !canvasEl) return;
    ctx.clearRect(0, 0, session.doc.source_w, session.doc.source_h);

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

    // Layers bottom-to-top
    for (const layer of session.doc.layers) {
      if (!layer.visible) continue;
      ctx.save();
      ctx.globalAlpha = layer.opacity;
      for (const item of layer.items) drawItem(item);
      ctx.restore();
    }

    // Live previews on top
    if (liveStroke) drawItem(liveStroke);
    if (liveShape) drawItem(liveShape);
  }

  function drawItem(item: Item) {
    if (!ctx) return;
    switch (item.kind) {
      case 'stroke': return drawStroke(item);
      case 'shape': return drawShape(item);
      case 'text': return drawText(item);
      case 'image': return drawImageItem(item);
    }
  }

  function drawStroke(stroke: StrokeItem) {
    if (!ctx) return;
    if (stroke.points.length === 0) return;
    const opts = strokeOptionsFor(stroke.tool);
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
      ctx.globalAlpha = (ctx.globalAlpha ?? 1) * (stroke.opacity ?? 1);
    }
    const path = new Path2D();
    path.moveTo(outline[0][0], outline[0][1]);
    for (let i = 1; i < outline.length; i++) path.lineTo(outline[i][0], outline[i][1]);
    path.closePath();
    ctx.fill(path);
    ctx.restore();
  }

  function drawShape(s: ShapeItem) {
    if (!ctx) return;
    // Normalize so negative w/h still draw correctly.
    const x = s.w >= 0 ? s.x : s.x + s.w;
    const y = s.h >= 0 ? s.y : s.y + s.h;
    const w = Math.abs(s.w);
    const h = Math.abs(s.h);
    ctx.save();
    ctx.strokeStyle = s.color;
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
        ctx.fillStyle = s.color;
        const prevAlpha = ctx.globalAlpha;
        ctx.globalAlpha = prevAlpha * s.fill;
        ctx.fillRect(x, y, w, h);
        ctx.globalAlpha = prevAlpha;
      }
      ctx.strokeRect(x, y, w, h);
    } else if (s.tool === 'ellipse') {
      ctx.beginPath();
      ctx.ellipse(x + w / 2, y + h / 2, w / 2, h / 2, 0, 0, Math.PI * 2);
      if (s.fill && s.fill > 0) {
        ctx.fillStyle = s.color;
        const prevAlpha = ctx.globalAlpha;
        ctx.globalAlpha = prevAlpha * s.fill;
        ctx.fill();
        ctx.globalAlpha = prevAlpha;
      }
      ctx.stroke();
    }
    ctx.restore();
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

  function drawText(t: TextItem) {
    if (!ctx) return;
    if (!t.body) return;
    ctx.save();
    ctx.fillStyle = t.color;
    ctx.globalAlpha = (ctx.globalAlpha ?? 1);
    const weight = t.bold ? '700' : '500';
    const style = t.italic ? 'italic ' : '';
    ctx.font = `${style}${weight} ${t.fontSize}px system-ui, -apple-system, sans-serif`;
    ctx.textBaseline = 'top';
    ctx.textAlign = t.align ?? 'left';
    const x = t.align === 'center' ? t.x + t.w / 2 : (t.align === 'right' ? t.x + t.w : t.x);
    // Wrap on \n; no auto-wrap for now (commit-time editor handles it).
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

  // ── Pointer handlers ──────────────────────────────────────────────

  function onPointerDown(e: PointerEvent) {
    if (readOnly) return;
    if (e.button !== 0 && e.pointerType === 'mouse') return;
    if (!canvasEl) return;

    // Block input on the active layer when it's locked.
    const layer = session.doc.layers.find((l) => l.id === session.activeLayerId);
    if (!layer || layer.locked) return;

    canvasEl.setPointerCapture(e.pointerId);
    const p = eventToSource(e);

    if (isBrushTool(session.tool)) {
      liveStroke = {
        kind: 'stroke',
        tool: session.tool,
        color: session.color,
        width: session.width,
        opacity: session.opacity,
        points: [[p.x, p.y, p.p ?? 0.5]],
      };
      render();
      e.preventDefault();
    } else if (isShapeTool(session.tool)) {
      dragStart = { x: p.x, y: p.y };
      liveShape = {
        kind: 'shape',
        tool: session.tool,
        x: p.x, y: p.y, w: 0, h: 0,
        color: session.color,
        width: session.width,
        fill: session.fillShapes ? 0.25 : 0,
        opacity: session.opacity,
      };
      render();
      e.preventDefault();
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
    const p = eventToSource(e);
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
    }
  }

  function onPointerUp(e: PointerEvent) {
    if (canvasEl?.hasPointerCapture(e.pointerId)) {
      canvasEl.releasePointerCapture(e.pointerId);
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
    }
    render();
  }

  function handleWindowPointerUp(e: PointerEvent) {
    if (liveStroke || liveShape) onPointerUp(e);
  }

  // ── Text edit commit ─────────────────────────────────────────────

  function commitTextEdit() {
    if (!textEdit) return;
    const body = textEditBody.trim();
    const layerId = session.activeLayerId;
    if (body && layerId) {
      // Measure to size the saved bounding box (so re-render matches
      // what the user saw). Naive single-line width measurement; the
      // canvas renderer wraps on \n explicitly anyway.
      const fontSize = Math.max(14, session.width * 2.5);
      const item: TextItem = {
        kind: 'text',
        x: textEdit.x,
        y: textEdit.y,
        w: textEdit.w,
        h: textEdit.h,
        body,
        fontSize,
        color: session.color,
        align: 'left',
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
    return () => ro.disconnect();
  });

  onDestroy(() => { ctx = null; });

  // Re-render reactively when session doc changes (undo/redo, items
  // added by paste, layer toggles, etc).
  $effect(() => {
    void session.doc;
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

<svelte:window onpointerup={handleWindowPointerUp} onpointercancel={handleWindowPointerUp} />

<div
  bind:this={wrapperEl}
  class="relative h-full w-full select-none"
  onpaste={onPaste}
  tabindex="-1"
>
  <canvas
    bind:this={canvasEl}
    class="absolute inset-0 h-full w-full touch-none"
    style:cursor={readOnly
      ? 'default'
      : session.tool === 'text'
        ? 'text'
        : isShapeTool(session.tool)
          ? 'crosshair'
          : 'crosshair'}
    onpointerdown={onPointerDown}
    onpointermove={onPointerMove}
    onpointerup={onPointerUp}
  ></canvas>

  {#if textEdit && !readOnly}
    {@const css = sourceToCss(textEdit.x, textEdit.y)}
    <div
      bind:this={textEditRef}
      contenteditable="true"
      class="absolute z-10 min-h-[1.5em] min-w-[120px] cursor-text rounded border border-accent bg-white/95 px-1 text-black outline-none focus:ring-2 focus:ring-accent"
      style:left={`${css.left}px`}
      style:top={`${css.top}px`}
      style:font-size={`${Math.max(14, session.width * 2.5) * (wrapperEl ? (wrapperEl.clientWidth / session.doc.source_w) : 1)}px`}
      style:color={session.color}
      oninput={onTextInput}
      onkeydown={onTextEditKey}
      onblur={commitTextEdit}
    ></div>
  {/if}
</div>
