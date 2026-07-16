<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/help/release-notes — changelog pointer.
  //
  // Static: the canonical changelog is CHANGELOG.md in the repo, and
  // tagged releases carry generated notes on GitHub. This page shows
  // the running version and links out rather than duplicating the
  // changelog (which would drift). A future phase can surface the
  // notes for the running build from a /system/about endpoint.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';

  // Keep in sync with /admin/about until a build-info endpoint lands.
  const VERSION = '0.1.0-dev';
  const RELEASES_URL = 'https://github.com/mscrnt/artist-alley/releases';
  const CHANGELOG_URL = 'https://github.com/mscrnt/artist-alley/blob/main/CHANGELOG.md';
</script>

<svelte:head><title>{t('admin.help.release_notes.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.help.release_notes.title')}</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">{t('admin.help.release_notes.intro')}</p>

<dl class="mb-4 grid max-w-xl grid-cols-[10rem_1fr] gap-y-2 text-sm">
  <dt class="text-fg-muted">{t('admin.help.release_notes.running_version')}</dt>
  <dd class="font-mono text-fg">{VERSION}</dd>
</dl>

<ul class="max-w-xl space-y-2">
  <li>
    <a href={CHANGELOG_URL} target="_blank" rel="noopener noreferrer"
       class="flex items-center gap-2 rounded-lg border border-border bg-surface-elevated px-4 py-3 text-sm hover:border-accent">
      <span class="font-medium text-fg">{t('admin.help.release_notes.link_changelog')}</span>
      <span class="text-xs text-fg-muted">↗</span>
    </a>
  </li>
  <li>
    <a href={RELEASES_URL} target="_blank" rel="noopener noreferrer"
       class="flex items-center gap-2 rounded-lg border border-border bg-surface-elevated px-4 py-3 text-sm hover:border-accent">
      <span class="font-medium text-fg">{t('admin.help.release_notes.link_releases')}</span>
      <span class="text-xs text-fg-muted">↗</span>
    </a>
  </li>
</ul>
