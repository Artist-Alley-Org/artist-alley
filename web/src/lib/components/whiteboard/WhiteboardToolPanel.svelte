<script lang="ts">
  // WhiteboardToolPanel — the full toolbox, rendered into the
  // viewer's right pane (via PostHost's metadataSlot snippet swap).
  //
  // Sections, top-to-bottom:
  //   1. Tool picker   (4 brush + 4 shape + text)
  //   2. Color palette (8 swatches, click to set)
  //   3. Size slider   (continuous 1–48 px; brush + shape stroke width)
  //   4. Opacity       (continuous 0–1; tool-defaults itself when
  //                    tool changes, user can override)
  //   5. Shape fill toggle (when a shape tool is active)
  //   6. Layers panel  (add / delete / rename / reorder / visibility
  //                    / opacity / lock; active-layer highlight)
  //   7. Save / Cancel
  //
  // Everything binds straight to the session store — there's no
  // local state we have to forward back to the canvas.

  import type { Tool } from '$lib/whiteboard/types';
  import {
    PALETTE,
    SIZES,
    isBrushTool,
    isShapeTool,
  } from '$lib/whiteboard/types';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';

  interface Props {
    session: WhiteboardSession;
    saving?: boolean;
    saveError?: string | null;
    onSave: () => void;
    onClose: () => void;
  }

  let { session, saving = false, saveError = null, onSave, onClose }: Props = $props();

  const TOOLS: Array<{ id: Tool; label: string; icon: string }> = [
    // Brushes
    { id: 'pen',         label: 'Pen (P)',         icon: 'M14 4l6 6-10 10H4v-6z' },
    { id: 'marker',      label: 'Marker (M)',      icon: 'M16 2l6 6-12 12-4-4z' },
    { id: 'highlighter', label: 'Highlighter (H)', icon: 'M9 11l-6 6v4h4l6-6 M14 4l6 6-7 7-6-6z' },
    { id: 'eraser',      label: 'Eraser (E)',      icon: 'M3 19h18 M18 13L10 5l-7 7 6 6h8z' },
    // Shapes
    { id: 'line',    label: 'Line (L)',    icon: 'M5 19L19 5' },
    { id: 'arrow',   label: 'Arrow (A)',   icon: 'M5 19L19 5 M19 5h-6 M19 5v6' },
    { id: 'rect',    label: 'Rectangle (R)', icon: 'M4 6h16v12H4z' },
    { id: 'ellipse', label: 'Ellipse (O)', icon: 'M12 6c5 0 8 2.5 8 6s-3 6-8 6-8-2.5-8-6 3-6 8-6z' },
    // Text
    { id: 'text',    label: 'Text (T)',    icon: 'M5 5h14 M12 5v14' },
  ];

  // Currently editing the name of which layer? Keyed by layer id.
  let editingLayerId = $state<string | null>(null);
  let editingLayerName = $state('');

  function startRename(id: string, current: string) {
    editingLayerId = id;
    editingLayerName = current;
  }
  function commitRename() {
    if (editingLayerId) session.renameLayer(editingLayerId, editingLayerName.trim() || 'Untitled');
    editingLayerId = null;
  }
</script>

<div class="flex h-full min-h-0 flex-col text-fg">
  <!-- Header -->
  <header class="flex shrink-0 items-center justify-between border-b border-border bg-surface-elevated px-3 py-2">
    <span class="text-sm font-semibold">Whiteboard</span>
    <button
      type="button"
      onclick={onClose}
      class="inline-flex h-6 w-6 items-center justify-center rounded text-fg-muted hover:bg-danger hover:text-white"
      title="Exit whiteboard (ESC)"
      aria-label="Exit whiteboard"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
    </button>
  </header>

  <div class="min-h-0 flex-1 overflow-y-auto">
    <!-- ── Tools ─────────────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      <div class="mb-2 text-[11px] font-medium uppercase tracking-wide text-fg-muted/80">Tools</div>
      <div class="grid grid-cols-5 gap-1">
        {#each TOOLS as t (t.id)}
          <button
            type="button"
            onclick={() => (session.tool = t.id)}
            class="inline-flex aspect-square items-center justify-center rounded transition-colors"
            class:bg-accent={session.tool === t.id}
            class:text-on-accent={session.tool === t.id}
            class:text-fg-muted={session.tool !== t.id}
            class:hover:bg-state-hover={session.tool !== t.id}
            title={t.label}
            aria-label={t.label}
            aria-pressed={session.tool === t.id}
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d={t.icon} />
            </svg>
          </button>
        {/each}
      </div>
    </section>

    <!-- ── Color ────────────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      <div class="mb-2 text-[11px] font-medium uppercase tracking-wide text-fg-muted/80">Color</div>
      <div class="flex flex-wrap items-center gap-1.5">
        {#each PALETTE as c (c)}
          <button
            type="button"
            onclick={() => (session.color = c)}
            class="h-6 w-6 rounded-full ring-1 ring-border transition-transform hover:scale-110"
            class:ring-2={session.color === c}
            class:ring-accent={session.color === c}
            class:scale-110={session.color === c}
            style:background-color={c}
            title={c}
            aria-label={`Color ${c}`}
            aria-pressed={session.color === c}
          ></button>
        {/each}
        <!-- Native color picker for arbitrary hues. Reuses the OS
             picker — no custom HSL UI in C-1.5; that's C-1.7. -->
        <input
          type="color"
          value={session.color}
          oninput={(e) => (session.color = (e.currentTarget as HTMLInputElement).value)}
          class="h-6 w-6 cursor-pointer rounded border border-border bg-transparent p-0"
          title="Custom color"
          aria-label="Custom color"
        />
      </div>
    </section>

    <!-- ── Size + Opacity ──────────────────────────────────────── -->
    <section class="space-y-3 border-b border-border p-3">
      <div>
        <div class="mb-1 flex items-center justify-between text-[11px] font-medium uppercase tracking-wide text-fg-muted/80">
          <span>Size</span>
          <span class="font-mono text-fg">{session.width}px</span>
        </div>
        <input
          type="range"
          min={1} max={48} step={1}
          value={session.width}
          oninput={(e) => (session.width = +(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
          aria-label="Brush size"
        />
        <div class="mt-1 flex justify-between gap-1">
          {#each SIZES as s (s.label)}
            <button
              type="button"
              onclick={() => (session.width = s.width)}
              class="flex-1 rounded border border-border px-1 py-0.5 text-[10px] hover:border-fg-muted/60"
              class:border-accent={session.width === s.width}
              class:text-accent={session.width === s.width}
              title={`${s.label} (${s.width}px)`}
            >{s.label}</button>
          {/each}
        </div>
      </div>
      <div>
        <div class="mb-1 flex items-center justify-between text-[11px] font-medium uppercase tracking-wide text-fg-muted/80">
          <span>Opacity</span>
          <span class="font-mono text-fg">{Math.round(session.opacity * 100)}%</span>
        </div>
        <input
          type="range"
          min={0} max={1} step={0.05}
          value={session.opacity}
          oninput={(e) => (session.opacity = +(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
          aria-label="Opacity"
        />
      </div>
      {#if isShapeTool(session.tool)}
        <label class="flex cursor-pointer items-center justify-between text-xs">
          <span>Fill shape</span>
          <input
            type="checkbox"
            checked={session.fillShapes}
            onchange={(e) => (session.fillShapes = (e.currentTarget as HTMLInputElement).checked)}
            class="accent-accent"
          />
        </label>
      {/if}
    </section>

    <!-- ── Layers ──────────────────────────────────────────────── -->
    <section class="border-b border-border p-3">
      <div class="mb-2 flex items-center justify-between text-[11px] font-medium uppercase tracking-wide text-fg-muted/80">
        <span>Layers</span>
        <button
          type="button"
          onclick={() => session.addLayer()}
          class="inline-flex h-5 w-5 items-center justify-center rounded text-fg-muted hover:bg-state-hover hover:text-fg"
          title="Add layer"
          aria-label="Add layer"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
        </button>
      </div>
      <!-- Render newest-on-top to match Photoshop / Procreate's
           layer panel orientation (rendering order is reversed). -->
      <ul class="space-y-1">
        {#each [...session.doc.layers].reverse() as layer (layer.id)}
          {@const isActive = session.activeLayerId === layer.id}
          <li
            class="group rounded border p-1.5 transition-colors"
            class:border-accent={isActive}
            class:bg-state-selected={isActive}
            class:border-border={!isActive}
            class:hover:bg-state-hover={!isActive}
          >
            <div class="flex items-center gap-1.5">
              <!-- Visibility toggle -->
              <button
                type="button"
                onclick={() => session.setLayerVisible(layer.id, !layer.visible)}
                class="inline-flex h-5 w-5 shrink-0 items-center justify-center rounded text-fg-muted hover:text-fg"
                title={layer.visible ? 'Hide layer' : 'Show layer'}
                aria-pressed={layer.visible}
              >
                {#if layer.visible}
                  <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>
                {:else}
                  <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/></svg>
                {/if}
              </button>
              <!-- Name (click to activate; dbl-click to rename) -->
              {#if editingLayerId === layer.id}
                <input
                  type="text"
                  bind:value={editingLayerName}
                  onblur={commitRename}
                  onkeydown={(e) => { if (e.key === 'Enter') commitRename(); if (e.key === 'Escape') editingLayerId = null; }}
                  class="min-w-0 flex-1 rounded border border-border bg-surface px-1 text-xs"
                  autofocus
                />
              {:else}
                <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
                <span
                  onclick={() => (session.activeLayerId = layer.id)}
                  ondblclick={() => startRename(layer.id, layer.name ?? '')}
                  class="min-w-0 flex-1 cursor-pointer truncate text-xs"
                  title="Click to activate · double-click to rename"
                >
                  {layer.name || 'Untitled'}
                </span>
              {/if}
              <!-- Reorder -->
              <button
                type="button"
                onclick={() => session.moveLayer(layer.id, 'up')}
                class="opacity-0 group-hover:opacity-100 text-fg-muted hover:text-fg"
                title="Move layer up"
                aria-label="Move layer up"
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="18 15 12 9 6 15"/></svg>
              </button>
              <button
                type="button"
                onclick={() => session.moveLayer(layer.id, 'down')}
                class="opacity-0 group-hover:opacity-100 text-fg-muted hover:text-fg"
                title="Move layer down"
                aria-label="Move layer down"
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
              </button>
              <!-- Lock -->
              <button
                type="button"
                onclick={() => session.setLayerLocked(layer.id, !layer.locked)}
                class="text-fg-muted hover:text-fg"
                class:text-warning={layer.locked}
                title={layer.locked ? 'Unlock layer' : 'Lock layer'}
                aria-pressed={layer.locked}
              >
                {#if layer.locked}
                  <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
                {:else}
                  <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 9.9-1"/></svg>
                {/if}
              </button>
              <!-- Delete -->
              {#if session.doc.layers.length > 1}
                <button
                  type="button"
                  onclick={() => session.removeLayer(layer.id)}
                  class="opacity-0 group-hover:opacity-100 text-fg-muted hover:text-danger"
                  title="Delete layer"
                  aria-label="Delete layer"
                >
                  <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/></svg>
                </button>
              {/if}
            </div>
            <!-- Per-layer opacity (compact slider; only shown for the
                 active layer to keep the panel tight). -->
            {#if isActive}
              <div class="mt-1.5 flex items-center gap-2">
                <span class="text-[10px] text-fg-muted">α</span>
                <input
                  type="range"
                  min={0} max={1} step={0.05}
                  value={layer.opacity}
                  oninput={(e) => session.setLayerOpacity(layer.id, +(e.currentTarget as HTMLInputElement).value)}
                  class="min-w-0 flex-1 accent-accent"
                  aria-label="Layer opacity"
                />
                <span class="w-8 text-right font-mono text-[10px] text-fg-muted">{Math.round(layer.opacity * 100)}%</span>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    </section>

    <!-- ── History + Clear ─────────────────────────────────────── -->
    <section class="flex items-center justify-between border-b border-border p-3">
      <div class="flex gap-1">
        <button
          type="button"
          onclick={() => session.undo()}
          disabled={!session.canUndo}
          class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-state-hover disabled:opacity-30 disabled:hover:bg-transparent"
          title="Undo (Ctrl/⌘+Z)"
          aria-label="Undo"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 7 3 13 9 13"/><path d="M3 13a9 9 0 1 0 3-7"/></svg>
        </button>
        <button
          type="button"
          onclick={() => session.redo()}
          disabled={!session.canRedo}
          class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-state-hover disabled:opacity-30 disabled:hover:bg-transparent"
          title="Redo (Ctrl/⌘+Shift+Z)"
          aria-label="Redo"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="21 7 21 13 15 13"/><path d="M21 13a9 9 0 1 1 -3-7"/></svg>
        </button>
      </div>
      <button
        type="button"
        onclick={() => { if (confirm('Clear all strokes on this whiteboard?')) session.clearAll(); }}
        class="inline-flex h-7 items-center rounded px-2 text-xs text-fg-muted hover:bg-state-hover"
        title="Clear all strokes"
      >
        Clear all
      </button>
    </section>

    <!-- ── Hints ────────────────────────────────────────────────── -->
    <section class="p-3 text-[10px] text-fg-muted">
      <div class="mb-1 font-medium uppercase tracking-wide text-fg-muted/80">Tips</div>
      <ul class="space-y-0.5">
        <li>Paste images / text directly (Ctrl/⌘+V)</li>
        <li>Shift while dragging a shape constrains it</li>
        <li>p / m / h / e / l / a / r / o / t — tool quick-keys</li>
        <li>Ctrl/⌘+Z undo · +Shift redo</li>
      </ul>
    </section>
  </div>

  <!-- ── Save / cancel — sticky footer ───────────────────────── -->
  <footer class="flex shrink-0 items-center gap-2 border-t border-border bg-surface-elevated px-3 py-2">
    <span class="flex-1 truncate text-xs text-danger">{saveError ?? ''}</span>
    <button
      type="button"
      onclick={onClose}
      class="inline-flex h-7 items-center rounded border border-border px-3 text-xs text-fg hover:bg-state-hover"
    >
      Cancel
    </button>
    <button
      type="button"
      onclick={onSave}
      disabled={saving}
      class="inline-flex h-7 items-center rounded bg-accent px-3 text-xs font-medium text-on-accent transition-colors hover:opacity-90 disabled:opacity-50"
    >
      {saving ? 'Saving…' : 'Save'}
    </button>
  </footer>
</div>
