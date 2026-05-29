<script lang="ts">
  // Universal asset viewer — one shell for image / video / PDF /
  // audio / 3D / sequence. The shell owns chrome (HUD, scrubber,
  // transport bar, fullscreen, hotkeys, jump-to-frame, pan + zoom)
  // and delegates the actual surface to a per-kind <ViewBody>
  // component that writes into a shared ViewController.
  //
  // Why this shape: when annotations (1.18.B-6) and presentation
  // rooms (1.18.B-5) land, they wire to the shell ONCE; every kind
  // of asset gets them for free.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import { defaultController, kindForExtension } from './controller';
  import ImageView from './ImageView.svelte';
  import VideoView from './VideoView.svelte';
  import PlaceholderView from './PlaceholderView.svelte';

  interface Asset {
    id: string;
    title?: string | null;
    file_extension?: string | null;
  }

  interface Props {
    asset: Asset;
    /** True when this viewer is the focused slide. Hotkeys + autoplay
        only fire for the focused viewer so two carousel slides don't
        fight for the keyboard. */
    active?: boolean;
  }

  let { asset, active = true }: Props = $props();

  const kind = $derived(kindForExtension(asset.file_extension));
  let controller = $state(defaultController());

  // ---- pan + zoom (shell-owned) -----------------------------------------

  let canvasEl: HTMLDivElement | undefined = $state();
  let containerEl: HTMLDivElement | undefined = $state();
  let zoom = $state(1);
  let panX = $state(0);
  let panY = $state(0);
  let dragging = $state(false);

  function onCanvasWheel(e: WheelEvent) {
    // Plain wheel = scrub a frame (when there's a timeline);
    // ctrl/⌘ + wheel = zoom.
    if (!e.ctrlKey && !e.metaKey) {
      if (controller.hasTimeline) {
        e.preventDefault();
        controller.stepFrames(e.deltaY > 0 ? 1 : -1);
      }
      return;
    }
    e.preventDefault();
    const next = Math.max(1, Math.min(20, zoom * (e.deltaY > 0 ? 0.9 : 1.1)));
    if (canvasEl) {
      const rect = canvasEl.getBoundingClientRect();
      const cx = e.clientX - rect.left - rect.width / 2;
      const cy = e.clientY - rect.top - rect.height / 2;
      const factor = next / zoom;
      panX = cx - (cx - panX) * factor;
      panY = cy - (cy - panY) * factor;
    }
    zoom = next;
  }

  function onCanvasMouseDown(e: MouseEvent) {
    if (e.button !== 0) return;
    const startX = e.clientX;
    const startY = e.clientY;
    const initialPanX = panX;
    const initialPanY = panY;
    dragging = false;
    const move = (mv: MouseEvent) => {
      const dx = mv.clientX - startX;
      const dy = mv.clientY - startY;
      if (!dragging && Math.hypot(dx, dy) > 4) dragging = true;
      if (dragging) {
        panX = initialPanX + dx;
        panY = initialPanY + dy;
      }
    };
    const up = () => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
      if (!dragging && controller.hasTimeline) {
        // Treat a non-drag click as a play/pause toggle for media.
        controller.togglePlay();
      }
      setTimeout(() => { dragging = false; }, 0);
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  }

  function resetView() {
    zoom = 1;
    panX = 0;
    panY = 0;
  }

  // ---- fullscreen --------------------------------------------------------

  let isFullscreen = $state(false);
  function toggleFullscreen() {
    if (!containerEl) return;
    if (!document.fullscreenElement) {
      void containerEl.requestFullscreen?.();
    } else {
      void document.exitFullscreen?.();
    }
  }
  function onFullscreenChange() {
    isFullscreen = !!document.fullscreenElement;
  }

  // ---- scrubber (sprite preview) ----------------------------------------

  interface SpriteCue { start: number; end: number; src: string; x: number; y: number; w: number; h: number; }
  let sprites = $state<SpriteCue[]>([]);
  let hoverSprite = $state<SpriteCue | null>(null);
  let hoverPctX = $state(0);
  let hoverTime = $state(0);
  let scrubberHovering = $state(false);
  let scrubberEl: HTMLDivElement | undefined = $state();

  $effect(() => {
    if (controller.spritesVttUrl) {
      void loadSprites(controller.spritesVttUrl, controller.spritesUrl ?? '');
    } else {
      sprites = [];
    }
  });

  async function loadSprites(vttUrl: string, baseHref: string) {
    try {
      const r = await fetch(vttUrl, { credentials: 'include' });
      if (!r.ok) { sprites = []; return; }
      sprites = parseVTTSprites(await r.text(), baseHref);
    } catch {
      sprites = [];
    }
  }

  function parseVTTSprites(vtt: string, baseHref: string): SpriteCue[] {
    const out: SpriteCue[] = [];
    const lines = vtt.split(/\r?\n/);
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      const m = line.match(/(\d+:\d+:\d+\.\d+)\s+-->\s+(\d+:\d+:\d+\.\d+)/);
      if (!m) continue;
      const start = parseVTTTime(m[1]);
      const end = parseVTTTime(m[2]);
      const xy = (lines[i + 1] || '').match(/#xywh=(\d+),(\d+),(\d+),(\d+)/);
      if (!xy) continue;
      out.push({ start, end, src: baseHref, x: +xy[1], y: +xy[2], w: +xy[3], h: +xy[4] });
    }
    return out;
  }
  function parseVTTTime(s: string): number {
    const [h, m, rest] = s.split(':');
    return +h * 3600 + +m * 60 + parseFloat(rest);
  }

  function clamp(n: number, lo: number, hi: number) { return Math.max(lo, Math.min(hi, n)); }

  function onScrubberMove(e: MouseEvent) {
    if (!scrubberEl) return;
    const rect = scrubberEl.getBoundingClientRect();
    const pct = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    hoverPctX = pct * 100;
    hoverTime = pct * controller.duration;
    hoverSprite = sprites.find((c) => hoverTime >= c.start && hoverTime < c.end) ?? null;
  }
  function onScrubberLeave() { scrubberHovering = false; hoverSprite = null; }
  function onScrubberClick(e: MouseEvent) {
    if (!scrubberEl) return;
    const rect = scrubberEl.getBoundingClientRect();
    const pct = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    controller.seekToFrame(Math.round(pct * controller.totalFrames));
  }

  // ---- loop region ------------------------------------------------------

  let loopIn = $state<number | null>(null);
  let loopOut = $state<number | null>(null);

  function enforceLoop() {
    if (!controller.hasTimeline) return;
    if (loopIn === null || loopOut === null || loopOut <= loopIn) return;
    if (controller.currentFrame > loopOut) controller.seekToFrame(loopIn);
  }
  $effect(enforceLoop);

  // ---- jump-to-frame ----------------------------------------------------

  let goToOpen = $state(false);
  let goToValue = $state('');
  function commitGoTo() {
    const s = goToValue.trim();
    if (!s) { goToOpen = false; return; }
    let frame = NaN;
    const tcM = s.match(/^(?:(\d+):)?(?:(\d+):)?(\d+)[:.,](\d+)$/);
    const secM = s.match(/^(\d+(?:\.\d+)?)\s*s?$/);
    if (/^\d+$/.test(s)) {
      frame = parseInt(s, 10);
    } else if (tcM) {
      const h = parseInt(tcM[1] || '0', 10);
      const m = parseInt(tcM[2] || '0', 10);
      const sec = parseInt(tcM[3], 10);
      const f = parseInt(tcM[4], 10);
      const fpsR = Math.max(1, Math.round(controller.fps));
      frame = ((h * 3600 + m * 60 + sec) * fpsR) + f;
    } else if (secM) {
      const fpsR = Math.max(1, controller.fps || 1);
      frame = Math.round(parseFloat(secM[1]) * fpsR);
    }
    if (Number.isFinite(frame)) {
      controller.seekToFrame(clamp(Math.round(frame), 0, controller.totalFrames));
    }
    goToValue = '';
    goToOpen = false;
  }

  // ---- hotkeys ----------------------------------------------------------

  function handleKey(e: KeyboardEvent) {
    if (!active) return;
    if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    const k = e.key.toLowerCase();
    switch (k) {
      case ' ': if (controller.hasTimeline) { e.preventDefault(); controller.togglePlay(); } break;
      case 'k': if (controller.hasTimeline) { e.preventDefault(); controller.pause(); } break;
      case 'l': if (controller.hasTimeline) { e.preventDefault(); controller.play(); } break;
      case 'j': if (controller.hasTimeline) { e.preventDefault(); controller.stepFrames(-1); } break;
      case ',':
      case 'arrowleft': if (controller.hasTimeline) { e.preventDefault(); controller.stepFrames(e.shiftKey ? -10 : -1); } break;
      case '.':
      case 'arrowright': if (controller.hasTimeline) { e.preventDefault(); controller.stepFrames(e.shiftKey ? 10 : 1); } break;
      case 'i': if (controller.hasTimeline) { e.preventDefault(); loopIn = controller.currentFrame; } break;
      case 'o': if (controller.hasTimeline) { e.preventDefault(); loopOut = controller.currentFrame; } break;
      case 'backspace': e.preventDefault(); loopIn = null; loopOut = null; break;
      case '1': if (controller.hasTimeline) controller.setRate(0.25); break;
      case '2': if (controller.hasTimeline) controller.setRate(0.5); break;
      case '3': if (controller.hasTimeline) controller.setRate(1); break;
      case '4': if (controller.hasTimeline) controller.setRate(2); break;
      case '5': if (controller.hasTimeline) controller.setRate(4); break;
      case 'f': e.preventDefault(); toggleFullscreen(); break;
      case 'r': e.preventDefault(); resetView(); break;
      case 'g': if (controller.hasTimeline) { e.preventDefault(); goToOpen = true; } break;
    }
  }

  onMount(() => {
    document.addEventListener('keydown', handleKey);
    document.addEventListener('fullscreenchange', onFullscreenChange);
  });
  onDestroy(() => {
    document.removeEventListener('keydown', handleKey);
    document.removeEventListener('fullscreenchange', onFullscreenChange);
  });

  // Derived UI values
  const playheadPct = $derived(controller.totalFrames > 0 ? (controller.currentFrame / controller.totalFrames) * 100 : 0);
  const loopInPct = $derived(controller.totalFrames > 0 && loopIn !== null ? (loopIn / controller.totalFrames) * 100 : 0);
  const loopOutPct = $derived(controller.totalFrames > 0 && loopOut !== null ? (loopOut / controller.totalFrames) * 100 : 0);
  const playRateChips = [0.25, 0.5, 1, 2, 4];
</script>

<div bind:this={containerEl} class="flex h-full w-full flex-col bg-black text-white">
  <!-- Canvas (pan + zoom transform wraps the view body) -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={canvasEl}
    class="relative flex-1 overflow-hidden bg-black"
    class:cursor-grabbing={dragging}
    class:cursor-grab={!dragging && zoom > 1}
    onwheel={onCanvasWheel}
    onmousedown={onCanvasMouseDown}
  >
    <div
      class="absolute inset-0 flex items-center justify-center"
      style="transform: translate({panX}px, {panY}px) scale({zoom}); transform-origin: center center;"
    >
      {#if kind === 'video'}
        <VideoView {asset} bind:controller />
      {:else if kind === 'image'}
        <ImageView {asset} bind:controller />
      {:else}
        <PlaceholderView {asset} bind:controller />
      {/if}
    </div>

    <!-- HUD: anchor display + extra metadata -->
    <div class="pointer-events-none absolute right-3 top-3 rounded bg-black/70 px-2 py-1 font-mono text-xs">
      {#if controller.hasTimeline}
        {controller.formatAnchor(controller.currentFrame)} · f{controller.currentFrame}{controller.totalFrames > 0 ? `/${controller.totalFrames}` : ''}
      {/if}
      {#if controller.hudExtra}
        {controller.hasTimeline ? ' · ' : ''}{controller.hudExtra}
      {/if}
      {#if zoom !== 1}
        {(controller.hasTimeline || controller.hudExtra) ? ' · ' : ''}{Math.round(zoom * 100)}%
      {/if}
    </div>

    <!-- Top-right buttons -->
    <div class="absolute right-3 top-12 flex flex-col gap-2">
      {#if controller.hasTimeline}
        <button type="button" onclick={() => (goToOpen = !goToOpen)} class="rounded bg-black/70 p-1.5 text-xs hover:bg-black/90" title="Jump to frame (G)" aria-label="Jump to frame">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
        </button>
      {/if}
      <button type="button" onclick={resetView} class="rounded bg-black/70 p-1.5 text-xs hover:bg-black/90" class:opacity-40={zoom === 1 && panX === 0 && panY === 0} title="Reset view (R)" aria-label="Reset view">
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="1 4 1 10 7 10" /><path d="M3.51 15a9 9 0 1 0 2.13-9.36L1 10" /></svg>
      </button>
      <button type="button" onclick={toggleFullscreen} class="rounded bg-black/70 p-1.5 text-xs hover:bg-black/90" title={isFullscreen ? 'Exit fullscreen (F)' : 'Fullscreen (F)'} aria-label="Fullscreen">
        {#if isFullscreen}
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 3v3a2 2 0 0 1-2 2H3" /><path d="M21 8h-3a2 2 0 0 1-2-2V3" /><path d="M3 16h3a2 2 0 0 1 2 2v3" /><path d="M16 21v-3a2 2 0 0 1 2-2h3" /></svg>
        {:else}
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7V3h4" /><path d="M21 7V3h-4" /><path d="M3 17v4h4" /><path d="M21 17v4h-4" /></svg>
        {/if}
      </button>
    </div>

    <!-- Jump-to-frame input -->
    {#if goToOpen}
      <div class="absolute left-1/2 top-12 -translate-x-1/2 rounded bg-black/85 p-2 text-xs shadow-xl">
        <form onsubmit={(e) => { e.preventDefault(); commitGoTo(); }} class="flex items-center gap-2">
          <span class="text-zinc-400">Go to</span>
          <input
            type="text"
            bind:value={goToValue}
            placeholder="frame, mm:ss, or 5.2s"
            class="w-44 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 font-mono text-xs text-white focus:border-accent focus:outline-none"
            autofocus
            onkeydown={(e) => { if (e.key === 'Escape') { goToOpen = false; goToValue = ''; } }}
          />
          <button type="submit" class="rounded bg-accent px-2 py-1 text-xs font-medium text-white">Go</button>
        </form>
      </div>
    {/if}
  </div>

  <!-- Transport rail (only when the body has a timeline) -->
  {#if controller.hasTimeline}
    <div class="relative h-3 bg-zinc-900">
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <div
        bind:this={scrubberEl}
        class="absolute inset-0 cursor-crosshair"
        onmouseenter={() => (scrubberHovering = true)}
        onmousemove={onScrubberMove}
        onmouseleave={onScrubberLeave}
        onclick={onScrubberClick}
        role="slider"
        aria-valuenow={controller.currentFrame}
        aria-valuemin={0}
        aria-valuemax={controller.totalFrames || 0}
        aria-label="Scrubber"
        tabindex="0"
      >
        <div class="absolute inset-y-0 left-0 bg-accent/60" style="width: {playheadPct}%"></div>
        {#if loopIn !== null && loopOut !== null && loopOut > loopIn}
          <div class="absolute inset-y-0 bg-yellow-500/30" style="left: {loopInPct}%; width: {loopOutPct - loopInPct}%"></div>
        {/if}
        <div class="absolute inset-y-0 w-px bg-white" style="left: {playheadPct}%"></div>
      </div>
      {#if scrubberHovering && hoverSprite}
        <div class="pointer-events-none absolute bottom-4 z-10 -translate-x-1/2 rounded border border-zinc-700 bg-black p-1 shadow-xl" style="left: {hoverPctX}%">
          <div class="bg-zinc-950" style="width: {hoverSprite.w}px; height: {hoverSprite.h}px; background-image: url({hoverSprite.src}); background-position: -{hoverSprite.x}px -{hoverSprite.y}px;"></div>
          <div class="mt-1 text-center font-mono text-[10px]">{controller.formatAnchor(Math.round(hoverTime * controller.fps))}</div>
        </div>
      {/if}
    </div>
    <div class="flex items-center gap-3 border-t border-zinc-800 bg-zinc-950 px-3 py-2 text-sm">
      <button type="button" onclick={() => controller.stepFrames(-10)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="−10 (Shift+←)">⏮</button>
      <button type="button" onclick={() => controller.stepFrames(-1)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="Step back (,)">◀|</button>
      <button type="button" onclick={() => controller.togglePlay()} class="rounded bg-zinc-800 px-3 py-1 font-medium hover:bg-zinc-700" title="Play/Pause (K)">
        {controller.playing ? '⏸' : '▶'}
      </button>
      <button type="button" onclick={() => controller.stepFrames(1)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="Step fwd (.)">|▶</button>
      <button type="button" onclick={() => controller.stepFrames(10)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="+10 (Shift+→)">⏭</button>
      <span class="mx-2 h-4 w-px bg-zinc-800"></span>
      {#each playRateChips as r}
        <button type="button" onclick={() => controller.setRate(r)} class="px-1.5 py-0.5 text-xs hover:bg-zinc-800" class:bg-zinc-800={controller.rate === r} title="Speed {r}×">{r}×</button>
      {/each}
      <span class="mx-2 h-4 w-px bg-zinc-800"></span>
      <button type="button" onclick={() => (loopIn = controller.currentFrame)} class="px-1.5 py-0.5 text-xs hover:bg-zinc-800" title="Mark loop in (I)">
        Loop in {loopIn !== null ? `(f${loopIn})` : ''}
      </button>
      <button type="button" onclick={() => (loopOut = controller.currentFrame)} class="px-1.5 py-0.5 text-xs hover:bg-zinc-800" title="Mark loop out (O)">
        Loop out {loopOut !== null ? `(f${loopOut})` : ''}
      </button>
      {#if loopIn !== null || loopOut !== null}
        <button type="button" onclick={() => { loopIn = null; loopOut = null; }} class="px-1.5 py-0.5 text-xs text-zinc-400 hover:text-white" title="Clear loop (⌫)">clear</button>
      {/if}
      <span class="ml-auto font-mono text-xs text-zinc-400">
        JKL · ⇧← → · I/O loop · 1-5 speed · G goto · F fullscreen · ⌘wheel zoom
      </span>
    </div>
  {/if}
</div>
