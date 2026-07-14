<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/saved-searches — Phase 1.16.B-4 management page.
  //
  // Lists the caller's saved searches with per-row Run-now,
  // Enable/Disable, Edit-interval, and Delete actions. Powers the
  // "click through from the digest email" landing surface too.

  import { onMount } from 'svelte';

  import { site } from '$stores/site.svelte';
  type SavedSearch = {
    id: string;
    name: string;
    dsl: string;
    notify_channel: 'email' | 'none';
    notify_interval_minutes: number;
    enabled: boolean;
    created_at: string;
    updated_at: string;
    last_run_at?: string;
    last_notified_at?: string;
    last_hit_count?: number;
  };

  let rows = $state<SavedSearch[]>([]);
  let loading = $state(true);
  let error = $state('');
  let runningID = $state<string | null>(null);
  let runResult = $state<Record<string, string>>({});

  async function load() {
    loading = true;
    error = '';
    try {
      const resp = await fetch('/api/v1/search/saved', { credentials: 'include' });
      if (!resp.ok) {
        error = `load failed: ${resp.status}`;
        return;
      }
      const data = await resp.json();
      rows = data.items ?? [];
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  async function runNow(id: string) {
    runningID = id;
    try {
      const resp = await fetch(`/api/v1/search/saved/${id}/run-now`, {
        method: 'POST',
        credentials: 'include',
      });
      if (!resp.ok) {
        runResult[id] = `run failed: ${resp.status}`;
        return;
      }
      const data = await resp.json();
      runResult[id] = `${data.hit_count} hits · ${data.added_count} new · ${data.notified ? 'emailed' : 'no email'}`;
      runResult = { ...runResult };
      void load();
    } catch (e) {
      runResult[id] = e instanceof Error ? e.message : String(e);
    } finally {
      runningID = null;
    }
  }

  async function toggleEnabled(row: SavedSearch) {
    const resp = await fetch(`/api/v1/search/saved/${row.id}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'include',
      body: JSON.stringify({ enabled: !row.enabled }),
    });
    if (resp.ok) void load();
  }

  async function deleteRow(row: SavedSearch) {
    if (!confirm(`Delete "${row.name}"?`)) return;
    const resp = await fetch(`/api/v1/search/saved/${row.id}`, {
      method: 'DELETE',
      credentials: 'include',
    });
    if (resp.ok) void load();
  }

  function relTime(s?: string): string {
    if (!s) return 'never';
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

<svelte:head><title>Saved searches — {site.name}</title></svelte:head>

<div class="mx-auto w-full max-w-4xl px-6 py-8">
  <h1 class="font-display mb-2 text-2xl font-semibold">Saved searches</h1>
  <p class="mb-6 text-sm text-fg-muted">
    Each saved search re-runs on its interval. When new hits appear, you receive an email digest with links back to
    the results.
  </p>

  {#if error}
    <div role="alert" class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
  {/if}

  {#if loading}
    <p class="text-sm text-fg-muted">Loading…</p>
  {:else if rows.length === 0}
    <p class="text-sm text-fg-muted">
      You haven't saved any searches yet. From <a class="text-accent hover:underline" href="/search">a results page</a>,
      hit <strong>Save search</strong> to start tracking one.
    </p>
  {:else}
    <ul class="space-y-3">
      {#each rows as r (r.id)}
        <li class="rounded-md border border-border bg-surface p-4">
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="mb-1 flex items-center gap-2">
                <h2 class="text-base font-semibold text-fg">{r.name}</h2>
                {#if !r.enabled}
                  <span class="rounded bg-fg-muted/20 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-fg-muted">Paused</span>
                {/if}
              </div>
              <p class="mb-1 truncate font-mono text-xs text-fg-muted" title={r.dsl}>{r.dsl}</p>
              <p class="text-xs text-fg-muted">
                Every {r.notify_interval_minutes} min · {r.notify_channel === 'email' ? 'email digest' : 'track only'}
                {#if r.last_run_at} · ran {relTime(r.last_run_at)}{/if}
                {#if r.last_hit_count != null} · {r.last_hit_count} hits last run{/if}
              </p>
              {#if runResult[r.id]}
                <p class="mt-1 text-xs text-fg-muted" data-testid="run-result">{runResult[r.id]}</p>
              {/if}
            </div>
            <div class="flex shrink-0 gap-1">
              <button
                type="button"
                onclick={() => runNow(r.id)}
                disabled={runningID === r.id}
                class="rounded border border-border bg-surface px-2 py-1 text-xs hover:border-border-strong disabled:opacity-50"
                data-testid="run-now"
              >{runningID === r.id ? 'Running…' : 'Run now'}</button>
              <button
                type="button"
                onclick={() => toggleEnabled(r)}
                class="rounded border border-border bg-surface px-2 py-1 text-xs hover:border-border-strong"
              >{r.enabled ? 'Pause' : 'Resume'}</button>
              <button
                type="button"
                onclick={() => deleteRow(r)}
                class="rounded border border-danger/60 px-2 py-1 text-xs text-danger hover:bg-danger hover:text-on-danger"
              >Delete</button>
            </div>
          </div>
        </li>
      {/each}
    </ul>
  {/if}
</div>
