<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // TipsSection — the global standard footer for any AssetViewer
  // tool. Pinned at the bottom of the side-panel scroll area; opens
  // / closes via the native <details>. Tools register a Tips snippet
  // on their ToolDef and the shell renders it through this primitive
  // so every tool's shortcuts look identical.
  //
  // The body is a <dl> the caller fills with <dt>/<dd> pairs. We
  // expose <dt class="font-mono text-fg"> and <dd class="text-fg-
  // muted"> conventions in our snippets; the styled <dl> + grid
  // layout lives here so per-tool snippets stay focused on content
  // not chrome. Section dividers inside the dl: `<dt class="col-
  // span-2 mt-1 text-fg-muted/70">Heading</dt>`.
  //
  // The defaultOpen flag is omitted on purpose — the shell decides
  // whether the footer opens on first load by binding `open` if it
  // wants. Stay closed by default so the body section above stays
  // tall.

  import type { Snippet } from 'svelte';

  let { children, title = 'Tips & shortcuts' }: {
    children?: Snippet;
    /** Override the summary text when a tool wants a different
     *  header (e.g. "Whiteboard shortcuts"). Defaults to the
     *  generic global heading. */
    title?: string;
  } = $props();
</script>

<section class="border-t border-border bg-surface p-3 text-xs">
  <details class="rounded border border-border bg-surface-elevated">
    <summary class="cursor-pointer px-2 py-1 text-[10px] font-medium uppercase tracking-wider text-fg-muted hover:text-fg">
      {title}
    </summary>
    <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 px-2 pb-2 pt-1 text-[10px]">
      {#if children}{@render children()}{/if}
    </dl>
  </details>
</section>
