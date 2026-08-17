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
  import CardKindBadge from './CardKindBadge.svelte';
  import CardAuthorLink from './CardAuthorLink.svelte';
  import { kindForAsset } from './viewers/controller';
  import { thumbhashMatteColor } from '$lib/util/thumbhash';
  import { auth } from '$stores/auth.svelte';
  import { selection } from '$stores/selection.svelte';
  import { cardTooltip } from '$stores/cardTooltip.svelte';
  import { DEFAULT_TILE_SIZES, type ViewMode } from '$stores/browseView.svelte';
  import { masonryLayout, masonryOverlayTier } from '$stores/masonryLayout.svelte';
  import { t } from '$stores/lang.svelte';
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

  // #883 — this row is a container-member placeholder for something the
  // viewer may not see. Every title, link and action below is suppressed
  // for it; see the CardThumb call. Only a container surface sets it.
  const restricted = $derived(!!asset.restricted);

  // ── Per-density posture (#1047) ──────────────────────────────────
  //
  // THE TWO WALL DENSITIES WEAR NO CARD CHROME. Grid is a contact sheet
  // and masonry is "maximum art per page; barely any metadata" (owner's
  // density table), and a card frame is metadata about a card: a
  // rounded, bordered, elevated panel around every tile spends pixels
  // saying "this is a card" on a surface whose whole job is saying "this
  // is the work". Masonry lost that frame in this pass — see the strip
  // note on `wrapperClass`.
  //
  // Thumbnail, list and feed keep it. They are reading surfaces, where
  // the panel is what separates one item's metadata from the next's.
  const framed = $derived(mode !== 'grid' && mode !== 'masonry');
  const detailed = $derived(mode === 'thumbnail');

  // ── Masonry's two postures (#652, split by scale in #1047) ────────
  //
  // Its tiles are the shape of their images since #646, so the thinnest
  // are ~60px — the floor CardThumb clamps them to, which is one 44px
  // tap target plus its inset. At that size the overlay holds the ⋮ menu
  // and the checkbox and nothing else; the title/date that grid paints
  // across the bottom of the artwork would cover the entire work. Those
  // facts move to the hover tooltip.
  //
  // ABOVE THE CALIBRATED BOX the tile is wider than a default grid tile
  // and there is nothing compact left to justify, so it takes the
  // ordinary hover chrome back (owner amendment). The switch is the
  // RENDERED BOX and not the rung — see masonryLayout for why, and for
  // where all three numbers come from.
  //
  // HEIGHT IS HALF THE QUESTION (#1139). Width alone let a wide, short
  // spanning tile through and then clipped the artist's avatar and name
  // off its bottom edge; between the two heights the overlay compresses
  // to the title alone instead. The twin in PostCard carries the same
  // three tiers from the same function — one rule, two cards.
  const overlayTier = $derived(
    mode === 'masonry' ? masonryOverlayTier(masonryLayout.box(asset.id)) : 'minimal',
  );
  const masonryWide = $derived(mode === 'masonry' && overlayTier !== 'minimal');
  const compact = $derived(mode === 'masonry' && !masonryWide);

  // The kind, as the icon vocabulary #1111 established (#1047). Resolved
  // through `kindForAsset` and not the extension, because a PNG uploaded
  // as a sprite atlas is a sprite sheet and only the asset type says so.
  const kind = $derived(
    kindForAsset({ asset_type: asset.asset_type, file_extension: asset.file_extension }),
  );

  // The artist, when the server disclosed one (#1047). Absent is the ADR
  // 0024 opt-out working — see cardAsset.ts — so this renders nothing
  // rather than a placeholder identity.
  const owner = $derived(asset.owner ?? null);

  // The extension, bare and without its dot, for thumbnail's band.
  //
  // THIS IS NOT A REVERSAL OF #1158 — it is that ruling meeting the
  // OTHER card. #1158 took the extension out of the POST band, where
  // the surface is browse: a discovery wall of finished work, on which
  // "png" next to an icon that already says "image" was a second and
  // coarser answer to a question nobody was asking.
  //
  // An ASSET card is the working surface — a person's own uploads
  // (#1161 makes that this component's main home), where the file IS
  // the unit and "which of these is the TXT and which is the PNG" is a
  // question the wall gets asked constantly. The owner drew that line
  // explicitly. So the extension rides the asset band and stays off the
  // post band, and the two are no longer twins in this one respect.
  const extension = $derived((asset.file_extension ?? '').replace(/^\./, ''));

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
  //
  // #1047 — MASONRY NOW TAKES THE UNFRAMED BRANCH TOO. It used to render
  // the same elevated, bordered, rounded panel as the thumbnail details
  // card, with CardThumb letterboxing the art inside a further 6px
  // matte inset. On a wall whose stated job is "maximum art per page"
  // that is two rings and a gutter of chrome per tile, around artwork
  // that is already cut to its own shape and therefore cannot letterbox
  // against the box it is in. Both are gone; the 2px matte separator is
  // what remains, exactly as in grid.
  const wrapperClass = $derived(
    framed
      ? `rounded-lg bg-surface-elevated border ${selected ? 'border-accent ring-2 ring-accent' : 'border-border hover:border-fg-muted/60'}`
      : `p-[2px] rounded-[4px] bg-thumb-matte hover:z-10 hover:scale-[1.03] ${selected ? 'z-10 ring-2 ring-inset ring-accent' : ''}`,
  );

  // The upload date. Already here for the masonry hover tooltip; the
  // owner's ruling ("I want to see a date too like posts have") now
  // also puts it on thumbnail's last metadata row, and it is the SAME
  // derived value in PostCard's exact format rather than a second one.
  const created = $derived(new Date(asset.created_at));
  const createdShort = $derived(
    created.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
  );

  // ── The operator-configured metadata line (#552, extended by #1047) ─
  //
  // THE CONFIGURED FIELD SET IS `show_on_card`. There is no second key
  // naming field codes, and adding one was considered and rejected in
  // this pass — see the handoff. #552's flag already IS the operator's
  // array: it is per-field, admin-editable at /admin/fields, ordered by
  // (display_group, display_order, code), resolves #822's mirrored
  // `title`/`description` columns through the same query, federates with
  // the definition, and — the part a config array could not carry — is
  // refused by a CHECK constraint on any field holding a
  // `read_capability`, so it cannot become a side door around a gate.
  // A sysconfig list of codes would have re-implemented that guarantee
  // in a validator, which is a second enforcement point for one rule.
  //
  // Capped rather than rendered in full: the line is two rows of a tile,
  // and a card that grows with the catalogue's field count stops being a
  // card. The cap is presentation, not policy — the server sends
  // everything marked, and a surface with more room may show more.
  const CARD_FIELD_LIMIT = 3;

  // THE DEFAULT IS TITLE ONLY (#1047). With nothing marked, the tile
  // shows its title and its artist and no metadata line at all — which
  // is the mature DAM's own default for this density, and the honest
  // floor for "information at a glance": a date nobody configured is a
  // field the operator did not ask for. The `createdShort` line that
  // used to fill this space is gone; the date is still one `show_on_card`
  // away, and it is still in masonry's hover tooltip.
  //
  // A field MIRRORING the title is dropped here rather than printed
  // (#822): an operator who marks `title` at-a-glance means "put the
  // title on the card", and the card already leads with it — printing it
  // twice, eight pixels apart, is what that setting would otherwise do.
  const cardFields = $derived(
    (asset.card_fields ?? [])
      .filter((f) => f.value !== asset.title)
      .slice(0, CARD_FIELD_LIMIT),
  );

  // Provenance (#552). Federated content uses the same card, the same
  // viewer and the same hints — seamless — and carries an attribution line
  // so a viewer never mistakes another server's work for something this
  // instance vouches for. Both halves of the operator's constraint bind:
  // "federation should work seamlessly, but be distinct enough to know
  // it's from another server."
  const origin = $derived(asset.origin ?? null);
  const originHost = $derived.by(() => {
    if (!origin?.instance_url) return '';
    try {
      return new URL(origin.instance_url).host;
    } catch {
      return '';
    }
  });

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
      // Masonry paints nothing across the artwork, so the tooltip is
      // where its facts live — including whose the work is.
      origin ? t('card.origin_from', { peer: origin.display_name }) : null,
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

  // The card's edit affordance (#549). Offered when this viewer is
  // plainly entitled to it: the owner, or a holder of the global
  // content-management grant. Both are disjuncts of canMutateAsset
  // (assets/handler.go) that the client can evaluate exactly.
  //
  // The two it CANNOT evaluate are left out, and each errs in the safe
  // direction for a menu item. A team-scoped assets.admin grant is
  // invisible to auth.caps (documented there as the GLOBAL set), so a
  // scoped holder gets no shortcut from this menu — /assets/{id}/edit
  // is still linkable and still lets them in. And an asset whose
  // owner_user_ref the surface did not carry reads as "not mine",
  // because a menu item that appears for everyone on a hand-mapped grid
  // is worse than one that appears on most of them.
  const canEdit = $derived(
    !!auth.user
      && (
        (asset.owner_user_ref != null && asset.owner_user_ref === auth.user.ref)
        || auth.can('assets.admin')
      ),
  );
</script>

<!--
  Stretched-link pattern (#515 slice 2): the card is a container, not an
  <a>, so the tool row's <button>s aren't illegally nested in an anchor.
  A whole-card <a> covers the thumb for navigation (z-[1]); the title
  overlay is pointer-events-none; the tool row (z-20) captures its own
  clicks above the link.
-->
<!-- `data-select-id` is what the marquee hit-tests against (#1177), and
     it carries the ASSET id — the same id CardCheckbox contributes to
     the selection store, so a band and a click build one set rather
     than two. PostCard has had this attribute since #1127; AssetCard
     never got it, which made marquee-drag select ZERO cards on the
     profile uploads grid while the checkbox and Shift+range worked
     fine (they go through CardCheckbox, not the hit-test).

     On the CARD ROOT, for the same reason it is on PostCard's: the band
     selects a card when it touches the card, not when it happens to
     clip a 24px control in one corner.

     Present on a restricted card too (#883 renders a placeholder tile
     from this same root). That is deliberate: a redacted member is
     still a row the reader can act on in bulk, and skipping it would
     make a sweep silently drop cards it visibly crossed. -->
<div
  data-select-id={asset.id}
  class="group relative block overflow-hidden transition duration-200 {wrapperClass}"
>
  {#if detailed && !restricted}
    <!-- ═══ #1136: the TOP CHROME BAND ═════════════════════════════
         TYPE ICON THEN EXTENSION on the left, CHECKBOX on the right.

         The ⋯ menu has left this band the same way it left PostCard's,
         for the same ruling and with the same sizing consequence — see
         that band and CardMenu's trigger comment. What differs is the
         EXTENSION, which #1158 removed from both bands and the owner
         has now put back on this one only: "The ones that are all the
         assets uploaded, should show the extension next to the asset
         type icon." The reasoning is on `extension` in the script — the
         short version is that a post wall is for discovery and an asset
         wall is for finding a particular FILE, so the two surfaces
         genuinely want different labels and this is no longer
         PostCard's twin in that one respect.

         The icon comes FIRST and the word second: the glyph is the
         notation the whole card vocabulary is built on (#1111/#1047)
         and the extension qualifies it, which is also the order the
         band read in before #1136 flipped it. Absent extension renders
         NOTHING rather than an empty slot — the icon still answers the
         question on its own, which is exactly what #1158 established.

         It REPLACES #556's title header; the title moves down into the
         metadata stack where one fact per row reads as a record. -->
    <div
      class="flex items-center gap-2 border-b border-border px-1.5 py-0.5"
      data-testid="thumb-band-top"
    >
      <!-- Glyph and word as ONE unit, no gap of their own, so the only
           thing between them is the badge pill's 6px trailing padding
           (was 14px: that padding plus the band's `gap-2`). PostCard's
           band made the same move for the same owner ruling, and the
           two are kept identical on purpose — a one-asset post and a
           standalone asset are showing the same fact in the same
           place. -->
      <div class="flex min-w-0 items-center">
        <CardKindBadge {kind} variant="inline" tooltipKey={asset.id} />
        {#if extension}
          <!-- The same type scale the band wore for this text before
               #1158 — 11px, uppercase, tracked, muted. Restored rather
               than re-picked, so the band looks like itself. `min-w-0`
               lets it truncate instead of shoving the checkbox. -->
          <span
            class="min-w-0 truncate text-[11px] font-medium uppercase tracking-wide text-fg-muted"
            data-testid="thumb-band-extension"
          >{extension}</span>
        {/if}
      </div>
      <span class="flex-1"></span>
      <CardCheckbox id={asset.id} placement="inline" />
    </div>
  {/if}

  <CardThumb
    assetId={asset.id}
    title={asset.title}
    thumbhash={asset.thumbhash}
    fileExtension={asset.file_extension}
    assetType={asset.asset_type}
    hasFileHash={!!asset.file_hash}
    previewAvailable={asset.preview_available}
    ladderAvailable={asset.ladder_available}
    scrubAvailable={asset.scrub_available}
    sizesHint={tileSizes}
    {hovering}
    {framed}
    fill={mode === 'grid'}
    variableAspect={mode === 'masonry'}
    {compact}
    pixelWidth={asset.pixel_width}
    pixelHeight={asset.pixel_height}
    titleAdjacent={detailed}
    matteColor={detailed ? thumbhashMatteColor(asset.thumbhash) : null}
    {restricted}
    restrictedOwnerName={asset.owner_display_name ?? null}
    requestAssetId={restricted ? asset.id : null}
  >
    {#if restricted}
      <!-- #883 — no link, no menu, no checkbox, no hover title. The tile
           is a statement that something is here the viewer cannot see;
           every one of those affordances either navigates to a page that
           would restate the restriction, or offers an action on an asset
           the viewer has no handle on. The "request access" affordance
           that belongs in this space is #881. -->
    {:else}
      <!-- Whole-card navigation target. Hover here drives CardThumb's
           sprite-scrub (an interactive element, so no a11y warning) and,
           in masonry, the shared hover tooltip. The listeners hang off
           THIS element and not the frame because it is exactly the tile's
           box and it is interactive; `currentTarget` is therefore the
           rect the tooltip anchors to.

           `data-marquee-passthrough` is the OTHER half of #1177, and
           without it `data-select-id` alone buys almost nothing. This
           anchor covers the tile edge to edge (the stretched-link
           pattern, #515), so every press on an asset's artwork lands on
           an <a> — and the marquee refuses to arm on a press that
           begins on a control (marquee.svelte.ts `onControl`). A band
           could therefore only ever be STARTED from the gutter between
           tiles. PostCard's stretched link has carried the opt-out
           since #1127; this one is safe for the same reason its is: a
           press that never travels the 5px threshold stays a click and
           still opens the asset. -->
      <a
        href="/assets/{asset.id}"
        onmouseenter={tipEnter}
        onmousemove={tipMove}
        onmouseleave={tipLeave}
        class="absolute inset-0 z-[1]"
        aria-label={asset.title}
        data-marquee-passthrough
      ></a>

      <!-- Multi-select checkbox (top-left). NOT IN THUMBNAIL (#1136):
           it is an inline control in the bottom band there, so nothing
           sits over the preview. -->
      {#if !detailed}
        <CardCheckbox id={asset.id} />
      {/if}

      <!-- The kind, as an ICON and never as a word (#1047). This is the
           replacement for CardThumb's hardcoded `video` / `3D` text
           chip, which covered two of the thirteen kinds — the asymmetry
           PR #1124 flagged, since grid post cards had already moved to
           the icon in #1111.

           BOTTOM-RIGHT, and the corner is forced rather than chosen:
           top-left is the selection checkbox, top-right is the ⋮ menu,
           and bottom-LEFT is where the hover title overlay's type sits.
           The old text chip drew at `left-2 top-2` — on top of the
           checkbox — which is the collision #578 recorded and never
           resolved for this card.

           Suppressed under `compact`: a 60px masonry tile is one 44px
           control band tall, and the wall is about the art. The kind is
           in its hover tooltip there instead (#652). -->
      <!-- NOT IN THUMBNAIL (#1136): the same badge draws in the top
           chrome band, which leaves the artwork untouched. -->
      {#if !compact && !detailed}
        <CardKindBadge {kind} class="absolute bottom-2 right-2 z-[2]" tooltipKey={asset.id} />
      {/if}
    {/if}

    {#if !detailed && !compact && !restricted}
      <!-- Grid/feed: hover-only title overlay (clicks fall to the link).
           Thumbnail shows a persistent footer below instead; masonry
           shows the hover tooltip instead (#652) — see `compact`. -->
      <div
        class="pointer-events-none absolute inset-x-0 bottom-0 z-[2] bg-gradient-to-t from-black/85 via-black/50 to-transparent
               p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
      >
        <p class="text-sm font-medium text-white line-clamp-2">{asset.title}</p>
        <!-- The COMPRESSED tier keeps the title and drops everything
             under it (#1139). This overlay is bottom-anchored so it
             cannot spill past the picture the way PostCard's
             `justify-between` stack could — what it does on a short
             wide tile is cover nearly all of it, which on a density
             whose premise is "maximum art per page" is the same
             complaint wearing different clothes. One line of caption
             is what fits; the date and the peer stay in the tooltip. -->
        {#if overlayTier !== 'compressed'}
          <p class="text-xs text-white/70 mt-0.5">{createdShort}</p>
        {/if}
        {#if origin && overlayTier !== 'compressed'}
          <!-- Provenance rides EVERY density, not just the details tile
               (#552). Grid is the default view: attributing remote work
               only in a mode most people never switch to would leave it
               unattributed in practice, which is the half of the
               operator's constraint that is easy to drop. -->
          <p class="mt-0.5 text-[11px] text-white/60" data-testid="card-origin">
            <span aria-hidden="true">↗</span>
            {t('card.origin_from', { peer: origin.display_name })}
          </p>
        {/if}
      </div>
    {/if}

    {#if !restricted && !detailed}
      <!-- Overflow menu (info / copy link / edit / add-to-collection). ONE affordance
           in every mode, including thumbnail — owner amendment 2026-07-25
           to #556, superseding "actions visible in the details tile".
           Thumbnail renders the SAME component inline in its bottom band
           (#1136); still exactly one per card. -->
      <CardMenu
        detailPath="/assets/{asset.id}"
        editPath={canEdit ? `/assets/${asset.id}/edit` : null}
      />
    {/if}
  </CardThumb>

  {#if detailed && !restricted}
    <!-- ═══ #1136: the METADATA STACK ══════════════════════════════
         "Information at a glance, preview still clear" (owner's density
         table), now with the owner's placement grammar: one fact per
         row, BELOW the preview, never over it — title, artist, the
         operator's `show_on_card` fields (#552), then the DATE and the
         ⋯ menu.

         The date row is the owner's addition, and it makes this stack
         PostCard's shape rather than a shorter cousin of it: "I want to
         see a date too like posts have." An upload wall is where
         someone goes looking for the thing they made last week, and it
         was the one card of the two that could not answer that without
         being opened.

         The rows are unconditional where the fact exists, and the block
         itself is no longer gated on `owner || fields || origin`: the
         TITLE is always present, so a card with no artist and no
         configured fields still has a metadata stack, where before it
         rendered a bare picture with a header. -->
    <div class="space-y-1 px-3 py-2" data-testid="thumb-metadata">
      <a
        href="/assets/{asset.id}"
        class="block focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
      >
        <p class="truncate text-sm font-medium text-fg" title={asset.title}>{asset.title}</p>
      </a>
      {#if owner}
        <!-- The artist. A SIBLING link, not nested inside the card's
             own anchor — nested anchors are invalid HTML and
             unreachable by keyboard (#1126). -->
        <CardAuthorLink author={owner} size="sm" />
      {/if}
      <!-- The operator's `show_on_card` fields and the provenance line
           (#552). Optional both — a card with neither renders nothing
           here and goes straight from the artist to the date row. -->
      <a
        href="/assets/{asset.id}"
        class="block focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
      >
        {#if cardFields.length > 0}
          <dl class="space-y-0.5" data-testid="card-fields">
            {#each cardFields as f (f.code)}
              <div class="flex gap-1.5 text-xs">
                <dt class="shrink-0 text-fg-subtle">{f.label}</dt>
                <dd class="truncate text-fg-muted" data-testid="card-field-{f.code}">{f.value}</dd>
              </div>
            {/each}
          </dl>
        {/if}
        {#if origin}
          <p
            class="flex items-center gap-1 text-[11px] text-fg-subtle"
            data-testid="card-origin"
            title={originHost ? `${origin.display_name} — ${originHost}` : origin.display_name}
            aria-label={t('card.origin_label')}
          >
            <span aria-hidden="true">↗</span>
            <span class="truncate">{t('card.origin_from', { peer: origin.display_name })}</span>
          </p>
        {/if}
      </a>
      <!-- THE LAST ROW — the DATE and the ⋯ MENU (owner's ruling: "I
           want to see a date too like posts have", and "I like the menu
           bottom right").

           PostCard's twin row carries the date plus the engagement
           counts; an asset has no likes and no comments, so this one is
           the date alone. Deliberately NOT padded out with a
           substitute fact: the row exists because the date and the menu
           both belong at the bottom of the stack, not because the two
           cards have to have the same number of things in it.

           THE TRIGGER IS A SIBLING OF THE LINK, NOT A CHILD OF IT.
           Interactive content inside an <a> is invalid and the anchor's
           own handler would navigate on the way to opening the menu
           (#1126, and the identical note is on PostCard's row). The
           anchor takes the elastic space with `min-w-0 flex-1` and
           stays the row's click target; the trigger sits at its end.

           The row's height is the 16px `text-xs` line box, and the
           trigger is sized so it stays that — see CardMenu. -->
      <div class="flex items-center gap-2">
        <a
          href="/assets/{asset.id}"
          class="block min-w-0 flex-1 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
        >
          <p class="truncate text-xs text-fg-muted" data-testid="card-date">{createdShort}</p>
        </a>
        <CardMenu
          detailPath="/assets/{asset.id}"
          editPath={canEdit ? `/assets/${asset.id}/edit` : null}
          placement="inline"
        />
      </div>
    </div>

  {/if}
</div>
