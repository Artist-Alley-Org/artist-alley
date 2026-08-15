<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The shared browse-view render switch (#511). One place decides how a
  // wall of cards is laid out for each browseView.mode, so every
  // asset-showing surface — the home browse feed, the profile pages, the
  // post-by-asset lookup — renders modes identically instead of forking
  // the switch per page:
  //   grid / thumbnail → auto-fill TileGrid (tiles ≥ --tile-min)
  //   masonry          → append-stable explicitly-placed grid (MasonryColumns)
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
  import MasonryColumns from '$components/MasonryColumns.svelte';

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
  <!-- Masonry is no longer a CSS multi-column flow (#651). Multicol
       BALANCES across the whole flow, so every infinite-scroll append
       re-sorted tiles the user was already looking at into different
       columns. It is no longer N sibling column boxes either (#747):
       those had no shared coordinate space, so nothing could straddle
       two columns. MasonryColumns owns the replacement mechanism and
       the full argument. -->
  <MasonryColumns {items} {tileMin} {loading} {card} />
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
       Grid is a ZERO-GAP contact sheet: tiles butt edge-to-edge into one
       unbroken wall (#555/#560). Thumbnail keeps its gutter — it's a
       "details" view, not a contact sheet.

       #561 briefly made this a uniform 8px gutter on a misreading of the
       reference; the owner rejected it and it is back. What makes zero
       gap safe now is CardThumb's boundary ring, added in #590: it is an
       inset ring on all four edges, and it is STRONGER on unframed tiles
       (black/0.12 · white/0.10) than on framed ones (0.07 · 0.06) — so
       grid, the one mode that has no gutter to separate tiles, gets the
       strongest separation. Two adjacent white-artwork tiles are divided
       by a hairline instead of by empty space. Don't restore a gutter
       here without checking that case first. -->
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
  /* Masonry's own CSS moved into MasonryColumns.svelte with the layout
     mechanism it belongs to (#651). Two notes worth carrying forward:
   *
   * #637 was `column-width: min(22rem, 100%)` — a percentage anywhere in
   * that expression makes it invalid at computed-value time, the
   * declaration falls back to `auto`, and `auto` with no `column-count`
   * is ONE full-width column. Five days of single-column masonry. The
   * column count is now computed in JS from a probe, so the trap is
   * gone, but the same `min(…, 100%)` guard IS valid on the probe's
   * plain `width` and is used there.
   *
   * The column-count formula the probe feeds —
   * `max(1, floor((available + gap) / (tile-min + gap)))` — is multicol's
   * own, deliberately, so the `--tile-min` ladder in browseView keeps
   * producing the column counts its rungs were measured against. */

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
