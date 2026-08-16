<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The operator promo band (#1118) — a full-width strip the browse feed
   * renders BETWEEN pages: headline, blurb and an optional call-to-action
   * on the left, an ordered row of curated cards on the right.
   *
   * # It renders NOTHING or it renders whole
   *
   * There is no empty state, no skeleton and no "no items" panel, and
   * there is deliberately no length check here either: the SERVER
   * answers `GET /featured/promo` with no band object at all when the
   * install has none, when the band is disabled, when it has no
   * headline, or when every card filtered away for this reader. The
   * collapse decision lives beside the filter that produced the
   * emptiness (see featured/band.go); re-deciding it here would be a
   * second copy of it, in the one place that cannot see why the list is
   * short.
   *
   * That is ADR 0030's rule — collapse a banner between content — and
   * NOT ADR 0079 §2's "an unfilled slot becomes ordinary content". §2 is
   * scoped to in-grid sized slots, where collapsing a 2×2 cell leaves a
   * hole in the middle of a wall. A full-width strip between two walls
   * has no hole to leave: the walls simply meet.
   *
   * # No filtering, no re-deciding
   *
   * Same posture as FeaturedRail: what a caller may see is the query's
   * answer. Every card arrives already resolved through the caller's
   * ADR 0063 predicate, with ADR 0020 sensitivity suppressing the
   * thumbnail hints for a gated asset and ADR 0090's mature conjunct
   * having dropped what the reader has not opted into. A client-side
   * "hide the ones that look private" pass would be a second expression
   * of a rule that has one home.
   *
   * # The author chip prints what the server resolved
   *
   * `author` is absent for an owner who took ADR 0024's opt-out and for
   * a hard-deleted account. Absent means NO CHIP — not a placeholder,
   * not "Unknown". ⛔ Do not derive a name from a username here: the
   * display-name ladder lives in users.ResolveDisplayName and #1023
   * exists because it had been transcribed four times.
   */
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { t } from '$stores/lang.svelte';
  import Avatar from '$components/Avatar.svelte';
  import type { components } from '$api/schema';

  type Band = components['schemas']['PromoBand'];
  type BandItem = components['schemas']['FeaturedItem'];

  interface Props {
    band: Band;
  }
  let { band }: Props = $props();

  /** The card's cover. Identical contract to FeaturedRail's `thumbUrl`,
   *  and identical for the same reason: an asset whose bytes are gated
   *  arrives with NO hash — the server strips it (ADR 0020) — so the
   *  presence of the hash is the signal and nothing here re-derives the
   *  rule. `cover_asset_id` and not `subject_id`, because for a
   *  collection the subject is the collection and the variant endpoint
   *  would 404 on it (#559). */
  function thumbUrl(it: BandItem): string | null {
    if (!it.cover_asset_id || !it.asset_file_hash) return null;
    return `/api/v1/assets/${it.cover_asset_id}/variants/col`;
  }

  /** Render the image only when the server says a servable `col` exists
   *  for THIS caller (#471) — so a gated card fires no byte request that
   *  could 404 and falls back to the title-only frame. */
  function showThumb(it: BandItem): boolean {
    return !!thumbUrl(it) && !!it.preview_available;
  }

  /** Only collections have a destination. An asset has no standalone
   *  route — #416 established that inventing one is out of scope — and a
   *  card that navigates somewhere unrelated is worse than one that does
   *  not navigate. Same rule as the featured rail's. */
  function href(it: BandItem): string | null {
    return it.subject_kind === 'collection' ? `/collections/${it.subject_id}` : null;
  }

  function srcsetFor(it: BandItem): string {
    if (!it.ladder_available || !it.cover_asset_id) return '';
    return previewLadder.srcsetFor(it.cover_asset_id) ?? '';
  }

  /** The candidate a browser that ignores `srcset` loads. The smallest
   *  CONTAIN rung when the set is live — mixing `col`'s server crop in
   *  as the fallback would fetch a second, differently-produced image
   *  for the same slot. Ported from FeaturedRail for the same reason. */
  function srcFor(it: BandItem, set: string): string | null {
    if (!set) return thumbUrl(it);
    const smallest = previewLadder.smallestKey();
    return smallest ? `/api/v1/assets/${it.cover_asset_id}/variants/${smallest}` : thumbUrl(it);
  }

  /** The card width, fixed, for the same reason FeaturedRail's is
   *  (#1098): this strip is chrome over the working surface, not part of
   *  it, so it must not move when the reader tunes the grid's density.
   *
   *  228px is narrower than the rail's 425 on purpose — the rail's card
   *  is a wide landscape frame carrying two lines of copy, and these are
   *  a ROW of five beside a block of text. At 1080p the text block takes
   *  its 24rem and five 228px cards with 12px gutters fit the remainder
   *  with room to spare; below that the row scrolls rather than
   *  wrapping, so the operator's ordering stays the primary read. */
  const CARD_WIDTH = 228;
  const CARD_ASPECT = '4 / 3';
  const CARD_SIZES = `auto, (max-width: 640px) 60vw, ${CARD_WIDTH}px`;

  /** `sizes` is NOT CSS and drops the whole attribute when it meets a
   *  `min()` (#639's trap), which is why the two zones above are spelled
   *  out. The leading `auto` depends on the `<img>` being
   *  `loading="lazy"`, which it is. */
</script>

<!-- `aria-labelledby` on the headline rather than a bare `aria-label`:
     the headline is real text on the page, and pointing at it keeps the
     accessible name and the visible name from drifting apart. A
     `<section>` with no accessible name is not exposed as a landmark at
     all, so this is what makes the band a navigation target. -->
<section
  class="promo-band my-6 overflow-hidden rounded-xl border border-border bg-surface-elevated"
  aria-labelledby="promo-band-title"
  data-testid="promo-band"
>
  <div class="flex flex-col gap-5 p-5 lg:flex-row lg:items-center lg:gap-6">
    <!-- The copy block. `lg:w-96` rather than a flex-basis percentage:
         the cards are a fixed width, so a percentage would shrink the
         text at exactly the widths where an extra card would have fit
         instead. -->
    <div class="shrink-0 lg:w-96">
      <p class="text-xs font-semibold uppercase tracking-wide text-accent">
        {t('promo_band.kicker')}
      </p>
      <h2 id="promo-band-title" class="mt-1 text-2xl font-semibold text-fg">
        {band.title}
      </h2>
      {#if band.blurb}
        <p class="mt-2 text-sm text-fg-muted" data-testid="promo-band-blurb">{band.blurb}</p>
      {/if}
      {#if band.cta_label && band.cta_url}
        <!-- The href is operator-supplied. It is constrained to an
             absolute http(s) URL or a site-relative path by the handler
             AND by a CHECK constraint (migration 00053), because a
             `javascript:` value here would be stored XSS on the browse
             page of every reader — Svelte does not sanitise hrefs.

             `rel="noopener noreferrer"` on the external arm: an operator
             linking out to a shop or a festival page should not hand
             that page a live `window.opener` back into this session. -->
        {@const external = /^https?:/i.test(band.cta_url)}
        <a
          href={band.cta_url}
          rel={external ? 'noopener noreferrer' : undefined}
          class="mt-4 inline-flex items-center rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent hover:opacity-90"
          data-testid="promo-band-cta"
        >
          {band.cta_label}
        </a>
      {/if}
    </div>

    <!-- The cards. A horizontal scroller rather than a wrapping grid,
         for the reason the featured rail's is one: a curated row is an
         ordered sequence and wrapping it into rows loses the operator's
         ordering as the primary read. `overflow-x` is scoped here so the
         page body never scrolls sideways. -->
    <div
      class="promo-band-cards -mx-1 flex min-w-0 flex-1 gap-3 overflow-x-auto px-1 pb-1"
      role="list"
      data-testid="promo-band-cards"
    >
      {#each band.items as it (it.id)}
        {@const set = srcsetFor(it)}
        {@const thumb = srcFor(it, set)}
        {@const to = href(it)}
        <div role="listitem" class="shrink-0" style="width: {CARD_WIDTH}px">
          <svelte:element
            this={to ? 'a' : 'div'}
            href={to}
            class="group block"
            data-testid="promo-band-item"
          >
            <div
              class="relative overflow-hidden rounded-lg border border-border bg-surface"
              style="aspect-ratio: {CARD_ASPECT}"
            >
              {#if showThumb(it)}
                <img
                  src={thumb}
                  srcset={set || undefined}
                  sizes={set ? CARD_SIZES : undefined}
                  alt=""
                  loading="lazy"
                  class="h-full w-full object-cover transition group-hover:scale-[1.02]"
                />
              {:else}
                <!-- Title-only frame: the correct render for an asset
                     whose bytes are gated, for a collection with no
                     eligible cover, and for a variant that 404s. The
                     title is printed BELOW in both arms, so this frame
                     stays empty rather than carrying a second copy of
                     the name — one title per card, always (the
                     invariant FeaturedRail states for its own tiles). -->
                <div class="flex h-full w-full items-center justify-center p-3">
                  <span class="text-xs uppercase tracking-wide text-fg-muted">
                    {t(`promo_band.kind_${it.subject_kind}`)}
                  </span>
                </div>
              {/if}
            </div>
            <p class="mt-2 truncate text-sm font-medium text-fg" data-testid="promo-band-item-title">
              {it.title}
            </p>
          </svelte:element>
          {#if it.author}
            <!-- The chip is OUTSIDE the card's anchor so the artist's
                 name links to the artist and not to the collection. Two
                 destinations inside one anchor is the pattern that makes
                 a card impossible to use with a keyboard. -->
            <a
              href="/users/by-username/{it.author.username}"
              class="mt-1 flex items-center gap-1.5 text-xs text-fg-muted hover:text-fg"
              data-testid="promo-band-item-author"
            >
              <Avatar
                src={it.author.avatar_url ?? null}
                name={it.author.display_name}
                sizeClass="h-4 w-4"
              />
              <span class="truncate">{it.author.display_name}</span>
            </a>
          {/if}
        </div>
      {/each}
    </div>
  </div>
</section>

<style>
  /* The band reads as a BAND — a distinct ground the feed's tiles do not
     have — which is what stops it being mistaken for feed content. The
     tint is a token blend rather than a fixed colour so it follows the
     theme; the ground here is the page, not a picture (contrast the
     featured rail's scrim, which sits ON artwork and is therefore
     deliberately theme-independent). */
  .promo-band {
    background-image: linear-gradient(
      to right,
      color-mix(in oklch, var(--color-accent) 10%, transparent),
      transparent 60%
    );
  }

  /* Hide the card row's scrollbar, matching the featured rail's strip.
     The row still scrolls by wheel, trackpad and touch. */
  .promo-band-cards {
    scrollbar-width: none;
  }
  .promo-band-cards::-webkit-scrollbar {
    display: none;
  }
</style>
