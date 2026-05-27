<script lang="ts">
  // /admin/integrations/api — Scalar API Explorer.
  //
  // Stub: the Scalar embed lands in the next commit of phase 1.16.
  // This page exists so the menu/sidebar can link to a real URL.

  import { t } from '$stores/lang.svelte';
</script>

<svelte:head><title>{t('admin.api_explorer.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.api_explorer.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('admin.api_explorer.intro')}</p>

<div class="max-w-2xl rounded-lg border border-border bg-surface p-4 text-sm text-fg-muted">
  <p>{t('admin.api_explorer.coming_soon')}</p>
  <p class="mt-2">
    {t('admin.api_explorer.see_yaml')}
    <code class="rounded bg-surface-elevated px-1.5 py-0.5 font-mono text-xs">app/api/openapi.yaml</code>
  </p>
</div>
