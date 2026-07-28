<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: saved-searches management (Phase 1.16.B-5).
  //
  // Lists all users' rows. Filters by owner_user_ref + has_failure.
  // Per-row: delete, dismiss-error.

  import { onMount } from 'svelte';

  import { site } from '$stores/site.svelte';
  type Row = {
    id: string;
    owner_user_ref: number;
    name: string;
    dsl: string;
    notify_channel: 'email' | 'none';
    notify_interval_minutes: number;
    enabled: boolean;
    last_run_at?: string;
    last_notified_at?: string;
    last_hit_count?: number;
    created_at: string;
    updated_at: string;
  };

  let rows = $state<Row[]>([]);
  let loading = $state(false);
  let error = $state('');
  let ownerFilter = $state('');
  let onlyFailing = $state(false);

  async function load() {
    loading = true;
    error = '';
    try {
      const params = new URLSearchParams({ limit: '100' });
      if (ownerFilter) params.set('owner_user_ref', ownerFilter);
      if (onlyFailing) params.set('has_failure', 'true');
      const r = await fetch(`/api/v1/admin/saved-searches?${params.toString()}`, { credentials: 'include' });
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

  async function deleteRow(row: Row) {
    if (!confirm(`Delete "${row.name}" (owner ${row.owner_user_ref})?`)) return;
    const r = await fetch(`/api/v1/admin/saved-searches/${row.id}`, {
      method: 'DELETE',
      credentials: 'include',
    });
    if (r.ok) void load();
  }

  async function dismissError(row: Row) {
    const r = await fetch(`/api/v1/admin/saved-searches/${row.id}/dismiss-error`, {
      method: 'POST',
      credentials: 'include',
    });
    if (r.ok) void load();
  }

  function relTime(s?: string): string {
    if (!s) return '—';
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

<svelte:head><title>Admin: saved searches — {site.name}</title></svelte:head>

<div class="mx-auto w-full max-w-6xl px-6 py-8">
  <h1 class="font-display mb-2 text-2xl font-semibold">Saved searches (admin)</h1>
  <p class="mb-6 text-sm text-fg-muted">
    Every user's saved search. Filter by owner or by failing rows. Delete is permanent;
    dismiss-error pauses a row so it stops surfacing in the failures view.
  </p>

  {#if error}
    <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
  {/if}

  <div class="mb-4 flex flex-wrap items-end gap-3">
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Owner user_ref</span>
      <input
        bind:value={ownerFilter}
        placeholder="leave blank for all"
        class="w-48 rounded border border-border-strong bg-bg p-2 text-fg"
      />
    </label>
    <label class="flex items-center gap-2 text-sm">
      <input type="checkbox" bind:checked={onlyFailing} />
      <span>Only failing</span>
    </label>
    <button
      onclick={load}
      disabled={loading}
      class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
    >{loading ? 'Loading…' : 'Filter'}</button>
    <a
      href="/admin/saved-searches/failures"
      class="rounded border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
    >Failures view</a>
  </div>

  <div class="rounded border border-border bg-bg-soft">
    <table class="w-full text-sm">
      <thead class="border-b border-border bg-bg/60 text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">Owner</th>
          <th class="px-3 py-2 text-left font-medium">Name</th>
          <th class="px-3 py-2 text-left font-medium">DSL</th>
          <th class="px-3 py-2 text-left font-medium">Every</th>
          <th class="px-3 py-2 text-left font-medium">Last run</th>
          <th class="px-3 py-2 text-left font-medium">State</th>
          <th class="px-3 py-2 text-right font-medium">Actions</th>
        </tr>
      </thead>
      <tbody>
        {#each rows as r (r.id)}
          <tr class="border-b border-border/40 last:border-0">
            <td class="px-3 py-2 tabular-nums">{r.owner_user_ref}</td>
            <td class="px-3 py-2 font-medium">{r.name}</td>
            <td class="px-3 py-2 truncate font-mono text-xs text-fg-muted" title={r.dsl}>{r.dsl}</td>
            <td class="px-3 py-2 text-fg-muted">{r.notify_interval_minutes}m</td>
            <td class="px-3 py-2 text-fg-muted">{relTime(r.last_run_at)}</td>
            <td class="px-3 py-2">
              {#if !r.enabled}
                <span class="rounded bg-fg-muted/15 px-1.5 py-0.5 text-[10px] uppercase text-fg-muted">paused</span>
              {:else}
                <span class="rounded bg-success/15 px-1.5 py-0.5 text-[10px] uppercase text-success">enabled</span>
              {/if}
            </td>
            <td class="px-3 py-2 text-right">
              <button
                onclick={() => dismissError(r)}
                class="rounded border border-border bg-surface px-2 py-1 text-xs hover:border-border-strong"
              >Dismiss</button>
              <button
                onclick={() => deleteRow(r)}
                class="ml-1 rounded border border-danger/60 px-2 py-1 text-xs text-danger hover:bg-danger hover:text-on-danger"
              >Delete</button>
            </td>
          </tr>
        {/each}
        {#if rows.length === 0 && !loading}
          <tr>
            <td colspan="7" class="px-3 py-6 text-center text-fg-muted">No saved searches match the filter.</td>
          </tr>
        {/if}
      </tbody>
    </table>
  </div>
</div>
