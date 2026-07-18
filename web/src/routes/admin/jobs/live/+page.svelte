<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin live job counts — v0.4.0 Sprint 0 (#400).
  //
  // Per-(type, status) counts, polled on an interval (no SSE this
  // sprint). Gives an operator a live pulse of the queue. Read-only.

  import { onMount, onDestroy } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';

  type Count = { type: string; status: string; count: number };

  const STATUS_ORDER = ['pending', 'running', 'done', 'failed', 'cancelled'];

  let counts = $state<Count[]>([]);
  let error = $state('');
  let lastAt = $state<Date | null>(null);

  // Pivot the flat (type,status,count) list into a type → status → count
  // matrix for the grid.
  const byType = $derived.by(() => {
    const m = new Map<string, Record<string, number>>();
    for (const c of counts) {
      const row = m.get(c.type) ?? {};
      row[c.status] = c.count;
      m.set(c.type, row);
    }
    return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  });
  const totals = $derived.by(() => {
    const t: Record<string, number> = {};
    for (const c of counts) t[c.status] = (t[c.status] ?? 0) + c.count;
    return t;
  });

  async function load() {
    const r = await api.GET('/admin/jobs/status-counts');
    if (r.error) {
      error = (r.error as { error?: string }).error || 'load failed';
      return;
    }
    error = '';
    counts = (r.data as { items: Count[] }).items;
    lastAt = new Date();
  }

  // 3s poll — the "live" tile's whole point. No SSE this sprint.
  let timer: ReturnType<typeof setInterval>;
  onMount(() => { void load(); timer = setInterval(() => void load(), 3000); });
  onDestroy(() => clearInterval(timer));

  function cellClass(status: string, n: number): string {
    if (!n) return 'text-fg-muted/40';
    if (status === 'failed') return 'text-danger font-medium';
    if (status === 'running') return 'text-info font-medium';
    if (status === 'pending') return 'text-warning';
    return 'text-fg';
  }
</script>

<svelte:head><title>Live job counts — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Live job counts</h2>
  <p class="text-sm text-fg-muted">
    Jobs by type and status, refreshed every 3s.
    {#if lastAt}<span class="text-fg-muted/70">Updated {lastAt.toLocaleTimeString()}.</span>{/if}
    <a class="text-accent hover:underline" href="/admin/jobs/queue">Queue</a> ·
    <a class="text-accent hover:underline" href="/admin/jobs/workers">Active workers</a>
  </p>
</header>

{#if error}
  <div role="alert" class="mb-4 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>
{/if}

<div class="overflow-x-auto rounded border border-border">
  <table class="w-full text-left text-sm">
    <thead class="bg-surface-elevated text-xs uppercase tracking-wide text-fg-muted">
      <tr>
        <th class="px-3 py-2">Type</th>
        {#each STATUS_ORDER as s}<th class="px-3 py-2 text-right">{s}</th>{/each}
      </tr>
    </thead>
    <tbody>
      {#each byType as [type, row] (type)}
        <tr class="border-t border-border">
          <td class="px-3 py-2 font-mono text-xs">{type}</td>
          {#each STATUS_ORDER as s}
            <td class={`px-3 py-2 text-right tabular-nums ${cellClass(s, row[s] ?? 0)}`}>{row[s] ?? 0}</td>
          {/each}
        </tr>
      {:else}
        <tr><td colspan="6" class="px-3 py-6 text-center text-fg-muted">No jobs in the queue.</td></tr>
      {/each}
    </tbody>
    {#if byType.length}
      <tfoot>
        <tr class="border-t-2 border-border font-medium">
          <td class="px-3 py-2">Total</td>
          {#each STATUS_ORDER as s}<td class="px-3 py-2 text-right tabular-nums">{totals[s] ?? 0}</td>{/each}
        </tr>
      </tfoot>
    {/if}
  </table>
</div>
