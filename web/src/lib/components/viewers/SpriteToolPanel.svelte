<script lang="ts">
  // SpriteToolPanel — the right-pane half of the sprite viewer.
  // Reads + writes the shared session; AssetViewer renders this
  // in the outer right pane (same slot whiteboard tools use).
  // Sections, top to bottom:
  //
  //   1. Header (title + asset name)
  //   2. Display    — zoom presets, Fit, smoothing, background
  //   3. Metadata   — companion-JSON uploader / detach card
  //   4. Animations — tag picker (only when metadata supplies tags)
  //   5. Slice grid — manual slicer (only when no metadata)
  //   6. Playback   — prev/play/next, fps, loop mode

  import type { SpriteSessionInstance } from '$lib/sprite/session.svelte';

  let { session = $bindable<SpriteSessionInstance>() }: { session: SpriteSessionInstance } = $props();

  const ZOOM_PRESETS = [1, 2, 4, 8, 16, 24, 32] as const;

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

  let metadataInput: HTMLInputElement | undefined = $state();
  function onMetadataPick(e: Event) {
    const t = e.currentTarget as HTMLInputElement;
    const f = t.files?.[0];
    t.value = '';
    if (f) void session.uploadMetadataFile(f);
  }
</script>

<div class="flex h-full min-h-0 flex-col text-fg">
  <header class="flex shrink-0 items-center justify-between border-b border-border bg-surface-elevated px-3 py-2">
    <span class="text-sm font-semibold">Sprite</span>
    <span class="font-mono text-[10px] text-fg-muted">{session.imgW}×{session.imgH}</span>
  </header>

  <div class="min-h-0 flex-1 overflow-y-auto">
    <!-- Display -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Display</h3>
      <div class="space-y-2">
        <div>
          <span class="mb-1 block text-fg-muted">Zoom</span>
          <div class="flex flex-wrap gap-1">
            {#each ZOOM_PRESETS as z (z)}
              <button
                type="button"
                onclick={() => (session.zoom = z)}
                class={`rounded border px-2 py-0.5 text-[10px] ${session.zoom === z ? 'border-accent bg-accent/20 text-fg' : 'border-border text-fg-muted hover:border-fg-muted hover:text-fg'}`}
              >{z}×</button>
            {/each}
            <button
              type="button"
              onclick={() => session.pickFitZoom()}
              class="rounded border border-border px-2 py-0.5 text-[10px] text-fg-muted hover:border-fg-muted hover:text-fg"
            >Fit</button>
          </div>
        </div>
        <label class="flex items-center justify-between">
          <span class="text-fg-muted">Smoothing</span>
          <input type="checkbox" bind:checked={session.smoothing} class="accent-accent" />
        </label>
        <div>
          <span class="mb-1 block text-fg-muted">Background</span>
          <div class="flex gap-1">
            {#each [
              { id: 'checker' as const,     label: 'Checker' },
              { id: 'transparent' as const, label: 'None' },
              { id: 'solid' as const,       label: 'Solid' },
            ] as opt (opt.id)}
              <button
                type="button"
                onclick={() => (session.bg = opt.id)}
                class={`flex-1 rounded border px-2 py-0.5 text-[10px] ${session.bg === opt.id ? 'border-accent bg-accent/20 text-fg' : 'border-border text-fg-muted hover:border-fg-muted hover:text-fg'}`}
              >{opt.label}</button>
            {/each}
          </div>
          {#if session.bg === 'solid'}
            <input
              type="color"
              bind:value={session.bgSolid}
              class="mt-1 h-6 w-full cursor-pointer rounded border border-border bg-transparent"
            />
          {/if}
        </div>
      </div>
    </section>

    <!-- Image info — Sprite Analyzer Overview parity. Cheap one-
         pass stats the panel surfaces immediately on load so the
         user knows what they're working with (dims, pixel count,
         palette size, transparent-pixel %) without having to dig.
         -->
    {#if session.analysis}
      {@const a = session.analysis}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Image info</h3>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 text-[10px]">
          <dt class="text-fg-muted">Dimensions</dt>
          <dd class="font-mono text-fg">{a.width} × {a.height}</dd>
          <dt class="text-fg-muted">Total px</dt>
          <dd class="font-mono text-fg">{a.totalPixels.toLocaleString()}</dd>
          <dt class="text-fg-muted">Transparent</dt>
          <dd class="font-mono text-fg">{a.transparentPixels.toLocaleString()} <span class="text-fg-muted">({((a.transparentPixels / a.totalPixels) * 100).toFixed(1)}%)</span></dd>
          {#if a.semiTransparentPixels > 0}
            <dt class="text-fg-muted">Anti-aliased</dt>
            <dd class="font-mono text-fg">{a.semiTransparentPixels.toLocaleString()}</dd>
          {/if}
          <dt class="text-fg-muted">Unique colours</dt>
          <dd class="font-mono text-fg">{a.uniqueColors >= 4096 ? '4096+' : a.uniqueColors.toLocaleString()}</dd>
        </dl>
        {#if a.palette.length > 0}
          <!-- Palette swatches — first 64 entries in usage-
               frequency order. Hovers show hex + count. -->
          <div class="mt-2">
            <span class="mb-1 block text-[10px] text-fg-muted">Palette</span>
            <div class="flex flex-wrap gap-0.5">
              {#each a.palette.slice(0, 64) as p (`${p.r}-${p.g}-${p.b}-${p.a}`)}
                {@const hex = `#${[p.r, p.g, p.b].map((v) => v.toString(16).padStart(2, '0')).join('')}`}
                <div
                  class="h-4 w-4 rounded-sm border border-black/40"
                  style:background-color={hex}
                  title={`${hex} · ${p.count.toLocaleString()} px${p.a < 255 ? ` · α${p.a}` : ''}`}
                ></div>
              {/each}
              {#if a.palette.length > 64}
                <span class="text-[9px] text-fg-muted/70">+{a.palette.length - 64}</span>
              {/if}
            </div>
          </div>
        {/if}
      </section>
    {/if}

    <!-- Auto-detect — Sprite Splitter parity. Runs connected-
         component analysis on the sheet to find sprites; the
         output populates metadataFrames so the rest of the
         playback / preview pipeline picks it up automatically. -->
    {#if !session.metadataCompanion}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Auto detect</h3>
        <p class="mb-2 text-[10px] leading-snug text-fg-muted">
          Find sprites by connected-pixel regions — works on
          irregular sheets where a uniform grid can't.
        </p>
        <div class="space-y-2">
          <div>
            <span class="mb-1 block text-[10px] text-fg-muted">Background</span>
            <div class="flex gap-1">
              {#each [
                { id: 'alpha' as const, label: 'Alpha' },
                { id: 'color' as const, label: 'Colour' },
              ] as opt (opt.id)}
                <button
                  type="button"
                  onclick={() => (session.detectBgMode = opt.id)}
                  class={`flex-1 rounded border px-2 py-0.5 text-[10px] ${session.detectBgMode === opt.id ? 'border-accent bg-accent/20 text-fg' : 'border-border text-fg-muted hover:border-fg-muted hover:text-fg'}`}
                >{opt.label}</button>
              {/each}
            </div>
            {#if session.detectBgMode === 'color'}
              <div class="mt-1 flex items-center gap-2">
                <input
                  type="color"
                  bind:value={session.detectBgColor}
                  class="h-6 w-8 cursor-pointer rounded border border-border bg-transparent"
                />
                <label class="flex flex-1 items-center gap-1 text-[10px] text-fg-muted">
                  <span>Tol</span>
                  <input
                    type="number"
                    min="0"
                    max="255"
                    bind:value={session.detectBgTolerance}
                    class="w-12 rounded border border-border bg-surface px-1.5 py-0.5 text-fg"
                  />
                </label>
              </div>
            {/if}
          </div>
          <div class="grid grid-cols-2 gap-2">
            <label>
              <span class="mb-0.5 block text-fg-muted">Min W</span>
              <input type="number" min="1" bind:value={session.detectMinW} class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
            </label>
            <label>
              <span class="mb-0.5 block text-fg-muted">Min H</span>
              <input type="number" min="1" bind:value={session.detectMinH} class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
            </label>
            <label>
              <span class="mb-0.5 block text-fg-muted">Merge gap</span>
              <input type="number" min="0" bind:value={session.detectMergeGap} class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
            </label>
            <label>
              <span class="mb-0.5 block text-fg-muted">Sort</span>
              <select
                bind:value={session.detectSort}
                onchange={() => session.applySort()}
                class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg"
              >
                <option value="position">Position</option>
                <option value="animationRows">Anim. rows</option>
                <option value="sizeDesc">Size (big)</option>
                <option value="widthAsc">Width</option>
                <option value="heightAsc">Height</option>
              </select>
            </label>
          </div>
          <button
            type="button"
            onclick={() => session.runDetect()}
            class="w-full rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25"
          >Detect sprites</button>
          {#if session.detectedBoxes && session.detectedBoxes.length > 0}
            <div class="rounded border border-accent/40 bg-accent/10 p-2 text-[10px]">
              <div class="flex items-center justify-between">
                <span class="text-fg">{session.detectedBoxes.length} sprite{session.detectedBoxes.length === 1 ? '' : 's'} detected</span>
                <button
                  type="button"
                  onclick={() => {
                    session.detectedBoxes = null;
                    session.metadataFrames = null;
                    session.currentFrame = 0;
                  }}
                  class="text-fg-muted hover:text-danger"
                  title="Clear detection"
                >clear</button>
              </div>
              <button
                type="button"
                onclick={() => session.saveDetectedAsCompanion()}
                disabled={session.metadataLoading}
                class="mt-1.5 w-full rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-fg-muted disabled:opacity-40"
              >{session.metadataLoading ? 'Saving…' : 'Save as companion JSON'}</button>
            </div>
          {/if}
        </div>
      </section>
    {/if}

    <!-- Metadata sidecar -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Metadata</h3>
      {#if session.metadataCompanion}
        <div class="rounded border border-accent/40 bg-accent/10 p-2 text-[10px]">
          <div class="flex items-start justify-between gap-2">
            <div class="min-w-0 flex-1">
              <div class="truncate font-mono text-fg" title={session.metadataCompanion.path}>{session.metadataCompanion.path}</div>
              <div class="text-fg-muted">{session.metadataFrames?.length ?? 0} frames{session.metadataTags.length ? ` · ${session.metadataTags.length} tags` : ''}</div>
            </div>
            <button
              type="button"
              onclick={() => session.detachMetadata()}
              disabled={session.metadataLoading}
              class="text-fg-muted hover:text-danger disabled:opacity-40"
              title="Detach metadata + revert to manual grid"
            >Detach</button>
          </div>
        </div>
      {:else}
        <p class="mb-2 text-[10px] leading-snug text-fg-muted">
          Drop a sprite-atlas <span class="font-mono text-fg">.json</span>
          (TexturePacker / Phaser / Aseprite export) to skip manual slicing.
        </p>
        <button
          type="button"
          onclick={() => metadataInput?.click()}
          disabled={session.metadataLoading}
          class="w-full rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-fg-muted disabled:opacity-40"
        >{session.metadataLoading ? 'Uploading…' : 'Upload metadata JSON…'}</button>
        <input
          bind:this={metadataInput}
          type="file"
          accept=".json,application/json"
          class="hidden"
          onchange={onMetadataPick}
        />
      {/if}
      {#if session.metadataError}
        <div class="mt-2 rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">
          {session.metadataError}
        </div>
      {/if}
    </section>

    <!-- Tag picker (only when metadata supplies tag ranges) -->
    {#if session.metadataTags.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Animations</h3>
        <div class="space-y-1">
          <button
            type="button"
            onclick={() => { session.activeTag = null; session.currentFrame = 0; session.pingDir = 1; }}
            class={`block w-full rounded border px-2 py-1 text-left text-[10px] ${session.activeTag === null ? 'border-accent bg-accent/20 text-fg' : 'border-border text-fg-muted hover:border-fg-muted hover:text-fg'}`}
          >All frames <span class="float-right font-mono text-fg-muted">{session.metadataFrames?.length ?? 0}</span></button>
          {#each session.metadataTags as t (t.name)}
            <button
              type="button"
              onclick={() => {
                session.activeTag = t.name;
                session.currentFrame = 0;
                session.pingDir = 1;
                if (t.direction === 'pingpong') session.loopMode = 'pingpong';
                else if (t.direction === 'forward' || t.direction === 'reverse') session.loopMode = 'forward';
              }}
              class={`block w-full rounded border px-2 py-1 text-left text-[10px] ${session.activeTag === t.name ? 'border-accent bg-accent/20 text-fg' : 'border-border text-fg-muted hover:border-fg-muted hover:text-fg'}`}
            >
              {t.name}
              <span class="float-right font-mono text-fg-muted">{t.from}\u2013{t.to}</span>
            </button>
          {/each}
        </div>
      </section>
    {/if}

    <!-- Slice grid (manual mode) -->
    {#if !session.metadataFrames}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Slice grid</h3>
        {#if session.cellW === 0 || session.cellH === 0}
          <p class="mb-2 rounded border border-border bg-surface/60 px-2 py-1 text-[10px] leading-snug text-fg-muted">
            Set Cell W / H below if you know the sprite size, or hit <span class="font-mono text-fg">Detect sprites</span> above to find them automatically.
          </p>
        {/if}
        {#if session.imgW > 0 && session.imgH > 0}
          <!-- Always-on sheet-preview thumbnail. Shows the entire
               sprite sheet at fit-to-panel size with red cell
               borders + an accent highlight tracking the active
               frame. Replaces the old "Show grid" toggle that
               swapped the main canvas between single-frame and
               full-sheet views — now you see both at once. -->
          {@const previewMax = 240}
          {@const previewScale = Math.min(previewMax / session.imgW, previewMax / session.imgH)}
          {@const previewW = session.imgW * previewScale}
          {@const previewH = session.imgH * previewScale}
          <div class="mb-2 flex justify-center">
            <div class="relative sprite-checker inline-block" style:width={`${previewW}px`} style:height={`${previewH}px`}>
              <img
                src={`/api/v1/assets/${session.assetId}/file`}
                alt=""
                width={previewW}
                height={previewH}
                style:image-rendering="pixelated"
                class="block"
              />
              <!-- Grid lines + active-frame highlight via SVG so
                   the math stays trivially scalable. -->
              {#if session.cellW > 0 && session.cellH > 0}
                {@const activeIdx = (session.rangeStart ?? 0) + session.currentFrame}
                {@const activeC = activeIdx % gridCols}
                {@const activeR = Math.floor(activeIdx / gridCols)}
                <svg
                  class="pointer-events-none absolute inset-0"
                  width={previewW}
                  height={previewH}
                  viewBox={`0 0 ${session.imgW} ${session.imgH}`}
                  preserveAspectRatio="none"
                >
                  {#each Array(Math.max(2, gridCols + 1)) as _, c (`v${c}`)}
                    <line
                      x1={session.originX + c * (session.cellW + session.padX)}
                      y1={0}
                      x2={session.originX + c * (session.cellW + session.padX)}
                      y2={session.imgH}
                      stroke="rgba(255,100,100,0.7)"
                      stroke-width="0.5"
                    />
                  {/each}
                  {#each Array(Math.max(2, gridRows + 1)) as _, r (`h${r}`)}
                    <line
                      x1={0}
                      y1={session.originY + r * (session.cellH + session.padY)}
                      x2={session.imgW}
                      y2={session.originY + r * (session.cellH + session.padY)}
                      stroke="rgba(255,100,100,0.7)"
                      stroke-width="0.5"
                    />
                  {/each}
                  <rect
                    x={session.originX + activeC * (session.cellW + session.padX)}
                    y={session.originY + activeR * (session.cellH + session.padY)}
                    width={session.cellW}
                    height={session.cellH}
                    fill="none"
                    stroke="rgb(96,165,250)"
                    stroke-width="1"
                  />
                </svg>
              {/if}
            </div>
          </div>
        {/if}
        <div class="grid grid-cols-2 gap-2">
          <label>
            <span class="mb-0.5 block text-fg-muted">Cell W</span>
            <input type="number" bind:value={session.cellW} min="0" max={session.imgW} class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
          </label>
          <label>
            <span class="mb-0.5 block text-fg-muted">Cell H</span>
            <input type="number" bind:value={session.cellH} min="0" max={session.imgH} class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
          </label>
          <label>
            <span class="mb-0.5 block text-fg-muted">Origin X</span>
            <input type="number" bind:value={session.originX} min="0" class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
          </label>
          <label>
            <span class="mb-0.5 block text-fg-muted">Origin Y</span>
            <input type="number" bind:value={session.originY} min="0" class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
          </label>
          <label>
            <span class="mb-0.5 block text-fg-muted">Pad X</span>
            <input type="number" bind:value={session.padX} min="0" class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
          </label>
          <label>
            <span class="mb-0.5 block text-fg-muted">Pad Y</span>
            <input type="number" bind:value={session.padY} min="0" class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
          </label>
        </div>
        <div class="mt-2 flex items-center justify-between text-[10px] text-fg-muted">
          <span>Grid <span class="font-mono text-fg">{gridCols} × {gridRows}</span></span>
          <span>Total <span class="font-mono text-fg">{gridCols * gridRows}</span></span>
        </div>
        <!-- Sub-range picker. Lets the user play a slice of the
             grid (e.g. just frames 0–8 of a 9×6 sheet) without
             re-slicing the whole sheet. Defaults: 0 → end. -->
        <div class="mt-2 grid grid-cols-2 gap-2">
          <label>
            <span class="mb-0.5 block text-fg-muted">Start frame</span>
            <input
              type="number"
              min="0"
              max={Math.max(0, gridCols * gridRows - 1)}
              value={session.rangeStart}
              oninput={(e) => {
                const v = (e.currentTarget as HTMLInputElement).value;
                const total = gridCols * gridRows;
                session.rangeStart = v === '' ? 0 : Math.max(0, Math.min(total - 1, parseInt(v, 10) || 0));
                if (session.currentFrame !== 0) session.currentFrame = 0;
              }}
              class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg"
            />
          </label>
          <label>
            <span class="mb-0.5 block text-fg-muted">End frame</span>
            <input
              type="number"
              min="0"
              max={Math.max(0, gridCols * gridRows - 1)}
              value={session.rangeEnd ?? (gridCols * gridRows - 1)}
              oninput={(e) => {
                const v = (e.currentTarget as HTMLInputElement).value;
                const total = gridCols * gridRows;
                const parsed = v === '' ? total - 1 : Math.max(0, Math.min(total - 1, parseInt(v, 10) || 0));
                session.rangeEnd = parsed === total - 1 ? null : parsed;
                if (session.currentFrame !== 0) session.currentFrame = 0;
              }}
              class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg"
            />
          </label>
        </div>
        <!-- Quick row picker — sets start/end to span a single
             grid row, the common "play only the red character"
             gesture. -->
        {#if gridRows > 1}
          <div class="mt-2">
            <span class="mb-1 block text-[10px] text-fg-muted">Play one row</span>
            <div class="flex flex-wrap gap-1">
              {#each Array(gridRows) as _, r (r)}
                <button
                  type="button"
                  onclick={() => {
                    session.rangeStart = r * gridCols;
                    session.rangeEnd = (r + 1) * gridCols - 1;
                    session.currentFrame = 0;
                  }}
                  class="rounded border border-border bg-surface px-2 py-0.5 text-[10px] text-fg-muted hover:border-fg-muted hover:text-fg"
                >Row {r + 1}</button>
              {/each}
              <button
                type="button"
                onclick={() => {
                  session.rangeStart = 0;
                  session.rangeEnd = null;
                  session.currentFrame = 0;
                }}
                class="rounded border border-border bg-surface px-2 py-0.5 text-[10px] text-fg-muted hover:border-fg-muted hover:text-fg"
              >All</button>
            </div>
          </div>
        {/if}
      </section>
    {/if}

    <!-- Playback -->
    <section class="p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Playback</h3>
      <div class="space-y-2">
        <div class="flex items-center gap-1">
          <button
            type="button"
            onclick={() => {
              session.stepFrame();
              // step backward by stepping forward n-2 times — cheap
              // for typical frame counts. Cleaner than duplicating the
              // direction math in two places.
              const back = (session.metadataFrames?.length ?? gridCols * gridRows) - 2;
              for (let i = 0; i < back; i++) session.stepFrame();
              session.playing = false;
            }}
            class="rounded border border-border px-2 py-0.5 text-fg-muted hover:border-fg-muted hover:text-fg"
            title="Previous frame"
            aria-label="Previous frame"
          >‹</button>
          <button
            type="button"
            onclick={() => (session.playing = !session.playing)}
            class="flex-1 rounded border border-border px-2 py-1 text-fg hover:border-fg-muted"
          >{session.playing ? '⏸ Pause' : '▶ Play'}</button>
          <button
            type="button"
            onclick={() => { session.stepFrame(); session.playing = false; }}
            class="rounded border border-border px-2 py-0.5 text-fg-muted hover:border-fg-muted hover:text-fg"
            title="Next frame"
            aria-label="Next frame"
          >›</button>
        </div>
        <label class="block">
          <span class="mb-0.5 flex justify-between text-fg-muted">
            <span>FPS</span><span class="font-mono text-fg">{session.fps.toFixed(1)}</span>
          </span>
          <input type="range" min="0.5" max="60" step="0.5" bind:value={session.fps} class="w-full accent-accent" />
        </label>
        <div>
          <span class="mb-1 block text-fg-muted">Loop</span>
          <div class="flex gap-1">
            {#each [
              { id: 'forward' as const,  label: 'Forward' },
              { id: 'pingpong' as const, label: 'Ping-pong' },
            ] as opt (opt.id)}
              <button
                type="button"
                onclick={() => { session.loopMode = opt.id; session.pingDir = 1; }}
                class={`flex-1 rounded border px-2 py-0.5 text-[10px] ${session.loopMode === opt.id ? 'border-accent bg-accent/20 text-fg' : 'border-border text-fg-muted hover:border-fg-muted hover:text-fg'}`}
              >{opt.label}</button>
            {/each}
          </div>
        </div>
      </div>
    </section>

    <!-- Tips — quick reference for sprite-specific gestures.
         Sticks at the bottom of the scrollable panel area so it's
         always findable. Hotkeys live in this list even if not all
         are wired yet; the panel section becomes the spec for what
         we ship in the editor phase. -->
    <section class="p-3 text-xs">
      <details class="rounded border border-border bg-surface-elevated">
        <summary class="cursor-pointer px-2 py-1 text-[10px] font-medium uppercase tracking-wider text-fg-muted hover:text-fg">Tips & shortcuts</summary>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 px-2 pb-2 pt-1 text-[10px]">
          <dt class="font-mono text-fg">Space</dt><dd class="text-fg-muted">Play / pause</dd>
          <dt class="font-mono text-fg">,</dt><dd class="text-fg-muted">Previous frame</dd>
          <dt class="font-mono text-fg">.</dt><dd class="text-fg-muted">Next frame</dd>
          <dt class="col-span-2 mt-1 text-fg-muted/70">Slicing</dt>
          <dt class="font-mono text-fg">Cell W / H</dt><dd class="text-fg-muted">Sprite size in pixels — auto-guessed from detection on load</dd>
          <dt class="font-mono text-fg">Start / End</dt><dd class="text-fg-muted">Frame range to loop — narrow to one row of a multi-section sheet</dd>
          <dt class="col-span-2 mt-1 text-fg-muted/70">Auto-detect</dt>
          <dt class="font-mono text-fg">Alpha</dt><dd class="text-fg-muted">Use the PNG's transparency channel (most pixel-art sheets)</dd>
          <dt class="font-mono text-fg">Colour</dt><dd class="text-fg-muted">Treat a single colour as background — for sheets without alpha</dd>
          <dt class="font-mono text-fg">Merge gap</dt><dd class="text-fg-muted">Glue separated pixels (floating hats, accessories) into one sprite</dd>
          <dt class="col-span-2 mt-1 text-fg-muted/70">Save</dt>
          <dt class="font-mono text-fg">Companion JSON</dt><dd class="text-fg-muted">Persists frames + tags as a sidecar — reloads pick them up automatically</dd>
        </dl>
      </details>
    </section>
  </div>
</div>
