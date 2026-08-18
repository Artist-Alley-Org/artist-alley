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
   * # The strip holds ONE size, and the grid below moves (#1098)
   *
   * #909 wired these tiles to the browse tile-size stepper so a page
   * did not show two card sizes. Seen live, that is the wrong half to
   * make move: the strip is a header over the working surface, not part
   * of it, and it jumping every time the reader tunes the grid's
   * density is what the stepper is being used to get AWAY from. So the
   * coupling is severed — deliberately, and only in this direction. The
   * strip renders at a fixed card width forever; the grid underneath
   * still follows the stepper exactly as it did.
   *
   * See `CARD_WIDTH` below.
   *
   * # Square tiles became WIDE CARDS (#1110)
   *
   * The owner's reshape. The strip is no longer a row of thumbnails
   * with a name printed on them — it is a slider of cards, each one a
   * 425px-wide landscape frame carrying the collection's name and a
   * second line about it. #1098's size LOCK carries over untouched;
   * only the shape it locks to changed.
   *
   * What that buys, and why it is a different component from the grid
   * beneath it: a curated strip is read, not scanned. Four wide cards
   * with two lines of copy each say what the operator picked and why;
   * twelve squares with a name on them say only that twelve things
   * exist. The grid below is where scanning happens, and it still has
   * its stepper.
   */
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { t } from '$stores/lang.svelte';
  import { clampZoom, coverPlacement } from '$lib/util/featuredCrop';
  import {
    createRailScroll,
    RAIL_ARROW_CLASS,
    RAIL_ARROW_LIVE_CLASS,
    RAIL_ARROW_DISABLED_CLASS,
  } from '$lib/util/railScroll.svelte';

  interface FeaturedItem {
    id: string;
    subject_kind: 'asset' | 'collection';
    subject_id: string;
    position: number;
    title: string;
    /** The card's second line (#1110): a collection's own description,
     *  '' when it has none and '' for every other subject kind.
     *
     *  Withheld by the SERVER exactly when the title is — it is read
     *  from the same predicate-gated join — so there is nothing to
     *  re-decide here. See the FeaturedItem schema. */
    subtitle?: string;
    /** The subtitle's fallback: members of a collection subject THIS
     *  caller can see. Null/absent for an asset subject, which is not
     *  the same as 0 — see `subtitleOf`. */
    item_count?: number | null;
    /** The asset to request the col variant from — the subject itself
     *  for an asset, the collection's hero-card fallback for a
     *  collection (#559). Null when nothing is servable. */
    cover_asset_id?: string | null;
    /** Where to centre this tile's crop, as fractions of the cover
     *  picture (#1207). Null means centre — the CSS default, and what
     *  every tile did before the curator could position one.
     *
     *  The SERVER decides when these are set: only when the tile is
     *  showing a cover the curator actually chose, never on the derived
     *  fallback. Nothing here re-decides that. */
    cover_focal_x?: number | null;
    cover_focal_y?: number | null;
    /** How far that crop is tightened, as a multiplier on the fitting
     *  rectangle (#1212). Null is the fit — what every tile did before
     *  the curator could zoom. Set on the same rungs, by the same
     *  server decision, as the focal pair above. */
    cover_zoom?: number | null;
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

  /** The card's second line (#1110). Description first, member count as
   *  the fallback, nothing at all when the server gave neither.
   *
   *  This is a PRESENTATION choice over two server-decided values, not
   *  a second visibility rule. Both arrive already withheld — the
   *  description because it rides the gated collection join, the count
   *  because each half of it is gated — so there is nothing here to
   *  re-check and deliberately nothing here that filters. See the
   *  FeaturedItem schema for both arguments.
   *
   *  `item_count == null` and `item_count === 0` are different answers
   *  and are treated as such: null is an asset subject, which has no
   *  membership and prints no second line; 0 is an empty collection,
   *  which has one and prints "0 items". Writing this as a falsy check
   *  would collapse the two and silently drop the line from every empty
   *  collection on the strip. */
  function subtitleOf(it: FeaturedItem): string | null {
    const desc = (it.subtitle ?? '').trim();
    if (desc) return desc;
    const n = it.item_count;
    if (n === null || n === undefined) return null;
    return n === 1
      ? t('collections.rail_item_count_one')
      : t('collections.rail_item_count', { count: String(n) });
  }

  /** The card's width, in px, fixed (#1110 + #1098's lock).
   *
   *  425 is the owner's measured figure from the reference design and
   *  is a literal rather than a ladder rung: the rung constants describe
   *  the GRID's density steps, and #1098's whole point is that this
   *  strip is not on that ladder. Reading `browseView.tileMin` here is
   *  what made the strip move with the stepper (#909); reading
   *  `DEFAULT_TILE_MIN` — the previous compromise — kept it pinned to a
   *  scale it no longer shares a shape with. So the number lives here,
   *  next to the aspect it is paired with.
   *
   *  If a future change wants the strip to breathe again, it belongs in
   *  the issue that reverses the product call, not in a quiet re-import
   *  of the stepper's getter.
   *
   *  The 890:500 image box is the other half of the same measurement —
   *  425 x 238.76 — and is applied as an `aspect-ratio` so the box stays
   *  proportional at the one width below where 425px does not fit. */
  const CARD_WIDTH = 425;
  const CARD_ASPECT = '890 / 500';
  const CARD_GAP = 12;

  /** 390px: ONE card per viewport, minus a peek.
   *
   *  The two candidates in #1110 were "scale down proportionally" and
   *  "one card per viewport". They are the same answer at this width
   *  once the card is 425px and a phone's content box is ~358px — the
   *  card cannot fit whole, so it scales, and only one lands on screen.
   *  What the choice actually decides is whether the NEXT card peeks.
   *
   *  It has to. A single card filling the viewport edge to edge is
   *  indistinguishable from a static hero image, and the strip's whole
   *  behaviour is that it scrolls. 85vw leaves ~15% of the next card
   *  showing, which is the affordance the old three-zone clamp bought
   *  with its floor (#909's note) — kept, in the shape this card needs.
   *
   *  At >=1080p the min resolves to 425px flat: 85vw is 918px there, so
   *  the measured acceptance figure is exact and not "about 425". */
  const cardWidthCSS = `min(${CARD_WIDTH}px, 85vw)`;

  /** Responsive source set for the card (#502/#610's pattern, reused).
   *
   *  A card that resizes with the VIEWPORT still needs a resizable
   *  source, so this survives #1098's size lock unchanged — and matters
   *  MORE after #1110 than before it. `col` is `fit: cover, max_dim:
   *  320`, and the card is now 425px wide, so `col` alone is an upscale
   *  at every viewport wide enough to show the card at full size.
   *
   *  Gated on `ladder_available` exactly as CardThumb is: that flag is
   *  the server confirming every CONFIGURED rung exists for THIS asset
   *  and this caller, and without it requesting anything but `col` is
   *  the 404 class #471 removed. The rung keys come from GET /previews,
   *  never hardcoded (#610's trap). No ladder → `srcset` is empty and
   *  the card renders from `col`, exactly as it did before.
   *
   *  The contain rungs are not 890:500, and that is fine here in a way
   *  it is NOT in the grid's `fill` mode: the box has a declared
   *  aspect-ratio and `object-cover`, so CSS takes a centre crop of
   *  whatever rung it is handed. See the note on the crop in the markup.
   *
   *  `sizes` ships with it and never without it — they are one hint, and
   *  it now restates `cardWidthCSS` in the `sizes` grammar rather than
   *  the grid's clamp. `sizes` is NOT CSS and discards the whole
   *  attribute when it meets a `min()`, so the two zones are spelled out
   *  (#639's trap). The leading `auto` depends on the `<img>` being
   *  `loading="lazy"`, which it is; making it eager would silently turn
   *  the hint into 100vw. */
  const CARD_SIZES = `auto, (max-width: ${Math.round(CARD_WIDTH / 0.85)}px) 85vw, ${CARD_WIDTH}px`;

  /** `sizes` for a tile that is ZOOMED (#1212).
   *
   *  Not decoration, and not the same hint. `sizes` states how wide the
   *  image is LAID OUT, and a zoomed tile lays its picture out at z
   *  times the card so that the card can show 1/z of it. Leaving the
   *  unzoomed hint in place would have the browser pick a rung sized
   *  for the card and then magnify it — the ladder would have `screen`
   *  and `hires` sitting there unasked-for, which is the whole reason
   *  the 4x cap is stated in terms of real rungs. Multiplying here is
   *  what makes that cap true rather than aspirational.
   *
   *  The `auto` keyword and the two zones are kept verbatim from
   *  CARD_SIZES, including #639's no-`min()` rule. */
  function sizesFor(it: FeaturedItem): string {
    const z = clampZoom(it.cover_zoom);
    if (z === 1) return CARD_SIZES;
    return `auto, (max-width: ${Math.round(CARD_WIDTH / 0.85)}px) ${Math.round(85 * z)}vw, ${Math.round(CARD_WIDTH * z)}px`;
  }

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

  // ── The slider's edge controls + drag pan (#1110) ─────────────────
  //
  // The strip is still an ordinary `overflow-x-auto` box: wheel,
  // trackpad and touch swipe keep working with no handler of ours in
  // the path. What its SCROLLBAR being hidden costs, and what pays that
  // back, is all in the shared helper — #1113 extracted it there when
  // the teams rail needed the identical behaviour, rather than growing
  // a second copy of a drag whose load-bearing detail (the dragstart
  // cancel) is invisible from the code.

  let scroller = $state<HTMLDivElement | null>(null);
  const rail = createRailScroll(() => scroller, {
    itemSelector: '[data-testid="featured-rail-item"]',
    gap: CARD_GAP,
    fallbackWidth: CARD_WIDTH,
  });

  // Re-measure whenever the strip's contents or box change. `items`
  // is read so the effect re-runs after the fetch lands, when the
  // scroller finally has something to overflow with.
  $effect(() => {
    void items.length;
    return rail.attach();
  });
</script>

{#if loaded && items.length > 0}
  <!-- `aria-label` carries the name the deleted `<h2>` used to give
       this region (#1030). Without it the element is a nameless
       `<section>`, which is not exposed as a landmark at all. -->
  <section class="mb-8" aria-label={t('collections.rail_title')} data-testid="featured-rail">
    <!-- `relative` is the arrows' anchor. `group/rail` scopes their
         hover reveal to the strip: they fade in when the pointer is
         anywhere over the row, and stay put for keyboard focus (see the
         button classes) — a control that only exists under the cursor
         is a control a keyboard reader cannot find. -->
    <div class="group/rail relative">
      <!-- Horizontal scroll rather than a wrapping grid: a rail is an
           ordered, curated sequence, and wrapping it into rows loses the
           operator's ordering as the primary read. overflow-x is scoped
           to this container so the page body never scrolls sideways.

           `gap-3` is 12px, #1110's measured figure, and CARD_GAP mirrors
           it for the arrow step — the two are one number and must not
           drift, which is why the step reads the rendered card's width
           rather than assuming the pair.

           `onscroll` only MEASURES. It does not drive the scroll, so
           wheel / trackpad / touch swipe are all untouched and this
           handler exists purely to keep the arrows' disabled state
           honest as the reader scrolls by any other means.

           `rail-scroller` hides the scrollbar (owner amendment) and
           `pb-2` goes with it — that padding existed to keep the bar
           clear of the cards, and with no bar it is just a gap.

           The pointer handlers are the drag pan. They are on the
           SCROLLER, not the section, so a press on the chevrons (which
           sit outside this box) is never mistaken for the start of a
           pan.

           `role="group"` because a div carrying pointer handlers is
           otherwise a control with no role in the accessibility tree.
           Deliberately UNLABELLED: the `<section>` one level up already
           names this row, and a second name here would have a screen
           reader announce "Featured" twice on the way in. The role says
           "these belong together", which is true and is all it needs to
           say — the pan is a pointer convenience with a keyboard
           equivalent (the chevrons), not a new interaction to
           announce. -->
      <div
        bind:this={scroller}
        {...rail.handlers}
        role="group"
        class="rail-scroller -mx-1 flex gap-3 overflow-x-auto px-1 {rail.dragging
          ? 'cursor-grabbing'
          : 'cursor-grab'}"
        data-testid="featured-rail-scroller"
      >
        {#each items as it (it.id)}
          {@const set = srcsetFor(it)}
          {@const thumb = srcFor(it, set)}
          {@const to = href(it)}
          {@const sub = subtitleOf(it)}
          <svelte:element
            this={to ? 'a' : 'div'}
            href={to}
            class="group shrink-0"
            style="width: {cardWidthCSS}"
            data-testid="featured-rail-item"
          >
            <!-- The 890:500 image box (#1110). `aspect-ratio` rather
                 than a height: the card scales at 390px and the frame
                 has to scale with it, and a fixed height would letterbox
                 exactly where there is least room to waste. -->
            <div
              class="relative overflow-hidden rounded-lg border border-border bg-surface-elevated"
              style="aspect-ratio: {CARD_ASPECT}"
            >
              {#if showThumb(it)}
                <!-- `object-cover` on a 16:9-ish frame crops whatever
                     the ladder served, and WHERE it crops is the
                     curator's call since #1207: `object-position` comes
                     from the focal fractions the cover editor's marquee
                     wrote, and falls back to the 50%/50% CSS default
                     when there are none. That is the answer to #1110's
                     open trade — a portrait cover still loses its top
                     and bottom, but the curator now decides which top
                     and which bottom, which is what a wide variant
                     would have bought at the cost of a whole rendition.
                     One helper renders the value for the rail, the
                     editor's live preview and the form's summary chip,
                     so "null means centre" is decided once.

                     SINCE #1212 THAT HELPER ALSO CARRIES THE ZOOM, and
                     that is why the image is absolutely positioned with
                     an explicit width rather than `h-full w-full`: a
                     zoomed tile lays the picture out larger than this
                     box and lets the box clip it. `object-fit: cover`
                     cannot express a window smaller than the fit, and
                     `transform: scale()` could not be used because the
                     hover polish on this very element already owns the
                     transform. At the fit the emitted values are
                     100%/100%/0/0, which is the box exactly — an
                     unzoomed tile paints what it always painted. -->
                <img
                  src={thumb}
                  srcset={set || undefined}
                  sizes={set ? sizesFor(it) : undefined}
                  alt=""
                  loading="lazy"
                  data-zoom={it.cover_zoom == null ? '' : String(it.cover_zoom)}
                  class="object-cover transition group-hover:scale-[1.02]"
                  style={coverPlacement(it.cover_focal_x, it.cover_focal_y, it.cover_zoom)}
                />
                <!-- The text block, ON the artwork (#1098, reshaped by
                     #1110). This is the card's ONLY title — the caption
                     that used to sit under the tile is gone, not hidden,
                     so the one-title invariant above still holds and a
                     screen reader still reads the name once. The
                     subtitle joins it inside the same block for the same
                     reason: one place, printed once.

                     The scrim is what makes this safe on ANY cover.
                     Light text needs a dark ground and a curated rail
                     has no say in what the artwork looks like, so the
                     bottom edge is darkened rather than the text being
                     tinted per image — the same solution PostCard's grid
                     overlay uses, and the reason both are `from-black/85`,
                     not a theme token: the ground here is the picture,
                     not the page, so it does not flip with the theme.
                     The scrim is taller than #1098's (`pt-10`) because it
                     now has two lines to keep legible, not one.

                     PERSISTENT, not hover-revealed like PostCard's. A
                     curated strip is read by name; hiding the name until
                     hover would make the rail unreadable at a glance and
                     unreadable full stop on a touch device.

                     `pointer-events-none` keeps the whole card one link —
                     the scrim covers the lower third of the anchor, and a
                     click there must open the collection, not land on a
                     decorative div. -->
                <div
                  class="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/85 via-black/55
                         to-transparent px-4 pb-3 pt-10"
                >
                  <p class="truncate text-base font-semibold text-white">{it.title}</p>
                  {#if sub}
                    <p class="truncate text-sm text-white/75" data-testid="featured-rail-subtitle">
                      {sub}
                    </p>
                  {/if}
                </div>
              {:else}
                <!-- Title-only card. The correct render for an asset
                     whose bytes are gated, for a collection with no
                     eligible cover (empty, or every member above the
                     public tier — #559), and for a variant that 404s.
                     This text is the card's accessible label — there is
                     no caption underneath it, precisely so the name
                     appears once.

                     NO SCRIM HERE, deliberately (#1110). A gradient over
                     an empty panel darkens nothing and reads as a
                     rendering failure; the ground is the page's own
                     surface, so the type simply sits on it in theme
                     colours. The subtitle comes along because it is the
                     same content, not because the arm is a copy of the
                     other one. -->
                <div class="flex h-full w-full flex-col items-center justify-center gap-1 p-4 text-center">
                  <span class="line-clamp-2 text-base font-semibold text-fg">{it.title}</span>
                  {#if sub}
                    <span
                      class="line-clamp-2 text-sm text-fg-muted"
                      data-testid="featured-rail-subtitle">{sub}</span
                    >
                  {/if}
                </div>
              {/if}
            </div>
            <!-- No caption under the card, in either arm (#1098). The
                 image card prints its name on the artwork; the
                 title-only card prints it inside the frame. Adding one
                 back here would print the name twice — visibly, not just
                 to a screen reader — which is the exact duplication the
                 one-title invariant above exists to prevent. -->
          </svelte:element>
        {/each}
      </div>

      <!-- Edge controls (#1110). Absolutely positioned OVER the
           scroller's ends rather than outside it, so they cost the strip
           no width on a narrow viewport.

           `aria-disabled` + `tabindex=-1` rather than `disabled` — see
           onPrev/onNext for why the button must stay in the DOM.
           `opacity` is the only thing hover changes; the buttons are
           always focusable (when live) so a keyboard reader reaches them
           without a pointer ever entering the strip, and
           `focus-visible:opacity-100` brings them back into view when
           they take focus that way. -->
      <button
        type="button"
        onclick={rail.prev}
        aria-disabled={rail.atStart}
        tabindex={rail.atStart ? -1 : 0}
        aria-label={t('collections.rail_prev')}
        title={t('collections.rail_prev')}
        data-testid="featured-rail-prev"
        class="{RAIL_ARROW_CLASS} left-1 {rail.atStart
          ? RAIL_ARROW_DISABLED_CLASS
          : RAIL_ARROW_LIVE_CLASS}"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m15 18-6-6 6-6" /></svg>
      </button>
      <button
        type="button"
        onclick={rail.next}
        aria-disabled={rail.atEnd}
        tabindex={rail.atEnd ? -1 : 0}
        aria-label={t('collections.rail_next')}
        title={t('collections.rail_next')}
        data-testid="featured-rail-next"
        class="{RAIL_ARROW_CLASS} right-1 {rail.atEnd
          ? RAIL_ARROW_DISABLED_CLASS
          : RAIL_ARROW_LIVE_CLASS}"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 18 6-6-6-6" /></svg>
      </button>
    </div>
  </section>
{/if}
