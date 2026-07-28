<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Single asset card for the browse / profile / collection grids. The
  // thumbnail itself (thumbhash placeholder, col variant, sprite scrub,
  // typed-doc + icon fallbacks, RS matte frame) lives in the shared
  // CardThumb component (#515 slice 1) so AssetCard and PostCard render
  // one identical treatment. This card adds the link wrapper + the
  // hover title overlay.

  import CardThumb from './CardThumb.svelte';
  import CardMenu from './CardMenu.svelte';
  import CardCheckbox from './CardCheckbox.svelte';
  import { selection } from '$stores/selection.svelte';
  import { cardTooltip } from '$stores/cardTooltip.svelte';
  import { DEFAULT_TILE_SIZES, type ViewMode } from '$stores/browseView.svelte';
  import type { CardAsset } from '$components/cardAsset';

  // The card feed contract lives in cardAsset.ts, not here, because it
  // is shared with PostCard and because its fields are REQUIRED — the
  // presentation-critical ones were optional until #595 and a surface
  // dropped two of them with no type error. Read that file before
  // widening this prop back out.

  interface Props {
    asset: CardAsset;
    /** Active view mode (#515 slice 4). Grid = clean dense wall (no frame,
     *  hover-only title); thumbnail = framed "details" tile with a
     *  persistent metadata footer. */
    mode?: ViewMode;
    /** Slot width for `<img sizes>` — browseView's `tileSizes`. This card
     *  used to pass nothing, so every asset tile on the collection and
     *  profile grids advertised CardThumb's hardcoded `22rem` at every
     *  viewport and every rung of the size stepper (#639). */
    tileSizes?: string;
  }

  let { asset, mode = 'grid', tileSizes = DEFAULT_TILE_SIZES }: Props = $props();

  // Grid reads as a clean dense wall (no frame, hover-only title). The
  // other modes keep the gallery frame + a persistent footer in
  // thumbnail. See CardThumb `framed`.
  const framed = $derived(mode !== 'grid');
  const detailed = $derived(mode === 'thumbnail');

  // Masonry only (#652). Its tiles are the shape of their images since
  // #646, so the thinnest are ~60px — the floor CardThumb clamps them
  // to, which is one 44px tap target plus its inset. At that size the
  // overlay holds the ⋮ menu and the checkbox and nothing else; the
  // title/date that grid paints across the bottom of the artwork would
  // cover the entire work. Those facts move to the hover tooltip.
  const compact = $derived(mode === 'masonry');

  // Hover state lives on the interactive <a> and feeds CardThumb's
  // sprite-scrub (keeps hover listeners off the presentation frame).
  let hovering = $state(false);

  // Selected state (#515 slice 3) — the card gets a ring, the checkbox a
  // check. Read from the shared selection singleton.
  const selected = $derived(selection.has(asset.id));

  // #555 — grid is a zero-gap CONTACT SHEET: drop the card chrome
  // (rounded / border / elevated bg) so tiles butt into one unbroken
  // wall, and let hover lift + enlarge the tile. The z-lift matters at
  // zero gap: without it a scaling tile slides under its neighbours.
  // Selection uses an INSET ring here so it reads inside the tile
  // instead of bleeding over the adjacent one.
  //
  // #596 — the separation between grid tiles is PER-TILE, not a grid
  // gap, matching the reference: its artwork grid declares no gap at all
  // and each tile link carries `padding:2px; border-radius:4px`. Two
  // neighbours therefore put 4px between their images (2px each) with
  // rounded corners, which is the "almost 1px" softness the owner was
  // describing. The grid itself stays flush (ContentGrid keeps gap-0).
  //
  // Literal px, not Tailwind's rem-based scale: the reference stylesheet
  // uses px throughout and our previous `gap-2` was 0.5rem, so it
  // rescaled with the user's font size — a gutter that grows when
  // someone bumps their browser text is not the same design.
  //
  // The 2px ring is filled with `bg-thumb-matte` rather than left to
  // show the page. The matte is deliberately offset from the page
  // (#590), so it separates light artwork on a light page — which the
  // page colour itself would not: at 95% against near-white art there
  // are ~3 L points, and that near-invisibility is the whole reason the
  // matte token exists.
  const wrapperClass = $derived(
    framed
      ? `rounded-lg bg-surface-elevated border ${selected ? 'border-accent ring-2 ring-accent' : 'border-border hover:border-fg-muted/60'}`
      : `p-[2px] rounded-[4px] bg-thumb-matte hover:z-10 hover:scale-[1.03] ${selected ? 'z-10 ring-2 ring-inset ring-accent' : ''}`,
  );

  const created = $derived(new Date(asset.created_at));
  const createdShort = $derived(
    created.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
  );

  // Tooltip payload (#652). Scan-level facts only — type, size, date.
  // Deliberately NOT the details card: masonry's job is looking at a lot
  // of things quickly, and a tooltip you have to read is a tooltip that
  // stops you scanning. The dimensions are here because #646 made them
  // available and they are the one fact a thin tile actively hides.
  const tipMeta = $derived(
    [
      asset.file_extension ? asset.file_extension.replace(/^\./, '').toUpperCase() : null,
      asset.pixel_width && asset.pixel_height ? `${asset.pixel_width} × ${asset.pixel_height}` : null,
      createdShort,
    ].filter((v): v is string => !!v),
  );

  function tipEnter(e: MouseEvent) {
    hovering = true;
    if (compact) cardTooltip.enter(asset.id, { title: asset.title, meta: tipMeta }, e);
  }
  function tipMove(e: MouseEvent) {
    if (compact) cardTooltip.move(asset.id, e);
  }
  function tipLeave() {
    hovering = false;
    if (compact) cardTooltip.leave(asset.id);
  }
</script>

<!--
  Stretched-link pattern (#515 slice 2): the card is a container, not an
  <a>, so the tool row's <button>s aren't illegally nested in an anchor.
  A whole-card <a> covers the thumb for navigation (z-[1]); the title
  overlay is pointer-events-none; the tool row (z-20) captures its own
  clicks above the link.
-->
<div
  class="group relative block overflow-hidden transition duration-200 {wrapperClass}"
>
  {#if detailed}
    <!-- Details HEADER (#556). The owner's ask was "there should be a
         top to the thumbnail cards … title near the top": the title now
         LEADS the card instead of trailing it as a caption strip.
         Actions are NOT duplicated here — per the owner's 2026-07-25
         amendment the ⋮ CardMenu is the one action affordance in every
         mode, so it stays in its overlay position over the thumb.
         Kept self-contained: #552 swaps this field set for an
         operator-configured one, and wants that swap local. -->
    <div class="border-b border-border px-3 py-2">
      <a
        href="/assets/{asset.id}"
        class="block focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
      >
        <p class="truncate text-sm font-medium text-fg" title={asset.title}>{asset.title}</p>
      </a>
    </div>
  {/if}

  <CardThumb
    assetId={asset.id}
    title={asset.title}
    thumbhash={asset.thumbhash}
    fileExtension={asset.file_extension}
    hasFileHash={!!asset.file_hash}
    previewAvailable={asset.preview_available}
    ladderAvailable={asset.ladder_available}
    sizesHint={tileSizes}
    {hovering}
    {framed}
    fill={mode === 'grid'}
    variableAspect={mode === 'masonry'}
    {compact}
    pixelWidth={asset.pixel_width}
    pixelHeight={asset.pixel_height}
  >
    <!-- Whole-card navigation target. Hover here drives CardThumb's
         sprite-scrub (an interactive element, so no a11y warning) and,
         in masonry, the shared hover tooltip. The listeners hang off
         THIS element and not the frame because it is exactly the tile's
         box and it is interactive; `currentTarget` is therefore the
         rect the tooltip anchors to. -->
    <a
      href="/assets/{asset.id}"
      onmouseenter={tipEnter}
      onmousemove={tipMove}
      onmouseleave={tipLeave}
      class="absolute inset-0 z-[1]"
      aria-label={asset.title}
    ></a>

    <!-- Multi-select checkbox (top-left). -->
    <CardCheckbox id={asset.id} />

    {#if !detailed && !compact}
      <!-- Grid/feed: hover-only title overlay (clicks fall to the link).
           Thumbnail shows a persistent footer below instead; masonry
           shows the hover tooltip instead (#652) — see `compact`. -->
      <div
        class="pointer-events-none absolute inset-x-0 bottom-0 z-[2] bg-gradient-to-t from-black/85 via-black/50 to-transparent
               p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
      >
        <p class="text-sm font-medium text-white line-clamp-2">{asset.title}</p>
        <p class="text-xs text-white/70 mt-0.5">{createdShort}</p>
      </div>
    {/if}

    <!-- Overflow menu (info / share / add-to-collection). ONE affordance
         in every mode, including thumbnail — owner amendment 2026-07-25
         to #556, superseding "actions visible in the details tile". -->
    <CardMenu assetId={asset.id} detailPath="/assets/{asset.id}" />
  </CardThumb>

  {#if detailed}
    <!-- Details FOOTER: the secondary at-a-glance metadata. The title
         moved to the header (#556); this keeps the supporting fields
         below the image where they don't compete with it. -->
    <a href="/assets/{asset.id}" class="block px-3 py-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring">
      <p class="text-xs text-fg-muted">{createdShort}</p>
    </a>
  {/if}
</div>
