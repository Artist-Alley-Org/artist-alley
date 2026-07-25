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
   * An empty rail renders NOTHING — no heading, no empty-state box. On
   * an install whose operator has curated nothing, a "no featured
   * items" panel is noise on the front page; the caller decides what
   * to show instead.
   */
  import { onMount } from 'svelte';
  import { api } from '$api/client';
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
    asset_has_image?: boolean;
    preview_available?: boolean;
  }

  let items = $state<FeaturedItem[]>([]);
  let loaded = $state(false);

  onMount(() => {
    void load();
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
  // Keyed on the hash rather than on `asset_has_image` deliberately:
  // has_image has no writer anywhere in the codebase (DEFAULT false),
  // so trusting it would make the rail render title-only tiles for
  // content that has perfectly good bytes. A 404 on the variant falls
  // back to the same title-only tile, so the worse failure is guarded
  // either way.
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
</script>

{#if loaded && items.length > 0}
  <section class="mb-8" data-testid="featured-rail">
    <h2 class="mb-3 text-lg font-semibold text-fg">{t('collections.rail_title')}</h2>
    <!-- Horizontal scroll rather than a wrapping grid: a rail is an
         ordered, curated sequence, and wrapping it into rows loses the
         operator's ordering as the primary read. overflow-x is scoped
         to this container so the page body never scrolls sideways. -->
    <div class="-mx-1 flex gap-3 overflow-x-auto px-1 pb-2">
      {#each items as it (it.id)}
        {@const thumb = thumbUrl(it)}
        {@const to = href(it)}
        <svelte:element
          this={to ? 'a' : 'div'}
          href={to}
          class="group w-40 shrink-0 sm:w-48"
          data-testid="featured-rail-item"
        >
          <div
            class="relative aspect-square overflow-hidden rounded-lg border border-border bg-surface-elevated"
          >
            {#if showThumb(it)}
              <img
                src={thumb}
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
