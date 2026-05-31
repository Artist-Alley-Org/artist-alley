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
    BBox, BrushContent, ImageItem, Item, Layer, ShapeItem, StrokeItem, TextItem,
  } from '$lib/whiteboard/types';
  import {
    MAX_PASTED_IMAGE_BYTES,
    DEFAULT_FONT_FAMILY,
    isBrushTool, isShapeTool, strokeOptionsFor,
    itemBBox, pointInItem, translateItem, resizeItemToBBox,
  } from '$lib/whiteboard/types';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';

  // Per-session local clipboard for copy/cut/paste of items. Lives
  // in module scope so it survives canvas remounts within the same
  // tab. Shared across whiteboard sessions in the same tab — useful
  // when the user copies from one sketch and pastes into another.
  let sessionClipboard: Item | null = null;

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
    if (s.rotation) {
      const cx = x + w / 2;
      const cy = y + h / 2;
      ctx.translate(cx, cy);
      ctx.rotate((s.rotation * Math.PI) / 180);
      ctx.translate(-cx, -cy);
    }
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

  // ── Pointer handlers ──────────────────────────────────────────────

  // ── Selection drag state ─────────────────────────────────────────
  // Active transform gesture started by mousedown on the selected
  // item's bbox or one of its handles. The mousemove handler reads
  // this to know what to update; mouseup clears it + commits.
  type SelectGesture =
    | { kind: 'move'; startX: number; startY: number; original: Item }
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

  function onPointerDown(e: PointerEvent) {
    if (readOnly) return;
    if (e.button !== 0 && e.pointerType === 'mouse') return;
    if (!canvasEl) return;

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
          selectGesture = {
            kind: 'move',
            startX: p.x,
            startY: p.y,
            original: JSON.parse(JSON.stringify(item)),
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
  function bboxToCss(bb: BBox): { left: number; top: number; width: number; height: number } {
    const a = sourceToCss(bb.x, bb.y);
    const b = sourceToCss(bb.x + bb.w, bb.y + bb.h);
    return { left: a.left, top: a.top, width: b.left - a.left, height: b.top - a.top };
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
      originalRotation: sel.item.kind !== 'stroke' ? (sel.item.rotation ?? 0) : 0,
      original: JSON.parse(JSON.stringify(sel.item)),
    };
    if (canvasEl) canvasEl.setPointerCapture(e.pointerId);
  }

  // Double-click a text item with the select tool active → re-enter
  // edit mode at that item. The contenteditable overlay reuses the
  // text-input UI so commit-on-blur/Enter works the same way.
  let editingTextRef: { layerId: string; index: number } | null = $state(null);

  function onCanvasDblClick(e: MouseEvent) {
    if (readOnly) return;
    if (session.tool !== 'select') return;
    const p = eventToSource(e);
    const hit = pickItem(p.x, p.y);
    if (!hit) return;
    const layer = session.doc.layers.find((l) => l.id === hit.layerId);
    const item = layer?.items[hit.index];
    if (!item || item.kind !== 'text') return;
    editingTextRef = { layerId: hit.layerId, index: hit.index };
    textEditBody = item.body;
    queueMicrotask(() => textEditRef?.focus());
    e.preventDefault();
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
    // Delete / Backspace = remove selected item.
    if ((e.key === 'Delete' || e.key === 'Backspace') && session.selection) {
      e.preventDefault();
      const sel = session.selection;
      session.removeItems(sel.layerId, [sel.index]);
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
      : session.tool === 'select'
        ? 'default'
        : session.tool === 'text'
          ? 'text'
          : 'crosshair'}
    onpointerdown={onPointerDown}
    onpointermove={onPointerMove}
    onpointerup={onPointerUp}
    ondblclick={onCanvasDblClick}
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
        style:font-size={`${item.fontSize * (wrapperEl ? (wrapperEl.clientWidth / session.doc.source_w) : 1)}px`}
        style:font-family={item.fontFamily ?? 'system-ui'}
        style:color={item.color}
        oninput={(e) => textEditBody = (e.currentTarget as HTMLDivElement).innerText}
        onkeydown={(e) => { if (e.key === 'Escape' || (e.key === 'Enter' && !e.shiftKey)) { e.preventDefault(); commitTextEdit2(); } }}
        onblur={commitTextEdit2}
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
      style:font-size={`${Math.max(14, session.width * 2.5) * (wrapperEl ? (wrapperEl.clientWidth / session.doc.source_w) : 1)}px`}
      style:color={session.color}
      oninput={onTextInput}
      onkeydown={onTextEditKey}
      onblur={commitTextEdit}
    ></div>
  {/if}
</div>
