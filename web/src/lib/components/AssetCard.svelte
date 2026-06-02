<script lang="ts">
  // Single asset card for the browse grid. Three rendering layers:
  //
  //   1. Thumbhash placeholder (CSS background-image, ~30-byte data
  //      URI decoded inline). Visible immediately, no HTTP RTT.
  //   2. The col-sized JPEG variant (currently 320² @ q82). Fades
  //      in over the placeholder once it arrives.
  //   3. Fallback chain when col is missing: retry with backoff
  //      (worker may still be generating) → /file (original) → icon
  //      placeholder for non-image assets.
  //
  // The variant URL is content-addressed so the response carries
  // long-lived Cache-Control + ETag (set by the VariantCache
  // middleware). Subsequent grid renders are 304s.

  import { onMount } from 'svelte';
  import { decodeThumbhash } from '$lib/util/thumbhash';

  interface Asset {
    id: string;
    title: string;
    file_hash?: string | null;
    file_extension?: string | null;
    asset_type: number;
    created_at: string;
    thumbhash?: string | null;
  }

  import { isVideoExt, is3DExt, isDocExt } from './viewers/controller';
  const isVideo = isVideoExt;
  const is3D = is3DExt;
  const isDoc = isDocExt;

  interface Props {
    asset: Asset;
  }

  let { asset }: Props = $props();

  // The col variant is the canonical grid thumbnail. /file is the
  // last-resort fallback when no variant exists yet (or never will,
  // for non-raster originals).
  const colUrl = $derived(asset.file_hash ? `/api/v1/assets/${asset.id}/variants/col` : '');
  const fullUrl = $derived(asset.file_hash ? `/api/v1/assets/${asset.id}/file` : '');

  // Decoded thumbhash → CSS background. Computed lazily once mounted
  // so the SSR snapshot stays light.
  let placeholder = $state<string | null>(null);
  onMount(() => {
    placeholder = decodeThumbhash(asset.thumbhash);
  });

  // Current variant URL we're trying. Starts at col, falls back to
  // /file after backoff exhaustion.
  let imgSrc = $state('');
  let imgLoaded = $state(false);
  let attempt = $state(0);
  let imgError = $state(false);
  let retryTimer: ReturnType<typeof setTimeout> | null = null;

  // Exponential backoff capped at 30s. The worker pool is fast
  // (~50ms per raster) so most retries succeed on the first or
  // second attempt; longer backoffs only matter for the trailing
  // edge of a big backfill.
  const BACKOFF_MS = [800, 1500, 3000, 6000, 12000, 30000];

  $effect(() => {
    // Doc assets render a typed card (see template) so we skip the
    // col fetch entirely — no thumbnail variant exists for text.
    if (!colUrl || assetIsDoc) {
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
    // First-line fallback: maybe the worker hasn't generated col
    // yet. Try again with backoff.
    if (attempt < BACKOFF_MS.length && imgSrc === colUrl) {
      const wait = BACKOFF_MS[attempt];
      attempt += 1;
      retryTimer = setTimeout(() => {
        // Cache-bust so the browser doesn't serve the cached 404.
        imgSrc = `${colUrl}?r=${attempt}`;
      }, wait);
      return;
    }
    // Second-line fallback: the original /file (may be huge for some
    // formats but at least renders something).
    if (imgSrc !== fullUrl && fullUrl) {
      imgSrc = fullUrl;
      return;
    }
    // Out of options — show the icon placeholder.
    imgError = true;
  }

  const created = $derived(new Date(asset.created_at));
  const createdShort = $derived(
    created.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  );

  // Sprite-sheet hover preview for video assets (same scheme as
  // PostCard). Video covers walk the preview.video 10×10 timeline
  // sheet; 3D covers walk the preview.model 6×6 turntable sheet.
  // Both serve from the same sprites.jpg variant.
  const assetIsVideo = $derived(isVideo(asset.file_extension));
  const assetIs3D = $derived(is3D(asset.file_extension));
  const assetIsDoc = $derived(isDoc(asset.file_extension));
  const assetHasSpriteScrub = $derived(assetIsVideo || assetIs3D);
  const spriteUrl = $derived(`/api/v1/assets/${asset.id}/variants/sprites.jpg`);
  const spriteCols = $derived(assetIs3D ? 6 : 10);
  const spriteRows = $derived(assetIs3D ? 6 : 10);
  const spriteCells = $derived(spriteCols * spriteRows);

  let hovering = $state(false);
  let spriteFrame = $state(0);
  let spriteInterval: ReturnType<typeof setInterval> | null = null;
  function onHoverEnter() {
    hovering = true;
    if (!assetHasSpriteScrub) return;
    if (spriteInterval) return;
    spriteInterval = setInterval(() => {
      spriteFrame = (spriteFrame + 1) % spriteCells;
    }, 120);
  }
  function onHoverLeave() {
    hovering = false;
    if (spriteInterval) {
      clearInterval(spriteInterval);
      spriteInterval = null;
    }
    spriteFrame = 0;
  }
</script>

<a
  href="/assets/{asset.id}"
  onmouseenter={onHoverEnter}
  onmouseleave={onHoverLeave}
  class="group block overflow-hidden rounded-lg bg-surface-elevated border border-border hover:border-fg-muted/60 transition-colors"
>
  <div
    class="relative aspect-square bg-surface bg-cover bg-center"
    style={placeholder ? `background-image: url(${placeholder})` : undefined}
  >
    {#if assetIsDoc}
      <!-- Typed doc card — text/code don't get a rasterised preview
           variant, so we render a file-shape with the extension. -->
      <div class="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-gradient-to-br from-surface-elevated to-surface text-fg-muted/80">
        <svg xmlns="http://www.w3.org/2000/svg" width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round">
          <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
          <polyline points="14 2 14 8 20 8" />
          <line x1="8" y1="13" x2="16" y2="13" />
          <line x1="8" y1="17" x2="16" y2="17" />
          <line x1="8" y1="9" x2="12" y2="9" />
        </svg>
        {#if asset.file_extension}
          <span class="rounded bg-black/40 px-2 py-0.5 font-mono text-[10px] uppercase tracking-wider text-fg">
            {asset.file_extension.replace(/^\./, '')}
          </span>
        {/if}
      </div>
    {:else if asset.file_hash && !imgError}
      <img
        src={imgSrc}
        alt={asset.title}
        loading="lazy"
        decoding="async"
        class="absolute inset-0 h-full w-full object-cover transition-opacity duration-200 group-hover:scale-[1.02]"
        class:opacity-0={!imgLoaded}
        class:opacity-100={imgLoaded}
        onload={onLoad}
        onerror={onError}
      />
      {#if assetHasSpriteScrub && hovering}
        <div
          class="pointer-events-none absolute inset-0 bg-cover bg-no-repeat transition-opacity duration-150"
          style="background-image: url({spriteUrl}); background-size: {spriteCols * 100}% {spriteRows * 100}%; background-position: {(spriteFrame % spriteCols) * (100 / (spriteCols - 1))}% {Math.floor(spriteFrame / spriteCols) * (100 / (spriteRows - 1))}%;"
        ></div>
      {/if}
      {#if assetIsVideo}
        <div class="pointer-events-none absolute left-2 top-2 inline-flex items-center gap-1 rounded-full bg-black/65 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm">
          <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="currentColor"><polygon points="6 4 20 12 6 20 6 4" /></svg>
          video
        </div>
      {:else if assetIs3D}
        <div class="pointer-events-none absolute left-2 top-2 inline-flex items-center gap-1 rounded-full bg-black/65 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm">
          <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" /><polyline points="3.27 6.96 12 12.01 20.73 6.96" /><line x1="12" y1="22.08" x2="12" y2="12" /></svg>
          3D
        </div>
      {/if}
    {:else if !placeholder}
      <!-- No thumbhash either — fall back to the icon. -->
      <div class="absolute inset-0 flex items-center justify-center text-fg-muted/40">
        <svg
          xmlns="http://www.w3.org/2000/svg"
          width="48"
          height="48"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <rect x="3" y="3" width="18" height="18" rx="2" />
          <circle cx="9" cy="9" r="2" />
          <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
        </svg>
      </div>
    {/if}

    <!-- Hover overlay with title -->
    <div
      class="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/85 via-black/50 to-transparent
             p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
    >
      <p class="text-sm font-medium text-white line-clamp-2">{asset.title}</p>
      <p class="text-xs text-white/70 mt-0.5">{createdShort}</p>
    </div>
  </div>
</a>
