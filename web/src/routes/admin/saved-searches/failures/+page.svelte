<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: saved-search failures queue (Phase 1.16.B-5).
  //
  // Reuses the has_failure=true filter on the admin endpoint. A
  // row is "failing" when it's enabled + hasn't been run in
  // 2× its own interval (coordinator missed it, or the row was
  // enabled long enough to have run but hasn't).

  import { onMount } from 'svelte';

  type Row = {
    id: string;
    owner_user_ref: number;
    name: string;
    dsl: string;
    notify_interval_minutes: number;
    enabled: boolean;
    last_run_at?: string;
  };

  let rows = $state<Row[]>([]);
  let loading = $state(false);
  let error = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      const r = await fetch('/api/v1/admin/saved-searches?has_failure=true&limit=100', { credentials: 'include' });
      if (!r.ok) {
        error = `load failed: ${r.status}`;
        return;
      }
      const data = await r.json();
      rows = data.items ?? [];
    } finally {
      loading = false;
    }
  }

  async function dismiss(row: Row) {
    const r = await fetch(`/api/v1/admin/saved-searches/${row.id}/dismiss-error`, {
      method: 'POST',
      credentials: 'include',
    });
    if (r.ok) void load();
  }

  async function retryNow(row: Row) {
    // Coordinator picks up any row past its next-run threshold on
    // the next 60s tick — reset last_run_at via PATCH so the row
    // becomes due immediately.
    const r = await fetch(`/api/v1/search/saved/${row.id}/run-now`, {
      method: 'POST',
      credentials: 'include',
    });
    if (r.ok) void load();
  }

  onMount(load);
</script>

<svelte:head><title>Saved-search failures — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-4xl px-6 py-8">
  <div class="mb-6 flex items-center justify-between gap-3">
    <div>
      <h1 class="font-display text-2xl font-semibold">Saved-search failures</h1>
      <p class="text-sm text-fg-muted">
        Rows the coordinator missed — enabled + no run in 2× the interval. Dismiss pauses; retry runs now.
      </p>
    </div>
    <a href="/admin/saved-searches" class="text-sm text-accent hover:underline">All saved searches →</a>
  </div>

  {#if error}
    <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
  {/if}

  {#if loading}
    <p class="text-sm text-fg-muted">Loading…</p>
  {:else if rows.length === 0}
    <p class="text-sm text-fg-muted" data-testid="failures-empty">No failing saved searches. 🎉</p>
  {:else}
    <ul class="space-y-3">
      {#each rows as r (r.id)}
        <li class="rounded border border-warning/40 bg-warning/5 p-4">
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="mb-1 flex items-center gap-2">
                <span class="text-xs text-fg-muted">Owner {r.owner_user_ref}</span>
                <h2 class="text-base font-semibold text-fg">{r.name}</h2>
              </div>
              <p class="mb-1 truncate font-mono text-xs text-fg-muted" title={r.dsl}>{r.dsl}</p>
              <p class="text-xs text-fg-muted">
                Every {r.notify_interval_minutes}m · last run: {r.last_run_at ?? 'never'}
              </p>
            </div>
            <div class="flex shrink-0 gap-1">
              <button
                onclick={() => retryNow(r)}
                class="rounded border border-border bg-surface px-2 py-1 text-xs hover:border-border-strong"
              >Retry now</button>
              <button
                onclick={() => dismiss(r)}
                class="rounded border border-border bg-surface px-2 py-1 text-xs hover:border-border-strong"
              >Dismiss</button>
            </div>
          </div>
        </li>
      {/each}
    </ul>
  {/if}
</div>
