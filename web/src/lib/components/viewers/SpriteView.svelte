<script lang="ts">
  // SpriteView — sprite-atlas viewer with manual grid slicing,
  // pixel-perfect playback, and a right-pane tool strip.
  //
  // Layout: canvas area on the left (centred, scrollable when zoomed
  // past viewport), tool panel on the right (~320 px, scrollable
  // independently). The tools are baked into the view body rather
  // than threaded through AssetViewer's metadataSlot so the panel
  // works regardless of host (post-modal, standalone viewer, etc.).
  //
  // Manual slicing is the first cut. Companion-JSON metadata
  // (TexturePacker Hash / Phaser / Aseprite export) lands in the
  // follow-on commit and populates the same `frames` derivation by
  // a different path; the playback engine doesn't care.
  //
  // Pixel-perfect: image-rendering: pixelated CSS + ctx
  // imageSmoothingEnabled=false + integer-step zoom.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import { defaultController } from './controller';

  type Asset = import('./controller').ViewAsset;

  let { asset, controller = $bindable<ViewController>() }: {
    asset: Asset;
    controller: ViewController;
  } = $props();

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);

  // ── Image load ─────────────────────────────────────────────────
  let img: HTMLImageElement | null = $state(null);
  let imgW = $state(0);
  let imgH = $state(0);
  let loadError = $state<string | null>(null);

  // ── Slicer (manual grid) ───────────────────────────────────────
  // cellW / cellH = 0 means "treat the whole sheet as one frame"
  // (the default until the user picks a grid). cols / rows are
  // derived from the cell size and the inner content rect (image
  // minus origin offset, divided by cell + padding).
  let cellW = $state(0);
  let cellH = $state(0);
  let padX = $state(0);
  let padY = $state(0);
  let originX = $state(0);
  let originY = $state(0);
  // Optional explicit frame count override — sheets with empty
  // trailing cells (e.g. 24 frames on a 5×5 grid leaves one empty).
  let frameCountOverride = $state<number | null>(null);
  let showGrid = $state(false);

  const cols = $derived(
    cellW > 0
      ? Math.max(1, Math.floor((imgW - originX + padX) / (cellW + padX)))
      : 1,
  );
  const rows = $derived(
    cellH > 0
      ? Math.max(1, Math.floor((imgH - originY + padY) / (cellH + padY)))
      : 1,
  );
  const frameCount = $derived(
    Math.max(1, frameCountOverride ?? cols * rows),
  );

  // ── Playback ──────────────────────────────────────────────────
  let playing = $state(true);
  let currentFrame = $state(0);
  let fps = $state(10);
  type LoopMode = 'forward' | 'pingpong';
  let loopMode = $state<LoopMode>('forward');
  // Pingpong direction: 1 = forward, -1 = backward.
  let pingDir = 1;

  let lastTick = 0;
  let rafHandle = 0;
  const frameMs = $derived(1000 / Math.max(0.1, fps));

  function stepFrame() {
    if (frameCount <= 1) return;
    if (loopMode === 'pingpong') {
      let next = currentFrame + pingDir;
      if (next >= frameCount) {
        pingDir = -1;
        next = Math.max(0, frameCount - 2);
      } else if (next < 0) {
        pingDir = 1;
        next = Math.min(frameCount - 1, 1);
      }
      currentFrame = next;
    } else {
      currentFrame = (currentFrame + 1) % frameCount;
    }
  }

  // ── Display ───────────────────────────────────────────────────
  let zoom = $state(2);
  const ZOOM_PRESETS = [1, 2, 4, 8, 16, 24, 32] as const;
  let bg = $state<'checker' | 'transparent' | 'solid'>('checker');
  let bgSolid = $state('#1a1a1a');
  let smoothing = $state(false); // false = pixel-perfect (default)

  // ── Canvas refs + render ──────────────────────────────────────
  let canvasEl: HTMLCanvasElement | undefined = $state();
  let overlayEl: HTMLCanvasElement | undefined = $state();

  function frameRect(idx: number): { sx: number; sy: number; sw: number; sh: number } {
    if (cellW <= 0 || cellH <= 0) {
      return { sx: 0, sy: 0, sw: imgW, sh: imgH };
    }
    const c = idx % cols;
    const r = Math.floor(idx / cols);
    return {
      sx: originX + c * (cellW + padX),
      sy: originY + r * (cellH + padY),
      sw: cellW,
      sh: cellH,
    };
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
    ctx.imageSmoothingEnabled = smoothing;
    ctx.clearRect(0, 0, canvasEl.width, canvasEl.height);
    ctx.drawImage(img, f.sx, f.sy, f.sw, f.sh, 0, 0, dw, dh);
  }

  // Grid overlay — drawn over a separate canvas the size of the
  // full sheet at current zoom. Renders when showGrid is on so
  // the user can verify their slicing parameters before playback.
  function renderOverlay() {
    if (!overlayEl || !img) return;
    overlayEl.width = imgW * zoom;
    overlayEl.height = imgH * zoom;
    const ctx = overlayEl.getContext('2d');
    if (!ctx) return;
    ctx.clearRect(0, 0, overlayEl.width, overlayEl.height);
    if (!showGrid || cellW <= 0 || cellH <= 0) return;
    ctx.strokeStyle = 'rgba(255, 100, 100, 0.7)';
    ctx.lineWidth = 1;
    // Vertical cell boundaries.
    for (let c = 0; c <= cols; c++) {
      const x = (originX + c * (cellW + padX)) * zoom + 0.5;
      ctx.beginPath();
      ctx.moveTo(x, 0);
      ctx.lineTo(x, overlayEl.height);
      ctx.stroke();
    }
    // Horizontal cell boundaries.
    for (let r = 0; r <= rows; r++) {
      const y = (originY + r * (cellH + padY)) * zoom + 0.5;
      ctx.beginPath();
      ctx.moveTo(0, y);
      ctx.lineTo(overlayEl.width, y);
      ctx.stroke();
    }
  }

  // RAF loop ticking the playhead at the configured fps.
  function tick(t: number) {
    if (!playing) { rafHandle = 0; return; }
    if (lastTick === 0) lastTick = t;
    const dt = t - lastTick;
    if (dt >= frameMs && frameCount > 1) {
      stepFrame();
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

  // Repaint on parameter change.
  $effect(() => {
    void zoom; void cellW; void cellH; void padX; void padY;
    void originX; void originY; void img; void smoothing;
    void currentFrame;
    render();
  });
  // Overlay reacts to slicer params + toggle + zoom.
  $effect(() => {
    void zoom; void cellW; void cellH; void padX; void padY;
    void originX; void originY; void img; void showGrid;
    void cols; void rows;
    renderOverlay();
  });

  // Clamp currentFrame when slicer params shrink the frame count.
  $effect(() => {
    if (currentFrame >= frameCount) currentFrame = 0;
  });

  onMount(() => {
    const i = new Image();
    i.onload = () => {
      imgW = i.naturalWidth;
      imgH = i.naturalHeight;
      img = i;
      // Reasonable starting zoom: aim for ~512 px tall canvas so
      // small sprites jump to 16× and big sheets stay at 2×.
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

  const canvasBgClass = $derived(
    bg === 'checker' ? 'sprite-checker' :
    bg === 'solid'   ? 'sprite-solid'   :
    'sprite-transparent',
  );

  // "Fit" zoom — biggest integer multiplier that keeps the sheet
  // inside a viewport-ish budget. Heuristic since we don't measure
  // the actual container; user can override with presets.
  function pickFitZoom() {
    const budget = 640;
    const z = Math.max(1, Math.floor(Math.min(budget / imgW, budget / imgH)));
    zoom = Math.max(1, Math.min(32, z));
  }
</script>

<div class="flex h-full w-full overflow-hidden bg-[#0d0e12] text-white/80">
  <!-- Canvas area -->
  <div class="flex flex-1 flex-col overflow-auto p-6">
    {#if loadError}
      <div class="m-auto text-center">
        <p class="text-sm text-danger">{loadError}</p>
        <a href={fileUrl} download class="mt-2 inline-block text-xs text-accent hover:underline">Download original</a>
      </div>
    {:else if !img}
      <p class="m-auto text-sm text-white/40">Loading sprite…</p>
    {:else}
      <div class="flex flex-1 items-center justify-center">
        <!-- Wrapper that anchors the overlay canvas on top of the
             sprite canvas. The overlay is the full sheet at the
             current zoom; the sprite canvas shows the active frame.
             Both share the same checker / solid background. -->
        <div class="relative inline-block {canvasBgClass}" style:background-color={bg === 'solid' ? bgSolid : undefined}>
          {#if showGrid}
            <!-- When the grid is on, the canvas shows the WHOLE sheet
                 (not just one frame) so the user can see the slicing
                 lines align with the artwork. Playback pauses
                 visually but the playhead keeps moving — toggle the
                 grid off to see the active frame. -->
            <canvas
              bind:this={overlayEl}
              class="block"
              style:image-rendering="pixelated"
            ></canvas>
            <!-- Active-frame highlight box on top of the overlay,
                 positioned absolutely so it tracks currentFrame as
                 the playhead advances. -->
            {@const f = frameRect(currentFrame)}
            <div
              class="pointer-events-none absolute border-2 border-accent"
              style:left={`${f.sx * zoom}px`}
              style:top={`${f.sy * zoom}px`}
              style:width={`${f.sw * zoom}px`}
              style:height={`${f.sh * zoom}px`}
            ></div>
          {:else}
            <canvas
              bind:this={canvasEl}
              class="block"
              style:image-rendering={smoothing ? 'auto' : 'pixelated'}
            ></canvas>
          {/if}
        </div>
      </div>
      <!-- Status strip -->
      <div class="mt-3 flex shrink-0 items-center gap-4 text-xs text-white/60">
        <span class="font-mono">{imgW}×{imgH}px</span>
        <span class="font-mono">zoom {zoom}×</span>
        {#if frameCount > 1}
          <span class="font-mono">frame {currentFrame + 1} / {frameCount}</span>
        {:else}
          <span class="text-white/30">single frame — set cell size to slice</span>
        {/if}
      </div>
    {/if}
  </div>

  <!-- Tool panel -->
  <aside class="flex w-72 shrink-0 flex-col overflow-y-auto border-l border-white/10 bg-[#16181f] text-xs">
    <!-- Display -->
    <section class="border-b border-white/10 p-3">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-white/40">Display</h3>
      <div class="space-y-2">
        <div>
          <label for="sprite-zoom" class="mb-1 block text-white/60">Zoom</label>
          <div class="flex flex-wrap gap-1">
            {#each ZOOM_PRESETS as z (z)}
              <button
                type="button"
                onclick={() => (zoom = z)}
                class={`rounded border px-2 py-0.5 text-[10px] ${zoom === z ? 'border-accent bg-accent/20 text-white' : 'border-white/15 text-white/60 hover:border-white/40 hover:text-white'}`}
              >{z}×</button>
            {/each}
            <button
              type="button"
              onclick={pickFitZoom}
              class="rounded border border-white/15 px-2 py-0.5 text-[10px] text-white/60 hover:border-white/40 hover:text-white"
            >Fit</button>
          </div>
        </div>
        <label class="flex items-center justify-between">
          <span class="text-white/60">Smoothing</span>
          <input type="checkbox" bind:checked={smoothing} class="accent-accent" />
        </label>
        <div>
          <span class="mb-1 block text-white/60">Background</span>
          <div class="flex gap-1">
            {#each [
              { id: 'checker' as const,     label: 'Checker' },
              { id: 'transparent' as const, label: 'None' },
              { id: 'solid' as const,       label: 'Solid' },
            ] as opt (opt.id)}
              <button
                type="button"
                onclick={() => (bg = opt.id)}
                class={`flex-1 rounded border px-2 py-0.5 text-[10px] ${bg === opt.id ? 'border-accent bg-accent/20 text-white' : 'border-white/15 text-white/60 hover:border-white/40 hover:text-white'}`}
              >{opt.label}</button>
            {/each}
          </div>
          {#if bg === 'solid'}
            <input
              type="color"
              bind:value={bgSolid}
              class="mt-1 h-6 w-full cursor-pointer rounded border border-white/15 bg-transparent"
            />
          {/if}
        </div>
      </div>
    </section>

    <!-- Slicer -->
    <section class="border-b border-white/10 p-3">
      <div class="mb-2 flex items-center justify-between">
        <h3 class="text-[10px] font-medium uppercase tracking-wider text-white/40">Slice grid</h3>
        <label class="flex items-center gap-1 text-[10px] text-white/60">
          <input type="checkbox" bind:checked={showGrid} class="accent-accent" />
          Show grid
        </label>
      </div>
      <div class="grid grid-cols-2 gap-2">
        <label>
          <span class="mb-0.5 block text-white/50">Cell W</span>
          <input type="number" bind:value={cellW} min="0" max={imgW} class="w-full rounded border border-white/15 bg-[#0f1117] px-1.5 py-0.5 text-white" />
        </label>
        <label>
          <span class="mb-0.5 block text-white/50">Cell H</span>
          <input type="number" bind:value={cellH} min="0" max={imgH} class="w-full rounded border border-white/15 bg-[#0f1117] px-1.5 py-0.5 text-white" />
        </label>
        <label>
          <span class="mb-0.5 block text-white/50">Origin X</span>
          <input type="number" bind:value={originX} min="0" class="w-full rounded border border-white/15 bg-[#0f1117] px-1.5 py-0.5 text-white" />
        </label>
        <label>
          <span class="mb-0.5 block text-white/50">Origin Y</span>
          <input type="number" bind:value={originY} min="0" class="w-full rounded border border-white/15 bg-[#0f1117] px-1.5 py-0.5 text-white" />
        </label>
        <label>
          <span class="mb-0.5 block text-white/50">Pad X</span>
          <input type="number" bind:value={padX} min="0" class="w-full rounded border border-white/15 bg-[#0f1117] px-1.5 py-0.5 text-white" />
        </label>
        <label>
          <span class="mb-0.5 block text-white/50">Pad Y</span>
          <input type="number" bind:value={padY} min="0" class="w-full rounded border border-white/15 bg-[#0f1117] px-1.5 py-0.5 text-white" />
        </label>
      </div>
      <div class="mt-2 flex items-center justify-between text-[10px] text-white/50">
        <span>Grid <span class="font-mono text-white/70">{cols} × {rows}</span></span>
        <span>Total <span class="font-mono text-white/70">{cols * rows}</span></span>
      </div>
      <label class="mt-2 flex items-center gap-2 text-[10px]">
        <span class="text-white/50">Limit frames</span>
        <input
          type="number"
          min="1"
          max={cols * rows}
          value={frameCountOverride ?? ''}
          oninput={(e) => {
            const v = (e.currentTarget as HTMLInputElement).value;
            frameCountOverride = v === '' ? null : Math.max(1, Math.min(cols * rows, parseInt(v, 10) || 1));
          }}
          placeholder="auto"
          class="w-16 rounded border border-white/15 bg-[#0f1117] px-1.5 py-0.5 text-white"
        />
        {#if frameCountOverride != null}
          <button
            type="button"
            onclick={() => (frameCountOverride = null)}
            class="text-white/40 hover:text-white/80"
            title="Clear override"
          >clear</button>
        {/if}
      </label>
    </section>

    <!-- Playback -->
    <section class="p-3">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-white/40">Playback</h3>
      <div class="space-y-2">
        <div class="flex items-center gap-1">
          <button
            type="button"
            onclick={() => { if (frameCount > 1) currentFrame = (currentFrame - 1 + frameCount) % frameCount; playing = false; }}
            class="rounded border border-white/15 px-2 py-0.5 text-white/60 hover:border-white/40 hover:text-white"
            title="Previous frame"
            aria-label="Previous frame"
          >‹</button>
          <button
            type="button"
            onclick={() => (playing = !playing)}
            disabled={frameCount <= 1}
            class="flex-1 rounded border border-white/15 px-2 py-1 text-white/80 hover:border-white/40 hover:text-white disabled:opacity-40"
          >{playing ? '⏸ Pause' : '▶ Play'}</button>
          <button
            type="button"
            onclick={() => { if (frameCount > 1) currentFrame = (currentFrame + 1) % frameCount; playing = false; }}
            class="rounded border border-white/15 px-2 py-0.5 text-white/60 hover:border-white/40 hover:text-white"
            title="Next frame"
            aria-label="Next frame"
          >›</button>
        </div>
        <label class="block">
          <span class="mb-0.5 flex justify-between text-white/60">
            <span>FPS</span><span class="font-mono text-white/80">{fps.toFixed(1)}</span>
          </span>
          <input type="range" min="0.5" max="60" step="0.5" bind:value={fps} class="w-full accent-accent" />
        </label>
        <div>
          <span class="mb-1 block text-white/60">Loop</span>
          <div class="flex gap-1">
            {#each [
              { id: 'forward' as const,  label: 'Forward' },
              { id: 'pingpong' as const, label: 'Ping-pong' },
            ] as opt (opt.id)}
              <button
                type="button"
                onclick={() => { loopMode = opt.id; pingDir = 1; }}
                class={`flex-1 rounded border px-2 py-0.5 text-[10px] ${loopMode === opt.id ? 'border-accent bg-accent/20 text-white' : 'border-white/15 text-white/60 hover:border-white/40 hover:text-white'}`}
              >{opt.label}</button>
            {/each}
          </div>
        </div>
      </div>
    </section>
  </aside>
</div>

<style>
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
