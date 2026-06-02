<script lang="ts">
  // DetailsTool body — the generic "what is this asset" surface.
  // Always available; this is the default body the standalone
  // /assets/[id] route gets when no host has overridden Details.
  // Hosts (PostHost) override by registering id='details' in their
  // customTools — the registry's merge-by-id replaces this body.
  //
  // Also owns "basic view controls" since the ViewTool was retired
  // (its own dropdown entry was redundant with this fallback). For
  // 2D kinds we render the shell's zoom presets; for 3D we render
  // the controller.tools sections (Camera / Display / Lighting /
  // Auto-rotate) that ModelView advertises.

  import type { ToolContext } from '../contract';

  let { ctx }: { ctx: ToolContext } = $props();

  const tools = $derived(ctx.controller.tools);
  const shellState = $derived(ctx.shellState);
  // 2D zoom presets are useful for everything that isn't 3D or
  // sprite (sprite owns its own integer-step pixel-perfect zoom).
  const showZoomPresets = $derived(
    !!shellState
    && ctx.controller.kind !== '3d'
    && ctx.controller.kind !== 'sprite',
  );
</script>

<div class="space-y-3 p-3 text-xs">
  <!-- Asset identity — title / id / extension / hash. Compact list
       at the top so the user always sees what they're looking at. -->
  <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
    <dt class="text-fg-muted">Title</dt>
    <dd class="truncate text-fg">{ctx.asset.title ?? 'Untitled'}</dd>
    <dt class="text-fg-muted">ID</dt>
    <dd class="truncate font-mono text-[10px] text-fg">{ctx.asset.id}</dd>
    {#if ctx.asset.file_extension}
      <dt class="text-fg-muted">Extension</dt>
      <dd class="font-mono text-fg">.{ctx.asset.file_extension}</dd>
    {/if}
    {#if ctx.asset.file_hash}
      <dt class="text-fg-muted">Hash</dt>
      <dd class="truncate font-mono text-[10px] text-fg" title={ctx.asset.file_hash}>
        {ctx.asset.file_hash.slice(0, 16)}…
      </dd>
    {/if}
  </dl>

  <div class="flex gap-2">
    <a
      href={`/api/v1/assets/${ctx.asset.id}/file`}
      download
      class="rounded border border-border bg-surface-elevated px-2 py-1 text-fg hover:border-accent"
    >Download original</a>
  </div>

  {#if showZoomPresets && shellState}
    <section class="rounded-md border border-border bg-surface-elevated">
      <header class="border-b border-border px-3 py-2 text-[10px] font-medium uppercase tracking-wide text-fg-muted">View</header>
      <div class="flex flex-wrap gap-1.5 p-3">
        {#each shellState.zoomPresets as p (p.label)}
          <button
            type="button"
            onclick={() => (p.factor === null ? shellState.resetView() : shellState.setZoom(p.factor))}
            class="rounded-md border border-border bg-surface px-2 py-1 text-xs hover:border-fg-muted/60 hover:bg-surface-elevated"
            class:border-accent={p.factor !== null && Math.abs(shellState.zoom - p.factor) < 0.001}
            class:text-accent={p.factor !== null && Math.abs(shellState.zoom - p.factor) < 0.001}
          >
            {p.label}
          </button>
        {/each}
      </div>
    </section>
  {/if}

  {#if tools}
    <!-- ── Camera (3D) ──────────────────────────────────────────── -->
    {#if tools.frameAll || tools.resetCamera || tools.cameraPreset}
      <section class="rounded-md border border-border bg-surface-elevated">
        <header class="border-b border-border px-3 py-2 text-[10px] font-medium uppercase tracking-wide text-fg-muted">Camera</header>
        <div class="flex flex-wrap gap-1.5 p-3">
          {#if tools.frameAll}
            <button type="button" onclick={tools.frameAll} class="rounded-md border border-border bg-surface px-2 py-1 text-xs hover:border-fg-muted/60 hover:bg-surface-elevated">Frame all</button>
          {/if}
          {#if tools.resetCamera}
            <button type="button" onclick={tools.resetCamera} class="rounded-md border border-border bg-surface px-2 py-1 text-xs hover:border-fg-muted/60 hover:bg-surface-elevated">Reset</button>
          {/if}
        </div>
      </section>
    {/if}

    <!-- ── Display (3D) ─────────────────────────────────────────── -->
    {#if tools.grid || tools.axes || tools.wireframe || tools.groundShadow}
      <section class="rounded-md border border-border bg-surface-elevated">
        <header class="border-b border-border px-3 py-2 text-[10px] font-medium uppercase tracking-wide text-fg-muted">Display</header>
        <div class="space-y-2 p-3">
          {#if tools.grid}
            <label class="flex items-center justify-between text-xs">
              <span>Grid</span>
              <button type="button" onclick={tools.grid.toggle} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={tools.grid.enabled} class:bg-border={!tools.grid.enabled} role="switch" aria-checked={tools.grid.enabled}>
                <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={tools.grid.enabled} class:translate-x-0.5={!tools.grid.enabled}></span>
              </button>
            </label>
          {/if}
          {#if tools.axes}
            <label class="flex items-center justify-between text-xs">
              <span>Axes</span>
              <button type="button" onclick={tools.axes.toggle} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={tools.axes.enabled} class:bg-border={!tools.axes.enabled} role="switch" aria-checked={tools.axes.enabled}>
                <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={tools.axes.enabled} class:translate-x-0.5={!tools.axes.enabled}></span>
              </button>
            </label>
          {/if}
          {#if tools.groundShadow}
            <label class="flex items-center justify-between text-xs">
              <span>Ground shadow</span>
              <button type="button" onclick={tools.groundShadow.toggle} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={tools.groundShadow.enabled} class:bg-border={!tools.groundShadow.enabled} role="switch" aria-checked={tools.groundShadow.enabled}>
                <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={tools.groundShadow.enabled} class:translate-x-0.5={!tools.groundShadow.enabled}></span>
              </button>
            </label>
          {/if}
          {#if tools.wireframe}
            <div class="flex items-center justify-between text-xs">
              <span>Wireframe</span>
              <button type="button" onclick={tools.wireframe.cycle} class="rounded-md border border-border bg-surface px-2 py-0.5 text-xs capitalize hover:border-fg-muted/60 hover:bg-surface-elevated" title={`Cycle: ${tools.wireframe.options.join(' → ')}`}>{tools.wireframe.mode}</button>
            </div>
          {/if}
        </div>
      </section>
    {/if}

    <!-- ── Lighting (3D) ────────────────────────────────────────── -->
    {#if tools.exposure || tools.envIntensity}
      <section class="rounded-md border border-border bg-surface-elevated">
        <header class="border-b border-border px-3 py-2 text-[10px] font-medium uppercase tracking-wide text-fg-muted">Lighting</header>
        <div class="space-y-3 p-3">
          {#if tools.exposure}
            <label class="block text-xs">
              <span class="mb-1 flex items-center justify-between">
                <span>{tools.exposure.label ?? 'Exposure'}</span>
                <span class="font-mono text-fg-muted">{tools.exposure.value.toFixed(2)}</span>
              </span>
              <input type="range" min={tools.exposure.min} max={tools.exposure.max} step={tools.exposure.step ?? 0.01} value={tools.exposure.value} oninput={(e) => tools.exposure!.set(+(e.currentTarget as HTMLInputElement).value)} class="w-full accent-accent" />
            </label>
          {/if}
          {#if tools.envIntensity}
            <label class="block text-xs">
              <span class="mb-1 flex items-center justify-between">
                <span>{tools.envIntensity.label ?? 'Env intensity'}</span>
                <span class="font-mono text-fg-muted">{tools.envIntensity.value.toFixed(2)}</span>
              </span>
              <input type="range" min={tools.envIntensity.min} max={tools.envIntensity.max} step={tools.envIntensity.step ?? 0.01} value={tools.envIntensity.value} oninput={(e) => tools.envIntensity!.set(+(e.currentTarget as HTMLInputElement).value)} class="w-full accent-accent" />
            </label>
          {/if}
        </div>
      </section>
    {/if}

    <!-- ── Auto-rotate (3D) ─────────────────────────────────────── -->
    {#if tools.autoRotate || tools.autoRotateSpeed}
      <section class="rounded-md border border-border bg-surface-elevated">
        <header class="border-b border-border px-3 py-2 text-[10px] font-medium uppercase tracking-wide text-fg-muted">Auto-rotate</header>
        <div class="space-y-3 p-3">
          {#if tools.autoRotate}
            <label class="flex items-center justify-between text-xs">
              <span>Enabled</span>
              <button type="button" onclick={tools.autoRotate.toggle} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={tools.autoRotate.enabled} class:bg-border={!tools.autoRotate.enabled} role="switch" aria-checked={tools.autoRotate.enabled}>
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
              <input type="range" min={tools.autoRotateSpeed.min} max={tools.autoRotateSpeed.max} step={tools.autoRotateSpeed.step ?? 0.1} value={tools.autoRotateSpeed.value} oninput={(e) => tools.autoRotateSpeed!.set(+(e.currentTarget as HTMLInputElement).value)} class="w-full accent-accent" />
            </label>
          {/if}
        </div>
      </section>
    {/if}
  {/if}
</div>
