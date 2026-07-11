<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // ArchiveTool side panel — Stats + extension breakdown + the
  // selected entry's details + expand/collapse-all helpers. The
  // file tree itself lives in the canvas-area ArchiveView; this
  // panel is for at-a-glance "what's in this archive" + quick
  // actions.

  import type { ToolContext } from '../contract';
  import { fmtBytes } from '$lib/archive/session.svelte';
  import { t } from '$stores/lang.svelte';

  let { ctx }: { ctx: ToolContext } = $props();
  const session = $derived(ctx.archiveSession);

  // Extension-frequency breakdown — useful for "what kind of bundle
  // is this" reads (200 .py + 50 .md → a Python project; 1k .png
  // + 1 .json → an asset pack).
  interface ExtCount { ext: string; count: number; size: number; }
  const extBreakdown = $derived.by<ExtCount[]>(() => {
    if (!session?.manifest) return [];
    const m = new Map<string, ExtCount>();
    for (const e of session.manifest.entries) {
      if (e.isDir) continue;
      const dot = e.path.lastIndexOf('.');
      const slash = e.path.lastIndexOf('/');
      // No extension OR extension contains a slash (= no real ext).
      const ext = dot > slash ? e.path.slice(dot + 1).toLowerCase() : '(none)';
      const cur = m.get(ext) ?? { ext, count: 0, size: 0 };
      cur.count++;
      cur.size += e.size;
      m.set(ext, cur);
    }
    return [...m.values()].sort((a, b) => b.count - a.count).slice(0, 12);
  });

  const fileCount = $derived(
    session?.manifest?.entries.filter((e) => !e.isDir).length ?? 0,
  );
  const dirCount = $derived(
    session?.manifest?.entries.filter((e) => e.isDir).length ?? 0,
  );

  function expandAll() {
    if (!session?.manifest) return;
    const next: Record<string, boolean> = {};
    for (const e of session.manifest.entries) {
      // Build folder set from each entry's parent segments.
      const parts = e.path.split('/');
      for (let i = 1; i < parts.length; i++) {
        const p = parts.slice(0, i).join('/');
        if (p) next[p] = true;
      }
    }
    session.setExpanded(next);
  }
  function collapseAll() {
    session?.setExpanded({});
  }
</script>

{#if !session}
  <div class="p-4 text-sm text-fg-muted"><p>{t('archive.tool_loading')}</p></div>
{:else}
  <div class="flex flex-col">
    <!-- ── Stats ─────────────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">{t('archive.stats')}</h3>
      {#if !session.manifest}
        <p class="rounded border border-border bg-surface/60 px-2 py-1 text-[10px] text-fg-muted">{t('archive.no_manifest_yet')}</p>
      {:else}
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-[10px]">
          <dt class="text-fg-muted">{t('archive.stat_format')}</dt>
          <dd class="font-mono text-fg">{session.manifest.format || '—'}</dd>
          <dt class="text-fg-muted">{t('archive.stat_files')}</dt>
          <dd class="font-mono text-fg">{fileCount}</dd>
          <dt class="text-fg-muted">{t('archive.stat_folders')}</dt>
          <dd class="font-mono text-fg">{dirCount}</dd>
          <dt class="text-fg-muted">{t('archive.stat_total_size')}</dt>
          <dd class="font-mono text-fg">{fmtBytes(session.manifest.totalSize)}</dd>
          {#if session.manifest.truncated}
            <dt class="text-yellow-400">{t('archive.stat_truncated')}</dt>
            <dd class="text-fg">{t('archive.stat_truncated_hint')}</dd>
          {/if}
        </dl>
      {/if}
    </section>

    <!-- ── Selection ────────────────────────────────────────── -->
    {#if session.selectedPath}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">{t('archive.selected')}</h3>
        <p class="break-all font-mono text-[10px] text-fg">{session.selectedPath}</p>
        <div class="mt-1 flex items-center gap-2 text-[10px] text-fg-muted">
          <span>{fmtBytes(session.previewSize)}</span>
          {#if session.previewMime}
            <span class="rounded bg-surface px-1 py-px font-mono">{session.previewMime}</span>
          {/if}
        </div>
        <button
          type="button"
          onclick={() => session.downloadEntry(session.selectedPath!)}
          class="mt-2 w-full rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25"
        >{t('archive.download_entry_button')}</button>
      </section>
    {/if}

    <!-- ── Tree controls ────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">{t('archive.tree')}</h3>
      <div class="grid grid-cols-2 gap-1">
        <button
          type="button"
          onclick={expandAll}
          class="rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
        >{t('archive.expand_all')}</button>
        <button
          type="button"
          onclick={collapseAll}
          class="rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
        >{t('archive.collapse_all')}</button>
      </div>
    </section>

    <!-- ── Extract ──────────────────────────────────────────── -->
    {#if session.manifest && fileCount > 0}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">{t('archive.extract')}</h3>
        <button
          type="button"
          onclick={() => session.downloadBundle()}
          class="w-full rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25"
          title={t('archive.extract_all_title')}
        >{t('archive.extract_all_button')}</button>
        <p class="mt-1 text-[10px] leading-snug text-fg-muted">
          {t('archive.extract_all_blurb')}
        </p>
      </section>
    {/if}

    <!-- ── By extension ─────────────────────────────────────── -->
    {#if extBreakdown.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">{t('archive.by_extension')}</h3>
        <div class="space-y-0.5">
          {#each extBreakdown as row (row.ext)}
            <button
              type="button"
              onclick={() => session.setFilter(row.ext === '(none)' ? '' : '.' + row.ext)}
              class="flex w-full items-center justify-between gap-2 rounded px-2 py-0.5 text-left text-[10px] text-fg hover:bg-surface-elevated"
              title={t('archive.filter_to_ext', { ext: row.ext })}
            >
              <span class="font-mono text-fg-muted/80">.{row.ext}</span>
              <span class="ml-auto shrink-0 font-mono text-fg">{row.count}</span>
              <span class="shrink-0 font-mono text-fg-muted/70">{fmtBytes(row.size)}</span>
            </button>
          {/each}
        </div>
      </section>
    {/if}
  </div>
{/if}
