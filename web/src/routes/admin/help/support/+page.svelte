<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/help/support — how to get help / file issues.
  //
  // Static reference. Self-hosted OSS: support is the docs site plus
  // the project's GitHub issue tracker; there's no vendor helpdesk.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';

  const ISSUES_URL = 'https://github.com/mscrnt/artist-alley/issues';
  const DOCS_URL = 'https://artist-alley.org';

  const links: { labelKey: string; descKey: string; href: string }[] = [
    { labelKey: 'admin.help.support.link_docs', descKey: 'admin.help.support.desc_docs', href: DOCS_URL },
    { labelKey: 'admin.help.support.link_issues', descKey: 'admin.help.support.desc_issues', href: ISSUES_URL },
  ];
</script>

<svelte:head><title>{t('admin.help.support.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.help.support.title')}</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">{t('admin.help.support.intro')}</p>

<ul class="max-w-xl space-y-2">
  {#each links as l (l.href)}
    <li>
      <a href={l.href} target="_blank" rel="noopener noreferrer"
         class="block rounded-lg border border-border bg-surface-elevated px-4 py-3 hover:border-accent">
        <span class="flex items-center gap-2 text-sm font-medium text-fg">{t(l.labelKey)} <span class="text-xs text-fg-muted">↗</span></span>
        <span class="mt-0.5 block text-xs text-fg-muted">{t(l.descKey)}</span>
      </a>
    </li>
  {/each}
</ul>

<p class="mt-4 max-w-2xl text-xs text-fg-muted">{t('admin.help.support.self_hosted_note')}</p>
