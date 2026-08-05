<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import '../app.css';
  import { onMount } from 'svelte';
  import { theme } from '$stores/theme.svelte';
  import { auth } from '$stores/auth.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import { site } from '$stores/site.svelte';
  import BrandMark from '$components/BrandMark.svelte';
  import { lang, t } from '$stores/lang.svelte';
  import { upload } from '$stores/upload.svelte';
  import { appearance } from '$stores/appearance.svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import SearchBar from '$components/SearchBar.svelte';
  import { chromeScroll } from '$stores/chromeScroll.svelte';
  import MobileNavDrawer from '$components/MobileNavDrawer.svelte';
  import NavUploadButton from '$components/NavUploadButton.svelte';
  import UploadModal from '$components/upload/UploadModal.svelte';
  import UploadDropZone from '$components/upload/UploadDropZone.svelte';
  import UserMenu from '$components/UserMenu.svelte';
  import AdminMenu from '$components/AdminMenu.svelte';
  import ImpersonationBanner from '$components/ImpersonationBanner.svelte';
  import MessagesButton from '$components/MessagesButton.svelte';
  import NotificationsButton from '$components/NotificationsButton.svelte';
  import ExploreMenu from '$components/ExploreMenu.svelte';
  import CardTooltip from '$components/CardTooltip.svelte';

  let { children } = $props();

  onMount(() => {
    theme.init();
    // Appearance (admin-chosen font slots). Applies cached picks
    // synchronously then refreshes from /appearance in the background.
    // Public endpoint — runs even for anonymous visitors so logged-out
    // pages get the right brand fonts.
    appearance.init();
    // i18n: must run AFTER auth state has hydrated so user pref wins
    // over the cookie. +layout.ts has populated the auth store by now
    // via hydrateFrom, capabilities included (#871) — there is
    // deliberately no follow-up capability fetch here. One used to
    // live at this spot, and because it landed AFTER +layout.ts had
    // already flipped `ready`, the /admin gate got to decide with an
    // empty capability set and rendered "you don't have permission"
    // at real administrators until the second response arrived.
    lang.init();
    // Drop-anywhere-to-upload — install once globally. The store
    // returns a cleanup but layouts don't unmount in normal use, so
    // we ignore it.
    upload.installGlobalDragListeners();
  });

  // Re-apply the account's appearance + browse preferences when the
  // session identity changes (#677, #706).
  //
  // onMount above is not enough on its own. This layout mounts once and
  // never unmounts, so a visitor who lands on /login — or browses a
  // public install as a guest — has already run `theme.init()` and
  // possibly `browseView.init()` with no account to consult. Signing in
  // is a client-side navigation, so without this the preferences they
  // set on another device do not appear until a full reload.
  //
  // Keyed on `ref` rather than the preference values themselves: this
  // fires when WHO is signed in changes, not when what they prefer
  // does. Both callees are no-ops for any setting this device has made
  // locally, so re-running them can never overwrite a choice made here.
  $effect(() => {
    void auth.user?.ref;
    theme.syncFromAccount();
    browseView.applyAccountDefaults();
  });

  // Pages that show the global nav chrome. Login/setup keep the bare
  // layout; everything else gets the header.
  //
  // No longer gated on a session (#416). It was `!!auth.user` because
  // before public mode an anonymous visitor could not reach any page
  // that rendered chrome — so the condition was equivalent to "not
  // login/setup" and the auth term was free. With public routes it
  // stopped being free: a guest on /collections got a page with no
  // header at all, no way to search, and no way to sign in. The route
  // gate in +layout.ts decides who may be here; this only decides
  // whether the page has chrome, and every page that is not
  // login/setup does.
  const showChrome = $derived(
    page.url.pathname !== '/login' && page.url.pathname !== '/setup'
  );

  // The navbar search input is present on every authenticated page
  // — including /account/* and /admin/* — per memory
  // `feedback_navbar_search_always_visible`. Search is the primary
  // discovery affordance in a media library this size; gating it to
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

  // Auto-hiding chrome. The store owns the single scroll listener on
  // <main>; the browse footer attaches to the same one. Ref-counted, so
  // whichever mounts first installs it and the last to leave removes it.
  $effect(() => chromeScroll.attach());
  const chromeHidden = $derived(chromeScroll.hidden);
  /** Mobile nav drawer (below md). Holds the left-nav links, account
   *  menu, and admin sections — everything the bar drops on a phone. */
  let drawerOpen = $state(false);
  /** Measured height of the chrome layer (banners + header). Drives
   *  <main>'s padding-top — see the markup for why it's measured. */
  let chromeH = $state(0);

  // Publish the chrome layer's BOTTOM EDGE as --aa-chrome-bottom on the
  // root so overlays that want to sit under the chrome (the asset
  // viewer's windowed mode) can position against it in CSS.
  //
  // This lives here, not in the overlay, because this is the only place
  // that knows what the layer contains. The viewer used to measure
  // `document.querySelector('header')` itself and take its HEIGHT — a
  // number that is only the chrome's bottom edge when the header is the
  // *first* thing in the layer. Add a bar above it (the demo banner, the
  // impersonation banner) and the viewer's top chrome slid up behind the
  // navbar by exactly the bar's height (#688). Deriving it from the same
  // measured box <main> already pads by means the next bar added up
  // there is free — nothing to keep in sync, no second constant.
  //
  // Three inputs, and each is load-bearing:
  //   - chromeH      — the measured layer height (bind:clientHeight
  //                    below), so banners appearing/disappearing and the
  //                    header reflowing between one and two rows all
  //                    move the edge.
  //   - chromeHidden — the layer auto-hides via translateY(-100%), which
  //                    moves its bottom edge to 0 WITHOUT changing its
  //                    height. Nothing re-measures on a transform, so
  //                    this term is what makes the hide reactive
  //                    (#628/#629).
  //   - showChrome   — login/setup render no chrome at all; there is
  //                    nothing to sit below.
  $effect(() => {
    const bottom = showChrome && !chromeHidden ? chromeH : 0;
    document.documentElement.style.setProperty('--aa-chrome-bottom', `${bottom}px`);
  });
</script>

<!-- `h-dvh`, not `h-screen`. Tailwind's h-screen is 100vh, which on
     mobile Safari/Chrome means the viewport WITHOUT the browser chrome
     subtracted — so the shell runs taller than the visible area and the
     bottom of the page hides behind the URL bar. dvh tracks the chrome
     as it collapses. -->
<div class="relative flex h-dvh flex-col overflow-hidden bg-surface text-fg">
  {#if showChrome}
    <!--
      The chrome layer (banners + header) overlays <main> rather than
      sitting in the flex flow, so hiding it costs one transform and
      zero layout. If it stayed in flow, translating it up would leave
      a hole, and closing that hole means animating margin or height —
      main-thread layout on every frame, i.e. exactly the jank this is
      meant to remove.

      <main> carries the layer's measured height as padding-top, so the
      first screenful still starts below the chrome. The padding is only
      "wasted" space at scrollTop 0, where the chrome is visible anyway
      — by the time it hides (past 96px) that padding has scrolled off.

      bind:clientHeight measures instead of hardcoding: the header is
      two rows on a phone and one from md up, and the banners come and
      go. A constant would be wrong in at least three states.
    -->
    <div
      class="chrome-slide absolute inset-x-0 top-0 z-30 transition-transform duration-200 ease-out"
      class:chrome-hidden-top={chromeHidden}
      bind:clientHeight={chromeH}
    >
    <!-- Persistent impersonation banner — only renders when the
         active session was minted via /admin/users/{ref}/impersonate.
         Above the header so it stays visible across navigation. -->
    <ImpersonationBanner />
    {#if site.demoMode}
      <!-- Read-only demo banner (AA_DEMO_MODE). Only inside showChrome,
           so it's signed-in only; anonymous visitors see the login
           card's demo hint instead. Write-blocking is enforced at the
           edge — this is the visible half of that contract. -->
      <div
        class="shrink-0 border-b border-amber-500/40 bg-amber-500/15 px-6 py-1.5 text-center text-xs font-medium text-amber-700 dark:text-amber-300"
        data-testid="demo-banner"
        role="status"
      >
        {t('demo.banner')}
      </div>
    {/if}
    <!-- Header anchors at the top of the fixed-height shell. <main>
         is the scroll context for normal pages; admin overrides main
         with its own h-full / overflow-hidden flex container.
         backdrop-blur softens the boundary against image-heavy pages
         when scroll content sits underneath. -->
    <!--
      The header hides on scroll-down and returns on scroll-up (see
      $stores/chromeScroll). Maximum image real estate — and it's an
      ultrawide win before it's a phone one: a 3840x1080 32:9 panel has
      less vertical room than a tablet, and the chrome eats exactly
      that axis.

      Implemented with a transform + a negative margin rather than a
      height animation: transform is compositable (no main-thread
      layout per frame), and the margin lets <main> reclaim the space
      instead of leaving a hole where the header was.
    -->
    <header class="shrink-0 border-b border-border bg-surface-elevated text-base">
      <!-- Two rows on a narrow viewport, one row from `md` up. This is
           a genuine STRUCTURAL change (the shape of the header), which
           is what breakpoints are for — every size in here is fluid. -->
      <div class="flex flex-wrap items-center gap-x-3 gap-y-2 px-4 py-2 md:flex-nowrap md:gap-x-4 md:px-6 md:py-3">
        <!-- Hamburger — below md only. Opens the nav drawer that holds
             what the bar drops on a phone (nav links, account, admin).
             44x44 on coarse pointers via tap-target. -->
        <button
          type="button"
          onclick={() => (drawerOpen = true)}
          aria-label={t('nav.menu')}
          aria-expanded={drawerOpen}
          class="tap-target -ml-1 inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-md text-fg hover:bg-state-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring md:hidden"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <line x1="3" y1="6" x2="21" y2="6" />
            <line x1="3" y1="12" x2="21" y2="12" />
            <line x1="3" y1="18" x2="21" y2="18" />
          </svg>
        </button>
        <a href="/" class="font-brand flex shrink-0 items-center gap-2 text-xl tracking-tight md:text-2xl">
          <BrandMark class="h-8 w-8 md:h-10 md:w-10" />
          {site.name}
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
          <!-- Review is members-only (#416): offering it to a guest
               produces a nav entry that bounces to the sign-in page,
               which reads as broken rather than as gated. Same rule as
               the drawer's link filter. -->
          {#if auth.user}
            <a
              href="/review"
              class={`rounded-md px-3 py-1.5 text-sm font-medium ${reviewActive ? 'bg-state-selected text-fg' : 'text-fg hover:bg-state-hover'}`}
            >
              {t('nav.review')}
            </a>
          {/if}
        </nav>

        {#if showSearch}
          <!-- Search gets its OWN FULL-WIDTH ROW below `md`, and shares
               the row from `md` up.
               `order-last basis-full` wraps it onto a second flex line;
               `md:order-none md:basis-auto` puts it back inline.

               The standing rule is that search is never hidden. On a
               390px phone, sharing one row with the brand + five icon
               buttons honoured that rule to the letter and produced an
               82px input — visible, and useless. Its own row is the
               first time the rule is honoured in substance.

               (Scrolling away with the header is a different thing: the
               rule means "never route-gated", not "never scrolls".) -->
          <div class="order-last flex basis-full items-center gap-2 min-w-0 md:order-none md:flex-1 md:basis-auto">
            <div class="flex-1 min-w-0">
              <SearchBar
                bind:value={searchValue}
                onsearch={handleSearch}
                placeholder={t('nav.search_placeholder')}
              />
            </div>
            <!-- The entry point to the search SURFACE (#850).
                 It used to read "Advanced search" and it pointed here
                 already — the label named a page (`/search/advanced`)
                 that no longer exists, and the destination it actually
                 opens is now just search: one result grid, with the kind
                 filter, the facet counts and the query builder inside
                 it. So the control is named after where it goes.

                 It CARRIES whatever is in the box beside it. A control
                 labelled "Search" sitting next to a search input, that
                 navigates away and silently drops what you typed, is a
                 trap — and renaming it without wiring it would have
                 built one. Empty box ⇒ plain `/search`. -->
            <a
              href={searchValue.trim() ? `/search?q=${encodeURIComponent(searchValue.trim())}` : '/search'}
              title={t('nav.search_page')}
              aria-label={t('nav.search_page')}
              data-testid="nav-search-page"
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
              <span class="hidden lg:inline">{t('nav.search_page')}</span>
            </a>
          </div>
        {:else}
          <div class="hidden flex-1 md:block"></div>
        {/if}

        <!-- Right cluster, in order: upload (primary CTA), notifications,
             messages, user menu (avatar + name → settings dropdown),
             admin overflow menu. Theme + sign-out + language live
             inside the user menu. AdminMenu self-gates on system.admin
             so non-admins never see it. -->
        <!-- ml-auto so the cluster stays right-aligned when search
             wraps to its own row and no longer provides the flex push. -->
        <div class="ml-auto flex shrink-0 items-center gap-1.5 md:ml-0">
          <!-- Member-only controls (#416). Each one opens a surface a
               guest cannot use, so showing them signed-out would offer
               three buttons that all dead-end. UserMenu below renders
               its own signed-out state (a sign-in link) rather than
               disappearing, and AdminMenu already self-gates on
               capabilities a guest never has. -->
          {#if auth.user}
            <NavUploadButton />
            <NotificationsButton />
            <MessagesButton />
          {/if}
          <!-- UserMenu + AdminMenu are dropdown triggers; below md their
               CONTENTS move into the drawer, so hide the triggers there
               to avoid two ways in. Upload / notifications / messages
               stay inline at every width. -->
          <div class="hidden md:contents">
            <UserMenu />
            <AdminMenu />
          </div>
        </div>
      </div>
    </header>
    </div>
  {/if}

  <main
    class="flex flex-1 flex-col overflow-y-auto"
    style={showChrome ? `padding-top:${chromeH}px` : undefined}
  >
    {@render children?.()}
  </main>

  <MobileNavDrawer bind:open={drawerOpen} onclose={() => (drawerOpen = false)} />

  <!-- Masonry's hover tooltip (#652) — one instance for the whole app,
       fed by whichever card the pointer is over. NOT gated on auth: the
       browse wall is public, and the tooltip only surfaces facts the
       card already renders. Mounted here rather than per-card so moving
       between adjacent tiles swaps its contents instead of unmounting
       and re-running the show delay, which is what made a per-card
       tooltip strobe. -->
  <CardTooltip />

  {#if !!auth.user}
    <!-- Upload modal + drop overlay are gated on auth: only signed-in
         users can upload. Mounted globally so a drop anywhere on any
         page can open the modal. -->
    <UploadModal />
    <UploadDropZone />
  {/if}
</div>

<style>
  /* Slide the chrome layer up out of view. transform only — compositable,
     off the main thread, no layout per frame. 100% is the layer's own
     height, so this stays correct as the header reflows between one and
     two rows and as banners appear. */
  .chrome-hidden-top {
    transform: translateY(-100%);
  }
</style>
