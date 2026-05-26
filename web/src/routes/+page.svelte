<script lang="ts">
  import { untrack } from 'svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import AssetCard from '$components/AssetCard.svelte';

  // Browse page — the first thing a logged-in user sees. Recent
  // assets first, infinite scroll via IntersectionObserver. View
  // mode switching lands in 1.13.E; for now this is the grid view.
  //
  // Search is sourced from the URL's `?q=` so refreshes and shared
  // links reproduce the same result set. The search input itself
  // lives in the global navbar (see +layout.svelte) and goto()s
  // here with the updated query string. Server-side the match runs
  // against the TSVECTOR `search_text` column on assets.

  interface Asset {
    id: string;
    title: string;
    description?: string;
    resource_type: number;
    owner_user_ref?: number | null;
    status: string;
    file_hash?: string | null;
    file_extension?: string | null;
    created_at: string;
    updated_at: string;
    tags: string[];
  }

  const PAGE = 36;

  // Read the current query from the URL. SvelteKit re-runs reactive
  // statements when `page` changes, so this triggers re-fetch on
  // navigation.
  const query = $derived(page.url.searchParams.get('q') ?? '');

  let items = $state<Asset[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(false);
  let initialLoaded = $state(false);
  let error = $state<string | null>(null);
  let sentinel: HTMLElement | undefined = $state();

  // Increment to invalidate in-flight fetches when the query changes,
  // so a slow response for the previous query can't overwrite the
  // new result set.
  let generation = 0;

  async function fetchPage(q: string, cursor: string | null, reset: boolean) {
    loading = true;
    error = null;
    const gen = ++generation;
    try {
      const params: Record<string, string | number> = { limit: PAGE };
      if (q.trim() !== '') params.q = q.trim();
      if (!reset && cursor) params.cursor = cursor;

      const { data, error: apiErr } = await api.GET('/assets', {
        params: { query: params as never },
      });

      // Race-guard: if the query changed since this fetch started,
      // drop the result.
      if (gen !== generation) return;

      if (apiErr || !data) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? 'Failed to load',
        );
      }

      const pageItems = (data.items ?? []) as Asset[];
      items = reset ? pageItems : [...items, ...pageItems];
      nextCursor = (data.next_cursor as string | null) ?? null;
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to load';
    } finally {
      if (gen === generation) {
        loading = false;
        initialLoaded = true;
      }
    }
  }

  // Reset and refetch every time the query changes. This covers the
  // initial mount AND subsequent navigations from the navbar search.
  $effect(() => {
    const q = query;
    untrack(() => {
      items = [];
      nextCursor = null;
      initialLoaded = false;
      void fetchPage(q, null, true);
    });
  });

  // Infinite scroll: when the sentinel scrolls into view, fetch the
  // next page. rootMargin gives a head-start so the next batch is in
  // flight before the user hits the end.
  $effect(() => {
    const node = sentinel;
    if (!node) return;
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            untrack(() => {
              if (nextCursor && !loading) {
                void fetchPage(query, nextCursor, false);
              }
            });
          }
        }
      },
      { rootMargin: '600px 0px' },
    );
    observer.observe(node);
    return () => observer.disconnect();
  });

  const hasMore = $derived(nextCursor !== null);
  const showEmpty = $derived(initialLoaded && items.length === 0 && !error);
</script>

<svelte:head>
  <title>{query ? `${query} — artist-alley` : 'Browse — artist-alley'}</title>
</svelte:head>

<div class="w-full px-4 py-4 space-y-4 sm:px-6">
  {#if query}
    <p class="text-sm text-fg-muted">
      Results for <span class="font-medium text-fg">"{query}"</span>
    </p>
  {/if}

  {#if error}
    <div role="alert" class="rounded-md border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-600 dark:text-red-300">
      {error}
    </div>
  {/if}

  {#if showEmpty}
    <div class="rounded-xl border border-dashed border-border p-12 text-center text-fg-muted">
      <p class="font-medium text-fg">{query ? 'No matches' : 'No assets yet'}</p>
      <p class="mt-1 text-sm">
        {query
          ? 'Try a different search term.'
          : "Once assets are uploaded they'll appear here, newest first."}
      </p>
    </div>
  {:else}
    <!--
      Full-bleed grid (the 1.13.D shape). Responsive: 2 columns on
      small phones up to 8 on extra-wide displays. Aspect-square
      cards keep the layout stable regardless of image dimensions.
      Masonry / thumbnail / list view-mode switching arrives in
      1.13.E; the column count is the only knob here.
    -->
    <div class="grid grid-cols-2 gap-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 2xl:grid-cols-8">
      {#each items as asset (asset.id)}
        <AssetCard {asset} />
      {/each}

      {#if loading}
        <!-- Skeleton cards while loading. Same shape as real ones so
             the layout doesn't reflow when results arrive. -->
        {#each Array(8) as _, i (i)}
          <div class="aspect-square rounded-lg bg-surface-elevated border border-border animate-pulse"></div>
        {/each}
      {/if}
    </div>

    {#if hasMore}
      <!-- IntersectionObserver target. -->
      <div bind:this={sentinel} class="h-px w-full" aria-hidden="true"></div>
    {/if}

    {#if !hasMore && items.length > 0}
      <p class="text-center text-xs text-fg-muted py-4">— end of feed —</p>
    {/if}
  {/if}
</div>
