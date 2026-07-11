<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // 3D Viewer Tips — shortcuts for the most common reach.
  import type { ToolContext } from '../contract';
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { ctx: _ctx }: { ctx: ToolContext } = $props();
</script>

<dt class="col-span-2 mt-1 text-fg-muted/70">3D Viewer</dt>
<dt class="font-mono text-fg">Drag</dt><dd class="text-fg-muted">Orbit around model</dd>
<dt class="font-mono text-fg">Right-drag</dt><dd class="text-fg-muted">Pan camera</dd>
<dt class="font-mono text-fg">Wheel</dt><dd class="text-fg-muted">Dolly in / out</dd>
<dt class="font-mono text-fg">Frame all</dt><dd class="text-fg-muted">Re-fit the model to view</dd>
<dt class="font-mono text-fg">Save view</dt><dd class="text-fg-muted">Persists per-asset across reloads</dd>
