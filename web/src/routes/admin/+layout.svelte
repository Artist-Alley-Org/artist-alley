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
  const overviewActive = $derived(page.url.pathname === '/admin');
</script>

<!--
  Admin shell: fills <main>'s viewport area exactly (h-full) and
  prevents <main> from scrolling — sidebar + content each own their
  own scroll. The global layout puts the navbar above main and gives
  <main> overflow-y-auto for normal pages; here we override by making
  the admin root exactly the size of main, so main has nothing to
  scroll.

  Panes:
    * aside  — overflow-y-auto so the sidebar list can scroll if it
               ever grows past viewport. Never moves with the right
               pane's scroll.
    * section — overflow-y-auto for the right-hand content. Always
                scrolls independently of the sidebar.
-->
<div class="flex h-full overflow-hidden">
  {#if !auth.ready}
    <div class="flex-1 p-6 text-fg-muted">{t('common.loading')}</div>
  {:else if !isAdmin}
    <div class="flex-1 p-6">
      <div class="rounded-lg border border-danger/40 bg-danger-container p-4 text-sm text-on-danger-container">
        {t('common.no_permission')}
      </div>
    </div>
  {:else}
    <!--
      Sidebar reads at 15px (text-[15px]) — between text-sm (14) and
      text-base (16). The text-sm default was too thin to scan
      comfortably in dark mode.

      Background tier: `bg-surface-elevated` (solid, no alpha) so the
      sidebar reads as a distinct surface from the content column. The
      active-item fill uses `bg-accent-container` — a soft accent
      tint that ships with a paired `text-on-accent-container`
      foreground at definition time, so contrast is structurally
      guaranteed in both palettes.

      Inactive items use `text-fg-muted` against the elevated surface
      (≈7.5:1 in dark, ≈6:1 in light — AA body and AAA UI).
    -->
    <aside
      class="hidden h-full w-64 shrink-0 overflow-y-auto border-r border-border bg-surface-elevated px-3 py-4 md:block"
      aria-label={t('admin.title')}
    >
      <nav class="space-y-0.5 text-[15px]">
        <a
          href="/admin"
          class={`block rounded-md px-3 py-2 ${overviewActive ? 'bg-accent-container text-on-accent-container font-medium' : 'text-fg-muted hover:bg-state-hover hover:text-fg'}`}
        >
          {t('admin_menu.overview')}
        </a>

        {#each ADMIN_SECTIONS as section (section.slug)}
          {@const href = `/admin/${section.slug}`}
          {@const active = page.url.pathname === href || page.url.pathname.startsWith(href + '/')}
          <a
            {href}
            class={`flex items-center gap-2.5 rounded-md px-3 py-2 ${active ? 'bg-accent-container text-on-accent-container font-medium' : 'text-fg-muted hover:bg-state-hover hover:text-fg'}`}
          >
            <span class={active ? 'text-on-accent-container' : 'text-fg-muted'}>
              <AdminIcon name={section.iconKey} size={17} />
            </span>
            <span>{t(`admin.sections.${section.slug}.title`)}</span>
          </a>
        {/each}

        <div class="my-2 border-t border-border"></div>
        <a
          href="/admin/about"
          class={`flex items-center gap-2.5 rounded-md px-3 py-2 ${aboutActive ? 'bg-accent-container text-on-accent-container font-medium' : 'text-fg-muted hover:bg-state-hover hover:text-fg'}`}
        >
          <span class={aboutActive ? 'text-on-accent-container' : 'text-fg-muted'}>
            <AdminIcon name="about" size={17} />
          </span>
          <span>{t('admin_menu.about')}</span>
        </a>
      </nav>
    </aside>

    <section class="flex flex-1 min-w-0 flex-col overflow-y-auto px-6 py-6">
      {@render children?.()}
    </section>
  {/if}
</div>
