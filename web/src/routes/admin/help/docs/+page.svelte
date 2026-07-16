<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/help/docs — documentation index.
  //
  // Static reference page: points at the published docs site and the
  // in-app API explorer, plus a short map of where each area is
  // documented. No backend — the canonical docs live at
  // artist-alley.org (built from the OSS docs/ tree).

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';

  const DOCS_URL = 'https://artist-alley.org';

  // In-app destinations worth surfacing from the docs landing.
  const links: { labelKey: string; href: string; external?: boolean }[] = [
    { labelKey: 'admin.help.docs.link_site', href: DOCS_URL, external: true },
    { labelKey: 'admin.help.docs.link_api', href: '/admin/integrations/api' },
    { labelKey: 'admin.help.docs.link_shortcuts', href: '/admin/help/shortcuts' },
    { labelKey: 'admin.help.docs.link_release', href: '/admin/help/release-notes' },
  ];
</script>

<svelte:head><title>{t('admin.help.docs.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.help.docs.title')}</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">{t('admin.help.docs.intro')}</p>

<ul class="max-w-xl space-y-2">
  {#each links as l (l.href)}
    <li>
      <a
        href={l.href}
        target={l.external ? '_blank' : undefined}
        rel={l.external ? 'noopener noreferrer' : undefined}
        class="flex items-center gap-2 rounded-lg border border-border bg-surface-elevated px-4 py-3 text-sm hover:border-accent"
      >
        <span class="font-medium text-fg">{t(l.labelKey)}</span>
        {#if l.external}<span class="text-xs text-fg-muted">↗</span>{/if}
      </a>
    </li>
  {/each}
</ul>
