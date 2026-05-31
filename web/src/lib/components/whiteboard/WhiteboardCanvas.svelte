<script lang="ts">
  // WhiteboardCanvas — the brush surface inside the AssetViewer's
  // canvas area.
  //
  // This used to be a top-layer <dialog showModal()> covering the
  // viewport, but that hid the sidebar (which is in a lower layer)
  // and broke the "tools in the side panel" pattern. Now we render
  // as a normal positioned div fed through AssetViewer's
  // canvasOverlay slot — covers only the asset canvas, leaves the
  // sidebar + top toolbar fully reachable.
  //
  // The host (PostHost) controls when this mounts via {#if
  // whiteboardOpen}; we don't manage dialog state ourselves.
  // Keyboard shortcuts (Esc, Ctrl-Z, tool quick-keys) bind via
  // svelte:window so they work even when the canvas itself doesn't
  // have focus.

  import BrushCanvas from './BrushCanvas.svelte';
  import WhiteboardMinimap from './WhiteboardMinimap.svelte';
  import { itemBBox } from '$lib/whiteboard/types';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';

  interface Props {
    session: WhiteboardSession;
    /** Called when the user presses ESC or the close pill. */
    onClose: () => void;
  }

  let { session, onClose }: Props = $props();

  // C-1.19 — zoom controls helper.  Uses the viewport mid-point as
  // the zoom anchor when triggered from a button (vs the cursor for
  // wheel-zoom). 1.2× per click matches Figma's `+` / `-` step.
  function zoomFromButton(factor: number) {
    const el = document.getElementById('aa-whiteboard-surface');
    const r = el?.getBoundingClientRect();
    const cx = r ? r.width / 2 : window.innerWidth / 2;
    const cy = r ? r.height / 2 : window.innerHeight / 2;
    session.zoomBy(factor, cx, cy);
  }
  function fitToContent() {
    const el = document.getElementById('aa-whiteboard-surface');
    if (!el) return;
    const r = el.getBoundingClientRect();
    // Compute bbox over every item; fall back to source-doc rect.
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    let any = false;
    // Delegate per-item bbox math to `itemBBox` so this loop handles
    // every kind uniformly — including connectors (Phase 1.22)
    // whose bbox spans resolved endpoint positions.
    for (const layer of session.doc.layers) {
      if (!layer.visible) continue;
      for (const it of layer.items) {
        any = true;
        const bb = itemBBox(it, session.doc);
        if (bb.x < minX) minX = bb.x;
        if (bb.y < minY) minY = bb.y;
        if (bb.x + bb.w > maxX) maxX = bb.x + bb.w;
        if (bb.y + bb.h > maxY) maxY = bb.y + bb.h;
      }
    }
    if (!any) { session.fitView(r.width, r.height); return; }
    const cw = maxX - minX, ch = maxY - minY;
    const margin = 64;
    const z = Math.max(0.05, Math.min(16, Math.min((r.width - margin*2) / cw, (r.height - margin*2) / ch)));
    session.viewZoom = z;
    session.viewX = (r.width - cw * z) / 2 - minX * z;
    session.viewY = (r.height - ch * z) / 2 - minY * z;
  }

  function handleKey(e: KeyboardEvent) {
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z') {
      e.preventDefault();
      if (e.shiftKey) session.redo(); else session.undo();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'y') {
      e.preventDefault();
      session.redo();
      return;
    }
    const map: Record<string, typeof session.tool> = {
      v: 'select',
      p: 'pen', m: 'marker', h: 'highlighter', e: 'eraser',
      l: 'line', a: 'arrow', r: 'rect', o: 'ellipse', g: 'triangle',
      b: 'bucket', i: 'eyedropper',
      t: 'text',
      q: 'lasso', c: 'crop',
    };
    const next = map[e.key.toLowerCase()];
    if (next) {
      e.preventDefault();
      session.tool = next;
      return;
    }
    // X swaps primary ↔ secondary color (Paint's idiom). Not in the
    // tool map because it's a one-shot action, not a mode-switch.
    if (e.key === 'x' || e.key === 'X') {
      e.preventDefault();
      session.swapColors();
    }
    // F = fit to content (Miro / Figma's "I got lost, take me home").
    // stopImmediatePropagation so the playlist's F=Fullscreen
    // window handler doesn't also fire — when the whiteboard is up
    // F means fit-to-content, full stop.
    if (e.key === 'f' || e.key === 'F') {
      e.preventDefault();
      e.stopImmediatePropagation();
      fitToContent();
    }
    // 0 = reset zoom + center on source-doc rect.
    if (e.key === '0') {
      e.preventDefault();
      e.stopImmediatePropagation();
      const r = document.getElementById('aa-whiteboard-surface')?.getBoundingClientRect();
      if (r) session.resetView(r.width, r.height);
    }
    // + / - = zoom in / out around the viewport center.
    if (e.key === '+' || e.key === '=') {
      e.preventDefault();
      e.stopImmediatePropagation();
      zoomFromButton(1.2);
    }
    if (e.key === '-' || e.key === '_') {
      e.preventDefault();
      e.stopImmediatePropagation();
      zoomFromButton(1 / 1.2);
    }
  }
</script>

<svelte:window onkeydown={handleKey} />

<!-- Surface — BrushCanvas now paints its own background (the
     `canvasColor` from session.doc) directly on the canvas. Outer
     wrapper just provides positioning + holds the floating chrome
     (exit pill, zoom controls, minimap). The id is what the F /
     zoom helpers use to anchor on this exact surface vs the
     viewport. -->
<div id="aa-whiteboard-surface" class="absolute inset-0">
  <BrushCanvas {session} infinite />

  <!-- Floating exit pill — top-right of the canvas, mirrors the
       AssetViewer's window-controls placement so users find it by
       muscle memory. Also gives a discoverable affordance beyond
       ESC + the Tools menu toggle. -->
  <button
    type="button"
    onclick={onClose}
    class="absolute right-3 top-3 z-30 inline-flex h-8 items-center gap-1.5 rounded-full bg-black/80 px-3 text-xs font-medium text-white shadow-lg hover:bg-black"
    title="Exit whiteboard (ESC)"
    aria-label="Exit whiteboard"
  >
    <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <line x1="18" y1="6" x2="6" y2="18"/>
      <line x1="6" y1="6" x2="18" y2="18"/>
    </svg>
    Exit whiteboard
  </button>

  <!-- Zoom controls — bottom-right, Miro / Figma corner pattern.
       Shows the live zoom % so users can dial in to "100% = export
       size" without guessing. The "Fit" button (or F key) bails
       them out if they pan off-canvas. -->
  <div class="absolute bottom-3 right-3 z-30 inline-flex h-8 items-stretch overflow-hidden rounded-full border border-black/20 bg-black/80 text-xs text-white shadow-lg">
    <button
      type="button"
      onclick={() => zoomFromButton(1 / 1.2)}
      class="px-3 font-mono hover:bg-black"
      title="Zoom out (−)"
      aria-label="Zoom out"
    >−</button>
    <button
      type="button"
      onclick={() => zoomFromButton(1.2)}
      class="border-x border-white/15 px-3 font-mono hover:bg-black"
      title="Zoom in (+)"
      aria-label="Zoom in"
    >+</button>
    <span
      class="inline-flex w-14 items-center justify-center font-mono"
      title={`Current zoom (${Math.round(session.viewZoom * 100)}%) — 0 to reset, F to fit`}
    >{Math.round(session.viewZoom * 100)}%</span>
    <button
      type="button"
      onclick={fitToContent}
      class="border-l border-white/15 px-3 text-[11px] uppercase tracking-wide hover:bg-black"
      title="Fit to content (F)"
      aria-label="Fit to content"
    >Fit</button>
  </div>

  <!-- Minimap — top-right under the exit pill, gives spatial
       reference at any zoom level + click-to-jump navigation.
       Renders a downsampled view of every item plus the viewport
       rectangle. Cached: re-renders only when the doc changes,
       not on pan/zoom (the viewport rectangle reads view state
       reactively via CSS positioning). -->
  <WhiteboardMinimap {session} />
</div>
