<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin storage usage — v0.4.0 Sprint 2 (#402).
  //
  // Read-only rollup of what storage holds: deduplicated bytes on disk,
  // the originals-vs-derivatives split, and breakdowns by content type
  // and backend. Gated on system.storage.read (or system.admin).

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';

  type Bucket = { content_type: string; variant_count: number; total_bytes: number };
  type Backend = { backend: string; object_count: number };
  type Usage = {
    object_count: number;
    variant_count: number;
    total_bytes: number;
    original_bytes: number;
    derivative_bytes: number;
    by_content_type: Bucket[];
    by_backend: Backend[];
  };

  let usage = $state<Usage | null>(null);
  let loading = $state(true);
  let error = $state('');

  const derivativePct = $derived(
    usage && usage.total_bytes > 0 ? (usage.derivative_bytes / usage.total_bytes) * 100 : 0,
  );

  async function load() {
    loading = true;
    error = '';
    try {
      const r = await api.GET('/admin/storage/usage');
      if (r.error) { error = (r.error as { error?: string }).error || 'load failed'; return; }
      usage = r.data as Usage;
    } finally { loading = false; }
  }

  // Binary units — this is disk occupancy, so 1 KiB = 1024 B.
  function bytes(n: number): string {
    if (n < 1024) return `${n} B`;
    const units = ['KiB', 'MiB', 'GiB', 'TiB'];
    let v = n / 1024, i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
  }
  function num(n: number): string { return n.toLocaleString(); }

  onMount(() => { void load(); });
</script>

<svelte:head><title>Storage usage — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Storage usage</h2>
  <p class="text-sm text-fg-muted">
    What the install actually holds. Storage is content-addressed, so these are
    <strong>deduplicated</strong> bytes — identical files are stored once.
    <a class="text-accent hover:underline" href="/admin/storage/variants">Variant families</a>
  </p>
</header>

{#if error}<div role="alert" class="mb-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</div>{/if}

{#if loading}
  <p class="text-fg-muted">Loading…</p>
{:else if usage}
  <!-- Headline numbers -->
  <div class="mb-6 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
    <div class="rounded-lg border border-border bg-surface-elevated p-4">
      <div class="text-sm text-fg-muted">Total on disk</div>
      <div class="mt-1 text-2xl font-semibold tabular-nums">{bytes(usage.total_bytes)}</div>
    </div>
    <div class="rounded-lg border border-border bg-surface-elevated p-4">
      <div class="text-sm text-fg-muted">Objects</div>
      <div class="mt-1 text-2xl font-semibold tabular-nums">{num(usage.object_count)}</div>
    </div>
    <div class="rounded-lg border border-border bg-surface-elevated p-4">
      <div class="text-sm text-fg-muted">Originals</div>
      <div class="mt-1 text-2xl font-semibold tabular-nums">{bytes(usage.original_bytes)}</div>
    </div>
    <div class="rounded-lg border border-border bg-surface-elevated p-4">
      <div class="text-sm text-fg-muted">Derivatives</div>
      <div class="mt-1 text-2xl font-semibold tabular-nums">{bytes(usage.derivative_bytes)}</div>
      <div class="mt-1 text-xs text-fg-muted">{derivativePct.toFixed(0)}% of total, across {num(usage.variant_count)} variants</div>
    </div>
  </div>

  <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
    <section>
      <h3 class="mb-2 text-lg font-medium">By content type</h3>
      <div class="overflow-x-auto rounded border border-border">
        <table class="w-full text-left text-sm">
          <thead class="bg-surface-elevated text-xs uppercase tracking-wide text-fg-muted">
            <tr>
              <th class="px-3 py-2">Content type</th>
              <th class="px-3 py-2 text-right">Variants</th>
              <th class="px-3 py-2 text-right">Size</th>
            </tr>
          </thead>
          <tbody>
            {#each usage.by_content_type as c (c.content_type)}
              <tr class="border-t border-border">
                <td class="px-3 py-2 font-mono text-xs">{c.content_type}</td>
                <td class="px-3 py-2 text-right tabular-nums">{num(c.variant_count)}</td>
                <td class="px-3 py-2 text-right tabular-nums">{bytes(c.total_bytes)}</td>
              </tr>
            {:else}
              <tr><td colspan="3" class="px-3 py-6 text-center text-fg-muted">Nothing stored yet.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    </section>

    <section>
      <h3 class="mb-2 text-lg font-medium">By backend</h3>
      <div class="overflow-x-auto rounded border border-border">
        <table class="w-full text-left text-sm">
          <thead class="bg-surface-elevated text-xs uppercase tracking-wide text-fg-muted">
            <tr>
              <th class="px-3 py-2">Backend</th>
              <th class="px-3 py-2 text-right">Objects</th>
            </tr>
          </thead>
          <tbody>
            {#each usage.by_backend as b (b.backend)}
              <tr class="border-t border-border">
                <td class="px-3 py-2 font-mono text-xs">{b.backend}</td>
                <td class="px-3 py-2 text-right tabular-nums">{num(b.object_count)}</td>
              </tr>
            {:else}
              <tr><td colspan="2" class="px-3 py-6 text-center text-fg-muted">No objects.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
      <p class="mt-2 text-xs text-fg-muted">
        Object counts only. Byte totals come from the variant rollup, which already
        includes each object's original.
      </p>
    </section>
  </div>
{/if}
