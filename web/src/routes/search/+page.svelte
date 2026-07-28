<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /search — unified results page + facet sidebar (Phase 1.16.B-2).
  //
  // Reads ?q= from the URL, calls GET /api/v1/search + /search/facets,
  // renders card list + collapsible facet groups; multi-select within
  // a facet AND-joins across facets. "Save as collection" persists
  // current results as a new collection.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { t } from '$stores/lang.svelte';
  import ThumbButtons from '$components/search/ThumbButtons.svelte';
  import { createScrollSnapshot } from '$lib/util/scrollSnapshot';

  type Hit = {
    type: 'asset' | 'collection' | 'post';
    id: string;
    title: string;
    summary: string;
    score: number;
    created_at: string;
    updated_at: string;
    owner_user_ref?: number;
    origin_server_id?: string;
    extra: Record<string, unknown>;
  };

  type SearchResponse = {
    hits: Hit[];
    next_cursor: string;
    total_count: number;
    total_count_capped: boolean;
    types_matched: string[];
  };

  type FacetBucket = { value: string; count: number; label?: string };
  type FacetResult = { type: string; buckets: FacetBucket[]; timed_out?: boolean };
  type FacetsResponse = { facets: Record<string, FacetResult> };

  let q = $state('');
  let hits = $state<Hit[]>([]);
  let cursor = $state('');
  let totalCount = $state(0);
  let totalCountCapped = $state(false);
  let loading = $state(false);
  let loadingMore = $state(false);
  let error = $state('');
  let facets = $state<Record<string, FacetResult>>({});
  let selectedFacets = $state<Record<string, Set<string>>>({});
  // Save-as-collection modal state.
  let saveOpen = $state(false);
  let saveName = $state('');
  // Phase 1.16.B-4 — Save-search modal state. Distinct from
  // save-as-collection so a user can persist EITHER a one-shot
  // snapshot (collection) OR an ongoing notification target
  // (saved_search) without one closing the other.
  let saveSearchOpen = $state(false);
  let saveSearchName = $state('');
  let saveSearchInterval = $state(60);
  let saveSearchChannel = $state<'email' | 'none'>('email');
  let savingSearch = $state(false);
  let saveSearchResult = $state('');
  let saving = $state(false);
  let saveResult = $state('');

  const activeCount = $derived.by(() => {
    if (totalCountCapped) return '10,000+';
    return totalCount.toLocaleString();
  });

  // dsl mode kicks in when the URL had ?dsl= — usually from the
  // /search/advanced builder or a "Find similar assets" nav.
  let dslMode = $state(false);

  /** Bumped by every runSearch and by snapshot restoration, so a
   *  result set that has been superseded can't land on top of a newer
   *  one. Restoring a back-navigation is exactly that race: the mount
   *  fetch and `snapshot.restore` have no defined order. */
  let searchGen = 0;

  async function runSearch(query: string, opts: { append?: boolean } = {}) {
    const gen = ++searchGen;
    if (!query) {
      hits = [];
      totalCount = 0;
      totalCountCapped = false;
      cursor = '';
      error = '';
      facets = {};
      return;
    }
    if (opts.append) {
      loadingMore = true;
    } else {
      loading = true;
    }
    error = '';
    try {
      const params = new URLSearchParams({ limit: '25' });
      if (dslMode) params.set('dsl', query); else params.set('q', query);
      if (opts.append && cursor) params.set('cursor', cursor);
      const [searchResp, facetsResp] = await Promise.all([
        fetch(`/api/v1/search?${params.toString()}`, { credentials: 'include' }),
        opts.append || dslMode
          ? Promise.resolve(null)
          : fetch(`/api/v1/search/facets?q=${encodeURIComponent(query)}`, { credentials: 'include' }),
      ]);
      if (gen !== searchGen) return;
      if (!searchResp.ok) {
        error = t('search.err_generic', { status: searchResp.status });
        return;
      }
      const data = (await searchResp.json()) as SearchResponse;
      if (gen !== searchGen) return;
      cursor = data.next_cursor || '';
      totalCount = data.total_count;
      totalCountCapped = data.total_count_capped;
      hits = opts.append ? [...hits, ...data.hits] : data.hits;
      if (facetsResp && facetsResp.ok) {
        const fd = (await facetsResp.json()) as FacetsResponse;
        facets = fd.facets ?? {};
      }
    } catch (e) {
      if (gen === searchGen) error = e instanceof Error ? e.message : String(e);
    } finally {
      if (gen === searchGen) {
        loading = false;
        loadingMore = false;
      }
    }
  }

  function submit(e: Event) {
    e.preventDefault();
    const url = new URL(page.url);
    url.searchParams.set('q', q);
    goto(url.pathname + url.search, { replaceState: false });
    void runSearch(q);
  }

  function toggleFacet(type: string, value: string) {
    const current = selectedFacets[type] ?? new Set<string>();
    if (current.has(value)) {
      current.delete(value);
    } else {
      current.add(value);
    }
    selectedFacets = { ...selectedFacets, [type]: current };
    // For B-2 the toggle is a UI-only state marker; wiring facet
    // filters into a re-issued /search query lands in a follow-up
    // once /search accepts filter params. Documented in PR body.
  }

  function isSelected(type: string, value: string): boolean {
    return (selectedFacets[type] ?? new Set()).has(value);
  }

  function clearFacets() {
    selectedFacets = {};
  }

  onMount(() => {
    const urlDSL = page.url.searchParams.get('dsl');
    const urlQ = page.url.searchParams.get('q');
    if (urlDSL) {
      dslMode = true;
      q = urlDSL;
    } else {
      dslMode = false;
      q = urlQ ?? '';
    }
    // `hits` already populated means snapshot.restore got here first
    // (back-navigation) — re-running the search would throw away the
    // "load more" pages the user had accumulated.
    if (q && hits.length === 0) void runSearch(q);
  });

  // Back-navigation restoration (#584). Results are paged behind a
  // manual "load more", so the offset is only meaningful alongside the
  // hits it was measured against — restoring one without the other
  // would land the user in the middle of a shorter list.
  interface SearchSnapshot {
    q: string;
    dsl: boolean;
    hits: Hit[];
    cursor: string;
    totalCount: number;
    totalCountCapped: boolean;
    facets: Record<string, FacetResult>;
  }
  export const snapshot = createScrollSnapshot<SearchSnapshot>({
    capture: () => ({
      q,
      dsl: dslMode,
      hits,
      cursor,
      totalCount,
      totalCountCapped,
      facets,
    }),
    restore: (saved) => {
      if (!saved || saved.hits.length === 0) return;
      searchGen++;
      q = saved.q;
      dslMode = saved.dsl;
      hits = saved.hits;
      cursor = saved.cursor;
      totalCount = saved.totalCount;
      totalCountCapped = saved.totalCountCapped;
      facets = saved.facets;
      loading = false;
      loadingMore = false;
    },
  });

  function typeBadge(type: Hit['type']): string {
    return type === 'asset' ? 'bg-info/15 text-info' : type === 'collection' ? 'bg-success/15 text-success' : 'bg-warning/15 text-warning';
  }

  function detailHref(h: Hit): string {
    if (h.type === 'asset') return `/assets/${h.id}`;
    if (h.type === 'collection') return `/collections/${h.id}`;
    return `/posts/${h.id}`;
  }

  async function submitSave() {
    if (!saveName.trim()) return;
    saving = true;
    saveResult = '';
    try {
      const resp = await fetch('/api/v1/search/save-as-collection', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({ name: saveName.trim(), q, types: ['asset'] }),
      });
      if (!resp.ok) {
        const err = await resp.text();
        saveResult = t('search.save_collection.save_failed', { status: resp.status, err });
        return;
      }
      const data = await resp.json();
      saveResult = data.truncated
        ? t('search.save_collection.saved_truncated', { n: data.saved_count })
        : t('search.save_collection.saved', { n: data.saved_count });
      setTimeout(() => {
        void goto(`/collections/${data.collection_id}`);
      }, 800);
    } catch (e) {
      saveResult = e instanceof Error ? e.message : String(e);
    } finally {
      saving = false;
    }
  }

  async function submitSaveSearch() {
    if (!saveSearchName.trim()) return;
    savingSearch = true;
    saveSearchResult = '';
    try {
      // Use ?dsl= as the stored query when the caller reached
      // this page from /search/advanced (dslMode); otherwise treat
      // the free-text q as the DSL string (single-token free-text
      // parses cleanly).
      const dslString = q;
      const resp = await fetch('/api/v1/search/saved', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({
          name: saveSearchName.trim(),
          dsl: dslString,
          notify_channel: saveSearchChannel,
          notify_interval_minutes: saveSearchInterval,
        }),
      });
      if (!resp.ok) {
        const err = await resp.text();
        saveSearchResult = t('search.save_search.save_failed', { status: resp.status, err });
        return;
      }
      const data = await resp.json();
      saveSearchResult = t('search.save_search.saved', { minutes: data.notify_interval_minutes });
      setTimeout(() => {
        void goto('/account/saved-searches');
      }, 1000);
    } catch (e) {
      saveSearchResult = e instanceof Error ? e.message : String(e);
    } finally {
      savingSearch = false;
    }
  }
</script>

<svelte:head><title>{t('nav.advanced_search')} — {site.name}</title></svelte:head>

<div class="mx-auto flex w-full max-w-6xl gap-6 px-6 py-8">
  <!-- Facet sidebar -->
  <aside class="w-64 shrink-0" data-testid="facet-sidebar">
    <div class="sticky top-4 space-y-4">
      <div class="flex items-center justify-between">
        <h2 class="text-sm font-semibold uppercase tracking-wide text-fg-muted">{t('search.facets_heading')}</h2>
        {#if Object.values(selectedFacets).some((s) => s.size > 0)}
          <button
            type="button"
            onclick={clearFacets}
            class="rounded px-2 py-0.5 text-xs text-accent hover:bg-surface-elevated"
          >{t('common.clear_all')}</button>
        {/if}
      </div>
      {#each Object.entries(facets) as [type, res] (type)}
        {#if res.buckets && res.buckets.length > 0}
          <div class="rounded-md border border-border bg-surface p-3">
            <div class="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">{type}</div>
            <ul class="space-y-1 text-sm">
              {#each res.buckets.slice(0, 10) as b (b.value)}
                <li>
                  <label class="flex cursor-pointer items-center gap-2">
                    <input
                      type="checkbox"
                      checked={isSelected(type, b.value)}
                      onchange={() => toggleFacet(type, b.value)}
                      class="h-3.5 w-3.5"
                    />
                    <span class="flex-1 truncate">{b.label ?? b.value}</span>
                    <span class="text-xs text-fg-muted">{b.count}</span>
                  </label>
                </li>
              {/each}
            </ul>
          </div>
        {/if}
      {/each}
    </div>
  </aside>

  <!-- Main results column — <section> not <main> to keep the outer
       layout's single <main> element authoritative (Playwright
       strict-mode locator('main') requires this). -->
  <section class="flex-1 min-w-0">
    <div class="mb-4 flex items-center justify-between gap-3">
      <h1 class="font-display text-3xl font-semibold">{t('nav.advanced_search')}</h1>
      <div class="flex items-center gap-2">
        <a
          href="/search/advanced"
          class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
        >{t('search.advanced_builder')}</a>
        {#if hits.length > 0}
          <button
            type="button"
            onclick={() => { saveSearchOpen = true; saveSearchName = q; }}
            class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
            data-testid="save-search"
          >{t('search.save_search_button')}</button>
          <button
            type="button"
            onclick={() => { saveOpen = true; saveName = q; }}
            class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
            data-testid="save-as-collection"
          >{t('search.save_as_collection')}</button>
        {/if}
      </div>
    </div>

    <form onsubmit={submit} class="mb-6 flex gap-2">
      <input
        bind:value={q}
        type="search"
        placeholder={t('search.query_placeholder')}
        data-testid="search-input"
        class="flex-1 rounded-md border border-border-strong bg-surface px-3 py-2 text-sm text-fg
               focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      />
      <button
        type="submit"
        class="rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent hover:bg-accent/90"
      >{t('common.search')}</button>
    </form>

    {#if error}
      <div role="alert" class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">
        {error}
      </div>
    {/if}

    {#if !loading && hits.length > 0}
      <p class="mb-3 text-sm text-fg-muted" data-testid="search-total-count">
        {t('search.counter', { n: hits.length, total: activeCount })}
      </p>
    {/if}

    {#if loading}
      <p class="text-sm text-fg-muted">{t('search.searching')}</p>
    {:else if hits.length === 0 && q}
      <p class="text-sm text-fg-muted">{t('search.no_matches')}</p>
    {/if}

    <ul class="space-y-3">
      {#each hits as h, i (h.type + ':' + h.id)}
        <li class="rounded-md border border-border bg-surface p-3 hover:border-border-strong" data-testid="search-hit">
          <a href={detailHref(h)} class="block">
            <div class="mb-1 flex items-center gap-2">
              <span class="rounded px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide {typeBadge(h.type)}">{h.type}</span>
              {#if h.origin_server_id}
                <span class="rounded border border-border px-1.5 py-0.5 text-[10px] text-fg-muted">{t('search.federated_badge')}</span>
              {/if}
              <span class="ml-auto text-xs text-fg-muted">score {h.score.toFixed(3)}</span>
            </div>
            <h2 class="text-base font-semibold text-fg">{h.title || t('common.untitled')}</h2>
            {#if h.summary}
              <p class="mt-1 text-sm text-fg-muted">{h.summary}</p>
            {/if}
          </a>
          {#if h.type === 'asset'}
            <div class="mt-2 flex justify-end">
              <ThumbButtons dsl={q} hitAssetId={h.id} hitPosition={i + 1} />
            </div>
          {/if}
        </li>
      {/each}
    </ul>

    {#if cursor}
      <div class="mt-4 flex justify-center">
        <button
          type="button"
          onclick={() => runSearch(q, { append: true })}
          disabled={loadingMore}
          class="rounded-md border border-border bg-surface px-4 py-1.5 text-sm hover:border-border-strong disabled:opacity-50"
        >{loadingMore ? t('common.loading') : t('common.load_more')}</button>
      </div>
    {/if}
  </section>
</div>

<!-- Save-as-collection modal -->
{#if saveOpen}
  <div
    role="dialog"
    aria-modal="true"
    aria-label={t('search.save_collection.dialog_label')}
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
    onclick={(e) => { if (e.target === e.currentTarget) saveOpen = false; }}
  >
    <div class="w-full max-w-md rounded-lg border border-border bg-surface p-5 shadow-xl">
      <h3 class="mb-3 text-lg font-semibold">{t('search.save_collection.heading')}</h3>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">{t('search.save_collection.name_label')}</span>
        <input
          bind:value={saveName}
          type="text"
          class="w-full rounded-md border border-border-strong bg-surface px-3 py-1.5 text-sm"
        />
      </label>
      {#if saveResult}
        <p class="mb-3 text-sm text-fg-muted" data-testid="save-result">{saveResult}</p>
      {/if}
      <div class="flex justify-end gap-2">
        <button
          type="button"
          onclick={() => (saveOpen = false)}
          class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
        >{t('common.cancel')}</button>
        <button
          type="button"
          onclick={submitSave}
          disabled={saving || !saveName.trim()}
          class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:opacity-50"
        >{saving ? t('common.saving') : t('search.save_collection.submit')}</button>
      </div>
    </div>
  </div>
{/if}

<!-- Save-search modal (Phase 1.16.B-4). Persists the query as a
     notification target rather than a snapshot; the coordinator
     runs it on the interval + emails when new hits appear. -->
{#if saveSearchOpen}
  <div
    role="dialog"
    aria-modal="true"
    aria-label={t('search.save_search.dialog_label')}
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
    onclick={(e) => { if (e.target === e.currentTarget) saveSearchOpen = false; }}
  >
    <div class="w-full max-w-md rounded-lg border border-border bg-surface p-5 shadow-xl">
      <h3 class="mb-3 text-lg font-semibold">{t('search.save_search.heading')}</h3>
      <p class="mb-3 text-sm text-fg-muted">
        {t('search.save_search.body')}
      </p>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">{t('search.save_search.name_label')}</span>
        <input
          bind:value={saveSearchName}
          type="text"
          class="w-full rounded-md border border-border-strong bg-surface px-3 py-1.5 text-sm"
        />
      </label>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">{t('search.save_search.interval_label')}</span>
        <input
          bind:value={saveSearchInterval}
          type="number"
          min="15"
          step="15"
          class="w-full rounded-md border border-border-strong bg-surface px-3 py-1.5 text-sm"
        />
      </label>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">{t('search.save_search.channel_label')}</span>
        <select
          bind:value={saveSearchChannel}
          class="w-full rounded-md border border-border-strong bg-surface px-3 py-1.5 text-sm"
        >
          <option value="email">{t('search.save_search.channel_email')}</option>
          <option value="none">{t('search.save_search.channel_none')}</option>
        </select>
      </label>
      {#if saveSearchResult}
        <p class="mb-3 text-sm text-fg-muted" data-testid="save-search-result">{saveSearchResult}</p>
      {/if}
      <div class="flex justify-end gap-2">
        <button
          type="button"
          onclick={() => (saveSearchOpen = false)}
          class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
        >{t('common.cancel')}</button>
        <button
          type="button"
          onclick={submitSaveSearch}
          disabled={savingSearch || !saveSearchName.trim()}
          class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:opacity-50"
        >{savingSearch ? t('common.saving') : t('search.save_search.submit')}</button>
      </div>
    </div>
  </div>
{/if}
