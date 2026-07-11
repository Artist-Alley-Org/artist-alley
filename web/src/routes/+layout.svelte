<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import '../app.css';
  import { onMount } from 'svelte';
  import { theme } from '$stores/theme.svelte';
  import { auth } from '$stores/auth.svelte';
  import { lang, t } from '$stores/lang.svelte';
  import { upload } from '$stores/upload.svelte';
  import { appearance } from '$stores/appearance.svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import SearchBar from '$components/SearchBar.svelte';
  import NavUploadButton from '$components/NavUploadButton.svelte';
  import UploadModal from '$components/upload/UploadModal.svelte';
  import UploadDropZone from '$components/upload/UploadDropZone.svelte';
  import UserMenu from '$components/UserMenu.svelte';
  import AdminMenu from '$components/AdminMenu.svelte';
  import ImpersonationBanner from '$components/ImpersonationBanner.svelte';
  import MessagesButton from '$components/MessagesButton.svelte';
  import NotificationsButton from '$components/NotificationsButton.svelte';
  import ExploreMenu from '$components/ExploreMenu.svelte';

  let { children } = $props();

  onMount(() => {
    theme.init();
    // Appearance (admin-chosen font slots). Applies cached picks
    // synchronously then refreshes from /appearance in the background.
    // Public endpoint — runs even for anonymous visitors so logged-out
    // pages get the right brand fonts.
    appearance.init();
    // i18n: must run AFTER auth state has hydrated so user pref wins
    // over the cookie. +layout.ts has populated `auth.user` by now
    // via hydrateFrom — but caps don't ride that path, so we pull
    // them here.
    lang.init();
    // Caps load unconditionally — `refreshCaps` bails early when
    // there's no user. Without this, the admin menu stays hidden
    // even for admins because +layout.ts's hydrateFrom doesn't
    // populate caps (only user fields).
    void auth.refreshCaps();
    // Drop-anywhere-to-upload — install once globally. The store
    // returns a cleanup but layouts don't unmount in normal use, so
    // we ignore it.
    upload.installGlobalDragListeners();
  });

  // Pages that show the global nav chrome. Login/setup keep the bare
  // layout; everything else under the auth gate gets the header.
  const showChrome = $derived(
    !!auth.user && page.url.pathname !== '/login' && page.url.pathname !== '/setup'
  );

  // The navbar search input is present on every authenticated page
  // — including /account/* and /admin/* — per memory
  // `feedback_navbar_search_always_visible`. Search is the primary
  // discovery affordance in an ArtStation-shaped app; gating it to
  // the browse page friction-trained users to leave settings/admin
  // just to look something up. Submitting from a non-browse page
  // navigates to `/?q=...` in handleSearch.
  const showSearch = $derived(showChrome);

  // Active-state for the Collections nav link. Derived in script so we
  // don't run a {@const} inside <nav> (Svelte forbids that placement).
  const collectionsActive = $derived(page.url.pathname.startsWith('/collections'));
  const reviewActive = $derived(page.url.pathname.startsWith('/review'));

  // Search query is the source-of-truth in the URL so a refresh or
  // shared link reproduces the same result set.
  const urlQuery = $derived(page.url.searchParams.get('q') ?? '');
  let searchValue = $state('');

  // Keep the local input mirror in sync with the URL whenever the
  // location changes (e.g. back/forward, programmatic navigation).
  $effect(() => {
    searchValue = urlQuery;
  });

  async function handleSearch(q: string) {
    const trimmed = q.trim();
    // From the browse page (/), keep the user in place and update
    // the query string. From any other page (account, admin, post
    // detail, etc.), navigate TO the browse page with the query —
    // the search input is global per
    // `feedback_navbar_search_always_visible` and submitting from a
    // non-browse surface should land the user on the result feed.
    const isBrowse = page.url.pathname === '/';
    const target = isBrowse ? new URL(page.url) : new URL('/', page.url);
    if (trimmed === '') {
      target.searchParams.delete('q');
    } else {
      target.searchParams.set('q', trimmed);
    }
    await goto(target.pathname + target.search, { keepFocus: isBrowse, noScroll: isBrowse });
  }

  // Sign-out + theme cycling moved into UserMenu.
</script>

<div class="flex h-screen flex-col overflow-hidden bg-surface text-fg">
  {#if showChrome}
    <!-- Persistent impersonation banner — only renders when the
         active session was minted via /admin/users/{ref}/impersonate.
         Above the header so it stays visible across navigation. -->
    <ImpersonationBanner />
    <!-- Header anchors at the top of the fixed-height shell. <main>
         is the scroll context for normal pages; admin overrides main
         with its own h-full / overflow-hidden flex container.
         backdrop-blur softens the boundary against image-heavy pages
         when scroll content sits underneath. -->
    <header class="shrink-0 border-b border-border bg-surface-elevated text-base">
      <div class="flex items-center gap-4 px-6 py-3">
        <a href="/" class="font-brand flex items-center gap-2 text-2xl tracking-tight shrink-0">
          <img src="/logo.svg" alt="" class="h-7 w-7" aria-hidden="true" />
          artist-alley
        </a>

        <!-- Left nav: Explore dropdown (Gallery + Blogs) + Collections.
             Hidden on small screens so the navbar doesn't crowd; a
             mobile drawer lands as the responsive sweep starts. -->
        <nav class="hidden items-center gap-1 md:flex">
          <ExploreMenu />
          <a
            href="/collections"
            class={`rounded-md px-3 py-1.5 text-sm font-medium ${collectionsActive ? 'bg-state-selected text-fg' : 'text-fg hover:bg-state-hover'}`}
          >
            {t('nav.collections')}
          </a>
          <a
            href="/review"
            class={`rounded-md px-3 py-1.5 text-sm font-medium ${reviewActive ? 'bg-state-selected text-fg' : 'text-fg hover:bg-state-hover'}`}
          >
            {t('nav.review')}
          </a>
        </nav>

        {#if showSearch}
          <!-- Search fills all space between the left nav and the
               right cluster. The advanced-search button is attached
               to the right edge of the bar so it reads as a single
               input + filter pair. -->
          <div class="flex flex-1 items-center gap-2 min-w-0">
            <div class="flex-1 min-w-0">
              <SearchBar
                bind:value={searchValue}
                onsearch={handleSearch}
                placeholder={t('nav.search_placeholder')}
              />
            </div>
            <a
              href="/search"
              title={t('nav.advanced_search')}
              aria-label={t('nav.advanced_search')}
              class="inline-flex h-9 shrink-0 items-center gap-1.5 rounded-md border border-border bg-surface px-3 py-1.5 text-sm text-fg-muted hover:bg-state-hover hover:text-fg"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <line x1="4" y1="6"  x2="20" y2="6" />
                <line x1="4" y1="12" x2="14" y2="12" />
                <line x1="4" y1="18" x2="9"  y2="18" />
                <circle cx="17" cy="12" r="1.5" />
                <circle cx="12" cy="18" r="1.5" />
                <circle cx="19" cy="6"  r="1.5" />
              </svg>
              <span class="hidden lg:inline">{t('nav.advanced_search')}</span>
            </a>
          </div>
        {:else}
          <div class="flex-1"></div>
        {/if}

        <!-- Right cluster, in order: upload (primary CTA), notifications,
             messages, user menu (avatar + name → settings dropdown),
             admin overflow menu. Theme + sign-out + language live
             inside the user menu. AdminMenu self-gates on system.admin
             so non-admins never see it. -->
        <div class="flex items-center gap-1.5 shrink-0">
          <NavUploadButton />
          <NotificationsButton />
          <MessagesButton />
          <UserMenu />
          <AdminMenu />
        </div>
      </div>
    </header>
  {/if}

  <main class="flex flex-1 flex-col overflow-y-auto">
    {@render children?.()}
  </main>

  {#if !!auth.user}
    <!-- Upload modal + drop overlay are gated on auth: only signed-in
         users can upload. Mounted globally so a drop anywhere on any
         page can open the modal. -->
    <UploadModal />
    <UploadDropZone />
  {/if}
</div>
