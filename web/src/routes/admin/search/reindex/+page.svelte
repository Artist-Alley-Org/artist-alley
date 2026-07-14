<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: search reindex (Phase 1.16.B-5).
  //
  // Phase 1.16.B-followup-2 (#186): rendering delegated to
  // AdminBackfillPanel. This file keeps the API integration + the
  // reindex-specific form (scope grammar + target dropdown) via the
  // `controls` snippet.

  import { onMount, onDestroy } from 'svelte';
  import { site } from '$stores/site.svelte';
  import AdminBackfillPanel, {
    type BaseRun,
  } from '$components/admin/AdminBackfillPanel.svelte';

  type Run = BaseRun & {
    scope: Record<string, unknown>;
    target: 'tsvector' | 'embedding' | 'both';
    is_active: boolean;
    total_estimated?: number | null;
    started_by_user_ref?: number | null;
  };

  let runs = $state<Run[]>([]);
  let starting = $state(false);
  let loading = $state(false);
  let error = $state('');
  let scope = $state('all');
  let target = $state<'tsvector' | 'embedding' | 'both'>('both');
  let pollHandle: ReturnType<typeof setInterval> | null = null;

  const anyActive = $derived(runs.some((r) => r.isActive));

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
      runs = (data.items ?? []).map((row: Omit<Run, 'isActive'>) => ({
        ...row,
        isActive: row.is_active,
      }));
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

  async function cancelRun(run: BaseRun) {
    const resp = await fetch(`/api/v1/admin/search/reindex/runs/${run.id}/cancel`, {
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
    return `Cancel reindex ${run.id.slice(0, 8)}…? Embed jobs already enqueued still run.`;
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

<svelte:head><title>Search reindex — {site.name}</title></svelte:head>

<div class="mx-auto w-full max-w-5xl px-6 py-8">
  <AdminBackfillPanel
    title="Search reindex"
    {runs}
    {loading}
    {starting}
    {error}
    onStart={startRun}
    onRefresh={loadRuns}
    onCancel={cancelRun}
    {cancelConfirmMessage}
    startLabel="Start reindex"
    emptyText="No reindex runs yet."
    extraColumnCount={2}
  >
    {#snippet headerDescription()}
      <p class="text-sm text-fg-muted">
        Rebuild search vectors + re-enqueue embeddings across scoped assets. Cancel-safe at batch boundaries;
        embed jobs already enqueued still run. Only one reindex active at a time.
      </p>
    {/snippet}

    {#snippet controls()}
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
    {/snippet}

    {#snippet extraColumnHeaders()}
      <th class="px-3 py-2 text-left font-medium">Scope</th>
      <th class="px-3 py-2 text-left font-medium">Target</th>
    {/snippet}
    {#snippet extraRowCells(r)}
      <td class="px-3 py-2 text-fg-muted">{scopeSummary(r as Run)}</td>
      <td class="px-3 py-2 text-fg-muted">{(r as Run).target}</td>
    {/snippet}
  </AdminBackfillPanel>
</div>
