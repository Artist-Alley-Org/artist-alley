<script lang="ts">
  // Ebook reader Tips — chapter nav + reader-specific shortcuts.
  import type { ToolContext } from '../contract';
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { ctx: _ctx }: { ctx: ToolContext } = $props();
</script>

<dt class="col-span-2 mt-1 text-fg-muted/70">Reader</dt>
<dt class="font-mono text-fg">← · →</dt><dd class="text-fg-muted">Previous · next chapter</dd>
<dt class="font-mono text-fg">Click TOC</dt><dd class="text-fg-muted">Jump to any chapter</dd>
<dt class="font-mono text-fg">Search hit</dt><dd class="text-fg-muted">Click jumps to that chapter</dd>
<dt class="font-mono text-fg">Bookmark +</dt><dd class="text-fg-muted">Save current chapter with optional note</dd>
