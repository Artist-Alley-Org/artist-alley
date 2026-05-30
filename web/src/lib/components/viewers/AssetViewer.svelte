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

  import { onMount, onDestroy, type Snippet } from 'svelte';
  import type { ViewController } from './controller';
  import { defaultController, kindForExtension } from './controller';
  import ImageView from './ImageView.svelte';
  import VideoView from './VideoView.svelte';
  import ModelView from './ModelView.svelte';
  import AudioView from './AudioView.svelte';
  import PDFView from './PDFView.svelte';
  import FontView from './FontView.svelte';
  import PlaceholderView from './PlaceholderView.svelte';
  import ViewerMenuBar from './ViewerMenuBar.svelte';

  // Native + three-loader 3D paths we ship today. Other 3D
  // extensions (mview, blend, mb, ma, max, usd*) fall through to
  // the placeholder until 1.18.B-10/11 ship dedicated paths.
  // 'mview' is here too — ModelView routes that one to marmoset.js
  // instead of the three.js stack.
  const SUPPORTED_3D = new Set(['glb', 'gltf', 'fbx', 'obj', 'mview']);

  // Shared shape lives in controller.ts so every view body sees
  // the exact same Asset and TS can prop-bind without drift.
  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    /** True when this viewer is the focused slide. Hotkeys + autoplay
        only fire for the focused viewer so two carousel slides don't
        fight for the keyboard. */
    active?: boolean;
    /** True once the host (e.g. PostModal) has explicitly entered
        review mode. Review mode swaps the right pane from metadata
        to the per-kind tools panel and turns on review-only
        affordances (3D wireframe toggle, exposure sliders, etc).
        Pan + zoom are *not* gated on this — they're always live the
        moment an asset is on screen, so a user can inspect anything
        without first hunting for a mode toggle. The Review button in
        the toolbar or a double-click on the canvas flips this on. */
    reviewMode?: boolean;
    /** Host-provided content for the right pane when NOT in review
        mode. PostModal injects its post metadata snippet here; the
        standalone /assets/[id] page can pass asset-only info; an
        embedded viewer can pass anything. When reviewMode flips on
        the pane swaps to the kind-aware tools panel the viewer owns. */
    metadataSlot?: Snippet;
    /** Bindable pane open/closed state so the host can persist the
        user's preference in its own localStorage. Default open when
        there's something to show. */
    paneCollapsed?: boolean;
    /** Optional close handler — when set, the File menu in the
        toolbar grows a "Close" entry. Hosts that own their own close
        affordance (the dialog × button) omit this. */
    onClose?: () => void;
  }

  let {
    asset,
    active = true,
    reviewMode = $bindable(false),
    metadataSlot,
    paneCollapsed = $bindable(false),
    onClose,
  }: Props = $props();

  // The right pane is shown when there's something to put in it:
  // review tools are always available for an active viewer; the
  // metadata slot is host-provided. No slot + no review = no pane
  // (so a small card preview doesn't grow an empty sidebar).
  const paneEnabled = $derived(reviewMode || !!metadataSlot);
  const paneOpen = $derived(paneEnabled && !paneCollapsed);

  function togglePane() {
    paneCollapsed = !paneCollapsed;
  }

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
    // 3D bodies own all input (orbit controls handle wheel-as-dolly
    // natively — the camera moves toward the model rather than the
    // viewport canvas scaling, which is the per-kind "pan/zoom on the
    // object, not the viewer" semantics 3D needs). Skip our outer
    // transform entirely.
    if (kind === '3d') return;
    // Timeline kinds (video, audio, paged PDF later) treat plain wheel
    // as one frame's worth of scrub — the muscle memory for review
    // work. Ctrl/⌘ + wheel still zooms so the user can inspect a
    // single frame without losing the scrub gesture.
    if (controller.hasTimeline && !e.ctrlKey && !e.metaKey) {
      e.preventDefault();
      controller.stepFrames(e.deltaY > 0 ? 1 : -1);
      return;
    }
    // Static kinds (image, font, PDF when we drop scroll-paged in a
    // later commit): wheel always zooms. No modifier required, no
    // "enter review mode first" gate — the user expects to inspect
    // the asset the moment it loads.
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
    // 3D bodies own all drag (orbit). Leave the outer transform alone.
    if (kind === '3d') return;
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
        // Treat a non-drag click on timeline kinds as play/pause.
        // Static kinds (image, pdf, font) have no play state, so a
        // bare click does nothing — the double-click handler below
        // owns the "fit to window" gesture without fighting this.
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

  // Double-click on the canvas = fit-to-window (reset zoom + pan).
  // Cheap "I'm lost, get me back" gesture that mirrors Photoshop's
  // double-click-the-hand-tool affordance. 3D bodies have their own
  // frame-all on the tools panel; for 2D this is the only gesture.
  function onCanvasDoubleClick(e: MouseEvent) {
    if (kind === '3d') return;
    if (controller.hasTimeline) return; // timeline kinds reserve dbl-click
    e.preventDefault();
    resetView();
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
    // Hotkeys are review-mode-only. Outside review the asset is just
    // chrome and the host owns the keyboard (modal nav, sidebar
    // toggle, ESC-to-close).
    if (!reviewMode) return;
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
  <!-- Top toolbar — File / Edit / About menus + asset info strip +
       Review toggle + reset/fullscreen/pane quick-actions. Replaces
       the old floating top-right button column (which overlaid the
       asset and had no home for non-icon actions). -->
  <ViewerMenuBar
    {asset}
    {controller}
    {reviewMode}
    {paneCollapsed}
    {paneEnabled}
    {isFullscreen}
    onResetView={resetView}
    onToggleFullscreen={toggleFullscreen}
    onTogglePane={togglePane}
    onToggleReview={() => (reviewMode = !reviewMode)}
    {onClose}
  />

  <!-- Canvas + pane row. The pane is a flex sibling so it pushes the
       canvas's width rather than overlaying part of it — otherwise the
       asset visually drifts off-center every time the user opens the
       pane. Width-animated slide-in keeps the smooth transition the
       overlay version had. -->
  <div class="flex min-h-0 flex-1">

  <!-- Canvas (pan + zoom transform wraps the view body) -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={canvasEl}
    class="relative min-w-0 flex-1 overflow-hidden bg-black"
    class:cursor-grabbing={dragging}
    class:cursor-grab={!dragging && zoom > 1}
    onwheel={onCanvasWheel}
    onmousedown={onCanvasMouseDown}
    ondblclick={onCanvasDoubleClick}
  >
    <div
      class="absolute inset-0 flex items-center justify-center"
      style="transform: translate({panX}px, {panY}px) scale({zoom}); transform-origin: center center;"
    >
      <!-- {#key} forces a fresh view-body mount when the asset id
           changes. Without this, three.js / model-viewer keep the old
           scene loaded when a host swaps assets without unmounting
           the AssetViewer (the common multi-asset carousel pattern).
           Side-effect: pan/zoom resets per asset, which is what users
           expect when navigating to a new image anyway. -->
      {#key asset.id}
        {#if kind === 'video'}
          <VideoView {asset} bind:controller />
        {:else if kind === 'image'}
          <ImageView {asset} bind:controller />
        {:else if kind === 'audio'}
          <AudioView {asset} bind:controller />
        {:else if kind === 'pdf'}
          <PDFView {asset} bind:controller />
        {:else if kind === 'font'}
          <FontView {asset} bind:controller />
        {:else if kind === '3d' && SUPPORTED_3D.has((asset.file_extension || '').toLowerCase().replace(/^\./, ''))}
          <ModelView {asset} bind:controller {reviewMode} />
        {:else}
          <PlaceholderView {asset} bind:controller />
        {/if}
      {/key}
    </div>

    <!-- HUD: live frame counter / zoom %. The static asset info
         (filename, dimensions, codec) is in the toolbar above; the
         HUD here only shows values that change as the user
         interacts — frame position on timeline kinds, zoom % when
         it's not 100%. Bottom-left so it doesn't fight the toolbar
         or the pane. -->
    {#if controller.hasTimeline || zoom !== 1}
      <div
        class="pointer-events-none absolute bottom-3 left-3 rounded bg-black/70 px-2 py-1 font-mono text-xs"
      >
        {#if controller.hasTimeline}
          {controller.formatAnchor(controller.currentFrame)} · f{controller.currentFrame}{controller.totalFrames > 0 ? `/${controller.totalFrames}` : ''}
        {/if}
        {#if zoom !== 1}
          {controller.hasTimeline ? ' · ' : ''}{Math.round(zoom * 100)}%
        {/if}
      </div>
    {/if}

    <!-- Jump-to-frame quick-action — only visible on timeline kinds.
         Floated top-right of the canvas (under the toolbar). Reset
         and fullscreen used to live here too; they moved into the
         toolbar's quick-action group. -->
    {#if controller.hasTimeline}
      <button
        type="button"
        onclick={() => (goToOpen = !goToOpen)}
        class="absolute top-3 right-3 rounded bg-black/70 p-1.5 text-xs hover:bg-black/90"
        class:right-[25rem]={paneOpen}
        title="Jump to frame (G)"
        aria-label="Jump to frame"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
      </button>
    {/if}

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

    <!-- Annotation overlay layer (placeholder for Phase 1.18.B-6).
         Sized to the viewport for now; B-6 will narrow it to the
         asset's rendered rect (image bounds, video frame, etc.) so
         annotations land on content, not letterboxing. -->
    <div class="pointer-events-none absolute inset-0 z-20" data-role="annotation-layer"></div>

    {#if paneEnabled && paneCollapsed}
      <!-- Re-open tab on the right edge so the user can recover the
           pane after collapsing it. Lives inside the canvas (not the
           aside) because the aside collapses to width=0. -->
      <button
        type="button"
        onclick={togglePane}
        class="absolute right-0 top-1/2 z-30 -translate-y-1/2 rounded-l-md bg-black/60 px-2 py-3 text-white backdrop-blur-sm transition-colors hover:bg-black/80"
        aria-label="Show panel"
        title="Show panel (i)"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="m15 18-6-6 6-6" />
        </svg>
      </button>
    {/if}
  </div><!-- /canvas -->

    <!-- Right pane: flex sibling of the canvas so opening it shrinks
         the canvas (asset stays centred in the visible region rather
         than drifting behind an overlay). Width-animates between
         w-96 (open) and w-0 (collapsed) for the same smooth slide the
         old translate-based overlay gave us. -->
    {#if paneEnabled}
      <aside
        class="flex max-w-[40vw] shrink-0 flex-col overflow-hidden border-l border-border bg-surface text-fg shadow-2xl transition-[width] duration-200 ease-out"
        class:w-96={!paneCollapsed}
        class:w-0={paneCollapsed}
        class:border-l-0={paneCollapsed}
        aria-label={reviewMode ? 'Review tools' : 'Asset details'}
      >
        <header class="flex shrink-0 items-center justify-between border-b border-border px-4 py-3">
          <h2 class="text-sm font-medium">
            {#if reviewMode}Review tools{:else}Details{/if}
          </h2>
          <button
            type="button"
            onclick={togglePane}
            class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-surface-elevated hover:text-fg"
            aria-label="Collapse panel"
            title="Collapse (i)"
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="m9 18 6-6-6-6" />
            </svg>
          </button>
        </header>
        <div class="min-h-0 flex-1 overflow-y-auto">
          {#if reviewMode}
            <!-- Kind-aware tools, read from controller.tools that the
                 mounted view body populated. Each section renders only
                 if the view body exposed that tool group — wireframe
                 has no meaning for model-viewer glb, exposure works
                 for both 3D paths, etc. -->
            {#if controller.tools}
              {@const tools = controller.tools}
              <div class="space-y-1 p-3">
                <!-- ── Camera section ───────────────────────────────── -->
                {#if tools.frameAll || tools.resetCamera || tools.cameraPreset}
                  <section class="rounded-md border border-border bg-surface-elevated">
                    <header class="border-b border-border px-3 py-2 text-xs font-medium uppercase tracking-wide text-fg-muted">Camera</header>
                    <div class="flex flex-wrap gap-1.5 p-3">
                      {#if tools.frameAll}
                        <button type="button" onclick={tools.frameAll} class="rounded-md border border-border bg-surface px-2 py-1 text-xs hover:border-fg-muted/60 hover:bg-surface-elevated">
                          Frame all
                        </button>
                      {/if}
                      {#if tools.resetCamera}
                        <button type="button" onclick={tools.resetCamera} class="rounded-md border border-border bg-surface px-2 py-1 text-xs hover:border-fg-muted/60 hover:bg-surface-elevated">
                          Reset
                        </button>
                      {/if}
                    </div>
                  </section>
                {/if}

                <!-- ── Display section ──────────────────────────────── -->
                {#if tools.grid || tools.axes || tools.wireframe || tools.groundShadow}
                  <section class="rounded-md border border-border bg-surface-elevated">
                    <header class="border-b border-border px-3 py-2 text-xs font-medium uppercase tracking-wide text-fg-muted">Display</header>
                    <div class="space-y-2 p-3">
                      {#if tools.grid}
                        <label class="flex items-center justify-between text-xs">
                          <span>Grid</span>
                          <button
                            type="button"
                            onclick={tools.grid.toggle}
                            class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
                            class:bg-accent={tools.grid.enabled}
                            class:bg-border={!tools.grid.enabled}
                            role="switch"
                            aria-checked={tools.grid.enabled}
                          >
                            <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={tools.grid.enabled} class:translate-x-0.5={!tools.grid.enabled}></span>
                          </button>
                        </label>
                      {/if}
                      {#if tools.axes}
                        <label class="flex items-center justify-between text-xs">
                          <span>Axes</span>
                          <button
                            type="button"
                            onclick={tools.axes.toggle}
                            class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
                            class:bg-accent={tools.axes.enabled}
                            class:bg-border={!tools.axes.enabled}
                            role="switch"
                            aria-checked={tools.axes.enabled}
                          >
                            <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={tools.axes.enabled} class:translate-x-0.5={!tools.axes.enabled}></span>
                          </button>
                        </label>
                      {/if}
                      {#if tools.groundShadow}
                        <label class="flex items-center justify-between text-xs">
                          <span>Ground shadow</span>
                          <button
                            type="button"
                            onclick={tools.groundShadow.toggle}
                            class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
                            class:bg-accent={tools.groundShadow.enabled}
                            class:bg-border={!tools.groundShadow.enabled}
                            role="switch"
                            aria-checked={tools.groundShadow.enabled}
                          >
                            <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={tools.groundShadow.enabled} class:translate-x-0.5={!tools.groundShadow.enabled}></span>
                          </button>
                        </label>
                      {/if}
                      {#if tools.wireframe}
                        <div class="flex items-center justify-between text-xs">
                          <span>Wireframe</span>
                          <button
                            type="button"
                            onclick={tools.wireframe.cycle}
                            class="rounded-md border border-border bg-surface px-2 py-0.5 text-xs capitalize hover:border-fg-muted/60 hover:bg-surface-elevated"
                            title="Cycle: {tools.wireframe.options.join(' → ')}"
                          >
                            {tools.wireframe.mode}
                          </button>
                        </div>
                      {/if}
                    </div>
                  </section>
                {/if}

                <!-- ── Lighting section ─────────────────────────────── -->
                {#if tools.exposure || tools.envIntensity}
                  <section class="rounded-md border border-border bg-surface-elevated">
                    <header class="border-b border-border px-3 py-2 text-xs font-medium uppercase tracking-wide text-fg-muted">Lighting</header>
                    <div class="space-y-3 p-3">
                      {#if tools.exposure}
                        <label class="block text-xs">
                          <span class="mb-1 flex items-center justify-between">
                            <span>{tools.exposure.label ?? 'Exposure'}</span>
                            <span class="font-mono text-fg-muted">{tools.exposure.value.toFixed(2)}</span>
                          </span>
                          <input
                            type="range"
                            min={tools.exposure.min}
                            max={tools.exposure.max}
                            step={tools.exposure.step ?? 0.01}
                            value={tools.exposure.value}
                            oninput={(e) => tools.exposure!.set(+(e.currentTarget as HTMLInputElement).value)}
                            class="w-full accent-accent"
                          />
                        </label>
                      {/if}
                      {#if tools.envIntensity}
                        <label class="block text-xs">
                          <span class="mb-1 flex items-center justify-between">
                            <span>{tools.envIntensity.label ?? 'Env intensity'}</span>
                            <span class="font-mono text-fg-muted">{tools.envIntensity.value.toFixed(2)}</span>
                          </span>
                          <input
                            type="range"
                            min={tools.envIntensity.min}
                            max={tools.envIntensity.max}
                            step={tools.envIntensity.step ?? 0.01}
                            value={tools.envIntensity.value}
                            oninput={(e) => tools.envIntensity!.set(+(e.currentTarget as HTMLInputElement).value)}
                            class="w-full accent-accent"
                          />
                        </label>
                      {/if}
                    </div>
                  </section>
                {/if}

                <!-- ── Auto-rotate section ──────────────────────────── -->
                {#if tools.autoRotate || tools.autoRotateSpeed}
                  <section class="rounded-md border border-border bg-surface-elevated">
                    <header class="border-b border-border px-3 py-2 text-xs font-medium uppercase tracking-wide text-fg-muted">Auto-rotate</header>
                    <div class="space-y-3 p-3">
                      {#if tools.autoRotate}
                        <label class="flex items-center justify-between text-xs">
                          <span>Enabled</span>
                          <button
                            type="button"
                            onclick={tools.autoRotate.toggle}
                            class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
                            class:bg-accent={tools.autoRotate.enabled}
                            class:bg-border={!tools.autoRotate.enabled}
                            role="switch"
                            aria-checked={tools.autoRotate.enabled}
                          >
                            <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={tools.autoRotate.enabled} class:translate-x-0.5={!tools.autoRotate.enabled}></span>
                          </button>
                        </label>
                      {/if}
                      {#if tools.autoRotateSpeed && tools.autoRotate?.enabled}
                        <label class="block text-xs">
                          <span class="mb-1 flex items-center justify-between">
                            <span>{tools.autoRotateSpeed.label ?? 'Speed'}</span>
                            <span class="font-mono text-fg-muted">{tools.autoRotateSpeed.value.toFixed(1)}×</span>
                          </span>
                          <input
                            type="range"
                            min={tools.autoRotateSpeed.min}
                            max={tools.autoRotateSpeed.max}
                            step={tools.autoRotateSpeed.step ?? 0.1}
                            value={tools.autoRotateSpeed.value}
                            oninput={(e) => tools.autoRotateSpeed!.set(+(e.currentTarget as HTMLInputElement).value)}
                            class="w-full accent-accent"
                          />
                        </label>
                      {/if}
                    </div>
                  </section>
                {/if}
              </div>
            {:else}
              <!-- No tools registered (kind doesn't have any yet OR
                   the view body hasn't mounted yet). -->
              <div class="p-4 text-sm text-fg-muted">
                <p>No review tools available for this asset kind yet.</p>
              </div>
            {/if}
          {:else if metadataSlot}
            {@render metadataSlot()}
          {/if}
        </div>
      </aside>
    {/if}
  </div><!-- /canvas+pane row -->

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
