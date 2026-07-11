<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // ModelTool body — the 3D viewer's side-panel surface. Binds the
  // shared ModelSession that ModelView also binds; both ends mutate
  // the same $state object so flipping a preset here updates the
  // renderer without an event bus.
  //
  // Sections (top → bottom):
  //   1. Camera      — presets, projection, FOV, frame/reset, save view
  //   2. Environment — HDRI preset, env intensity, BG, tone-map, exposure
  //   3. Lighting    — preset, key/fill/rim toggles + intensity, shadows
  //   4. Display     — render mode, grid/axes/bbox, up axis
  //   5. Materials   — per-material color/metalness/roughness overrides
  //   6. Animation   — clip selector + transport + scrub
  //   7. Stats       — verts/tris/materials/textures/draw calls/size
  //   8. Annotations — placeholder for surface-pinned comments (next)
  //
  // Compact-rail support isn't enabled (the controls don't fit a
  // 3.5rem rail meaningfully); the user collapses the whole pane if
  // they want more canvas.

  import type { ToolContext } from '../contract';
  import type {
    EnvPresetId,
    ToneMappingId,
  } from '$lib/3d/environments';
  import type {
    LightingPresetId,
    RenderModeId,
    CameraPresetId,
    ProjectionId,
    UpAxisId,
  } from '$lib/3d/session.svelte';

  let { ctx }: { ctx: ToolContext } = $props();
  const session = $derived(ctx.modelSession);
  // Marmoset's WebViewer is closed-source: it renders the asset
  // through its own pipeline and only exposes resetCamera + resize.
  // Most of the panel's sliders/toggles can't drive it, so we hide
  // the inapplicable sections and surface an informational note
  // pointing the user at Marmoset's in-canvas controls. The Camera
  // section's Frame all / Reset DO call resetCamera, so they stay.
  const isMarmoset = $derived(session?.backend === 'marmoset');

  // Section enums declared at module scope so the template's #each
  // iterations don't churn (Svelte 5 keys on identity for primitives,
  // but keeping them constant avoids needless reactivity hops).
  const CAMERA_PRESETS: { id: CameraPresetId; label: string }[] = [
    { id: 'iso', label: 'Iso' },
    { id: 'front', label: 'Front' },
    { id: 'back', label: 'Back' },
    { id: 'left', label: 'Left' },
    { id: 'right', label: 'Right' },
    { id: 'top', label: 'Top' },
    { id: 'bottom', label: 'Bottom' },
  ];
  const ENV_PRESETS: { id: EnvPresetId; label: string }[] = [
    { id: 'studio', label: 'Studio' },
    { id: 'park', label: 'Park' },
    { id: 'sunset', label: 'Sunset' },
    { id: 'city', label: 'City' },
    { id: 'night', label: 'Night' },
    { id: 'none', label: 'None' },
  ];
  const TONE_MAPS: { id: ToneMappingId; label: string }[] = [
    { id: 'none', label: 'None' },
    { id: 'linear', label: 'Linear' },
    { id: 'reinhard', label: 'Reinhard' },
    { id: 'cineon', label: 'Cineon' },
    { id: 'aces', label: 'ACES' },
    { id: 'neutral', label: 'Neutral' },
  ];
  const LIGHTING_PRESETS: { id: LightingPresetId; label: string }[] = [
    { id: 'three-point', label: 'Three-point' },
    { id: 'studio', label: 'Studio' },
    { id: 'outdoor', label: 'Outdoor' },
    { id: 'showroom', label: 'Showroom' },
    { id: 'custom', label: 'Custom' },
  ];
  const RENDER_MODES: { id: RenderModeId; label: string }[] = [
    { id: 'solid', label: 'Solid' },
    { id: 'wireframe', label: 'Wire' },
    { id: 'overlay', label: 'Overlay' },
    { id: 'xray', label: 'X-Ray' },
    { id: 'normals', label: 'Normals' },
    { id: 'matcap', label: 'Matcap' },
  ];

  function fmtBytes(n: number | null): string {
    if (n == null) return '—';
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
    return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
  }
  function fmtCount(n: number): string {
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
    if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
    return String(n);
  }
  function fmtTime(s: number): string {
    if (!Number.isFinite(s) || s < 0) return '0:00';
    const m = Math.floor(s / 60);
    const sec = s - m * 60;
    return `${m}:${sec.toFixed(2).padStart(5, '0')}`;
  }

  // Save-view bag — the panel doesn't actually know the current camera
  // state; the viewer does. We dispatch the trigger and let the viewer
  // snapshot. But the viewer's snapshot helper lives behind the host
  // closure — so we have a tiny indirection: the panel sets a sentinel
  // (.savedView with a special marker would fight with restore), and
  // instead provides callbacks the viewer registers on mount.
  // For now we wire through a simple convention: viewer exposes a
  // window-side helper at session._snapshot when ready. Keep this
  // wholly in-session by having the ModelView publish a getter.
  // Simpler: ModelView already exposes host.snapshotView, but only
  // internally. So we add a method on the session that just calls
  // session.saveCurrentView with the LAST mirrored viewer state; the
  // viewer mirrors view-state onto session._lastViewSnapshot every
  // frame is overkill. Cleanest compromise: have the panel emit a
  // "save" by setting cameraPreset='custom' and emitting a trigger.
  // The viewer reads its own snapshot helper and writes back via
  // session.saveCurrentView. We expose that as an imperative on the
  // session by binding the viewer's snapshotView into a method at
  // mount time — but to avoid that wiring complexity for the first
  // cut, the SAVE button below simply hides behind a "viewer hookup"
  // sentinel below; until that callback is plumbed, the button reads
  // disabled and tooltipped. (Plumbing is small; can land in a
  // follow-up if this commit's surface is already large.)
  // ----- update: the simplest path is to expose a callback on the
  // session that the viewer overwrites at mount; see session methods
  // saveCurrentView / clearSavedView. The viewer wires this in mount.
</script>

{#if !session}
  <div class="p-4 text-sm text-fg-muted"><p>3D viewer is loading…</p></div>
{:else}
  <div class="flex flex-col">

    {#if isMarmoset}
      <!-- Marmoset's WebViewer comes with its own bottom-overlay
           controls (orbit / animation / fullscreen) baked into the
           canvas. The panel below this notice keeps the bits we
           CAN drive (Frame all + Reset) and hides the rest. -->
      <section class="border-b border-border bg-accent/5 p-3 text-xs">
        <h3 class="mb-1 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Marmoset viewer</h3>
        <p class="text-[10px] leading-snug text-fg-muted">
          This <span class="font-mono text-fg">.mview</span> renders through Marmoset Toolbag's WebViewer.
          Hover the canvas to reveal Marmoset's built-in chrome — environment,
          lighting, animation, and material toggles all live there since the
          format embeds those choices alongside the geometry.
        </p>
      </section>
    {/if}

    <!-- ── 1. Camera ─────────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Camera</h3>
      <div class="mb-2">
        <span class="mb-1 block text-fg-muted">View</span>
        <div class="grid grid-cols-4 gap-1">
          {#each CAMERA_PRESETS as p (p.id)}
            <button
              type="button"
              onclick={() => session.setCameraPreset(p.id)}
              class={`rounded border px-1.5 py-1 text-[10px] ${session.cameraPreset === p.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
              title={`${p.label} view`}
            >{p.label}</button>
          {/each}
        </div>
      </div>
      <div class="mb-2 flex items-center justify-between">
        <span class="text-fg-muted">Projection</span>
        <div class="flex overflow-hidden rounded border border-border">
          {#each ['perspective', 'orthographic'] as P (P)}
            <button
              type="button"
              onclick={() => session.setProjection(P as ProjectionId)}
              class={`px-2 py-1 text-[10px] capitalize ${session.projection === P ? 'bg-accent/20 text-fg' : 'bg-surface text-fg hover:bg-surface-elevated'}`}
            >{P === 'perspective' ? 'Persp' : 'Ortho'}</button>
          {/each}
        </div>
      </div>
      {#if session.projection === 'perspective'}
        <label class="mb-2 block">
          <span class="mb-1 flex items-center justify-between text-fg-muted">
            <span>FOV</span>
            <span class="font-mono text-fg">{session.fov}°</span>
          </span>
          <input
            type="range" min="15" max="90" step="1"
            value={session.fov}
            oninput={(e) => session.setFov(+(e.currentTarget as HTMLInputElement).value)}
            class="w-full accent-accent"
          />
        </label>
      {/if}
      <div class="mb-2 flex items-center justify-between">
        <span class="text-fg-muted">Up axis</span>
        <div class="flex overflow-hidden rounded border border-border">
          {#each ['y', 'z'] as A (A)}
            <button
              type="button"
              onclick={() => session.setUpAxis(A as UpAxisId)}
              class={`px-2 py-1 text-[10px] uppercase ${session.upAxis === A ? 'bg-accent/20 text-fg' : 'bg-surface text-fg hover:bg-surface-elevated'}`}
              title={A === 'y' ? 'Y-up (glTF default)' : 'Z-up (DCC default)'}
            >{A}-up</button>
          {/each}
        </div>
      </div>
      <div class="mb-2 grid grid-cols-2 gap-1">
        <button
          type="button"
          onclick={() => session.frameAll()}
          class="rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
        >Frame all</button>
        <button
          type="button"
          onclick={() => session.resetCamera()}
          class="rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
        >Reset</button>
      </div>
      {#if !isMarmoset}
      <div class="grid grid-cols-2 gap-1">
        <button
          type="button"
          onclick={() => { if (session.snapshotView) session.saveCurrentView(session.snapshotView()); }}
          disabled={!session.snapshotView}
          class="rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25 disabled:opacity-40"
          title="Save the current camera as this asset's bookmark"
        >Save view</button>
        <button
          type="button"
          onclick={() => session.clearSavedView()}
          disabled={!session.savedView}
          class="rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent disabled:opacity-40"
          title="Clear the saved camera"
        >Clear</button>
      </div>
      {#if session.savedView && session.restoreSavedView}
        <button
          type="button"
          onclick={() => session.restoreSavedView?.()}
          class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
        >Restore saved</button>
      {/if}
      {/if}
      {#if !isMarmoset}
      <label class="mt-2 flex items-center justify-between text-fg-muted">
        <span>Auto-rotate</span>
        <button
          type="button"
          onclick={() => session.toggleAutoRotate()}
          class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
          class:bg-accent={session.autoRotate}
          class:bg-border={!session.autoRotate}
          role="switch"
          aria-checked={session.autoRotate}
          aria-label="Auto-rotate"
        >
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.autoRotate} class:translate-x-0.5={!session.autoRotate}></span>
        </button>
      </label>
      {#if session.autoRotate}
        <label class="mt-1 block">
          <span class="mb-1 flex items-center justify-between text-fg-muted">
            <span>Spin</span>
            <span class="font-mono text-fg">{session.autoRotateSpeed.toFixed(1)}×</span>
          </span>
          <input
            type="range" min="0.1" max="8" step="0.1"
            value={session.autoRotateSpeed}
            oninput={(e) => session.setAutoRotateSpeed(+(e.currentTarget as HTMLInputElement).value)}
            class="w-full accent-accent"
          />
        </label>
      {/if}
      {/if}
    </section>

    {#if !isMarmoset}
    <!-- ── 2. Environment ────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Environment</h3>
      <div class="mb-2">
        <span class="mb-1 block text-fg-muted">HDRI preset</span>
        <div class="grid grid-cols-3 gap-1">
          {#each ENV_PRESETS as e (e.id)}
            <button
              type="button"
              onclick={() => session.setEnvPreset(e.id)}
              class={`rounded border px-1.5 py-1 text-[10px] ${session.envPreset === e.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
              title={`${e.label} HDRI`}
            >{e.label}</button>
          {/each}
        </div>
      </div>
      <label class="mb-2 block">
        <span class="mb-1 flex items-center justify-between text-fg-muted">
          <span>Env intensity</span>
          <span class="font-mono text-fg">{session.envIntensity.toFixed(2)}</span>
        </span>
        <input
          type="range" min="0" max="3" step="0.01"
          value={session.envIntensity}
          oninput={(e) => session.setEnvIntensity(+(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
        />
      </label>
      <label class="mb-2 flex items-center justify-between text-fg-muted">
        <span>Show as background</span>
        <button
          type="button"
          onclick={() => session.toggleBackgroundVisible()}
          class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
          class:bg-accent={session.backgroundVisible}
          class:bg-border={!session.backgroundVisible}
          role="switch"
          aria-checked={session.backgroundVisible}
          aria-label="Show env as background"
        >
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.backgroundVisible} class:translate-x-0.5={!session.backgroundVisible}></span>
        </button>
      </label>
      <div class="mb-2">
        <span class="mb-1 block text-fg-muted">Tone mapping</span>
        <select
          value={session.toneMapping}
          onchange={(e) => session.setToneMapping((e.currentTarget as HTMLSelectElement).value as ToneMappingId)}
          class="w-full rounded border border-border bg-surface px-2 py-1 text-[11px] text-fg focus:border-accent focus:outline-none"
        >
          {#each TONE_MAPS as t (t.id)}
            <option value={t.id}>{t.label}</option>
          {/each}
        </select>
      </div>
      <label class="block">
        <span class="mb-1 flex items-center justify-between text-fg-muted">
          <span>Exposure</span>
          <span class="font-mono text-fg">{session.exposure.toFixed(2)}</span>
        </span>
        <input
          type="range" min="0.1" max="3" step="0.01"
          value={session.exposure}
          oninput={(e) => session.setExposure(+(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
        />
      </label>
    </section>

    <!-- ── 3. Lighting ───────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Lighting</h3>
      <div class="mb-2">
        <span class="mb-1 block text-fg-muted">Preset</span>
        <div class="grid grid-cols-3 gap-1">
          {#each LIGHTING_PRESETS as p (p.id)}
            <button
              type="button"
              onclick={() => session.setLightingPreset(p.id)}
              class={`rounded border px-1.5 py-1 text-[10px] ${session.lightingPreset === p.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
              title={p.label}
            >{p.label}</button>
          {/each}
        </div>
      </div>

      <!-- Key light -->
      <div class="mb-2 rounded border border-border/60 bg-surface/50 p-2">
        <label class="mb-1 flex items-center justify-between text-fg-muted">
          <span class="font-medium text-fg">Key</span>
          <button
            type="button"
            onclick={() => session.setKeyEnabled(!session.keyEnabled)}
            class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
            class:bg-accent={session.keyEnabled}
            class:bg-border={!session.keyEnabled}
            role="switch"
            aria-checked={session.keyEnabled}
            aria-label="Key light enabled"
          >
            <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.keyEnabled} class:translate-x-0.5={!session.keyEnabled}></span>
          </button>
        </label>
        {#if session.keyEnabled}
          <label class="mb-1 block">
            <span class="mb-0.5 flex items-center justify-between text-fg-muted">
              <span>Intensity</span><span class="font-mono text-fg">{session.keyIntensity.toFixed(2)}</span>
            </span>
            <input type="range" min="0" max="8" step="0.05" value={session.keyIntensity}
              oninput={(e) => session.setKeyIntensity(+(e.currentTarget as HTMLInputElement).value)}
              class="w-full accent-accent" />
          </label>
          <label class="mb-1 block">
            <span class="mb-0.5 flex items-center justify-between text-fg-muted">
              <span>Azimuth</span><span class="font-mono text-fg">{session.keyAzimuth}°</span>
            </span>
            <input type="range" min="0" max="359" step="1" value={session.keyAzimuth}
              oninput={(e) => session.setKeyAzimuth(+(e.currentTarget as HTMLInputElement).value)}
              class="w-full accent-accent" />
          </label>
          <label class="mb-1 block">
            <span class="mb-0.5 flex items-center justify-between text-fg-muted">
              <span>Elevation</span><span class="font-mono text-fg">{session.keyElevation}°</span>
            </span>
            <input type="range" min="-90" max="90" step="1" value={session.keyElevation}
              oninput={(e) => session.setKeyElevation(+(e.currentTarget as HTMLInputElement).value)}
              class="w-full accent-accent" />
          </label>
          <label class="flex items-center justify-between text-fg-muted">
            <span>Color</span>
            <input type="color" value={session.keyColor}
              oninput={(e) => session.setKeyColor((e.currentTarget as HTMLInputElement).value)}
              class="h-6 w-12 cursor-pointer rounded border border-border bg-surface" />
          </label>
        {/if}
      </div>

      <!-- Fill light -->
      <div class="mb-2 rounded border border-border/60 bg-surface/50 p-2">
        <label class="mb-1 flex items-center justify-between text-fg-muted">
          <span class="font-medium text-fg">Fill</span>
          <button
            type="button"
            onclick={() => session.setFillEnabled(!session.fillEnabled)}
            class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
            class:bg-accent={session.fillEnabled}
            class:bg-border={!session.fillEnabled}
            role="switch"
            aria-checked={session.fillEnabled}
            aria-label="Fill light enabled"
          >
            <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.fillEnabled} class:translate-x-0.5={!session.fillEnabled}></span>
          </button>
        </label>
        {#if session.fillEnabled}
          <label class="block">
            <span class="mb-0.5 flex items-center justify-between text-fg-muted">
              <span>Intensity</span><span class="font-mono text-fg">{session.fillIntensity.toFixed(2)}</span>
            </span>
            <input type="range" min="0" max="4" step="0.05" value={session.fillIntensity}
              oninput={(e) => session.setFillIntensity(+(e.currentTarget as HTMLInputElement).value)}
              class="w-full accent-accent" />
          </label>
        {/if}
      </div>

      <!-- Rim light -->
      <div class="mb-2 rounded border border-border/60 bg-surface/50 p-2">
        <label class="mb-1 flex items-center justify-between text-fg-muted">
          <span class="font-medium text-fg">Rim</span>
          <button
            type="button"
            onclick={() => session.setRimEnabled(!session.rimEnabled)}
            class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
            class:bg-accent={session.rimEnabled}
            class:bg-border={!session.rimEnabled}
            role="switch"
            aria-checked={session.rimEnabled}
            aria-label="Rim light enabled"
          >
            <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.rimEnabled} class:translate-x-0.5={!session.rimEnabled}></span>
          </button>
        </label>
        {#if session.rimEnabled}
          <label class="block">
            <span class="mb-0.5 flex items-center justify-between text-fg-muted">
              <span>Intensity</span><span class="font-mono text-fg">{session.rimIntensity.toFixed(2)}</span>
            </span>
            <input type="range" min="0" max="4" step="0.05" value={session.rimIntensity}
              oninput={(e) => session.setRimIntensity(+(e.currentTarget as HTMLInputElement).value)}
              class="w-full accent-accent" />
          </label>
        {/if}
      </div>

      <!-- Shadows / ground -->
      <label class="mb-1 flex items-center justify-between text-fg-muted">
        <span>Shadows</span>
        <button
          type="button"
          onclick={() => session.toggleShadows()}
          class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
          class:bg-accent={session.shadows}
          class:bg-border={!session.shadows}
          role="switch"
          aria-checked={session.shadows}
          aria-label="Shadows enabled"
        >
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.shadows} class:translate-x-0.5={!session.shadows}></span>
        </button>
      </label>
      {#if session.shadows}
        <label class="mb-1 block">
          <span class="mb-0.5 flex items-center justify-between text-fg-muted">
            <span>Softness</span><span class="font-mono text-fg">{session.shadowSoftness.toFixed(2)}</span>
          </span>
          <input type="range" min="0" max="1" step="0.01" value={session.shadowSoftness}
            oninput={(e) => session.setShadowSoftness(+(e.currentTarget as HTMLInputElement).value)}
            class="w-full accent-accent" />
        </label>
      {/if}
      <label class="mb-1 flex items-center justify-between text-fg-muted">
        <span>Ground plane</span>
        <button
          type="button"
          onclick={() => session.toggleGroundPlane()}
          class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
          class:bg-accent={session.groundPlane}
          class:bg-border={!session.groundPlane}
          role="switch"
          aria-checked={session.groundPlane}
          aria-label="Ground plane visible"
        >
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.groundPlane} class:translate-x-0.5={!session.groundPlane}></span>
        </button>
      </label>
      <label class="flex items-center justify-between text-fg-muted">
        <span>Contact shadow</span>
        <button
          type="button"
          onclick={() => session.toggleContactShadow()}
          class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
          class:bg-accent={session.contactShadow}
          class:bg-border={!session.contactShadow}
          role="switch"
          aria-checked={session.contactShadow}
          aria-label="Contact shadow visible"
        >
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.contactShadow} class:translate-x-0.5={!session.contactShadow}></span>
        </button>
      </label>
    </section>
    {/if}

    {#if !isMarmoset}
    <!-- ── 4. Display ────────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Display</h3>
      <div class="mb-2">
        <span class="mb-1 block text-fg-muted">Render mode</span>
        <div class="grid grid-cols-3 gap-1">
          {#each RENDER_MODES as m (m.id)}
            <button
              type="button"
              onclick={() => session.setRenderMode(m.id)}
              class={`rounded border px-1.5 py-1 text-[10px] ${session.renderMode === m.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
              title={m.label}
            >{m.label}</button>
          {/each}
        </div>
      </div>
      <label class="mb-1 flex items-center justify-between text-fg-muted">
        <span>Grid</span>
        <button type="button" onclick={() => session.toggleGrid()} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={session.showGrid} class:bg-border={!session.showGrid} role="switch" aria-checked={session.showGrid} aria-label="Grid visible">
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.showGrid} class:translate-x-0.5={!session.showGrid}></span>
        </button>
      </label>
      <label class="mb-1 flex items-center justify-between text-fg-muted">
        <span>Axes</span>
        <button type="button" onclick={() => session.toggleAxes()} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={session.showAxes} class:bg-border={!session.showAxes} role="switch" aria-checked={session.showAxes} aria-label="Axes visible">
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.showAxes} class:translate-x-0.5={!session.showAxes}></span>
        </button>
      </label>
      <label class="flex items-center justify-between text-fg-muted">
        <span>Bounding box</span>
        <button type="button" onclick={() => session.toggleBoundingBox()} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={session.showBoundingBox} class:bg-border={!session.showBoundingBox} role="switch" aria-checked={session.showBoundingBox} aria-label="Bounding box visible">
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.showBoundingBox} class:translate-x-0.5={!session.showBoundingBox}></span>
        </button>
      </label>
    </section>
    {/if}

    <!-- ── 5. Materials ──────────────────────────────────────── -->
    {#if session.materials.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Materials</h3>
          <span class="font-mono text-[10px] text-fg-muted">{session.materials.length}</span>
        </div>
        <div class="max-h-72 space-y-1.5 overflow-y-auto pr-1">
          {#each session.materials as m (m.id)}
            {@const ov = session.materialOverrides[m.id] ?? {}}
            {@const curColor = ov.color ?? m.baseColor}
            {@const curMet = ov.metalness ?? m.baseMetalness}
            {@const curRough = ov.roughness ?? m.baseRoughness}
            <details class="rounded border border-border/60 bg-surface/50 p-1.5">
              <summary class="flex cursor-pointer items-center justify-between gap-1 text-[10px]">
                <span class="truncate text-fg" title={m.name}>{m.name}</span>
                <span class="ml-1 shrink-0 font-mono text-fg-muted/70">×{m.meshCount}</span>
              </summary>
              <div class="mt-1 space-y-1">
                <label class="flex items-center justify-between text-fg-muted">
                  <span>Color</span>
                  <input type="color" value={curColor}
                    oninput={(e) => session.setMaterialOverride(m.id, { color: (e.currentTarget as HTMLInputElement).value })}
                    class="h-5 w-10 cursor-pointer rounded border border-border bg-surface" />
                </label>
                <label class="block">
                  <span class="mb-0.5 flex items-center justify-between text-fg-muted">
                    <span>Metalness</span><span class="font-mono text-fg">{curMet.toFixed(2)}</span>
                  </span>
                  <input type="range" min="0" max="1" step="0.01" value={curMet}
                    oninput={(e) => session.setMaterialOverride(m.id, { metalness: +(e.currentTarget as HTMLInputElement).value })}
                    class="w-full accent-accent" />
                </label>
                <label class="block">
                  <span class="mb-0.5 flex items-center justify-between text-fg-muted">
                    <span>Roughness</span><span class="font-mono text-fg">{curRough.toFixed(2)}</span>
                  </span>
                  <input type="range" min="0" max="1" step="0.01" value={curRough}
                    oninput={(e) => session.setMaterialOverride(m.id, { roughness: +(e.currentTarget as HTMLInputElement).value })}
                    class="w-full accent-accent" />
                </label>
                <button
                  type="button"
                  onclick={() => session.resetMaterialOverride(m.id)}
                  disabled={!session.materialOverrides[m.id]}
                  class="w-full rounded border border-border bg-surface px-2 py-0.5 text-[10px] text-fg hover:border-accent disabled:opacity-40"
                >Reset</button>
              </div>
            </details>
          {/each}
        </div>
        {#if Object.keys(session.materialOverrides).length > 0}
          <button
            type="button"
            onclick={() => session.resetAllMaterials()}
            class="mt-2 w-full rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
          >Reset all materials</button>
        {/if}
      </section>
    {/if}

    <!-- ── 6. Animation ──────────────────────────────────────── -->
    {#if session.clips.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Animation</h3>
          <span class="font-mono text-[10px] text-fg-muted">{session.clips.length}</span>
        </div>
        <select
          value={session.currentClip}
          onchange={(e) => session.selectClip(+(e.currentTarget as HTMLSelectElement).value)}
          class="mb-2 w-full rounded border border-border bg-surface px-2 py-1 text-[11px] text-fg focus:border-accent focus:outline-none"
        >
          <option value={-1}>(No clip)</option>
          {#each session.clips as c, i (i)}
            <option value={i}>{c.name} · {fmtTime(c.duration)}</option>
          {/each}
        </select>
        {#if session.currentClip >= 0}
          {@const dur = session.clips[session.currentClip].duration}
          <div class="mb-1 flex items-center gap-1">
            <button
              type="button"
              onclick={() => session.toggleAnimationPlaying()}
              class="rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
              title={session.animationPlaying ? 'Pause' : 'Play'}
            >{session.animationPlaying ? '⏸' : '▶'}</button>
            <button
              type="button"
              onclick={() => { session.scrubAnimation(0); }}
              class="rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
              title="Rewind"
            >⏮</button>
            <span class="ml-auto font-mono text-[10px] text-fg-muted">
              {fmtTime(session.animationTime)} / {fmtTime(dur)}
            </span>
          </div>
          <input
            type="range" min="0" max={dur} step="0.01" value={session.animationTime}
            oninput={(e) => session.scrubAnimation(+(e.currentTarget as HTMLInputElement).value)}
            class="mb-2 w-full accent-accent"
          />
          <label class="mb-1 block">
            <span class="mb-0.5 flex items-center justify-between text-fg-muted">
              <span>Speed</span><span class="font-mono text-fg">{session.animationSpeed.toFixed(2)}×</span>
            </span>
            <input
              type="range" min="0.1" max="4" step="0.05" value={session.animationSpeed}
              oninput={(e) => session.setAnimationSpeed(+(e.currentTarget as HTMLInputElement).value)}
              class="w-full accent-accent"
            />
          </label>
          <label class="flex items-center justify-between text-fg-muted">
            <span>Loop</span>
            <button
              type="button"
              onclick={() => session.toggleAnimationLoop()}
              class="inline-flex h-5 w-9 items-center rounded-full transition-colors"
              class:bg-accent={session.animationLoop}
              class:bg-border={!session.animationLoop}
              role="switch"
              aria-checked={session.animationLoop}
              aria-label="Loop animation"
            >
              <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.animationLoop} class:translate-x-0.5={!session.animationLoop}></span>
            </button>
          </label>
        {/if}
      </section>
    {/if}

    <!-- ── 7. Stats ──────────────────────────────────────────── -->
    {#if session.stats}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Stats</h3>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-[10px]">
          <dt class="text-fg-muted">Vertices</dt><dd class="font-mono text-fg">{fmtCount(session.stats.vertices)}</dd>
          <dt class="text-fg-muted">Triangles</dt><dd class="font-mono text-fg">{fmtCount(session.stats.triangles)}</dd>
          <dt class="text-fg-muted">Meshes</dt><dd class="font-mono text-fg">{session.stats.meshes}</dd>
          <dt class="text-fg-muted">Materials</dt><dd class="font-mono text-fg">{session.stats.materials}</dd>
          <dt class="text-fg-muted">Textures</dt><dd class="font-mono text-fg">{session.stats.textures}</dd>
          <dt class="text-fg-muted">Draw calls</dt><dd class="font-mono text-fg">{session.stats.drawCalls}</dd>
          <dt class="text-fg-muted">File size</dt><dd class="font-mono text-fg">{fmtBytes(session.stats.fileSize)}</dd>
        </dl>
      </section>
    {/if}

    {#if !isMarmoset}
    <!-- ── Reset to defaults ─────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <button
        type="button"
        onclick={() => {
          if (confirm('Reset every 3D Viewer preference back to factory defaults? Your saved view for this asset will also be cleared.')) {
            session.resetAll();
          }
        }}
        class="w-full rounded border border-danger/40 bg-danger/10 px-2 py-1.5 text-[10px] font-medium text-fg hover:border-danger hover:bg-danger/20"
        title="Snap camera, environment, lighting, display, and materials back to the out-of-the-box defaults"
      >Reset to defaults</button>
    </section>
    {/if}

    <!-- ── 8. Annotations (placeholder for surface pins) ────── -->
    <section class="p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Annotations · coming soon</h3>
      <p class="text-[10px] leading-snug text-fg-muted">
        Click a point on the model to drop a comment pin. Pins anchor to the surface
        in object space, so they ride along when the camera orbits or the model
        animates — same model in another asset's viewport, same pin location.
      </p>
    </section>
  </div>
{/if}
