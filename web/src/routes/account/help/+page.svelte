<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/help — where a signed-in user goes for help (#600).
  //
  // Deliberately NOT a mirror of /admin/help: that section is behind
  // the admin capability gate, so linking a plain user at
  // /admin/help/docs would land them on a "no permission" panel. Every
  // destination below is one this user can actually open — the public
  // docs site, the public issue tracker, and the /account copy of the
  // shortcut cheatsheet.
  //
  // No backend. If an install ever grows an operator-set support
  // contact, this is where it belongs.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
  import { DOCS_URL, ISSUES_URL } from '$lib/help/links';

  const links: {
    labelKey: string;
    descKey: string;
    href: string;
    external?: boolean;
  }[] = [
    {
      labelKey: 'account.help.link_docs',
      descKey: 'account.help.desc_docs',
      href: DOCS_URL,
      external: true,
    },
    {
      labelKey: 'account.help.link_shortcuts',
      descKey: 'account.help.desc_shortcuts',
      href: '/account/shortcuts',
    },
    {
      labelKey: 'account.help.link_issues',
      descKey: 'account.help.desc_issues',
      href: ISSUES_URL,
      external: true,
    },
  ];
</script>

<svelte:head><title>{t('account.help.title')} — {site.name}</title></svelte:head>

<section class="space-y-4">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('account.help.title')}</h1>
    <p class="mt-1 max-w-2xl text-sm text-fg-muted">{t('account.help.intro')}</p>
  </header>

  <ul class="max-w-2xl space-y-2" data-testid="help-links">
    {#each links as l (l.href)}
      <li>
        <a
          href={l.href}
          target={l.external ? '_blank' : undefined}
          rel={l.external ? 'noopener noreferrer' : undefined}
          class="block rounded-lg border border-border bg-surface-elevated px-4 py-3 hover:border-accent"
        >
          <span class="flex items-center gap-2 text-sm font-medium text-fg">
            {t(l.labelKey)}
            {#if l.external}<span class="text-xs text-fg-muted">↗</span>{/if}
          </span>
          <span class="mt-0.5 block text-xs text-fg-muted">{t(l.descKey)}</span>
        </a>
      </li>
    {/each}
  </ul>

  <p class="max-w-2xl text-xs text-fg-muted">{t('account.help.self_hosted_note')}</p>
</section>
