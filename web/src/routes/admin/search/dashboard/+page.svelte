<script lang="ts">
  // Admin: search dashboard (Phase 1.16.B-5).
  //
  // Groups the flat /admin/search/health JSON by subsystem at
  // render time (per pre-audit Q6 — backend JSON stays flat).

  import { onMount, onDestroy } from 'svelte';

  type Health = {
    subsystem: string;
    counter_total: number;
    by_result: Record<string, number>;
    notes: string[];
  };

  let health = $state<Health | null>(null);
  let loading = $state(false);
  let error = $state('');
  let pollHandle: ReturnType<typeof setInterval> | null = null;

  async function load() {
    loading = true;
    error = '';
    try {
      const r = await fetch('/api/v1/admin/search/health', { credentials: 'include' });
      if (!r.ok) {
        error = `load failed: ${r.status}`;
        return;
      }
      health = await r.json();
    } finally {
      loading = false;
    }
  }

  // Group flat by_result keys by subsystem prefix.
  function groupByResult(byResult: Record<string, number>) {
    const groups: Record<string, Record<string, number>> = {
      engine: {},
      vector: {},
      saved_search: {},
      by_image: {},
      other: {},
    };
    for (const [k, v] of Object.entries(byResult)) {
      if (k.startsWith('vector_') || k === 'similar_to_not_embedded') groups.vector[k] = v;
      else if (k.startsWith('saved_search_')) groups.saved_search[k] = v;
      else if (k.startsWith('by_image_')) groups.by_image[k] = v;
      else if (['hit', 'empty', 'error', 'cache_hit', 'cache_miss', 'rate_limited', 'bad_request', 'dsl_parse_error'].includes(k)) groups.engine[k] = v;
      else groups.other[k] = v;
    }
    return groups;
  }

  // Extract gauges + latency from notes[] "key=value" strings.
  function parseNotes(notes: string[]): Record<string, string> {
    const out: Record<string, string> = {};
    for (const n of notes) {
      const eq = n.indexOf('=');
      if (eq > 0) out[n.slice(0, eq)] = n.slice(eq + 1);
    }
    return out;
  }

  const grouped = $derived(health ? groupByResult(health.by_result) : null);
  const notes = $derived(health ? parseNotes(health.notes) : {});

  onMount(async () => {
    await load();
    pollHandle = setInterval(load, 15000);
  });
  onDestroy(() => {
    if (pollHandle) clearInterval(pollHandle);
  });
</script>

<svelte:head><title>Search dashboard — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-6xl px-6 py-8">
  <div class="mb-6 flex items-center justify-between gap-3">
    <div>
      <h1 class="font-display text-2xl font-semibold">Search dashboard</h1>
      <p class="text-sm text-fg-muted">
        Live counters + latency + gauges. Polls every 15s.
        <a href="/admin/search/disk-usage" class="text-accent hover:underline">Disk usage →</a>
        <a href="/admin/search/reindex" class="ml-2 text-accent hover:underline">Reindex →</a>
        <a href="/admin/saved-searches" class="ml-2 text-accent hover:underline">Saved searches →</a>
      </p>
    </div>
    <button
      onclick={load}
      disabled={loading}
      class="rounded border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong disabled:opacity-50"
    >{loading ? 'Loading…' : 'Refresh'}</button>
  </div>

  {#if error}
    <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
  {/if}

  {#if health && grouped}
    <div class="grid grid-cols-1 gap-4 lg:grid-cols-3">
      <!-- Engine card -->
      <section class="rounded border border-border bg-surface p-4">
        <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">Engine</h2>
        <div class="space-y-1 text-sm">
          <div class="flex justify-between"><span class="text-fg-muted">Total requests</span><span class="tabular-nums">{health.counter_total}</span></div>
          {#each Object.entries(grouped.engine) as [k, v] (k)}
            <div class="flex justify-between text-xs"><span class="text-fg-muted">{k}</span><span class="tabular-nums">{v}</span></div>
          {/each}
        </div>
        <div class="mt-3 border-t border-border pt-2 text-xs text-fg-muted">
          <div>p50 {notes.latency_p50_ms}ms · p95 {notes.latency_p95_ms}ms · p99 {notes.latency_p99_ms}ms</div>
          <div>{notes.sample_count} samples · uptime {notes.uptime_seconds}s</div>
        </div>
      </section>

      <!-- Vector card -->
      <section class="rounded border border-border bg-surface p-4">
        <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">Vector</h2>
        <div class="space-y-1 text-sm">
          {#each Object.entries(grouped.vector) as [k, v] (k)}
            <div class="flex justify-between text-xs"><span class="text-fg-muted">{k}</span><span class="tabular-nums">{v}</span></div>
          {/each}
          {#if Object.keys(grouped.vector).length === 0}
            <div class="text-xs text-fg-muted">No vector requests yet.</div>
          {/if}
        </div>
        <div class="mt-3 border-t border-border pt-2 text-xs text-fg-muted">
          {#if notes.assets_pending_embedding}<div>Pending embed: {notes.assets_pending_embedding}</div>{/if}
          {#if notes.asset_embedding_row_count}<div>Embedded rows: {notes.asset_embedding_row_count}</div>{/if}
        </div>
      </section>

      <!-- Saved-search card -->
      <section class="rounded border border-border bg-surface p-4">
        <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">Saved searches</h2>
        <div class="space-y-1 text-sm">
          {#each Object.entries(grouped.saved_search) as [k, v] (k)}
            <div class="flex justify-between text-xs"><span class="text-fg-muted">{k}</span><span class="tabular-nums">{v}</span></div>
          {/each}
        </div>
        <div class="mt-3 border-t border-border pt-2 text-xs text-fg-muted">
          {#if notes.saved_search_active}<div>Active rows: {notes.saved_search_active}</div>{/if}
          {#if notes.saved_search_rows}<div>Total rows: {notes.saved_search_rows}</div>{/if}
        </div>
      </section>

      <!-- Cache card -->
      <section class="rounded border border-border bg-surface p-4">
        <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">Cache</h2>
        <div class="space-y-1 text-sm">
          <div class="flex justify-between text-xs"><span class="text-fg-muted">entries</span><span class="tabular-nums">{notes.cache_entries ?? '0'}</span></div>
          <div class="flex justify-between text-xs"><span class="text-fg-muted">hits</span><span class="tabular-nums">{notes.cache_hits ?? '0'}</span></div>
          <div class="flex justify-between text-xs"><span class="text-fg-muted">misses</span><span class="tabular-nums">{notes.cache_misses ?? '0'}</span></div>
          <div class="flex justify-between text-xs"><span class="text-fg-muted">invalidations</span><span class="tabular-nums">{notes.cache_invalidations ?? '0'}</span></div>
        </div>
      </section>

      <!-- By-image reservation demand signal -->
      <section class="rounded border border-border bg-surface p-4">
        <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">By-image (reserved)</h2>
        <div class="space-y-1 text-sm">
          {#each Object.entries(grouped.by_image) as [k, v] (k)}
            <div class="flex justify-between text-xs"><span class="text-fg-muted">{k}</span><span class="tabular-nums">{v}</span></div>
          {/each}
          {#if Object.keys(grouped.by_image).length === 0}
            <div class="text-xs text-fg-muted">No reverse-image attempts yet.</div>
          {/if}
        </div>
        <p class="mt-3 text-xs text-fg-muted">
          Demand signal for the CLIP visual encoder sidecar (deferred).
        </p>
      </section>

      <!-- Reindex history -->
      <section class="rounded border border-border bg-surface p-4">
        <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">Reindex history</h2>
        <div class="text-sm">
          <div class="flex justify-between"><span class="text-fg-muted">Total runs</span><span class="tabular-nums">{notes.search_reindex_history_rows ?? '0'}</span></div>
        </div>
        <a
          href="/admin/search/reindex"
          class="mt-3 inline-block rounded border border-border bg-surface px-3 py-1 text-xs hover:border-border-strong"
        >Open reindex →</a>
      </section>
    </div>
  {:else if loading}
    <p class="text-sm text-fg-muted">Loading…</p>
  {/if}
</div>
