<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: CLIP visual-embedding backfill (Phase 1.16.B-3-followup-4,
  // closes #200).
  //
  // Phase 1.16.B-followup-2 (#186): rendering delegated to
  // AdminBackfillPanel. This file keeps the API integration + the
  // gauges (backlog / embedded / coverage) + the 503 sidecar-not-
  // registered gate; the panel owns the header / start-controls
  // layout / recent-runs table.

  import { onMount, onDestroy } from 'svelte';
  import AdminBackfillPanel, {
    type BaseRun,
  } from '$components/admin/AdminBackfillPanel.svelte';

  type Run = BaseRun & {
    scope: Record<string, unknown>;
    is_active: boolean;
    total_estimated?: number | null;
    started_by_user_ref?: number | null;
  };

  type HealthPayload = {
    notes?: string[];
  };

  let runs = $state<Run[]>([]);
  let starting = $state(false);
  let loading = $state(false);
  let error = $state('');
  let backlog = $state<number | null>(null);
  let totalEmbedded = $state<number | null>(null);
  let providerRegistered = $state(true);
  let sidecarWarning = $state('');
  let pollHandle: ReturnType<typeof setInterval> | null = null;

  const anyActive = $derived(runs.some((r) => r.isActive));

  async function loadRuns() {
    loading = true;
    error = '';
    try {
      const r = await fetch('/api/v1/admin/search/visual-backfill/runs?limit=20', {
        credentials: 'include',
      });
      if (!r.ok) {
        error = `load failed: ${r.status}`;
        return;
      }
      const data = await r.json();
      runs = (data.items ?? []).map((row: Omit<Run, 'isActive'>) => ({
        ...row,
        isActive: row.is_active,
      }));
    } finally {
      loading = false;
    }
  }

  async function loadHealth() {
    // Pull backlog + total-embedded from /admin/search/health gauges.
    // Notes are formatted as "key=value" strings appended by the
    // gauge provider on the Go side.
    try {
      const r = await fetch('/api/v1/admin/search/health', { credentials: 'include' });
      if (!r.ok) return;
      const data = (await r.json()) as HealthPayload;
      const notes = data.notes ?? [];
      const pick = (key: string): number | null => {
        for (const n of notes) {
          const [k, v] = n.split('=');
          if (k === key) {
            const parsed = Number(v);
            return Number.isFinite(parsed) ? parsed : null;
          }
        }
        return null;
      };
      backlog = pick('visual_embedding_backlog');
      totalEmbedded = pick('visual_embedding_total');
    } catch {
      // best-effort; health may briefly 5xx while workers restart
    }
  }

  async function startRun() {
    if (starting) return;
    starting = true;
    error = '';
    try {
      const resp = await fetch('/api/v1/admin/search/visual-backfill', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: '{}',
      });
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({}));
        if (resp.status === 503 && body.error === 'provider_not_registered') {
          providerRegistered = false;
          sidecarWarning = 'Sidecar not registered — enable search.visual.enabled in sysconfig.';
          error = body.message || 'Visual encoder sidecar not registered.';
        } else if (body.error === 'run_in_progress') {
          error = 'Another visual backfill is already running — cancel it first.';
        } else {
          error = `start failed: ${resp.status} ${body.error ?? ''}`.trim();
        }
        return;
      }
      providerRegistered = true;
      sidecarWarning = '';
      await Promise.all([loadRuns(), loadHealth()]);
    } finally {
      starting = false;
    }
  }

  async function refreshAll() {
    await Promise.all([loadRuns(), loadHealth()]);
  }

  async function cancelRun(run: BaseRun) {
    const resp = await fetch(`/api/v1/admin/search/visual-backfill/runs/${run.id}/cancel`, {
      method: 'POST',
      credentials: 'include',
    });
    if (!resp.ok) {
      alert(`cancel failed: ${resp.status}`);
      return;
    }
    await loadRuns();
  }

  function cancelConfirmMessage(run: BaseRun): string {
    return `Cancel visual backfill ${run.id.slice(0, 8)}…? Embeddings already written stay in place.`;
  }

  function progressPct(r: Run): number | null {
    if (!r.total_estimated || r.total_estimated <= 0) return null;
    const pct = Math.min(100, Math.max(0, (r.processed / r.total_estimated) * 100));
    return Math.round(pct);
  }

  onMount(async () => {
    await Promise.all([loadRuns(), loadHealth()]);
    pollHandle = setInterval(() => {
      if (anyActive) void loadRuns();
      void loadHealth();
    }, 3000);
  });
  onDestroy(() => {
    if (pollHandle) clearInterval(pollHandle);
  });

  const startDisabledReason = $derived(
    providerRegistered ? '' : 'Visual encoder sidecar not registered.',
  );
</script>

<svelte:head><title>Visual-embedding backfill — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-5xl px-6 py-8">
  <AdminBackfillPanel
    title="Visual-embedding backfill"
    {runs}
    {loading}
    {starting}
    {error}
    onStart={startRun}
    onRefresh={refreshAll}
    onCancel={cancelRun}
    {cancelConfirmMessage}
    startLabel="Start backfill"
    {startDisabledReason}
    warning={sidecarWarning}
    emptyText="No visual backfill runs yet."
    extraColumnCountAfterFailed={1}
  >
    {#snippet headerDescription()}
      <p class="mb-6 text-sm text-fg-muted">
        Generate CLIP visual embeddings for image assets that don't yet have one, so reverse-image search can find them.
        Requires the <code class="rounded bg-bg px-1">aa-clip-visual-local</code> sidecar to be running and enabled in
        system configuration. Cancel-safe at batch boundaries; embeddings already written stay in place.
      </p>
    {/snippet}

    {#snippet gauges()}
      <div class="rounded border border-border bg-bg-soft p-4">
        <div class="text-xs font-semibold uppercase tracking-wide text-fg-muted">Backlog</div>
        <div class="mt-1 text-2xl font-semibold tabular-nums" data-testid="backlog">
          {backlog === null ? '—' : backlog < 0 ? 'error' : backlog.toLocaleString()}
        </div>
        <div class="text-xs text-fg-muted">image assets missing embedding</div>
      </div>
      <div class="rounded border border-border bg-bg-soft p-4">
        <div class="text-xs font-semibold uppercase tracking-wide text-fg-muted">Embedded</div>
        <div class="mt-1 text-2xl font-semibold tabular-nums" data-testid="total-embedded">
          {totalEmbedded === null ? '—' : totalEmbedded < 0 ? 'error' : totalEmbedded.toLocaleString()}
        </div>
        <div class="text-xs text-fg-muted">assets with a visual embedding row</div>
      </div>
      <div class="rounded border border-border bg-bg-soft p-4">
        <div class="text-xs font-semibold uppercase tracking-wide text-fg-muted">Coverage</div>
        <div class="mt-1 text-2xl font-semibold tabular-nums">
          {#if backlog !== null && totalEmbedded !== null && backlog >= 0 && totalEmbedded >= 0}
            {@const total = backlog + totalEmbedded}
            {total === 0 ? '—' : `${Math.round((totalEmbedded / total) * 100)}%`}
          {:else}
            —
          {/if}
        </div>
        <div class="text-xs text-fg-muted">embedded / (embedded + backlog)</div>
      </div>
    {/snippet}

    {#snippet extraColumnHeadersAfterFailed()}
      <th class="px-3 py-2 text-right font-medium">Progress</th>
    {/snippet}
    {#snippet extraRowCellsAfterFailed(r)}
      <td class="px-3 py-2 text-right tabular-nums text-fg-muted">
        {#if progressPct(r as Run) !== null}
          {progressPct(r as Run)}%
        {:else}
          —
        {/if}
      </td>
    {/snippet}
  </AdminBackfillPanel>
</div>
