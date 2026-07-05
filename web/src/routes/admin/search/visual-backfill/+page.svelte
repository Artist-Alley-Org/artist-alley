<script lang="ts">
  // Admin: CLIP visual-embedding backfill (Phase 1.16.B-3-followup-4,
  // closes #200).
  //
  // Mirrors the /admin/search/reindex page shape verbatim so operators
  // learn one UX. Difference: single-mode (embed image assets missing a
  // visual embedding); no scope picker; visible backlog gauge so
  // operators know whether to trigger a run at all.

  import { onMount, onDestroy } from 'svelte';

  type Run = {
    id: string;
    scope: Record<string, unknown>;
    processed: number;
    succeeded: number;
    failed: number;
    started_at: string;
    completed_at?: string | null;
    cancelled_at?: string | null;
    is_active: boolean;
    total_estimated?: number | null;
    last_error?: string | null;
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
  let pollHandle: ReturnType<typeof setInterval> | null = null;

  const anyActive = $derived(runs.some((r) => r.is_active));

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
      runs = data.items ?? [];
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
          error = body.message || 'Visual encoder sidecar not registered.';
        } else if (body.error === 'run_in_progress') {
          error = 'Another visual backfill is already running — cancel it first.';
        } else {
          error = `start failed: ${resp.status} ${body.error ?? ''}`.trim();
        }
        return;
      }
      providerRegistered = true;
      await Promise.all([loadRuns(), loadHealth()]);
    } finally {
      starting = false;
    }
  }

  async function cancelRun(r: Run) {
    if (!confirm(`Cancel visual backfill ${r.id.slice(0, 8)}…? Embeddings already written stay in place.`)) return;
    const resp = await fetch(`/api/v1/admin/search/visual-backfill/runs/${r.id}/cancel`, {
      method: 'POST',
      credentials: 'include',
    });
    if (!resp.ok) {
      alert(`cancel failed: ${resp.status}`);
      return;
    }
    await loadRuns();
  }

  function relTime(s: string): string {
    const ms = Date.now() - new Date(s).getTime();
    if (ms < 60_000) return 'just now';
    const m = Math.floor(ms / 60_000);
    if (m < 60) return `${m}m ago`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h ago`;
    return `${Math.floor(h / 24)}d ago`;
  }

  function statusLabel(r: Run): string {
    if (r.cancelled_at) return 'cancelled';
    if (r.completed_at) return r.last_error ? 'failed' : 'done';
    return 'running';
  }

  function statusClass(r: Run): string {
    if (r.cancelled_at) return 'bg-fg-muted/15 text-fg-muted border-fg-muted/40';
    if (r.completed_at) return r.last_error
      ? 'bg-danger/15 text-danger border-danger/40'
      : 'bg-success/15 text-success border-success/40';
    return 'bg-info/15 text-info border-info/40';
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
</script>

<svelte:head><title>Visual-embedding backfill — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-5xl px-6 py-8">
  <h1 class="font-display mb-2 text-2xl font-semibold">Visual-embedding backfill</h1>
  <p class="mb-6 text-sm text-fg-muted">
    Generate CLIP visual embeddings for image assets that don't yet have one, so reverse-image search can find them.
    Requires the <code class="rounded bg-bg px-1">aa-clip-visual-local</code> sidecar to be running and enabled in
    system configuration. Cancel-safe at batch boundaries; embeddings already written stay in place.
  </p>

  <section class="mb-6 grid grid-cols-1 gap-3 sm:grid-cols-3">
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
  </section>

  <section class="mb-6 rounded border border-border bg-bg-soft p-4">
    <h2 class="mb-3 text-xs font-semibold uppercase tracking-wide text-fg-muted">Start a run</h2>
    <div class="flex flex-wrap items-center gap-3">
      <button
        onclick={startRun}
        disabled={starting || anyActive}
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
        data-testid="start-visual-backfill"
      >{starting ? 'Starting…' : anyActive ? 'Run active' : 'Start backfill'}</button>
      <button
        onclick={() => Promise.all([loadRuns(), loadHealth()])}
        disabled={loading}
        class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg disabled:opacity-50"
      >{loading ? 'Loading…' : 'Refresh'}</button>
      {#if !providerRegistered}
        <span class="text-xs text-danger">Sidecar not registered — enable <code>search.visual.enabled</code> in sysconfig.</span>
      {/if}
    </div>
    {#if error}
      <div class="mt-3 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
    {/if}
  </section>

  <section class="rounded border border-border bg-bg-soft">
    <header class="border-b border-border px-3 py-2 text-sm font-medium text-fg-muted">
      Recent runs ({runs.length})
    </header>
    <table class="w-full text-sm">
      <thead class="border-b border-border bg-bg/60 text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">Status</th>
          <th class="px-3 py-2 text-left font-medium">Started</th>
          <th class="px-3 py-2 text-right font-medium">Processed</th>
          <th class="px-3 py-2 text-right font-medium">Succeeded</th>
          <th class="px-3 py-2 text-right font-medium">Failed</th>
          <th class="px-3 py-2 text-right font-medium">Progress</th>
          <th class="px-3 py-2 text-right font-medium">Actions</th>
        </tr>
      </thead>
      <tbody>
        {#each runs as r (r.id)}
          <tr class="border-b border-border/40 last:border-0">
            <td class="px-3 py-2">
              <span class="inline-flex items-center rounded border px-2 py-0.5 text-xs font-medium {statusClass(r)}">{statusLabel(r)}</span>
            </td>
            <td class="px-3 py-2 text-fg-muted" title={r.started_at}>{relTime(r.started_at)}</td>
            <td class="px-3 py-2 text-right tabular-nums">{r.processed.toLocaleString()}</td>
            <td class="px-3 py-2 text-right tabular-nums text-success">{r.succeeded.toLocaleString()}</td>
            <td class="px-3 py-2 text-right tabular-nums {r.failed > 0 ? 'text-danger' : 'text-fg-muted'}">{r.failed.toLocaleString()}</td>
            <td class="px-3 py-2 text-right tabular-nums text-fg-muted">
              {#if progressPct(r) !== null}
                {progressPct(r)}%
              {:else}
                —
              {/if}
            </td>
            <td class="px-3 py-2 text-right">
              {#if r.is_active}
                <button
                  onclick={() => cancelRun(r)}
                  class="rounded border border-danger/60 px-2 py-1 text-xs text-danger hover:bg-danger hover:text-on-danger"
                >Cancel</button>
              {/if}
            </td>
          </tr>
        {/each}
        {#if runs.length === 0 && !loading}
          <tr>
            <td colspan="7" class="px-3 py-6 text-center text-fg-muted">No visual backfill runs yet.</td>
          </tr>
        {/if}
      </tbody>
    </table>
  </section>
</div>
