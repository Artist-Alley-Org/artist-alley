<script lang="ts">
  // WhiteboardTool body — adapts the existing WhiteboardToolPanel
  // into the registry-driven shell. The host (PostHost today) wires
  // save / close / compact hooks via ctx.hostHooks under the
  // `whiteboard` key — see WhiteboardHostHooks below for the shape.

  import WhiteboardToolPanel from '$lib/components/whiteboard/WhiteboardToolPanel.svelte';
  import type { ToolContext } from '../contract';

  let { ctx }: { ctx: ToolContext } = $props();

  // Strongly-typed view of the loose hostHooks bag — keeps every
  // .ts host that wires a whiteboard agreeing on the same names.
  type WhiteboardHostHooks = {
    saving?: boolean;
    saveError?: string | null;
    onSave: () => void;
    onClose: () => void;
    compact?: boolean;
    onToggleCompact?: () => void;
  };

  const hooks = $derived(
    (ctx.hostHooks?.whiteboard as WhiteboardHostHooks | undefined),
  );
</script>

{#if ctx.whiteboardSession && hooks}
  <WhiteboardToolPanel
    session={ctx.whiteboardSession}
    saving={hooks.saving ?? false}
    saveError={hooks.saveError ?? null}
    onSave={hooks.onSave}
    onClose={hooks.onClose}
    compact={hooks.compact ?? false}
    onToggleCompact={hooks.onToggleCompact}
  />
{/if}
