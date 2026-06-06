<script lang="ts">
  // WhiteboardMinimap — Figma-style bird's-eye view of the
  // whole whiteboard. Lives in the top-right corner of the canvas
  // surface and gives the user three things:
  //   1. Spatial reference — at deep zoom-out or far pan, where is
  //      my work relative to where I am?
  //   2. Click-to-jump — click anywhere in the minimap to recenter
  //      the viewport on that world point.
  //   3. Drag-the-viewport-rectangle — alternative pan affordance
  //      for users who don't know about middle-click / space-drag.
  //
  // Caching (the bit that keeps it fast):
  // The item content is rendered to an *offscreen* canvas that only
  // updates when the doc changes (items added / removed / edited).
  // Pan and zoom do NOT trigger a re-render — only the viewport
  // overlay rectangle re-positions, via CSS. That keeps the minimap
  // free even during continuous wheel-zoom.
  //
  // The world bbox we map into the minimap is computed once per
  // doc change: the union of (every item's bbox, the source-doc
  // rect, the current viewport rect). That way the user always sees
  // where they are, even if they've panned past every item.

  import { onMount, onDestroy } from 'svelte';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';
  import { itemBBox } from '$lib/whiteboard/types';

  interface Props { session: WhiteboardSession; }
  let { session }: Props = $props();

  // Fixed minimap dimensions in CSS pixels. Aspect ratio matches a
  // typical landscape doc (4:3); the contained content scales
  // proportionally so the doc fits at any size.
  const MAP_W = 192;
  const MAP_H = 128;

  let mapCanvas: HTMLCanvasElement | undefined = $state();
  let containerEl: HTMLDivElement | undefined = $state();
  // Tracks the world-space bbox the minimap is currently showing.
  // Used by both the offscreen render + the viewport rectangle
  // positioning so they stay in sync.
  let worldBBox = $state({ x: 0, y: 0, w: 1, h: 1 });

  /** Compute the world-space bbox to display. Union of:
   *  - every visible item's bbox
   *  - the source-doc rect (so users see the "page" frame)
   *  - the current viewport rect (so the user's not "off-map" even
   *    when they've panned far past their work)
   *  Padded 10% to give breathing room. */
  function computeWorldBBox(): { x: number; y: number; w: number; h: number } {
    let minX = 0, minY = 0;
    let maxX = session.doc.source_w;
    let maxY = session.doc.source_h;
    for (const layer of session.doc.layers) {
      if (!layer.visible) continue;
      for (const item of layer.items) {
        const bb = itemBBox(item);
        if (bb.w === 0 || bb.h === 0) continue;
        if (bb.x < minX) minX = bb.x;
        if (bb.y < minY) minY = bb.y;
        if (bb.x + bb.w > maxX) maxX = bb.x + bb.w;
        if (bb.y + bb.h > maxY) maxY = bb.y + bb.h;
      }
    }
    // Include the live viewport rect in world coords so the
    // viewport marker is always inside the visible map area.
    const surface = document.getElementById('aa-whiteboard-surface');
    if (surface && session.viewZoom > 0) {
      const r = surface.getBoundingClientRect();
      const vx0 = -session.viewX / session.viewZoom;
      const vy0 = -session.viewY / session.viewZoom;
      const vx1 = (r.width - session.viewX) / session.viewZoom;
      const vy1 = (r.height - session.viewY) / session.viewZoom;
      if (vx0 < minX) minX = vx0;
      if (vy0 < minY) minY = vy0;
      if (vx1 > maxX) maxX = vx1;
      if (vy1 > maxY) maxY = vy1;
    }
    const w = Math.max(1, maxX - minX);
    const h = Math.max(1, maxY - minY);
    const padX = w * 0.1;
    const padY = h * 0.1;
    return { x: minX - padX, y: minY - padY, w: w + padX * 2, h: h + padY * 2 };
  }

  /** Re-render the offscreen canvas. Cached: only fires when the
   *  doc changes (see $effect below). */
  function renderMap() {
    if (!mapCanvas) return;
    const ctx = mapCanvas.getContext('2d');
    if (!ctx) return;
    const dpr = window.devicePixelRatio || 1;
    mapCanvas.width = MAP_W * dpr;
    mapCanvas.height = MAP_H * dpr;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    // Background
    ctx.fillStyle = session.doc.canvas_color ?? '#ffffff';
    ctx.fillRect(0, 0, MAP_W, MAP_H);

    const bb = computeWorldBBox();
    worldBBox = bb;
    // Letterbox the world into the minimap rect so we never stretch.
    const sx = MAP_W / bb.w;
    const sy = MAP_H / bb.h;
    const s = Math.min(sx, sy);
    const ox = (MAP_W - bb.w * s) / 2 - bb.x * s;
    const oy = (MAP_H - bb.h * s) / 2 - bb.y * s;

    // Source-doc rect outline.
    ctx.strokeStyle = 'rgba(100, 116, 139, 0.5)';
    ctx.lineWidth = 1;
    ctx.strokeRect(ox, oy, session.doc.source_w * s, session.doc.source_h * s);

    // Every item rendered as its bbox-as-rect at the minimap scale.
    // Strokes use a representative color (the stroke's own color);
    // shapes / text use fillColor or color; images render as a
    // generic gray rect (we don't shrink-render the bitmap — that
    // would tank perf on a big paste).
    for (const layer of session.doc.layers) {
      if (!layer.visible) continue;
      for (const item of layer.items) {
        const ibb = itemBBox(item);
        if (ibb.w === 0 || ibb.h === 0) continue;
        const rx = ox + ibb.x * s;
        const ry = oy + ibb.y * s;
        const rw = Math.max(1, ibb.w * s);
        const rh = Math.max(1, ibb.h * s);
        if (item.kind === 'stroke') {
          ctx.fillStyle = item.color;
          ctx.globalAlpha = (item.opacity ?? 1) * layer.opacity;
          ctx.fillRect(rx, ry, rw, rh);
        } else if (item.kind === 'shape') {
          const fill = item.fillColor ?? item.color;
          const stroke = item.strokeColor ?? item.color;
          ctx.globalAlpha = layer.opacity;
          if (item.fill && item.fill > 0) {
            ctx.fillStyle = fill;
            ctx.fillRect(rx, ry, rw, rh);
          }
          if (item.width > 0) {
            ctx.strokeStyle = stroke;
            ctx.lineWidth = 0.5;
            ctx.strokeRect(rx, ry, rw, rh);
          }
        } else if (item.kind === 'text') {
          ctx.fillStyle = item.color;
          ctx.globalAlpha = layer.opacity;
          ctx.fillRect(rx, ry, rw, rh);
        } else {
          ctx.fillStyle = 'rgba(148, 163, 184, 0.6)';
          ctx.globalAlpha = layer.opacity;
          ctx.fillRect(rx, ry, rw, rh);
        }
      }
    }
    ctx.globalAlpha = 1;
  }

  // World → minimap CSS coords (for the viewport rectangle overlay).
  function worldToMap(wx: number, wy: number): { x: number; y: number } {
    const bb = worldBBox;
    const sx = MAP_W / bb.w;
    const sy = MAP_H / bb.h;
    const s = Math.min(sx, sy);
    const ox = (MAP_W - bb.w * s) / 2 - bb.x * s;
    const oy = (MAP_H - bb.h * s) / 2 - bb.y * s;
    return { x: ox + wx * s, y: oy + wy * s };
  }

  // Viewport rectangle in minimap CSS coords — reactive on view +
  // worldBBox so it tracks pan / zoom without re-rendering the
  // offscreen content.
  const viewportRect = $derived.by(() => {
    const surface = document.getElementById('aa-whiteboard-surface');
    if (!surface) return null;
    const r = surface.getBoundingClientRect();
    const wx0 = -session.viewX / session.viewZoom;
    const wy0 = -session.viewY / session.viewZoom;
    const wx1 = (r.width - session.viewX) / session.viewZoom;
    const wy1 = (r.height - session.viewY) / session.viewZoom;
    const a = worldToMap(wx0, wy0);
    const b = worldToMap(wx1, wy1);
    return {
      x: Math.max(0, Math.min(MAP_W, a.x)),
      y: Math.max(0, Math.min(MAP_H, a.y)),
      w: Math.max(2, Math.min(MAP_W, b.x - a.x)),
      h: Math.max(2, Math.min(MAP_H, b.y - a.y)),
    };
  });

  // Click / drag inside the minimap → recenter the viewport on the
  // clicked world point. Drag is just continuous click — same handler
  // captures pointer + updates on every move.
  function recenterOnClick(e: PointerEvent) {
    if (!containerEl) return;
    const r = containerEl.getBoundingClientRect();
    const mx = e.clientX - r.left;
    const my = e.clientY - r.top;
    const bb = worldBBox;
    const s = Math.min(MAP_W / bb.w, MAP_H / bb.h);
    const ox = (MAP_W - bb.w * s) / 2 - bb.x * s;
    const oy = (MAP_H - bb.h * s) / 2 - bb.y * s;
    // World point under the minimap click.
    const wx = (mx - ox) / s;
    const wy = (my - oy) / s;
    // Recenter viewport on (wx, wy).
    const surface = document.getElementById('aa-whiteboard-surface');
    const sr = surface?.getBoundingClientRect();
    if (!sr) return;
    session.viewX = sr.width / 2 - wx * session.viewZoom;
    session.viewY = sr.height / 2 - wy * session.viewZoom;
  }

  let dragging = $state(false);
  function onDown(e: PointerEvent) {
    if (!containerEl) return;
    dragging = true;
    containerEl.setPointerCapture(e.pointerId);
    recenterOnClick(e);
  }
  function onMove(e: PointerEvent) {
    if (dragging) recenterOnClick(e);
  }
  function onUp(e: PointerEvent) {
    dragging = false;
    if (containerEl?.hasPointerCapture(e.pointerId)) containerEl.releasePointerCapture(e.pointerId);
  }

  // Doc-change re-render. The render itself is cheap-ish (one fillRect
  // per item) so we don't bother throttling at the typical item count.
  // If the doc gets very large the soft-cap UI in BrushCanvas warns
  // the user before this becomes a perf wall.
  $effect(() => {
    void session.doc;
    renderMap();
  });

  onMount(() => {
    renderMap();
    const ro = new ResizeObserver(renderMap);
    const surface = document.getElementById('aa-whiteboard-surface');
    if (surface) ro.observe(surface);
    return () => ro.disconnect();
  });
  onDestroy(() => { /* nothing else to clean up */ });
</script>

<div
  bind:this={containerEl}
  class="absolute right-3 top-14 z-30 select-none rounded-lg border border-black/30 bg-black/70 p-1 shadow-lg"
  style:width={`${MAP_W + 8}px`}
  style:height={`${MAP_H + 8}px`}
  onpointerdown={onDown}
  onpointermove={onMove}
  onpointerup={onUp}
  role="region"
  aria-label="Whiteboard minimap"
>
  <div class="relative h-full w-full overflow-hidden rounded-sm">
    <canvas
      bind:this={mapCanvas}
      class="absolute inset-0 h-full w-full cursor-pointer"
      style:width={`${MAP_W}px`}
      style:height={`${MAP_H}px`}
    ></canvas>
    {#if viewportRect}
      <!-- Viewport indicator — translucent rect with a sharp outline.
           Pointer-events disabled so the click/drag handler on the
           container catches everything. -->
      <div
        class="pointer-events-none absolute border-2 border-accent bg-accent/15"
        style:left={`${viewportRect.x}px`}
        style:top={`${viewportRect.y}px`}
        style:width={`${viewportRect.w}px`}
        style:height={`${viewportRect.h}px`}
      ></div>
    {/if}
  </div>
</div>
