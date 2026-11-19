<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/storage/history: one asset's preservation history (#1598).
  //
  // The operator types or pastes an asset id and reads the fixity
  // checks the checksum sweep recorded for it, newest first, one page
  // at a time. Reads gate on system.storage.read server-side; the admin
  // layout already hides this route from anyone who cannot open it.
  //
  // `?asset=<id>` makes a lookup linkable. Paging is keyset (the API
  // only moves to older events), so "Newer" walks back a trail of the
  // cursors that opened each page. Only the current page is rendered.

  import { onMount, tick } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { site } from '$stores/site.svelte';
  import {
    CursorTrail,
    HISTORY_PAGE_SIZE,
    eventDetails,
    outcomeLabel,
    parseAssetId,
    rangeLabel,
    reasonLabel,
    variantLabel,
    type PreservationEvent,
    type PreservationEventPage,
  } from '$lib/admin/preservationHistory';

  let input = $state('');
  let inputError = $state('');
  let assetId = $state<string | null>(null);
  let loading = $state(false);
  let error = $state('');
  let result = $state<PreservationEventPage | null>(null);
  let trail = new CursorTrail();
  let pageNo = $state(1);
  let resultsHeading = $state<HTMLHeadingElement | null>(null);

  // Only the LATEST request may change what is on screen. An operator
  // can submit a second asset (Enter keeps working) or page again while
  // a request is still out; each call takes a number, aborts the one
  // before it, and a response that is no longer the latest is dropped
  // whole: no result, error, loading state or focus from it.
  let seq = 0;
  let inflight: AbortController | null = null;

  /**
   * Loads one page. `commit` runs only when THIS request succeeds and
   * is still the latest, so the cursor trail and page number change
   * together with the rendered page or not at all. `fresh` is a lookup
   * of a (possibly different) asset: its failure clears the old page;
   * a failed page change keeps the current page on screen, so a retry
   * starts from a coherent state.
   */
  async function load(id: string, cursor: string | null, commit: () => void, fresh: boolean) {
    const mine = ++seq;
    inflight?.abort();
    const ctl = new AbortController();
    inflight = ctl;
    loading = true;
    error = '';
    if (fresh) result = null;
    try {
      const r = await api.GET('/admin/storage/assets/{id}/preservation-events', {
        params: { path: { id }, query: { limit: HISTORY_PAGE_SIZE, ...(cursor ? { cursor } : {}) } },
        signal: ctl.signal,
      });
      if (mine !== seq) return;
      if (r.error) {
        const status = r.response.status;
        if (status === 404) error = 'No asset has this ID.';
        else if (status === 403) error = 'You need the storage read permission to see preservation history.';
        else if (status === 400) error = 'This page of the history is out of date. Start again from the newest events.';
        else error = (r.error as { error?: string }).error || 'Could not load the preservation history.';
        return;
      }
      commit();
      result = r.data as PreservationEventPage;
      pageNo = trail.page;
    } catch {
      if (mine !== seq) return;
      error = 'Could not reach the server. Check your connection and try again.';
      return;
    } finally {
      if (mine === seq) {
        loading = false;
        inflight = null;
      }
    }
    await tick();
    if (mine === seq) resultsHeading?.focus();
  }

  async function lookup(e?: SubmitEvent) {
    e?.preventDefault();
    const id = parseAssetId(input);
    if (!id) {
      inputError = 'Enter an asset ID in the form 1b4e28ba-2fa1-11d2-883f-0016d3cca427.';
      return;
    }
    inputError = '';
    input = id;
    assetId = id;
    void goto(`?asset=${id}`, { replaceState: true, keepFocus: true, noScroll: true });
    await load(id, null, () => trail.reset(), true);
  }

  async function older() {
    const next = result?.next_cursor;
    if (!assetId || !next) return;
    await load(assetId, next, () => trail.older(next), false);
  }

  async function newer() {
    if (!assetId || !trail.canGoNewer) return;
    await load(assetId, trail.previous, () => trail.newer(), false);
  }

  async function restart() {
    if (!assetId) return;
    await load(assetId, null, () => trail.reset(), false);
  }

  function fmt(iso: string): string {
    return new Date(iso).toLocaleString();
  }

  function tone(o: PreservationEvent['outcome']): string {
    if (o === 'success') return 'border-success/40 bg-success-container text-on-success-container';
    if (o === 'warning') return 'border-warning/40 bg-warning-container text-on-warning-container';
    return 'border-danger/40 bg-danger-container text-on-danger-container';
  }

  onMount(() => {
    const q = page.url.searchParams.get('asset');
    if (q) {
      input = q;
      void lookup();
    }
  });
</script>

<svelte:head><title>Preservation history - {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Preservation history</h2>
  <p class="mt-1 max-w-3xl text-sm text-fg-muted">
    Every fixity check recorded for one asset, newest first. The
    <a class="text-accent hover:underline" href="/admin/storage/checksums">checksum sweep</a> writes one entry for each
    file it checks, on every asset that uses that file, so bytes shared by two assets appear in both histories. An
    entry describes what the sweep saw at the time; the file may have changed since.
  </p>
</header>

<form class="mb-4 flex max-w-2xl flex-col gap-1" onsubmit={lookup} novalidate>
  <label for="history-asset" class="text-sm font-medium">Asset ID</label>
  <div class="flex flex-wrap gap-2">
    <input
      id="history-asset"
      name="asset"
      bind:value={input}
      autocomplete="off"
      spellcheck="false"
      aria-invalid={inputError ? 'true' : undefined}
      aria-describedby={inputError ? 'history-asset-error' : 'history-asset-hint'}
      class="min-w-0 flex-1 basis-64 rounded border border-border bg-surface px-3 py-2 font-mono text-sm"
      placeholder="1b4e28ba-2fa1-11d2-883f-0016d3cca427"
    />
    <!-- Never disabled while loading: a disabled default button also
         blocks Enter, and a new lookup should replace a slow one (the
         latest-request guard in load() keeps the old answer off screen). -->
    <button type="submit" class="shrink-0 rounded bg-accent px-3 py-2 text-sm font-medium text-on-accent">
      Show history
    </button>
  </div>
  {#if inputError}
    <p id="history-asset-error" class="text-xs text-danger">{inputError}</p>
  {:else}
    <p id="history-asset-hint" class="text-xs text-fg-muted">The asset's UUID, as shown in its address.</p>
  {/if}
</form>

{#if error}
  <div role="alert" class="mb-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">
    {error}
    {#if error.startsWith('This page of the history')}
      <button type="button" onclick={restart} class="ml-2 underline">Show the newest events</button>
    {/if}
  </div>
{/if}

<section aria-live="polite" aria-busy={loading}>
  {#if result}
    <div class="mb-2 flex flex-wrap items-baseline justify-between gap-2">
      <h3 bind:this={resultsHeading} tabindex="-1" class="text-lg font-semibold outline-none">
        {rangeLabel(pageNo, result.items.length, result.total)}
      </h3>
      <p class="text-xs text-fg-muted">Asset <span class="font-mono">{result.asset_id}</span></p>
    </div>

    {#if result.items.length === 0}
      <p class="rounded border border-border px-3 py-8 text-center text-sm text-fg-muted">
        No preservation events have been recorded for this asset yet. They appear after a checksum sweep checks its
        files.
      </p>
    {:else}
      <ol class="space-y-2">
        {#each result.items as e (e.id)}
          <li class="rounded border border-border bg-surface p-3 text-sm">
            <div class="flex flex-wrap items-center gap-2">
              <span class={`inline-flex items-center gap-1 rounded border px-2 py-0.5 text-xs font-semibold ${tone(e.outcome)}`}>
                {#if e.outcome === 'success'}
                  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" aria-hidden="true"><path d="M20 6 9 17l-5-5" /></svg>
                {:else if e.outcome === 'warning'}
                  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z" /><path d="M12 9v4M12 17h.01" /></svg>
                {:else}
                  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12" /></svg>
                {/if}
                {outcomeLabel(e.outcome)}
              </span>
              <span class="font-medium">{variantLabel(e)}</span>
              <span class="text-xs text-fg-muted">generation {e.observed_generation}</span>
              <time class="ml-auto whitespace-nowrap text-xs text-fg-muted" datetime={e.occurred_at}>{fmt(e.occurred_at)}</time>
            </div>
            <p class="mt-1">{reasonLabel(e.outcome_reason)}</p>
            <dl class="mt-2 grid grid-cols-1 gap-x-4 gap-y-1 text-xs sm:grid-cols-2">
              {#each eventDetails(e) as d (d.label)}
                <div class="flex gap-2"><dt class="text-fg-muted">{d.label}</dt><dd class={d.mono ? 'font-mono' : ''} title={d.title}>{d.value}</dd></div>
              {/each}
            </dl>
          </li>
        {/each}
      </ol>
    {/if}

    {#if pageNo > 1 || result.next_cursor}
      <nav class="mt-3 flex items-center gap-2" aria-label="History pages">
        <button type="button" onclick={newer} disabled={loading || pageNo <= 1} class="rounded border border-border px-3 py-1.5 text-sm hover:bg-state-hover disabled:opacity-50">
          Newer
        </button>
        <span class="text-xs text-fg-muted">Page {pageNo}</span>
        <button type="button" onclick={older} disabled={loading || !result.next_cursor} class="rounded border border-border px-3 py-1.5 text-sm hover:bg-state-hover disabled:opacity-50">
          Older
        </button>
      </nav>
    {/if}
  {:else if loading}
    <p class="text-sm text-fg-muted">Loading the history…</p>
  {/if}
</section>
