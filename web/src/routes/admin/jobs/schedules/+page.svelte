<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin scheduled jobs — v0.4.0 Sprint 1 (#401).
  //
  // Read-only view of future-dated pending work (jobs enqueued with a
  // scheduled_for in the future). There is no scheduler to configure —
  // this is a window into what's queued to run later.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';

  type Row = {
    id: string;
    type: string;
    priority: number;
    attempts: number;
    max_attempts: number;
    scheduled_for?: string | null;
    due_in_seconds: number;
  };

  let rows = $state<Row[]>([]);
  let total = $state(0);
  let offset = $state(0);
  const PAGE_SIZE = 50;
  let loading = $state(false);
  let error = $state('');

  async function load(reset = true) {
    loading = true;
    error = '';
    if (reset) offset = 0;
    try {
      const r = await api.GET('/admin/jobs/scheduled', { params: { query: { limit: PAGE_SIZE, offset } } });
      if (r.error) { error = (r.error as { error?: string }).error || 'load failed'; return; }
      const data = r.data as { items: Row[]; total: number };
      rows = data.items;
      total = data.total;
    } finally { loading = false; }
  }

  function humanDue(s: number): string {
    if (s <= 0) return 'due';
    if (s < 60) return `in ${s}s`;
    const m = Math.floor(s / 60); if (m < 60) return `in ${m}m`;
    const h = Math.floor(m / 60); if (h < 24) return `in ${h}h`;
    return `in ${Math.floor(h / 24)}d`;
  }
  function fmt(iso?: string | null): string { return iso ? new Date(iso).toLocaleString() : '—'; }
  function nextPage() { offset += PAGE_SIZE; void load(false); }
  function prevPage() { offset = Math.max(0, offset - PAGE_SIZE); void load(false); }

  onMount(() => { void load(true); });
</script>

<svelte:head><title>Scheduled jobs — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Scheduled jobs</h2>
  <p class="text-sm text-fg-muted">
    Pending work dated to run in the future. Read-only.
    <a class="text-accent hover:underline" href="/admin/jobs/queue">Queue</a> ·
    <a class="text-accent hover:underline" href="/admin/jobs/failed">Failed</a>
  </p>
</header>

{#if error}<div role="alert" class="mb-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>{/if}

<div class="mb-2 flex items-center justify-between text-sm text-fg-muted">
  <span>{total} scheduled</span>
  <span class="flex gap-2">
    <button onclick={prevPage} disabled={offset === 0 || loading} class="rounded border border-border px-2 py-1 disabled:opacity-40">Prev</button>
    <button onclick={nextPage} disabled={offset + rows.length >= total || loading} class="rounded border border-border px-2 py-1 disabled:opacity-40">Next</button>
  </span>
</div>

<div class="overflow-x-auto rounded border border-border">
  <table class="w-full text-left text-sm">
    <thead class="bg-surface-elevated text-xs uppercase tracking-wide text-fg-muted">
      <tr>
        <th class="px-3 py-2">Type</th>
        <th class="px-3 py-2 text-right">Priority</th>
        <th class="px-3 py-2">Scheduled for</th>
        <th class="px-3 py-2 text-right">Due</th>
      </tr>
    </thead>
    <tbody>
      {#each rows as j (j.id)}
        <tr class="border-t border-border">
          <td class="px-3 py-2 font-mono text-xs">{j.type}</td>
          <td class="px-3 py-2 text-right tabular-nums">{j.priority}</td>
          <td class="px-3 py-2 text-xs">{fmt(j.scheduled_for)}</td>
          <td class="px-3 py-2 text-right text-xs tabular-nums">{humanDue(j.due_in_seconds)}</td>
        </tr>
      {:else}
        <tr><td colspan="4" class="px-3 py-6 text-center text-fg-muted">{loading ? 'Loading…' : 'Nothing scheduled.'}</td></tr>
      {/each}
    </tbody>
  </table>
</div>
