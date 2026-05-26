<script lang="ts">
  import { untrack } from 'svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import PostCard from '$components/PostCard.svelte';

  // Browse page — feed of Posts (per Phase 1.13.D-2's model change).
  // Each Post wraps 1+ assets; the card renders the cover. Grid mode
  // (default); 1.13.E adds Masonry / Thumbnail / List + a switcher.
  //
  // Search is sourced from the URL's `?q=` so refreshes and shared
  // links reproduce the same result set. The search input itself
  // lives in the global navbar (see +layout.svelte) and goto()s
  // here with the updated query string. Server-side the match runs
  // against the TSVECTOR `search_text` column on posts (built from
  // title + description + tags + member-asset search_text by the
  // 00014 migration trigger).

  interface AssetSummary {
    id: string;
    file_hash?: string | null;
  }
  interface PostMember {
    asset_id: string;
    sort_order: number;
    asset: AssetSummary;
  }
  interface Post {
    id: string;
    author_user_ref: number;
    title: string;
    description: string;
    visibility: 'private' | 'followers' | 'public';
    cover_asset_id?: string | null;
    posted_at: string;
    like_count: number;
    comment_count: number;
    tags: string[];
    members: PostMember[];
    created_at: string;
    updated_at: string;
  }

  const PAGE = 36;

  const query = $derived(page.url.searchParams.get('q') ?? '');

  let items = $state<Post[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(false);
  let initialLoaded = $state(false);
  let error = $state<string | null>(null);
  let sentinel: HTMLElement | undefined = $state();

  let generation = 0;

  async function fetchPage(q: string, cursor: string | null, reset: boolean) {
    loading = true;
    error = null;
    const gen = ++generation;
    try {
      const params: Record<string, string | number> = { limit: PAGE };
      if (q.trim() !== '') params.q = q.trim();
      if (!reset && cursor) params.cursor = cursor;

      const { data, error: apiErr } = await api.GET('/posts', {
        params: { query: params as never },
      });

      if (gen !== generation) return;

      if (apiErr || !data) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? 'Failed to load',
        );
      }

      const pageItems = (data.items ?? []) as Post[];
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

  // Reset and refetch every time the query changes. Covers initial
  // mount AND subsequent navigations from the navbar search.
  $effect(() => {
    const q = query;
    untrack(() => {
      items = [];
      nextCursor = null;
      initialLoaded = false;
      void fetchPage(q, null, true);
    });
  });

  // Infinite scroll: rootMargin head-start so the next batch is in
  // flight before the user reaches the end.
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
      <p class="font-medium text-fg">{query ? 'No matches' : 'No posts yet'}</p>
      <p class="mt-1 text-sm">
        {query
          ? 'Try a different search term.'
          : 'Once posts are uploaded they\'ll appear here, newest first.'}
      </p>
    </div>
  {:else}
    <div class="grid grid-cols-2 gap-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 2xl:grid-cols-8">
      {#each items as post (post.id)}
        <PostCard {post} />
      {/each}

      {#if loading}
        {#each Array(8) as _, i (i)}
          <div class="aspect-square rounded-lg bg-surface-elevated border border-border animate-pulse"></div>
        {/each}
      {/if}
    </div>

    {#if hasMore}
      <div bind:this={sentinel} class="h-px w-full" aria-hidden="true"></div>
    {/if}

    {#if !hasMore && items.length > 0}
      <p class="text-center text-xs text-fg-muted py-4">— end of feed —</p>
    {/if}
  {/if}
</div>
