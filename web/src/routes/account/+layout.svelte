<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/* layout — grouped sidebar + content slot.
  //
  // Sections come from web/src/lib/account/sections.ts so the
  // sidebar, the overview tile grid, and the dynamic [stub] route
  // all stay in sync. Adding a future page is a one-line entry in
  // sections.ts.

  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import AdminIcon from '$components/AdminIcon.svelte';
  import { ACCOUNT_GROUPS, itemsByGroup } from '$lib/account/sections';

  let { children } = $props();

  onMount(() => {
    if (!auth.user) void goto('/login?next=' + encodeURIComponent(page.url.pathname));
  });
</script>

<div class="w-full px-6 py-6">
  <header class="mb-6">
    <h1 class="text-2xl font-semibold">{t('account.title')}</h1>
  </header>

  <div class="grid grid-cols-1 gap-6 md:grid-cols-[15rem_1fr]">
    <nav class="space-y-3 self-start rounded-lg border border-border bg-surface-elevated p-3 text-sm">
      <a
        href="/account"
        class={`block rounded-md px-3 py-1.5 ${page.url.pathname === '/account' ? 'bg-accent-container text-on-accent-container font-medium' : 'text-fg-muted hover:bg-state-hover hover:text-fg'}`}
      >
        {t('account.overview.title')}
      </a>

      {#each ACCOUNT_GROUPS as group (group.id)}
        <div class="space-y-0.5">
          <div class="flex items-center gap-2 px-3 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-fg-muted">
            <AdminIcon name={group.iconKey} size={13} />
            <span>{t(`account.groups.${group.id}.title`)}</span>
          </div>
          {#each itemsByGroup(group.id) as item (item.slug)}
            {@const active = page.url.pathname === item.href || page.url.pathname.startsWith(item.href + '/')}
            <a
              href={item.href}
              class={`flex items-center justify-between gap-2 rounded-md px-3 py-1.5 ${active ? 'bg-accent-container text-on-accent-container font-medium' : 'text-fg-muted hover:bg-state-hover hover:text-fg'}`}
            >
              <span>{t(`account.items.${item.slug}.title`)}</span>
              {#if item.status === 'stub' && item.phase}
                <span class="rounded bg-surface-elevated px-1.5 py-0.5 text-[10px] font-medium text-fg-muted">
                  {item.phase}
                </span>
              {/if}
            </a>
          {/each}
        </div>
      {/each}
    </nav>

    <section>
      {@render children?.()}
    </section>
  </div>
</div>
