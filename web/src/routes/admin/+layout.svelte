<script lang="ts">
  // /admin/* layout — sidebar nav + capability gate.
  //
  // Renders nothing (just a "no permission" panel) when the caller
  // lacks `system.admin`. The backend enforces every action anyway;
  // this is the UX hide.

  import { goto } from '$app/navigation';
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

  let { children } = $props();

  onMount(() => {
    if (!auth.user) void goto('/login?next=' + encodeURIComponent(page.url.pathname));
  });

  const isAdmin = $derived(auth.can('system.admin'));

  interface NavLink {
    href: string;
    label: () => string;
  }

  const links: NavLink[] = [
    { href: '/admin/users',          label: () => t('admin_menu.users') },
    { href: '/admin/roles',          label: () => t('admin_menu.roles') },
    { href: '/admin/workflow',       label: () => t('admin_menu.workflow') },
    { href: '/admin/fields',         label: () => t('admin_menu.fields') },
    { href: '/admin/resource-types', label: () => t('admin_menu.resource_types') },
    { href: '/admin/system',         label: () => t('admin_menu.system') },
    { href: '/admin/system/log',     label: () => t('admin_menu.system_log') },
  ];
</script>

<div class="mx-auto w-full max-w-7xl px-6 py-6">
  <header class="mb-6">
    <h1 class="text-2xl font-semibold">{t('admin.title')}</h1>
  </header>

  {#if !auth.ready}
    <p class="text-fg-muted">{t('common.loading')}</p>
  {:else if !isAdmin}
    <div class="rounded-lg border border-red-500/40 bg-red-500/10 p-4 text-sm text-red-700">
      {t('common.no_permission')}
    </div>
  {:else}
    <div class="grid grid-cols-1 gap-6 md:grid-cols-[16rem_1fr]">
      <nav class="space-y-1 text-sm">
        <a
          href="/admin"
          class={`block rounded-md px-3 py-1.5 ${page.url.pathname === '/admin' ? 'bg-surface-elevated text-fg' : 'text-fg-muted hover:bg-surface-elevated/60 hover:text-fg'}`}
        >
          Overview
        </a>
        {#each links as l (l.href)}
          {@const active = page.url.pathname === l.href || page.url.pathname.startsWith(l.href + '/')}
          <a
            href={l.href}
            class={`block rounded-md px-3 py-1.5 ${active ? 'bg-surface-elevated text-fg' : 'text-fg-muted hover:bg-surface-elevated/60 hover:text-fg'}`}
          >
            {l.label()}
          </a>
        {/each}
      </nav>

      <section>
        {@render children?.()}
      </section>
    </div>
  {/if}
</div>
