<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin job kinds / per-type concurrency — v0.4.0 Sprint 1 (#401).
  //
  // Lists every registered job type with its per-type concurrency cap
  // (0 = pool default, no per-type limit). system.admin can edit a cap;
  // the value is persisted to system_config and read by the Pool at boot,
  // so it applies on the next restart — surfaced plainly here. Read-only
  // holders see the caps but no editors.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';

  type Kind = { type: string; cap: number };

  const canAct = $derived(auth.can('system.admin'));

  let rows = $state<Kind[]>([]);
  let appliesOnRestart = $state(true);
  let loading = $state(false);
  let error = $state('');
  let flash = $state('');

  // per-type edit buffer keyed by type
  let editing = $state<Record<string, string>>({});

  async function load() {
    loading = true;
    error = '';
    try {
      const r = await api.GET('/admin/jobs/concurrency');
      if (r.error) { error = (r.error as { error?: string }).error || 'load failed'; return; }
      const data = r.data as { items: Kind[]; applies_on_restart: boolean };
      rows = data.items;
      appliesOnRestart = data.applies_on_restart;
      editing = Object.fromEntries(rows.map(k => [k.type, String(k.cap)]));
    } finally { loading = false; }
  }

  async function save(k: Kind) {
    flash = '';
    error = '';
    const raw = editing[k.type];
    const cap = Number(raw);
    if (!Number.isInteger(cap) || cap < 0 || cap > 64) { error = `Cap for ${k.type} must be an integer 0–64.`; return; }
    const r = await api.PUT('/admin/jobs/concurrency/{type}', {
      params: { path: { type: k.type } },
      body: { cap }
    });
    if (r.error) { error = (r.error as { error?: string }).error || 'save failed'; return; }
    k.cap = cap;
    rows = rows;
    flash = `Set ${k.type} concurrency to ${cap === 0 ? 'pool default' : cap}. ${appliesOnRestart ? 'Applies after the next restart.' : ''}`;
  }

  function capLabel(cap: number): string { return cap === 0 ? 'pool default' : String(cap); }

  onMount(() => { void load(); });
</script>

<svelte:head><title>Job kinds — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Job kinds</h2>
  <p class="text-sm text-fg-muted">
    Registered job types and their per-type concurrency cap.
    A cap of <span class="font-mono">0</span> means no per-type limit — the type shares the pool's overall worker count.
  </p>
</header>

{#if canAct && appliesOnRestart}
  <div class="mb-3 rounded border border-warning/40 bg-warning-container px-3 py-2 text-sm text-on-warning-container">
    Concurrency caps are read by the worker pool at startup. Edits are saved immediately but <strong>apply after the next app restart</strong>.
  </div>
{/if}

{#if flash}<div class="mb-3 rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-on-success-container">{flash}</div>{/if}
{#if error}<div role="alert" class="mb-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>{/if}

<div class="overflow-x-auto rounded border border-border">
  <table class="w-full text-left text-sm">
    <thead class="bg-surface-elevated text-xs uppercase tracking-wide text-fg-muted">
      <tr>
        <th class="px-3 py-2">Type</th>
        <th class="px-3 py-2">Concurrency cap</th>
        {#if canAct}<th class="px-3 py-2 text-right">Edit</th>{/if}
      </tr>
    </thead>
    <tbody>
      {#each rows as k (k.type)}
        <tr class="border-t border-border">
          <td class="px-3 py-2 font-mono text-xs">{k.type}</td>
          <td class="px-3 py-2 tabular-nums">{capLabel(k.cap)}</td>
          {#if canAct}
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <input
                type="number" min="0" max="64" step="1"
                bind:value={editing[k.type]}
                class="w-20 rounded border border-border-strong bg-surface px-2 py-1 text-right text-sm"
                aria-label={`Concurrency cap for ${k.type}`} />
              <button onclick={() => save(k)} class="ml-1 rounded bg-accent px-2 py-1 text-xs font-medium text-on-accent" disabled={editing[k.type] === String(k.cap)}>Save</button>
            </td>
          {/if}
        </tr>
      {:else}
        <tr><td colspan={canAct ? 3 : 2} class="px-3 py-6 text-center text-fg-muted">{loading ? 'Loading…' : 'No registered job types.'}</td></tr>
      {/each}
    </tbody>
  </table>
</div>
