<script lang="ts">
  // SpriteCanvas — the view-body half of the sprite viewer. Owns
  // the <canvas>, the RAF playback loop, and the grid/frame-box
  // overlay; reads / writes everything else through the session.
  // The right-pane controls live in SpriteToolPanel.svelte and
  // share the same session instance.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import { defaultController } from './controller';
  import type { SpriteSessionInstance } from '$lib/sprite/session.svelte';

  type Asset = import('./controller').ViewAsset;

  let { asset, session = $bindable<SpriteSessionInstance>(), controller = $bindable<ViewController>() }: {
    asset: Asset;
    session: SpriteSessionInstance;
    controller: ViewController;
  } = $props();

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);

  let loadError = $state<string | null>(null);
  let canvasEl: HTMLCanvasElement | undefined = $state();
  let stripEl: HTMLDivElement | undefined = $state();

  // Svelte action that paints one frame thumbnail into a per-tile
  // <canvas>. Re-runs when the frame rect or scale changes so the
  // strip stays in sync with the slicer.
  function thumbCanvas(node: HTMLCanvasElement, params: { frame: { sx: number; sy: number; sw: number; sh: number }; scale: number }) {
    function paint() {
      if (!session.img) return;
      const dw = params.frame.sw * params.scale;
      const dh = params.frame.sh * params.scale;
      node.width = Math.max(1, dw);
      node.height = Math.max(1, dh);
      const ctx = node.getContext('2d');
      if (!ctx) return;
      ctx.imageSmoothingEnabled = false;
      ctx.clearRect(0, 0, node.width, node.height);
      ctx.drawImage(session.img, params.frame.sx, params.frame.sy, params.frame.sw, params.frame.sh, 0, 0, dw, dh);
    }
    paint();
    return {
      update(next: { frame: { sx: number; sy: number; sw: number; sh: number }; scale: number }) {
        params = next;
        paint();
      },
    };
  }

  // ── Derived counts (mirror of session.stepFrame's math) ──────
  const gridCols = $derived(
    session.cellW > 0
      ? Math.max(1, Math.floor((session.imgW - session.originX + session.padX) / (session.cellW + session.padX)))
      : 1,
  );
  const gridRows = $derived(
    session.cellH > 0
      ? Math.max(1, Math.floor((session.imgH - session.originY + session.padY) / (session.cellH + session.padY)))
      : 1,
  );
  const playRange = $derived.by(() => {
    const frames = session.metadataFrames;
    if (frames && session.activeTag) {
      const t = session.metadataTags.find((x) => x.name === session.activeTag);
      if (t) return { from: t.from, to: t.to };
    }
    if (frames) return { from: 0, to: Math.max(0, frames.length - 1) };
    const total = gridCols * gridRows;
    const from = Math.max(0, Math.min(total - 1, session.rangeStart));
    const to = Math.max(from, Math.min(total - 1, session.rangeEnd ?? total - 1));
    return { from, to };
  });
  const frameCount = $derived(Math.max(1, playRange.to - playRange.from + 1));

  function frameRect(relIdx: number): { sx: number; sy: number; sw: number; sh: number } {
    const idx = playRange.from + relIdx;
    const frames = session.metadataFrames;
    if (frames && frames.length > 0) {
      const f = frames[Math.max(0, Math.min(frames.length - 1, idx))];
      return { sx: f.sx, sy: f.sy, sw: f.sw, sh: f.sh };
    }
    if (session.cellW <= 0 || session.cellH <= 0) {
      return { sx: 0, sy: 0, sw: session.imgW, sh: session.imgH };
    }
    const c = idx % gridCols;
    const r = Math.floor(idx / gridCols);
    return {
      sx: session.originX + c * (session.cellW + session.padX),
      sy: session.originY + r * (session.cellH + session.padY),
      sw: session.cellW,
      sh: session.cellH,
    };
  }

  function render() {
    if (!canvasEl || !session.img) return;
    const f = frameRect(session.currentFrame);
    const dw = f.sw * session.zoom;
    const dh = f.sh * session.zoom;
    canvasEl.width = Math.max(1, dw);
    canvasEl.height = Math.max(1, dh);
    const ctx = canvasEl.getContext('2d');
    if (!ctx) return;
    ctx.imageSmoothingEnabled = session.smoothing;
    ctx.clearRect(0, 0, canvasEl.width, canvasEl.height);
    ctx.drawImage(session.img, f.sx, f.sy, f.sw, f.sh, 0, 0, dw, dh);
  }

  // RAF tick — drives playback. Reads frameMs derived from
  // session.fps so the user's slider takes effect mid-loop.
  let lastTick = 0;
  let rafHandle = 0;
  const frameMs = $derived(1000 / Math.max(0.1, session.fps));
  function tick(t: number) {
    if (!session.playing) { rafHandle = 0; return; }
    if (lastTick === 0) lastTick = t;
    const dt = t - lastTick;
    if (dt >= frameMs && frameCount > 1) {
      session.stepFrame();
      lastTick = t;
    }
    rafHandle = requestAnimationFrame(tick);
  }
  $effect(() => {
    if (session.playing && rafHandle === 0) {
      lastTick = 0;
      rafHandle = requestAnimationFrame(tick);
    }
  });

  // Re-render on any visual-affecting field change. Listed
  // explicitly so the dep graph is obvious.
  $effect(() => {
    void session.zoom; void session.cellW; void session.cellH;
    void session.padX; void session.padY; void session.originX;
    void session.originY; void session.img; void session.smoothing;
    void session.currentFrame; void session.metadataFrames;
    void session.activeTag;
    render();
  });

  // Clamp currentFrame when the frame count shrinks underneath.
  $effect(() => {
    if (session.currentFrame >= frameCount) session.currentFrame = 0;
  });

  // Auto-scroll the timeline strip so the active frame stays in
  // view during playback. Without this, a long animation scrolls
  // off the right edge and the user can't see which frame is
  // playing without manually scrolling the strip.
  $effect(() => {
    void session.currentFrame;
    if (!stripEl) return;
    const tile = stripEl.querySelector(`[data-frame="${session.currentFrame}"]`) as HTMLElement | null;
    if (!tile) return;
    const tileLeft = tile.offsetLeft;
    const tileRight = tileLeft + tile.offsetWidth;
    const viewLeft = stripEl.scrollLeft;
    const viewRight = viewLeft + stripEl.clientWidth;
    if (tileLeft < viewLeft) {
      stripEl.scrollTo({ left: tileLeft - 16, behavior: 'smooth' });
    } else if (tileRight > viewRight) {
      stripEl.scrollTo({ left: tileRight - stripEl.clientWidth + 16, behavior: 'smooth' });
    }
  });

  // Sprite-scoped hotkeys: Space toggles play, `,`/`.` step frames.
  // Mounted at document level so they fire regardless of focus, but
  // bail when typing in an input/textarea so panel fields keep
  // working. Arrow keys are deliberately NOT bound here — the
  // surrounding AssetPlaylist owns ←/→ for asset navigation; the
  // sprite's `,`/`.` aliases give frame-by-frame access without
  // hijacking it.
  function onSpriteKey(e: KeyboardEvent) {
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === ' ') {
      e.preventDefault();
      session.playing = !session.playing;
    } else if (e.key === ',') {
      e.preventDefault();
      // Step backward by computing the new frame within playRange.
      const len = frameCount;
      if (len > 1) session.currentFrame = (session.currentFrame - 1 + len) % len;
      session.playing = false;
    } else if (e.key === '.') {
      e.preventDefault();
      session.stepFrame();
      session.playing = false;
    }
  }

  onMount(() => {
    document.addEventListener('keydown', onSpriteKey);
    const i = new Image();
    i.onload = () => {
      session.imgW = i.naturalWidth;
      session.imgH = i.naturalHeight;
      session.img = i;
      // Reasonable starting zoom: aim for ~512 px tall canvas.
      const target = 512;
      session.zoom = Math.max(1, Math.min(16, Math.round(target / Math.max(session.imgW, session.imgH))));
      // Auto-pick a cell size when the slicer is still untouched.
      // 9-col × 6-row pixel sheets typically slice cleanly at 32 or
      // 16 px; the heuristic tries common sizes and stops at the
      // largest that divides both dims cleanly. Users can override.
      if (session.cellW === 0 && session.cellH === 0 && !session.metadataFrames) {
        const guess = session.guessCellSize(session.imgW, session.imgH);
        session.cellW = guess.w;
        session.cellH = guess.h;
      }
      // Image-info / palette analysis is cheap (single pass over
      // the pixel buffer) — run it once on load so the panel can
      // show the analyzer stats without an explicit user click.
      session.runAnalyze();
      render();
    };
    i.onerror = () => { loadError = 'Failed to load sprite image.'; };
    i.src = fileUrl;
    controller = {
      ...defaultController(),
      kind: 'sprite' as const,
      hudExtra: '',
    };
    void session.loadCompanions();
  });

  onDestroy(() => {
    if (rafHandle) cancelAnimationFrame(rafHandle);
    rafHandle = 0;
    document.removeEventListener('keydown', onSpriteKey);
  });

  const canvasBgClass = $derived(
    session.bg === 'checker' ? 'sprite-checker' :
    session.bg === 'solid'   ? 'sprite-solid'   :
    'sprite-transparent',
  );
</script>

<div class="flex h-full w-full flex-col overflow-auto bg-[#0d0e12] p-6 text-white/80">
  {#if loadError}
    <div class="m-auto text-center">
      <p class="text-sm text-danger">{loadError}</p>
      <a href={fileUrl} download class="mt-2 inline-block text-xs text-accent hover:underline">Download original</a>
    </div>
  {:else if !session.img}
    <p class="m-auto text-sm text-white/40">Loading sprite…</p>
  {:else}
    <div class="flex flex-1 items-center justify-center">
      <div
        class="relative inline-block {canvasBgClass}"
        style:background-color={session.bg === 'solid' ? session.bgSolid : undefined}
      >
        <!-- Main canvas always shows the current frame. The
             full-sheet-with-grid preview moved to the tool panel
             (see SpriteToolPanel's Slicer section), so the user
             never loses the playback view to look at slicing. -->
        <canvas
          bind:this={canvasEl}
          class="block"
          style:image-rendering={session.smoothing ? 'auto' : 'pixelated'}
        ></canvas>
      </div>
    </div>
    <div class="mt-3 flex shrink-0 items-center gap-4 text-xs text-white/60">
      <span class="font-mono">{session.imgW}×{session.imgH}px</span>
      <span class="font-mono">zoom {session.zoom}×</span>
      {#if frameCount > 1}
        <span class="font-mono">frame {session.currentFrame + 1} / {frameCount}</span>
      {:else}
        <span class="text-white/30">single frame — set cell size in the side panel to slice</span>
      {/if}
    </div>

    <!-- Frame timeline strip — Aseprite-style horizontal thumbnail
         row at the bottom of the canvas area. Each tile is the
         frame's source rect drawn at a fixed thumbnail size.
         Click → jumps to the frame + pauses. Active frame gets an
         accent border. Auto-scrolls so the active tile stays in
         view during playback. -->
    {#if frameCount > 1}
      <div
        bind:this={stripEl}
        class="mt-2 flex shrink-0 items-center gap-1 overflow-x-auto border-t border-white/10 pt-2"
        style:scrollbar-width="thin"
      >
        {#each Array(frameCount) as _, i (i)}
          {@const f = frameRect(i)}
          {@const thumbBudget = 56}
          {@const s = Math.max(1, Math.floor(Math.min(thumbBudget / f.sw, thumbBudget / f.sh)))}
          {@const dw = f.sw * s}
          {@const dh = f.sh * s}
          <button
            type="button"
            data-frame={i}
            class={`group relative flex shrink-0 flex-col items-center gap-0.5 rounded border ${i === session.currentFrame ? 'border-accent bg-accent/15' : 'border-white/15 hover:border-white/40'}`}
            onclick={() => { session.currentFrame = i; session.playing = false; }}
            title={`Frame ${i + 1}`}
          >
            <div class="sprite-checker p-0.5" style:width={`${thumbBudget + 4}px`} style:height={`${thumbBudget + 4}px`}>
              <div class="flex h-full w-full items-center justify-center">
                <canvas
                  width={dw}
                  height={dh}
                  style:image-rendering="pixelated"
                  use:thumbCanvas={{ frame: f, scale: s }}
                ></canvas>
              </div>
            </div>
            <span class="px-1 pb-0.5 font-mono text-[9px] leading-none text-white/50">{i + 1}</span>
          </button>
        {/each}
      </div>
    {/if}
  {/if}
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
