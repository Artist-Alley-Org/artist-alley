<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin jobs queue — v0.4.0 Sprint 0 (#400).
  //
  // Read-only view of the job queue: filter by status + type, ordered
  // the way they'll run (priority ASC, enqueued_at ASC), with server-
  // computed age. No write path — requeue/cancel land in Sprint 1.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';

  type JobRow = {
    id: string;
    type: string;
    status: string;
    priority: number;
    attempts: number;
    max_attempts: number;
    claimed_by?: string | null;
    last_error?: string | null;
    enqueued_at?: string | null;
    age_seconds: number;
  };

  const STATUSES = ['', 'pending', 'running', 'done', 'failed', 'cancelled'];

  let statusFilter = $state('');
  let typeFilter = $state('');
  let rows = $state<JobRow[]>([]);
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
      const params: Record<string, string | number> = { limit: PAGE_SIZE, offset };
      if (statusFilter) params.status = statusFilter;
      if (typeFilter) params.type = typeFilter;
      const r = await api.GET('/admin/jobs', { params: { query: params } });
      if (r.error) {
        error = (r.error as { error?: string }).error || 'load failed';
        return;
      }
      const data = r.data as { items: JobRow[]; total: number };
      rows = data.items;
      total = data.total;
    } finally {
      loading = false;
    }
  }

  function pageInfo(): string {
    const start = total === 0 ? 0 : offset + 1;
    const end = Math.min(offset + rows.length, total);
    return `${start}–${end} of ${total}`;
  }
  function nextPage() { offset += PAGE_SIZE; void load(false); }
  function prevPage() { offset = Math.max(0, offset - PAGE_SIZE); void load(false); }

  // age_seconds is server-computed (NOW() - enqueued_at) so it doesn't
  // trust the client clock; humanize it.
  function humanAge(s: number): string {
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h`;
    return `${Math.floor(h / 24)}d`;
  }

  function statusPill(s: string): string {
    switch (s) {
      case 'running':   return 'bg-info/15 text-info border-info/40';
      case 'pending':   return 'bg-warning/15 text-warning border-warning/40';
      case 'done':      return 'bg-success/15 text-success border-success/40';
      case 'failed':    return 'bg-danger/15 text-danger border-danger/40';
      case 'cancelled': return 'bg-fg-muted/15 text-fg-muted border-fg-muted/40';
      default:          return 'bg-fg-muted/15 text-fg-muted border-fg-muted/40';
    }
  }

  onMount(() => { void load(true); });
</script>

<svelte:head><title>Jobs queue — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Jobs queue</h2>
  <p class="text-sm text-fg-muted">
    Every async job — derivatives, previews, AI tagging, federation
    outbox — ordered as it will run (priority, then enqueue time).
    Read-only.
    <a class="text-accent hover:underline" href="/admin/jobs/workers">Active workers</a> ·
    <a class="text-accent hover:underline" href="/admin/jobs/live">Live counts</a>
  </p>
</header>

<section class="mb-6 rounded border border-border bg-surface-elevated p-4">
  <div class="grid gap-3 sm:grid-cols-3">
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Status</span>
      <select bind:value={statusFilter} class="rounded border border-border-strong bg-surface p-2 text-fg">
        {#each STATUSES as s}
          <option value={s}>{s || '— all —'}</option>
        {/each}
      </select>
    </label>
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Type</span>
      <input bind:value={typeFilter} placeholder="preview.raster, ai.embed, …"
        class="rounded border border-border-strong bg-surface p-2 text-fg" />
    </label>
    <div class="flex items-end gap-2">
      <button onclick={() => load(true)} disabled={loading}
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-on-accent disabled:opacity-50">
        {loading ? 'Loading…' : 'Apply'}
      </button>
    </div>
  </div>
</section>

{#if error}
  <div role="alert" class="mb-4 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>
{/if}

<div class="mb-2 flex items-center justify-between text-sm text-fg-muted">
  <span>{pageInfo()}</span>
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
        <th class="px-3 py-2">Status</th>
        <th class="px-3 py-2 text-right">Priority</th>
        <th class="px-3 py-2 text-right">Attempts</th>
        <th class="px-3 py-2 text-right">Age</th>
        <th class="px-3 py-2">Worker</th>
        <th class="px-3 py-2">Last error</th>
      </tr>
    </thead>
    <tbody>
      {#each rows as j (j.id)}
        <tr class="border-t border-border">
          <td class="px-3 py-2 font-mono text-xs">{j.type}</td>
          <td class="px-3 py-2"><span class={`rounded-full border px-2 py-0.5 text-xs ${statusPill(j.status)}`}>{j.status}</span></td>
          <td class="px-3 py-2 text-right tabular-nums">{j.priority}</td>
          <td class="px-3 py-2 text-right tabular-nums">{j.attempts}/{j.max_attempts}</td>
          <td class="px-3 py-2 text-right tabular-nums">{humanAge(j.age_seconds)}</td>
          <td class="px-3 py-2 font-mono text-xs text-fg-muted">{j.claimed_by ?? '—'}</td>
          <td class="max-w-xs truncate px-3 py-2 text-xs text-danger" title={j.last_error ?? ''}>{j.last_error ?? ''}</td>
        </tr>
      {:else}
        <tr><td colspan="7" class="px-3 py-6 text-center text-fg-muted">{loading ? 'Loading…' : 'No jobs match.'}</td></tr>
      {/each}
    </tbody>
  </table>
</div>
