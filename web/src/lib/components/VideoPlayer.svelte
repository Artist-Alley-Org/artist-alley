<script lang="ts">
  // Frame-accurate video player for animator review.
  //
  // Targets the "SyncSketch / Keyframe replacement" use case: artists
  // need to step every frame, leave precise comments, and trust what
  // they see. The browser's native <video> + the HLS adaptive ladder
  // produced by preview.video does the heavy lifting; this component
  // adds the editorial UX on top.
  //
  // Foundation in 1.18.B-1:
  //   - HLS playback via hls.js (Safari uses native HLS)
  //   - Frame-accurate counter via requestVideoFrameCallback
  //   - J/K/L transport, ←/→ for ±1 frame, ,/. for ±1 frame
  //   - 0.25 / 0.5 / 1 / 2× speed
  //   - Loop region (set IN / OUT, loop between)
  //   - Sprite-sheet scrub preview (the VTT shipped by the worker)
  //
  // Annotations + drawing land in 1.18.B-2.
  // Real-time presence + shared playhead in 1.18.B-3.

  import { onMount, onDestroy } from 'svelte';

  interface Props {
    assetId: string;
    fps?: number; // poster + ffprobe report; we default 24 if unknown
    poster?: string;
  }

  let { assetId, fps: fpsProp = 24, poster }: Props = $props();

  const masterUrl = $derived(`/api/v1/assets/${assetId}/variants/hls/master.m3u8`);
  const fallbackUrl = $derived(`/api/v1/assets/${assetId}/file`);
  const posterUrl = $derived(poster ?? `/api/v1/assets/${assetId}/variants/poster`);
  const spriteVTT = $derived(`/api/v1/assets/${assetId}/variants/sprites.vtt`);

  let videoEl: HTMLVideoElement | undefined = $state();
  let hls: any = null;

  let currentFrame = $state(0);
  let totalFrames = $state(0);
  let duration = $state(0);
  let playing = $state(false);
  let playbackRate = $state(1);
  let detectedFps = $state(24);

  $effect(() => {
    detectedFps = fpsProp;
  });

  // Loop region.
  let loopIn = $state<number | null>(null);
  let loopOut = $state<number | null>(null);

  // Sprite preview state — populated lazily on first scrubber hover.
  interface SpriteCue {
    start: number;
    end: number;
    src: string;
    x: number;
    y: number;
    w: number;
    h: number;
  }
  let sprites = $state<SpriteCue[]>([]);
  let hoverSprite = $state<SpriteCue | null>(null);
  let hoverPctX = $state(0);
  let hoverTime = $state(0);
  let scrubberHovering = $state(false);
  let scrubberEl: HTMLDivElement | undefined = $state();

  // ---- mount / unmount ---------------------------------------------------

  // Sticky volume state persisted across player mounts. Default
  // muted so autoplay can kick in (browsers block sound-on autoplay).
  // The moment the user touches the volume control we treat that as
  // their explicit preference: mute is cleared and the level is
  // saved in localStorage for the next session.
  const VOL_KEY = 'video.volume';
  const MUTE_KEY = 'video.muted';
  function readSavedVolume(): { vol: number; muted: boolean } {
    try {
      const v = localStorage.getItem(VOL_KEY);
      const m = localStorage.getItem(MUTE_KEY);
      return {
        vol: v !== null ? Math.max(0, Math.min(1, +v)) : 1,
        muted: m === null ? true : m === '1',
      };
    } catch {
      return { vol: 1, muted: true };
    }
  }

  onMount(async () => {
    if (!videoEl) return;
    videoEl.poster = posterUrl;

    // Apply saved volume / mute preference BEFORE attaching a source.
    const { vol, muted } = readSavedVolume();
    videoEl.volume = vol;
    videoEl.muted = muted;
    videoEl.autoplay = true;
    videoEl.playsInline = true;

    // Probe the manifest first. The variant route can 404 if the
    // worker hasn't generated HLS yet; fall back directly to /file
    // instead of letting HLS.js retry into a fatal-error loop.
    let useHLS = false;
    try {
      const head = await fetch(masterUrl, { method: 'GET', credentials: 'include' });
      useHLS = head.ok;
    } catch {
      useHLS = false;
    }

    const attemptPlay = () => {
      // Browsers may still gate autoplay if the page hasn't been
      // interacted with — calling play() on an already-muted element
      // is the canonical pattern that survives policy.
      if (!videoEl) return;
      videoEl.play().catch(() => {
        // Couldn't start (rare even when muted). Leave paused; the
        // user's first click in the transport will recover.
      });
    };

    if (useHLS && videoEl.canPlayType('application/vnd.apple.mpegurl')) {
      videoEl.src = masterUrl;
      videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
    } else if (useHLS) {
      try {
        const mod = await import('hls.js');
        const Hls = mod.default;
        if (Hls.isSupported()) {
          hls = new Hls({ enableWorker: true, lowLatencyMode: false });
          hls.loadSource(masterUrl);
          hls.attachMedia(videoEl);
          hls.on(Hls.Events.MANIFEST_PARSED, attemptPlay);
          hls.on(Hls.Events.ERROR, (_e: unknown, data: any) => {
            if (data?.fatal) {
              try { hls?.destroy(); } catch { /* ignore */ }
              hls = null;
              if (videoEl) {
                videoEl.src = fallbackUrl;
                videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
              }
            }
          });
        } else if (videoEl) {
          videoEl.src = fallbackUrl;
          videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
        }
      } catch {
        if (videoEl) {
          videoEl.src = fallbackUrl;
          videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
        }
      }
    } else {
      videoEl.src = fallbackUrl;
      videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
    }

    // Per-frame callback: the browser invokes this on every painted
    // frame, with the source PTS. We derive a frame number from PTS
    // × fps. requestVideoFrameCallback is gated on a modern browser;
    // fallback path uses rAF + currentTime.
    if ('requestVideoFrameCallback' in videoEl) {
      const cb = (_now: number, meta: any) => {
        if (typeof meta?.mediaTime === 'number') {
          currentFrame = Math.round(meta.mediaTime * detectedFps);
        }
        if (videoEl) (videoEl as any).requestVideoFrameCallback(cb);
      };
      (videoEl as any).requestVideoFrameCallback(cb);
    } else {
      const tick = () => {
        if (!videoEl) return;
        currentFrame = Math.round(videoEl.currentTime * detectedFps);
        requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    }

    // Persist any volume / mute change the user makes from now on.
    videoEl.addEventListener('volumechange', () => {
      if (!videoEl) return;
      try {
        localStorage.setItem(VOL_KEY, String(videoEl.volume));
        localStorage.setItem(MUTE_KEY, videoEl.muted ? '1' : '0');
      } catch { /* private mode etc. */ }
    });

    void loadSprites();
    document.addEventListener('keydown', handleKey);
    document.addEventListener('fullscreenchange', onFullscreenChange);
  });

  function onFullscreenChange() {
    isFullscreen = !!document.fullscreenElement;
  }

  onDestroy(() => {
    document.removeEventListener('keydown', handleKey);
    document.removeEventListener('fullscreenchange', onFullscreenChange);
    if (hls) {
      try { hls.destroy(); } catch { /* ignore */ }
      hls = null;
    }
  });

  // ---- duration / fps tracking ------------------------------------------

  function onLoadedMetadata() {
    if (!videoEl) return;
    duration = videoEl.duration || 0;
    totalFrames = Math.round(duration * detectedFps);
  }

  // ---- transport ---------------------------------------------------------

  function play() { videoEl?.play(); playing = true; }
  function pause() { videoEl?.pause(); playing = false; }
  function togglePlay() { (playing || (videoEl && !videoEl.paused)) ? pause() : play(); }

  function stepFrames(n: number) {
    if (!videoEl) return;
    pause();
    const newFrame = clamp(currentFrame + n, 0, totalFrames);
    videoEl.currentTime = newFrame / detectedFps;
    currentFrame = newFrame;
  }

  function seekToFrame(frame: number) {
    if (!videoEl) return;
    const f = clamp(frame, 0, totalFrames);
    videoEl.currentTime = f / detectedFps;
    currentFrame = f;
  }

  function clamp(n: number, lo: number, hi: number): number {
    return Math.max(lo, Math.min(hi, n));
  }

  function setRate(r: number) {
    if (!videoEl) return;
    playbackRate = r;
    videoEl.playbackRate = r;
  }

  // JKL transport (industry standard):
  //   J — reverse (or slow rev with multiple presses)
  //   K — pause
  //   L — play (or fast fwd with multiple presses)
  //   ←/→ , . — ±1 frame
  //   Shift+←/→ — ±10 frames
  //   I / O — mark loop in / out
  //   Backspace — clear loop
  //   Space — toggle play
  //   1-5 — speed presets
  function handleKey(e: KeyboardEvent) {
    if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    const k = e.key.toLowerCase();
    switch (k) {
      case ' ': e.preventDefault(); togglePlay(); break;
      case 'k': e.preventDefault(); pause(); break;
      case 'l': e.preventDefault(); play(); setRate(playbackRate >= 1 ? Math.min(playbackRate * 2, 4) : 1); break;
      case 'j':
        e.preventDefault();
        if (videoEl) {
          videoEl.playbackRate = -Math.abs(playbackRate >= 1 ? playbackRate : 1);
          play();
        }
        break;
      case ',':
      case 'arrowleft': e.preventDefault(); stepFrames(e.shiftKey ? -10 : -1); break;
      case '.':
      case 'arrowright': e.preventDefault(); stepFrames(e.shiftKey ? 10 : 1); break;
      case 'i': e.preventDefault(); loopIn = currentFrame; break;
      case 'o': e.preventDefault(); loopOut = currentFrame; break;
      case 'backspace': e.preventDefault(); loopIn = null; loopOut = null; break;
      case '1': setRate(0.25); break;
      case '2': setRate(0.5); break;
      case '3': setRate(1); break;
      case '4': setRate(2); break;
      case '5': setRate(4); break;
      case 'f': e.preventDefault(); toggleFullscreen(); break;
      case 'r': e.preventDefault(); resetView(); break;
      case 'g': e.preventDefault(); goToOpen = true; break;
    }
  }

  // Loop region enforcement during playback.
  function onTimeUpdate() {
    if (!videoEl) return;
    if (loopIn !== null && loopOut !== null && loopOut > loopIn) {
      const inT = loopIn / detectedFps;
      const outT = loopOut / detectedFps;
      if (videoEl.currentTime > outT) videoEl.currentTime = inT;
    }
  }

  // ---- sprite VTT --------------------------------------------------------

  async function loadSprites() {
    try {
      const r = await fetch(spriteVTT, { credentials: 'include' });
      if (!r.ok) return;
      const text = await r.text();
      sprites = parseVTTSprites(text, `/api/v1/assets/${assetId}/variants/sprites.jpg`);
    } catch {
      /* ignore — scrub still works without preview */
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
      const next = lines[i + 1] || '';
      const xy = next.match(/#xywh=(\d+),(\d+),(\d+),(\d+)/);
      if (!xy) continue;
      out.push({
        start, end, src: baseHref,
        x: +xy[1], y: +xy[2], w: +xy[3], h: +xy[4],
      });
    }
    return out;
  }

  function parseVTTTime(s: string): number {
    const [h, m, rest] = s.split(':');
    return +h * 3600 + +m * 60 + parseFloat(rest);
  }

  // ---- scrubber interaction ---------------------------------------------

  function onScrubberMove(e: MouseEvent) {
    if (!scrubberEl) return;
    const rect = scrubberEl.getBoundingClientRect();
    const pct = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    hoverPctX = pct * 100;
    hoverTime = pct * duration;
    hoverSprite = sprites.find((c) => hoverTime >= c.start && hoverTime < c.end) ?? null;
  }
  function onScrubberLeave() {
    scrubberHovering = false;
    hoverSprite = null;
  }
  function onScrubberClick(e: MouseEvent) {
    if (!scrubberEl || !videoEl) return;
    const rect = scrubberEl.getBoundingClientRect();
    const pct = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    seekToFrame(Math.round(pct * totalFrames));
  }

  // ---- format helpers ----------------------------------------------------

  function tc(frame: number): string {
    // Animation-standard timecode: HH:MM:SS:FF.
    if (!Number.isFinite(frame) || frame < 0) frame = 0;
    const fr = Math.round(frame);
    const fpsR = Math.round(detectedFps);
    const f = fr % fpsR;
    const totSec = Math.floor(fr / fpsR);
    const s = totSec % 60;
    const m = Math.floor(totSec / 60) % 60;
    const h = Math.floor(totSec / 3600);
    const pad = (n: number, w = 2) => n.toString().padStart(w, '0');
    return `${pad(h)}:${pad(m)}:${pad(s)}:${pad(f)}`;
  }

  const playheadPct = $derived(totalFrames > 0 ? (currentFrame / totalFrames) * 100 : 0);
  const loopInPct = $derived(totalFrames > 0 && loopIn !== null ? (loopIn / totalFrames) * 100 : 0);
  const loopOutPct = $derived(totalFrames > 0 && loopOut !== null ? (loopOut / totalFrames) * 100 : 0);

  // ---- canvas pan + zoom -------------------------------------------------
  //
  // Pixel-perfect inspection — animators want to zoom into a single
  // pixel without leaving playback. We apply a CSS transform on the
  // video element directly so playback continues while panned.

  let zoom = $state(1);
  let panX = $state(0);
  let panY = $state(0);
  let dragging = $state(false);
  let dragStartX = 0;
  let dragStartY = 0;
  let panStartX = 0;
  let panStartY = 0;
  let canvasEl: HTMLDivElement | undefined = $state();

  function onCanvasWheel(e: WheelEvent) {
    // Plain wheel = scrub a few frames; ctrl/⌘ + wheel = zoom.
    if (!e.ctrlKey && !e.metaKey) {
      e.preventDefault();
      stepFrames(e.deltaY > 0 ? 1 : -1);
      return;
    }
    e.preventDefault();
    const next = Math.max(1, Math.min(20, zoom * (e.deltaY > 0 ? 0.9 : 1.1)));
    // Zoom toward the cursor.
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
    // Plain click on the video toggles play. A drag starts pan.
    dragStartX = e.clientX;
    dragStartY = e.clientY;
    panStartX = panX;
    panStartY = panY;
    dragging = false;
    const move = (mv: MouseEvent) => {
      const dx = mv.clientX - dragStartX;
      const dy = mv.clientY - dragStartY;
      if (!dragging && Math.hypot(dx, dy) > 4) dragging = true;
      if (dragging) {
        panX = panStartX + dx;
        panY = panStartY + dy;
      }
    };
    const up = () => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
      if (!dragging) {
        // Treat it as a click → toggle play.
        togglePlay();
      }
      // Reset after the click so a follow-up handler doesn't see it.
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

  let containerEl: HTMLDivElement | undefined = $state();
  let isFullscreen = $state(false);

  function toggleFullscreen() {
    if (!containerEl) return;
    if (!document.fullscreenElement) {
      void containerEl.requestFullscreen?.();
    } else {
      void document.exitFullscreen?.();
    }
  }

  // ---- jump-to-frame input ----------------------------------------------

  let goToValue = $state('');
  let goToOpen = $state(false);

  function commitGoTo() {
    const s = goToValue.trim();
    if (!s) { goToOpen = false; return; }
    // Accept a raw frame number ("1234") or a timecode ("00:00:05:12"
    // or "5:12" or "5.5s").
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
      frame = ((h * 3600 + m * 60 + sec) * Math.round(detectedFps)) + f;
    } else if (secM) {
      frame = Math.round(parseFloat(secM[1]) * detectedFps);
    }
    if (Number.isFinite(frame)) {
      seekToFrame(Math.max(0, Math.min(totalFrames, Math.round(frame))));
    }
    goToValue = '';
    goToOpen = false;
  }
</script>

<div bind:this={containerEl} class="flex h-full w-full flex-col bg-black text-white">
  <!-- canvas -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={canvasEl}
    class="relative flex-1 overflow-hidden bg-black"
    class:cursor-grabbing={dragging}
    class:cursor-grab={!dragging && zoom > 1}
    onwheel={onCanvasWheel}
    onmousedown={onCanvasMouseDown}
  >
    <video
      bind:this={videoEl}
      class="absolute left-1/2 top-1/2 max-h-none max-w-none select-none"
      style="transform: translate(calc(-50% + {panX}px), calc(-50% + {panY}px)) scale({zoom}); transform-origin: center center; width: 100%; height: 100%; object-fit: contain;"
      onplay={() => (playing = true)}
      onpause={() => (playing = false)}
      onloadedmetadata={onLoadedMetadata}
      ontimeupdate={onTimeUpdate}
      preload="metadata"
      playsinline
    >
      <track kind="metadata" src={spriteVTT} default />
    </video>

    <!-- HUD: timecode + frame counter, always visible -->
    <div class="pointer-events-none absolute right-3 top-3 rounded bg-black/70 px-2 py-1 font-mono text-xs">
      {tc(currentFrame)} · f{currentFrame}{totalFrames > 0 ? `/${totalFrames}` : ''} · {detectedFps.toFixed(2)} fps
      {#if zoom !== 1} · {Math.round(zoom * 100)}%{/if}
    </div>

    <!-- Top-right action buttons -->
    <div class="absolute right-3 top-12 flex flex-col gap-2">
      <button
        type="button"
        onclick={() => (goToOpen = !goToOpen)}
        class="rounded bg-black/70 p-1.5 text-xs hover:bg-black/90"
        title="Jump to frame (G)"
        aria-label="Jump to frame"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
      </button>
      <button
        type="button"
        onclick={resetView}
        class="rounded bg-black/70 p-1.5 text-xs hover:bg-black/90"
        class:opacity-40={zoom === 1 && panX === 0 && panY === 0}
        title="Reset view (R)"
        aria-label="Reset view"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="1 4 1 10 7 10" /><path d="M3.51 15a9 9 0 1 0 2.13-9.36L1 10" /></svg>
      </button>
      <button
        type="button"
        onclick={toggleFullscreen}
        class="rounded bg-black/70 p-1.5 text-xs hover:bg-black/90"
        title={isFullscreen ? 'Exit fullscreen (F)' : 'Fullscreen (F)'}
        aria-label="Fullscreen"
      >
        {#if isFullscreen}
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 3v3a2 2 0 0 1-2 2H3" /><path d="M21 8h-3a2 2 0 0 1-2-2V3" /><path d="M3 16h3a2 2 0 0 1 2 2v3" /><path d="M16 21v-3a2 2 0 0 1 2-2h3" /></svg>
        {:else}
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7V3h4" /><path d="M21 7V3h-4" /><path d="M3 17v4h4" /><path d="M21 17v4h-4" /></svg>
        {/if}
      </button>
    </div>

    <!-- Go-to-frame input — opens via G or the arrow button. -->
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

  <!-- scrubber -->
  <div class="relative h-3 bg-zinc-900">
    <!-- hit area -->
    <div
      bind:this={scrubberEl}
      class="absolute inset-0 cursor-crosshair"
      onmouseenter={() => (scrubberHovering = true)}
      onmousemove={onScrubberMove}
      onmouseleave={onScrubberLeave}
      onclick={onScrubberClick}
      role="slider"
      aria-valuenow={currentFrame}
      aria-valuemin={0}
      aria-valuemax={totalFrames || 0}
      aria-label="Scrubber"
      tabindex="0"
    >
      <!-- played -->
      <div class="absolute inset-y-0 left-0 bg-accent/60" style="width: {playheadPct}%"></div>
      <!-- loop region -->
      {#if loopIn !== null && loopOut !== null && loopOut > loopIn}
        <div
          class="absolute inset-y-0 bg-yellow-500/30"
          style="left: {loopInPct}%; width: {loopOutPct - loopInPct}%"
        ></div>
      {/if}
      <!-- playhead -->
      <div class="absolute inset-y-0 w-px bg-white" style="left: {playheadPct}%"></div>
    </div>

    <!-- sprite preview tooltip -->
    {#if scrubberHovering && hoverSprite}
      <div
        class="pointer-events-none absolute bottom-4 z-10 -translate-x-1/2 rounded border border-zinc-700 bg-black p-1 shadow-xl"
        style="left: {hoverPctX}%"
      >
        <div
          class="bg-zinc-950"
          style="width: {hoverSprite.w}px; height: {hoverSprite.h}px; background-image: url({hoverSprite.src}); background-position: -{hoverSprite.x}px -{hoverSprite.y}px;"
        ></div>
        <div class="mt-1 text-center font-mono text-[10px]">{tc(Math.round(hoverTime * detectedFps))}</div>
      </div>
    {/if}
  </div>

  <!-- transport bar -->
  <div class="flex items-center gap-3 border-t border-zinc-800 bg-zinc-950 px-3 py-2 text-sm">
    <button type="button" onclick={() => stepFrames(-10)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="−10 (Shift+←)">⏮</button>
    <button type="button" onclick={() => stepFrames(-1)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="Step back (,)">◀|</button>
    <button type="button" onclick={togglePlay} class="rounded bg-zinc-800 px-3 py-1 font-medium hover:bg-zinc-700" title="Play/Pause (K)">
      {playing ? '⏸' : '▶'}
    </button>
    <button type="button" onclick={() => stepFrames(1)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="Step fwd (.)">|▶</button>
    <button type="button" onclick={() => stepFrames(10)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="+10 (Shift+→)">⏭</button>

    <span class="mx-2 h-4 w-px bg-zinc-800"></span>

    {#each [0.25, 0.5, 1, 2, 4] as r}
      <button
        type="button"
        onclick={() => setRate(r)}
        class="px-1.5 py-0.5 text-xs hover:bg-zinc-800"
        class:bg-zinc-800={playbackRate === r}
        title="Speed {r}×"
      >
        {r}×
      </button>
    {/each}

    <span class="mx-2 h-4 w-px bg-zinc-800"></span>

    <button
      type="button"
      onclick={() => (loopIn = currentFrame)}
      class="px-1.5 py-0.5 text-xs hover:bg-zinc-800"
      title="Mark loop in (I)"
    >
      Loop in {loopIn !== null ? `(f${loopIn})` : ''}
    </button>
    <button
      type="button"
      onclick={() => (loopOut = currentFrame)}
      class="px-1.5 py-0.5 text-xs hover:bg-zinc-800"
      title="Mark loop out (O)"
    >
      Loop out {loopOut !== null ? `(f${loopOut})` : ''}
    </button>
    {#if loopIn !== null || loopOut !== null}
      <button
        type="button"
        onclick={() => { loopIn = null; loopOut = null; }}
        class="px-1.5 py-0.5 text-xs text-zinc-400 hover:text-white"
        title="Clear loop (⌫)"
      >
        clear
      </button>
    {/if}

    <span class="ml-auto font-mono text-xs text-zinc-400">
      JKL · ⇧← → · I/O loop · 1-5 speed · G goto · F fullscreen · ⌘wheel zoom
    </span>
  </div>
</div>
