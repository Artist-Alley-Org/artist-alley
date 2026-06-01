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

    <!-- Slice grid OR frame-box toggle -->
    {#if !session.metadataFrames}
      <section class="border-b border-border p-3 text-xs">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Slice grid</h3>
          <label class="flex items-center gap-1 text-[10px] text-fg-muted">
            <input type="checkbox" bind:checked={session.showGrid} class="accent-accent" />
            Show grid
          </label>
        </div>
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
        <label class="mt-2 flex items-center gap-2 text-[10px]">
          <span class="text-fg-muted">Limit frames</span>
          <input
            type="number"
            min="1"
            max={gridCols * gridRows}
            value={session.frameCountOverride ?? ''}
            oninput={(e) => {
              const v = (e.currentTarget as HTMLInputElement).value;
              session.frameCountOverride = v === '' ? null : Math.max(1, Math.min(gridCols * gridRows, parseInt(v, 10) || 1));
            }}
            placeholder="auto"
            class="w-16 rounded border border-border bg-surface px-1.5 py-0.5 text-fg"
          />
          {#if session.frameCountOverride != null}
            <button
              type="button"
              onclick={() => (session.frameCountOverride = null)}
              class="text-fg-muted hover:text-fg"
              title="Clear override"
            >clear</button>
          {/if}
        </label>
      </section>
    {:else}
      <section class="border-b border-border p-3 text-xs">
        <label class="flex items-center justify-between text-[10px] text-fg-muted">
          <span>Show frame boxes</span>
          <input type="checkbox" bind:checked={session.showGrid} class="accent-accent" />
        </label>
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
  </div>
</div>
