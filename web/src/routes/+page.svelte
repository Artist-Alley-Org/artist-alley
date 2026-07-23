<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { untrack, onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import FeaturedRail from '$components/FeaturedRail.svelte';
  import PostCard from '$components/PostCard.svelte';
  import PostHost from '$components/PostHost.svelte';
  import BrowseFooter from '$components/BrowseFooter.svelte';
  import PostListTable from '$components/PostListTable.svelte';
  import ContentGrid from '$components/ContentGrid.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import { t } from '$stores/lang.svelte';

  onMount(() => { browseView.init(); });

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
    preview_available?: boolean;
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
  // Signed-out visitor on the members-only feed — an expected state,
  // deliberately kept distinct from `error`.
  let guestFeed = $state(false);
  let sentinel: HTMLElement | undefined = $state();

  let generation = 0;

  async function fetchPage(q: string, cursor: string | null, reset: boolean) {
    loading = true;
    error = null;
    guestFeed = false;
    const gen = ++generation;
    try {
      const params: Record<string, string | number> = { limit: PAGE };
      if (q.trim() !== '') params.q = q.trim();
      if (!reset && cursor) params.cursor = cursor;
      // Feed filter + direction from the BrowseFooter store. The
      // backend's `feed` enum currently accepts `latest` + `following`
      // (Phase 1.17.G2); `team` and `trending` are still client-only
      // pills that the server treats as `latest` until those phases
      // land. The full `filter` value still goes out for
      // observability — server logs which pill the user hit.
      params.filter = browseView.filter;
      params.dir = browseView.feedDir;
      // Map the client pill onto the server's typed `feed` enum.
      // Unknown pills fall through to `latest` (the default), so a
      // pre-1.17.G2 client sending `following` to a server without
      // that enum value still gets a sensible page.
      if (browseView.filter === 'following') {
        params.feed = 'following';
      }

      const { data, error: apiErr } = await api.GET('/posts', {
        params: { query: params as never },
      });

      if (gen !== generation) return;

      if (apiErr || !data) {
        // A signed-out visitor gets 401 here and that is EXPECTED
        // (#416). `/` is a public route so a guest can reach the site
        // root, but its only data source is the posts feed, and posts
        // stay members-only — the followers visibility tier is not
        // modelled in the predicate yet. Rendering "authentication
        // required" in a red alert told a guest something had broken.
        // Nothing had; they are simply looking at a members-only
        // surface, so it gets an empty state, not an error.
        if (!auth.user) {
          guestFeed = true;
          return;
        }
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? t('common.failed_to_load'),
        );
      }

      const pageItems = (data.items ?? []) as Post[];
      items = reset ? pageItems : [...items, ...pageItems];
      nextCursor = (data.next_cursor as string | null) ?? null;
    } catch (e) {
      error = e instanceof Error ? e.message : t('common.failed_to_load');
    } finally {
      if (gen === generation) {
        loading = false;
        initialLoaded = true;
      }
    }
  }

  // Reset and refetch every time the query, feed filter, or feed
  // direction changes. The read of all three inside the effect body
  // (outside untrack) is what subscribes us to them.
  $effect(() => {
    const q = query;
    // Touch the filter + direction so the effect re-runs on switch.
    browseView.filter;
    browseView.feedDir;
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
  // guestFeed has its own empty state below; without this the generic
  // "nothing here yet" block would render underneath it.
  const showEmpty = $derived(initialLoaded && items.length === 0 && !error && !guestFeed);

  // ?post={uuid} → overlay the PostModal on top of the feed. The
  // feed stays mounted (no scroll loss, no re-fetch). PostCard's
  // click handler sets this param via goto(); the modal's onClose
  // clears it.
  const modalPostId = $derived(page.url.searchParams.get('post'));

  async function closeModal() {
    const target = new URL(page.url);
    target.searchParams.delete('post');
    await goto(target.pathname + target.search, {
      keepFocus: true,
      noScroll: true,
    });
  }

  // ← / → inside an open post overlay jumps to the prev / next post
  // in the current feed page. Two corners of UX nuance:
  //   1. If the current post id isn't in items (deep-linked from
  //      somewhere outside this feed), we no-op rather than guess.
  //   2. At the end of the feed, if there's a next cursor, we kick
  //      off a fetchPage() so navigation can spill into the next
  //      page — the user sees the new post as soon as it arrives.
  //      We don't await it; the keypress is fire-and-forget.
  async function navigateToSibling(dir: 'prev' | 'next') {
    if (!modalPostId) return;
    const idx = items.findIndex((p) => p.id === modalPostId);
    if (idx < 0) return;
    const targetIdx = dir === 'next' ? idx + 1 : idx - 1;
    if (targetIdx < 0) return;
    if (targetIdx >= items.length) {
      // Past the end — fetch the next page if we can; the new post
      // doesn't auto-open (we don't know which id is "next" until
      // the fetch resolves), so this only matters when the user
      // presses → again after the page lands.
      if (nextCursor && !loading) {
        void fetchPage(query, nextCursor, false);
      }
      return;
    }
    const target = new URL(page.url);
    target.searchParams.set('post', items[targetIdx].id);
    await goto(target.pathname + target.search, {
      keepFocus: true,
      noScroll: true,
    });
  }
</script>

<svelte:head>
  <title>{query ? `${t('browse.title_search', { query })} — ${site.name}` : `${t('browse.title')} — ${site.name}`}</title>
</svelte:head>

<div class="w-full px-4 py-4 space-y-4 sm:px-6">
  {#if query}
    <p class="text-sm text-fg-muted">
      {t('browse.results_for', { query })}
    </p>
  {/if}

  <!-- #417 — the curated rail sits ABOVE both branches below. For a
       guest it is the entire landing page (posts are members-only);
       for a member it is a curated strip over their feed. Rendering it
       outside the guest/member split is what makes it the same surface
       for both, which is the point of curating it. -->
  <FeaturedRail />

  {#if guestFeed}
    <!-- Calm empty state, not an alert. See the !auth.user branch in
         the loader. -->
    <div
      class="rounded-xl border border-dashed border-border p-12 text-center"
      data-testid="guest-feed-empty"
    >
      <p class="text-base font-medium text-fg">{t('user_menu.guest_feed_title')}</p>
      <p class="mx-auto mt-1 max-w-md text-sm text-fg-muted">{t('user_menu.guest_feed_hint')}</p>
      <div class="mt-4 flex flex-wrap items-center justify-center gap-2">
        <a
          href="/collections"
          class="inline-flex items-center rounded-md border border-border px-4 py-2 text-sm font-medium text-fg hover:border-border-strong"
        >
          {t('user_menu.guest_browse_collections')}
        </a>
        <a
          href="/login"
          class="inline-flex items-center rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent"
        >
          {t('user_menu.sign_in')}
        </a>
      </div>
    </div>
  {:else if error}
    <div role="alert" class="rounded-md border border-danger/40 bg-danger-container px-4 py-3 text-sm text-danger">
      {error}
    </div>
  {/if}

  {#if showEmpty}
    <div class="rounded-xl border border-dashed border-border p-12 text-center text-fg-muted">
      <p class="font-medium text-fg">{query ? t('browse.empty.no_matches') : t('browse.empty.no_posts_yet')}</p>
      <p class="mt-1 text-sm">
        {query
          ? t('browse.empty.try_different')
          : t('browse.empty.uploaded_appear_here')}
      </p>
    </div>
  {:else}
    <!--
      Layout is driven by browseView (footer switcher + localStorage).
        grid / thumbnail → auto-fill grid, tiles ≥ --tile-min
        masonry          → multi-column flow, columns ≥ --tile-min
        feed             → single column, image full-bleed
        list             → sortable table
      `--tile-min` is set inline because it's user state, not a design
      token — it changes per interaction, so it can't be a class.
      Column COUNT is never computed: see browseView.svelte.ts.
    -->
    <ContentGrid mode={browseView.mode} {items} tileMin={browseView.tileMin} {loading}>
      {#snippet card(item, mode)}
        {@const post = item as Post}
        <PostCard {post} feed={mode === 'feed'} tileSizesLen={browseView.tileSizesLen} />
      {/snippet}
      {#snippet list()}
        <PostListTable {items} {loading} />
      {/snippet}
    </ContentGrid>

    {#if hasMore}
      <div bind:this={sentinel} class="h-px w-full" aria-hidden="true"></div>
    {/if}

    {#if !hasMore && items.length > 0}
      <p class="text-center text-xs text-fg-muted py-4">{t('browse.end_of_feed')}</p>
    {/if}
  {/if}
</div>

{#if modalPostId}
  <PostHost
    postId={modalPostId}
    onClose={closeModal}
    onNavigateSibling={navigateToSibling}
  />
{/if}

<!-- Floating browse controls: view switcher + back-to-top. Stays
     mounted alongside the feed so the user can change layouts without
     losing scroll position. -->
<BrowseFooter />

<!-- The grid / masonry / feed / list layouts moved to the shared
     ContentGrid component (#511) so the profile + post-by-asset pages
     render modes identically. No page-local layout CSS remains here. -->
