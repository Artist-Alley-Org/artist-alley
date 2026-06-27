<script lang="ts">
  // Admin metadata-extraction backfills — Phase 1.18.A-2 follow-up B.
  //
  // Operator surface for starting + monitoring re-extract sweeps.
  // The job walks the active image-asset population (optionally
  // scoped to one asset_type) and enqueues one metadata.extract
  // job per asset. Progress polls every 2s while any run is
  // unfinished; the table renders the last 20 runs.

  import { onMount, onDestroy } from 'svelte';
  import { api } from '$api/client';

  type Run = {
    id: string;
    asset_type_ref?: number | null;
    total: number;
    processed: number;
    succeeded: number;
    failed: number;
    started_at: string;
    completed_at?: string | null;
    cancelled_at?: string | null;
    started_by_user_ref?: number | null;
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
      const data = r.data as { items: Run[] };
      runs = data.items || [];
    } finally {
      loading = false;
    }
  }

  function isActive(r: Run): boolean {
    return !r.completed_at && !r.cancelled_at;
  }
  const anyActive = $derived(runs.some(isActive));

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

  async function cancelRun(r: Run) {
    if (!confirm(`Cancel run ${r.id.slice(0, 8)}…? In-flight extract children continue but no further assets are enqueued.`)) return;
    const resp = await api.POST('/admin/metadata-extraction/backfills/{id}/cancel', {
      params: { path: { id: r.id } },
    });
    if (resp.error) {
      alert((resp.error as { error?: string }).error || 'cancel failed');
      return;
    }
    await loadRuns();
  }

  function relTime(s: string): string {
    const d = new Date(s);
    const ms = Date.now() - d.getTime();
    if (ms < 0) return 'in ' + relUnits(-ms);
    return relUnits(ms) + ' ago';
  }
  function relUnits(ms: number): string {
    const s = Math.floor(ms / 1000);
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h`;
    return `${Math.floor(h / 24)}d`;
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

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Metadata extraction backfill</h2>
  <p class="text-sm text-fg-muted">
    Re-extract metadata across every active image asset (or one
    asset type). The coordinator job walks the population in
    batches + enqueues one <code>metadata.extract</code> per
    eligible asset. Per-asset outcomes land in the
    <a class="text-accent hover:underline" href="/admin/metadata-extraction/failures">failures queue</a>
    + the per-process counter at <code>/admin/metadata-extraction/health</code>.
  </p>
</header>

<section class="mb-6 rounded border border-border bg-bg-soft p-4">
  <h3 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">Start a run</h3>
  <div class="flex flex-wrap items-end gap-3">
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
    <button
      onclick={startRun}
      disabled={starting}
      class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
    >{starting ? 'Starting…' : 'Start backfill'}</button>
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
          <td class="px-3 py-2 text-fg-muted">
            {#if r.asset_type_ref != null}
              asset_type={r.asset_type_ref}
            {:else}
              all
            {/if}
          </td>
          <td class="px-3 py-2 text-right tabular-nums">{r.processed}</td>
          <td class="px-3 py-2 text-right tabular-nums text-success">{r.succeeded}</td>
          <td class="px-3 py-2 text-right tabular-nums {r.failed > 0 ? 'text-danger' : 'text-fg-muted'}">{r.failed}</td>
          <td class="px-3 py-2 text-right">
            {#if isActive(r)}
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
          <td colspan="7" class="px-3 py-6 text-center text-fg-muted">No backfill runs yet.</td>
        </tr>
      {/if}
    </tbody>
  </table>
</section>
