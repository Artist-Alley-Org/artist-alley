<script lang="ts">
  // Public user-profile page (#478, ADR 0070). Rendered by both
  // /users/by-username/[username] and /users/by-ref/[ref] — one shared
  // component so the two permalinks stay identical.
  //
  // A profile is a display header plus an owner-scoped browse: the
  // content grids are the standard /assets, /collections and /posts
  // list endpoints filtered by owner_ref, so the visibility predicate
  // (ADR 0063) does all the gating. An anonymous viewer (public mode on)
  // sees only public work; posts are members-only, so anonymous viewers
  // simply get an empty posts section.
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import AssetCard from '$components/AssetCard.svelte';
  import CollectionCard from '$components/CollectionCard.svelte';
  import PostCard from '$components/PostCard.svelte';
  import TileGrid from '$components/TileGrid.svelte';

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

  async function loadContent(ownerRef: number) {
    // All three are best-effort + independent: a members-only 401 on the
    // posts feed (anonymous viewer) must not blank the assets/collections
    // an anonymous viewer IS allowed to see.
    const [p, a, c] = await Promise.all([
      api.GET('/posts', { params: { query: { author_ref: ownerRef, limit: 24 } } }).catch(() => ({ data: null })),
      api.GET('/assets', { params: { query: { owner_ref: ownerRef, limit: 24 } } }).catch(() => ({ data: null })),
      api.GET('/collections', { params: { query: { owner_ref: ownerRef, limit: 24 } } }).catch(() => ({ data: null })),
    ]);
    posts = (p.data?.items ?? []) as any[];
    assets = (a.data?.items ?? []) as any[];
    collections = (c.data?.items ?? []) as any[];
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
    await loadContent(data.ref);
    loading = false;
  });

  const socialEntries = $derived(
    profile?.social_links ? Object.entries(profile.social_links as Record<string, string>) : [],
  );
  const memberSince = $derived(
    profile?.member_since ? new Date(profile.member_since).getFullYear() : null,
  );
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
    <!-- Header -->
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
        {#if profile.bio}
          <p class="mt-2 max-w-2xl whitespace-pre-line text-fg">{profile.bio}</p>
        {/if}
        <div class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-fg-muted">
          {#if profile.location}<span>{profile.location}</span>{/if}
          {#if profile.website_url}
            <a href={profile.website_url} rel="noopener noreferrer nofollow" target="_blank"
               class="text-accent hover:underline">{profile.website_url.replace(/^https?:\/\//, '')}</a>
          {/if}
          {#if memberSince}<span>{t('profile.member_since')} {memberSince}</span>{/if}
          {#if profile.post_count != null}<span>{profile.post_count} {t('profile.posts')}</span>{/if}
        </div>
        {#if socialEntries.length}
          <div class="mt-2 flex flex-wrap gap-3 text-sm">
            {#each socialEntries as [name, url] (name)}
              <a href={url} rel="noopener noreferrer nofollow" target="_blank"
                 class="text-accent hover:underline">{name}</a>
            {/each}
          </div>
        {/if}
      </div>
    </header>

    {#if loading}
      <p class="mt-10 text-center text-fg-muted">{t('common.loading')}</p>
    {:else}
      <!-- All three sections render through the shared browse TileGrid,
           so tile size / columns / spacing / responsive behaviour are
           identical to the home browse and inherit the user's tile-size
           preference. -->
      {#if posts.length}
        <section class="mt-10">
          <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.section.posts')}</h2>
          <TileGrid tileMin={browseView.tileMin}>
            {#each posts as post (post.id)}<PostCard {post} tileSizesLen={browseView.tileSizesLen} />{/each}
          </TileGrid>
        </section>
      {/if}

      {#if collections.length}
        <section class="mt-10">
          <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.section.collections')}</h2>
          <TileGrid tileMin={browseView.tileMin}>
            {#each collections as collection (collection.id)}<CollectionCard {collection} />{/each}
          </TileGrid>
        </section>
      {/if}

      {#if assets.length}
        <section class="mt-10">
          <h2 class="mb-3 text-lg font-semibold text-fg">{t('profile.section.assets')}</h2>
          <TileGrid tileMin={browseView.tileMin}>
            {#each assets as asset (asset.id)}<AssetCard {asset} />{/each}
          </TileGrid>
        </section>
      {/if}

      {#if !posts.length && !collections.length && !assets.length}
        <p class="mt-10 text-center text-fg-muted">{t('profile.no_content')}</p>
      {/if}
    {/if}
  </div>
{/if}
