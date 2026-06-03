<script lang="ts">
  // Whiteboard Tips — lifted from PostHost's whiteboardHotkeys
  // snippet (which is going away in the cutover commit). Splits
  // into Common / View / Tool-specific so the user can scan to
  // what they need.
  import type { ToolContext } from '../contract';

  let { ctx }: { ctx: ToolContext } = $props();
  const tool = $derived(ctx.whiteboardSession?.tool);
</script>

<dt class="font-mono text-fg">Ctrl/⌘ + Z</dt><dd class="text-fg-muted">Undo</dd>
<dt class="font-mono text-fg">Ctrl/⌘ + ⇧ + Z</dt><dd class="text-fg-muted">Redo</dd>
<dt class="font-mono text-fg">Ctrl/⌘ + V</dt><dd class="text-fg-muted">Paste image / text</dd>
<dt class="font-mono text-fg">X</dt><dd class="text-fg-muted">Swap Color 1 ↔ Color 2</dd>
<dt class="font-mono text-fg">Esc</dt><dd class="text-fg-muted">Exit whiteboard</dd>

<dt class="col-span-2 mt-1 text-fg-muted/70">View · pan / zoom</dt>
<dt class="font-mono text-fg">Ctrl/⌘ + wheel</dt><dd class="text-fg-muted">Zoom in / out at cursor</dd>
<dt class="font-mono text-fg">Wheel · ⇧ + wheel</dt><dd class="text-fg-muted">Pan vertically · horizontally</dd>
<dt class="font-mono text-fg">Middle-drag</dt><dd class="text-fg-muted">Pan</dd>
<dt class="font-mono text-fg">Space + drag</dt><dd class="text-fg-muted">Pan (hand tool)</dd>
<dt class="font-mono text-fg">+ · −</dt><dd class="text-fg-muted">Zoom in · out (viewport center)</dd>

{#if tool === 'pen' || tool === 'marker' || tool === 'highlighter' || tool === 'eraser'}
  <dt class="col-span-2 mt-1 text-fg-muted/70">{tool === 'eraser' ? 'Eraser' : 'Brush'}</dt>
  <dt class="font-mono text-fg">[ · ]</dt><dd class="text-fg-muted">Decrease · increase size</dd>
{/if}

{#if tool === 'text' || tool === 'sticky' || tool === 'label'}
  <dt class="col-span-2 mt-1 text-fg-muted/70">Text</dt>
  <dt class="font-mono text-fg">Click</dt><dd class="text-fg-muted">Start a new text block</dd>
  <dt class="font-mono text-fg">Esc</dt><dd class="text-fg-muted">Commit text</dd>
{/if}
