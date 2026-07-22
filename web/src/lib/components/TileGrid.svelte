<script lang="ts">
  // The one browse tile-grid, extracted so every surface that shows a
  // wall of cards renders through the SAME responsive system instead of
  // proliferating one-off `grid-cols-*` grids. Used by the home browse
  // (grid / thumbnail mode) and the user-profile content sections.
  //
  // `--tile-min` is a SIZE the caller picks (browseView.tileMin); the
  // column count is whatever fits. 390px → 1 column, 1920px → 5,
  // 3840px → 10, 32:9 → however many — no breakpoint, no resize
  // listener, no width written down. The caller supplies the cards as
  // children, so the grid stays card-agnostic (PostCard, AssetCard,
  // CollectionCard all drop in).
  import type { Snippet } from 'svelte';

  interface Props {
    /** Minimum tile width as a CSS length (e.g. browseView.tileMin). */
    tileMin?: string;
    /** Extra classes on the grid element (e.g. a different gap). */
    class?: string;
    children: Snippet;
  }
  let { tileMin = '22rem', class: cls = 'gap-2', children }: Props = $props();
</script>

<div class="tile-grid {cls}" style="--tile-min: {tileMin}">
  {@render children()}
</div>

<style>
  /* The whole responsive story in one declaration.
   *
   * auto-fill, NOT auto-fit: WebKit bug 256047 collapses auto-fit tracks
   * under inline-size containment, and this grid is exactly the shape
   * that triggers it. The two differ only when a row is underfull —
   * auto-fit collapses the empty tracks, so a lone card would stretch
   * across the whole 3840px row. auto-fill keeps them, which is both the
   * behaviour we want and the one Safari renders.
   *
   * `--tile-min` is already a viewport-aware clamp (see
   * browseView.svelte.ts). Here we only guard overflow: a grid track
   * can't shrink below its minmax() floor, so at the largest rungs the
   * floor can exceed a phone's column width — min(…, 100%) degrades that
   * to a single full-width column instead of scrolling sideways. */
  :global(.tile-grid) {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(var(--tile-min, 22rem), 100%), 1fr));
  }
</style>
