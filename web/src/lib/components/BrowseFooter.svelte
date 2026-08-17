<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Browse's floating control bar. Since #511 the view switcher + sort
  // toggle live in the shared ViewControls component (reused by the
  // profile + post-by-asset pages); BrowseFooter is now just ViewControls
  // plus the browse-only feed filter (latest / following), injected as
  // the centre `middle` snippet.
  //
  // Since #1106 the filter's MARKUP is shared too, as FooterTabs — the
  // segmented control, the below-`sm` menu pill and the ARIA wiring were
  // sixty lines here that a profile hosting Portfolio / About / Likes
  // would have had to reproduce. What stays here is the only part that
  // is actually about browse: which segments exist, and where the
  // selection lives.
  //
  // FILTERS is deliberately not a superset of what the server serves.
  // It used to carry `team` and `trending` too, neither of which was in
  // the `feed` enum — the pills rendered, the click did nothing the
  // server could see, and the user got the latest feed under another
  // name (#691). Every segment here must be a `FeedFilter`, and every
  // `FeedFilter` must be a value `GET /posts` accepts.
  //
  // #1166 adds the asset-type filter to the RIGHT cluster, beside sort,
  // through the same seam (`trailing`). It is browse-only for the same
  // reason the feed filter is: it filters a feed, and the surfaces that
  // share ViewControls do not have one.
  //
  // Its selection lives in the URL rather than in browseView, which is
  // the one place it deliberately differs from the feed filter beside
  // it. `?kind=` has to make a filtered wall shareable and the back
  // button correct — a store would survive navigation and describe
  // nothing about the page you are looking at. Same choice the team and
  // tag chips already made (#1113, #1123), so all three of the browse
  // page's narrowing controls read out of one place.
  import ViewControls from '$components/ViewControls.svelte';
  import FooterTabs from '$components/FooterTabs.svelte';
  import FeedKindFilter from '$components/FeedKindFilter.svelte';
  import { browseView, type FeedFilter } from '$stores/browseView.svelte';
  import { t } from '$stores/lang.svelte';

  let {
    kinds = [],
    onkinds,
  }: { kinds?: readonly string[]; onkinds?: (next: string[]) => void } = $props();

  /** The panel's own open state, lifted here so ViewControls can hold
   *  the bar on screen while it is up. */
  let kindOpen = $state(false);

  const FILTERS: Array<{ id: FeedFilter; labelKey: string }> = [
    { id: 'latest',    labelKey: 'browse.filter.latest' },
    { id: 'following', labelKey: 'browse.filter.following' },
  ];

  const DEFAULT_FILTER: FeedFilter = 'latest';

  const tabs = $derived(FILTERS.map((f) => ({ id: f.id, label: t(f.labelKey) })));

  /** Resolved BY ID rather than by position, so trimming or reordering
   *  FILTERS cannot silently point the fallback at some other segment. */
  const active = $derived(
    FILTERS.some((f) => f.id === browseView.filter) ? browseView.filter : DEFAULT_FILTER,
  );
</script>

<ViewControls trailingOpen={kindOpen}>
  {#snippet middle()}
    <!-- The selection lives in the browseView store, not in FooterTabs:
         it survives navigation and drives a query param, which a
         page-local `let` could not do. -->
    <FooterTabs
      {tabs}
      {active}
      label={t('browse.filter.label')}
      onSelect={(id) => browseView.setFilter(id as FeedFilter)}
    />
  {/snippet}

  {#snippet trailing()}
    <FeedKindFilter
      selected={kinds}
      bind:open={kindOpen}
      onapply={(next) => onkinds?.(next)}
    />
  {/snippet}
</ViewControls>
