<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Hub tile for a single collection. Lays out a 2×2 mosaic cover from
  // the collection's `covers`, with graceful fallbacks (1 = full bleed,
  // 2 = side-by-side, 3 = big + 2 small, 0 = icon).
  //
  // The covers RIDE THE ROW (#1026). They used to come from a
  // client-side store that fetched /collections/{id}/resources per card,
  // which had two defects no amount of caching fixed: it could not see
  // POST members at all — so a collection of saved posts rendered as an
  // empty folder — and it slotted a member the caller may not picture as
  // a blank tile, crowding out renderable members behind it. Both are
  // decisions the server has to make, so the server makes them; every
  // entry here is renderable and there is nothing to probe or branch on.

  import { t } from '$stores/lang.svelte';
  import { objectPosition } from '$lib/util/featuredCrop';

  interface CoverEntry {
    asset_id: string;
    /** A CONTAIN rung exists for this asset (#1207). Decides which
     *  source a focal-pointed cover is painted from — see coverSrc. */
    preview_available?: boolean;
  }

  interface Collection {
    id: string;
    name: string;
    description: string;
    visibility: string;
    owner_user_ref: number;
    created_at: string;
    // Absent on a surface that did not compose covers; an empty array
    // is the honest "this collection has nothing to show".
    covers?: CoverEntry[];
    // #1207 — where the curator put the crop, as fractions of the
    // original picture. Null means centre, which is what every card did
    // before this existed.
    cover_focal_x?: number | null;
    cover_focal_y?: number | null;
  }

  interface Props {
    collection: Collection;
  }

  let { collection }: Props = $props();

  const covers = $derived(collection.covers ?? []);

  function colUrl(a: CoverEntry): string {
    return `/api/v1/assets/${a.asset_id}/variants/col`;
  }

  // ── The single chosen cover, and its focal point (#1207) ───────────
  //
  // ⚠️ THE SOURCE HAS TO CHANGE WITH THE FOCAL POINT. `col` is
  // `fit: cover` at 320px — a 320x320 CENTRE-CROP — so by the time the
  // browser sees it the picture's edges are already gone. Applying
  // `object-position` to it does not move the crop the curator chose;
  // it takes a second crop of the server's crop and lands somewhere
  // nobody picked. The fractions are stored against the ORIGINAL, so the
  // source has to still BE the original shape: the `preview` contain
  // rung.
  //
  // Both conditions are required, and each falls back cleanly:
  //   - no focal point  → `col`, centred. What every card always did.
  //   - no contain rung → `col`, centred, because there is nothing else
  //                       to paint and a 404 would be worse than a
  //                       centred crop.
  //
  // MOSAIC TILES ARE UNTOUCHED. A focal point is a statement about ONE
  // picture filling the tile; there is no meaningful place to apply it
  // across two or four, and #1026's mosaic is a summary rather than a
  // composition anyone framed.
  const hasFocal = $derived(
    collection.cover_focal_x != null && collection.cover_focal_y != null,
  );
  const singleCover = $derived(covers.length === 1 ? covers[0] : null);
  const singleUsesContainRung = $derived(
    singleCover !== null && hasFocal && singleCover.preview_available === true,
  );
  const singleSrc = $derived(
    singleCover === null
      ? ''
      : singleUsesContainRung
        ? `/api/v1/assets/${singleCover.asset_id}/variants/preview`
        : colUrl(singleCover),
  );
  // Centre unless BOTH the focal point and the rung that makes it
  // meaningful are present — the same helper the cover editor's preview
  // and the featured rail use, so "null means centre" is decided once.
  const singlePosition = $derived(
    singleUsesContainRung
      ? objectPosition(collection.cover_focal_x, collection.cover_focal_y)
      : '50% 50%',
  );

  const visibilityLabel = $derived(
    collection.visibility === 'public'
      ? t('collections.vis_public')
      : collection.visibility === 'shared'
        ? t('collections.vis_shared')
        : t('collections.vis_private'),
  );
</script>

<!-- One mosaic tile. No availability branch: the server only sends
     entries this caller can render. -->
{#snippet cover(a: CoverEntry, cls: string)}
  <img src={colUrl(a)} alt="" class={cls} loading="lazy" />
{/snippet}

<a
  href="/collections/{collection.id}"
  class="group block overflow-hidden rounded-xl border border-border bg-surface-elevated transition-colors hover:border-fg-muted/60"
>
  <div class="relative aspect-[4/3] bg-surface">
    {#if covers.length === 0}
      <div class="absolute inset-0 flex items-center justify-center text-fg-muted/40">
        <svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
        </svg>
      </div>
    {:else if covers.length === 1}
      <!-- Not `{@render cover(...)}`: this is the one tile that carries
           a curator-chosen crop, so it needs its own source and its own
           object-position. The mosaic snippet stays exactly as it was. -->
      <img
        src={singleSrc}
        alt=""
        loading="lazy"
        data-testid="collection-card-cover"
        data-focal={singleUsesContainRung ? 'on' : 'off'}
        class="absolute inset-0 h-full w-full object-cover"
        style="object-position: {singlePosition}"
      />
    {:else if covers.length === 2}
      <div class="absolute inset-0 grid grid-cols-2 gap-0.5">
        {#each covers as a (a.asset_id)}
          {@render cover(a, 'h-full w-full object-cover')}
        {/each}
      </div>
    {:else if covers.length === 3}
      <div class="absolute inset-0 grid grid-cols-2 grid-rows-2 gap-0.5">
        {@render cover(covers[0], 'row-span-2 h-full w-full object-cover')}
        {@render cover(covers[1], 'h-full w-full object-cover')}
        {@render cover(covers[2], 'h-full w-full object-cover')}
      </div>
    {:else}
      <div class="absolute inset-0 grid grid-cols-2 grid-rows-2 gap-0.5">
        {#each covers.slice(0, 4) as a (a.asset_id)}
          {@render cover(a, 'h-full w-full object-cover')}
        {/each}
      </div>
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
