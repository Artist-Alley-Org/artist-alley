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

  interface Asset {
    id: string;
    title?: string | null;
    file_extension?: string | null;
  }

  interface Props {
    asset: Asset;
    controller: ViewController;
  }

  let { asset, controller = $bindable() }: Props = $props();

  const hiresUrl = $derived(`/api/v1/assets/${asset.id}/variants/hires`);
  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);

  let imgEl: HTMLImageElement | undefined = $state();
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
    if (imgSrc !== fileUrl) {
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
  <div class="flex h-full w-full items-center justify-center text-fg-muted">
    <svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" stroke-linecap="round" stroke-linejoin="round">
      <rect x="3" y="3" width="18" height="18" rx="2" />
      <circle cx="9" cy="9" r="2" />
      <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
    </svg>
  </div>
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
