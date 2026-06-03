<script lang="ts">
  import type { ToolContext } from '../contract';
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { ctx: _ctx }: { ctx: ToolContext } = $props();
</script>

<dt class="col-span-2 mt-1 text-fg-muted/70">Audiobook</dt>
<dt class="font-mono text-fg">Space</dt><dd class="text-fg-muted">Play / pause</dd>
<dt class="font-mono text-fg">,&nbsp;·&nbsp;.</dt><dd class="text-fg-muted">Skip back / forward (configurable)</dd>
<dt class="font-mono text-fg">1–5</dt><dd class="text-fg-muted">Speed presets (0.25× → 4×)</dd>
<dt class="font-mono text-fg">B</dt><dd class="text-fg-muted">Bookmark at current position</dd>
<dt class="font-mono text-fg">Auto-rewind</dt><dd class="text-fg-muted">Resume jumps back 5s by default</dd>
