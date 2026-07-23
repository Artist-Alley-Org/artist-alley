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
  import CardToolRow from './CardToolRow.svelte';

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
  }

  let { post, feed = false, tileSizesLen = '22rem' }: Props = $props();

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

  const memberCount = $derived(post.members.length);
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
  class="group relative block overflow-hidden rounded-lg bg-surface-elevated border border-border hover:border-fg-muted/60 transition-colors"
>
  <CardThumb
    assetId={coverAssetId}
    title={post.title || 'Untitled'}
    thumbhash={coverThumbhash}
    fileExtension={coverFileExtension}
    hasFileHash={coverHasFile}
    previewAvailable={coverPreviewAvailable}
    {hovering}
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

    <!-- Multi-asset indicator badge (top-right). Fades out when the tool
         row takes the same corner on hover / touch. -->
    {#if memberCount > 1}
      <div
        class="pointer-events-none absolute top-2 right-2 z-[2] inline-flex items-center gap-1 rounded-full bg-black/60 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm
               transition-opacity duration-150 group-hover:opacity-0 [@media(hover:none)]:opacity-0"
        title="{memberCount} assets"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="14" height="14" rx="2" />
          <path d="M7 21h14a2 2 0 0 0 2-2V8" />
        </svg>
        {memberCount}
      </div>
    {/if}

    <!-- Hover overlay (non-interactive — clicks fall to the link). -->
    <div
      class="pointer-events-none absolute inset-x-0 bottom-0 z-[2] bg-gradient-to-t from-black/85 via-black/50 to-transparent
             p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
    >
      <p class="text-sm font-medium text-white line-clamp-2">{post.title || 'Untitled'}</p>
      <p class="text-xs text-white/70 mt-0.5">
        {createdShort}{post.like_count > 0 ? ` · ♥ ${post.like_count}` : ''}
      </p>
    </div>

    <!-- Quick-action tool row. add-to-collection targets the cover asset. -->
    <CardToolRow assetId={coverAssetId} detailPath="/posts/{post.id}" />
  </CardThumb>
</div>
