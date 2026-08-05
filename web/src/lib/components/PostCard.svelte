<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Grid-mode tile for a Post. The thumbnail treatment (thumbhash
  // placeholder, col variant, sprite scrub, typed-doc + icon fallbacks,
  // RS matte frame) lives in the shared CardThumb component (#515 slice
  // 1) — the same one AssetCard renders — so browse / profile /
  // post-by-asset all show one identical treatment. This card derives
  // the COVER asset from the post's members, wraps the link, and adds
  // the multi-asset badge + hover overlay.
  //
  // Click behavior (Phase 1.13.F-1): default left-click intercepts
  // navigation and updates the URL to /?post={id}, which opens the
  // modal as an overlay over the still-mounted feed. Modifier-key
  // clicks (cmd/ctrl/shift, middle-click) fall through to the native
  // href so users still get new-tab / new-window behavior.

  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import CardThumb from './CardThumb.svelte';
  import CardMenu from './CardMenu.svelte';
  import CardCheckbox from './CardCheckbox.svelte';
  import { auth } from '$stores/auth.svelte';
  import { selection } from '$stores/selection.svelte';
  import { cardTooltip } from '$stores/cardTooltip.svelte';
  import { t } from '$stores/lang.svelte';
  import { DEFAULT_TILE_SIZES, type ViewMode } from '$stores/browseView.svelte';
  import type { CardCoverAsset } from '$components/cardAsset';

  // Cover-asset shape is the shared card feed contract (#595) — its
  // presentation fields are REQUIRED so a surface cannot hand-map a
  // narrower object and silently lose the media-type badge + sprite
  // scrub. See cardAsset.ts.
  // #883 — `asset` is ABSENT on a member the viewer may not see, and
  // `restricted` says so. Optional here for exactly that reason; every
  // read below already goes through `?.`, and the cover branch checks
  // the flag rather than inferring from the missing object.
  interface PostMemberSummary {
    asset_id: string;
    asset?: CardCoverAsset;
    restricted?: boolean;
    owner_display_name?: string;
  }
  interface Post {
    id: string;
    title: string;
    /** Required on the API's `Post` schema; optional here only because
     *  a couple of hand-built card feeds narrow the object. Absent means
     *  "not provably mine", which hides the manage-access action rather
     *  than offering one that 403s. */
    author_user_ref?: number;
    cover_asset_id?: string | null;
    created_at: string;
    like_count: number;
    comment_count: number;
    members: PostMemberSummary[];
  }

  interface Props {
    post: Post;
    /** Feed mode: the card is the full column width, so the image is
     *  rendered far larger than a grid tile. Only affects `sizes` —
     *  the card treatment is deliberately identical. */
    feed?: boolean;
    /** The slot width to advertise in `<img sizes>` — browseView's
     *  `tileSizes`, which mirrors the tile clamp rather than its
     *  ceiling (#639). Never a `clamp()` / `min()`: `sizes` is not CSS
     *  and discards the whole attribute when it sees one. */
    tileSizes?: string;
    /** Active view mode (#515 slice 4). Grid = clean dense wall (no frame,
     *  hover-only title); thumbnail = framed "details" tile with a
     *  persistent metadata footer. */
    mode?: ViewMode;
    /** The true size of the post's membership, when the caller's row
     *  carries fewer members than the post has (#850).
     *
     *  A search hit ships ONE member — the cover — because a tile renders
     *  one image and joining a whole membership per hit would be an
     *  unbounded query for pixels nobody looks at. The multi-asset badge
     *  still has to tell the truth, so the count travels beside the
     *  member instead of being inferred from the array's length.
     *  Undefined ⇒ the array IS the membership, which is the case on
     *  every list endpoint. */
    memberCount?: number;
  }

  let {
    post,
    feed = false,
    tileSizes = DEFAULT_TILE_SIZES,
    mode = 'grid',
    memberCount: memberCountProp,
  }: Props = $props();

  // Grid reads clean/dense (no frame, hover-only title); the other modes
  // keep the gallery frame + a persistent footer in thumbnail.
  const framed = $derived(mode !== 'grid');
  const detailed = $derived(mode === 'thumbnail');

  // Masonry only (#652) — the tile can be as short as the 60px control
  // floor, so the overlay carries the ⋮ menu and the checkbox and
  // nothing else. See the twin in AssetCard.
  const compact = $derived(mode === 'masonry');

  // Only the author gets the manage-access action (#880). The API gate
  // is `canMutatePost` — author, `posts.admin` or `system.admin` — and
  // this deliberately checks only the first: the card has the author ref
  // and not the caller's capabilities, so widening the client-side test
  // would mean guessing. An admin who needs the dialog opens the post,
  // where PostHost asks the same question with the post loaded. Guessing
  // wide here would show admins-in-waiting a menu item that 403s.
  const isAuthor = $derived(
    !!auth.user && post.author_user_ref !== undefined && post.author_user_ref === auth.user.ref,
  );

  // Pick the cover asset id (explicit cover → first member → nothing),
  // then resolve its summary from members. CardThumb turns these into
  // the framed thumbnail + preview_available gating.
  const coverAssetId = $derived(
    post.cover_asset_id ?? (post.members.length > 0 ? post.members[0].asset_id : null),
  );
  const coverMember = $derived(post.members.find((m) => m.asset_id === coverAssetId));
  const coverAsset = $derived(coverMember?.asset);
  // #883 — the cover itself can be a member the viewer may not see. The
  // tile then states the restriction and the owner instead of degrading
  // to the generic no-preview plate, which would be indistinguishable
  // from "this asset has no thumbnail yet".
  const coverRestricted = $derived(!!coverMember?.restricted);
  const coverOwnerName = $derived(coverMember?.owner_display_name ?? null);
  const coverThumbhash = $derived(coverAsset?.thumbhash ?? null);
  const coverHasFile = $derived(!!coverAsset?.file_hash);
  const coverPreviewAvailable = $derived(!!coverAsset?.preview_available);
  const coverFileExtension = $derived(coverAsset?.file_extension ?? null);

  // The `ladder_available` signal arrived in #610, so the responsive
  // srcset these two props were parked for is live (#502/#589) — the
  // `void` lines that dead-coded them are gone.
  //
  // `sizes` tells the browser how wide the slot actually is, which is
  // what stops a 200px tile pulling `hires`. Feed is one full-width
  // column capped at the `measure` (see ContentGrid); every other mode
  // is a tile at the user's chosen rung.
  const coverLadderAvailable = $derived(!!coverAsset?.ladder_available);

  // The hover-scrub gate (#835) — see CardThumb.scrubAvailable.
  const coverScrubAvailable = $derived(!!coverAsset?.scrub_available);

  // The cover's recorded dimensions (#640) — what lets a masonry tile
  // reserve its height before the image loads. `?? null` because a post
  // whose cover member is missing has no asset at all, which is the same
  // answer as "no dimensions recorded".
  const coverPixelWidth = $derived(coverAsset?.pixel_width ?? null);
  const coverPixelHeight = $derived(coverAsset?.pixel_height ?? null);
  // Feed is one column capped at a 46rem MEASURE (ContentGrid's
  // `.posts-feed { max-width: min(100%, 46rem) }`), so the slot is the
  // viewport up to that cap and the cap above it.
  //
  // Was `min(100vw, 46rem)`, which — unlike the tile hint — was already
  // telling the truth: measured in Chromium it resolves to 736px against
  // a 737px feed column. This is the same value in the portable spelling
  // plus the `auto` first entry, so feed and tiles share one shape and
  // neither depends on a CSS math function parsing inside a non-CSS
  // attribute. See browseView.tileSizes for the whole argument.
  const FEED_SIZES = 'auto, (max-width: 46rem) 100vw, 46rem';
  const sizesHint = $derived(feed ? FEED_SIZES : tileSizes);

  // Hover state lives on the interactive <a> and feeds CardThumb's
  // sprite-scrub animation.
  let hovering = $state(false);

  // Selected state (#515 slice 3). A browse card contributes its POST id
  // to the shared selection.
  const selected = $derived(selection.has(post.id));

  // #555 — grid is a zero-gap CONTACT SHEET: drop the card chrome
  // (rounded / border / elevated bg) so tiles butt into one unbroken
  // wall, and let hover lift + enlarge the tile. The z-lift matters at
  // zero gap: without it a scaling tile slides under its neighbours.
  // Selection uses an INSET ring here so it reads inside the tile
  // instead of bleeding over the adjacent one.
  //
  // #596 — per-tile 2px inset + 4px radius, filled with the matte, so
  // neighbours put 4px between their images while the GRID stays flush.
  // See the twin in AssetCard for the full reasoning (and why these are
  // literal px rather than Tailwind's rem scale).
  const wrapperClass = $derived(
    framed
      ? `rounded-lg bg-surface-elevated border ${selected ? 'border-accent ring-2 ring-accent' : 'border-border hover:border-fg-muted/60'}`
      : `p-[2px] rounded-[4px] bg-thumb-matte hover:z-10 hover:scale-[1.03] ${selected ? 'z-10 ring-2 ring-inset ring-accent' : ''}`,
  );

  const memberCount = $derived(memberCountProp ?? post.members.length);

  // The corner conflict was a THREE-way fight, not the two the brief
  // described (#578). Top-left already hosts the select checkbox AND
  // CardThumb's persistent video/3D media-type badge; top-right now hosts
  // the ⋮ menu. Putting the multi-asset indicator top-left too overlapped
  // the type badge on every video/3D multi-asset tile (measured). So it
  // goes BOTTOM-left — the one persistently-clear corner — still always
  // visible, still the single piece of resting chrome.

  const created = $derived(new Date(post.created_at));
  const createdShort = $derived(
    created.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
  );

  // Tooltip payload (#652) — the facts the compact overlay drops, in
  // scan order. Multi-asset count is here because the badge that used
  // to carry it is one of the things `compact` removes: it sits
  // bottom-right, which on a 60px tile is the SAME 44px band as the ⋮
  // menu top-right. Likes/comments deliberately stay out — they are
  // engagement, not identification, and this is not the details card.
  const tipMeta = $derived(
    [
      coverFileExtension ? coverFileExtension.replace(/^\./, '').toUpperCase() : null,
      coverPixelWidth && coverPixelHeight ? `${coverPixelWidth} × ${coverPixelHeight}` : null,
      memberCount > 1 ? t('card.multi.badge_label', { count: String(memberCount) }) : null,
      createdShort,
    ].filter((v): v is string => !!v),
  );

  function tipEnter(e: MouseEvent) {
    hovering = true;
    if (compact) {
      cardTooltip.enter(post.id, { title: post.title || 'Untitled', meta: tipMeta }, e);
    }
  }
  function tipMove(e: MouseEvent) {
    if (compact) cardTooltip.move(post.id, e);
  }
  function tipLeave() {
    hovering = false;
    if (compact) cardTooltip.leave(post.id);
  }

  async function handleClick(e: MouseEvent) {
    // Modifier-key / non-primary clicks fall through to the native
    // <a href>. Standard browser behavior: new tab, new window,
    // download — all preserved.
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) {
      return;
    }
    e.preventDefault();
    const target = new URL(page.url);
    target.searchParams.set('post', post.id);
    await goto(target.pathname + target.search, {
      keepFocus: true,
      noScroll: true,
    });
  }
</script>

<!--
  Stretched-link pattern (#515 slice 2): container div (not an <a>) so the
  tool row's <button>s aren't nested in an anchor. The whole-card <a>
  keeps the /?post={id} modal intercept (handleClick) with /posts/{id} as
  the modifier-click / new-tab fallback.
-->
<div
  class="group relative block overflow-hidden transition duration-200 {wrapperClass}"
>
  {#if detailed}
    <!-- Details HEADER (#556) — the title leads the card. See the twin in
         AssetCard for the reasoning; actions stay in the overlay ⋮ menu. -->
    <div class="border-b border-border px-3 py-2">
      <a
        href="/posts/{post.id}"
        onclick={handleClick}
        class="block focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
      >
        <p class="truncate text-sm font-medium text-fg" title={post.title || 'Untitled'}>
          {post.title || 'Untitled'}
        </p>
      </a>
    </div>
  {/if}

  <CardThumb
    assetId={coverAssetId}
    title={post.title || 'Untitled'}
    thumbhash={coverThumbhash}
    fileExtension={coverFileExtension}
    hasFileHash={coverHasFile}
    previewAvailable={coverPreviewAvailable}
    scrubAvailable={coverScrubAvailable}
    ladderAvailable={coverLadderAvailable}
    {sizesHint}
    {hovering}
    {framed}
    fill={mode === 'grid'}
    variableAspect={mode === 'masonry'}
    {compact}
    pixelWidth={coverPixelWidth}
    pixelHeight={coverPixelHeight}
    titleAdjacent={detailed}
    restricted={coverRestricted}
    restrictedOwnerName={coverOwnerName}
  >
    <!-- Whole-card navigation target (modal intercept + permalink
         fallback). Hover here drives CardThumb's sprite-scrub and, in
         masonry, the shared hover tooltip (#652). -->
    <a
      href="/posts/{post.id}"
      onclick={handleClick}
      onmouseenter={tipEnter}
      onmousemove={tipMove}
      onmouseleave={tipLeave}
      class="absolute inset-0 z-[1]"
      aria-label={post.title || 'Untitled'}
    ></a>

    <!-- Multi-select checkbox (top-left). -->
    <CardCheckbox id={post.id} />

    <!-- Multi-asset "stacked" indicator (#578). BOTTOM-right, PERSISTENT —
         the one piece of chrome that stays at rest, so a wall of art
         still signals which posts hold a set. Every other corner is
         claimed: checkbox + CardThumb's video/3D type badge top-left, ⋮
         menu top-right. Bottom-right is also clear of the hover title
         overlay, which is bottom-LEFT-aligned. Only shown when the post
         bundles more than one asset.

         Suppressed under `compact` (#652). On a 60px masonry tile
         "bottom-right" and "top-right" are the same 44px band, so the
         badge and the ⋮ menu sit on top of each other — the owner's
         "only keep the options and checkbox" resolves that collision.
         The count is in the hover tooltip there instead, so the signal
         is still available, just not at rest. This is the one place
         #580's "persistent at rest" property is traded away; if the
         owner wants it back in masonry, the tile floor has to grow. -->
    {#if memberCount > 1 && !compact}
      <div
        class="pointer-events-none absolute bottom-2 right-2 z-[2] inline-flex items-center gap-1 rounded-full bg-black/60 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm"
        aria-label={t('card.multi.badge_label', { count: String(memberCount) })}
        title={t('card.multi.badge_label', { count: String(memberCount) })}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.25" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <rect x="8" y="8" width="12" height="12" rx="2" />
          <path d="M4 16V6a2 2 0 0 1 2-2h10" />
        </svg>
        {memberCount}
      </div>
    {/if}

    {#if !detailed && !compact}
      <!-- Grid/feed: hover-only title overlay (clicks fall to the link).
           Thumbnail shows a persistent footer below instead; masonry
           shows the hover tooltip instead (#652). -->
      <div
        class="pointer-events-none absolute inset-x-0 bottom-0 z-[2] bg-gradient-to-t from-black/85 via-black/50 to-transparent
               p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
      >
        <p class="text-sm font-medium text-white line-clamp-2">{post.title || 'Untitled'}</p>
        <p class="text-xs text-white/70 mt-0.5">
          {createdShort}{post.like_count > 0 ? ` · ♥ ${post.like_count}` : ''}
        </p>
      </div>
    {/if}

    <!-- Overflow menu. ONE affordance in every mode, including thumbnail
         — owner amendment 2026-07-25 to #556. add-to-collection targets
         the cover asset. -->
    <CardMenu
      assetId={coverAssetId}
      detailPath="/posts/{post.id}"
      manageAccess={isAuthor ? { kind: 'post', id: post.id } : null}
    />
  </CardThumb>

  {#if detailed}
    <!-- Details FOOTER: the secondary at-a-glance fields — date +
         like/comment counts. The title moved to the header (#556).
         #552 will make this field set operator-configurable; kept
         self-contained so that swap is local. -->
    <a href="/posts/{post.id}" onclick={handleClick} class="block px-3 py-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring">
      <p class="flex items-center gap-2 text-xs text-fg-muted">
        <span>{createdShort}</span>
        {#if post.like_count > 0}
          <span class="inline-flex items-center gap-1" title={t('card.footer.likes', { count: String(post.like_count) })}>
            <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M12 21s-6.7-4.35-9.33-8.24C.9 10.06 1.6 6.5 4.6 5.4c2-.73 3.9.2 4.9 1.7l.5.75.5-.75c1-1.5 2.9-2.43 4.9-1.7 3 1.1 3.7 4.66 1.93 7.36C18.7 16.65 12 21 12 21z"/></svg>
            {post.like_count}
          </span>
        {/if}
        {#if post.comment_count > 0}
          <span class="inline-flex items-center gap-1" title={t('card.footer.comments', { count: String(post.comment_count) })}>
            <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>
            {post.comment_count}
          </span>
        {/if}
      </p>
    </a>
  {/if}
</div>
