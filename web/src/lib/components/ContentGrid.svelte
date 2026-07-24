<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The shared browse-view render switch (#511). One place decides how a
  // wall of cards is laid out for each browseView.mode, so every
  // asset-showing surface — the home browse feed, the profile pages, the
  // post-by-asset lookup — renders modes identically instead of forking
  // the switch per page:
  //   grid / thumbnail → auto-fill TileGrid (tiles ≥ --tile-min)
  //   masonry          → multi-column flow (columns ≥ --tile-min)
  //   feed             → single column, image full-bleed (a `measure` cap)
  //   list             → the caller's table (`list` snippet); posts only,
  //                       so anything without one falls back to the grid
  //
  // Card-agnostic: the caller supplies a `card` snippet rendered per item
  // and given the active mode, so per-mode card props (e.g. PostCard's
  // `feed`) stay the caller's business. Sorting is the caller's too — this
  // renders `items` in the order given.
  import type { Snippet } from 'svelte';
  import type { ViewMode } from '$stores/browseView.svelte';
  import TileGrid from '$components/TileGrid.svelte';

  interface Props {
    mode: ViewMode;
    items: Array<{ id: string }>;
    /** Minimum tile width as a CSS length (browseView.tileMin). */
    tileMin?: string;
    /** Show loading skeletons after the items. */
    loading?: boolean;
    /** Renders one card, given (item, activeMode). `item` is loosely
     *  typed — the API list rows differ per surface (Post / Asset /
     *  Collection), and each caller passes the row straight to its card. */
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    card: Snippet<[any, ViewMode]>;
    /** Whole-list table for `list` mode. Absent ⇒ list falls back to grid
     *  (assets/collections have no list table today). */
    list?: Snippet;
  }
  let { mode, items, tileMin = '22rem', loading = false, card, list }: Props = $props();
</script>

{#if mode === 'list' && list}
  {@render list()}
{:else if mode === 'masonry'}
  <div class="posts-masonry" style="--tile-min: {tileMin}">
    {#each items as item (item.id)}
      <div class="mb-2 break-inside-avoid">{@render card(item, 'masonry')}</div>
    {/each}
    {#if loading}
      {#each Array(8) as _, i (i)}
        <div class="mb-2 break-inside-avoid aspect-square rounded-lg bg-surface-elevated border border-border animate-pulse"></div>
      {/each}
    {/if}
  </div>
{:else if mode === 'feed'}
  <div class="posts-feed gap-4">
    {#each items as item (item.id)}{@render card(item, 'feed')}{/each}
    {#if loading}
      {#each Array(3) as _, i (i)}
        <div class="aspect-square rounded-lg bg-surface-elevated border border-border animate-pulse"></div>
      {/each}
    {/if}
  </div>
{:else}
  <!-- grid / thumbnail — and list with no table falls through here.
       Grid is a zero-gap contact sheet (#555): tiles butt edge-to-edge
       into one unbroken wall. Thumbnail keeps its gutter — it's a
       "details" view, not a contact sheet. -->
  <TileGrid {tileMin} class={mode === 'grid' ? 'gap-0' : 'gap-2'}>
    {#each items as item (item.id)}{@render card(item, mode)}{/each}
    {#if loading}
      {#each Array(8) as _, i (i)}
        <div
          class="aspect-square bg-surface-elevated animate-pulse
                 {mode === 'grid' ? '' : 'rounded-lg border border-border'}"
        ></div>
      {/each}
    {/if}
  </TileGrid>
{/if}

<style>
  /* Masonry's analogue of auto-fill: `column-width` is a MINIMUM, and
     the browser fits as many columns as it can. Same lever, same
     token, no `column-count` to guess. */
  :global(.posts-masonry) {
    column-width: min(var(--tile-min, 22rem), 100%);
    column-gap: 0.5rem;
  }
  /* feed is the honest floor of the same scale rather than a special
     case: one column at every width, image at full column width.
   *
   * The max-width is a MEASURE (same argument as prose's 65ch). Measured
   * at 3840x1080 an uncapped feed column is 3762px wide, so its square
   * media box is 3760px TALL — 3.5x the viewport. The cap is what makes
   * "one column" mean something at 32:9. 46rem stays under a 1080px-tall
   * viewport with room for chrome, and phones never reach it (390px <
   * 736px), so there `min()` resolves to 100% and the image is genuinely
   * full-bleed. */
  :global(.posts-feed) {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    max-width: min(100%, 46rem);
    margin-inline: auto;
  }
</style>
