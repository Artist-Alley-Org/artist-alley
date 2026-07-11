<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // User menu — avatar dropdown in the navbar.
  //
  // Holds: profile, account settings, theme submenu, language
  // submenu, API tokens, sign out. The theme + language submenus
  // are inline (not real nested submenus) — clicking a row sets the
  // pref and the parent dropdown stays open so the user can see the
  // effect take hold.

  import { auth } from '$stores/auth.svelte';
  import { theme } from '$stores/theme.svelte';
  import { lang, t } from '$stores/lang.svelte';
  import { goto } from '$app/navigation';
  import Menu from '$components/Menu.svelte';
  import Avatar from '$components/Avatar.svelte';

  let themeOpen = $state(false);
  let langOpen = $state(false);

  async function handleSignOut() {
    await auth.logout();
    await goto('/login');
  }

  function pickTheme(p: 'light' | 'dark' | 'system') {
    theme.set(p);
    themeOpen = false;
  }

  function pickLocale(code: string) {
    void lang.set(code);
    langOpen = false;
  }

  const u = $derived(auth.user);
</script>

{#if u}
  <Menu align="right" triggerTestId="nav-user-menu-trigger" panelTestId="user-menu-panel">
    {#snippet trigger({ open })}
      <span
        class="inline-flex items-center gap-2 rounded-full p-1 pr-3 hover:bg-surface-elevated"
        title={t('nav.open_user_menu')}
      >
        <Avatar name={u.fullname || u.username} sizeClass="h-8 w-8" />
        <span class="hidden text-sm text-fg md:inline">
          {u.fullname || u.username}
        </span>
        <svg
          xmlns="http://www.w3.org/2000/svg"
          width="12"
          height="12"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2.5"
          class="text-fg-muted"
        >
          <path d={open ? 'm18 15-6-6-6 6' : 'm6 9 6 6 6-6'} />
        </svg>
      </span>
    {/snippet}

    <p class="px-3 pb-2 pt-1 text-xs text-fg-muted">
      {t('user_menu.signed_in_as', { username: u.username })}
    </p>
    <div class="border-t border-border"></div>

    <a
      href="/users/by-username/{u.username}"
      role="menuitem"
      class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated"
    >
      {t('user_menu.profile')}
    </a>
    <a
      href="/account/preferences"
      role="menuitem"
      class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated"
    >
      {t('user_menu.account_settings')}
    </a>

    <!-- Theme inline submenu -->
    <button
      type="button"
      role="menuitem"
      onclick={(e) => {
        e.stopPropagation();
        themeOpen = !themeOpen;
      }}
      class="flex w-full items-center justify-between px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated"
      aria-expanded={themeOpen}
    >
      <span>{t('user_menu.theme')}</span>
      <span class="text-xs text-fg-muted">
        {theme.pref === 'light' ? t('user_menu.theme_light') : theme.pref === 'dark' ? t('user_menu.theme_dark') : t('user_menu.theme_system')}
      </span>
    </button>
    {#if themeOpen}
      <div class="bg-surface-elevated/40 px-1 py-1">
        {#each [['system', t('user_menu.theme_system')], ['light', t('user_menu.theme_light')], ['dark', t('user_menu.theme_dark')]] as [code, label] (code)}
          <button
            type="button"
            role="menuitem"
            onclick={(e) => {
              e.stopPropagation();
              pickTheme(code as 'light' | 'dark' | 'system');
            }}
            class={`flex w-full items-center justify-between rounded px-3 py-1 text-xs hover:bg-surface ${theme.pref === code ? 'text-accent' : 'text-fg-muted'}`}
          >
            <span>{label}</span>
            {#if theme.pref === code}<span>✓</span>{/if}
          </button>
        {/each}
      </div>
    {/if}

    <!-- Language inline submenu -->
    <button
      type="button"
      role="menuitem"
      onclick={(e) => {
        e.stopPropagation();
        langOpen = !langOpen;
      }}
      class="flex w-full items-center justify-between px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated"
      aria-expanded={langOpen}
    >
      <span>{t('user_menu.language')}</span>
      <span class="text-xs text-fg-muted">
        {lang.locales.find((l) => l.code === lang.resolved)?.nativeName ?? lang.resolved}
      </span>
    </button>
    {#if langOpen}
      <div class="bg-surface-elevated/40 px-1 py-1">
        <button
          type="button"
          role="menuitem"
          onclick={(e) => { e.stopPropagation(); pickLocale(''); }}
          class={`flex w-full items-center justify-between rounded px-3 py-1 text-xs hover:bg-surface ${lang.pref === '' ? 'text-accent' : 'text-fg-muted'}`}
        >
          <span>{t('user_menu.theme_system')}</span>
          {#if lang.pref === ''}<span>✓</span>{/if}
        </button>
        {#each lang.locales as l (l.code)}
          <button
            type="button"
            role="menuitem"
            onclick={(e) => { e.stopPropagation(); pickLocale(l.code); }}
            class={`flex w-full items-center justify-between rounded px-3 py-1 text-xs hover:bg-surface ${lang.pref === l.code ? 'text-accent' : 'text-fg-muted'}`}
          >
            <span>{l.nativeName}{l.completionPct < 100 ? ` (${l.completionPct}%)` : ''}</span>
            {#if lang.pref === l.code}<span>✓</span>{/if}
          </button>
        {/each}
      </div>
    {/if}

    <a
      href="/account/tokens"
      role="menuitem"
      class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated"
    >
      {t('user_menu.api_tokens')}
    </a>

    <div class="border-t border-border"></div>
    <button
      type="button"
      role="menuitem"
      onclick={handleSignOut}
      data-testid="user-menu-sign-out"
      class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
    >
      {t('user_menu.sign_out')}
    </button>
  </Menu>
{/if}
