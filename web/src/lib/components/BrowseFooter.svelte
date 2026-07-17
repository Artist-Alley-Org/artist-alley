<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Floating footer for the browse feed.
  //
  // Layout (left → right)
  //   [view switcher] [back-to-top]
  //                       … flex spacer …
  //                       [team | trending | latest | following]
  //                       … flex spacer …
  //                                                       [sort ↑↓]
  //
  // View switcher cycles between four modes — grid / masonry /
  // thumbnail / list — and the size ± knobs adjust the column count
  // within the current mode.
  //
  // Filter pill is a segmented control. Setting the active segment
  // updates browseView.filter, which +page.svelte feeds to /posts as
  // a query param. Backend support for trending / following / team
  // lands incrementally — until then the param is sent and the
  // server returns the default newest-first feed.
  //
  // Sort toggle flips the feed direction (asc ↔ desc).
  //
  // Back-to-top scrolls main to 0 and only shows once the user has
  // actually scrolled.

  import { browseView, type ViewMode, type FeedFilter } from '$stores/browseView.svelte';
  import { chromeScroll } from '$stores/chromeScroll.svelte';
  import { t } from '$stores/lang.svelte';

  // ── View catalogue. Order chosen so the icons cluster naturally:
  //    grid (square grid), masonry (offset columns), thumbnail (dense
  //    grid), list (rows). Keeping `grid` first means the default
  //    active button visually anchors centre when expanded.
  //    `feed` sits between masonry and thumbnail: it's the one-column
  //    floor of the same tile scale (image at full column width), and
  //    it's the default on coarse pointers. Available at every width —
  //    it's a layout you can want on a 4k panel, not a phone fallback.
  const VIEWS: Array<{ id: ViewMode; labelKey: string; icon: string }> = [
    { id: 'grid',      labelKey: 'browse.view.grid',      icon: 'grid' },
    { id: 'masonry',   labelKey: 'browse.view.masonry',   icon: 'masonry' },
    { id: 'feed',      labelKey: 'browse.view.feed',      icon: 'feed' },
    { id: 'thumbnail', labelKey: 'browse.view.thumbnail', icon: 'thumbnail' },
    { id: 'list',      labelKey: 'browse.view.list',      icon: 'list' },
  ];

  let expanded = $state(false);
  /** Mobile filter dropdown. Below `sm` the four segments don't fit
   *  beside the switcher + sort (measured: 498px needed, 343 available
   *  at 390px), so they collapse into one pill that opens a menu. */
  let filterOpen = $state(false);

  // Scroll state moved to $stores/chromeScroll: the header needs the
  // same signal, and a second listener on `main` would mean two
  // handlers per scroll event for one piece of information. Attach is
  // ref-counted, so this effect is still the thing that owns the
  // listener's lifetime here.
  //
  // Still doesn't reference `expanded`: that would tear down and re-run
  // the effect on every toggle, which used to close the menu the
  // instant it opened.
  $effect(() => chromeScroll.attach());

  const scrolled = $derived(chromeScroll.scrolled);
  // Keep the cluster on screen while the menu is open — yanking it
  // away mid-interaction would be hostile.
  const hidden = $derived(chromeScroll.hidden && !expanded);

  const activeView = $derived(VIEWS.find((v) => v.id === browseView.mode) ?? VIEWS[0]);
  const otherViews = $derived(VIEWS.filter((v) => v.id !== browseView.mode));

  // Filter pill catalogue. Order matches the user's spec: team
  // (group-scoped), trending (engagement), latest (newest), following
  // (subscribed users).
  const FILTERS: Array<{ id: FeedFilter; labelKey: string }> = [
    { id: 'team',      labelKey: 'browse.filter.team' },
    { id: 'trending',  labelKey: 'browse.filter.trending' },
    { id: 'latest',    labelKey: 'browse.filter.latest' },
    { id: 'following', labelKey: 'browse.filter.following' },
  ];

  /** The pill's label below `sm`. Falls back to `latest`, the store's
   *  own default, rather than to the first segment. */
  const activeFilter = $derived(FILTERS.find((f) => f.id === browseView.filter) ?? FILTERS[2]);

  function pick(mode: ViewMode) {
    browseView.setMode(mode);
    expanded = false;
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
    // The app-shell's <main> is the scroll context on browse.
    const main = document.querySelector('main');
    main?.scrollTo({ top: 0, behavior: 'smooth' });
  }

  // Close the expanded cluster on Escape so keyboard users can dismiss
  // without picking. Click-outside is handled by the floating wrapper.
  function onWindowKey(e: KeyboardEvent) {
    if (e.key !== 'Escape') return;
    if (filterOpen) filterOpen = false;
    else if (expanded) expanded = false;
  }

  $effect(() => {
    if (!expanded && !filterOpen) return;
    window.addEventListener('keydown', onWindowKey);
    return () => window.removeEventListener('keydown', onWindowKey);
  });
</script>

<!--
  The floating cluster spans the bottom of the browse viewport.
  `pointer-events-none` on the outer wrapper lets clicks pass through
  to the cards underneath; each button re-enables pointer events
  locally. Three regions:
    left    — view switcher + back-to-top
    middle  — segmented filter (team / trending / latest / following)
    right   — feed sort direction toggle
-->
<div
  class="chrome-slide pointer-events-none fixed inset-x-4 bottom-4 z-20 flex items-end gap-3 transition-transform duration-200 ease-out"
  class:chrome-hidden-bottom={hidden}
  style="padding-bottom: env(safe-area-inset-bottom, 0px)"
  aria-label={t('browse.footer.label')}
>
  <!-- LEFT cluster: view switcher + back-to-top -->
  <div class="flex items-end gap-3">
  <!-- View switcher -->
  <div class="pointer-events-auto flex flex-col items-center gap-1.5">
    {#if expanded}
      <!-- Top row: inactive view options laid out horizontally so the
           active button below acts as a centred anchor. -->
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
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3"  y="3"  width="4" height="4" rx="0.5" />
              <rect x="10" y="3"  width="4" height="4" rx="0.5" />
              <rect x="17" y="3"  width="4" height="4" rx="0.5" />
              <rect x="3"  y="10" width="4" height="4" rx="0.5" />
              <rect x="10" y="10" width="4" height="4" rx="0.5" />
              <rect x="17" y="10" width="4" height="4" rx="0.5" />
              <rect x="3"  y="17" width="4" height="4" rx="0.5" />
              <rect x="10" y="17" width="4" height="4" rx="0.5" />
              <rect x="17" y="17" width="4" height="4" rx="0.5" />
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

    <!-- Active-view row: [-] [active] [+] when expanded, just [active] collapsed -->
    <div class="flex items-center gap-1.5">
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
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="3"  y="3"  width="4" height="4" rx="0.5" />
            <rect x="10" y="3"  width="4" height="4" rx="0.5" />
            <rect x="17" y="3"  width="4" height="4" rx="0.5" />
            <rect x="3"  y="10" width="4" height="4" rx="0.5" />
            <rect x="10" y="10" width="4" height="4" rx="0.5" />
            <rect x="17" y="10" width="4" height="4" rx="0.5" />
            <rect x="3"  y="17" width="4" height="4" rx="0.5" />
            <rect x="10" y="17" width="4" height="4" rx="0.5" />
            <rect x="17" y="17" width="4" height="4" rx="0.5" />
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
    </div>
  </div>

  <!-- Back-to-top — hidden when main is at the top so the cluster
       stays minimal until the user actually scrolls. -->
  {#if scrolled}
  <button
    type="button"
    onclick={backToTop}
    title={t('browse.footer.back_to_top')}
    aria-label={t('browse.footer.back_to_top')}
    class="pointer-events-auto inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
  >
    <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <line x1="12" y1="19" x2="12" y2="5" />
      <polyline points="5 12 12 5 19 12" />
    </svg>
  </button>
  {/if}
  </div>

  <!-- MIDDLE cluster: feed filter.
       Two presentations of one control, swapped STRUCTURALLY (which is
       what breakpoints are for — neither is a resize of the other):

         below sm — a single pill showing the active filter, opening a
                    menu upward. Measured at 390px the three clusters
                    need 498px and have 343; the segmented control is
                    336px of that. Collapsing it to ~110px is what lets
                    the footer stay on ONE row, which matters more on a
                    phone than anywhere else — vertical space is the
                    scarce axis, and reclaiming it is the whole point of
                    hiding this bar on scroll.
         sm and up — the full segmented control, unchanged.
  -->
  <div class="flex flex-1 justify-center">
    <!-- Below sm: collapsed to a menu. `relative` anchors the popup;
         it opens upward (bottom-full) because this bar is pinned to the
         bottom of the viewport. -->
    <div class="pointer-events-auto relative sm:hidden">
      <button
        type="button"
        onclick={() => (filterOpen = !filterOpen)}
        aria-haspopup="menu"
        aria-expanded={filterOpen}
        aria-label={t('browse.filter.label')}
        class="inline-flex h-11 items-center gap-1.5 rounded-full border border-border bg-surface-elevated px-4 text-sm font-medium text-fg shadow-lg transition-colors hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {t(activeFilter.labelKey)}
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" class="transition-transform" class:rotate-180={filterOpen}>
          <polyline points="18 15 12 9 6 15" />
        </svg>
      </button>
      {#if filterOpen}
        <div
          role="menu"
          aria-label={t('browse.filter.label')}
          class="absolute bottom-full left-1/2 mb-2 min-w-[9rem] -translate-x-1/2 rounded-xl border border-border bg-surface-elevated p-1 shadow-lg"
        >
          {#each FILTERS as f (f.id)}
            {@const active = browseView.filter === f.id}
            <button
              type="button"
              role="menuitem"
              onclick={() => { browseView.setFilter(f.id); filterOpen = false; }}
              class={`block w-full rounded-lg px-3 py-2 text-left text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${active ? 'bg-accent text-on-accent' : 'text-fg hover:bg-state-hover'}`}
            >
              {t(f.labelKey)}
            </button>
          {/each}
        </div>
      {/if}
    </div>

    <!-- sm and up: the segmented control, unchanged. -->
    <div
      class="pointer-events-auto hidden items-center rounded-full border border-border bg-surface-elevated p-1 shadow-lg sm:inline-flex"
      role="tablist"
      aria-label={t('browse.filter.label')}
    >
      {#each FILTERS as f (f.id)}
        {@const active = browseView.filter === f.id}
        <button
          type="button"
          role="tab"
          aria-selected={active}
          onclick={() => browseView.setFilter(f.id)}
          class={`rounded-full px-4 py-1.5 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${active ? 'bg-accent text-on-accent' : 'text-fg-muted hover:text-fg'}`}
        >
          {t(f.labelKey)}
        </button>
      {/each}
    </div>
  </div>

  <!-- RIGHT cluster: sort direction toggle. -->
  <button
    type="button"
    onclick={() => browseView.toggleFeedDir()}
    title={browseView.feedDir === 'desc' ? t('browse.sort.newest_first') : t('browse.sort.oldest_first')}
    aria-label={t('browse.sort.toggle')}
    class="pointer-events-auto ml-auto inline-flex h-11 items-center gap-1.5 rounded-full border border-border bg-surface-elevated px-4 text-sm text-fg shadow-lg transition-colors hover:bg-surface-overlay focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
  >
    <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      {#if browseView.feedDir === 'desc'}
        <!-- arrows pointing down (newest first) -->
        <path d="M3 6h13" />
        <path d="M3 12h9" />
        <path d="M3 18h5" />
        <path d="m17 8 4 4-4 4" />
        <path d="M21 12H12" />
      {:else}
        <!-- arrows pointing up (oldest first) -->
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
</div>

<style>
  /* Slide the whole cluster below the viewport edge. `translate` only —
     compositable, so the scroll stays off the main thread. Animating
     `bottom` here would force layout on every frame of a flick.
     The extra 2rem clears the safe-area inset + shadow. */
  .chrome-hidden-bottom {
    transform: translateY(calc(100% + 2rem));
  }
</style>
