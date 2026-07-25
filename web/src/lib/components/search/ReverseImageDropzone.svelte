<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Reverse-image search dropzone (Phase 1.55.W). Sits above the DSL
  // builder on /search/advanced. Accepts a drag+drop or click-selected
  // image, POSTs it multipart to the existing POST /search/by-image
  // endpoint (feature-complete since #199 + #205 + #206), and renders
  // the visually-similar hits as an AssetCard grid.
  //
  // The endpoint isn't in openapi.yaml, so we call it via raw fetch +
  // FormData rather than the typed client. Its response is a thin
  // {hits:[{asset_id, similarity}]} list, so we hydrate the top-K via
  // GET /assets/{id} to feed AssetCard (which needs file_hash +
  // asset_type for the thumbnail). No new frontend cache — the server
  // caches by SimilarityHintID.
  //
  // Visual search may be disabled server-side (search.visual.enabled=
  // false → the endpoint returns 501 sidecar_not_installed). There's no
  // client-readable flag, so we attempt-and-handle: a 501 flips the UI
  // to an explanatory "not configured" state.

  import { t } from '$stores/lang.svelte';
  import { api } from '$api/client';
  import AssetCard from '$components/AssetCard.svelte';
  import type { CardAsset } from '$components/cardAsset';

  // Cap the hydrate fan-out — reverse-image is a deliberate action, so
  // a top-30 grid is plenty and bounds the per-hit GET /assets/{id}.
  const TOP_K = 30;

  // The hit's asset is the shared card feed contract (#595) rather than
  // a local narrower copy. The local copy had drifted: it declared
  // neither `preview_available` nor a required `thumbhash`, so every
  // result tile got `previewAvailable === undefined` and CardThumb's
  // `showImage` gate never opened — the whole similarity grid rendered
  // as placeholders even though GET /assets/{id} had returned the flag
  // all along. Typing the field is what makes that impossible.
  interface HydratedHit {
    similarity: number;
    asset: CardAsset;
  }

  let file = $state<File | null>(null);
  let previewUrl = $state<string>('');
  let dragActive = $state(false);
  let loading = $state(false);
  let error = $state<string>('');
  let notConfigured = $state(false);
  let searched = $state(false);
  let results = $state<HydratedHit[]>([]);
  let fileInput: HTMLInputElement;

  function pickFile(f: File | null) {
    error = '';
    if (!f) return;
    if (!f.type.startsWith('image/')) {
      error = t('search.by_image.err_not_image');
      return;
    }
    if (previewUrl) URL.revokeObjectURL(previewUrl);
    file = f;
    previewUrl = URL.createObjectURL(f);
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    dragActive = false;
    const f = e.dataTransfer?.files?.[0] ?? null;
    pickFile(f);
  }

  function onDragOver(e: DragEvent) {
    e.preventDefault();
    dragActive = true;
  }

  function onDragLeave(e: DragEvent) {
    e.preventDefault();
    dragActive = false;
  }

  function onFileInput(e: Event) {
    const f = (e.target as HTMLInputElement).files?.[0] ?? null;
    pickFile(f);
  }

  function clear() {
    if (previewUrl) URL.revokeObjectURL(previewUrl);
    file = null;
    previewUrl = '';
    error = '';
    results = [];
    searched = false;
    notConfigured = false;
    if (fileInput) fileInput.value = '';
  }

  // Hydrate a single hit into an AssetCard-shaped asset. Returns null
  // if the asset can't be read (deleted / not visible) — those drop out
  // of the grid silently rather than rendering a broken card.
  async function hydrate(assetId: string, similarity: number): Promise<HydratedHit | null> {
    try {
      const r = await api.GET('/assets/{id}', { params: { path: { id: assetId } } });
      if (!r.data) return null;
      const a = r.data as unknown as HydratedHit['asset'];
      return { similarity, asset: a };
    } catch {
      return null;
    }
  }

  async function submit() {
    if (!file || loading) return;
    loading = true;
    error = '';
    notConfigured = false;
    results = [];
    try {
      const form = new FormData();
      form.append('file', file);
      const resp = await fetch(`/api/v1/search/by-image?limit=${TOP_K}`, {
        method: 'POST',
        body: form,
        credentials: 'same-origin',
      });

      if (resp.status === 501) {
        notConfigured = true;
        searched = true;
        return;
      }
      if (resp.status === 503) {
        error = t('search.by_image.err_unavailable');
        return;
      }
      if (resp.status === 413) {
        const body = (await resp.json().catch(() => ({}))) as { max_bytes?: number };
        const mb = body.max_bytes ? Math.floor(body.max_bytes / (1024 * 1024)) : 10;
        error = t('search.by_image.err_too_large', { mb });
        return;
      }
      if (resp.status === 429) {
        error = t('search.by_image.err_rate_limited');
        return;
      }
      if (!resp.ok) {
        error = t('search.by_image.err_generic');
        return;
      }

      const data = (await resp.json()) as { hits: Array<{ asset_id: string; similarity: number }> };
      const hydrated = await Promise.all(data.hits.map((h) => hydrate(h.asset_id, h.similarity)));
      results = hydrated.filter((h): h is HydratedHit => h !== null);
      searched = true;
    } catch {
      error = t('search.by_image.err_generic');
    } finally {
      loading = false;
    }
  }
</script>

<section class="mb-6 rounded-lg border border-border bg-surface p-4" data-testid="reverse-image-dropzone">
  <header class="mb-3">
    <h2 class="text-base font-semibold text-fg">{t('search.by_image.heading')}</h2>
    <p class="text-xs text-fg-muted">{t('search.by_image.hint')}</p>
  </header>

  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    role="button"
    tabindex="0"
    class="flex cursor-pointer flex-col items-center justify-center rounded-lg border-2 border-dashed px-6 py-8 text-center transition-colors {dragActive
      ? 'border-accent bg-accent/10'
      : 'border-border hover:border-border-strong'}"
    ondrop={onDrop}
    ondragover={onDragOver}
    ondragleave={onDragLeave}
    onclick={() => fileInput.click()}
    onkeydown={(e) => (e.key === 'Enter' || e.key === ' ') && fileInput.click()}
    data-testid="reverse-image-drop"
  >
    {#if previewUrl}
      <img src={previewUrl} alt={t('search.by_image.preview_alt')} class="mb-2 max-h-40 rounded object-contain" />
      <p class="text-xs text-fg-muted">{file?.name}</p>
    {:else}
      <svg xmlns="http://www.w3.org/2000/svg" width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="mb-2 text-fg-muted" aria-hidden="true">
        <rect x="3" y="3" width="18" height="18" rx="2" ry="2" />
        <circle cx="9" cy="9" r="2" />
        <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
      </svg>
      <p class="text-sm text-fg">{dragActive ? t('search.by_image.drag_active') : t('search.by_image.drop_prompt')}</p>
    {/if}
    <input
      bind:this={fileInput}
      type="file"
      accept="image/*"
      class="hidden"
      onchange={onFileInput}
      data-testid="reverse-image-file"
    />
  </div>

  <div class="mt-3 flex items-center gap-2">
    <button
      type="button"
      onclick={submit}
      disabled={!file || loading}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:opacity-50"
      data-testid="reverse-image-submit"
    >{loading ? t('search.by_image.searching') : t('search.by_image.submit')}</button>
    {#if file}
      <button
        type="button"
        onclick={clear}
        class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
      >{t('common.clear')}</button>
    {/if}
  </div>

  {#if error}
    <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger" data-testid="reverse-image-error">{error}</p>
  {/if}

  {#if notConfigured}
    <p class="mt-3 rounded border border-border bg-bg px-3 py-2 text-sm text-fg-muted" data-testid="reverse-image-not-configured">{t('search.by_image.err_not_configured')}</p>
  {/if}

  {#if searched && !notConfigured}
    <div class="mt-4" data-testid="reverse-image-results">
      {#if results.length === 0}
        <p class="text-sm text-fg-muted">{t('search.by_image.no_results')}</p>
      {:else}
        <h3 class="mb-2 text-sm font-medium text-fg">{t('search.by_image.results_count', { n: results.length })}</h3>
        <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5">
          {#each results as hit (hit.asset.id)}
            <div class="relative">
              <AssetCard asset={hit.asset} />
              <span
                class="absolute right-1 top-1 rounded bg-black/70 px-1.5 py-0.5 text-[10px] font-semibold text-white"
                data-testid="reverse-image-score"
              >{t('search.by_image.similarity_badge', { pct: Math.round(hit.similarity * 100) })}</span>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  {/if}
</section>
