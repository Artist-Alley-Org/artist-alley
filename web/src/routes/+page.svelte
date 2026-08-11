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
  import ChannelsRail from '$components/ChannelsRail.svelte';
  import PostCard from '$components/PostCard.svelte';
  import type { CardCoverAsset } from '$components/cardAsset';
  import PostHost from '$components/PostHost.svelte';
  import BrowseFooter from '$components/BrowseFooter.svelte';
  import PostListTable from '$components/PostListTable.svelte';
  import ContentGrid from '$components/ContentGrid.svelte';
  import SelectionBar from '$components/SelectionBar.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import { t } from '$stores/lang.svelte';
  import { createScrollSnapshot } from '$lib/util/scrollSnapshot';

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

  // Member assets use the shared card feed contract (#595). The local
  // type this replaces declared only `id`, `file_hash?` and
  // `preview_available?` — it never mentioned file_extension or
  // thumbhash at all, and browse rendered its video / 3D badges and
  // sprite-scrub previews purely because the runtime objects carried
  // fields the type had no opinion about. That is the same silence that
  // let the collection page drop them for real.
  interface PostMember {
    asset_id: string;
    sort_order: number;
    asset: CardCoverAsset;
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

  // #891 shipped a one-line note here — "items you don't have access to
  // are hidden by your preferences", with a link to change it — because
  // hiding was an opt-in and an opted-in reader's grid was shorter than
  // the one everyone else saw. #921 made hiding the DEFAULT, and the
  // note stopped being true on both halves: it is not the reader's
  // preference (they set nothing), and the feed is not shorter than
  // anyone else's (it is the feed). A line that fires for every reader
  // on every browse paint is chrome, not an explanation, so it is gone.
  //
  // If a "there is more here you cannot open" affordance is wanted, it
  // is a fresh design question rather than this note inverted — the
  // mirror ("you are seeing work you can't open") would fire only for
  // the few who opted in, who already know.

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
      // Feed filter + direction from the BrowseFooter store.
      //
      // `filter` is now a straight pass-through to the server's typed
      // `feed` enum, because since #691 `FeedFilter` and that enum hold
      // the same two values. This used to be a MAPPING — every pill
      // went out as an undeclared `filter=` param for "observability"
      // and only `following` was translated into `feed=`, so the
      // client-only `team` and `trending` pills produced a request the
      // server read as plain `latest`. There is nothing left to map, so
      // there is nothing left to silently swallow: a pill the server
      // can't serve now fails to typecheck here.
      params.feed = browseView.filter;
      params.dir = browseView.feedDir;

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

  // Identity of the result set currently on screen. Reading all three
  // inputs here is what subscribes the effect below to them.
  //
  // The separator is U+001F (INFORMATION SEPARATOR ONE), written as an
  // escape rather than as a literal byte. It only has to be something a
  // user cannot type into the query box, so that ("a","b") and ("ab","")
  // cannot collide. It used to be a raw NUL, which did that job but also
  // made git, grep and ripgrep classify this whole file as binary —
  // searches for anything in it silently returned nothing, and every
  // diff rendered as "Binary file not shown" (#925). Do not put a raw
  // control byte back in here.
  const feedKey = () => `${query}\u001f${browseView.filter}\u001f${browseView.feedDir}`;

  /** The feedKey whose first page we've already loaded (or restored).
   *  Guards the effect against re-fetching a set we already hold —
   *  which is what makes back-navigation restoration work regardless
   *  of whether `snapshot.restore` lands before or after mount. */
  let loadedKey: string | null = null;

  // Reset and refetch every time the query, feed filter, or feed
  // direction changes.
  $effect(() => {
    const key = feedKey();
    untrack(() => {
      if (key === loadedKey) return;
      loadedKey = key;
      items = [];
      nextCursor = null;
      initialLoaded = false;
      void fetchPage(query, null, true);
    });
  });

  // Scroll + loaded-pages restoration on back-navigation (#584).
  //
  // The feed is the one surface where restoring the offset alone would
  // be actively worse than not restoring it: come back holding only
  // page 1 and a 1500px offset sits inside the sentinel's 600px
  // rootMargin, so the loader fires, the content grows, and the user is
  // parked somewhere they never were. Handing back the accumulated
  // pages puts the offset back over the same posts and leaves the
  // sentinel where it belongs — off screen.
  //
  // `restore` has no ordering guarantee against the mount effect above,
  // so it covers both: setting `loadedKey` stops a fetch that hasn't
  // started, and bumping `generation` cancels one already in flight
  // (fetchPage's own guard then drops the response — including its
  // `loading = false`, hence the explicit reset here, or the infinite
  // scroll would stay wedged).
  interface FeedSnapshot {
    key: string;
    items: Post[];
    cursor: string | null;
  }
  export const snapshot = createScrollSnapshot<FeedSnapshot>({
    capture: () => ({ key: feedKey(), items, cursor: nextCursor }),
    restore: (saved) => {
      if (!saved || saved.key !== feedKey() || saved.items.length === 0) return;
      generation++;
      items = saved.items;
      nextCursor = saved.cursor;
      loadedKey = saved.key;
      initialLoaded = true;
      loading = false;
    },
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

  <!-- Multi-select indicator (#515 slice 3). Sticky under the navbar so
       the count stays visible while scrolling a long feed; the full
       bulk-action bar is #39. Renders only while a selection is active. -->
  <div class="sticky top-2 z-30 empty:hidden">
    <SelectionBar />
  </div>

  <!-- #417 — the curated rail sits ABOVE both branches below. For a
       guest it is the entire landing page (posts are members-only);
       for a member it is a curated strip over their feed. Rendering it
       outside the guest/member split is what makes it the same surface
       for both, which is the point of curating it.

       #908 — but only when the page is BROWSE. `?q=` (the navbar search
       box submits here) turns this route into a result surface, and a
       strip of curated collections that has nothing to do with the
       query is then just unrelated content pinned above the answer.
       `query` is the route's only narrowing param, so it is the whole
       condition: everything else — the feed pill, the sort direction,
       the view mode — rearranges the same set rather than asking a
       question, and the rail belongs over all of those.

       ADR 0065 / #417 is why this is `{#if !query}` and not a
       guest/member check: unfiltered browse keeps the rail for BOTH,
       including the signed-out visitor for whom it is the only thing on
       the page. -->
  <!-- #577 — the channels rail, on the same `{#if !query}` condition as
       the featured rail above and for the same reason (#908): `?q=`
       turns this route into a result surface, and navigation chrome
       unrelated to the query is then noise pinned above the answer.

       It sits BELOW FeaturedRail deliberately. The featured strip is
       the operator's curation and is the whole page for a guest; the
       channels rail is the reader's own subscriptions and renders
       nothing at all for a guest, so putting it second keeps the
       signed-out layout identical to what it was. -->
  {#if !query}
    <FeaturedRail />
    <ChannelsRail />
  {/if}

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
        <PostCard {post} {mode} feed={mode === 'feed'} tileSizes={browseView.tileSizes} />
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
