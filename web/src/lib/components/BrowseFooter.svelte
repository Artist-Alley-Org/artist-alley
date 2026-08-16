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
  import ViewControls from '$components/ViewControls.svelte';
  import FooterTabs from '$components/FooterTabs.svelte';
  import { browseView, type FeedFilter } from '$stores/browseView.svelte';
  import { t } from '$stores/lang.svelte';

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

<ViewControls>
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
</ViewControls>
