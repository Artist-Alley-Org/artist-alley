<script lang="ts">
  // Post-by-asset lookup (#478 slice-2, ADR 0070). Resolves the posts
  // that feature an asset — the target of SimilarAssetsPanel's
  // "featured in" link. Visibility-filtered server-side (anonymous sees
  // only public posts). When exactly one post is visible we redirect
  // straight to it; otherwise we list them through the SAME shared
  // TileGrid + PostCard the browse feed uses.
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import TileGrid from '$components/TileGrid.svelte';
  import PostCard from '$components/PostCard.svelte';

  let posts = $state<any[]>([]);
  let loading = $state(true);

  onMount(async () => {
    browseView.init(); // inherit the user's tile-size preference
    const id = page.params.id ?? '';
    const { data } = await api.GET('/posts/by-asset/{id}', {
      params: { path: { id } },
    });
    const items = (data?.items ?? []) as any[];
    if (items.length === 1) {
      // Exactly one visible post → go straight to it (ADR 0070 §4).
      await goto(`/posts/${items[0].id}`, { replaceState: true });
      return;
    }
    posts = items;
    loading = false;
  });
</script>

<svelte:head>
  <title>{t('post_by_asset.title')} — Artist Alley</title>
</svelte:head>

<div class="w-full px-4 py-8 sm:px-6">
  <h1 class="mb-4 text-xl font-bold text-fg">{t('post_by_asset.heading')}</h1>
  {#if loading}
    <p class="mt-6 text-center text-fg-muted">{t('common.loading')}</p>
  {:else if posts.length === 0}
    <p class="mt-6 text-center text-fg-muted">{t('post_by_asset.none')}</p>
  {:else}
    <TileGrid tileMin={browseView.tileMin}>
      {#each posts as post (post.id)}<PostCard {post} tileSizesLen={browseView.tileSizesLen} />{/each}
    </TileGrid>
  {/if}
</div>
