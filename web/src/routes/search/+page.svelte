<script lang="ts">
  // /search — unified results page (Phase 1.16.B-1).
  //
  // Reads ?q= from the URL, calls GET /api/v1/search, renders a card
  // list with total-count display. Advanced filters (facets, entity-
  // specific fields) land in 1.16.B-2.

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

  let q = $state('');
  let hits = $state<Hit[]>([]);
  let cursor = $state('');
  let totalCount = $state(0);
  let totalCountCapped = $state(false);
  let loading = $state(false);
  let loadingMore = $state(false);
  let error = $state('');

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
      const resp = await fetch(`/api/v1/search?${params.toString()}`, { credentials: 'include' });
      if (!resp.ok) {
        error = `search: ${resp.status}`;
        return;
      }
      const data = (await resp.json()) as SearchResponse;
      cursor = data.next_cursor || '';
      totalCount = data.total_count;
      totalCountCapped = data.total_count_capped;
      hits = opts.append ? [...hits, ...data.hits] : data.hits;
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
</script>

<svelte:head><title>{t('nav.advanced_search')} — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-4xl px-6 py-8">
  <h1 class="font-display mb-4 text-3xl font-semibold">{t('nav.advanced_search')}</h1>

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
</div>
