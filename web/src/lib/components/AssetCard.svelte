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
    resource_type: number;
    created_at: string;
    thumbhash?: string | null;
  }

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
    if (!colUrl) {
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
</script>

<a
  href="/assets/{asset.id}"
  class="group block overflow-hidden rounded-lg bg-surface-elevated border border-border hover:border-fg-muted/60 transition-colors"
>
  <div
    class="relative aspect-square bg-surface bg-cover bg-center"
    style={placeholder ? `background-image: url(${placeholder})` : undefined}
  >
    {#if asset.file_hash && !imgError}
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
