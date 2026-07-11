<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Tips for the Details tool. Details is the always-on default
  // tool, so its tips list the gestures the AssetViewer shell
  // owns globally (pan / zoom / fullscreen / reset) PLUS the
  // tile-mode toggle (only meaningful for image / texture kinds
  // but lives at the shell level so this tool is the right home).
  // PostHost's Details override registers this same Tips so the
  // gestures stay documented regardless of who owns the body.
  import type { ToolContext } from '../contract';

  let { ctx }: { ctx: ToolContext } = $props();
  const canTile = $derived(ctx.controller.kind === 'image');
</script>

<dt class="col-span-2 mt-1 text-fg-muted/70">View</dt>
<dt class="font-mono text-fg">Wheel</dt><dd class="text-fg-muted">Zoom in / out at cursor</dd>
<dt class="font-mono text-fg">Drag</dt><dd class="text-fg-muted">Pan when zoomed</dd>
<dt class="font-mono text-fg">R</dt><dd class="text-fg-muted">Reset zoom + pan</dd>
<dt class="font-mono text-fg">F</dt><dd class="text-fg-muted">Toggle fullscreen</dd>

{#if canTile}
  <dt class="col-span-2 mt-1 text-fg-muted/70">Texture</dt>
  <dt class="font-mono text-fg">T</dt><dd class="text-fg-muted">Toggle tile preview (repeats the image across the canvas — useful for seamless textures)</dd>
{/if}
