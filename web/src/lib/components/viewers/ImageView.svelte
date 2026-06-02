<script lang="ts">
  // Static-image body for the AssetViewer.
  //
  // Loads the `hires` variant when available, falls back to /file
  // (original) on 404, then to a tiny "couldn't load" icon. The
  // backend's VariantCache middleware sets long-immutable cache
  // headers on the 200, so navigation back to the same asset is
  // free.
  //
  // Pan + zoom are owned by the AssetViewer shell — this body just
  // renders the <img>.

  import { onMount } from 'svelte';
  import type { ViewController } from './controller';

  type Asset = import('./controller').ViewAsset;

  type TileMode = 'off' | 'tile';

  interface Props {
    asset: Asset;
    controller: ViewController;
    /** Tile mode for texture-style assets. 'off' = single
     *  pan-zoomable image (default). 'tile' = fill the canvas
     *  with the image repeated both directions at native size so
     *  the user can preview seamless tileability. */
    tileMode?: TileMode;
  }

  let { asset, controller = $bindable(), tileMode = 'off' }: Props = $props();

  const hiresUrl = $derived(`/api/v1/assets/${asset.id}/variants/hires`);
  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);

  let imgEl: HTMLImageElement | undefined = $state();
  // Extensions the browser can natively render as <img>. For these
  // a hires-variant 404 is OK to fall back to /file — the browser
  // will sniff and render the source bytes directly. For anything
  // else (eps / psd / epub / mobi / cbz / …) the source bytes
  // aren't browser-renderable, so we skip the file fallback when
  // hires is missing and go straight to the friendly placeholder
  // state with the format name + download link.
  const NATIVE_BROWSER_EXTS = new Set([
    'jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'avif', 'svg',
  ]);
  const sourceExt = $derived(
    (asset.file_extension ?? '').toLowerCase().replace(/^\./, ''),
  );
  const sourceIsRenderable = $derived(NATIVE_BROWSER_EXTS.has(sourceExt));

  let imgSrc = $state('');
  let imgError = $state(false);
  let naturalW = $state(0);
  let naturalH = $state(0);

  $effect(() => {
    imgSrc = hiresUrl;
    imgError = false;
  });

  function onLoad() {
    if (!imgEl) return;
    naturalW = imgEl.naturalWidth;
    naturalH = imgEl.naturalHeight;
    // Tell the shell what to put in the HUD subtitle.
    controller.hudExtra =
      naturalW > 0 && naturalH > 0 ? `${naturalW}×${naturalH}` : '';
  }

  function onError() {
    if (imgSrc !== fileUrl && sourceIsRenderable) {
      // Hires variant missing but the source IS a format the
      // browser can render. Try /file before giving up.
      imgSrc = fileUrl;
      return;
    }
    imgError = true;
  }

  onMount(() => {
    controller.kind = 'image';
    controller.hasTimeline = false;
    controller.totalFrames = 0;
    controller.fps = 0;
    controller.duration = 0;
    controller.playing = false;
    controller.spritesUrl = null;
    controller.spritesVttUrl = null;
    controller.formatAnchor = () => '';
    // Static images have no transport; the shell hides the bar
    // when hasTimeline is false. We still install no-op fns so the
    // shell can call them safely if a hotkey is pressed.
    controller.play = () => {};
    controller.pause = () => {};
    controller.togglePlay = () => {};
    controller.seekToFrame = () => {};
    controller.stepFrames = () => {};
    controller.setRate = () => {};
  });
</script>

{#if imgError}
  <!-- Friendly placeholder when no preview exists. Covers two
       cases: a true raster that failed to load (broken upload,
       backend variant missing) and a non-browser-renderable source
       whose preview pipeline didn't extract a cover (e.g. an
       EPUB / MOBI shipped without one). Always offers the source
       download so the user can open it locally. -->
  <div class="flex h-full w-full flex-col items-center justify-center gap-3 p-8 text-center text-fg-muted">
    <svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" stroke-linecap="round" stroke-linejoin="round">
      <rect x="3" y="3" width="18" height="18" rx="2" />
      <circle cx="9" cy="9" r="2" />
      <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
    </svg>
    <p class="max-w-md text-sm">
      {#if sourceExt && !sourceIsRenderable}
        No preview generated for this <span class="font-mono uppercase">{sourceExt}</span>.
      {:else}
        Preview unavailable.
      {/if}
    </p>
    <a
      href={fileUrl}
      download
      class="rounded-md border border-border bg-surface-elevated px-3 py-1.5 text-xs text-fg hover:border-accent"
    >Download original</a>
  </div>
{:else if tileMode !== 'off'}
  <!-- Hidden loader IMG keeps natural-size + load-error handling
       working in tile mode (otherwise we'd duplicate the variant /
       fallback wiring). The background div is what the user sees. -->
  <img
    bind:this={imgEl}
    src={imgSrc}
    alt=""
    onload={onLoad}
    onerror={onError}
    class="absolute h-0 w-0 opacity-0"
    aria-hidden="true"
  />
  <!-- Tile background — repeat both directions at native pixel
       size so the user can preview seamless tileability. The
       wrapper covers the full canvas area; AssetViewer bypasses
       the pan/zoom transform when tile mode is on (a repeated
       texture inside translate/scale would have edges flying
       around as the user panned). -->
  <div
    class="h-full w-full"
    role="img"
    aria-label={asset.title || ''}
    style:background-image={`url(${imgSrc})`}
    style:background-repeat="repeat"
    style:background-size="auto"
    style:background-position="center"
  ></div>
{:else}
  <img
    bind:this={imgEl}
    src={imgSrc}
    alt={asset.title || ''}
    onload={onLoad}
    onerror={onError}
    draggable="false"
    class="pointer-events-none max-h-full max-w-full select-none object-contain"
  />
{/if}
