<script lang="ts">
  // DetailsTool body — the generic "what is this asset" surface.
  // Always available; this is the default body the standalone
  // /assets/[id] route gets when no host has overridden Details.
  // Hosts (PostHost) override by registering id='details' in their
  // customTools — the registry's merge-by-id replaces this body.
  //
  // 2D zoom presets render here for image/video/etc since the View
  // tool was retired and "Details" is the no-tool fallback. Per-
  // kind rich tools live in their own ToolDef now — 3D controls
  // moved to ModelTool, sprite controls to SpriteTool, etc — so
  // this body stays free of kind-specific surface code.

  import type { ToolContext } from '../contract';
  import SimilarAssetsPanel from '$components/SimilarAssetsPanel.svelte';

  let { ctx }: { ctx: ToolContext } = $props();

  const shellState = $derived(ctx.shellState);
  // 2D zoom presets are useful for everything that isn't 3D or
  // sprite (sprite owns its own integer-step pixel-perfect zoom;
  // 3D owns orbit/dolly via OrbitControls inside ModelView).
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


  <!-- Phase 1.14.B — vector similarity neighbours. Lazy-loads from
       /assets/{id}/similar; renders empty state when the embedding
       hasn't been computed yet (just-uploaded asset). -->
  <SimilarAssetsPanel assetId={ctx.asset.id} />

  {#if ctx.controller.kind === '3d' && ctx.modelSession && (ctx.asset.file_extension || '').toLowerCase().replace(/^\./, '') !== 'mview'}
    <!-- Kind-specific hint: when a richer tool exists for this asset,
         nudge the user toward it. ModelTool owns the rich 3D surface
         for glTF/FBX/OBJ; this fallback only renders the asset
         identity above. Hidden for .mview since Marmoset's WebViewer
         is closed-source and we don't surface a ModelTool for it. -->
    <p class="rounded border border-border bg-surface-elevated/60 p-2 text-[10px] leading-snug text-fg-muted">
      Switch to the <span class="font-medium text-fg">3D Viewer</span> tool above for camera / lighting / display / material controls.
    </p>
  {:else if ctx.controller.kind === '3d' && (ctx.asset.file_extension || '').toLowerCase().replace(/^\./, '') === 'mview'}
    <p class="rounded border border-border bg-surface-elevated/60 p-2 text-[10px] leading-snug text-fg-muted">
      <span class="font-medium text-fg">.mview</span> renders through Marmoset Toolbag's WebViewer. Hover the canvas for its built-in orbit, lighting, animation, and material controls.
    </p>
  {/if}
</div>
