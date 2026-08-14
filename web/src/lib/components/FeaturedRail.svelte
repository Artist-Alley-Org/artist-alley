<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The public featured rail (#417, ADR 0065).
   *
   * With posts members-only pending the followers tier, this is the
   * only content an anonymous visitor sees at `/` — so it is the
   * landing page for a public install, not a decoration.
   *
   * It renders whatever GET /featured returns and nothing more. The
   * server decides what a caller may see, by composing the visibility
   * predicate into the rail query; there is deliberately no filtering
   * here. A client-side "hide the ones that look private" pass would
   * be a second expression of a rule that already has one home, which
   * is the defect class ADR 0063 exists to prevent.
   *
   * An empty rail renders NOTHING — no empty-state box. On an install
   * whose operator has curated nothing, a "no featured items" panel is
   * noise on the front page; the caller decides what to show instead.
   *
   * # No visible heading (#1030)
   *
   * The "Featured" `<h2>` is gone: a row of curated cards is
   * self-evidently curated, and the cards carry their own kicker
   * labels, which is where labelling belongs — on the item, not over
   * the row. The string survives as the section's `aria-label`, so the
   * region still has a name in the accessibility tree; a `<section>`
   * with no accessible name is not a landmark at all, so deleting the
   * heading outright would have removed a navigation target rather
   * than just some pixels.
   *
   * # The tiles follow the browse tile-size control (#909)
   *
   * See `tileWidth` below.
   */
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { browseView } from '$stores/browseView.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { t } from '$stores/lang.svelte';

  interface FeaturedItem {
    id: string;
    subject_kind: 'asset' | 'collection';
    subject_id: string;
    position: number;
    title: string;
    /** The asset to request the col variant from — the subject itself
     *  for an asset, the collection's hero-card fallback for a
     *  collection (#559). Null when nothing is servable. */
    cover_asset_id?: string | null;
    asset_file_hash?: string | null;
    preview_available?: boolean;
    /** Every rung of the operator's CONFIGURED ladder exists for this
     *  tile's cover asset AND the caller passes the content plane
     *  (#610). Required on the wire since #591; the rail simply never
     *  read it while its tiles were a fixed 160px. */
    ladder_available?: boolean;
  }

  let items = $state<FeaturedItem[]>([]);
  let loaded = $state(false);

  onMount(() => {
    void load();
    // Shared, once per page load — the grid's cards call this too and
    // every caller awaits the same flight.
    previewLadder.init();
  });

  async function load() {
    try {
      const { data } = await api.GET('/featured', { params: { query: { limit: 24 } } });
      if (data) items = ((data as { items?: FeaturedItem[] }).items ?? []) as FeaturedItem[];
    } finally {
      // No error state on purpose. The rail is supplementary chrome on
      // a page that has its own content; a failed fetch should leave
      // the page looking un-curated, never broken.
      loaded = true;
    }
  }

  // Assets whose sensitivity gates the bytes arrive with NO HASH at
  // all — the server strips it (ADR 0020). So the presence of the hash
  // is the signal, and this does not re-derive the rule.
  //
  // Keyed on the hash rather than on a stored "has an image" flag
  // deliberately. The `asset_has_image` field this used to avoid was
  // the projection of a column with no writer anywhere (DEFAULT false),
  // so trusting it would have made the rail render title-only tiles for
  // content with perfectly good bytes; both the field and the column
  // are gone as of #579. A 404 on the variant falls back to the same
  // title-only tile, so the worse failure is guarded either way.
  //
  // Collections resolve a cover the same way now (#559). The subject
  // kind is no longer the gate — `cover_asset_id` is, because for a
  // collection the tile renders ADR 0027's hero-card fallback (the
  // most-recent post's asset) and `subject_id` is the collection, not
  // something the variant endpoint accepts. A collection with no
  // eligible public cover arrives with both fields null and still
  // renders title-only, firing no request.
  function thumbUrl(it: FeaturedItem): string | null {
    if (!it.cover_asset_id || !it.asset_file_hash) return null;
    return `/api/v1/assets/${it.cover_asset_id}/variants/col`;
  }

  // Fall back to the title-only tile if the variant is missing, rather
  // than leaving a broken-image glyph on the front page.
  //
  // Tracked as state rather than by stacking a hidden layer behind the
  // image: the stacked version kept the title in the DOM twice for
  // every item, which duplicated it for screen readers and made
  // innerText-based assertions read it twice. Rendering one or the
  // other means the tile has exactly one title, always.
  //
  // The server tells us whether a servable col exists for this caller
  // (preview_available, #471), so we render the image only when true and
  // otherwise the title-only tile — with no probe and no byte request
  // that could 404.
  function showThumb(it: FeaturedItem): boolean {
    return !!thumbUrl(it) && !!it.preview_available;
  }

  // Only collections have a destination. Assets have no standalone
  // route — they render inside the viewer/modal on other surfaces —
  // and #416 established that inventing one is out of scope. A tile
  // that navigates somewhere unrelated is worse than a tile that does
  // not navigate, so featured assets render as plain tiles rather than
  // as links to the collections index.
  function href(it: FeaturedItem): string | null {
    return it.subject_kind === 'collection' ? `/collections/${it.subject_id}` : null;
  }

  /** The rail tile's width — the SAME CSS length the grid below uses as
   *  its column minimum (#909).
   *
   *  Before this, the tiles were `w-40 sm:w-48` and the browse tile-size
   *  control moved the post grid underneath while the curated strip
   *  above sat still: one page, two card sizes.
   *
   *  # What "larger tiles" means for a horizontal strip
   *
   *  A rail is not a grid, so `auto-fill` has nothing to say here — the
   *  tile width is chosen, not derived from a column count. The closest
   *  a fixed-width strip can get to the grid it sits over is
   *  `browseView.tileMin` itself: the grid's painted column is
   *  `tileMin + remainder/n`, so the two agree to within one row's
   *  leftover, and they step together on every rung.
   *
   *  The three-zone clamp transfers UNCHANGED, and it is load-bearing
   *  rather than inherited. A bare rung would be 22rem at the default —
   *  352px, which at 390px is one tile filling the viewport with no hint
   *  that the row scrolls sideways. The clamp's floor (0.4·R) puts about
   *  two and a half tiles in view on a phone, which is what says
   *  "scroller". So the same formula that gives the grid its column
   *  count gives the rail its peek.
   *
   *  Reading the store's GETTER, not the raw rung, is deliberate: it
   *  already carries thumbnail mode's +1 rung offset, so the rail
   *  matches whatever the grid is painting right now rather than what
   *  the stepper last stored. In `list` and `feed` the stepper is inert
   *  by definition, so nothing moves in either place.
   *
   *  `min(…, 100%)` for the same overflow reason TileGrid has it — the
   *  top rung's floor is 22.8rem, which is wider than a 390px viewport's
   *  content box.
   *
   *  `tileSizes` comes along as the matched other half — see `srcset`
   *  below for why the tile now HAS a source set to steer. */
  const tileWidth = $derived(`min(${browseView.tileMin}, 100%)`);

  /** Responsive source set for the tile (#502/#610's pattern, reused).
   *
   *  A resizable tile needs a resizable source. `col` is `fit: cover,
   *  max_dim: 320` — fine when this rail was a fixed `w-40 sm:w-48`,
   *  and a 2.6x upscale the moment the size control can take the tile
   *  to 38rem. Making the tiles follow the grid without this would have
   *  fixed "two card sizes" by shipping "two card qualities".
   *
   *  Gated on `ladder_available` exactly as CardThumb is: that flag is
   *  the server confirming every CONFIGURED rung exists for THIS asset
   *  and this caller, and without it requesting anything but `col` is
   *  the 404 class #471 removed. The rung keys come from GET /previews,
   *  never hardcoded (#610's trap). No ladder → `srcset` is empty and
   *  the tile renders from `col`, exactly as it did before.
   *
   *  The contain rungs are not square, and that is fine here in a way
   *  it is NOT in the grid's `fill` mode: this tile is an
   *  `aspect-square` box with `object-cover`, so CSS takes the same
   *  centre crop `col` was baked with. The rail stays a wall of
   *  squares; only the pixel count changes.
   *
   *  `sizes` ships with it and never without it — they are one hint.
   *  `browseView.tileSizes` is the same three-zone clamp `tileWidth`
   *  resolves, restated in the `sizes` grammar, so the width the
   *  browser budgets for is the width the tile actually gets. Its
   *  leading `auto` depends on the `<img>` being `loading="lazy"`,
   *  which it is; making it eager would silently turn the hint into
   *  100vw. */
  function srcsetFor(it: FeaturedItem): string {
    if (!it.ladder_available || !it.cover_asset_id) return '';
    return previewLadder.srcsetFor(it.cover_asset_id) ?? '';
  }

  /** The `src` a browser that ignores `srcset` loads, and the candidate
   *  the loader starts from. The smallest CONTAIN rung when the set is
   *  live — mixing `col`'s server crop in as the fallback would fetch a
   *  second, differently-produced image for the same slot. */
  function srcFor(it: FeaturedItem, set: string): string | null {
    if (!set) return thumbUrl(it);
    const smallest = previewLadder.smallestKey();
    return smallest ? `/api/v1/assets/${it.cover_asset_id}/variants/${smallest}` : thumbUrl(it);
  }
</script>

{#if loaded && items.length > 0}
  <!-- `aria-label` carries the name the deleted `<h2>` used to give
       this region (#1030). Without it the element is a nameless
       `<section>`, which is not exposed as a landmark at all. -->
  <section class="mb-8" aria-label={t('collections.rail_title')} data-testid="featured-rail">
    <!-- Horizontal scroll rather than a wrapping grid: a rail is an
         ordered, curated sequence, and wrapping it into rows loses the
         operator's ordering as the primary read. overflow-x is scoped
         to this container so the page body never scrolls sideways. -->
    <div class="-mx-1 flex gap-3 overflow-x-auto px-1 pb-2">
      {#each items as it (it.id)}
        {@const set = srcsetFor(it)}
        {@const thumb = srcFor(it, set)}
        {@const to = href(it)}
        <svelte:element
          this={to ? 'a' : 'div'}
          href={to}
          class="group shrink-0"
          style="width: {tileWidth}"
          data-testid="featured-rail-item"
        >
          <div
            class="relative aspect-square overflow-hidden rounded-lg border border-border bg-surface-elevated"
          >
            {#if showThumb(it)}
              <img
                src={thumb}
                srcset={set || undefined}
                sizes={set ? browseView.tileSizes : undefined}
                alt=""
                loading="lazy"
                class="h-full w-full object-cover transition group-hover:scale-[1.02]"
              />
            {:else}
              <!-- Title-only tile. The correct render for an asset
                   whose bytes are gated, for a collection with no
                   eligible cover (empty, or every member above the
                   public tier — #559), and for a variant that 404s.
                   This text is the tile's accessible label — there is
                   no caption underneath it, precisely so the name
                   appears once. -->
              <div class="flex h-full w-full items-center justify-center p-3 text-center">
                <span class="line-clamp-3 text-sm font-medium text-fg-muted">{it.title}</span>
              </div>
            {/if}
          </div>
          <!-- Caption only under an IMAGE tile. A title-only tile
               already displays the name inside the square, so a caption
               would print it twice — visibly, not just to a screen
               reader. -->
          {#if showThumb(it)}
            <p class="mt-1.5 line-clamp-2 text-sm text-fg">{it.title}</p>
          {/if}
        </svelte:element>
      {/each}
    </div>
  </section>
{/if}
