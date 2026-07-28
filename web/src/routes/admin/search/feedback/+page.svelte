<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: search-feedback aggregation (Phase 1.16.B-5-followup,
  // closes #184).
  //
  // Two anonymized sections:
  //   1. Queries with most down-votes in the aggregation window.
  //   2. Under-ranked hits (thumbs-up from position > 5).
  //
  // Per-user log lives at /admin/search/feedback/audit/[user_ref];
  // this page has a jump-to-user form for the abuse-review flow.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { goto } from '$app/navigation';

  type TopQueryRow = {
    query_hash: string;
    dsl_query: string;
    total_votes: number;
    down_votes: number;
    down_vote_pct: number;
  };

  type UnderRankedRow = {
    hit_asset_id: string;
    query_hash: string;
    dsl_query: string;
    avg_position: number;
    up_votes: number;
    asset_title: string;
  };

  let topQueries = $state<TopQueryRow[]>([]);
  let underRanked = $state<UnderRankedRow[]>([]);
  let loading = $state(false);
  let error = $state('');
  let userRefInput = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      const resp = await fetch('/api/v1/admin/search/feedback', { credentials: 'include' });
      if (!resp.ok) {
        error = `load failed: ${resp.status}`;
        return;
      }
      const data = await resp.json();
      topQueries = data.top_queries ?? [];
      underRanked = data.under_ranked_hits ?? [];
    } finally {
      loading = false;
    }
  }

  function jumpToUser() {
    const trimmed = userRefInput.trim();
    if (!trimmed) return;
    goto(`/admin/search/feedback/audit/${encodeURIComponent(trimmed)}`);
  }

  onMount(load);
</script>

<svelte:head><title>Search feedback — {site.name}</title></svelte:head>

<div class="mx-auto w-full max-w-5xl px-6 py-8">
  <h1 class="font-display mb-2 text-2xl font-semibold">Search feedback</h1>
  <p class="mb-6 text-sm text-fg-muted">
    Anonymized aggregation of thumbs up/down submissions on search results. Down-voted queries + under-ranked
    hits are ranking-quality candidates for review.
  </p>

  {#if error}
    <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
  {/if}

  <section class="mb-6 rounded border border-border bg-bg-soft p-4">
    <h2 class="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">Jump to user (abuse review)</h2>
    <form class="flex flex-wrap items-end gap-3" onsubmit={(e) => { e.preventDefault(); jumpToUser(); }}>
      <label class="flex flex-col gap-1 text-sm">
        <span class="text-fg-muted">User ref</span>
        <input
          bind:value={userRefInput}
          type="number"
          min="1"
          placeholder="e.g. 42"
          class="w-40 rounded border border-border-strong bg-bg p-2 text-fg"
          data-testid="user-ref-input"
        />
      </label>
      <button
        type="submit"
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg"
      >View per-user log</button>
      <button
        type="button"
        onclick={load}
        disabled={loading}
        class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg disabled:opacity-50"
      >{loading ? 'Loading…' : 'Refresh'}</button>
    </form>
  </section>

  <section class="mb-6 rounded border border-border bg-bg-soft">
    <header class="border-b border-border px-3 py-2 text-sm font-medium text-fg-muted">
      Queries with most down-votes ({topQueries.length})
    </header>
    <table class="w-full text-sm">
      <thead class="border-b border-border bg-bg/60 text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">Query</th>
          <th class="px-3 py-2 text-right font-medium">Total votes</th>
          <th class="px-3 py-2 text-right font-medium">Down-votes</th>
          <th class="px-3 py-2 text-right font-medium">Down %</th>
        </tr>
      </thead>
      <tbody>
        {#each topQueries as row (row.query_hash)}
          <tr class="border-b border-border/40 last:border-0">
            <td class="px-3 py-2 font-mono text-xs">{row.dsl_query}</td>
            <td class="px-3 py-2 text-right tabular-nums">{row.total_votes}</td>
            <td class="px-3 py-2 text-right tabular-nums text-danger">{row.down_votes}</td>
            <td class="px-3 py-2 text-right tabular-nums text-fg-muted">{Math.round((row.down_vote_pct ?? 0) * 100)}%</td>
          </tr>
        {/each}
        {#if topQueries.length === 0 && !loading}
          <tr><td colspan="4" class="px-3 py-6 text-center text-fg-muted">No down-voted queries in the window yet.</td></tr>
        {/if}
      </tbody>
    </table>
  </section>

  <section class="rounded border border-border bg-bg-soft">
    <header class="border-b border-border px-3 py-2 text-sm font-medium text-fg-muted">
      Under-ranked hits ({underRanked.length})
      <span class="text-xs text-fg-muted"> — thumbs-up from average position &gt; 5</span>
    </header>
    <table class="w-full text-sm">
      <thead class="border-b border-border bg-bg/60 text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">Query</th>
          <th class="px-3 py-2 text-left font-medium">Asset</th>
          <th class="px-3 py-2 text-right font-medium">Avg position</th>
          <th class="px-3 py-2 text-right font-medium">Up-votes</th>
        </tr>
      </thead>
      <tbody>
        {#each underRanked as row (row.hit_asset_id + row.query_hash)}
          <tr class="border-b border-border/40 last:border-0">
            <td class="px-3 py-2 font-mono text-xs">{row.dsl_query}</td>
            <td class="px-3 py-2 text-fg">
              <a href={`/assets/${row.hit_asset_id}`} class="text-accent hover:underline">{row.asset_title || row.hit_asset_id.slice(0, 8)}</a>
            </td>
            <td class="px-3 py-2 text-right tabular-nums">{row.avg_position.toFixed(1)}</td>
            <td class="px-3 py-2 text-right tabular-nums text-success">{row.up_votes}</td>
          </tr>
        {/each}
        {#if underRanked.length === 0 && !loading}
          <tr><td colspan="4" class="px-3 py-6 text-center text-fg-muted">No under-ranked hits in the window yet.</td></tr>
        {/if}
      </tbody>
    </table>
  </section>
</div>
