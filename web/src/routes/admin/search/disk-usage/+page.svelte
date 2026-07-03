<script lang="ts">
  // Admin: search disk usage (Phase 1.16.B-5).
  import { onMount } from 'svelte';

  type Snapshot = {
    tsvector_bytes: Record<string, number>;
    embedding_table_bytes: number;
    embedding_index_bytes: number;
    assets_pending_embedding: number;
    asset_embedding_row_count: number;
    saved_search_rows: number;
    saved_search_active: number;
    search_reindex_history_rows: number;
    snapshot_at: string;
  };

  let snap = $state<Snapshot | null>(null);
  let loading = $state(false);
  let error = $state('');

  async function load(refresh = false) {
    loading = true;
    error = '';
    try {
      const url = refresh
        ? '/api/v1/admin/search/disk-usage?refresh=true'
        : '/api/v1/admin/search/disk-usage';
      const r = await fetch(url, { credentials: 'include' });
      if (!r.ok) {
        error = `load failed: ${r.status}`;
        return;
      }
      snap = await r.json();
    } finally {
      loading = false;
    }
  }

  function fmtBytes(n: number): string {
    if (n === 0) return '0';
    const kb = 1024, mb = kb * 1024, gb = mb * 1024;
    if (n >= gb) return (n / gb).toFixed(2) + ' GB';
    if (n >= mb) return (n / mb).toFixed(2) + ' MB';
    if (n >= kb) return (n / kb).toFixed(1) + ' KB';
    return n + ' B';
  }

  const totalTsvectorBytes = $derived(
    snap ? Object.values(snap.tsvector_bytes).reduce((a, b) => a + b, 0) : 0,
  );

  onMount(() => void load(false));
</script>

<svelte:head><title>Search disk usage — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-4xl px-6 py-8">
  <div class="mb-6 flex items-center justify-between gap-3">
    <div>
      <h1 class="font-display text-2xl font-semibold">Search disk usage</h1>
      <p class="text-sm text-fg-muted">
        Storage footprint of the search subsystem. Cached 30s — force-refresh to recompute.
      </p>
    </div>
    <button
      onclick={() => load(true)}
      disabled={loading}
      class="rounded border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong disabled:opacity-50"
      data-testid="refresh"
    >{loading ? 'Loading…' : 'Force refresh'}</button>
  </div>

  {#if error}
    <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
  {/if}

  {#if snap}
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
      <div class="rounded border border-border bg-surface p-4">
        <div class="text-xs uppercase tracking-wide text-fg-muted">Tsvector total</div>
        <div class="mt-1 text-xl font-semibold" data-testid="tsvector-total">{fmtBytes(totalTsvectorBytes)}</div>
        <div class="mt-2 text-xs text-fg-muted">
          {#each Object.entries(snap.tsvector_bytes) as [tbl, bytes] (tbl)}
            <div>{tbl}: {fmtBytes(bytes)}</div>
          {/each}
        </div>
      </div>
      <div class="rounded border border-border bg-surface p-4">
        <div class="text-xs uppercase tracking-wide text-fg-muted">Embedding table</div>
        <div class="mt-1 text-xl font-semibold">{fmtBytes(snap.embedding_table_bytes)}</div>
        <div class="mt-1 text-xs text-fg-muted">
          Index: {fmtBytes(snap.embedding_index_bytes)}
        </div>
      </div>
      <div class="rounded border border-border bg-surface p-4">
        <div class="text-xs uppercase tracking-wide text-fg-muted">Assets pending embed</div>
        <div class="mt-1 text-xl font-semibold">{snap.assets_pending_embedding.toLocaleString()}</div>
        <div class="mt-1 text-xs text-fg-muted">
          Embedded: {snap.asset_embedding_row_count.toLocaleString()}
        </div>
      </div>
      <div class="rounded border border-border bg-surface p-4">
        <div class="text-xs uppercase tracking-wide text-fg-muted">Saved searches</div>
        <div class="mt-1 text-xl font-semibold">{snap.saved_search_rows.toLocaleString()}</div>
        <div class="mt-1 text-xs text-fg-muted">
          Active: {snap.saved_search_active.toLocaleString()}
        </div>
      </div>
      <div class="rounded border border-border bg-surface p-4">
        <div class="text-xs uppercase tracking-wide text-fg-muted">Reindex history</div>
        <div class="mt-1 text-xl font-semibold">{snap.search_reindex_history_rows.toLocaleString()}</div>
        <div class="mt-1 text-xs text-fg-muted">Rows in search_reindex_run</div>
      </div>
    </div>

    <p class="mt-4 text-xs text-fg-muted">
      Snapshot at {new Date(snap.snapshot_at).toLocaleString()}.
    </p>
  {:else if loading}
    <p class="text-sm text-fg-muted">Loading…</p>
  {/if}
</div>
