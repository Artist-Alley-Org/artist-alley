<script lang="ts">
  import type { ToolContext } from '../contract';
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { ctx: _ctx }: { ctx: ToolContext } = $props();
</script>

<dt class="col-span-2 mt-1 text-fg-muted/70">Archive</dt>
<dt class="font-mono text-fg">Click folder</dt><dd class="text-fg-muted">Expand / collapse</dd>
<dt class="font-mono text-fg">Click file</dt><dd class="text-fg-muted">Preview inline (text/image) or download (binary)</dd>
<dt class="font-mono text-fg">Filter box</dt><dd class="text-fg-muted">Path substring; case-insensitive</dd>
<dt class="font-mono text-fg">Download</dt><dd class="text-fg-muted">Streams one entry — never the whole archive</dd>
