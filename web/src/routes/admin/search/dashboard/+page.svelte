<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: search dashboard (Phase 1.16.B-5).
  //
  // Groups the flat /admin/search/health JSON by subsystem at
  // render time (per pre-audit Q6 — backend JSON stays flat).

  import { onMount, onDestroy } from 'svelte';

  import { site } from '$stores/site.svelte';
  type Health = {
    subsystem: string;
    counter_total: number;
    by_result: Record<string, number>;
    notes: string[];
  };

  let health = $state<Health | null>(null);
  let iiifHealth = $state<Health | null>(null);
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
    // IIIF health is a separate endpoint — fetch alongside so the
    // dashboard surfaces both subsystems on one poll cycle. Silent
    // on failure; IIIF might not be enabled on every install.
    try {
      const r = await fetch('/api/v1/admin/iiif/health', { credentials: 'include' });
      if (r.ok) iiifHealth = await r.json();
    } catch { /* ignore */ }
  }

  // Group flat by_result keys by subsystem prefix.
  function groupByResult(byResult: Record<string, number>) {
    const groups: Record<string, Record<string, number>> = {
      engine: {},
      vector: {},
      saved_search: {},
      by_image: {},
      feedback: {},
      other: {},
    };
    for (const [k, v] of Object.entries(byResult)) {
      if (k.startsWith('vector_') || k === 'similar_to_not_embedded') groups.vector[k] = v;
      else if (k.startsWith('saved_search_')) groups.saved_search[k] = v;
      else if (k.startsWith('by_image_')) groups.by_image[k] = v;
      else if (k.startsWith('search_feedback_')) groups.feedback[k] = v;
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
  const iiifNotes = $derived(iiifHealth ? parseNotes(iiifHealth.notes) : {});
  const iiifManifestCounters = $derived(
    iiifHealth ? Object.entries(iiifHealth.by_result).filter(([k]) => k.startsWith('manifest_requests/')) : []
  );
  const iiifRedirectCounters = $derived(
    iiifHealth ? Object.entries(iiifHealth.by_result).filter(([k]) => k.startsWith('redirect_2to3/')) : []
  );
  const iiifContentSearchCounters = $derived(
    iiifHealth
      ? Object.entries(iiifHealth.by_result).filter(
          ([k]) => k.startsWith('content_search/') && !k.startsWith('content_search_hits/')
        )
      : []
  );
  // Extract the multi-line "note=..." hints (dashboard renders as bullets).
  const iiifHints = $derived(
    iiifHealth ? iiifHealth.notes.filter((n) => n.startsWith('note=')).map((n) => n.slice(5)) : []
  );

  onMount(async () => {
    await load();
    pollHandle = setInterval(load, 15000);
  });
  onDestroy(() => {
    if (pollHandle) clearInterval(pollHandle);
  });
</script>

<svelte:head><title>Search dashboard — {site.name}</title></svelte:head>

<div class="mx-auto w-full max-w-6xl px-6 py-8">
  <div class="mb-6 flex items-center justify-between gap-3">
    <div>
      <h1 class="font-display text-2xl font-semibold">Search dashboard</h1>
      <p class="text-sm text-fg-muted">
        Live counters + latency + gauges. Polls every 15s.
        <a href="/admin/search/disk-usage" class="text-accent hover:underline">Disk usage →</a>
        <a href="/admin/search/reindex" class="ml-2 text-accent hover:underline">Reindex →</a>
        <a href="/admin/search/visual-backfill" class="ml-2 text-accent hover:underline">Visual backfill →</a>
        <a href="/admin/search/feedback" class="ml-2 text-accent hover:underline">Feedback →</a>
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

      <!-- Visual search — CLIP visual encoder sidecar + reverse-image -->
      <section class="rounded border border-border bg-surface p-4">
        <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">Visual search</h2>
        <div class="space-y-1 text-sm">
          {#each Object.entries(grouped.by_image) as [k, v] (k)}
            <div class="flex justify-between text-xs"><span class="text-fg-muted">{k}</span><span class="tabular-nums">{v}</span></div>
          {/each}
          {#if Object.keys(grouped.by_image).length === 0}
            <div class="text-xs text-fg-muted">No reverse-image attempts yet.</div>
          {/if}
        </div>
        <div class="mt-3 border-t border-border pt-2 text-xs text-fg-muted">
          {#if notes.visual_embedding_total !== undefined}<div>Embedded: {notes.visual_embedding_total}</div>{/if}
          {#if notes.visual_embedding_backlog !== undefined}<div>Backlog: {notes.visual_embedding_backlog}</div>{/if}
          {#if notes.visual_backfill_active}<div class="text-info">Backfill running</div>{/if}
        </div>
        <a
          href="/admin/search/visual-backfill"
          class="mt-3 inline-block rounded border border-border bg-surface px-3 py-1 text-xs hover:border-border-strong"
        >Open backfill →</a>
      </section>

      <!-- Feedback — thumbs up/down on search results (Phase 1.16.B-5-followup) -->
      <section class="rounded border border-border bg-surface p-4">
        <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">Feedback</h2>
        <div class="space-y-1 text-sm">
          {#each Object.entries(grouped.feedback) as [k, v] (k)}
            <div class="flex justify-between text-xs"><span class="text-fg-muted">{k.replace('search_feedback_', '')}</span><span class="tabular-nums">{v}</span></div>
          {/each}
          {#if Object.keys(grouped.feedback).length === 0}
            <div class="text-xs text-fg-muted">No feedback submitted yet.</div>
          {/if}
        </div>
        <div class="mt-3 border-t border-border pt-2 text-xs text-fg-muted">
          {#if notes.search_feedback_active_voters !== undefined}<div>Active voters (window): {notes.search_feedback_active_voters}</div>{/if}
        </div>
        <a
          href="/admin/search/feedback"
          class="mt-3 inline-block rounded border border-border bg-surface px-3 py-1 text-xs hover:border-border-strong"
        >Open feedback →</a>
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

      <!-- IIIF subsystem (Phase 1.54.B) -->
      {#if iiifHealth}
        <section class="rounded border border-border bg-surface p-4 lg:col-span-3">
          <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-fg-muted">
            IIIF · manifests + content search + tile API
          </h2>
          <div class="grid grid-cols-1 gap-3 md:grid-cols-3">
            <div class="text-sm">
              <div class="mb-1 text-xs font-semibold text-fg-muted">Manifests</div>
              <div class="flex justify-between text-xs">
                <span class="text-fg-muted">total requests</span>
                <span class="tabular-nums">{iiifHealth.counter_total}</span>
              </div>
              {#each iiifManifestCounters as [k, v] (k)}
                <div class="flex justify-between text-xs">
                  <span class="text-fg-muted">{k.replace('manifest_requests/', '')}</span>
                  <span class="tabular-nums">{v}</span>
                </div>
              {/each}
              <div class="mt-2 border-t border-border pt-1 text-xs text-fg-muted">
                p50 {iiifNotes.manifest_latency_p50_ms}ms · p95 {iiifNotes.manifest_latency_p95_ms}ms · p99 {iiifNotes.manifest_latency_p99_ms}ms
              </div>
              <div class="text-xs text-fg-muted">
                cache hit ratio {iiifNotes.cache_hit_ratio ?? '0.000'}
              </div>
            </div>
            <div class="text-sm">
              <div class="mb-1 text-xs font-semibold text-fg-muted">Content Search</div>
              {#each iiifContentSearchCounters as [k, v] (k)}
                <div class="flex justify-between text-xs">
                  <span class="text-fg-muted">{k.replace('content_search/', '')}</span>
                  <span class="tabular-nums">{v}</span>
                </div>
              {/each}
              {#if iiifContentSearchCounters.length === 0}
                <div class="text-xs text-fg-muted">No content-search requests yet.</div>
              {/if}
              <div class="mt-2 border-t border-border pt-1 text-xs text-fg-muted">
                p50 {iiifNotes.content_search_latency_p50_ms}ms · p95 {iiifNotes.content_search_latency_p95_ms}ms · p99 {iiifNotes.content_search_latency_p99_ms}ms
              </div>
            </div>
            <div class="text-sm">
              <div class="mb-1 text-xs font-semibold text-fg-muted">Legacy 2.0 redirects</div>
              {#each iiifRedirectCounters as [k, v] (k)}
                <div class="flex justify-between text-xs">
                  <span class="text-fg-muted">{k.replace('redirect_2to3/', '')}</span>
                  <span class="tabular-nums">{v}</span>
                </div>
              {/each}
              {#if iiifRedirectCounters.length === 0}
                <div class="text-xs text-fg-muted">No legacy-URL traffic.</div>
              {/if}
              <div class="mt-2 border-t border-border pt-1 text-xs text-fg-muted">
                federated canvases served: <span class="tabular-nums">{iiifHealth.by_result['federated_canvas/served'] ?? 0}</span>
              </div>
            </div>
          </div>
          {#if iiifHints.length > 0}
            <ul class="mt-3 border-t border-border pt-2 text-xs text-fg-muted">
              {#each iiifHints as hint (hint)}
                <li class="mt-1">• {hint}</li>
              {/each}
            </ul>
          {/if}
        </section>
      {/if}
    </div>
  {:else if loading}
    <p class="text-sm text-fg-muted">Loading…</p>
  {/if}
</div>
