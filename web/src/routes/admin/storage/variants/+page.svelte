<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin variant inventory — v0.4.0 Sprint 2 (#402).
  //
  // Read-only, grouped by variant FAMILY rather than raw variant_key.
  // variant_key is high-cardinality (one key per HLS segment and per
  // turntable frame — 2090 distinct on a dev install), so the raw grain
  // would be thousands of unreadable rows; the family grain is ~12.
  // Gated on system.storage.read (or system.admin).

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';

  type Family = {
    family: string;
    variant_count: number;
    distinct_keys: number;
    object_count: number;
    total_bytes: number;
    newest_at?: string | null;
  };

  let rows = $state<Family[]>([]);
  let loading = $state(true);
  let error = $state('');

  const totalBytes = $derived(rows.reduce((a, r) => a + r.total_bytes, 0));

  async function load() {
    loading = true;
    error = '';
    try {
      const r = await api.GET('/admin/storage/variants');
      if (r.error) { error = (r.error as { error?: string }).error || 'load failed'; return; }
      rows = (r.data as { items: Family[] }).items;
    } finally { loading = false; }
  }

  function bytes(n: number): string {
    if (n < 1024) return `${n} B`;
    const units = ['KiB', 'MiB', 'GiB', 'TiB'];
    let v = n / 1024, i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
  }
  function num(n: number): string { return n.toLocaleString(); }
  function pct(n: number): number { return totalBytes > 0 ? (n / totalBytes) * 100 : 0; }
  function fmt(iso?: string | null): string { return iso ? new Date(iso).toLocaleDateString() : '—'; }

  onMount(() => { void load(); });
</script>

<svelte:head><title>Variant families — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Variant families</h2>
  <p class="text-sm text-fg-muted">
    Derivatives grouped by family — the part of the variant key before the first
    <span class="font-mono">/</span> (so <span class="font-mono">turntable/0028.png</span> counts under
    <span class="font-mono">turntable</span>). Largest first.
    <a class="text-accent hover:underline" href="/admin/storage/usage">Usage summary</a>
  </p>
</header>

{#if error}<div role="alert" class="mb-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>{/if}

<div class="mb-2 text-sm text-fg-muted">{rows.length} families · {bytes(totalBytes)} total</div>

<div class="overflow-x-auto rounded border border-border">
  <table class="w-full text-left text-sm">
    <thead class="bg-surface-elevated text-xs uppercase tracking-wide text-fg-muted">
      <tr>
        <th class="px-3 py-2">Family</th>
        <th class="px-3 py-2 text-right">Size</th>
        <th class="px-3 py-2">Share</th>
        <th class="px-3 py-2 text-right">Variants</th>
        <th class="px-3 py-2 text-right">Keys</th>
        <th class="px-3 py-2 text-right">Objects</th>
        <th class="px-3 py-2 text-right">Newest</th>
      </tr>
    </thead>
    <tbody>
      {#each rows as f (f.family)}
        <tr class="border-t border-border">
          <td class="px-3 py-2 font-mono text-xs">{f.family}</td>
          <td class="px-3 py-2 text-right tabular-nums">{bytes(f.total_bytes)}</td>
          <td class="px-3 py-2">
            <div class="flex items-center gap-2">
              <div class="h-1.5 w-24 shrink-0 overflow-hidden rounded-full bg-surface-elevated">
                <div class="h-full rounded-full bg-accent" style={`width:${pct(f.total_bytes).toFixed(1)}%`}></div>
              </div>
              <span class="text-xs tabular-nums text-fg-muted">{pct(f.total_bytes).toFixed(1)}%</span>
            </div>
          </td>
          <td class="px-3 py-2 text-right tabular-nums">{num(f.variant_count)}</td>
          <td class="px-3 py-2 text-right tabular-nums">{num(f.distinct_keys)}</td>
          <td class="px-3 py-2 text-right tabular-nums">{num(f.object_count)}</td>
          <td class="px-3 py-2 text-right text-xs tabular-nums">{fmt(f.newest_at)}</td>
        </tr>
      {:else}
        <tr><td colspan="7" class="px-3 py-6 text-center text-fg-muted">{loading ? 'Loading…' : 'No variants stored.'}</td></tr>
      {/each}
    </tbody>
  </table>
</div>
