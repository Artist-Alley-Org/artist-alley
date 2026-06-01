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

  import { onMount } from 'svelte';
  import type { SpriteSessionInstance } from '$lib/sprite/session.svelte';
  import { exportGIF, exportSpriteSheet, exportPNGsZip, downloadBlob, type ExportFrame } from '$lib/sprite/export';
  import { applyPaletteRemap, entryToHex, parseHexColor, type PaletteEntry, type RemapPair } from '$lib/sprite/palette';
  import { listAlternates, addAlternate, removeAlternate, alternateDownloadURL, type Alternate } from '$lib/sprite/alternates';

  let { session = $bindable<SpriteSessionInstance>() }: { session: SpriteSessionInstance } = $props();

  // ── Export ────────────────────────────────────────────────────
  // Build the export-frame list from whichever source is
  // authoritative — companion metadata when loaded, the manual
  // slicer otherwise. Honours the user's start/end range so a
  // partial selection exports only what they're playing.
  function buildExportFrames(): ExportFrame[] {
    const frames = session.metadataFrames;
    if (frames && frames.length > 0) {
      // Active tag wins (explicit pick); otherwise honour the
      // user's rangeStart/rangeEnd selection (drag-selected in the
      // timeline OR typed in the slicer's Start/End fields).
      let from = 0;
      let to = frames.length - 1;
      if (session.activeTag) {
        const t = session.metadataTags.find((x) => x.name === session.activeTag);
        if (t) { from = t.from; to = t.to; }
      } else {
        from = Math.max(0, Math.min(frames.length - 1, session.rangeStart));
        to = Math.max(from, Math.min(frames.length - 1, session.rangeEnd ?? frames.length - 1));
      }
      const out: ExportFrame[] = [];
      for (let i = from; i <= to; i++) {
        const f = frames[i];
        out.push({
          sx: f.sx, sy: f.sy, sw: f.sw, sh: f.sh,
          duration: f.duration,
          flipH: f.flipH, flipV: f.flipV, rotate: f.rotate,
        });
      }
      return out;
    }
    const cols = gridCols;
    const rows = gridRows;
    const total = cols * rows;
    if (session.cellW <= 0 || session.cellH <= 0 || total <= 0) return [];
    const from = Math.max(0, Math.min(total - 1, session.rangeStart));
    const to = Math.max(from, Math.min(total - 1, session.rangeEnd ?? total - 1));
    const out: ExportFrame[] = [];
    for (let i = from; i <= to; i++) {
      const c = i % cols;
      const r = Math.floor(i / cols);
      out.push({
        sx: session.originX + c * (session.cellW + session.padX),
        sy: session.originY + r * (session.cellH + session.padY),
        sw: session.cellW,
        sh: session.cellH,
      });
    }
    return out;
  }

  let exportBusy = $state(false);
  let exportError = $state<string | null>(null);
  let exportScale = $state(1);
  let exportProgress = $state<{ done: number; total: number } | null>(null);

  // Tag composer — user types a name, picks a direction, saves
  // the current rangeStart..rangeEnd window as a tag with that
  // name. Persistence to the companion JSON is a separate explicit
  // button so users can stage multiple tags before round-tripping.
  let newTagName = $state('');
  let newTagDirection = $state<'forward' | 'reverse' | 'pingpong'>('forward');
  function onSaveTag() {
    const name = newTagName.trim();
    if (!name) return;
    session.addTag(name, newTagDirection);
    newTagName = '';
  }

  // Slice composer — name + Add. Bounds default at the session
  // factory; user adjusts via the per-slice editor row.
  let newSliceName = $state('');
  function onAddSlice() {
    const name = newSliceName.trim();
    if (!name) return;
    session.addSlice(name);
    newSliceName = '';
  }

  // ── Palette remap (Phase 9) ──────────────────────────────────
  // Staged colour-swap pairs. User picks a source from the
  // palette grid then a target via <input type=color>; we don't
  // touch the source PNG — applying writes a NEW PNG as an
  // alt-file on the asset (the alternates panel below lists them).
  let remapPairs = $state<RemapPair[]>([]);
  let pendingSource = $state<PaletteEntry | null>(null);
  let pendingTargetHex = $state('#ff00ff');
  let altLabel = $state('');
  let altBusy = $state(false);
  let altError = $state<string | null>(null);
  function pickPaletteSource(e: PaletteEntry) {
    pendingSource = { r: e.r, g: e.g, b: e.b, a: e.a };
    pendingTargetHex = entryToHex(e);
  }
  function commitRemapPair() {
    if (!pendingSource) return;
    const t = parseHexColor(pendingTargetHex);
    if (!t) return;
    // Replace any existing pair targeting the same source so the
    // user can refine a mapping without manually removing first.
    const next = remapPairs.filter(
      (p) => !(p.from.r === pendingSource!.r && p.from.g === pendingSource!.g && p.from.b === pendingSource!.b),
    );
    next.push({ from: pendingSource, to: { ...t, a: pendingSource.a } });
    remapPairs = next;
    pendingSource = null;
  }
  function removeRemapPair(i: number) {
    remapPairs = remapPairs.filter((_, idx) => idx !== i);
  }

  // ── Alternates (Phase 9) ─────────────────────────────────────
  let alternates = $state<Alternate[]>([]);
  let alternatesError = $state<string | null>(null);
  async function refreshAlternates() {
    try {
      alternates = await listAlternates(session.assetId);
    } catch (e) {
      alternatesError = e instanceof Error ? e.message : String(e);
    }
  }
  async function applyAndSaveRemap() {
    if (!session.img || remapPairs.length === 0) return;
    const label = altLabel.trim() || `palette swap ${new Date().toISOString().slice(0, 19).replace('T', ' ')}`;
    altBusy = true;
    altError = null;
    try {
      const blob = await applyPaletteRemap(session.img, remapPairs);
      await addAlternate({
        assetId: session.assetId,
        label,
        kind: 'palette_swap',
        contentType: 'image/png',
        metadata: {
          remap: remapPairs.map((p) => ({
            from: entryToHex(p.from),
            to: entryToHex(p.to),
          })),
        },
        body: blob,
      });
      await refreshAlternates();
      remapPairs = [];
      altLabel = '';
    } catch (e) {
      altError = e instanceof Error ? e.message : String(e);
    } finally {
      altBusy = false;
    }
  }
  async function deleteAlt(id: string) {
    try {
      await removeAlternate(session.assetId, id);
      await refreshAlternates();
    } catch (e) {
      alternatesError = e instanceof Error ? e.message : String(e);
    }
  }
  onMount(() => { void refreshAlternates(); });

  async function doExportGIF() {
    if (!session.img) return;
    exportBusy = true;
    exportError = null;
    exportProgress = { done: 0, total: 0 };
    try {
      const frames = buildExportFrames();
      exportProgress = { done: 0, total: frames.length };
      const defaultMs = 1000 / Math.max(0.1, session.fps);
      const blob = await exportGIF(session.img, frames, {
        defaultFrameMs: defaultMs,
        scale: exportScale,
        onProgress: (done, total) => { exportProgress = { done, total }; },
      });
      downloadBlob(blob, `${session.assetId}-sprite.gif`);
    } catch (e) {
      exportError = e instanceof Error ? e.message : 'GIF export failed';
    } finally {
      exportBusy = false;
      exportProgress = null;
    }
  }
  async function doExportSheet() {
    if (!session.img) return;
    exportBusy = true;
    exportError = null;
    try {
      const frames = buildExportFrames();
      const { png, json } = await exportSpriteSheet(session.img, frames);
      downloadBlob(png, `${session.assetId}-sheet.png`);
      downloadBlob(json, `${session.assetId}-sheet.json`);
    } catch (e) {
      exportError = e instanceof Error ? e.message : 'Sheet export failed';
    } finally {
      exportBusy = false;
    }
  }
  async function doExportZip() {
    if (!session.img) return;
    exportBusy = true;
    exportError = null;
    try {
      const frames = buildExportFrames();
      const blob = await exportPNGsZip(session.img, frames);
      downloadBlob(blob, `${session.assetId}-frames.zip`);
    } catch (e) {
      exportError = e instanceof Error ? e.message : 'Zip export failed';
    } finally {
      exportBusy = false;
    }
  }

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
               frequency order. Click to stage a colour as the
               source of a remap pair (Phase 9). Hovers show hex
               + count. -->
          <div class="mt-2">
            <span class="mb-1 block text-[10px] text-fg-muted">Palette</span>
            <div class="flex flex-wrap gap-0.5">
              {#each a.palette.slice(0, 64) as p (`${p.r}-${p.g}-${p.b}-${p.a}`)}
                {@const hex = entryToHex(p)}
                <button
                  type="button"
                  class={`h-4 w-4 rounded-sm border ${pendingSource && pendingSource.r === p.r && pendingSource.g === p.g && pendingSource.b === p.b ? 'border-accent ring-1 ring-accent' : 'border-black/40 hover:border-fg'}`}
                  style:background-color={hex}
                  title={`${hex} · ${p.count.toLocaleString()} px${p.a < 255 ? ` · α${p.a}` : ''} · click to stage palette swap`}
                  onclick={() => pickPaletteSource(p)}
                  aria-label={`Stage ${hex} as remap source`}
                ></button>
              {/each}
              {#if a.palette.length > 64}
                <span class="text-[9px] text-fg-muted/70">+{a.palette.length - 64}</span>
              {/if}
            </div>
          </div>
        {/if}
      </section>
    {/if}

    <!-- Palette swap — Phase 9. Re-renders the full sheet with one
         or more colour remaps applied and saves the result as a new
         alternate file on the asset. Source PNG stays untouched —
         this is a sibling variant, not an edit. -->
    {#if session.img && session.analysis && session.analysis.palette.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Palette swap</h3>
        <p class="mb-2 text-[10px] leading-snug text-fg-muted">
          Click a palette swatch above to pick a source colour, then choose a target and add the pair. Apply writes a remapped PNG as an alternate file on this asset.
        </p>
        {#if pendingSource}
          <div class="mb-2 flex items-center gap-2 rounded border border-accent/40 bg-accent/10 px-2 py-1">
            <span class="h-4 w-4 shrink-0 rounded border border-black/40" style:background-color={entryToHex(pendingSource)}></span>
            <span class="font-mono text-[10px] text-fg">{entryToHex(pendingSource)}</span>
            <span class="text-[10px] text-fg-muted">→</span>
            <input
              type="color"
              bind:value={pendingTargetHex}
              class="h-5 w-6 rounded border border-border"
              aria-label="Target colour"
            />
            <input
              type="text"
              bind:value={pendingTargetHex}
              class="flex-1 rounded border border-border bg-surface px-1 py-0.5 font-mono text-[10px] text-fg"
              maxlength="7"
            />
            <button type="button" onclick={commitRemapPair} class="rounded border border-accent bg-accent/15 px-2 py-0.5 text-[10px] text-fg hover:bg-accent/25">Add</button>
            <button type="button" onclick={() => (pendingSource = null)} class="text-fg-muted hover:text-danger" title="Cancel" aria-label="Cancel">×</button>
          </div>
        {/if}
        {#if remapPairs.length > 0}
          <div class="mb-2 space-y-0.5">
            {#each remapPairs as p, i (i)}
              <div class="flex items-center gap-1 rounded border border-border px-1.5 py-0.5">
                <span class="h-3 w-3 rounded-sm border border-black/40" style:background-color={entryToHex(p.from)}></span>
                <span class="font-mono text-[10px] text-fg">{entryToHex(p.from)}</span>
                <span class="text-[10px] text-fg-muted">→</span>
                <span class="h-3 w-3 rounded-sm border border-black/40" style:background-color={entryToHex(p.to)}></span>
                <span class="flex-1 font-mono text-[10px] text-fg">{entryToHex(p.to)}</span>
                <button type="button" onclick={() => removeRemapPair(i)} class="text-fg-muted hover:text-danger" aria-label="Remove pair">×</button>
              </div>
            {/each}
          </div>
          <input
            type="text"
            bind:value={altLabel}
            placeholder="Label (defaults to a timestamp)…"
            class="mb-1 w-full rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg focus:border-accent focus:outline-none"
          />
          <button
            type="button"
            onclick={applyAndSaveRemap}
            disabled={altBusy}
            class="w-full rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25 disabled:opacity-40"
          >{altBusy ? 'Saving…' : `Apply + save (${remapPairs.length} pair${remapPairs.length === 1 ? '' : 's'})`}</button>
        {/if}
        {#if altError}
          <p class="mt-1 text-[10px] text-danger">{altError}</p>
        {/if}
      </section>
    {/if}

    <!-- Alternates — Phase 9. Lists sibling-variant files attached
         to this asset (palette swaps so far; future paint-track
         output lands here too). Each row links to the raw bytes
         and offers a delete. -->
    {#if alternates.length > 0 || alternatesError}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Alternates</h3>
        {#if alternatesError}
          <p class="mb-2 text-[10px] text-danger">{alternatesError}</p>
        {/if}
        <div class="space-y-1">
          {#each alternates as alt (alt.id)}
            <div class="flex items-center gap-2 rounded border border-border p-1.5">
              <div class="flex-1 min-w-0">
                <div class="truncate text-[10px] text-fg" title={alt.label}>{alt.label}</div>
                <div class="font-mono text-[9px] text-fg-muted">{alt.kind} · {(alt.size_bytes / 1024).toFixed(1)} KB</div>
              </div>
              <a
                href={alternateDownloadURL(alt.asset_id, alt.id)}
                download={`${alt.label}.png`}
                class="rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg hover:border-accent"
              >↓</a>
              <button
                type="button"
                onclick={() => deleteAlt(alt.id)}
                class="rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg-muted hover:border-danger hover:text-danger"
                aria-label="Delete alternate"
              >×</button>
            </div>
          {/each}
        </div>
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
              <div class="text-fg-muted">
                {session.metadataFrames?.length ?? 0} frames{session.metadataTags.length ? ` · ${session.metadataTags.length} tags` : ''}{session.slices.length ? ` · ${session.slices.length} slices` : ''}
              </div>
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
      <!-- Single consolidated save. Persists frames + tags +
           slices together (one companion JSON owns all three).
           Shown whenever the user has in-memory metadata to
           save, regardless of whether the source is auto-detect
           or a previously-loaded companion. The two old
           per-section buttons (one in Animations, one in Slices)
           that confusingly both saved everything are retired. -->
      {#if session.metadataFrames && session.metadataFrames.length > 0}
        <button
          type="button"
          onclick={() => session.saveTagsAsCompanion()}
          disabled={session.metadataLoading}
          class="mt-2 w-full rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent disabled:opacity-40"
        >{session.metadataLoading ? 'Saving\u2026' : 'Save metadata to companion JSON'}</button>
      {/if}
    </section>

    <!-- Animations — picker + editor for named frame ranges. The
         user's flow: drag-select a window in the timeline strip
         (or set Start / End in the slicer) → type a name → "Save
         as tag" → range is named + persists to the companion JSON
         on the next save. Always shows when there are frames to
         tag (not just when companion-supplied tags exist), so
         users can author tags from scratch. -->
    {#if session.metadataFrames && session.metadataFrames.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Animations</h3>
        <div class="space-y-1">
          <button
            type="button"
            onclick={() => { session.activeTag = null; session.currentFrame = 0; session.pingDir = 1; }}
            class={`block w-full rounded border px-2 py-1 text-left text-[10px] ${session.activeTag === null ? 'border-accent bg-accent/20 text-fg' : 'border-border text-fg-muted hover:border-fg-muted hover:text-fg'}`}
          >All frames <span class="float-right font-mono text-fg-muted">{session.metadataFrames?.length ?? 0}</span></button>
          {#each session.metadataTags as t (t.name)}
            <div class={`group flex items-center gap-1 rounded border ${session.activeTag === t.name ? 'border-accent bg-accent/20' : 'border-border'}`}>
              <button
                type="button"
                onclick={() => {
                  session.activeTag = t.name;
                  session.currentFrame = 0;
                  session.pingDir = 1;
                  if (t.direction === 'pingpong') session.loopMode = 'pingpong';
                  else if (t.direction === 'forward' || t.direction === 'reverse') session.loopMode = 'forward';
                }}
                class="flex-1 px-2 py-1 text-left text-[10px] text-fg hover:text-fg"
              >
                {t.name}
                <span class="float-right font-mono text-fg-muted">{t.from}–{t.to}</span>
              </button>
              <button
                type="button"
                onclick={() => { session.removeTag(t.name); }}
                class="px-1.5 py-1 text-[10px] text-fg-muted opacity-0 hover:text-danger group-hover:opacity-100"
                title="Delete tag"
                aria-label="Delete tag"
              >×</button>
            </div>
          {/each}
        </div>
        <!-- Compose-tag form: name + direction + save button.
             Uses whatever the user's currently-selected range is
             (rangeStart..rangeEnd) so the workflow lines up with
             the drag-select gesture in the timeline strip. -->
        <div class="mt-3 space-y-1.5 border-t border-border pt-2">
          <p class="text-[10px] leading-snug text-fg-muted">
            Drag-select a range in the timeline strip below, then
            name + save it as a tag.
          </p>
          <div class="flex items-center gap-1">
            <input
              type="text"
              bind:value={newTagName}
              placeholder="Tag name…"
              class="flex-1 rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg focus:border-accent focus:outline-none"
              onkeydown={(e) => { if (e.key === 'Enter') onSaveTag(); }}
            />
            <select
              bind:value={newTagDirection}
              class="rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg"
            >
              <option value="forward">→</option>
              <option value="reverse">←</option>
              <option value="pingpong">↔</option>
            </select>
          </div>
          <button
            type="button"
            onclick={onSaveTag}
            disabled={!newTagName.trim() || session.metadataLoading}
            class="w-full rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25 disabled:opacity-40"
          >Save range as tag</button>
          <!-- Persist button retired — single "Save metadata to
               companion JSON" in the Metadata section above covers
               both tags + slices in one round-trip. -->
        </div>
      </section>
    {/if}

    <!-- Frame ops — Phase 8. Per-frame manipulation: reorder,
         delete, duplicate, retime, mirror, flip, rotate-90°.
         Operates on session.currentFrame (the tile selected in
         the timeline strip). All transforms are metadata — the
         source PNG pixels stay untouched. Tags remap automatically
         when frames are inserted / deleted; the user is expected
         to verify after a reorder since tags reference positions
         (no stable per-frame IDs in the model yet). -->
    {#if session.metadataFrames && session.metadataFrames.length > 0}
      {@const frames = session.metadataFrames}
      {@const cur = Math.max(0, Math.min(frames.length - 1, session.currentFrame))}
      {@const curFrame = frames[cur]}
      <section class="border-b border-border p-3 text-xs">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Frame ops</h3>
          <span class="font-mono text-[10px] text-fg-muted">{cur + 1} / {frames.length}</span>
        </div>
        <p class="mb-2 truncate font-mono text-[10px] text-fg-muted" title={curFrame.name}>
          {curFrame.name}
        </p>
        <div class="mb-2 grid grid-cols-4 gap-1">
          <button type="button" onclick={() => session.moveFrame(cur, cur - 1)} disabled={cur <= 0} class="rounded border border-border bg-surface px-1 py-1 text-fg hover:border-accent disabled:opacity-30">←</button>
          <button type="button" onclick={() => session.moveFrame(cur, cur + 1)} disabled={cur >= frames.length - 1} class="rounded border border-border bg-surface px-1 py-1 text-fg hover:border-accent disabled:opacity-30">→</button>
          <button type="button" onclick={() => session.duplicateFrame(cur)} class="rounded border border-border bg-surface px-1 py-1 text-fg hover:border-accent" title="Duplicate this frame">⎘</button>
          <button type="button" onclick={() => session.deleteFrame(cur)} class="rounded border border-border bg-surface px-1 py-1 text-fg hover:border-danger hover:text-danger" title="Delete this frame">×</button>
        </div>
        <div class="mb-2 grid grid-cols-3 gap-1">
          <button
            type="button"
            onclick={() => session.setFrameTransform(cur, { flipH: !curFrame.flipH })}
            class={`rounded border px-1 py-1 ${curFrame.flipH ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
            title="Flip horizontally"
          >⇄</button>
          <button
            type="button"
            onclick={() => session.setFrameTransform(cur, { flipV: !curFrame.flipV })}
            class={`rounded border px-1 py-1 ${curFrame.flipV ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
            title="Flip vertically"
          >⇅</button>
          <button
            type="button"
            onclick={() => {
              const r = curFrame.rotate ?? 0;
              const next = ((r + 90) % 360) as 0 | 90 | 180 | 270;
              session.setFrameTransform(cur, { rotate: next });
            }}
            class={`rounded border px-1 py-1 ${curFrame.rotate ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
            title={`Rotate 90° (currently ${curFrame.rotate ?? 0}°)`}
          >↻ {curFrame.rotate ?? 0}°</button>
        </div>
        <label class="block">
          <span class="mb-0.5 flex justify-between text-fg-muted">
            <span>Duration (ms)</span>
            <span class="font-mono text-fg">{curFrame.duration ? `${curFrame.duration}` : 'default'}</span>
          </span>
          <input
            type="number"
            min="0"
            step="10"
            value={curFrame.duration ?? 0}
            oninput={(e) => session.setFrameDuration(cur, Number((e.currentTarget as HTMLInputElement).value))}
            class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg"
          />
          <span class="mt-0.5 block text-[10px] text-fg-muted">0 = use session FPS default.</span>
        </label>
        <!-- Phase 10 brainstorm note. Short free-form annotation
             shown as a hover marker on the timeline tile. Persists
             to the companion JSON under `note`. -->
        <label class="mt-2 block">
          <span class="mb-0.5 flex justify-between text-fg-muted">
            <span>Note</span>
            {#if curFrame.note}<span class="font-mono text-[9px] text-accent">●</span>{/if}
          </span>
          <textarea
            value={curFrame.note ?? ''}
            oninput={(e) => session.setFrameNote(cur, (e.currentTarget as HTMLTextAreaElement).value)}
            placeholder="Brainstorm note for this frame…"
            class="h-12 w-full resize-none rounded border border-border bg-surface px-1.5 py-1 text-[10px] text-fg focus:border-accent focus:outline-none"
          ></textarea>
        </label>
        <!-- Phase 11 trim. Per-frame shrinks the source rect to its
             non-transparent bounding box; bulk trims every frame.
             Pure metadata — source PNG pixels never change. Fully
             transparent frames are no-ops (don't collapse to zero). -->
        <div class="mt-2 grid grid-cols-2 gap-1">
          <button
            type="button"
            onclick={() => session.trimFrame(cur)}
            class="rounded border border-border bg-surface px-2 py-1 text-fg hover:border-accent"
            title="Shrink this frame's rect to its non-transparent bounds"
          >Trim frame</button>
          <button
            type="button"
            onclick={() => session.trimAllFrames()}
            class="rounded border border-border bg-surface px-2 py-1 text-fg hover:border-accent"
            title="Trim every frame"
          >Trim all</button>
        </div>
        <button
          type="button"
          onclick={() => session.pinToLightbox(cur)}
          disabled={session.lightboxPins.includes(cur)}
          class="mt-2 w-full rounded border border-border bg-surface px-2 py-1 text-fg hover:border-accent disabled:opacity-40"
          title="Add this frame to the lightbox comparison"
        >Pin to lightbox</button>
      </section>
    {/if}

    <!-- Lightbox — Phase 8 review surface. When enabled, the main
         canvas pane swaps from single-frame view to a comparison
         view of all pinned frames (side-by-side / stacked /
         XOR-diff). This is animation-review brainstorm material —
         pins are session-local, never saved to the companion JSON. -->
    {#if session.metadataFrames && session.metadataFrames.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Lightbox</h3>
          <label class="flex items-center gap-1 text-[10px] text-fg-muted">
            <input type="checkbox" bind:checked={session.lightboxEnabled} class="accent-accent" />
            On
          </label>
        </div>
        {#if session.lightboxEnabled}
          <div class="mb-2 grid grid-cols-3 gap-1">
            <button type="button" onclick={() => (session.lightboxMode = 'side')} class={`rounded border px-1 py-1 ${session.lightboxMode === 'side' ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}>Side</button>
            <button type="button" onclick={() => (session.lightboxMode = 'stack')} class={`rounded border px-1 py-1 ${session.lightboxMode === 'stack' ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}>Stack</button>
            <button type="button" onclick={() => (session.lightboxMode = 'diff')} class={`rounded border px-1 py-1 ${session.lightboxMode === 'diff' ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}>Diff</button>
          </div>
          {#if session.lightboxMode === 'stack'}
            <label class="mb-2 block">
              <span class="mb-0.5 flex justify-between text-fg-muted">
                <span>Overlay opacity</span><span class="font-mono text-fg">{Math.round(session.lightboxStackOpacity * 100)}%</span>
              </span>
              <input type="range" min="0.05" max="1" step="0.05" bind:value={session.lightboxStackOpacity} class="w-full accent-accent" />
            </label>
          {/if}
        {/if}
        <div class="space-y-1">
          {#if session.lightboxPins.length === 0}
            <p class="rounded border border-border bg-surface/60 px-2 py-1 text-[10px] leading-snug text-fg-muted">
              No frames pinned. Use <span class="font-mono text-fg">Pin to lightbox</span> in Frame ops, or pin from the timeline strip.
            </p>
          {:else}
            {#each session.lightboxPins as p (p)}
              <div class="flex items-center gap-1 rounded border border-border px-2 py-1">
                <span class="flex-1 font-mono text-[10px] text-fg-muted">Frame {p + 1}</span>
                <button type="button" onclick={() => session.unpinFromLightbox(p)} class="text-fg-muted hover:text-danger" title="Unpin">×</button>
              </div>
            {/each}
            <button type="button" onclick={() => session.clearLightbox()} class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 text-fg-muted hover:border-fg-muted hover:text-fg">Clear all pins</button>
          {/if}
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

    <!-- Slices — Aseprite-style region annotations on the sheet.
         9-patch / pivot / bounds, all persisted as part of the
         companion JSON's meta.slices array. Always shown when a
         sheet is loaded; works with or without companion-metadata
         frames. -->
    {#if session.img}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Slices</h3>
        {#if session.slices.length === 0}
          <p class="mb-2 text-[10px] leading-snug text-fg-muted">
            Region annotations on the sheet — useful for hit-box,
            9-patch UI, or pivot anchors. Hover the canvas to see
            each slice; edit bounds in the list below.
          </p>
        {/if}
        <div class="space-y-1.5">
          {#each session.slices as s (s.name)}
            {@const active = session.activeSlice === s.name}
            <div class={`rounded border ${active ? 'border-accent bg-accent/10' : 'border-border'} p-1.5`}>
              <div class="flex items-center gap-1">
                <button
                  type="button"
                  onclick={() => (session.activeSlice = active ? null : s.name)}
                  class="h-3 w-3 rounded-sm border border-black/40"
                  style:background-color={s.color ?? '#00ff00'}
                  title="Toggle highlight"
                  aria-label="Slice colour swatch"
                ></button>
                <input
                  type="text"
                  value={s.name}
                  onchange={(e) => session.updateSlice(s.name, { ...s, name: (e.currentTarget as HTMLInputElement).value })}
                  class="flex-1 rounded border border-border bg-surface px-1 py-0.5 text-[10px] text-fg focus:border-accent focus:outline-none"
                />
                <button
                  type="button"
                  onclick={() => session.removeSlice(s.name)}
                  class="text-[10px] text-fg-muted hover:text-danger"
                  title="Delete slice"
                  aria-label="Delete slice"
                >×</button>
              </div>
              <div class="mt-1 grid grid-cols-4 gap-1 text-[10px]">
                {#each [
                  { k: 'x' as const, label: 'X' },
                  { k: 'y' as const, label: 'Y' },
                  { k: 'w' as const, label: 'W' },
                  { k: 'h' as const, label: 'H' },
                ] as field (field.k)}
                  <label class="flex flex-col">
                    <span class="text-fg-muted/70">{field.label}</span>
                    <input
                      type="number"
                      min="0"
                      value={s.bounds[field.k]}
                      oninput={(e) => {
                        const v = parseInt((e.currentTarget as HTMLInputElement).value, 10) || 0;
                        session.updateSlice(s.name, { ...s, bounds: { ...s.bounds, [field.k]: v } });
                      }}
                      class="rounded border border-border bg-surface px-1 py-0.5 text-fg"
                    />
                  </label>
                {/each}
              </div>
              <!-- Pivot (optional) -->
              <details class="mt-1">
                <summary class="cursor-pointer text-[10px] text-fg-muted hover:text-fg">Pivot / 9-patch</summary>
                <div class="mt-1 grid grid-cols-2 gap-1 text-[10px]">
                  <label class="flex flex-col">
                    <span class="text-fg-muted/70">Pivot X</span>
                    <input
                      type="number"
                      value={s.pivot?.x ?? ''}
                      placeholder="—"
                      oninput={(e) => {
                        const raw = (e.currentTarget as HTMLInputElement).value;
                        const v = raw === '' ? null : parseInt(raw, 10) || 0;
                        const pivot = v === null ? undefined : { x: v, y: s.pivot?.y ?? 0 };
                        session.updateSlice(s.name, { ...s, pivot });
                      }}
                      class="rounded border border-border bg-surface px-1 py-0.5 text-fg"
                    />
                  </label>
                  <label class="flex flex-col">
                    <span class="text-fg-muted/70">Pivot Y</span>
                    <input
                      type="number"
                      value={s.pivot?.y ?? ''}
                      placeholder="—"
                      oninput={(e) => {
                        const raw = (e.currentTarget as HTMLInputElement).value;
                        const v = raw === '' ? null : parseInt(raw, 10) || 0;
                        const pivot = v === null ? undefined : { x: s.pivot?.x ?? 0, y: v };
                        session.updateSlice(s.name, { ...s, pivot });
                      }}
                      class="rounded border border-border bg-surface px-1 py-0.5 text-fg"
                    />
                  </label>
                </div>
                <p class="mt-1 text-[10px] leading-snug text-fg-muted/70">
                  9-patch centre rect lands in a follow-up; pivot
                  + bounds round-trip through Aseprite-compatible
                  JSON already.
                </p>
              </details>
            </div>
          {/each}
        </div>
        <div class="mt-2 flex items-center gap-1">
          <input
            type="text"
            bind:value={newSliceName}
            placeholder="New slice name…"
            class="flex-1 rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg focus:border-accent focus:outline-none"
            onkeydown={(e) => { if (e.key === 'Enter') onAddSlice(); }}
          />
          <button
            type="button"
            onclick={onAddSlice}
            disabled={!newSliceName.trim()}
            class="rounded border border-accent bg-accent/15 px-2 py-0.5 text-[10px] font-medium text-fg hover:bg-accent/25 disabled:opacity-40"
          >Add</button>
        </div>
        <!-- Persist button retired — single "Save metadata to
             companion JSON" in the Metadata section saves slices
             along with frames + tags. -->
      </section>
    {/if}

    <!-- Onion skin — show prev / next frames at low opacity behind
         the current frame so the animator can compare nearby
         poses. Aseprite parity: prev frames tinted red, next blue,
         configurable counts + opacity, F3 hotkey toggles. -->
    {#if (session.metadataFrames && session.metadataFrames.length > 1) || (session.cellW > 0 && session.cellH > 0 && gridCols * gridRows > 1)}
      <section class="border-b border-border p-3 text-xs">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Onion skin</h3>
          <label class="flex items-center gap-1 text-[10px] text-fg-muted">
            <input type="checkbox" bind:checked={session.onionEnabled} class="accent-accent" />
            On (F3)
          </label>
        </div>
        {#if session.onionEnabled}
          <div class="grid grid-cols-2 gap-2">
            <label>
              <span class="mb-0.5 block text-fg-muted">Prev frames</span>
              <input type="number" min="0" max="8" bind:value={session.onionPrev} class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
            </label>
            <label>
              <span class="mb-0.5 block text-fg-muted">Next frames</span>
              <input type="number" min="0" max="8" bind:value={session.onionNext} class="w-full rounded border border-border bg-surface px-1.5 py-0.5 text-fg" />
            </label>
          </div>
          <label class="mt-2 block">
            <span class="mb-0.5 flex justify-between text-fg-muted">
              <span>Opacity</span><span class="font-mono text-fg">{Math.round(session.onionOpacity * 100)}%</span>
            </span>
            <input type="range" min="0.05" max="0.8" step="0.05" bind:value={session.onionOpacity} class="w-full accent-accent" />
          </label>
          <label class="mt-1 flex items-center justify-between text-[10px] text-fg-muted">
            <span>Red / blue tint</span>
            <input type="checkbox" bind:checked={session.onionTint} class="accent-accent" />
          </label>
        {/if}
      </section>
    {/if}

    <!-- Export — client-side encoders for the three formats people
         actually ask for (animated GIF, packed sheet PNG + JSON,
         individual PNGs in a zip). All run in the browser via
         OffscreenCanvas + gifenc + fflate, which keeps federation
         trivial (any node that can read the asset can export from
         it without a backend round-trip). Disabled when the
         frame list is empty (no slicer + no metadata + no
         detection). -->
    {#if (session.metadataFrames && session.metadataFrames.length > 0) || (session.cellW > 0 && session.cellH > 0)}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Export</h3>
        <label class="mb-2 flex items-center justify-between gap-2 text-[10px] text-fg-muted">
          <span>Scale</span>
          <select bind:value={exportScale} class="w-20 rounded border border-border bg-surface px-1.5 py-0.5 text-fg">
            <option value={1}>1×</option>
            <option value={2}>2×</option>
            <option value={4}>4×</option>
            <option value={8}>8×</option>
          </select>
        </label>
        <div class="space-y-1">
          <button
            type="button"
            onclick={doExportGIF}
            disabled={exportBusy}
            class="w-full rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent disabled:opacity-40"
          >{#if exportBusy && exportProgress && exportProgress.total > 0}Encoding {exportProgress.done} / {exportProgress.total}{:else if exportBusy}Working…{:else}Export GIF{/if}</button>
          <button
            type="button"
            onclick={doExportSheet}
            disabled={exportBusy}
            class="w-full rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent disabled:opacity-40"
          >{exportBusy ? 'Working…' : 'Export sheet (PNG + JSON)'}</button>
          <button
            type="button"
            onclick={doExportZip}
            disabled={exportBusy}
            class="w-full rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent disabled:opacity-40"
          >{exportBusy ? 'Working…' : 'Export PNGs (zip)'}</button>
        </div>
        {#if exportError}
          <div class="mt-2 rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">{exportError}</div>
        {/if}
      </section>
    {/if}

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
          <dt class="font-mono text-fg">F3</dt><dd class="text-fg-muted">Toggle onion skin</dd>
          <dt class="col-span-2 mt-1 text-fg-muted/70">Timeline strip</dt>
          <dt class="font-mono text-fg">Click</dt><dd class="text-fg-muted">Jump to that frame · clears any active sub-range</dd>
          <dt class="font-mono text-fg">Drag</dt><dd class="text-fg-muted">Drag across tiles to set a loop range</dd>
          <dt class="font-mono text-fg">Shift+click</dt><dd class="text-fg-muted">Extend the existing range to that frame</dd>
          <dt class="col-span-2 mt-1 text-fg-muted/70">Slicing</dt>
          <dt class="font-mono text-fg">Cell W / H</dt><dd class="text-fg-muted">Sprite size in pixels — auto-guessed from detection on load</dd>
          <dt class="font-mono text-fg">Start / End</dt><dd class="text-fg-muted">Frame range to loop — narrow to one row of a multi-section sheet</dd>
          <dt class="col-span-2 mt-1 text-fg-muted/70">Auto-detect</dt>
          <dt class="font-mono text-fg">Alpha</dt><dd class="text-fg-muted">Use the PNG's transparency channel (most pixel-art sheets)</dd>
          <dt class="font-mono text-fg">Colour</dt><dd class="text-fg-muted">Treat a single colour as background — for sheets without alpha</dd>
          <dt class="font-mono text-fg">Merge gap</dt><dd class="text-fg-muted">Glue separated pixels (floating hats, accessories) into one sprite</dd>
          <dt class="col-span-2 mt-1 text-fg-muted/70">Save</dt>
          <dt class="font-mono text-fg">Companion JSON</dt><dd class="text-fg-muted">Persists frames + tags as a sidecar — reloads pick them up automatically</dd>
          <dt class="col-span-2 mt-1 text-fg-muted/70">Export</dt>
          <dt class="font-mono text-fg">GIF</dt><dd class="text-fg-muted">Animated GIF of the current frame range at the current FPS</dd>
          <dt class="font-mono text-fg">Sheet</dt><dd class="text-fg-muted">Packed PNG + TexturePacker JSON describing the layout</dd>
          <dt class="font-mono text-fg">Zip</dt><dd class="text-fg-muted">Each frame as an independent PNG, ordered by filename</dd>
        </dl>
      </details>
    </section>
  </div>
</div>
