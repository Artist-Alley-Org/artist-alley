<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Mobile navigation drawer — the overflow home for everything the
  // navbar can't fit below `md`.
  //
  // The bar keeps the time-sensitive actions inline (upload,
  // notifications, messages); this drawer holds the rest: the left-nav
  // links, the account menu, and the admin sections. Above `md` it's
  // never rendered — the full bar shows all of it.
  //
  // It reuses the SAME sources the desktop menus use (ADMIN_SECTIONS,
  // auth, theme, lang) rather than duplicating their contents, so a new
  // admin section or a nav change shows up in both places automatically.

  import { auth } from '$stores/auth.svelte';
  import { theme } from '$stores/theme.svelte';
  import { t } from '$stores/lang.svelte';
  import { goto } from '$app/navigation';
  import Avatar from '$components/Avatar.svelte';
  import AdminIcon from '$components/AdminIcon.svelte';
  import { ADMIN_SECTIONS } from '$lib/admin/sections';

  interface Props {
    open: boolean;
    onclose: () => void;
  }
  let { open = $bindable(), onclose }: Props = $props();

  const u = $derived(auth.user);
  const isAdmin = $derived(auth.can('system.admin'));

  const NAV_LINKS: Array<{ href: string; labelKey: string }> = [
    { href: '/',            labelKey: 'nav.gallery' },
    { href: '/blogs',       labelKey: 'nav.blogs' },
    { href: '/collections', labelKey: 'nav.collections' },
    { href: '/review',      labelKey: 'nav.review' },
  ];

  // Navigating from a drawer link should close the drawer. SvelteKit
  // does client-side nav, so there's no reload to dismiss it for us.
  function close() {
    open = false;
    onclose();
  }

  function cycleTheme() {
    const order = ['system', 'light', 'dark'] as const;
    const next = order[(order.indexOf(theme.pref) + 1) % order.length];
    theme.set(next);
  }

  async function signOut() {
    close();
    await auth.logout();
    await goto('/login');
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && open) close();
  }

  $effect(() => {
    if (!open) return;
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  });
</script>

<!-- Scrim + panel are only in the DOM while open, so nothing below `md`
     pays for them at rest. `md:hidden` guarantees that if the viewport
     grows past md while open (rotate, resize), the drawer vanishes and
     the full bar takes over. -->
{#if open}
  <div class="fixed inset-0 z-40 md:hidden" role="presentation">
    <!-- Scrim. Click closes; transform-free fade honours reduced motion
         via the utility on the panel. -->
    <button
      type="button"
      class="absolute inset-0 bg-scrim"
      aria-label={t('common.close')}
      onclick={close}
    ></button>

    <!-- Panel. translate only (compositable). Width caps at 20rem so it
         never eats the whole phone, and pl/pt safe-area insets keep it
         clear of notch + home indicator. -->
    <nav
      class="chrome-slide absolute inset-y-0 left-0 flex w-[80%] max-w-[20rem] flex-col overflow-y-auto border-r border-border bg-surface-elevated shadow-xl"
      style="padding-top: env(safe-area-inset-top, 0px); padding-bottom: env(safe-area-inset-bottom, 0px); padding-left: env(safe-area-inset-left, 0px)"
      aria-label={t('nav.menu')}
    >
      <!-- Account header -->
      {#if u}
        <div class="flex items-center gap-3 border-b border-border px-4 py-3">
          <Avatar name={u.fullname || u.username} sizeClass="h-10 w-10" />
          <div class="min-w-0">
            <p class="truncate text-sm font-medium text-fg">{u.fullname || u.username}</p>
            <p class="truncate text-xs text-fg-muted">@{u.username}</p>
          </div>
        </div>
      {/if}

      <!-- Primary nav -->
      <div class="px-2 py-2">
        {#each NAV_LINKS as link (link.href)}
          <a
            href={link.href}
            onclick={close}
            class="tap-target flex items-center rounded-lg px-3 py-2.5 text-sm font-medium text-fg hover:bg-state-hover"
          >
            {t(link.labelKey)}
          </a>
        {/each}
      </div>

      <!-- Account actions -->
      {#if u}
        <div class="border-t border-border px-2 py-2">
          <a href="/users/by-username/{u.username}" onclick={close} class="tap-target flex items-center rounded-lg px-3 py-2.5 text-sm text-fg hover:bg-state-hover">
            {t('user_menu.profile')}
          </a>
          <a href="/account/preferences" onclick={close} class="tap-target flex items-center rounded-lg px-3 py-2.5 text-sm text-fg hover:bg-state-hover">
            {t('user_menu.account_settings')}
          </a>
          <a href="/account/tokens" onclick={close} class="tap-target flex items-center rounded-lg px-3 py-2.5 text-sm text-fg hover:bg-state-hover">
            {t('user_menu.api_tokens')}
          </a>
          <button
            type="button"
            onclick={cycleTheme}
            class="tap-target flex w-full items-center justify-between rounded-lg px-3 py-2.5 text-sm text-fg hover:bg-state-hover"
          >
            <span>{t('user_menu.theme')}</span>
            <span class="text-xs text-fg-muted">
              {theme.pref === 'light' ? t('user_menu.theme_light') : theme.pref === 'dark' ? t('user_menu.theme_dark') : t('user_menu.theme_system')}
            </span>
          </button>
          <button
            type="button"
            onclick={signOut}
            class="tap-target flex w-full items-center rounded-lg px-3 py-2.5 text-left text-sm text-fg hover:bg-state-hover"
          >
            {t('user_menu.sign_out')}
          </button>
        </div>
      {/if}

      <!-- Admin sections — gated on system.admin, same as the desktop
           AdminMenu. Render-time hide only; the backend enforces. -->
      {#if isAdmin}
        <div class="border-t border-border px-2 py-2">
          <a href="/admin" onclick={close} class="tap-target flex items-center rounded-lg px-3 py-2.5 text-sm font-medium text-fg hover:bg-state-hover">
            {t('admin_menu.overview')}
          </a>
          {#each ADMIN_SECTIONS as section (section.slug)}
            <a
              href={`/admin/${section.slug}`}
              onclick={close}
              class="tap-target flex items-center gap-2 rounded-lg px-3 py-2.5 text-sm text-fg hover:bg-state-hover"
            >
              <span class="text-fg-muted"><AdminIcon name={section.iconKey} size={16} /></span>
              <span>{t(`admin.sections.${section.slug}.title`)}</span>
            </a>
          {/each}
        </div>
      {/if}
    </nav>
  </div>
{/if}
