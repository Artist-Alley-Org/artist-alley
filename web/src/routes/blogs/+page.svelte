<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /blogs — long-form writing surface. Stub for phase 1.16; the real
  // implementation (author UI + reader feed + per-post comments) lands
  // when the blogs feature opens in its dedicated phase.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
</script>

<svelte:head><title>{t('nav.blogs')} — {site.name}</title></svelte:head>

<div class="mx-auto w-full max-w-3xl px-6 py-12">
  <h1 class="font-display mb-2 text-3xl font-semibold">{t('nav.blogs')}</h1>
  <p class="mb-6 text-fg-muted">{t('blogs.intro')}</p>

  <div class="rounded-lg border border-dashed border-border bg-surface-elevated/50 p-6 text-sm text-fg-muted">
    <p>{t('blogs.coming_soon')}</p>
  </div>
</div>
