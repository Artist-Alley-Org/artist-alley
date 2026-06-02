<script lang="ts">
  // Adapter Body for host-injected snippet tools. Looks up the
  // host-provided snippet under the conventional hostHook key
  // (see snippetToolHookKey in contract.ts) and renders it. Lets
  // a host register a tool whose body lives as a snippet in its
  // own scope — no need to extract host-local state into a
  // standalone Body component.

  import type { ToolContext, SnippetToolHooks } from './contract';
  import { snippetToolHookKey } from './contract';

  let { ctx }: { ctx: ToolContext } = $props();

  const hooks = $derived<SnippetToolHooks | undefined>(
    ctx.activeToolId
      ? (ctx.hostHooks?.[snippetToolHookKey(ctx.activeToolId)] as SnippetToolHooks | undefined)
      : undefined,
  );
</script>

{#if hooks?.body}
  {@render hooks.body(ctx)}
{/if}
