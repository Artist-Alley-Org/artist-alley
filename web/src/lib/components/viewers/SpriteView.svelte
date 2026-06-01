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

  const gridCols = $derived(
    cellW > 0
      ? Math.max(1, Math.floor((imgW - originX + padX) / (cellW + padX)))
      : 1,
  );
  const gridRows = $derived(
    cellH > 0
      ? Math.max(1, Math.floor((imgH - originY + padY) / (cellH + padY)))
      : 1,
  );

  // ── Companion metadata ────────────────────────────────────────
  // When the asset has a companion .json sidecar (TexturePacker
  // JSON Hash / Array, Phaser, Aseprite export), parse it for
  // explicit frame rects + named animation tags. Metadata wins
  // over manual grid — slicer hides when frames are loaded.
  interface FrameRect { name: string; sx: number; sy: number; sw: number; sh: number; duration?: number }
  interface TagRange { name: string; from: number; to: number; direction?: 'forward' | 'reverse' | 'pingpong' }
  interface AssetCompanion { id: string; asset_id: string; path: string; content_type: string; size_bytes: number }

  let metadataFrames = $state<FrameRect[] | null>(null);
  let metadataTags = $state<TagRange[]>([]);
  let metadataCompanion = $state<AssetCompanion | null>(null);
  let metadataError = $state<string | null>(null);
  let metadataLoading = $state(false);
  // Active tag — when set, playback loops within the tag's [from, to]
  // range instead of the full frame list. null = play everything.
  let activeTag = $state<string | null>(null);

  const frames = $derived<FrameRect[] | null>(metadataFrames);

  // Effective playback window: tag range when one is active, else
  // the whole frame list. Used by stepFrame + the playhead clamp.
  const playRange = $derived.by(() => {
    if (frames && activeTag) {
      const t = metadataTags.find((x) => x.name === activeTag);
      if (t) return { from: t.from, to: t.to, dir: t.direction ?? 'forward' };
    }
    const len = frames ? frames.length : (frameCountOverride ?? gridCols * gridRows);
    return { from: 0, to: Math.max(0, len - 1), dir: 'forward' as 'forward' | 'reverse' | 'pingpong' };
  });
  const frameCount = $derived(Math.max(1, playRange.to - playRange.from + 1));

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

  // Map a relative index inside the active playRange into a frame
  // rect. When metadata frames are loaded we index those; otherwise
  // we walk the manual grid in reading order.
  function frameRect(relIdx: number): { sx: number; sy: number; sw: number; sh: number } {
    const idx = playRange.from + relIdx;
    if (frames && frames.length > 0) {
      const f = frames[Math.max(0, Math.min(frames.length - 1, idx))];
      return { sx: f.sx, sy: f.sy, sw: f.sw, sh: f.sh };
    }
    if (cellW <= 0 || cellH <= 0) {
      return { sx: 0, sy: 0, sw: imgW, sh: imgH };
    }
    const c = idx % gridCols;
    const r = Math.floor(idx / gridCols);
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
    if (!showGrid) return;
    ctx.strokeStyle = 'rgba(255, 100, 100, 0.7)';
    ctx.lineWidth = 1;
    if (frames && frames.length > 0) {
      // Metadata mode — draw each frame's exact rect. Lets the user
      // verify the JSON's frame coords against the artwork.
      for (const f of frames) {
        ctx.strokeRect(f.sx * zoom + 0.5, f.sy * zoom + 0.5, f.sw * zoom, f.sh * zoom);
      }
      return;
    }
    if (cellW <= 0 || cellH <= 0) return;
    // Manual-grid mode — draw the cell boundaries.
    for (let c = 0; c <= gridCols; c++) {
      const x = (originX + c * (cellW + padX)) * zoom + 0.5;
      ctx.beginPath();
      ctx.moveTo(x, 0);
      ctx.lineTo(x, overlayEl.height);
      ctx.stroke();
    }
    for (let r = 0; r <= gridRows; r++) {
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
  // Overlay reacts to slicer params + toggle + zoom + metadata.
  $effect(() => {
    void zoom; void cellW; void cellH; void padX; void padY;
    void originX; void originY; void img; void showGrid;
    void gridCols; void gridRows; void frames;
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
    void loadCompanions();
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

  // ── Companion metadata: fetch + parse ─────────────────────────
  //
  // Sidecar formats we recognise:
  //   - TexturePacker JSON Hash: { frames: { "frame_name": {frame:{x,y,w,h}, ...}, ... }, meta: {...} }
  //   - TexturePacker / Phaser JSON Array: { frames: [{filename, frame:{x,y,w,h}, ...}], meta: {...} }
  //   - Aseprite export (same as TP, plus meta.frameTags for named ranges)
  //
  // For Hash form we natural-sort entries by key so files named
  // "walk 0.png", "walk 1.png" ... land in order. For Array form
  // the JSON's order is authoritative. Frame durations + tag
  // ranges come straight from the meta block when present.

  // Pick the companion that looks like sprite metadata. Heuristic:
  // application/json content type, OR file path ending in .json /
  // .atlas. We don't yet pick "best" if there are several — first
  // match wins; the user can detach + re-upload if they hit a
  // disambiguation case (unlikely on a sprite asset).
  function pickMetadataCompanion(list: AssetCompanion[]): AssetCompanion | null {
    for (const c of list) {
      const p = c.path.toLowerCase();
      if (c.content_type.startsWith('application/json')) return c;
      if (p.endsWith('.json') || p.endsWith('.atlas')) return c;
    }
    return null;
  }

  // Natural-sort comparator for keys like "walk 0.png", "walk 10.png".
  function naturalCompare(a: string, b: string): number {
    return a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' });
  }

  // Convert a parsed JSON blob into our FrameRect[] + TagRange[].
  // Tolerant: returns null on shape it can't make sense of. Logs
  // shape mismatches to the metadataError state so the user sees
  // why their sidecar didn't take.
  function parseSpriteJSON(text: string): { frames: FrameRect[]; tags: TagRange[] } | null {
    let data: unknown;
    try {
      data = JSON.parse(text);
    } catch (e) {
      metadataError = 'Companion JSON failed to parse: ' + (e instanceof Error ? e.message : String(e));
      return null;
    }
    if (!data || typeof data !== 'object') {
      metadataError = 'Companion JSON is not an object.';
      return null;
    }
    const obj = data as Record<string, unknown>;
    const rawFrames = obj.frames;
    const out: FrameRect[] = [];
    if (Array.isArray(rawFrames)) {
      // Array form — JSON order is authoritative.
      for (const entry of rawFrames) {
        const ef = (entry as Record<string, unknown>);
        const fr = ef.frame as { x?: number; y?: number; w?: number; h?: number } | undefined;
        if (!fr || typeof fr.x !== 'number') continue;
        out.push({
          name: String(ef.filename ?? ef.name ?? out.length),
          sx: fr.x, sy: fr.y ?? 0, sw: fr.w ?? 0, sh: fr.h ?? 0,
          duration: typeof ef.duration === 'number' ? ef.duration : undefined,
        });
      }
    } else if (rawFrames && typeof rawFrames === 'object') {
      // Hash form — natural-sort entries by key.
      const entries = Object.entries(rawFrames as Record<string, unknown>);
      entries.sort((a, b) => naturalCompare(a[0], b[0]));
      for (const [name, entry] of entries) {
        const ef = entry as Record<string, unknown>;
        const fr = ef.frame as { x?: number; y?: number; w?: number; h?: number } | undefined;
        if (!fr || typeof fr.x !== 'number') continue;
        out.push({
          name,
          sx: fr.x, sy: fr.y ?? 0, sw: fr.w ?? 0, sh: fr.h ?? 0,
          duration: typeof ef.duration === 'number' ? ef.duration : undefined,
        });
      }
    } else {
      metadataError = 'Companion JSON has no `frames` field; not a sprite atlas.';
      return null;
    }
    if (out.length === 0) {
      metadataError = 'Companion JSON had no valid frame rects.';
      return null;
    }
    // Tag ranges from Aseprite-style `meta.frameTags`.
    const meta = (obj.meta ?? {}) as Record<string, unknown>;
    const rawTags = (meta.frameTags ?? []) as unknown[];
    const tags: TagRange[] = [];
    for (const t of rawTags) {
      const tg = t as Record<string, unknown>;
      if (typeof tg.from === 'number' && typeof tg.to === 'number') {
        tags.push({
          name: String(tg.name ?? `tag ${tags.length + 1}`),
          from: tg.from,
          to: tg.to,
          direction: tg.direction === 'reverse' || tg.direction === 'pingpong' ? tg.direction : 'forward',
        });
      }
    }
    return { frames: out, tags };
  }

  // Fetch the companion list + parse metadata if present. Called
  // at mount; also re-called after the user uploads a fresh
  // sidecar so the parsed frames refresh without a reload.
  async function loadCompanions() {
    metadataError = null;
    try {
      const r = await fetch(`/api/v1/assets/${asset.id}/companions`, { credentials: 'include' });
      if (!r.ok) return;
      const list = (await r.json()) as AssetCompanion[];
      const meta = pickMetadataCompanion(list);
      if (!meta) return;
      const rr = await fetch(`/api/v1/assets/${asset.id}/companions/${meta.id}`, { credentials: 'include' });
      if (!rr.ok) {
        metadataError = `Companion fetch failed: HTTP ${rr.status}`;
        return;
      }
      const text = await rr.text();
      const parsed = parseSpriteJSON(text);
      if (parsed) {
        metadataCompanion = meta;
        metadataFrames = parsed.frames;
        metadataTags = parsed.tags;
        // Recommend FPS from the first frame's duration if metadata
        // provides one (Aseprite-style per-frame ms). Otherwise leave
        // the user's setting alone.
        const d = parsed.frames[0]?.duration;
        if (d && d > 0) fps = Math.max(0.5, Math.min(60, 1000 / d));
      }
    } catch (e) {
      metadataError = 'Companion load error: ' + (e instanceof Error ? e.message : String(e));
    }
  }

  // Detach the current metadata companion. Reverts the view to
  // manual-grid mode. Server-side the companion row is deleted +
  // its storage pin removed.
  async function detachMetadata() {
    if (!metadataCompanion) return;
    metadataLoading = true;
    try {
      await fetch(`/api/v1/assets/${asset.id}/companions/${metadataCompanion.id}`, {
        method: 'DELETE',
        credentials: 'include',
      });
    } finally {
      metadataCompanion = null;
      metadataFrames = null;
      metadataTags = [];
      activeTag = null;
      metadataLoading = false;
    }
  }

  // Upload a .json sidecar as a companion. Mirrors the existing
  // 3D-model companion flow: octet-stream PUT-like POST with an
  // X-Companion-Path header carrying the relative filename. After
  // upload we re-fetch the companion list to pick the new entry.
  let metadataInput: HTMLInputElement | undefined = $state();
  async function uploadMetadataFile(file: File) {
    metadataLoading = true;
    metadataError = null;
    try {
      const r = await fetch(`/api/v1/assets/${asset.id}/companions`, {
        method: 'POST',
        credentials: 'include',
        headers: {
          'Content-Type': 'application/octet-stream',
          'X-Companion-Path': file.name,
          'X-Content-Type': file.type || 'application/json',
        },
        body: file,
      });
      if (!r.ok) {
        const j = await r.json().catch(() => ({ error: `HTTP ${r.status}` }));
        metadataError = (j as { error?: string }).error ?? `Upload failed (HTTP ${r.status})`;
        return;
      }
      await loadCompanions();
    } catch (e) {
      metadataError = 'Upload error: ' + (e instanceof Error ? e.message : String(e));
    } finally {
      metadataLoading = false;
    }
  }
  function onMetadataPick(e: Event) {
    const t = e.currentTarget as HTMLInputElement;
    const f = t.files?.[0];
    t.value = '';
    if (f) void uploadMetadataFile(f);
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

    <!-- Metadata sidecar -->
    <section class="border-b border-white/10 p-3">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-white/40">Metadata</h3>
      {#if metadataCompanion}
        <div class="rounded border border-accent/40 bg-accent/10 p-2 text-[10px]">
          <div class="flex items-start justify-between gap-2">
            <div class="min-w-0 flex-1">
              <div class="truncate font-mono text-white/90" title={metadataCompanion.path}>{metadataCompanion.path}</div>
              <div class="text-white/50">{frames?.length ?? 0} frames{metadataTags.length ? ` · ${metadataTags.length} tags` : ''}</div>
            </div>
            <button
              type="button"
              onclick={detachMetadata}
              disabled={metadataLoading}
              class="text-white/40 hover:text-danger disabled:opacity-40"
              title="Detach metadata + revert to manual grid"
            >Detach</button>
          </div>
        </div>
      {:else}
        <p class="mb-2 text-[10px] leading-snug text-white/50">
          Drop a sprite-atlas <span class="font-mono text-white/70">.json</span>
          (TexturePacker / Phaser / Aseprite export) to skip the manual slicer.
          Frame rects + animation tags come straight from the file.
        </p>
        <button
          type="button"
          onclick={() => metadataInput?.click()}
          disabled={metadataLoading}
          class="w-full rounded border border-white/15 bg-[#0f1117] px-2 py-1 text-[10px] text-white/80 hover:border-white/40 hover:text-white disabled:opacity-40"
        >{metadataLoading ? 'Uploading…' : 'Upload metadata JSON…'}</button>
        <input
          bind:this={metadataInput}
          type="file"
          accept=".json,application/json"
          class="hidden"
          onchange={onMetadataPick}
        />
      {/if}
      {#if metadataError}
        <div class="mt-2 rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">
          {metadataError}
        </div>
      {/if}
    </section>

    <!-- Tag picker (only when metadata supplies tag ranges) -->
    {#if metadataTags.length > 0}
      <section class="border-b border-white/10 p-3">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-white/40">Animations</h3>
        <div class="space-y-1">
          <button
            type="button"
            onclick={() => { activeTag = null; currentFrame = 0; pingDir = 1; }}
            class={`block w-full rounded border px-2 py-1 text-left text-[10px] ${activeTag === null ? 'border-accent bg-accent/20 text-white' : 'border-white/15 text-white/60 hover:border-white/40 hover:text-white'}`}
          >All frames <span class="float-right font-mono text-white/40">{frames?.length ?? 0}</span></button>
          {#each metadataTags as t (t.name)}
            <button
              type="button"
              onclick={() => {
                activeTag = t.name;
                currentFrame = 0;
                pingDir = 1;
                // Honour the tag's preferred direction if the
                // exporter set one — Aseprite tags carry forward /
                // reverse / pingpong, and "play the tag as authored"
                // beats "use the global loop pref" here.
                if (t.direction === 'pingpong') loopMode = 'pingpong';
                else if (t.direction === 'forward' || t.direction === 'reverse') loopMode = 'forward';
              }}
              class={`block w-full rounded border px-2 py-1 text-left text-[10px] ${activeTag === t.name ? 'border-accent bg-accent/20 text-white' : 'border-white/15 text-white/60 hover:border-white/40 hover:text-white'}`}
            >
              {t.name}
              <span class="float-right font-mono text-white/40">{t.from}\u2013{t.to}</span>
            </button>
          {/each}
        </div>
      </section>
    {/if}

    <!-- Slicer — only when no metadata is loaded; metadata mode owns
         frame definitions and the manual grid would just confuse. -->
    {#if !frames}
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
        <span>Grid <span class="font-mono text-white/70">{gridCols} × {gridRows}</span></span>
        <span>Total <span class="font-mono text-white/70">{gridCols * gridRows}</span></span>
      </div>
      <label class="mt-2 flex items-center gap-2 text-[10px]">
        <span class="text-white/50">Limit frames</span>
        <input
          type="number"
          min="1"
          max={gridCols * gridRows}
          value={frameCountOverride ?? ''}
          oninput={(e) => {
            const v = (e.currentTarget as HTMLInputElement).value;
            frameCountOverride = v === '' ? null : Math.max(1, Math.min(gridCols * gridRows, parseInt(v, 10) || 1));
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
    {:else}
    <!-- Metadata mode owns slicing; keep a "Show frame boxes" toggle
         so users can verify the JSON's coords against the artwork. -->
    <section class="border-b border-white/10 p-3">
      <label class="flex items-center justify-between text-[10px] text-white/60">
        <span>Show frame boxes</span>
        <input type="checkbox" bind:checked={showGrid} class="accent-accent" />
      </label>
    </section>
    {/if}

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
