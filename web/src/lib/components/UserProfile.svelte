<script lang="ts">
  // Public user-profile page (#478, ADR 0070). Rendered by both
  // /users/by-username/[username] and /users/by-ref/[ref] — one shared
  // component so the two permalinks stay identical.
  //
  // A profile is a display header plus an owner-scoped browse: the
  // content grids are the standard /assets, /collections and /posts
  // list endpoints filtered by owner_ref, so the visibility predicate
  // (ADR 0063) does all the gating.
  //
  // ── Sections (#1106) ────────────────────────────────────────────────
  //
  // The page is three tabs, hosted in the SAME floating footer browse
  // uses. ViewControls has taken a `middle` snippet since #511, so the
  // seam already existed; what this adds is a profile tab set in it, and
  // FooterTabs is the control both hosts now render (see that file).
  //
  // PORTFOLIO IS POSTS, NOT UPLOADS — for a visitor.
  //
  //   "When viewing someone else's profile, we should only see their
  //    posts and only if they are not private. Not all their uploads."
  //    — owner, 2026-08-15
  //
  // The page used to fetch three grids for everyone: /posts, /assets
  // (the raw uploads) and /collections. A profile is a portfolio, not a
  // file manager, so the uploads grid is now the AUTHOR'S OWN view only.
  // Public collections stay for visitors — they are curated, not
  // uploads.
  //
  // "Only if they are not private" needed no work and is asserted as a
  // regression rather than built: /posts composes the VIEWER's read rule
  // server-side, so a stranger's page has never carried another user's
  // private posts. Dropping the uploads grid is the actual change.
  //
  // LIKES composes the VIEWER's rule too, and that is the whole design
  // of `?liked_by=`: it is a filter on the same feed the browse page
  // reads, so the read rule is ANDed onto it exactly as it is for every
  // other filter. A liked item this viewer cannot read is absent — not
  // withheld, not counted. See posts/list_page.go for why it is a
  // parameter rather than an endpoint.
  //
  // ABOUT needs no client-side gate for `hide_from_anonymous`. It is
  // enforced upstream: GetUserPublicByUsername / GetUserPublicByRefPath
  // return 404 to an anonymous caller when the owner opted out (ADR
  // 0024), and strip `fullname` for anonymous callers regardless (ADR
  // 0070 §3). An anonymous viewer who reaches this component at all is
  // one the owner admitted, so the fields below are already the ones
  // they may see. Re-checking here would be a second copy of a rule with
  // one home — and the weaker copy, since it cannot un-send a payload.
  //
  // The bio / links / member-since block LIVES in About rather than in
  // the header. Rendering it in both would make the tab a duplicate of
  // the thing above it.
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import AssetCard from '$components/AssetCard.svelte';
  import CollectionCard from '$components/CollectionCard.svelte';
  import PostCard from '$components/PostCard.svelte';
  import ContentGrid from '$components/ContentGrid.svelte';
  import PostListTable from '$components/PostListTable.svelte';
  import ViewControls from '$components/ViewControls.svelte';
  import FooterTabs from '$components/FooterTabs.svelte';
  import PostParamHost from '$components/PostParamHost.svelte';

  interface Props {
    ref?: number;
    username?: string;
  }
  let { ref, username }: Props = $props();

  let profile = $state<Record<string, any> | null>(null);
  let notFound = $state(false);
  let loading = $state(true);
  let posts = $state<any[]>([]);
  let assets = $state<any[]>([]);
  let collections = $state<any[]>([]);

  // Likes are fetched when the tab is first opened, not with the page:
  // it is two more list requests for a section most visitors never open,
  // on a page that already makes two.
  let likedPosts = $state<any[]>([]);
  let likedAssets = $state<any[]>([]);
  let likesLoaded = $state(false);
  let likesLoading = $state(false);

  type Tab = 'portfolio' | 'about' | 'likes';
  const TABS: Tab[] = ['portfolio', 'about', 'likes'];

  // The URL owns the tab, for the reasons browse's `?q=` / `?team=`
  // already do: it survives a reload, it travels in a shared link, and
  // it answers the back button. It also composes with `?post=` — the
  // viewer host deletes only its own param on close, so closing a post
  // opened from Likes returns to Likes.
  const activeTab = $derived<Tab>(
    (TABS as string[]).includes(page.url.searchParams.get('tab') ?? '')
      ? (page.url.searchParams.get('tab') as Tab)
      : 'portfolio',
  );

  async function selectTab(id: string) {
    const target = new URL(page.url);
    if (id === 'portfolio') target.searchParams.delete('tab');
    else target.searchParams.set('tab', id);
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }

  const tabs = $derived(
    TABS.map((id) => ({ id, label: t(`profile.tab.${id}`) })),
  );

  /** The author looking at their own profile. Widens Portfolio back to
   *  everything they see today — the uploads grid survives HERE and
   *  nowhere else. `auth.user` is null for an anonymous viewer, which
   *  resolves this to false without a separate branch. */
  const isSelf = $derived(!!profile && !!auth.user && auth.user.ref === profile.ref);

  async function loadProfile() {
    if (username) {
      const { data, error } = await api.GET('/users/by-username/{username}', {
        params: { path: { username } },
      });
      if (error || !data) return null;
      return data;
    }
    if (ref != null) {
      const { data, error } = await api.GET('/users/by-ref/{ref}', {
        params: { path: { ref } },
      });
      if (error || !data) return null;
      return data;
    }
    return null;
  }

  async function loadContent(ownerRef: number, self: boolean) {
    // Independent + best-effort: a members-only 401 on the posts feed
    // (anonymous viewer) must not blank the collections an anonymous
    // viewer IS allowed to see.
    //
    // The uploads request is only made for the author's own view. Not
    // requesting it is the point rather than an optimisation: the raw
    // uploads grid is off the visitor view, and a fetch whose result is
    // conditionally rendered is a grid one `{#if}` away from coming
    // back.
    const [p, c] = await Promise.all([
      api.GET('/posts', { params: { query: { author_ref: ownerRef, limit: 24 } } }).catch(() => ({ data: null })),
      api.GET('/collections', { params: { query: { owner_ref: ownerRef, limit: 24 } } }).catch(() => ({ data: null })),
    ]);
    posts = (p.data?.items ?? []) as any[];
    collections = (c.data?.items ?? []) as any[];

    if (self) {
      const a = await api
        .GET('/assets', { params: { query: { owner_ref: ownerRef, limit: 24 } } })
        .catch(() => ({ data: null }));
      assets = (a.data?.items ?? []) as any[];
    } else {
      assets = [];
    }
  }

  async function loadLikes(ownerRef: number) {
    if (likesLoaded || likesLoading) return;
    likesLoading = true;
    try {
      const [p, a] = await Promise.all([
        api.GET('/posts', { params: { query: { liked_by: ownerRef, limit: 24 } } }).catch(() => ({ data: null })),
        api.GET('/assets', { params: { query: { liked_by: ownerRef, limit: 24 } } }).catch(() => ({ data: null })),
      ]);
      likedPosts = (p.data?.items ?? []) as any[];
      likedAssets = (a.data?.items ?? []) as any[];
      likesLoaded = true;
    } finally {
      likesLoading = false;
    }
  }

  onMount(async () => {
    browseView.init(); // pick up the user's tile-size preference for the grids
    const data = await loadProfile();
    if (!data) {
      notFound = true;
      loading = false;
      return;
    }
    profile = data;
    await loadContent(data.ref, !!auth.user && auth.user.ref === data.ref);
    loading = false;
  });

  // Fetch the Likes tab the first time it is opened, whether that is a
  // click or a deep link into `?tab=likes`.
  $effect(() => {
    if (activeTab === 'likes' && profile) void loadLikes(profile.ref);
  });

  const socialEntries = $derived(
    profile?.social_links ? Object.entries(profile.social_links as Record<string, string>) : [],
  );
  const memberSince = $derived(
    profile?.member_since ? new Date(profile.member_since).getFullYear() : null,
  );
  const hasAbout = $derived(
    !!profile && (!!profile.bio || !!profile.location || !!profile.website_url || socialEntries.length > 0),
  );

  // Mode + sort come from the GLOBAL browseView store (localStorage), so
  // the profile shares the view preference with browse. SEAM: per-surface
  // view state is probably reworked in the future; the coupling to the one
  // store is the simplest-correct option for now. Sort is honored
  // client-side by reversing the (bounded, first-page) result each section
  // fetched newest-first.
  const rev = <T,>(xs: T[]): T[] => (browseView.feedDir === 'asc' ? [...xs].reverse() : xs);
  const sortedPosts = $derived(rev(posts));
  const sortedCollections = $derived(rev(collections));
  const sortedAssets = $derived(rev(assets));
  const sortedLikedPosts = $derived(rev(likedPosts));
  const sortedLikedAssets = $derived(rev(likedAssets));

  // What the viewer host walks with ← / →: whichever post grid is on
  // screen. Portfolio and Likes never render together, so this is
  // unambiguous, and on About there is no grid and the arrows go inert.
  const hostedPostIds = $derived(
    activeTab === 'likes' ? sortedLikedPosts.map((p) => p.id) : sortedPosts.map((p) => p.id),
  );

  const portfolioEmpty = $derived(!posts.length && !collections.length && !assets.length);
  const likesEmpty = $derived(likesLoaded && !likedPosts.length && !likedAssets.length);
</script>

<svelte:head>
  <title>{profile ? `${profile.display_name} — Artist Alley` : 'Profile — Artist Alley'}</title>
</svelte:head>

{#if notFound}
  <div class="mx-auto max-w-2xl px-4 py-16 text-center text-fg-muted">
    <h1 class="text-xl font-semibold text-fg">{t('profile.not_found')}</h1>
    <p class="mt-2">{t('profile.not_found_body')}</p>
  </div>
{:else if profile}
  <!-- Full-viewport width + the same px-4/sm:px-6 padding as the browse
       feed, so the shared TileGrid resolves the SAME column count (not a
       narrower capped one) and the profile reads as one grid system. -->
  <div class="w-full px-4 py-8 sm:px-6">
    <!-- Header: identity only. Bio / links / member-since moved into the
         About tab (#1106) — rendering them here as well would make that
         tab a duplicate of the block above it. -->
    <header class="flex flex-col gap-4 sm:flex-row sm:items-center">
      {#if profile.avatar_url}
        <img src={profile.avatar_url} alt={profile.display_name}
             class="h-24 w-24 rounded-full object-cover ring-1 ring-border" />
      {:else}
        <div class="flex h-24 w-24 items-center justify-center rounded-full bg-state-hover text-3xl font-semibold text-fg-muted">
          {(profile.display_name ?? '?').slice(0, 1).toUpperCase()}
        </div>
      {/if}
      <div class="min-w-0">
        <h1 class="truncate text-2xl font-bold text-fg">{profile.display_name}</h1>
        {#if profile.username}
          <p class="text-sm text-fg-muted">@{profile.username}</p>
        {/if}
        {#if profile.post_count != null}
          <p class="mt-2 text-sm text-fg-muted">{profile.post_count} {t('profile.posts')}</p>
        {/if}
      </div>
    </header>

    {#if loading}
      <p class="mt-10 text-center text-fg-muted">{t('common.loading')}</p>
    {:else if activeTab === 'about'}
      <section class="mt-10 max-w-2xl">
        <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.tab.about')}</h2>
        {#if hasAbout || memberSince}
          {#if profile.bio}
            <p class="whitespace-pre-line text-fg">{profile.bio}</p>
          {/if}
          <dl class="mt-4 space-y-2 text-sm">
            {#if profile.location}
              <div class="flex gap-2">
                <dt class="text-fg-muted">{t('profile.about.location')}</dt>
                <dd class="text-fg">{profile.location}</dd>
              </div>
            {/if}
            {#if profile.website_url}
              <div class="flex gap-2">
                <dt class="text-fg-muted">{t('profile.about.website')}</dt>
                <dd>
                  <a href={profile.website_url} rel="noopener noreferrer nofollow" target="_blank"
                     class="text-accent hover:underline">{profile.website_url.replace(/^https?:\/\//, '')}</a>
                </dd>
              </div>
            {/if}
            {#if memberSince}
              <div class="flex gap-2">
                <dt class="text-fg-muted">{t('profile.member_since')}</dt>
                <dd class="text-fg">{memberSince}</dd>
              </div>
            {/if}
          </dl>
          {#if socialEntries.length}
            <div class="mt-4 flex flex-wrap gap-3 text-sm">
              {#each socialEntries as [name, url] (name)}
                <a href={url} rel="noopener noreferrer nofollow" target="_blank"
                   class="text-accent hover:underline">{name}</a>
              {/each}
            </div>
          {/if}
        {:else}
          <p class="text-fg-muted">{t('profile.about.empty')}</p>
        {/if}
      </section>
    {:else if activeTab === 'likes'}
      {#if likesLoading && !likesLoaded}
        <p class="mt-10 text-center text-fg-muted">{t('common.loading')}</p>
      {:else}
        {#if likedPosts.length}
          <section class="mt-10">
            <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.section.liked_posts')}</h2>
            <ContentGrid mode={browseView.mode} items={sortedLikedPosts} tileMin={browseView.tileMin}>
              {#snippet card(item, mode)}
                <PostCard post={item} {mode} feed={mode === 'feed'} tileSizes={browseView.tileSizes} />
              {/snippet}
              {#snippet list()}
                <PostListTable items={sortedLikedPosts} loading={false} />
              {/snippet}
            </ContentGrid>
          </section>
        {/if}
        {#if likedAssets.length}
          <section class="mt-10">
            <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.section.liked_assets')}</h2>
            <ContentGrid mode={browseView.mode} items={sortedLikedAssets} tileMin={browseView.tileMin}>
              {#snippet card(item, mode)}
                <AssetCard asset={item} {mode} tileSizes={browseView.tileSizes} />
              {/snippet}
            </ContentGrid>
          </section>
        {/if}
        {#if likesEmpty}
          <p class="mt-10 text-center text-fg-muted">{t('profile.likes.empty')}</p>
        {/if}
      {/if}
    {:else}
      <!-- Portfolio. All sections render through the shared ContentGrid,
           so mode (grid/masonry/feed/thumbnail/list) + tile size + sort
           match the home browse. Posts carry a list table; assets and
           collections have none, so list mode falls back to the grid for
           them. -->
      {#if posts.length}
        <section class="mt-10">
          <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.section.posts')}</h2>
          <ContentGrid mode={browseView.mode} items={sortedPosts} tileMin={browseView.tileMin}>
            {#snippet card(item, mode)}
              <PostCard post={item} {mode} feed={mode === 'feed'} tileSizes={browseView.tileSizes} />
            {/snippet}
            {#snippet list()}
              <PostListTable items={sortedPosts} loading={false} />
            {/snippet}
          </ContentGrid>
        </section>
      {/if}

      {#if collections.length}
        <section class="mt-10">
          <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.section.collections')}</h2>
          <ContentGrid mode={browseView.mode} items={sortedCollections} tileMin={browseView.tileMin}>
            {#snippet card(item)}
              <CollectionCard collection={item} />
            {/snippet}
          </ContentGrid>
        </section>
      {/if}

      <!-- The raw uploads grid, the AUTHOR'S OWN view only (#1106). Not
           a hidden section on a visitor's page — the request is not made
           at all for a visitor, so there is nothing here for a stray
           `{#if}` to bring back. -->
      {#if isSelf && assets.length}
        <section class="mt-10">
          <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.section.uploads')}</h2>
          <ContentGrid mode={browseView.mode} items={sortedAssets} tileMin={browseView.tileMin}>
            {#snippet card(item, mode)}
              <AssetCard asset={item} {mode} tileSizes={browseView.tileSizes} />
            {/snippet}
          </ContentGrid>
        </section>
      {/if}

      {#if portfolioEmpty}
        <p class="mt-10 text-center text-fg-muted">{t('profile.no_content')}</p>
      {/if}
    {/if}
  </div>

  <!-- The shared floating view controls, with the profile's OWN tab set
       in the host-supplied middle (#1106) — the same seam browse injects
       its feed filter through, and the same FooterTabs control. -->
  <ViewControls>
    {#snippet middle()}
      <FooterTabs
        {tabs}
        active={activeTab}
        label={t('profile.tabs.label')}
        onSelect={selectTab}
      />
    {/snippet}
  </ViewControls>

  <!-- #1130's sweep. The profile grids render PostCard, whose primary
       click writes `?post=` onto this url, and nothing here consumed it.

       Declared in this COMPONENT rather than in the two routes that use
       it (/users/by-username, /users/by-ref) on purpose: it is the whole
       body of both, it has no dialog ancestor, so this IS route level —
       and one declaration keeps the two permalinks identical, which is
       the reason this component exists at all. ADR 0067's portal rule is
       what makes "no dialog ancestor" the thing to check. -->
  <PostParamHost ordered={() => hostedPostIds} />
{/if}
