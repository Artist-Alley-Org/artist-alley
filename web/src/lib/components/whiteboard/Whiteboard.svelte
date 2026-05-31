<script lang="ts">
  // Whiteboard — the post-anchored brush surface.
  //
  // Renders a full-canvas overlay on top of the AssetViewer. The host
  // (PostHost) toggles us on, hands us the postId + an onClose
  // callback, and we own the rest:
  //
  //   - BrushCanvas primitive does the painting
  //   - A toolbar over the canvas: tool picker, color palette, size
  //     presets, undo / redo, clear, save, close
  //   - On Save: POST /posts/{id}/whiteboards with the vector doc;
  //     fires onSaved so the host's sidebar list can refresh
  //
  // The shell's keyboard nav (arrow keys, ESC) is silently swallowed
  // while we're up — drawing must not also flip posts or close the
  // modal accidentally.
  //
  // C-1 ships pen / marker / highlighter / eraser + 8-color palette +
  // 4-size presets + undo/redo + clear + save + close. The schema +
  // BrushCanvas primitive already support layers, shapes, text, and
  // per-stroke opacity; the toolbar surfaces that polish in a later
  // commit (C-1.5).

  import { onMount, onDestroy } from 'svelte';
  import { api } from '$api/client';
  import type { BrushContent, Tool } from '$lib/whiteboard/types';
  import { PALETTE, SIZES, defaultOpacityFor, emptyDoc } from '$lib/whiteboard/types';
  import BrushCanvas from './BrushCanvas.svelte';

  interface Props {
    postId: string;
    /** Called when the user clicks × / hits ESC. */
    onClose: () => void;
    /** Called after a successful save — host refreshes its sidebar
        list and stays in whiteboard mode (so the user can save again
        without re-opening). */
    onSaved?: () => void;
  }

  let { postId, onClose, onSaved }: Props = $props();

  // Source canvas dimensions: we pick a 1920 × 1080 logical surface
  // regardless of the viewport — gives consistent stroke widths
  // across devices and saves cleanly to other viewers. The DPR-aware
  // resize in BrushCanvas keeps it crisp.
  const SOURCE_W = 1920;
  const SOURCE_H = 1080;

  let doc = $state<BrushContent>(emptyDoc(SOURCE_W, SOURCE_H));
  let tool = $state<Tool>('pen');
  let color = $state<string>(PALETTE[7]); // near-black default
  let sizeIdx = $state(1); // 'M'
  let activeLayer = $state(0);
  let canvasRef: BrushCanvas | undefined = $state();
  let dialogEl: HTMLDialogElement | undefined = $state();

  onMount(() => {
    // showModal puts the dialog in the browser's top layer so it
    // stacks above the AssetPlaylist dialog underneath without us
    // having to wrangle z-index against the dialog top-layer rules.
    dialogEl?.showModal();
    document.body.classList.add('overflow-hidden');
  });
  onDestroy(() => {
    document.body.classList.remove('overflow-hidden');
    if (dialogEl?.open) dialogEl.close();
  });

  const width = $derived(SIZES[sizeIdx].width);
  const opacity = $derived(defaultOpacityFor(tool));

  let saving = $state(false);
  let saveError = $state<string | null>(null);

  // ── Tool palette config ───────────────────────────────────────────
  // Tool catalogue rendered in the toolbar. Icon strokes are inline
  // SVG paths (so we don't pull a whole icon lib in for four glyphs).
  const TOOLS: Array<{ id: Tool; label: string; path: string }> = [
    {
      id: 'pen',
      label: 'Pen',
      // Pen tip outline
      path: 'M14 4l6 6-10 10H4v-6z M14 4l3-3 6 6-3 3',
    },
    {
      id: 'marker',
      label: 'Marker',
      // Chisel marker
      path: 'M16 2l6 6-12 12-4-4z M3 21l4-2-2-2z',
    },
    {
      id: 'highlighter',
      label: 'Highlighter',
      path: 'M9 11l-6 6v4h4l6-6 M14 4l6 6-7 7-6-6z',
    },
    {
      id: 'eraser',
      label: 'Eraser',
      path: 'M3 19h18 M18 13L10 5l-7 7 6 6h8z',
    },
  ];

  function pickTool(t: Tool) { tool = t; }
  function pickColor(c: string) {
    color = c;
    if (tool === 'eraser') tool = 'pen';
  }
  function pickSize(i: number) { sizeIdx = i; }

  function handleUndo() { canvasRef?.undo(); }
  function handleRedo() { canvasRef?.redo(); }
  function handleClear() {
    if (!confirm('Clear all strokes on this whiteboard?')) return;
    canvasRef?.clearAll();
  }

  async function handleSave() {
    if (saving) return;
    // Skip empty saves — every layer's strokes are empty means the
    // user opened then closed without drawing anything.
    const empty = doc.layers.every((l) => l.strokes.length === 0);
    if (empty) {
      saveError = "Draw something first.";
      return;
    }
    saving = true;
    saveError = null;
    try {
      const { error } = await api.POST('/posts/{id}/whiteboards', {
        params: { path: { id: postId } },
        body: {
          // No title input yet; sidebar shows "Untitled sketch" + author + time.
          content: {
            source_w: doc.source_w,
            source_h: doc.source_h,
            layers: doc.layers.map((l) => ({
              id: l.id,
              name: l.name,
              visible: l.visible,
              opacity: l.opacity,
              strokes: l.strokes.map((s) => ({
                tool: s.tool,
                color: s.color,
                width: s.width,
                opacity: s.opacity,
                text: s.text,
                points: s.points.map((p) => [...p]),
              })),
            })),
          },
        } as unknown as never, // openapi-typescript types the deep stroke arrays loosely; the runtime shape is exactly what the server validates
      });
      if (error) {
        saveError = (error as { error?: string } | undefined)?.error ?? 'Save failed.';
        return;
      }
      onSaved?.();
      // Reset the doc so the user can immediately start another one
      // without leaving whiteboard mode.
      doc = emptyDoc(SOURCE_W, SOURCE_H);
    } catch (e) {
      saveError = e instanceof Error ? e.message : 'Save failed.';
    } finally {
      saving = false;
    }
  }

  function handleKey(e: KeyboardEvent) {
    // Ignore key handling while focus is in an input.
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;

    // ESC closes (after a confirm if the user has unsaved strokes —
    // simplest "don't-lose-work" guard).
    if (e.key === 'Escape') {
      e.preventDefault();
      const hasWork = doc.layers.some((l) => l.strokes.length > 0);
      if (!hasWork || confirm('Discard this whiteboard?')) {
        onClose();
      }
      return;
    }

    // Undo / Redo
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z') {
      e.preventDefault();
      if (e.shiftKey) handleRedo(); else handleUndo();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'y') {
      e.preventDefault();
      handleRedo();
      return;
    }

    // Tool quick-keys (single-letter — common in DCC sketch tools)
    if (e.key === 'p' || e.key === 'P') { tool = 'pen'; e.preventDefault(); }
    if (e.key === 'm' || e.key === 'M') { tool = 'marker'; e.preventDefault(); }
    if (e.key === 'h' || e.key === 'H') { tool = 'highlighter'; e.preventDefault(); }
    if (e.key === 'e' || e.key === 'E') { tool = 'eraser'; e.preventDefault(); }
  }
</script>

<svelte:window onkeydown={handleKey} />

<!-- Native <dialog> opened with showModal() so the browser puts us in
     the top layer above the AssetPlaylist dialog underneath — no
     z-index tug-of-war. The host (PostHost) decides when to mount
     us; we open / close the dialog element via onMount / onDestroy
     plus the explicit close button. -->
<dialog
  bind:this={dialogEl}
  class="m-0 h-screen w-screen max-w-none max-h-none flex-col border-none bg-surface/95 p-0 text-fg backdrop:bg-black/70 backdrop:backdrop-blur-sm"
  style="display: flex;"
  aria-label="Whiteboard"
  onclose={onClose}
>
  <!-- Toolbar — single row across the top of the overlay. Mirrors
       the rest of the viewer's chrome density (h-9, text-xs). -->
  <div
    class="flex h-10 shrink-0 items-center gap-1 border-b border-border bg-surface-elevated px-2 text-xs text-fg"
  >
    <!-- Tool picker -->
    <div class="flex items-center gap-0.5 pr-2">
      {#each TOOLS as t (t.id)}
        <button
          type="button"
          onclick={() => pickTool(t.id)}
          class="inline-flex h-7 w-7 items-center justify-center rounded transition-colors"
          class:bg-accent={tool === t.id}
          class:text-on-accent={tool === t.id}
          class:text-fg-muted={tool !== t.id}
          class:hover:bg-state-hover={tool !== t.id}
          title={t.label}
          aria-label={t.label}
          aria-pressed={tool === t.id}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d={t.path} />
          </svg>
        </button>
      {/each}
    </div>

    <span class="h-5 w-px bg-border"></span>

    <!-- Color palette -->
    <div class="flex items-center gap-1 px-2">
      {#each PALETTE as c (c)}
        <button
          type="button"
          onclick={() => pickColor(c)}
          class="inline-block h-5 w-5 rounded-full ring-1 ring-border transition-transform"
          class:ring-2={color === c}
          class:ring-accent={color === c}
          class:scale-110={color === c}
          style:background-color={c}
          title={c}
          aria-label={`Color ${c}`}
          aria-pressed={color === c}
        ></button>
      {/each}
    </div>

    <span class="h-5 w-px bg-border"></span>

    <!-- Size presets -->
    <div class="flex items-center gap-0.5 px-2">
      {#each SIZES as s, i (s.label)}
        <button
          type="button"
          onclick={() => pickSize(i)}
          class="inline-flex h-7 w-7 items-center justify-center rounded transition-colors"
          class:bg-accent={sizeIdx === i}
          class:text-on-accent={sizeIdx === i}
          class:hover:bg-state-hover={sizeIdx !== i}
          title={`Size ${s.label} (${s.width}px)`}
          aria-label={`Size ${s.label}`}
          aria-pressed={sizeIdx === i}
        >
          <span
            class="block rounded-full"
            class:bg-on-accent={sizeIdx === i}
            class:bg-fg={sizeIdx !== i}
            style:width={`${Math.min(16, s.width / 2)}px`}
            style:height={`${Math.min(16, s.width / 2)}px`}
          ></span>
        </button>
      {/each}
    </div>

    <span class="h-5 w-px bg-border"></span>

    <!-- Undo / Redo / Clear -->
    <button
      type="button"
      onclick={handleUndo}
      disabled={!canvasRef?.canUndo()}
      class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-state-hover disabled:opacity-30 disabled:hover:bg-transparent"
      title="Undo (Ctrl/⌘+Z)"
      aria-label="Undo"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 7 3 13 9 13"/><path d="M3 13a9 9 0 1 0 3-7"/></svg>
    </button>
    <button
      type="button"
      onclick={handleRedo}
      disabled={!canvasRef?.canRedo()}
      class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-state-hover disabled:opacity-30 disabled:hover:bg-transparent"
      title="Redo (Ctrl/⌘+Shift+Z)"
      aria-label="Redo"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="21 7 21 13 15 13"/><path d="M21 13a9 9 0 1 1 -3-7"/></svg>
    </button>
    <button
      type="button"
      onclick={handleClear}
      class="inline-flex h-7 items-center rounded px-2 text-xs text-fg-muted hover:bg-state-hover"
      title="Clear all strokes"
    >
      Clear
    </button>

    <!-- Save error message + spacer + Save / Close on the right -->
    <span class="ml-2 flex-1 truncate text-xs text-danger">{saveError ?? ''}</span>

    <button
      type="button"
      onclick={handleSave}
      disabled={saving}
      class="inline-flex h-7 items-center rounded bg-accent px-3 text-xs font-medium text-on-accent transition-colors hover:opacity-90 disabled:opacity-50"
      title="Save whiteboard (becomes a sketch on this post)"
    >
      {saving ? 'Saving…' : 'Save'}
    </button>
    <button
      type="button"
      onclick={onClose}
      class="ml-1 inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-danger hover:text-white"
      title="Close whiteboard (ESC)"
      aria-label="Close whiteboard"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
    </button>
  </div>

  <!-- Drawing surface. Hardcoded white background regardless of
       theme so the 8-color palette reads at full saturation — a near-
       black default on a dark surface is invisible. Inline style
       instead of Tailwind to avoid theme override surprises.
       Annotations later will paint over the asset itself — they pass
       a backgroundUrl to BrushCanvas instead. -->
  <div class="relative min-h-0 flex-1" style="background-color: #ffffff;">
    <BrushCanvas
      bind:this={canvasRef}
      bind:value={doc}
      bind:tool
      bind:color
      bind:activeLayer
      width={width}
      opacity={opacity}
    />
  </div>
</dialog>
