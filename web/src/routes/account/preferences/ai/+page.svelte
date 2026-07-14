<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
</script>

<svelte:head><title>{t('account.ai.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('account.ai.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('account.ai.intro')}</p>

<div class="max-w-xl rounded-lg border border-border bg-surface-elevated p-4 text-sm text-fg-muted">
  <p>{t('account.ai.coming_soon')}</p>
</div>
