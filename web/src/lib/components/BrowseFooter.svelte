<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Browse's floating control bar. Since #511 the view switcher + sort
  // toggle live in the shared ViewControls component (reused by the
  // profile + post-by-asset pages); BrowseFooter is now just ViewControls
  // plus the browse-only feed filter (team / trending / latest /
  // following), injected as the centre `middle` snippet.
  //
  // Filter pill is a segmented control. Setting the active segment
  // updates browseView.filter, which +page.svelte feeds to /posts as a
  // query param. Backend support for trending / following / team lands
  // incrementally — until then the param is sent and the server returns
  // the default newest-first feed.
  import ViewControls from '$components/ViewControls.svelte';
  import { browseView, type FeedFilter } from '$stores/browseView.svelte';
  import { t } from '$stores/lang.svelte';

  let filterOpen = $state(false);

  const FILTERS: Array<{ id: FeedFilter; labelKey: string }> = [
    { id: 'team',      labelKey: 'browse.filter.team' },
    { id: 'trending',  labelKey: 'browse.filter.trending' },
    { id: 'latest',    labelKey: 'browse.filter.latest' },
    { id: 'following', labelKey: 'browse.filter.following' },
  ];

  /** The pill's label below `sm`. Falls back to `latest`, the store's own
   *  default, rather than to the first segment. */
  const activeFilter = $derived(FILTERS.find((f) => f.id === browseView.filter) ?? FILTERS[2]);

  function onWindowKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && filterOpen) filterOpen = false;
  }
  $effect(() => {
    if (!filterOpen) return;
    window.addEventListener('keydown', onWindowKey);
    return () => window.removeEventListener('keydown', onWindowKey);
  });
</script>

<ViewControls>
  {#snippet middle()}
    <!-- Two presentations of one control, swapped STRUCTURALLY:
           below sm — a single pill opening a menu upward (the segmented
                      control is 336px and doesn't fit beside the switcher
                      + sort at 390px);
           sm and up — the full segmented control. -->
    <div class="pointer-events-auto relative sm:hidden">
      <button
        type="button"
        onclick={() => (filterOpen = !filterOpen)}
        aria-haspopup="menu"
        aria-expanded={filterOpen}
        aria-label={t('browse.filter.label')}
        class="inline-flex h-11 items-center gap-1.5 rounded-full border border-border bg-surface-elevated px-4 text-sm font-medium text-fg shadow-lg transition-colors hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
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

    <div
      class="pointer-events-auto hidden items-center rounded-full border border-border bg-surface-elevated p-1 shadow-lg sm:inline-flex"
      role="tablist"
      aria-label={t('browse.filter.label')}
    >
      <!-- Inactive segments get a real BACKGROUND on hover, not just a
           text-colour change (#590 amendment 3): `hover:text-fg` alone
           moved fg-muted -> fg with no fill behind it, so Team /
           Trending / Following felt dead to the pointer. Active keeps
           the solid accent, so selected stays clearly distinct from
           hovered. -->
      {#each FILTERS as f (f.id)}
        {@const active = browseView.filter === f.id}
        <button
          type="button"
          role="tab"
          aria-selected={active}
          onclick={() => browseView.setFilter(f.id)}
          class={`rounded-full px-4 py-1.5 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${active ? 'bg-accent text-on-accent' : 'text-fg-muted hover:bg-surface-hover hover:text-fg'}`}
        >
          {t(f.labelKey)}
        </button>
      {/each}
    </div>
  {/snippet}
</ViewControls>
