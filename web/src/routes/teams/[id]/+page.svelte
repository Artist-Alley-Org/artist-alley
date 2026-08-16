<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * A team page (#684) — a studio's front door.
   *
   * # Posts first, assets second
   *
   * A team is a social space before it is a folder, so the default tab
   * is the posts feed and the assets tab is the "show me everything"
   * fallback. Both are the ORDINARY list endpoints with `?team_id=`
   * added, which is the important part: no team-specific read path
   * exists, so there is no second place for the visibility rules to be
   * re-expressed and drift. The server's planes decide every row, and a
   * non-member sees this studio's public work and placeholders where
   * its restricted work is — exactly what browse already shows them,
   * filtered to one team.
   *
   * # Reusing the browse components, not the browse page
   *
   * ContentGrid + PostCard + AssetCard + PostListTable + ViewControls
   * are shared, and this page composes them the same way
   * UserProfile.svelte does for an owner-scoped browse — a team page is
   * that shape with `team_id` in place of `owner_ref`.
   *
   * Worth being explicit about what is NOT reused, because it looked
   * like a risk and turned out not to be one: the feed's `feedKey()`,
   * its scroll-snapshot and its infinite-scroll observer are not shared
   * components at all. They are local to `routes/+page.svelte`. So the
   * direction handling #868 added to that machinery cannot interact
   * with this page — there is nothing here to interact with. This page
   * pages with an explicit button and owns its own cursor.
   *
   * # View mode is shared, feed filter is not
   *
   * `browseView.mode` / `tileMin` / `tileSizes` come from the global
   * store so a user who prefers masonry gets masonry here too. The
   * browse-only `filter` pill (latest / following) is deliberately not
   * read: "posts by people I follow, inside this team" is a question
   * nobody asked, and BrowseFooter (which owns that pill) is not
   * mounted here — ViewControls is.
   */
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import { teamFollows } from '$stores/teamFollows.svelte';
  import { t } from '$stores/lang.svelte';
  import ContentGrid from '$components/ContentGrid.svelte';
  import PostCard from '$components/PostCard.svelte';
  import PostListTable from '$components/PostListTable.svelte';
  import AssetCard from '$components/AssetCard.svelte';
  import ViewControls from '$components/ViewControls.svelte';
  import PostParamHost from '$components/PostParamHost.svelte';
  import TeamFollowButton from '$components/TeamFollowButton.svelte';
  import TeamAvatar from '$components/TeamAvatar.svelte';

  interface Team {
    id: string;
    slug: string;
    name: string;
    description: string;
    /** #982 — the server's re-derived render answer; absent means the
     *  header falls back to the initials tile. */
    hero_asset_id?: string | null;
  }
  interface Member {
    user_ref: number;
    username?: string | null;
    display_name?: string | null;
  }

  const PAGE = 36;
  const MEMBER_STRIP = 12;

  type Tab = 'posts' | 'assets';

  const teamId = $derived(page.params.id ?? '');

  let team = $state<Team | null>(null);
  let members = $state<Member[]>([]);
  let notFound = $state(false);
  let guest = $state(false);
  let loadingTeam = $state(true);

  let tab = $state<Tab>('posts');

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let posts = $state<any[]>([]);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let assets = $state<any[]>([]);
  let postsCursor = $state<string | null>(null);
  let assetsCursor = $state<string | null>(null);
  let postsLoaded = $state(false);
  let assetsLoaded = $state(false);
  let loadingContent = $state(false);

  async function loadTeam(id: string): Promise<void> {
    loadingTeam = true;
    notFound = false;
    guest = false;
    const { data, error } = await api.GET('/teams/{id}', { params: { path: { id } } });
    if (error || !data) {
      // A guest holds no teams.read, so a 403 here is the members-only
      // surface rather than a missing team. Distinguishing them matters:
      // "this studio does not exist" is a lie to tell a signed-out
      // visitor about a studio that does.
      if (!auth.user) guest = true;
      else notFound = true;
      loadingTeam = false;
      return;
    }
    team = data as Team;
    loadingTeam = false;

    // Members are best-effort and independent of the content below: a
    // failure here must not blank a feed that loaded fine.
    const m = await api
      .GET('/teams/{id}/members', { params: { path: { id } } })
      .catch(() => ({ data: null }));
    members = ((m.data ?? []) as Member[]).slice(0, MEMBER_STRIP);
  }

  async function loadPosts(cursor: string | null): Promise<void> {
    loadingContent = true;
    try {
      const query: Record<string, string | number> = { team_id: teamId, limit: PAGE };
      if (cursor) query.cursor = cursor;
      query.dir = browseView.feedDir;
      const { data } = await api.GET('/posts', { params: { query: query as never } });
      const items = (data?.items ?? []) as unknown[];
      posts = cursor ? [...posts, ...items] : items;
      postsCursor = (data?.next_cursor as string | null) ?? null;
    } finally {
      loadingContent = false;
      postsLoaded = true;
    }
  }

  async function loadAssets(cursor: string | null): Promise<void> {
    loadingContent = true;
    try {
      const query: Record<string, string | number> = { team_id: teamId, limit: PAGE };
      if (cursor) query.cursor = cursor;
      const { data } = await api.GET('/assets', { params: { query: query as never } });
      const items = (data?.items ?? []) as unknown[];
      assets = cursor ? [...assets, ...items] : items;
      assetsCursor = (data?.next_cursor as string | null) ?? null;
    } finally {
      loadingContent = false;
      assetsLoaded = true;
    }
  }

  onMount(() => {
    browseView.init(); // pick up the user's tile-size + mode preference
    if (auth.user) void teamFollows.load(); // so the follow pill renders correct on first paint
  });

  /** Load (or reload) everything when the route's team changes.
   *
   *  Guarded on a key rather than firing per render: `loadedTeamId`
   *  is read and written inside the effect but never in a way that
   *  re-triggers it, because both reads are of a plain `let`, not
   *  $state. Svelte 5 collects dependencies through call frames, so a
   *  $state read inside a callee WOULD subscribe this effect and loop. */
  let loadedTeamId: string | null = null;
  $effect(() => {
    const id = teamId;
    if (!id || id === loadedTeamId) return;
    loadedTeamId = id;
    team = null;
    members = [];
    posts = [];
    assets = [];
    postsCursor = null;
    assetsCursor = null;
    postsLoaded = false;
    assetsLoaded = false;
    void (async () => {
      await loadTeam(id);
      if (!notFound && !guest) await loadPosts(null);
    })();
  });

  /** Switching to the assets tab loads it once, lazily — a team page
   *  opened to read the feed should not pay for a second list nobody
   *  looked at. */
  async function selectTab(next: Tab): Promise<void> {
    tab = next;
    if (next === 'assets' && !assetsLoaded) await loadAssets(null);
  }

  /** The reversal trick UserProfile uses: sort direction is honoured
   *  client-side for assets (whose endpoint has no `dir`), and
   *  server-side for posts (whose endpoint does, since #868). Posts are
   *  therefore NOT reversed here — doing both would cancel out. */
  const sortedAssets = $derived(
    browseView.feedDir === 'asc' ? [...assets].reverse() : assets,
  );

  /** Display name, then username, then the bare ref.
   *
   *  The last rung should never be reached: listTeamMembers joins
   *  `"user"` so every row carries a username (#684). It stays as a
   *  floor rather than an assertion because a member strip that renders
   *  "#19" is a cosmetic defect, and one that renders nothing at all
   *  looks like the team has no members. */
  function memberLabel(m: Member): string {
    return m.display_name || m.username || `#${m.user_ref}`;
  }
  function initials(name: string): string {
    const parts = name.trim().split(/\s+/).slice(0, 2);
    return parts.map((p) => p.slice(0, 1).toUpperCase()).join('') || '?';
  }
</script>

<svelte:head>
  <title>{team ? `${team.name} — ${site.name}` : `${t('teams.directory_title')} — ${site.name}`}</title>
</svelte:head>

{#if guest}
  <div class="mx-auto max-w-2xl px-4 py-16 text-center">
    <h1 class="text-xl font-semibold text-fg">{t('teams.guest_title')}</h1>
    <p class="mt-2 text-sm text-fg-muted">{t('teams.guest_hint')}</p>
    <a
      href="/login"
      class="mt-4 inline-flex items-center rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent"
    >
      {t('user_menu.sign_in')}
    </a>
  </div>
{:else if notFound}
  <div class="mx-auto max-w-2xl px-4 py-16 text-center text-fg-muted">
    <h1 class="text-xl font-semibold text-fg">{t('teams.not_found')}</h1>
    <p class="mt-2">{t('teams.not_found_body')}</p>
    <a href="/teams" class="mt-4 inline-block text-sm font-medium text-accent hover:underline">
      {t('teams.browse_all')}
    </a>
  </div>
{:else if loadingTeam}
  <p class="px-4 py-16 text-center text-fg-muted">{t('common.loading')}</p>
{:else if team}
  <!-- Full-viewport width with the same px-4/sm:px-6 padding as browse,
       so the shared TileGrid resolves the SAME column count and the
       page reads as one grid system rather than a narrower island. -->
  <div class="w-full px-4 py-8 sm:px-6" data-testid="team-page">
    <header class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
      <!-- The picture leads the header (#982). It is the same
           TeamAvatar the rail and the directory use, just larger, so a
           team that has no hero shows the same initials tile here that
           the reader followed in from. -->
      <div class="flex min-w-0 gap-4">
        <TeamAvatar {team} class="h-16 w-16 rounded-xl sm:h-20 sm:w-20" textClass="text-xl" />
        <div class="min-w-0">
          <p class="text-xs text-fg-muted">
            <a href="/teams" class="hover:underline">{t('teams.directory_title')}</a>
          </p>
          <h1 class="mt-1 truncate text-2xl font-bold text-fg" data-testid="team-name">
            {team.name}
          </h1>
          <p class="text-sm text-fg-muted">@{team.slug}</p>
          {#if team.description}
            <p class="mt-2 max-w-2xl whitespace-pre-line text-sm text-fg">{team.description}</p>
          {/if}
        </div>
      </div>
      <div class="shrink-0">
        <TeamFollowButton {team} />
      </div>
    </header>

    {#if members.length}
      <!-- Member strip. Names only: a team member's avatar is a user
           concern and /teams/{id}/members does not carry one, so this
           renders what the endpoint actually returns rather than firing
           a profile request per member. -->
      <section class="mt-5" aria-labelledby="team-members-heading">
        <h2 id="team-members-heading" class="sr-only">{t('teams.members')}</h2>
        <ul class="flex flex-wrap gap-2">
          {#each members as m (m.user_ref)}
            <li>
              <a
                href={`/users/by-ref/${m.user_ref}`}
                class="flex items-center gap-2 rounded-full border border-border bg-surface-elevated py-0.5 pl-0.5 pr-3
                       text-xs text-fg transition-colors hover:border-border-strong"
              >
                <span
                  class="flex h-6 w-6 items-center justify-center rounded-full bg-state-hover text-[0.6rem] font-semibold text-fg-muted"
                  aria-hidden="true">{initials(memberLabel(m))}</span
                >
                <span class="max-w-[10rem] truncate">{memberLabel(m)}</span>
              </a>
            </li>
          {/each}
        </ul>
      </section>
    {/if}

    <!-- Tabs -->
    <div class="mt-6 flex gap-1 border-b border-border" role="tablist">
      {#each [{ id: 'posts' as Tab, label: t('teams.tab_posts') }, { id: 'assets' as Tab, label: t('teams.tab_assets') }] as tb (tb.id)}
        <button
          type="button"
          role="tab"
          aria-selected={tab === tb.id}
          data-testid={`team-tab-${tb.id}`}
          class={`-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors ${
            tab === tb.id
              ? 'border-accent text-fg'
              : 'border-transparent text-fg-muted hover:text-fg'
          }`}
          onclick={() => void selectTab(tb.id)}
        >
          {tb.label}
        </button>
      {/each}
    </div>

    <div class="mt-5">
      {#if tab === 'posts'}
        {#if postsLoaded && posts.length === 0}
          <p class="rounded-xl border border-dashed border-border p-12 text-center text-fg-muted">
            {t('teams.no_posts')}
          </p>
        {:else}
          <ContentGrid mode={browseView.mode} items={posts} tileMin={browseView.tileMin} loading={loadingContent}>
            {#snippet card(item, mode)}
              <PostCard post={item} {mode} feed={mode === 'feed'} tileSizes={browseView.tileSizes} />
            {/snippet}
            {#snippet list()}
              <PostListTable items={posts} loading={loadingContent} />
            {/snippet}
          </ContentGrid>
          {#if postsCursor}
            <div class="mt-6 text-center">
              <button
                type="button"
                class="rounded-md border border-border px-4 py-2 text-sm font-medium text-fg hover:border-border-strong disabled:opacity-60"
                onclick={() => void loadPosts(postsCursor)}
                disabled={loadingContent}
              >
                {loadingContent ? t('common.loading') : t('teams.load_more')}
              </button>
            </div>
          {/if}
        {/if}
      {:else if assetsLoaded && assets.length === 0}
        <p class="rounded-xl border border-dashed border-border p-12 text-center text-fg-muted">
          {t('teams.no_assets')}
        </p>
      {:else}
        <ContentGrid mode={browseView.mode} items={sortedAssets} tileMin={browseView.tileMin} loading={loadingContent}>
          {#snippet card(item, mode)}
            <AssetCard asset={item} {mode} tileSizes={browseView.tileSizes} />
          {/snippet}
        </ContentGrid>
        {#if assetsCursor}
          <div class="mt-6 text-center">
            <button
              type="button"
              class="rounded-md border border-border px-4 py-2 text-sm font-medium text-fg hover:border-border-strong disabled:opacity-60"
              onclick={() => void loadAssets(assetsCursor)}
              disabled={loadingContent}
            >
              {loadingContent ? t('common.loading') : t('teams.load_more')}
            </button>
          </div>
        {/if}
      {/if}
    </div>
  </div>

  <!-- The shared floating view controls (mode switcher + sort), same
       bar as browse and the profile page. No feed-filter middle —
       that's browse-only. -->
  <ViewControls />
{/if}

<!-- #1130's sweep. This page renders PostCard, whose primary click
     writes `?post=` onto this url, and nothing here consumed it — the
     same silent dead-end the collection route was filed for. Outside
     the `{#if team}` block so the host's lifetime is the route's, not
     the team fetch's.

     `ordered` is the loaded team feed; `onEndReached` pages it, the
     same contract browse uses. -->
<PostParamHost
  ordered={() => posts.map((p) => (p as { id: string }).id)}
  onEndReached={() => {
    if (postsCursor && !loadingContent) void loadPosts(postsCursor);
  }}
/>
