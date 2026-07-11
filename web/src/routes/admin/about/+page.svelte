<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/about — version, build info, license, contributors.
  //
  // Real-ish: version comes from a compile-time constant baked into
  // the bundle. Future-phase work pulls live build info from a
  // /system/about backend endpoint.

  import { t } from '$stores/lang.svelte';

  const VERSION = '0.1.0-dev';
  const BUILD = 'phase-1.16';
  const LICENSE = 'BSD-3-Clause';
</script>

<svelte:head><title>{t('admin.about.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.about.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('admin.about.intro')}</p>

<dl class="grid max-w-xl grid-cols-[10rem_1fr] gap-y-2 text-sm">
  <dt class="text-fg-muted">{t('admin.about.product')}</dt>
  <dd class="font-medium text-fg">artist-alley</dd>

  <dt class="text-fg-muted">{t('admin.about.version')}</dt>
  <dd class="font-mono text-fg">{VERSION}</dd>

  <dt class="text-fg-muted">{t('admin.about.build')}</dt>
  <dd class="font-mono text-fg">{BUILD}</dd>

  <dt class="text-fg-muted">{t('admin.about.license')}</dt>
  <dd class="text-fg">{LICENSE}</dd>
</dl>
