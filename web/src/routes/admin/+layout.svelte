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
  import AdminIcon from '$components/AdminIcon.svelte';
  import { ADMIN_SECTIONS } from '$lib/admin/sections';

  let { children } = $props();

  onMount(() => {
    if (!auth.user) void goto('/login?next=' + encodeURIComponent(page.url.pathname));
  });

  const isAdmin = $derived(auth.can('system.admin'));
  const aboutActive = $derived(page.url.pathname.startsWith('/admin/about'));
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
      <nav class="space-y-0.5 text-sm">
        <a
          href="/admin"
          class={`block rounded-md px-3 py-1.5 ${page.url.pathname === '/admin' ? 'bg-surface-elevated text-fg' : 'text-fg-muted hover:bg-surface-elevated/60 hover:text-fg'}`}
        >
          {t('admin_menu.overview')}
        </a>

        {#each ADMIN_SECTIONS as section (section.slug)}
          {@const href = `/admin/${section.slug}`}
          {@const active = page.url.pathname === href || page.url.pathname.startsWith(href + '/')}
          <a
            {href}
            class={`flex items-center gap-2 rounded-md px-3 py-1.5 ${active ? 'bg-surface-elevated text-fg' : 'text-fg-muted hover:bg-surface-elevated/60 hover:text-fg'}`}
          >
            <span class={active ? 'text-fg' : 'text-fg-muted'}>
              <AdminIcon name={section.iconKey} size={15} />
            </span>
            <span>{t(`admin.sections.${section.slug}.title`)}</span>
          </a>
        {/each}

        <div class="my-1 border-t border-border"></div>
        <a
          href="/admin/about"
          class={`flex items-center gap-2 rounded-md px-3 py-1.5 ${aboutActive ? 'bg-surface-elevated text-fg' : 'text-fg-muted hover:bg-surface-elevated/60 hover:text-fg'}`}
        >
          <span class={aboutActive ? 'text-fg' : 'text-fg-muted'}>
            <AdminIcon name="about" size={15} />
          </span>
          <span>{t('admin_menu.about')}</span>
        </a>
      </nav>

      <section>
        {@render children?.()}
      </section>
    </div>
  {/if}
</div>
