<script lang="ts">
  // SpriteView — sprite-atlas viewer with manual grid slicing + a
  // simple Canvas 2D animator. This first cut handles the no-metadata
  // PNG case: the user uploads a sheet, the panel exposes cell
  // width / height / columns / rows, and the viewer plays the slice
  // sequence on a loop. Companion-JSON-driven metadata (TexturePacker
  // Hash / Phaser / Aseprite export) lands in the next commit; the
  // architecture leaves room for it via the `frames` state below
  // (which the metadata loader will populate instead of the
  // manual-grid derivation).
  //
  // Pixel-perfect rendering: image-rendering: pixelated CSS on the
  // <canvas> + integer-step zoom so retro sprites stay crisp. Bigger
  // zoom presets land in commit #2 with the rest of the panel tools.
  //
  // The component skips the AssetViewer's outer pan/zoom transform
  // wrapper (the AssetViewer special-cases `kind === 'sprite'`),
  // because sprites own their own zoom semantics.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import { defaultController } from './controller';

  type Asset = import('./controller').ViewAsset;

  let { asset, controller = $bindable<ViewController>() }: {
    asset: Asset;
    controller: ViewController;
  } = $props();

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);

  // Loaded sprite image. Null until onload fires.
  let img: HTMLImageElement | null = $state(null);
  let imgW = $state(0);
  let imgH = $state(0);
  let loadError = $state<string | null>(null);

  // Manual grid params. Defaults assume a single-cell sheet so the
  // first paint shows the whole sheet as one frame; the user adjusts
  // from there. (A reasonable starting cell size auto-derive could
  // be `gcd(w, h) of common sprite-strip dimensions` but we'd guess
  // wrong on most sheets — better to land at 1×1 and let the user
  // pick.) These will be controllable from the right pane in
  // commit #2.
  let cellW = $state(0);
  let cellH = $state(0);
  let cols = $derived(cellW > 0 ? Math.max(1, Math.floor(imgW / cellW)) : 1);
  let rows = $derived(cellH > 0 ? Math.max(1, Math.floor(imgH / cellH)) : 1);
  let frameCount = $derived(cols * rows);

  // Playback.
  let playing = $state(true);
  let currentFrame = $state(0);
  let frameMs = $state(100);  // 10 fps default — common pixel-art tempo
  let lastTick = 0;
  let rafHandle = 0;

  // Render state.
  let canvasEl: HTMLCanvasElement | undefined = $state();
  let zoom = $state(2); // 2× nearest-neighbor by default — most pixel sprites are tiny
  let bg = $state<'checker' | 'transparent' | 'solid'>('checker');
  let bgSolid = $state('#1a1a1a');

  // Frame rect for the active frame. Manual-grid mode walks the
  // sheet in reading order (left→right, top→bottom). Metadata mode
  // (next commit) will replace this with the parsed frame list.
  function frameRect(idx: number): { sx: number; sy: number; sw: number; sh: number } {
    if (cellW <= 0 || cellH <= 0) {
      return { sx: 0, sy: 0, sw: imgW, sh: imgH };
    }
    const c = idx % cols;
    const r = Math.floor(idx / cols);
    return { sx: c * cellW, sy: r * cellH, sw: cellW, sh: cellH };
  }

  function render() {
    if (!canvasEl || !img) return;
    const f = frameRect(currentFrame);
    const dw = f.sw * zoom;
    const dh = f.sh * zoom;
    canvasEl.width = Math.max(1, dw);
    canvasEl.height = Math.max(1, dh);
    const ctx = canvasEl.getContext('2d');
    if (!ctx) return;
    // Disable smoothing so retro sprites stay crisp at any zoom.
    ctx.imageSmoothingEnabled = false;
    ctx.clearRect(0, 0, canvasEl.width, canvasEl.height);
    ctx.drawImage(img, f.sx, f.sy, f.sw, f.sh, 0, 0, dw, dh);
  }

  // RAF loop — bumps currentFrame based on wall-clock time so the
  // tempo stays stable even if the browser deprioritises us (background
  // tab). Stops when playing flips off; reschedules when it flips on.
  function tick(t: number) {
    if (!playing) { rafHandle = 0; return; }
    if (lastTick === 0) lastTick = t;
    const dt = t - lastTick;
    if (dt >= frameMs && frameCount > 1) {
      currentFrame = (currentFrame + 1) % frameCount;
      lastTick = t;
      render();
    }
    rafHandle = requestAnimationFrame(tick);
  }
  $effect(() => {
    if (playing && rafHandle === 0) {
      lastTick = 0;
      rafHandle = requestAnimationFrame(tick);
    }
  });

  // Re-render whenever the parameters that affect the *current
  // frame's appearance* change. Listed explicitly rather than reading
  // a generic dep so the loop above (which already calls render after
  // currentFrame++) doesn't fire two redundant renders per tick.
  $effect(() => {
    void zoom;
    void cellW;
    void cellH;
    void img;
    render();
  });

  onMount(() => {
    const i = new Image();
    i.onload = () => {
      imgW = i.naturalWidth;
      imgH = i.naturalHeight;
      img = i;
      // Reasonable starting zoom: aim for ~512 px tall canvas so
      // small sprites (32×32) jump to 16× and big sheets (256×256)
      // stay at 2×. Clamped to integer multiples for pixel-perfect.
      const target = 512;
      zoom = Math.max(1, Math.min(16, Math.round(target / Math.max(imgW, imgH))));
      render();
    };
    i.onerror = () => { loadError = 'Failed to load sprite image.'; };
    i.src = fileUrl;
    controller = {
      ...defaultController(),
      kind: 'sprite' as const,
      hudExtra: '',
    };
  });

  onDestroy(() => {
    if (rafHandle) cancelAnimationFrame(rafHandle);
    rafHandle = 0;
  });

  // CSS class for the canvas background. Checker is a tiny SVG data
  // URL repeated; transparent shows the page bg; solid uses bgSolid.
  const canvasBgClass = $derived(
    bg === 'checker' ? 'sprite-checker' :
    bg === 'solid'   ? 'sprite-solid'   :
    'sprite-transparent',
  );
</script>

<div class="flex h-full w-full flex-col items-center justify-center overflow-auto bg-[#0d0e12] p-6 text-white/80">
  {#if loadError}
    <p class="text-sm text-danger">{loadError}</p>
    <a href={fileUrl} download class="mt-2 text-xs text-accent hover:underline">Download original</a>
  {:else if !img}
    <p class="text-sm text-white/40">Loading sprite…</p>
  {:else}
    <!-- Canvas + status row. The canvas's intrinsic size matches
         frame × zoom; the wrapper centres it and scrolls if the
         user zooms past viewport. -->
    <div class="flex flex-1 items-center justify-center self-stretch">
      <canvas
        bind:this={canvasEl}
        class={`max-h-full max-w-full ${canvasBgClass}`}
        style:image-rendering="pixelated"
        style:background-color={bg === 'solid' ? bgSolid : undefined}
      ></canvas>
    </div>
    <!-- Minimal status strip. The full toolset (slicer, playback
         controls, tag picker, companion uploader) lands in commit
         #2 in the AssetViewer right pane. This strip is just enough
         for a first-cut sanity check. -->
    <div class="mt-3 flex shrink-0 items-center gap-4 text-xs text-white/60">
      <span class="font-mono">{imgW}×{imgH}px</span>
      <span class="font-mono">zoom {zoom}×</span>
      {#if frameCount > 1}
        <span class="font-mono">frame {currentFrame + 1} / {frameCount}</span>
        <button
          type="button"
          onclick={() => (playing = !playing)}
          class="rounded border border-white/15 px-2 py-0.5 hover:border-white/40 hover:text-white"
        >{playing ? 'Pause' : 'Play'}</button>
      {:else}
        <span class="text-white/30">set cell size in the side panel to slice frames</span>
      {/if}
    </div>
  {/if}
</div>

<style>
  /* 8×8 checkerboard via a CSS gradient — no extra HTTP. Reads as
     "transparency" in every image editor on the planet. */
  :global(.sprite-checker) {
    background-image:
      linear-gradient(45deg, rgba(255,255,255,0.06) 25%, transparent 25%),
      linear-gradient(-45deg, rgba(255,255,255,0.06) 25%, transparent 25%),
      linear-gradient(45deg, transparent 75%, rgba(255,255,255,0.06) 75%),
      linear-gradient(-45deg, transparent 75%, rgba(255,255,255,0.06) 75%);
    background-size: 16px 16px;
    background-position: 0 0, 0 8px, 8px -8px, -8px 0;
  }
</style>
