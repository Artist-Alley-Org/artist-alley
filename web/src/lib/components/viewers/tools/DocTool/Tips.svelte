<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Document Tips — reader / editor shortcuts.
  import type { ToolContext } from '../contract';
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { ctx: _ctx }: { ctx: ToolContext } = $props();
</script>

<dt class="col-span-2 mt-1 text-fg-muted/70">Document</dt>
<dt class="font-mono text-fg">Ctrl/⌘ F</dt><dd class="text-fg-muted">Find in document</dd>
<dt class="font-mono text-fg">Ctrl/⌘ G</dt><dd class="text-fg-muted">Next match</dd>
<dt class="font-mono text-fg">⇧ Ctrl/⌘ G</dt><dd class="text-fg-muted">Previous match</dd>
<dt class="font-mono text-fg">Click outline</dt><dd class="text-fg-muted">Jump to that heading / symbol</dd>
<dt class="font-mono text-fg">Click bookmark</dt><dd class="text-fg-muted">Jump to saved line</dd>
