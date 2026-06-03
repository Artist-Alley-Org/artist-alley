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
  // Selecting Whiteboard in the menubar fires onActivate (host
  // opens the canvas overlay + creates the session); the body
  // here shows a prompt until the session is up so the user
  // sees confirmation that something is happening.
  //
  // The `compact` flag now comes from ctx.shellState.paneCompact
  // — the shell owns the rail toggle (chevron in the panel
  // header), not the tool. WhiteboardToolPanel's in-tool toggle
  // was removed in the same change.
  type WhiteboardHostHooks = {
    saving?: boolean;
    saveError?: string | null;
    onSave?: () => void;
    onClose?: () => void;
    onActivate?: () => void;
  };

  const hooks = $derived(
    (ctx.hostHooks?.whiteboard as WhiteboardHostHooks | undefined),
  );
  const compact = $derived(!!ctx.shellState?.paneCompact);
</script>

{#if ctx.whiteboardSession && hooks?.onSave && hooks?.onClose}
  <WhiteboardToolPanel
    session={ctx.whiteboardSession}
    saving={hooks.saving ?? false}
    saveError={hooks.saveError ?? null}
    onSave={hooks.onSave}
    onClose={hooks.onClose}
    {compact}
  />
{:else}
  <!-- No session yet — render a prompt so the panel doesn't sit
       empty while the user waits for the overlay to spin up. The
       AssetViewer activates the whiteboard mode automatically on
       picker selection, so this state is only visible during the
       short delay between selection and overlay mount. -->
  <div class="p-4 text-sm text-fg-muted">
    <p>Opening whiteboard…</p>
  </div>
{/if}
