<script lang="ts">
  // ViewTool body — two cleanly-separated halves:
  //
  //   1. 2D zoom presets — driven by ctx.shellState (shell-owned
  //      canvas transform). Shown for image / pdf / font / video /
  //      audio / sequence; suppressed for 3D where the camera owns
  //      framing.
  //   2. Per-kind controller.tools sections — Camera / Display /
  //      Lighting / Auto-rotate. These plug in when the mounted
  //      view body advertises them (ModelView fills the 3D set
  //      today). Empty for kinds whose body didn't expose any.
  //
  // The "View" name is intentionally generic — the same slot is
  // the natural home for both 2D zoom and 3D camera, so users
  // never have to learn which tool name matches which kind.

  import type { ToolContext } from '../contract';

  let { ctx }: { ctx: ToolContext } = $props();
  const tools = $derived(ctx.controller.tools);
  const shellState = $derived(ctx.shellState);
  // 2D zoom presets are useful for everything that isn't 3D.
  // 3D kinds get framing via their controller.tools.cameraPreset
  // / frameAll instead.
  const showZoomPresets = $derived(!!shellState && ctx.controller.kind !== '3d');
</script>

<div class="space-y-1 p-3">
  {#if showZoomPresets && shellState}
    <section class="rounded-md border border-border bg-surface-elevated">
      <header class="border-b border-border px-3 py-2 text-xs font-medium uppercase tracking-wide text-fg-muted">View</header>
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
    <!-- ── Camera ─────────────────────────────────────────────── -->
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

    <!-- ── Display ────────────────────────────────────────────── -->
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
                title={`Cycle: ${tools.wireframe.options.join(' → ')}`}
              >
                {tools.wireframe.mode}
              </button>
            </div>
          {/if}
        </div>
      </section>
    {/if}

    <!-- ── Lighting ───────────────────────────────────────────── -->
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

    <!-- ── Auto-rotate ────────────────────────────────────────── -->
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
  {/if}

  {#if ctx.controller.kind === '3d' && !tools}
    <!-- 3D body hasn't mounted yet — placeholder until ModelView
         posts controller.tools. -->
    <div class="p-4 text-sm text-fg-muted">
      <p>Loading view controls…</p>
    </div>
  {/if}
</div>
