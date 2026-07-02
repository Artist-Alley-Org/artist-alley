<script lang="ts">
  // Admin: search reindex (Phase 1.16.B-5).
  //
  // Mirrors the metadata-extraction/backfills page shape verbatim
  // (scope picker + Start/Cancel + progress row + history table)
  // per pre-audit Q1 finding.

  import { onMount, onDestroy } from 'svelte';

  type Run = {
    id: string;
    scope: Record<string, unknown>;
    target: 'tsvector' | 'embedding' | 'both';
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

  let runs = $state<Run[]>([]);
  let starting = $state(false);
  let loading = $state(false);
  let error = $state('');
  let scope = $state('all');
  let target = $state<'tsvector' | 'embedding' | 'both'>('both');
  let pollHandle: ReturnType<typeof setInterval> | null = null;

  const anyActive = $derived(runs.some((r) => r.is_active));

  async function loadRuns() {
    loading = true;
    error = '';
    try {
      const r = await fetch('/api/v1/admin/search/reindex/runs?limit=20', { credentials: 'include' });
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

  async function startRun() {
    if (starting) return;
    starting = true;
    error = '';
    try {
      const resp = await fetch('/api/v1/admin/search/reindex', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({ scope: scope || 'all', target }),
      });
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({}));
        error = body.error === 'run_in_progress'
          ? 'Another reindex run is already active — cancel it first.'
          : `start failed: ${resp.status} ${body.error ?? ''}`.trim();
        return;
      }
      await loadRuns();
    } finally {
      starting = false;
    }
  }

  async function cancelRun(r: Run) {
    if (!confirm(`Cancel reindex ${r.id.slice(0, 8)}…? Embed jobs already enqueued still run.`)) return;
    const resp = await fetch(`/api/v1/admin/search/reindex/runs/${r.id}/cancel`, {
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
    if (r.completed_at) return 'done';
    return 'running';
  }

  function statusClass(r: Run): string {
    if (r.cancelled_at) return 'bg-fg-muted/15 text-fg-muted border-fg-muted/40';
    if (r.completed_at) return 'bg-success/15 text-success border-success/40';
    return 'bg-info/15 text-info border-info/40';
  }

  function scopeSummary(r: Run): string {
    const kind = (r.scope as { kind?: string } | undefined)?.kind ?? 'all';
    return kind;
  }

  onMount(async () => {
    await loadRuns();
    pollHandle = setInterval(() => {
      if (anyActive) void loadRuns();
    }, 2000);
  });
  onDestroy(() => {
    if (pollHandle) clearInterval(pollHandle);
  });
</script>

<svelte:head><title>Search reindex — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-5xl px-6 py-8">
  <h1 class="font-display mb-2 text-2xl font-semibold">Search reindex</h1>
  <p class="mb-6 text-sm text-fg-muted">
    Rebuild search vectors + re-enqueue embeddings across scoped assets. Cancel-safe at batch boundaries;
    embed jobs already enqueued still run. Only one reindex active at a time.
  </p>

  <section class="mb-6 rounded border border-border bg-bg-soft p-4">
    <h2 class="mb-3 text-xs font-semibold uppercase tracking-wide text-fg-muted">Start a run</h2>
    <div class="flex flex-wrap items-end gap-3">
      <label class="flex flex-col gap-1 text-sm">
        <span class="text-fg-muted">Scope</span>
        <input
          bind:value={scope}
          placeholder="all, asset_type:&lt;uuid&gt;, collection:&lt;uuid&gt;, embedding_model:&lt;provider&gt;/&lt;model&gt;"
          class="w-96 rounded border border-border bg-bg p-2 text-fg"
          data-testid="scope"
        />
      </label>
      <label class="flex flex-col gap-1 text-sm">
        <span class="text-fg-muted">Target</span>
        <select bind:value={target} class="rounded border border-border bg-bg p-2 text-fg">
          <option value="both">Both</option>
          <option value="tsvector">Tsvector only</option>
          <option value="embedding">Embedding only</option>
        </select>
      </label>
      <button
        onclick={startRun}
        disabled={starting || anyActive}
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
        data-testid="start-reindex"
      >{starting ? 'Starting…' : anyActive ? 'Run active' : 'Start reindex'}</button>
      <button
        onclick={loadRuns}
        disabled={loading}
        class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg disabled:opacity-50"
      >{loading ? 'Loading…' : 'Refresh'}</button>
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
          <th class="px-3 py-2 text-left font-medium">Scope</th>
          <th class="px-3 py-2 text-left font-medium">Target</th>
          <th class="px-3 py-2 text-right font-medium">Processed</th>
          <th class="px-3 py-2 text-right font-medium">Succeeded</th>
          <th class="px-3 py-2 text-right font-medium">Failed</th>
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
            <td class="px-3 py-2 text-fg-muted">{scopeSummary(r)}</td>
            <td class="px-3 py-2 text-fg-muted">{r.target}</td>
            <td class="px-3 py-2 text-right tabular-nums">{r.processed}</td>
            <td class="px-3 py-2 text-right tabular-nums text-success">{r.succeeded}</td>
            <td class="px-3 py-2 text-right tabular-nums {r.failed > 0 ? 'text-danger' : 'text-fg-muted'}">{r.failed}</td>
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
            <td colspan="8" class="px-3 py-6 text-center text-fg-muted">No reindex runs yet.</td>
          </tr>
        {/if}
      </tbody>
    </table>
  </section>
</div>
