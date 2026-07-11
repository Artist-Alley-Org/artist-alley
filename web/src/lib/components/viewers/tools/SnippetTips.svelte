<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Mirror of SnippetBody but for the optional Tips slot.
  import type { ToolContext, SnippetToolHooks } from './contract';
  import { snippetToolHookKey } from './contract';

  let { ctx }: { ctx: ToolContext } = $props();

  const hooks = $derived<SnippetToolHooks | undefined>(
    ctx.activeToolId
      ? (ctx.hostHooks?.[snippetToolHookKey(ctx.activeToolId)] as SnippetToolHooks | undefined)
      : undefined,
  );
</script>

{#if hooks?.tips}
  {@render hooks.tips(ctx)}
{/if}
