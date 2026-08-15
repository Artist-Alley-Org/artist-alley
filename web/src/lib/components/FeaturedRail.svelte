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

  // ── The slider's edge controls (#1110) ─────────────────────────────
  //
  // The strip is still an ordinary `overflow-x-auto` box: wheel,
  // trackpad and touch swipe keep working with no handler of ours in
  // the path. What changed with the owner's amendment is that its
  // SCROLLBAR is hidden (see the `.rail-scroller` style) and a
  // click-and-drag pan replaces it, so the strip reads as a slider
  // rather than as a scrolling div with a bar under it.
  //
  // Hiding a scrollbar removes an affordance, so three others have to
  // carry its weight, and all three are here rather than assumed:
  // the edge chevrons (for a mouse with no horizontal wheel), the
  // grab-cursor drag (for a mouse that would have used the bar), and
  // the keyboard path through the chevrons (unchanged — the drag adds
  // no keyboard requirement because it adds no keyboard-only action).

  let scroller = $state<HTMLDivElement | null>(null);
  let atStart = $state(true);
  let atEnd = $state(false);
  /** True from the moment a drag passes the movement threshold until
   *  the click that ends it has been swallowed. Drives the cursor and
   *  the click guard. */
  let dragging = $state(false);

  /** Recompute which ends the scroller is parked at.
   *
   *  The 1px tolerance is not superstition: `scrollLeft` is fractional
   *  on a zoomed or fractionally-scaled display, so `scrollLeft + w ===
   *  scrollWidth` is false at the true end often enough to leave "next"
   *  live with nothing left to scroll to. */
  function measure() {
    const el = scroller;
    if (!el) return;
    atStart = el.scrollLeft <= 1;
    atEnd = el.scrollLeft + el.clientWidth >= el.scrollWidth - 1;
  }

  /** Scroll by one card + its gap.
   *
   *  A card at a time rather than a viewport at a time: a viewport-sized
   *  jump on a wide display moves two and a half cards and lands
   *  mid-card, and the reader loses the one they were reading. The step
   *  is computed from the rendered card, not from CARD_WIDTH, so it
   *  stays right at the 390px width where the card is narrower than its
   *  nominal size.
   *
   *  `behavior: smooth` is deliberate on a control that can be held
   *  down; the browser coalesces repeats rather than queueing them. */
  function step(direction: -1 | 1) {
    const el = scroller;
    if (!el) return;
    const card = el.querySelector<HTMLElement>('[data-testid="featured-rail-item"]');
    const by = (card?.getBoundingClientRect().width ?? CARD_WIDTH) + CARD_GAP;
    el.scrollBy({ left: direction * by, behavior: 'smooth' });
  }

  /** The arrows are DISABLED, not removed, and disabled via ARIA rather
   *  than the `disabled` attribute.
   *
   *  `disabled` takes a button out of the tab order entirely, so a
   *  keyboard reader tabbing along the strip would find the control
   *  appear and disappear under them as they scrolled — the control
   *  moves, which is worse than a control that is present and inert.
   *  `aria-disabled` + `tabindex=-1` is #1110's spelling and is the
   *  pairing that keeps the button in the DOM, announced, and
   *  unreachable while it has nothing to do.
   *
   *  The guard is in the handler too. aria-disabled is advisory — it
   *  stops nothing on its own — so a click that arrives anyway (a
   *  screen-reader activation, a stale pointer) must be a no-op here
   *  rather than a scroll to a place that does not exist. */
  function onPrev() {
    if (atStart) return;
    step(-1);
  }
  function onNext() {
    if (atEnd) return;
    step(1);
  }

  /** The arrows' classes, as plain strings rather than Tailwind's
   *  `aria-disabled:` variant.
   *
   *  The variant works, but the disabled arm needs three declarations
   *  including one that is itself breakpoint-scoped
   *  (`aria-disabled:md:opacity-0`), and a stacked variant that silently
   *  fails to compile leaves a live-looking control that does nothing —
   *  a failure invisible to `npm run check` and to any test that does
   *  not read computed styles. Two named strings switched in the markup
   *  cannot half-apply. */
  const ARROW_CLASS =
    'absolute top-1/2 z-[3] grid h-10 w-10 -translate-y-1/2 place-items-center rounded-full ' +
    'border border-border bg-surface/90 text-fg shadow-md backdrop-blur-sm transition ' +
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:opacity-100';
  /** Live: revealed on pointer hover over the strip on a pointer-ish
   *  viewport, always visible below `md` where there is no hover to
   *  speak of and the arrows are the only non-drag way across. */
  const LIVE_CLASS = 'hover:bg-surface md:opacity-0 md:group-hover/rail:opacity-100';
  /** Disabled: same reveal rule as live, dimmed, and click-through-proof.
   *
   *  Revealed on hover rather than hidden outright so the pair reads as
   *  a pair — hovering the strip shows both ends, one of them plainly
   *  spent. An arrow that VANISHES at the end of its travel makes the
   *  other one jump position in the reader's peripheral vision, and
   *  gives no clue that scrolling back is the thing that brings it back.
   *
   *  `pointer-events-none` is the belt to `aria-disabled`'s braces — the
   *  handler already refuses, and this stops the cursor changing over a
   *  control that will not respond. */
  const DISABLED_CLASS =
    'pointer-events-none opacity-30 md:opacity-0 md:group-hover/rail:opacity-30';

  // ── Click-and-drag panning (owner amendment to #1110) ───────────────
  //
  // Pointer events, not mouse events, and MOUSE ONLY. Touch already
  // pans natively through `overflow-x-auto`; running our own drag over
  // the top of that fights the browser's momentum scrolling and loses,
  // so `pointerType === 'mouse'` is the gate and a phone never enters
  // this code at all. A pen behaves like touch here for the same reason.
  //
  // THE THRESHOLD IS THE WHOLE DESIGN. Every card is a link, so a drag
  // implementation that treats pointerdown-then-pointerup as a drag
  // breaks clicking, and one that never suppresses the click makes
  // every pan open a collection. So movement is measured first and
  // nothing is "a drag" until it exceeds DRAG_THRESHOLD; below that the
  // gesture was a click and is left entirely alone.
  //
  // Capture is taken LAZILY, at the threshold rather than at
  // pointerdown. Taking it on down would retarget the pointer to the
  // scroller immediately, and the `click` a plain press produces would
  // then never reach the anchor — the card would stop opening at all,
  // which is the failure this ordering exists to avoid.

  /** Pixels of horizontal movement before a press becomes a pan. 5px is
   *  above the jitter a hand resting on a mouse produces and well below
   *  what anyone would call a deliberate drag. */
  const DRAG_THRESHOLD = 5;

  let dragStartX = 0;
  let dragStartScroll = 0;
  let dragPointerId: number | null = null;

  /** Kill the browser's OWN drag-and-drop inside the strip.
   *
   *  Without this the pan does not work at all, and the reason is worth
   *  recording because it is invisible from the code: every card is an
   *  `<a>` wrapping an `<img>`, and both are natively draggable. The
   *  first pointermove of a press is below DRAG_THRESHOLD, so it
   *  returns early — correctly — WITHOUT calling preventDefault, and in
   *  that same instant Chromium starts a native image/link drag.
   *  `dragstart` cancels the pointer sequence, so no further pointermove
   *  is ever delivered and the strip moves by exactly one frame's worth
   *  of travel and then stops. Measured: a 260px drag panned 20px.
   *
   *  Cancelling dragstart is the fix rather than moving preventDefault
   *  up into pointerdown: preventing the default on pointerdown also
   *  suppresses focus and the click that follows it, which would break
   *  every card's link to buy the same thing. */
  function onDragStart(e: DragEvent) {
    e.preventDefault();
  }

  function onPointerDown(e: PointerEvent) {
    if (e.pointerType !== 'mouse' || e.button !== 0) return;
    const el = scroller;
    if (!el || el.scrollWidth <= el.clientWidth) return;
    dragPointerId = e.pointerId;
    dragStartX = e.clientX;
    dragStartScroll = el.scrollLeft;
    // NOT dragging yet, and no capture taken — see the threshold note.
    dragging = false;
  }

  function onPointerMove(e: PointerEvent) {
    if (dragPointerId === null || e.pointerId !== dragPointerId) return;
    const el = scroller;
    if (!el) return;
    const dx = e.clientX - dragStartX;
    if (!dragging) {
      if (Math.abs(dx) < DRAG_THRESHOLD) return;
      dragging = true;
      // Now that this IS a pan, keep receiving moves even when the
      // pointer leaves the strip — a pan that stops at the element's
      // edge is a pan that stops halfway.
      el.setPointerCapture(e.pointerId);
    }
    // Native `behavior: smooth` from the chevrons would fight a direct
    // assignment; setting scrollLeft cancels any in-flight smooth
    // scroll, which is the behaviour a reader grabbing the strip
    // mid-animation expects.
    el.scrollLeft = dragStartScroll - dx;
    // The browser's own text/image drag would otherwise start on the
    // card artwork and paint a ghost image under the cursor.
    e.preventDefault();
  }

  function endDrag(e: PointerEvent) {
    if (dragPointerId === null || e.pointerId !== dragPointerId) return;
    const el = scroller;
    if (el?.hasPointerCapture(e.pointerId)) el.releasePointerCapture(e.pointerId);
    dragPointerId = null;
    // `dragging` is NOT cleared here. The click that follows this
    // pointerup has not fired yet, and onClickCapture below needs to
    // know a pan just happened so it can swallow it. It clears itself
    // there, or on the next pointerdown if no click arrives.
  }

  /** Swallow the click a pan produces, and only that one.
   *
   *  Registered in the CAPTURE phase so it runs before the anchor's own
   *  handler — by the bubble phase the card has already navigated.
   *  A press that never crossed the threshold left `dragging` false and
   *  passes through untouched, which is what keeps the cards clickable. */
  function onClickCapture(e: MouseEvent) {
    if (!dragging) return;
    dragging = false;
    e.preventDefault();
    e.stopPropagation();
  }

  // Re-measure whenever the strip's contents or box change. `items`
  // is read so the effect re-runs after the fetch lands, when the
  // scroller finally has something to overflow with.
  $effect(() => {
    void items.length;
    const el = scroller;
    if (!el) return;
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
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
        onscroll={measure}
        onpointerdown={onPointerDown}
        onpointermove={onPointerMove}
        onpointerup={endDrag}
        onpointercancel={endDrag}
        ondragstart={onDragStart}
        onclickcapture={onClickCapture}
        role="group"
        class="rail-scroller -mx-1 flex gap-3 overflow-x-auto px-1 {dragging
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
                <!-- `object-cover` on a 16:9-ish frame takes a CENTRE
                     crop of whatever the ladder served. `col` is baked
                     `fit: cover` at 320px square, so a portrait cover
                     loses its top and bottom here — the trade #1110
                     accepted rather than minting a wide variant for one
                     surface. Watch this if the strip ever features
                     portrait-first work; the fix is a variant, not a
                     `contain` that would letterbox every card. -->
                <img
                  src={thumb}
                  srcset={set || undefined}
                  sizes={set ? CARD_SIZES : undefined}
                  alt=""
                  loading="lazy"
                  class="h-full w-full object-cover transition group-hover:scale-[1.02]"
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
        onclick={onPrev}
        aria-disabled={atStart}
        tabindex={atStart ? -1 : 0}
        aria-label={t('collections.rail_prev')}
        title={t('collections.rail_prev')}
        data-testid="featured-rail-prev"
        class="{ARROW_CLASS} left-1 {atStart ? DISABLED_CLASS : LIVE_CLASS}"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m15 18-6-6 6-6" /></svg>
      </button>
      <button
        type="button"
        onclick={onNext}
        aria-disabled={atEnd}
        tabindex={atEnd ? -1 : 0}
        aria-label={t('collections.rail_next')}
        title={t('collections.rail_next')}
        data-testid="featured-rail-next"
        class="{ARROW_CLASS} right-1 {atEnd ? DISABLED_CLASS : LIVE_CLASS}"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 18 6-6-6-6" /></svg>
      </button>
    </div>
  </section>
{/if}

<style>
  /* NO SCROLLBAR (owner amendment to #1110).

     The strip is a slider; a scrollbar under it is the div showing
     through. Both spellings are required and neither is redundant:
     `scrollbar-width` is the standard property Firefox implements, and
     the `::-webkit-scrollbar` pseudo-element is what Chromium and
     Safari read. Shipping one alone leaves a visible bar in the other
     half of the browser market.

     Hiding a scrollbar REMOVES AN AFFORDANCE, and this is the note that
     says what replaces it rather than leaving the next reader to
     wonder: the edge chevrons, the click-and-drag pan (see
     onPointerDown), the wheel — untouched — and touch swipe, which is
     the browser's own and was never using the bar anyway.

     This is deliberately NOT a global utility. The teams rail keeps its
     bar until #1113 gives it the same treatment; a shared class would
     have changed it today, silently, in a sprint that does not own it. */
  .rail-scroller {
    scrollbar-width: none;
  }
  .rail-scroller::-webkit-scrollbar {
    display: none;
  }

  /* A dragged strip must not select the card titles as it goes — a pan
     across three cards would otherwise leave three highlighted names
     behind it. Scoped to the scroller so the rest of the page keeps
     ordinary text selection. */
  .rail-scroller {
    user-select: none;
    -webkit-user-select: none;
  }
</style>
