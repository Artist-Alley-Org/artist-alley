<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: per-user feedback abuse-review page (Phase 1.16.B-5-followup).
  //
  // Access to this page emits an admin.search.feedback.audit_viewed
  // event on the backend so operators can see who's browsing whom.

  import { onMount } from 'svelte';
  import { page } from '$app/state';

  type PerUserRow = {
    id: string;
    query_hash: string;
    dsl_query: string;
    hit_asset_id: string;
    hit_position: number;
    direction: 'up' | 'down';
    feedback_at: string;
    asset_title: string;
    ip_hash?: string;
  };

  let userRef = $derived(page.params.user_ref ?? '');
  let rows = $state<PerUserRow[]>([]);
  let loading = $state(false);
  let error = $state('');

  async function load() {
    if (!userRef) return;
    loading = true;
    error = '';
    try {
      const resp = await fetch(`/api/v1/admin/search/feedback/audit/${encodeURIComponent(userRef)}?limit=200`, {
        credentials: 'include',
      });
      if (!resp.ok) {
        error = `load failed: ${resp.status}`;
        return;
      }
      const data = await resp.json();
      rows = data.items ?? [];
    } finally {
      loading = false;
    }
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

  onMount(load);
</script>

<svelte:head><title>Feedback audit — user {userRef}</title></svelte:head>

<div class="mx-auto w-full max-w-5xl px-6 py-8">
  <h1 class="font-display mb-2 text-2xl font-semibold">Per-user feedback log</h1>
  <p class="mb-6 text-sm text-fg-muted">
    User ref: <span class="font-mono">{userRef}</span>.
    Your access to this page is audit-logged. Aggregation view lives at
    <a href="/admin/search/feedback" class="text-accent hover:underline">/admin/search/feedback</a>.
  </p>

  {#if error}
    <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
  {/if}

  <section class="rounded border border-border bg-bg-soft">
    <header class="border-b border-border px-3 py-2 text-sm font-medium text-fg-muted">
      Feedback rows ({rows.length})
    </header>
    <table class="w-full text-sm">
      <thead class="border-b border-border bg-bg/60 text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">When</th>
          <th class="px-3 py-2 text-left font-medium">Direction</th>
          <th class="px-3 py-2 text-left font-medium">Query</th>
          <th class="px-3 py-2 text-left font-medium">Asset</th>
          <th class="px-3 py-2 text-right font-medium">Position</th>
          <th class="px-3 py-2 text-left font-medium">IP class</th>
        </tr>
      </thead>
      <tbody>
        {#each rows as row (row.id)}
          <tr class="border-b border-border/40 last:border-0">
            <td class="px-3 py-2 text-fg-muted" title={row.feedback_at}>{relTime(row.feedback_at)}</td>
            <td class="px-3 py-2">
              <span class="inline-flex items-center rounded px-2 py-0.5 text-xs {row.direction === 'up' ? 'bg-success/15 text-success' : 'bg-danger/15 text-danger'}">{row.direction}</span>
            </td>
            <td class="px-3 py-2 font-mono text-xs">{row.dsl_query}</td>
            <td class="px-3 py-2">
              <a href={`/assets/${row.hit_asset_id}`} class="text-accent hover:underline">{row.asset_title || row.hit_asset_id.slice(0, 8)}</a>
            </td>
            <td class="px-3 py-2 text-right tabular-nums">{row.hit_position}</td>
            <td class="px-3 py-2 font-mono text-xs text-fg-muted">{row.ip_hash ?? '—'}</td>
          </tr>
        {/each}
        {#if rows.length === 0 && !loading}
          <tr><td colspan="6" class="px-3 py-6 text-center text-fg-muted">No feedback rows for this user.</td></tr>
        {/if}
      </tbody>
    </table>
  </section>
</div>
