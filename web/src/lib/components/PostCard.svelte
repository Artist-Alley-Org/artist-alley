<script lang="ts">
  // Grid-mode tile for a Post. Three layers like AssetCard:
  //
  //   1. Thumbhash placeholder background (instant).
  //   2. Col-sized JPEG variant (fades in).
  //   3. Fallback chain: retry with backoff → /file → icon.
  //
  // Click behavior (Phase 1.13.F-1): default left-click intercepts
  // navigation and updates the URL to /?post={id}, which opens the
  // modal as an overlay over the still-mounted feed. Modifier-key
  // clicks (cmd/ctrl/shift, middle-click) fall through to the
  // native href so users still get new-tab / new-window behavior.

  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { decodeThumbhash } from '$lib/util/thumbhash';

  interface AssetSummary {
    id: string;
    file_hash?: string | null;
    file_extension?: string | null;
    thumbhash?: string | null;
  }
  interface PostMemberSummary {
    asset_id: string;
    asset: AssetSummary;
  }

  import { isVideoExt, is3DExt } from './viewers/controller';
  function isVideoAsset(a: AssetSummary | undefined | null): boolean {
    return isVideoExt(a?.file_extension);
  }
  function is3DAsset(a: AssetSummary | undefined | null): boolean {
    return is3DExt(a?.file_extension);
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
  }

  let { post }: Props = $props();

  // Pick the cover asset id. Falls back to the first member; falls
  // back further to nothing (placeholder).
  const coverAssetId = $derived(
    post.cover_asset_id ??
      (post.members.length > 0 ? post.members[0].asset_id : null),
  );
  const coverThumbhash = $derived(
    post.members.find((m) => m.asset_id === coverAssetId)?.asset?.thumbhash ?? null,
  );
  const hasFile = $derived(
    coverAssetId !== null &&
      post.members.some(
        (m) => m.asset_id === coverAssetId && !!m.asset.file_hash,
      ),
  );

  const colUrl = $derived(
    coverAssetId ? `/api/v1/assets/${coverAssetId}/variants/col` : '',
  );
  const fullUrl = $derived(
    coverAssetId ? `/api/v1/assets/${coverAssetId}/file` : '',
  );

  let placeholder = $state<string | null>(null);
  onMount(() => {
    placeholder = decodeThumbhash(coverThumbhash);
  });
  $effect(() => {
    placeholder = decodeThumbhash(coverThumbhash);
  });

  let imgSrc = $state('');
  let imgLoaded = $state(false);
  let attempt = $state(0);
  let imgError = $state(false);
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  const BACKOFF_MS = [800, 1500, 3000, 6000, 12000, 30000];

  $effect(() => {
    if (!colUrl || !hasFile) {
      imgSrc = '';
      return;
    }
    imgSrc = colUrl;
    imgLoaded = false;
    attempt = 0;
    imgError = false;
  });

  function onLoad() {
    imgLoaded = true;
  }

  function onError() {
    if (retryTimer) clearTimeout(retryTimer);
    if (attempt < BACKOFF_MS.length && imgSrc === colUrl) {
      const wait = BACKOFF_MS[attempt];
      attempt += 1;
      retryTimer = setTimeout(() => {
        imgSrc = `${colUrl}?r=${attempt}`;
      }, wait);
      return;
    }
    if (imgSrc !== fullUrl && fullUrl) {
      imgSrc = fullUrl;
      return;
    }
    imgError = true;
  }

  // Hover scrub preview for video covers. Animates the sprite sheet
  // already produced by preview.video — a single image download
  // (well-cached, content-addressed) gives ~100 frames of scrub. No
  // video decode, no extra network round-trips after first hover.
  const coverAsset = $derived(post.members.find((m) => m.asset_id === coverAssetId)?.asset);
  const coverIsVideo = $derived(!!coverAsset && isVideoAsset(coverAsset));
  const coverIs3D = $derived(!!coverAsset && is3DAsset(coverAsset));
  const spriteUrl = $derived(coverAssetId ? `/api/v1/assets/${coverAssetId}/variants/sprites.jpg` : '');

  let hovering = $state(false);
  let spriteFrame = $state(0);
  let spriteInterval: ReturnType<typeof setInterval> | null = null;
  const SPRITE_COLS = 10;
  const SPRITE_ROWS = 10;
  const SPRITE_CELL = SPRITE_COLS * SPRITE_ROWS;

  function onHoverEnter() {
    hovering = true;
    if (!coverIsVideo) return;
    if (spriteInterval) return;
    spriteInterval = setInterval(() => {
      spriteFrame = (spriteFrame + 1) % SPRITE_CELL;
    }, 120); // ~8 fps — feels lively without being seizurish
  }
  function onHoverLeave() {
    hovering = false;
    if (spriteInterval) {
      clearInterval(spriteInterval);
      spriteInterval = null;
    }
    spriteFrame = 0;
  }

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

<a
  href="/posts/{post.id}"
  onclick={handleClick}
  onmouseenter={onHoverEnter}
  onmouseleave={onHoverLeave}
  class="group block overflow-hidden rounded-lg bg-surface-elevated border border-border hover:border-fg-muted/60 transition-colors"
>
  <div
    class="relative aspect-square bg-surface bg-cover bg-center"
    style={placeholder ? `background-image: url(${placeholder})` : undefined}
  >
    {#if hasFile && !imgError}
      <img
        src={imgSrc}
        alt={post.title}
        loading="lazy"
        decoding="async"
        class="absolute inset-0 h-full w-full object-cover transition-opacity duration-200 group-hover:scale-[1.02]"
        class:opacity-0={!imgLoaded}
        class:opacity-100={imgLoaded}
        onload={onLoad}
        onerror={onError}
      />
      {#if coverIsVideo && hovering}
        <!-- Sprite-sheet scrub preview. The sheet is 10×10 = 100
             frames spanning the duration; we walk it at ~8 fps so
             the user sees a quick "trailer" of the video. -->
        <div
          class="pointer-events-none absolute inset-0 bg-cover bg-no-repeat transition-opacity duration-150"
          style="background-image: url({spriteUrl}); background-size: 1000% 1000%; background-position: {(spriteFrame % SPRITE_COLS) * (100 / (SPRITE_COLS - 1))}% {Math.floor(spriteFrame / SPRITE_COLS) * (100 / (SPRITE_ROWS - 1))}%;"
        ></div>
      {/if}
      {#if coverIsVideo}
        <!-- Play-glyph badge to advertise "this is a video". -->
        <div class="pointer-events-none absolute left-2 top-2 inline-flex items-center gap-1 rounded-full bg-black/65 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm">
          <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="currentColor"><polygon points="6 4 20 12 6 20 6 4" /></svg>
          video
        </div>
      {:else if coverIs3D}
        <!-- Cube glyph for 3D assets. -->
        <div class="pointer-events-none absolute left-2 top-2 inline-flex items-center gap-1 rounded-full bg-black/65 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm">
          <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" /><polyline points="3.27 6.96 12 12.01 20.73 6.96" /><line x1="12" y1="22.08" x2="12" y2="12" /></svg>
          3D
        </div>
      {/if}
    {:else if !placeholder}
      <div class="absolute inset-0 flex items-center justify-center text-fg-muted/40">
        <svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="18" height="18" rx="2" />
          <circle cx="9" cy="9" r="2" />
          <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
        </svg>
      </div>
    {/if}

    <!-- Multi-asset indicator badge (top-right). -->
    {#if memberCount > 1}
      <div
        class="absolute top-2 right-2 inline-flex items-center gap-1 rounded-full bg-black/60 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm"
        title="{memberCount} assets"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="14" height="14" rx="2" />
          <path d="M7 21h14a2 2 0 0 0 2-2V8" />
        </svg>
        {memberCount}
      </div>
    {/if}

    <!-- Hover overlay -->
    <div
      class="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/85 via-black/50 to-transparent
             p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
    >
      <p class="text-sm font-medium text-white line-clamp-2">{post.title || 'Untitled'}</p>
      <p class="text-xs text-white/70 mt-0.5">
        {createdShort}{post.like_count > 0 ? ` · ♥ ${post.like_count}` : ''}
      </p>
    </div>
  </div>
</a>
