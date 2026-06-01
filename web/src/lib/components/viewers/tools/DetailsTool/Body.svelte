<script lang="ts">
  // DetailsTool body — the generic "what is this asset" surface.
  // Always available; falls back to a minimal info block when no
  // host has registered a richer details tool via customTools.
  //
  // Hosts that own richer details (PostHost has likes / comments /
  // cover-picker / tags) register their own DetailsTool with a
  // higher .order, which takes the dropdown slot. The default body
  // here is what shows on the standalone /assets/[id] route where
  // no host wired anything.

  import type { ToolContext } from '../contract';

  let { ctx }: { ctx: ToolContext } = $props();

  function formatBytes(n: number | undefined | null): string {
    if (!n || n < 0) return '—';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let v = n;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
  }
</script>

<section class="space-y-3 p-3 text-xs">
  <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
    <dt class="text-fg-muted">Title</dt>
    <dd class="truncate text-fg">{ctx.asset.title ?? 'Untitled'}</dd>
    <dt class="text-fg-muted">ID</dt>
    <dd class="truncate font-mono text-[10px] text-fg">{ctx.asset.id}</dd>
    {#if ctx.asset.file_extension}
      <dt class="text-fg-muted">Extension</dt>
      <dd class="font-mono text-fg">.{ctx.asset.file_extension}</dd>
    {/if}
    {#if ctx.asset.file_hash}
      <dt class="text-fg-muted">Hash</dt>
      <dd class="truncate font-mono text-[10px] text-fg" title={ctx.asset.file_hash}>
        {ctx.asset.file_hash.slice(0, 16)}…
      </dd>
    {/if}
  </dl>

  <div class="flex gap-2">
    <a
      href={`/api/v1/assets/${ctx.asset.id}/file`}
      download
      class="rounded border border-border bg-surface-elevated px-2 py-1 text-fg hover:border-accent"
    >Download original</a>
  </div>
</section>
