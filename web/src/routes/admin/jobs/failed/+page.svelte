<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin failed jobs — v0.4.0 Sprint 1 (#401).
  //
  // Lists failed jobs with their last_error, and (for system.admin) lets
  // an operator requeue a dead job or cancel it. Reads use the S0 jobs
  // list with status=failed; the actions gate on system.admin server-
  // side — a read-only holder sees the list but no buttons.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';

  type JobRow = {
    id: string;
    type: string;
    status: string;
    priority: number;
    attempts: number;
    max_attempts: number;
    last_error?: string | null;
    age_seconds: number;
  };

  const canAct = $derived(auth.can('system.admin'));

  let rows = $state<JobRow[]>([]);
  let total = $state(0);
  let offset = $state(0);
  const PAGE_SIZE = 50;
  let loading = $state(false);
  let error = $state('');
  let flash = $state('');

  async function load(reset = true) {
    loading = true;
    error = '';
    if (reset) offset = 0;
    try {
      const r = await api.GET('/admin/jobs', { params: { query: { status: 'failed', limit: PAGE_SIZE, offset } } });
      if (r.error) { error = (r.error as { error?: string }).error || 'load failed'; return; }
      const data = r.data as { items: JobRow[]; total: number };
      rows = data.items;
      total = data.total;
    } finally { loading = false; }
  }

  async function requeue(row: JobRow) {
    flash = '';
    const r = await api.POST('/admin/jobs/{id}/requeue', { params: { path: { id: row.id } } });
    if (r.error) { error = (r.error as { error?: string }).error || 'requeue failed'; return; }
    flash = `Requeued ${row.type}.`;
    rows = rows.filter(x => x.id !== row.id);
    total = Math.max(0, total - 1);
  }

  async function cancel(row: JobRow) {
    if (!confirm(`Cancel this ${row.type} job?`)) return;
    flash = '';
    const r = await api.POST('/admin/jobs/{id}/cancel', { params: { path: { id: row.id } } });
    if (r.error) { error = (r.error as { error?: string }).error || 'cancel failed'; return; }
    flash = `Cancelled ${row.type}.`;
    rows = rows.filter(x => x.id !== row.id);
    total = Math.max(0, total - 1);
  }

  function humanAge(s: number): string {
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60); if (m < 60) return `${m}m`;
    const h = Math.floor(m / 60); if (h < 24) return `${h}h`;
    return `${Math.floor(h / 24)}d`;
  }
  function nextPage() { offset += PAGE_SIZE; void load(false); }
  function prevPage() { offset = Math.max(0, offset - PAGE_SIZE); void load(false); }

  onMount(() => { void load(true); });
</script>

<svelte:head><title>Failed jobs — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Failed jobs</h2>
  <p class="text-sm text-fg-muted">
    Jobs that exhausted their retries.
    {#if canAct}Requeue sends one back to the pending pool (fresh attempt); cancel marks it done-with-no-retry.{:else}Read-only — requeue/cancel need admin.{/if}
    <a class="text-accent hover:underline" href="/admin/jobs/queue">Queue</a> ·
    <a class="text-accent hover:underline" href="/admin/jobs/schedules">Scheduled</a>
  </p>
</header>

{#if flash}<div class="mb-3 rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-on-success-container">{flash}</div>{/if}
{#if error}<div role="alert" class="mb-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>{/if}

<div class="mb-2 flex items-center justify-between text-sm text-fg-muted">
  <span>{total} failed</span>
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
        <th class="px-3 py-2 text-right">Attempts</th>
        <th class="px-3 py-2 text-right">Age</th>
        <th class="px-3 py-2">Last error</th>
        {#if canAct}<th class="px-3 py-2 text-right">Actions</th>{/if}
      </tr>
    </thead>
    <tbody>
      {#each rows as j (j.id)}
        <tr class="border-t border-border">
          <td class="px-3 py-2 font-mono text-xs">{j.type}</td>
          <td class="px-3 py-2 text-right tabular-nums">{j.attempts}/{j.max_attempts}</td>
          <td class="px-3 py-2 text-right tabular-nums">{humanAge(j.age_seconds)}</td>
          <td class="max-w-md truncate px-3 py-2 text-xs text-danger" title={j.last_error ?? ''}>{j.last_error ?? ''}</td>
          {#if canAct}
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <button onclick={() => requeue(j)} class="rounded bg-accent px-2 py-1 text-xs font-medium text-on-accent">Requeue</button>
              <button onclick={() => cancel(j)} class="ml-1 rounded border border-border px-2 py-1 text-xs hover:bg-state-hover">Cancel</button>
            </td>
          {/if}
        </tr>
      {:else}
        <tr><td colspan={canAct ? 5 : 4} class="px-3 py-6 text-center text-fg-muted">{loading ? 'Loading…' : 'No failed jobs.'}</td></tr>
      {/each}
    </tbody>
  </table>
</div>
