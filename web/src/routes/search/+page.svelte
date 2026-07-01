<script lang="ts">
  // /search — unified results page + facet sidebar (Phase 1.16.B-2).
  //
  // Reads ?q= from the URL, calls GET /api/v1/search + /search/facets,
  // renders card list + collapsible facet groups; multi-select within
  // a facet AND-joins across facets. "Save as collection" persists
  // current results as a new collection.

  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { t } from '$stores/lang.svelte';

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
  let saving = $state(false);
  let saveResult = $state('');

  const activeCount = $derived.by(() => {
    if (totalCountCapped) return '10,000+';
    return totalCount.toLocaleString();
  });

  async function runSearch(query: string, opts: { append?: boolean } = {}) {
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
      const params = new URLSearchParams({ q: query, limit: '25' });
      if (opts.append && cursor) params.set('cursor', cursor);
      const [searchResp, facetsResp] = await Promise.all([
        fetch(`/api/v1/search?${params.toString()}`, { credentials: 'include' }),
        opts.append ? Promise.resolve(null) : fetch(`/api/v1/search/facets?q=${encodeURIComponent(query)}`, { credentials: 'include' }),
      ]);
      if (!searchResp.ok) {
        error = `search: ${searchResp.status}`;
        return;
      }
      const data = (await searchResp.json()) as SearchResponse;
      cursor = data.next_cursor || '';
      totalCount = data.total_count;
      totalCountCapped = data.total_count_capped;
      hits = opts.append ? [...hits, ...data.hits] : data.hits;
      if (facetsResp && facetsResp.ok) {
        const fd = (await facetsResp.json()) as FacetsResponse;
        facets = fd.facets ?? {};
      }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
      loadingMore = false;
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
    q = page.url.searchParams.get('q') ?? '';
    if (q) void runSearch(q);
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
        saveResult = `Save failed: ${resp.status} ${err}`;
        return;
      }
      const data = await resp.json();
      saveResult = `Saved ${data.saved_count} results${data.truncated ? ' (truncated to first 100)' : ''}.`;
      setTimeout(() => {
        void goto(`/collections/${data.collection_id}`);
      }, 800);
    } catch (e) {
      saveResult = e instanceof Error ? e.message : String(e);
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('nav.advanced_search')} — artist-alley</title></svelte:head>

<div class="mx-auto flex w-full max-w-6xl gap-6 px-6 py-8">
  <!-- Facet sidebar -->
  <aside class="w-64 shrink-0" data-testid="facet-sidebar">
    <div class="sticky top-4 space-y-4">
      <div class="flex items-center justify-between">
        <h2 class="text-sm font-semibold uppercase tracking-wide text-fg-muted">Facets</h2>
        {#if Object.values(selectedFacets).some((s) => s.size > 0)}
          <button
            type="button"
            onclick={clearFacets}
            class="rounded px-2 py-0.5 text-xs text-accent hover:bg-surface-elevated"
          >Clear all</button>
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
        >Advanced builder</a>
        {#if hits.length > 0}
          <button
            type="button"
            onclick={() => { saveOpen = true; saveName = q; }}
            class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
            data-testid="save-as-collection"
          >Save as collection</button>
        {/if}
      </div>
    </div>

    <form onsubmit={submit} class="mb-6 flex gap-2">
      <input
        bind:value={q}
        type="search"
        placeholder="Type a query…"
        data-testid="search-input"
        class="flex-1 rounded-md border border-border bg-surface px-3 py-2 text-sm text-fg
               focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      />
      <button
        type="submit"
        class="rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent hover:bg-accent/90"
      >Search</button>
    </form>

    {#if error}
      <div role="alert" class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">
        {error}
      </div>
    {/if}

    {#if !loading && hits.length > 0}
      <p class="mb-3 text-sm text-fg-muted" data-testid="search-total-count">
        Showing <strong>{hits.length}</strong> of <strong>{activeCount}</strong> results
      </p>
    {/if}

    {#if loading}
      <p class="text-sm text-fg-muted">Searching…</p>
    {:else if hits.length === 0 && q}
      <p class="text-sm text-fg-muted">No matches. Try a different query.</p>
    {/if}

    <ul class="space-y-3">
      {#each hits as h (h.type + ':' + h.id)}
        <li class="rounded-md border border-border bg-surface p-3 hover:border-border-strong" data-testid="search-hit">
          <a href={detailHref(h)} class="block">
            <div class="mb-1 flex items-center gap-2">
              <span class="rounded px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide {typeBadge(h.type)}">{h.type}</span>
              {#if h.origin_server_id}
                <span class="rounded border border-border px-1.5 py-0.5 text-[10px] text-fg-muted">Federated</span>
              {/if}
              <span class="ml-auto text-xs text-fg-muted">score {h.score.toFixed(3)}</span>
            </div>
            <h2 class="text-base font-semibold text-fg">{h.title || 'Untitled'}</h2>
            {#if h.summary}
              <p class="mt-1 text-sm text-fg-muted">{h.summary}</p>
            {/if}
          </a>
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
        >{loadingMore ? 'Loading…' : 'Load more'}</button>
      </div>
    {/if}
  </section>
</div>

<!-- Save-as-collection modal -->
{#if saveOpen}
  <div
    role="dialog"
    aria-modal="true"
    aria-label="Save search as collection"
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
    onclick={(e) => { if (e.target === e.currentTarget) saveOpen = false; }}
  >
    <div class="w-full max-w-md rounded-lg border border-border bg-surface p-5 shadow-xl">
      <h3 class="mb-3 text-lg font-semibold">Save these results as a collection</h3>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">Collection name</span>
        <input
          bind:value={saveName}
          type="text"
          class="w-full rounded-md border border-border bg-surface px-3 py-1.5 text-sm"
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
        >Cancel</button>
        <button
          type="button"
          onclick={submitSave}
          disabled={saving || !saveName.trim()}
          class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:opacity-50"
        >{saving ? 'Saving…' : 'Save collection'}</button>
      </div>
    </div>
  </div>
{/if}
