<script lang="ts">
  // /review — review / critique surface. Placeholder for now; the
  // full experience (request queues, approval states, per-asset
  // annotations layered on the post detail modal) lands in a
  // dedicated phase.

  import { t } from '$stores/lang.svelte';
</script>

<svelte:head><title>{t('nav.review')} — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-3xl px-6 py-12">
  <h1 class="font-display mb-2 text-3xl font-semibold">{t('nav.review')}</h1>
  <p class="mb-6 text-fg-muted">{t('review.intro')}</p>

  <div class="rounded-lg border border-dashed border-border bg-surface-elevated/50 p-6 text-sm text-fg-muted">
    <p>{t('review.coming_soon')}</p>
  </div>
</div>
