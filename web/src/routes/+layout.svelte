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
  import MessagesButton from '$components/MessagesButton.svelte';

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

  // Pages that get the search box rendered into the nav. Currently
  // only the browse page (/) — when other browsable surfaces land
  // (collection detail, user gallery, etc.) we'll widen this gate.
  const showSearch = $derived(showChrome && page.url.pathname === '/');

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
    const target = new URL(page.url);
    if (trimmed === '') {
      target.searchParams.delete('q');
    } else {
      target.searchParams.set('q', trimmed);
    }
    // keepFocus avoids stealing focus from the still-typing input.
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }

  // Sign-out + theme cycling moved into UserMenu.
</script>

<div class="min-h-screen flex flex-col bg-surface text-fg">
  {#if showChrome}
    <!-- Header uses base 1rem (16px) — the web body-text gold standard.
         Brand stays slightly larger; everything else inherits 1rem so
         the navbar reads at comfortable scanning distance on big
         monitors. -->
    <header class="border-b border-border bg-surface-elevated text-base">
      <div class="flex items-center gap-4 px-6 py-3">
        <a href="/" class="font-brand flex items-center gap-2 text-2xl tracking-tight shrink-0">
          <span class="inline-block h-6 w-6 rounded bg-accent"></span>
          artist-alley
        </a>

        {#if showSearch}
          <div class="flex-1 max-w-2xl mx-auto">
            <SearchBar
              bind:value={searchValue}
              onsearch={handleSearch}
              placeholder={t('nav.search_placeholder')}
            />
          </div>
        {:else}
          <div class="flex-1"></div>
        {/if}

        <!-- Right cluster: upload button, messages, user menu, admin
             menu. Theme + sign-out + language live inside the user
             menu. AdminMenu self-gates on system.admin so non-admins
             never see it. -->
        <div class="flex items-center gap-2 shrink-0">
          <NavUploadButton />
          <MessagesButton />
          <UserMenu />
          <AdminMenu />
        </div>
      </div>
    </header>
  {/if}

  <main class="flex-1 flex flex-col">
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
