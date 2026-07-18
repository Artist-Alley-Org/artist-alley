<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/about — version, license, product identity.
  //
  // Version is the running server's build version (the git tag baked
  // in via ldflags, or "dev"), fetched from GET /build-info (#406) —
  // no longer a hardcoded stub.

  import { onMount } from 'svelte';
  import { t } from '$stores/lang.svelte';
  import { api } from '$api/client';
  import { site } from '$stores/site.svelte';

  const LICENSE = 'AGPL-3.0-only';
  let version = $state('…');

  onMount(async () => {
    const r = await api.GET('/build-info');
    version = r.data?.version || 'unknown';
  });
</script>

<svelte:head><title>{t('admin.about.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.about.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('admin.about.intro')}</p>

<dl class="grid max-w-xl grid-cols-[10rem_1fr] gap-y-2 text-sm">
  <dt class="text-fg-muted">{t('admin.about.product')}</dt>
  <dd class="font-medium text-fg">artist-alley</dd>

  <dt class="text-fg-muted">{t('admin.about.version')}</dt>
  <dd class="font-mono text-fg">{version}</dd>

  <dt class="text-fg-muted">{t('admin.about.license')}</dt>
  <dd class="text-fg">{LICENSE}</dd>
</dl>
