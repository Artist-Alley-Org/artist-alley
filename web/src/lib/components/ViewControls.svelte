<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The shared floating view-control bar (#511): the mode switcher
  // (grid / masonry / feed / thumbnail / list + size ±), the
  // sort-direction toggle and back-to-top, all driven by the global
  // browseView store. Extracted from BrowseFooter so every asset-showing
  // surface — browse, the profile pages, post-by-asset — mounts the SAME
  // controls instead of forking them.
  //
  // LAYOUT (#554): these used to be three clusters pinned across a
  // full-width `inset-x-4` bar — switcher + back-to-top hard left, the
  // filter centred, sort hard right — so the two controls you actually
  // use together sat a whole viewport apart and read as unrelated
  // floating islands. They are now ONE centred cluster: a single row of
  // adjacent controls, with the optional `middle` snippet stacked
  // directly above it and the expanded view choices above that. Same
  // components, same 44px targets — just gathered, so the pointer
  // travel between "change the view" and "flip the sort" is one button
  // instead of ~1900px on a wide screen.
  //
  // Browse-only chrome (the feed filter: team / trending / latest /
  // following) is NOT here — it's injected by BrowseFooter through the
  // optional `middle` snippet. Surfaces without a feed filter (profile,
  // post-by-asset) simply omit it and the row collapses to the controls.
  import type { Snippet } from 'svelte';
  import { browseView, type ViewMode } from '$stores/browseView.svelte';
  import { chromeScroll } from '$stores/chromeScroll.svelte';
  import { t } from '$stores/lang.svelte';

  /** Optional centre content (browse's feed filter). */
  let { middle }: { middle?: Snippet } = $props();

  // View catalogue — order chosen so the icons cluster naturally and the
  // default (grid) anchors centre when expanded. `feed` is the
  // one-column floor of the same tile scale, available at every width.
  const VIEWS: Array<{ id: ViewMode; labelKey: string; icon: string }> = [
    { id: 'grid',      labelKey: 'browse.view.grid',      icon: 'grid' },
    { id: 'masonry',   labelKey: 'browse.view.masonry',   icon: 'masonry' },
    { id: 'feed',      labelKey: 'browse.view.feed',      icon: 'feed' },
    { id: 'thumbnail', labelKey: 'browse.view.thumbnail', icon: 'thumbnail' },
    { id: 'list',      labelKey: 'browse.view.list',      icon: 'list' },
  ];

  let expanded = $state(false);

  $effect(() => chromeScroll.attach());

  const scrolled = $derived(chromeScroll.scrolled);
  // Keep the cluster on screen while the switcher is expanded — yanking
  // it away mid-interaction would be hostile.
  const hidden = $derived(chromeScroll.hidden && !expanded);

  const activeView = $derived(VIEWS.find((v) => v.id === browseView.mode) ?? VIEWS[0]);
  const otherViews = $derived(VIEWS.filter((v) => v.id !== browseView.mode));

  function pick(mode: ViewMode) {
    browseView.setMode(mode);
    expanded = false;
    // Keep the bar on screen after choosing (#554). `expanded` was the
    // only thing holding it visible, so collapsing the switcher handed
    // control straight back to a `hidden` that may already be true from
    // scrolling down mid-interaction — the bar vanished the instant you
    // picked. Clearing it in the store means the bar stays until the
    // next scroll-down, which then hides it normally.
    chromeScroll.reveal();
  }
  function dec() {
    browseView.decSize();
  }
  function inc() {
    browseView.incSize();
  }
  function toggle() {
    expanded = !expanded;
  }
  function backToTop() {
    // The app-shell's <main> is the scroll context on every surface that
    // mounts this bar.
    const main = document.querySelector('main');
    main?.scrollTo({ top: 0, behavior: 'smooth' });
  }

  // Escape collapses the expanded switcher so keyboard users can dismiss
  // without picking. Click-outside is handled by the floating wrapper.
  function onWindowKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && expanded) expanded = false;
  }
  $effect(() => {
    if (!expanded) return;
    window.addEventListener('keydown', onWindowKey);
    return () => window.removeEventListener('keydown', onWindowKey);
  });
</script>

<div
  data-testid="view-controls"
  class="chrome-slide pointer-events-none fixed inset-x-4 bottom-4 z-20 flex flex-col items-center gap-2 transition-transform duration-200 ease-out"
  class:chrome-hidden-bottom={hidden}
  style="padding-bottom: env(safe-area-inset-bottom, 0px)"
  aria-label={t('browse.footer.label')}
>
  <!-- ONE row (#590 amendment). The filter and the controls used to be
       two independently-centred rows stacked on each other; because they
       are different widths, nothing lined up and the cluster read as
       bunched and ragged. They now share a single horizontal row, so
       there is one alignment for the whole thing and the surfaces that
       pass no `middle` (UserProfile, posts/by-asset, the collection
       page) simply get a shorter version of the same row. -->
  <div class="pointer-events-auto flex flex-col items-center gap-1.5">
    {#if expanded}
      <div class="flex items-center gap-1.5">
      {#each otherViews as v (v.id)}
        <button
          type="button"
          onclick={() => pick(v.id)}
          title={t(v.labelKey)}
          aria-label={t(v.labelKey)}
          class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {#if v.icon === 'grid'}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="7" height="7" rx="1" />
              <rect x="14" y="3" width="7" height="7" rx="1" />
              <rect x="14" y="14" width="7" height="7" rx="1" />
              <rect x="3" y="14" width="7" height="7" rx="1" />
            </svg>
          {:else if v.icon === 'masonry'}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="7" height="10" rx="1" />
              <rect x="14" y="3" width="7" height="6" rx="1" />
              <rect x="3" y="16" width="7" height="5" rx="1" />
              <rect x="14" y="12" width="7" height="9" rx="1" />
            </svg>
          {:else if v.icon === 'thumbnail'}
            <!-- lucide `gallery-thumbnails` (#554): a large preview over a
                 strip of thumbs, which is what this view actually is. The
                 old glyph was a 3×3 of equal squares — indistinguishable
                 from the grid icon two buttons away. -->
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect width="18" height="14" x="3" y="3" rx="2" />
              <path d="M4 21h1" />
              <path d="M9 21h1" />
              <path d="M14 21h1" />
              <path d="M19 21h1" />
            </svg>
          {:else if v.icon === 'feed'}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="18" height="12" rx="1" />
              <line x1="3" y1="19" x2="14" y2="19" />
            </svg>
          {:else if v.icon === 'list'}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <line x1="8" y1="6"  x2="21" y2="6" />
              <line x1="8" y1="12" x2="21" y2="12" />
              <line x1="8" y1="18" x2="21" y2="18" />
              <line x1="3" y1="6"  x2="3.01" y2="6" />
              <line x1="3" y1="12" x2="3.01" y2="12" />
              <line x1="3" y1="18" x2="3.01" y2="18" />
            </svg>
          {/if}
        </button>
      {/each}
      </div>
    {/if}

    <div class="flex items-center gap-1.5">
      <!-- Surface-specific content leads the row (browse's feed filter);
           absent on every other surface, which just shortens the row. -->
      {#if middle}{@render middle()}{/if}

      {#if expanded}
        <button
          type="button"
          onclick={dec}
          disabled={!browseView.canDec}
          title={t('browse.footer.dec_size')}
          aria-label={t('browse.footer.dec_size')}
          class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-40"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
        </button>
      {/if}

      <button
        type="button"
        onclick={toggle}
        title={t('browse.footer.toggle')}
        aria-label={t('browse.footer.toggle')}
        aria-expanded={expanded}
        class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-accent text-on-accent shadow-lg transition-colors hover:bg-accent/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {#if activeView.icon === 'grid'}
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="3" y="3" width="7" height="7" rx="1" />
            <rect x="14" y="3" width="7" height="7" rx="1" />
            <rect x="14" y="14" width="7" height="7" rx="1" />
            <rect x="3" y="14" width="7" height="7" rx="1" />
          </svg>
        {:else if activeView.icon === 'masonry'}
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="3" y="3" width="7" height="10" rx="1" />
            <rect x="14" y="3" width="7" height="6" rx="1" />
            <rect x="3" y="16" width="7" height="5" rx="1" />
            <rect x="14" y="12" width="7" height="9" rx="1" />
          </svg>
        {:else if activeView.icon === 'feed'}
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="3" y="3" width="18" height="12" rx="1" />
            <line x1="3" y1="19" x2="14" y2="19" />
          </svg>
        {:else if activeView.icon === 'thumbnail'}
          <!-- lucide `gallery-thumbnails` — see the note on the twin above. -->
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect width="18" height="14" x="3" y="3" rx="2" />
            <path d="M4 21h1" />
            <path d="M9 21h1" />
            <path d="M14 21h1" />
            <path d="M19 21h1" />
          </svg>
        {:else}
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <line x1="8" y1="6"  x2="21" y2="6" />
            <line x1="8" y1="12" x2="21" y2="12" />
            <line x1="8" y1="18" x2="21" y2="18" />
            <line x1="3" y1="6"  x2="3.01" y2="6" />
            <line x1="3" y1="12" x2="3.01" y2="12" />
            <line x1="3" y1="18" x2="3.01" y2="18" />
          </svg>
        {/if}
      </button>

      {#if expanded}
        <button
          type="button"
          onclick={inc}
          disabled={!browseView.canInc}
          title={t('browse.footer.inc_size')}
          aria-label={t('browse.footer.inc_size')}
          class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-40"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
            <line x1="12" y1="5" x2="12" y2="19" />
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
        </button>
      {/if}

      <!-- Sort sits in the SAME row as the switcher (#554) — these are
           the two controls used together, so they are now neighbours
           rather than opposite edges of the viewport. -->
      <button
        type="button"
        onclick={() => browseView.toggleFeedDir()}
        title={browseView.feedDir === 'desc' ? t('browse.sort.newest_first') : t('browse.sort.oldest_first')}
        aria-label={t('browse.sort.toggle')}
        class="inline-flex h-11 items-center gap-1.5 rounded-full border border-border bg-surface-elevated px-4 text-sm text-fg shadow-lg transition-colors hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
    <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      {#if browseView.feedDir === 'desc'}
        <path d="M3 6h13" />
        <path d="M3 12h9" />
        <path d="M3 18h5" />
        <path d="m17 8 4 4-4 4" />
        <path d="M21 12H12" />
      {:else}
        <path d="M3 6h5" />
        <path d="M3 12h9" />
        <path d="M3 18h13" />
        <path d="m17 16 4-4-4-4" />
        <path d="M21 12H12" />
      {/if}
    </svg>
        <span class="hidden sm:inline">
          {browseView.feedDir === 'desc' ? t('browse.sort.newest') : t('browse.sort.oldest')}
        </span>
      </button>

      <!-- Back-to-top joins the same row instead of floating alone
           (#554). Only present once there's somewhere to go back to. -->
      {#if scrolled}
        <button
          type="button"
          onclick={backToTop}
          title={t('browse.footer.back_to_top')}
          aria-label={t('browse.footer.back_to_top')}
          class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <line x1="12" y1="19" x2="12" y2="5" />
            <polyline points="5 12 12 5 19 12" />
          </svg>
        </button>
      {/if}
    </div>
  </div>
</div>

<style>
  /* Slide the whole cluster below the viewport edge. `translate` only —
     compositable, so the scroll stays off the main thread. */
  .chrome-hidden-bottom {
    transform: translateY(calc(100% + 2rem));
  }
</style>
