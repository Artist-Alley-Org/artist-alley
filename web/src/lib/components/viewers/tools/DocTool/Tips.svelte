<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Document Tips — reader / editor shortcuts.
  //
  // Ctrl/⌘ G and ⇧ Ctrl/⌘ G used to be listed here and were never
  // bound (#885). They are not listed now and not bound either,
  // because next / previous match is dead through every route — see
  // the note on handleFindKeys in Body.svelte.
  import type { ToolContext } from '../contract';
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { ctx: _ctx }: { ctx: ToolContext } = $props();
</script>

<dt class="col-span-2 mt-1 text-fg-muted/70">Document</dt>
<dt class="font-mono text-fg">Ctrl/⌘ F</dt><dd class="text-fg-muted">Jump to the Find box</dd>
<dt class="font-mono text-fg">Click outline</dt><dd class="text-fg-muted">Jump to that heading / symbol</dd>
<dt class="font-mono text-fg">Click bookmark</dt><dd class="text-fg-muted">Jump to saved line</dd>
