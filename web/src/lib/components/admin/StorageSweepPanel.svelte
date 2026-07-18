<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Shared surface for both storage integrity sweeps (#403 part 2).
  //
  // The orphan-scan and checksum-verify pages are the same view over
  // the same endpoints, differing only in which `kind` they filter to
  // and how expensive a run is — so this is one component parameterised
  // by kind rather than two near-identical pages.
  //
  // Reads gate on system.storage.read; triggering gates on system.admin
  // server-side, so the trigger control is hidden (not just disabled)
  // for read-cap holders and the API refuses regardless.

  import { onMount, onDestroy } from 'svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';

  type Props = {
    kind: 'orphan_scan' | 'checksum_verify';
    title: string;
    intro: string;
    /** Shown in the confirm dialog before triggering. */
    costWarning?: string;
  };
  let { kind, title, intro, costWarning }: Props = $props();

  type Run = {
    id: string;
    kind: string;
    status: 'running' | 'completed' | 'failed';
    objects_scanned: number;
    findings_count: number;
    started_at?: string | null;
    finished_at?: string | null;
    error?: string | null;
  };
  type Finding = {
    id: string;
    finding: string;
    object_hash: string;
    variant_key: string;
    detail: string;
    detected_at?: string | null;
  };

  const canTrigger = $derived(auth.can('system.admin'));

  let runs = $state<Run[]>([]);
  let loading = $state(true);
  let error = $state('');
  let flash = $state('');
  let triggering = $state(false);

  // Drill-in state: which run's findings are open.
  let openRunId = $state<string | null>(null);
  let findings = $state<Finding[]>([]);
  let findingsTotal = $state(0);
  let findingsLoading = $state(false);

  const hasRunning = $derived(runs.some((r) => r.status === 'running'));
  let poll: ReturnType<typeof setInterval> | null = null;

  async function loadRuns() {
    error = '';
    const r = await api.GET('/admin/storage/sweeps', { params: { query: { kind, limit: 20 } } });
    if (r.error) { error = (r.error as { error?: string }).error || 'Could not load sweep runs.'; return; }
    runs = (r.data as { items: Run[] }).items;
  }

  async function openFindings(runId: string) {
    if (openRunId === runId) { openRunId = null; return; }
    openRunId = runId;
    findingsLoading = true;
    findings = [];
    try {
      const r = await api.GET('/admin/storage/sweeps/{id}/findings', {
        params: { path: { id: runId }, query: { limit: 100 } }
      });
      if (r.error) { error = (r.error as { error?: string }).error || 'Could not load findings.'; return; }
      const d = r.data as { items: Finding[]; total: number };
      findings = d.items;
      findingsTotal = d.total;
    } finally { findingsLoading = false; }
  }

  async function trigger() {
    const msg = costWarning
      ? `${costWarning}\n\nStart this sweep now?`
      : 'Start this sweep now?';
    if (!confirm(msg)) return;
    triggering = true;
    flash = '';
    error = '';
    try {
      const r = await api.POST('/admin/storage/sweeps', { body: { kind } });
      if (r.error) { error = (r.error as { error?: string }).error || 'Could not start the sweep.'; return; }
      flash = 'Sweep queued. It runs as a background job and updates here as it progresses.';
      await loadRuns();
    } finally { triggering = false; }
  }

  function fmt(iso?: string | null): string { return iso ? new Date(iso).toLocaleString() : '—'; }
  function num(n: number): string { return n.toLocaleString(); }
  function duration(r: Run): string {
    if (!r.started_at) return '—';
    const end = r.finished_at ? new Date(r.finished_at) : new Date();
    const s = Math.max(0, Math.round((end.getTime() - new Date(r.started_at).getTime()) / 1000));
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    return m < 60 ? `${m}m ${s % 60}s` : `${Math.floor(m / 60)}h ${m % 60}m`;
  }
  function statusClass(s: Run['status']): string {
    if (s === 'completed') return 'text-success';
    if (s === 'failed') return 'text-danger';
    return 'text-accent';
  }

  onMount(() => {
    void loadRuns().finally(() => (loading = false));
    // A running sweep re-enqueues itself batch by batch, so refresh
    // while one is in flight to show progress climbing.
    poll = setInterval(() => { if (hasRunning) void loadRuns(); }, 4000);
  });
  onDestroy(() => { if (poll) clearInterval(poll); });
</script>

<header class="mb-4">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <h2 class="text-2xl font-semibold">{title}</h2>
      <p class="mt-1 max-w-3xl text-sm text-fg-muted">{intro}</p>
    </div>
    {#if canTrigger}
      <button
        onclick={trigger}
        disabled={triggering || hasRunning}
        class="shrink-0 rounded bg-accent px-3 py-2 text-sm font-medium text-on-accent disabled:opacity-50"
      >
        {triggering ? 'Starting…' : hasRunning ? 'Sweep running…' : 'Run sweep'}
      </button>
    {/if}
  </div>
  <p class="mt-2 text-sm text-fg-muted">
    Sweeps run as background jobs — <a class="text-accent hover:underline" href="/admin/jobs/queue">view the queue</a>.
    {#if !canTrigger}Read-only — starting a sweep needs admin.{/if}
  </p>
</header>

{#if flash}<div class="mb-3 rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-on-success-container">{flash}</div>{/if}
{#if error}<div role="alert" class="mb-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>{/if}

<div class="overflow-x-auto rounded border border-border">
  <table class="w-full text-left text-sm">
    <thead class="bg-surface-elevated text-xs uppercase tracking-wide text-fg-muted">
      <tr>
        <th class="px-3 py-2">Started</th>
        <th class="px-3 py-2">Status</th>
        <th class="px-3 py-2 text-right">Scanned</th>
        <th class="px-3 py-2 text-right">Findings</th>
        <th class="px-3 py-2 text-right">Took</th>
        <th class="px-3 py-2"></th>
      </tr>
    </thead>
    <tbody>
      {#each runs as r (r.id)}
        <tr class="border-t border-border">
          <td class="px-3 py-2 whitespace-nowrap text-xs">{fmt(r.started_at)}</td>
          <td class={`px-3 py-2 font-medium ${statusClass(r.status)}`}>
            {r.status}
            {#if r.error}<span class="block text-xs text-danger" title={r.error}>{r.error}</span>{/if}
          </td>
          <td class="px-3 py-2 text-right tabular-nums">{num(r.objects_scanned)}</td>
          <td class="px-3 py-2 text-right tabular-nums">
            {#if r.findings_count > 0}
              <span class="font-semibold text-danger">{num(r.findings_count)}</span>
            {:else}
              <span class="text-fg-muted">0</span>
            {/if}
          </td>
          <td class="px-3 py-2 text-right text-xs tabular-nums">{duration(r)}</td>
          <td class="px-3 py-2 text-right">
            {#if r.findings_count > 0}
              <button onclick={() => openFindings(r.id)} class="rounded border border-border px-2 py-1 text-xs hover:bg-state-hover">
                {openRunId === r.id ? 'Hide' : 'View'}
              </button>
            {/if}
          </td>
        </tr>
        {#if openRunId === r.id}
          <tr class="border-t border-border bg-surface-elevated/50">
            <td colspan="6" class="px-3 py-3">
              {#if findingsLoading}
                <p class="text-sm text-fg-muted">Loading findings…</p>
              {:else}
                <p class="mb-2 text-xs text-fg-muted">
                  Showing {findings.length} of {num(findingsTotal)} findings. These record what was true at scan
                  time — the database may have changed since.
                </p>
                <div class="overflow-x-auto rounded border border-border bg-surface">
                  <table class="w-full text-left text-xs">
                    <thead class="text-fg-muted">
                      <tr>
                        <th class="px-2 py-1.5">Finding</th>
                        <th class="px-2 py-1.5">Object</th>
                        <th class="px-2 py-1.5">Variant</th>
                        <th class="px-2 py-1.5">Detail</th>
                      </tr>
                    </thead>
                    <tbody>
                      {#each findings as f (f.id)}
                        <tr class="border-t border-border">
                          <td class="px-2 py-1.5 font-medium text-danger whitespace-nowrap">{f.finding}</td>
                          <td class="px-2 py-1.5 font-mono" title={f.object_hash}>{f.object_hash.slice(0, 12)}…</td>
                          <td class="px-2 py-1.5 font-mono">{f.variant_key}</td>
                          <td class="px-2 py-1.5 text-fg-muted">{f.detail}</td>
                        </tr>
                      {/each}
                    </tbody>
                  </table>
                </div>
              {/if}
            </td>
          </tr>
        {/if}
      {:else}
        <tr>
          <td colspan="6" class="px-3 py-8 text-center text-fg-muted">
            {#if loading}
              Loading…
            {:else}
              No sweeps have run yet.
              {#if canTrigger}Use <strong>Run sweep</strong> to start one.{/if}
            {/if}
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>
