<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin active workers — v0.4.0 Sprint 0 (#400).
  //
  // One row per running job = one busy worker. Shows the lease and
  // flags a stale one (RequeueStuckJobs will reclaim it). Read-only.

  import { onMount, onDestroy } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';

  type Worker = {
    claimed_by: string;
    job_id: string;
    type: string;
    priority: number;
    attempts: number;
    claimed_at?: string | null;
    lease_expires_at?: string | null;
    lease_stale: boolean;
  };

  let workers = $state<Worker[]>([]);
  let loading = $state(false);
  let error = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      const r = await api.GET('/admin/jobs/workers');
      if (r.error) {
        error = (r.error as { error?: string }).error || 'load failed';
        return;
      }
      workers = (r.data as { items: Worker[] }).items;
    } finally {
      loading = false;
    }
  }

  function leaseIn(iso?: string | null): string {
    if (!iso) return '—';
    const ms = new Date(iso).getTime() - Date.now();
    const s = Math.round(ms / 1000);
    if (s < 0) return `${-s}s ago`;
    if (s < 60) return `${s}s`;
    return `${Math.floor(s / 60)}m`;
  }

  // Light auto-refresh so "active" stays honest without an SSE channel
  // (out of scope this sprint). 5s is gentle on the DB.
  let timer: ReturnType<typeof setInterval>;
  onMount(() => { void load(); timer = setInterval(() => void load(), 5000); });
  onDestroy(() => clearInterval(timer));
</script>

<svelte:head><title>Active workers — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Active workers</h2>
  <p class="text-sm text-fg-muted">
    Each running job is held by one worker. A stale lease means the
    worker missed its heartbeat; the queue will reclaim the job.
    Auto-refreshes every 5s. Read-only.
    <a class="text-accent hover:underline" href="/admin/jobs/queue">Queue</a> ·
    <a class="text-accent hover:underline" href="/admin/jobs/live">Live counts</a>
  </p>
</header>

{#if error}
  <div role="alert" class="mb-4 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>
{/if}

<div class="mb-2 text-sm text-fg-muted">{workers.length} active worker{workers.length === 1 ? '' : 's'}</div>

<div class="overflow-x-auto rounded border border-border">
  <table class="w-full text-left text-sm">
    <thead class="bg-surface-elevated text-xs uppercase tracking-wide text-fg-muted">
      <tr>
        <th class="px-3 py-2">Worker</th>
        <th class="px-3 py-2">Job type</th>
        <th class="px-3 py-2 text-right">Priority</th>
        <th class="px-3 py-2 text-right">Attempts</th>
        <th class="px-3 py-2 text-right">Lease</th>
        <th class="px-3 py-2">State</th>
      </tr>
    </thead>
    <tbody>
      {#each workers as w (w.job_id)}
        <tr class="border-t border-border">
          <td class="px-3 py-2 font-mono text-xs">{w.claimed_by}</td>
          <td class="px-3 py-2 font-mono text-xs">{w.type}</td>
          <td class="px-3 py-2 text-right tabular-nums">{w.priority}</td>
          <td class="px-3 py-2 text-right tabular-nums">{w.attempts}</td>
          <td class="px-3 py-2 text-right tabular-nums">{leaseIn(w.lease_expires_at)}</td>
          <td class="px-3 py-2">
            {#if w.lease_stale}
              <span class="rounded-full border border-danger/40 bg-danger/15 px-2 py-0.5 text-xs text-danger">lease stale</span>
            {:else}
              <span class="rounded-full border border-success/40 bg-success/15 px-2 py-0.5 text-xs text-success">running</span>
            {/if}
          </td>
        </tr>
      {:else}
        <tr><td colspan="6" class="px-3 py-6 text-center text-fg-muted">{loading ? 'Loading…' : 'No workers are running right now.'}</td></tr>
      {/each}
    </tbody>
  </table>
</div>
