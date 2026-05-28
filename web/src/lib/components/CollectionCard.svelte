<script lang="ts">
  // Hub tile for a single collection. Composes a 2×2 mosaic cover
  // from the collection's first 4 assets, with graceful fallbacks
  // (1 = full bleed, 2 = side-by-side, 3 = big + 2 small, 0 = icon).
  //
  // Thumbnail asset ids come from the shared collectionCovers store
  // so toggling tabs on the hub doesn't re-fetch every card.

  import { onMount } from 'svelte';
  import { fetchCovers } from '$stores/collectionCovers.svelte';
  import { t } from '$stores/lang.svelte';

  interface Collection {
    id: string;
    name: string;
    description: string;
    visibility: string;
    featured: boolean;
    owner_user_ref: number;
    created_at: string;
  }

  interface Props {
    collection: Collection;
  }

  let { collection }: Props = $props();

  interface CoverAsset {
    id: string;
    file_hash: string | null;
  }

  let covers = $state<CoverAsset[]>([]);
  let loaded = $state(false);

  onMount(async () => {
    covers = await fetchCovers(collection.id);
    loaded = true;
  });

  function colUrl(a: CoverAsset): string {
    return `/api/v1/assets/${a.id}/variants/col`;
  }
  function fullUrl(a: CoverAsset): string {
    return `/api/v1/assets/${a.id}/file`;
  }

  function handleImgError(e: Event, a: CoverAsset) {
    const img = e.currentTarget as HTMLImageElement;
    if (!img.dataset.fallback) {
      img.dataset.fallback = '1';
      img.src = fullUrl(a);
      return;
    }
    img.style.visibility = 'hidden';
  }

  const visibilityLabel = $derived(
    collection.visibility === 'public'
      ? t('collections.vis_public')
      : collection.visibility === 'shared'
        ? t('collections.vis_shared')
        : t('collections.vis_private'),
  );
</script>

<a
  href="/collections/{collection.id}"
  class="group block overflow-hidden rounded-xl border border-border bg-surface-elevated transition-colors hover:border-fg-muted/60"
>
  <div class="relative aspect-[4/3] bg-surface">
    {#if !loaded}
      <div class="absolute inset-0 animate-pulse bg-surface"></div>
    {:else if covers.length === 0}
      <div class="absolute inset-0 flex items-center justify-center text-fg-muted/40">
        <svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
        </svg>
      </div>
    {:else if covers.length === 1}
      <img src={colUrl(covers[0])} alt="" class="absolute inset-0 h-full w-full object-cover" loading="lazy" onerror={(e) => handleImgError(e, covers[0])} />
    {:else if covers.length === 2}
      <div class="absolute inset-0 grid grid-cols-2 gap-0.5">
        {#each covers as a (a.id)}
          <img src={colUrl(a)} alt="" class="h-full w-full object-cover" loading="lazy" onerror={(e) => handleImgError(e, a)} />
        {/each}
      </div>
    {:else if covers.length === 3}
      <div class="absolute inset-0 grid grid-cols-2 grid-rows-2 gap-0.5">
        <img src={colUrl(covers[0])} alt="" class="row-span-2 h-full w-full object-cover" loading="lazy" onerror={(e) => handleImgError(e, covers[0])} />
        <img src={colUrl(covers[1])} alt="" class="h-full w-full object-cover" loading="lazy" onerror={(e) => handleImgError(e, covers[1])} />
        <img src={colUrl(covers[2])} alt="" class="h-full w-full object-cover" loading="lazy" onerror={(e) => handleImgError(e, covers[2])} />
      </div>
    {:else}
      <div class="absolute inset-0 grid grid-cols-2 grid-rows-2 gap-0.5">
        {#each covers.slice(0, 4) as a (a.id)}
          <img src={colUrl(a)} alt="" class="h-full w-full object-cover" loading="lazy" onerror={(e) => handleImgError(e, a)} />
        {/each}
      </div>
    {/if}

    {#if collection.featured}
      <span class="absolute left-2 top-2 inline-flex items-center gap-1 rounded-full bg-accent/90 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm">
        <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="currentColor" stroke="none"><path d="M12 2 14.39 8.26 21 9.27l-4.91 4.79L17.46 21 12 17.77 6.54 21 7.91 14.06 3 9.27l6.61-1.01z" /></svg>
        {t('collections.featured')}
      </span>
    {/if}

    {#if collection.visibility !== 'private'}
      <span class="absolute right-2 top-2 rounded-full bg-black/55 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm">
        {visibilityLabel}
      </span>
    {/if}
  </div>

  <div class="p-3">
    <h3 class="line-clamp-1 text-sm font-semibold text-fg group-hover:text-accent">
      {collection.name || t('collections.untitled')}
    </h3>
    {#if collection.description}
      <p class="mt-0.5 line-clamp-2 text-xs text-fg-muted">{collection.description}</p>
    {/if}
  </div>
</a>
