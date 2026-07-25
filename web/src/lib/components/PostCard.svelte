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
  import { selection } from '$stores/selection.svelte';
  import { t } from '$stores/lang.svelte';
  import type { ViewMode } from '$stores/browseView.svelte';

  interface AssetSummary {
    id: string;
    file_hash?: string | null;
    file_extension?: string | null;
    thumbhash?: string | null;
    preview_available?: boolean;
  }
  interface PostMemberSummary {
    asset_id: string;
    asset: AssetSummary;
  }
  interface Post {
    id: string;
    title: string;
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
    /** The active rung as a plain `${R}rem`, for `sizes` (not the
     *  clamp — `sizes` rejects clamp()). */
    tileSizesLen?: string;
    /** Active view mode (#515 slice 4). Grid = clean dense wall (no frame,
     *  hover-only title); thumbnail = framed "details" tile with a
     *  persistent metadata footer. */
    mode?: ViewMode;
  }

  let { post, feed = false, tileSizesLen = '22rem', mode = 'grid' }: Props = $props();

  // Grid reads clean/dense (no frame, hover-only title); the other modes
  // keep the gallery frame + a persistent footer in thumbnail.
  const framed = $derived(mode !== 'grid');
  const detailed = $derived(mode === 'thumbnail');

  // Pick the cover asset id (explicit cover → first member → nothing),
  // then resolve its summary from members. CardThumb turns these into
  // the framed thumbnail + preview_available gating.
  const coverAssetId = $derived(
    post.cover_asset_id ?? (post.members.length > 0 ? post.members[0].asset_id : null),
  );
  const coverAsset = $derived(post.members.find((m) => m.asset_id === coverAssetId)?.asset);
  const coverThumbhash = $derived(coverAsset?.thumbhash ?? null);
  const coverHasFile = $derived(!!coverAsset?.file_hash);
  const coverPreviewAvailable = $derived(!!coverAsset?.preview_available);
  const coverFileExtension = $derived(coverAsset?.file_extension ?? null);

  // A responsive `srcset` would need the wider preview/screen/hires
  // rungs, but preview_available only guarantees `col`; requesting a
  // missing rung is exactly the 404 #471 removes. `feed` / `tileSizesLen`
  // stay in the API for when a `ladder_available` signal brings it back.
  void feed;
  void tileSizesLen;

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
  const wrapperClass = $derived(
    framed
      ? `rounded-lg bg-surface-elevated border ${selected ? 'border-accent ring-2 ring-accent' : 'border-border hover:border-fg-muted/60'}`
      : `hover:z-10 hover:scale-[1.03] ${selected ? 'z-10 ring-2 ring-inset ring-accent' : ''}`,
  );

  const memberCount = $derived(post.members.length);

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
  <CardThumb
    assetId={coverAssetId}
    title={post.title || 'Untitled'}
    thumbhash={coverThumbhash}
    fileExtension={coverFileExtension}
    hasFileHash={coverHasFile}
    previewAvailable={coverPreviewAvailable}
    {hovering}
    {framed}
  >
    <!-- Whole-card navigation target (modal intercept + permalink
         fallback). Hover here drives CardThumb's sprite-scrub. -->
    <a
      href="/posts/{post.id}"
      onclick={handleClick}
      onmouseenter={() => (hovering = true)}
      onmouseleave={() => (hovering = false)}
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
         bundles more than one asset. -->
    {#if memberCount > 1}
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

    {#if !detailed}
      <!-- Grid/masonry/feed: hover-only title overlay (clicks fall to the
           link). Thumbnail shows a persistent footer below instead. -->
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

    <!-- Overflow menu. add-to-collection targets the cover asset. -->
    <CardMenu assetId={coverAssetId} detailPath="/posts/{post.id}" />
  </CardThumb>

  {#if detailed}
    <!-- Thumbnail ("details") footer: the default at-a-glance field set —
         title + date + like/comment counts (all already on the post).
         #552 will make this operator-configurable; kept self-contained so
         that swap is local. -->
    <a href="/posts/{post.id}" onclick={handleClick} class="block px-3 py-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring">
      <p class="truncate text-sm font-medium text-fg" title={post.title || 'Untitled'}>{post.title || 'Untitled'}</p>
      <p class="mt-0.5 flex items-center gap-2 text-xs text-fg-muted">
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
