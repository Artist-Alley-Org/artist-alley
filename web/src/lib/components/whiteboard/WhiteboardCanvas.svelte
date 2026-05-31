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
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';

  interface Props {
    session: WhiteboardSession;
    /** Called when the user presses ESC or the close pill. */
    onClose: () => void;
  }

  let { session, onClose }: Props = $props();

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
      l: 'line', a: 'arrow', r: 'rect', o: 'ellipse',
      t: 'text',
      q: 'lasso', c: 'crop',
    };
    const next = map[e.key.toLowerCase()];
    if (next) {
      e.preventDefault();
      session.tool = next;
    }
  }
</script>

<svelte:window onkeydown={handleKey} />

<!-- White backdrop covers the asset surface so brush strokes read
     against a consistent canvas. The asset behind us still loads
     (so closing the whiteboard reveals it instantly) but is hidden
     while we're up. -->
<div class="absolute inset-0 bg-white">
  <BrushCanvas {session} />

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
</div>
