<script lang="ts">
  // /search — advanced search builder.
  //
  // Stub for phase 1.16. Full implementation lands with the search
  // overhaul (phase 1.12) — filter panel + tag chips + saved searches.

  import { t } from '$stores/lang.svelte';
</script>

<svelte:head><title>{t('nav.advanced_search')} — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-3xl px-6 py-12">
  <h1 class="font-display mb-2 text-3xl font-semibold">{t('nav.advanced_search')}</h1>
  <p class="mb-6 text-fg-muted">{t('search.intro')}</p>

  <div class="rounded-lg border border-dashed border-border bg-surface-elevated/50 p-6 text-sm text-fg-muted">
    <p>{t('search.coming_soon')}</p>
  </div>
</div>
