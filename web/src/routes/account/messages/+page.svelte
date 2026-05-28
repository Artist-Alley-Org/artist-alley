<script lang="ts">
  import { t } from '$stores/lang.svelte';
</script>

<svelte:head><title>{t('account.messages.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('account.messages.title')}</h2>

<div class="max-w-xl rounded-lg border border-border bg-surface-elevated p-4 text-sm text-fg-muted">
  <p>{t('account.messages.coming_soon')}</p>
</div>
