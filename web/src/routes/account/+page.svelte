<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account home — full tile grid grouped by section.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
  import AdminIcon from '$components/AdminIcon.svelte';
  import { ACCOUNT_GROUPS, itemsByGroup } from '$lib/account/sections';
</script>

<svelte:head><title>{t('account.title')} — {site.name}</title></svelte:head>

<p class="mb-6 text-sm text-fg-muted">{t('account.overview.intro')}</p>

<!-- data-testid so tests can scope to the grid: the sidebar in
     +layout.svelte renders the SAME hrefs, so a bare
     a[href="/account/x"] locator matches twice. -->
<div class="space-y-8" data-testid="account-tiles">
  {#each ACCOUNT_GROUPS as group (group.id)}
    <section>
      <header class="mb-3 flex items-center gap-2">
        <span class="text-fg-muted"><AdminIcon name={group.iconKey} size={16} /></span>
        <h2 class="text-sm font-semibold uppercase tracking-wide text-fg-muted">
          {t(`account.groups.${group.id}.title`)}
        </h2>
      </header>

      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {#each itemsByGroup(group.id) as item (item.slug)}
          <a
            href={item.href}
            class="rounded-lg border border-border bg-surface-elevated p-4 transition-colors hover:border-accent/50"
          >
            <h3 class="text-sm font-medium text-fg">
              {t(`account.items.${item.slug}.title`)}
            </h3>
            <p class="mt-1 text-xs text-fg-muted">{t(`account.items.${item.slug}.blurb`)}</p>
          </a>
        {/each}
      </div>
    </section>
  {/each}
</div>
