<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin metadata-extraction backfills — Phase 1.18.A-2 follow-up B.
  //
  // Operator surface for starting + monitoring re-extract sweeps.
  // The job walks the active image-asset population (optionally
  // scoped to one asset_type) and enqueues one metadata.extract
  // job per asset. Progress polls every 2s while any run is
  // unfinished; the table renders the last 20 runs.
  //
  // Phase 1.16.B-followup-2 (#186): rendering delegated to
  // AdminBackfillPanel. This file keeps the API integration + the
  // metadata-specific form (asset_type / file_extensions /
  // include_non_image) via the `controls` snippet; the panel owns
  // the header / start-controls layout / recent-runs table.

  import { onMount, onDestroy } from 'svelte';
  import { api } from '$api/client';
  import AdminBackfillPanel, {
    type BaseRun,
  } from '$components/admin/AdminBackfillPanel.svelte';

  type Run = BaseRun & {
    asset_type_ref?: number | null;
    started_by_user_ref?: number | null;
    // Metadata-extraction's backend surface returns `total` rather
    // than the visual-backfill-style `total_estimated`; we don't
    // need it in the panel columns today but keep it for future
    // progress-bar consolidation.
    total: number;
  };

  let runs = $state<Run[]>([]);
  let starting = $state(false);
  let loading = $state(false);
  let error = $state('');
  let assetTypeFilter = $state('');
  // Phase 1.18.A-3.B: comma-separated file extensions (cr2,nef,...)
  // + a "include non-image assets" gate for the PDF case. Empty
  // input = backend defaults preserved (image-only, every type).
  let fileExtensionsInput = $state('');
  let includeNonImage = $state(false);
  let pollHandle: ReturnType<typeof setInterval> | null = null;

  const anyActive = $derived(runs.some((r) => r.isActive));

  async function loadRuns() {
    loading = true;
    error = '';
    try {
      const r = await api.GET('/admin/metadata-extraction/backfills', {
        params: { query: { limit: 20 } },
      });
      if (r.error) {
        error = (r.error as { error?: string }).error || 'load failed';
        return;
      }
      const data = r.data as { items: Array<Omit<Run, 'isActive'>> };
      runs = (data.items || []).map(withIsActive);
    } finally {
      loading = false;
    }
  }

  function withIsActive(r: Omit<Run, 'isActive'>): Run {
    return { ...r, isActive: !r.completed_at && !r.cancelled_at };
  }

  async function startRun() {
    if (starting) return;
    starting = true;
    error = '';
    try {
      const body: {
        asset_type_ref?: number;
        file_extensions?: string[];
        include_non_image?: boolean;
      } = {};
      if (assetTypeFilter) {
        const n = Number(assetTypeFilter);
        if (!Number.isFinite(n)) {
          error = 'asset_type_ref must be a number';
          return;
        }
        body.asset_type_ref = n;
      }
      const exts = fileExtensionsInput
        .split(',')
        .map((s) => s.trim().toLowerCase().replace(/^\./, ''))
        .filter((s) => s.length > 0);
      if (exts.length > 0) body.file_extensions = exts;
      if (includeNonImage) body.include_non_image = true;
      const r = await api.POST('/admin/metadata-extraction/backfills', { body });
      if (r.error) {
        error = (r.error as { error?: string }).error || 'start failed';
        return;
      }
      await loadRuns();
    } finally {
      starting = false;
    }
  }

  async function cancelRun(run: BaseRun) {
    const resp = await api.POST(
      '/admin/metadata-extraction/backfills/{id}/cancel',
      { params: { path: { id: run.id } } },
    );
    if (resp.error) {
      alert((resp.error as { error?: string }).error || 'cancel failed');
      return;
    }
    await loadRuns();
  }

  function cancelConfirmMessage(run: BaseRun): string {
    return `Cancel run ${run.id.slice(0, 8)}…? In-flight extract children continue but no further assets are enqueued.`;
  }

  onMount(async () => {
    await loadRuns();
    // Poll every 2s while any run is unfinished. We don't try to be
    // clever — the runs table is small + the GET is cheap.
    pollHandle = setInterval(() => {
      if (anyActive) void loadRuns();
    }, 2000);
  });
  onDestroy(() => {
    if (pollHandle) clearInterval(pollHandle);
  });
</script>

<svelte:head><title>Metadata backfill — artist-alley</title></svelte:head>

<AdminBackfillPanel
  title="Metadata extraction backfill"
  {runs}
  {loading}
  {starting}
  {error}
  onStart={startRun}
  onRefresh={loadRuns}
  onCancel={cancelRun}
  {cancelConfirmMessage}
  startLabel="Start backfill"
  disableStartWhenActive={false}
  emptyText="No backfill runs yet."
  extraColumnCount={1}
>
  {#snippet headerDescription()}
    <p class="text-sm text-fg-muted">
      Re-extract metadata across every active image asset (or one
      asset type). The coordinator job walks the population in
      batches + enqueues one <code>metadata.extract</code> per
      eligible asset. Per-asset outcomes land in the
      <a class="text-accent hover:underline" href="/admin/metadata-extraction/failures">failures queue</a>
      + the per-process counter at <code>/admin/metadata-extraction/health</code>.
    </p>
  {/snippet}

  {#snippet controls()}
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Asset type ref (optional)</span>
      <input
        bind:value={assetTypeFilter}
        placeholder="leave blank for all types"
        class="w-64 rounded border border-border bg-bg p-2 text-fg"
      />
    </label>
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">File extensions (comma-separated)</span>
      <input
        bind:value={fileExtensionsInput}
        placeholder="e.g. cr2,nef,dng or pdf"
        class="w-64 rounded border border-border bg-bg p-2 text-fg"
      />
    </label>
    <label class="flex items-center gap-2 text-sm">
      <input type="checkbox" bind:checked={includeNonImage} />
      <span>Include non-image assets (PDF)</span>
    </label>
  {/snippet}

  {#snippet extraColumnHeaders()}
    <th class="px-3 py-2 text-left font-medium">Scope</th>
  {/snippet}
  {#snippet extraRowCells(r)}
    <td class="px-3 py-2 text-fg-muted">
      {#if (r as Run).asset_type_ref != null}
        asset_type={(r as Run).asset_type_ref}
      {:else}
        all
      {/if}
    </td>
  {/snippet}
</AdminBackfillPanel>
