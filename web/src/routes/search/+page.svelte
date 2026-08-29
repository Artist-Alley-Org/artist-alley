<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /search — ONE result surface (#850).
  //
  // This page used to render its own text cards beside a fixed 16rem
  // facet rail while every other browse surface — the home feed, the
  // profile grids, a collection, "shared with me" — rendered the same
  // rows through ContentGrid + AssetCard / PostCard / CollectionCard.
  // Searching therefore felt like leaving the app: the same artwork you
  // had been looking at as a tile came back as a line of text with
  // `score 1.000` next to it.
  //
  // It now renders through the SAME ContentGrid, driven by the SAME
  // global browseView store, with the SAME cards and the same hover
  // previews and view modes. The card snippet switches on `hit.type`,
  // the way UserProfile already does for its three mixed sections. What
  // made that possible is the card payload the engine started shipping
  // in #850 (app/internal/search/cards.go) — a hit carries what a tile
  // renders from, so nothing here has to hydrate an id through a second
  // endpoint.
  //
  // PostParamHost is mounted for the same reason every card-showing
  // surface mounts it (#1130): PostCard's primary click writes
  // `?post={id}` onto whatever URL it is on, so without a host here the
  // click would dead-end.
  //
  // LAYOUT. No `max-w`. Every other browse surface is full-viewport
  // width (the standing direction: 1080p baseline, 4K for art houses),
  // and the old `max-w-6xl` capped results at ~1152px — on a 3840px
  // display that is two thirds of the screen left empty beside a grid
  // whose whole job is showing a lot of work at once. There is no reason
  // for search to be the exception, so it is not one.
  //
  // FACETS. The rail is gone (#901). Counts live in a slide-over; the
  // KIND filter (artwork / posts / collections) lives as chips over the
  // grid because it is a different KIND of question — what am I looking
  // at, rather than which of these.
  //
  // The buckets in that slide-over are now CONTROLS (#907). They were
  // counts-only for five releases because GET /search took no facet
  // filter parameter: the DSL compiled a Filters struct and the engine
  // ignored it, so a checkbox beside a bucket would have been an
  // affordance that could not do anything. It had been exactly that
  // once — the old sidebar's checkboxes set a local Set and re-queried
  // nothing — and removing it was the honest half of keeping the counts.
  //
  // The engine plumbing landed with this change, so the checkboxes are
  // back and real. A tick appends `filter=<dimension>:<value>` to both
  // /search and /search/facets, which is why the counts re-narrow as you
  // pick: the rail describes the page beside it rather than the corpus.

  import { onMount, tick, untrack } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { page, navigating } from '$app/state';
  import { goto } from '$app/navigation';
  import { t } from '$stores/lang.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import ThumbButtons from '$components/search/ThumbButtons.svelte';
  import SearchSlideOver from '$components/search/SearchSlideOver.svelte';
  import AdvancedQueryBuilder from '$components/search/AdvancedQueryBuilder.svelte';
  import ContentGrid from '$components/ContentGrid.svelte';
  import AssetCard from '$components/AssetCard.svelte';
  import PostCard from '$components/PostCard.svelte';
  import CollectionCard from '$components/CollectionCard.svelte';
  import PostParamHost from '$components/PostParamHost.svelte';
  import ViewControls from '$components/ViewControls.svelte';
  import { createScrollSnapshot } from '$lib/util/scrollSnapshot';
  import { createInfiniteScroll } from '$lib/util/infiniteScroll.svelte';
  import { resetResultsScroll } from '$lib/util/resultsScroll';
  import {
    hitAsCardAsset,
    hitAsCollection,
    hitAsPost,
    hitMemberCount,
    type HitType,
    type SearchHit,
  } from '$lib/search/hitCards';

  type SearchResponse = {
    hits: SearchHit[];
    next_cursor: string;
    total_count: number;
    total_count_capped: boolean;
    types_matched: string[];
  };

  type FacetBucket = { value: string; count: number; label?: string };
  type FacetResult = { type: string; buckets: FacetBucket[]; timed_out?: boolean };
  type FacetsResponse = { facets: Record<string, FacetResult> };

  let q = $state('');
  let hits = $state<SearchHit[]>([]);
  let cursor = $state('');
  let totalCount = $state(0);
  let totalCountCapped = $state(false);
  let loading = $state(false);
  let loadingMore = $state(false);
  let error = $state('');
  let facets = $state<Record<string, FacetResult>>({});
  /** The results region. Only ever used to FIND the scrollport (#1298);
   *  see resultsScroll.ts for why the app cannot address `window`. */
  let resultsEl: HTMLElement | undefined = $state();
  /** The paging sentinel at the list's tail (#1354). */
  let sentinel: HTMLElement | undefined = $state();
  /** Is there another page to ask for? The cursor is the whole answer,
   *  and it is `''` rather than null when there is not. */
  const hasMore = $derived(cursor !== '');
  // The kind filter. Empty = everything, which is what /search does when
  // `types=` is absent.
  let kinds = $state<HitType[]>([]);
  // The FACET filter (#907), as the canonical `dimension:value` tokens
  // the API takes — not a nested map. The wire form IS the state: it is
  // what goes in the URL, what both fetches send, and what a bucket's
  // checkbox tests membership against, so there is nothing to serialise
  // and nothing that can be serialised two ways.
  let filters = $state<string[]>([]);
  const filterToken = (type: string, value: string) => `${type}:${value}`;
  let facetsOpen = $state(false);
  let advancedOpen = $state(false);
  // Save-as-collection modal state.
  let saveOpen = $state(false);
  let saveName = $state('');
  // Phase 1.16.B-4 — Save-search modal state. Distinct from
  // save-as-collection so a user can persist EITHER a one-shot
  // snapshot (collection) OR an ongoing notification target
  // (saved_search) without one closing the other.
  let saveSearchOpen = $state(false);
  let saveSearchName = $state('');
  let saveSearchInterval = $state(60);
  let saveSearchChannel = $state<'email' | 'none'>('email');
  let savingSearch = $state(false);
  let saveSearchResult = $state('');
  let saving = $state(false);
  let saveResult = $state('');

  /** IS THERE A SEARCH TO RUN? (#1157)
   *
   *  One predicate, because there used to be seven copies of `!q` and
   *  they all said the same wrong thing: that a search is text or it is
   *  nothing. A facet selection is a complete question on its own —
   *  "everything at pipeline stage Final" is the primary thing the
   *  advanced page exists to ask — and the dead arm was never specific
   *  to its `field:` dimension: `/search?filter=extension:png` with no
   *  `q` sat on the "Search the library" prompt too, and had since #907.
   *
   *  `kinds` is deliberately NOT part of this. A kind chip narrows a
   *  result set; on its own "assets" is not a question, it is every
   *  asset on the instance, which is browse rather than search.
   *
   *  Anything asking "may I query now" reads THIS, never `q` — including
   *  the markup, so the idle prompt and the no-matches line agree with
   *  what actually ran. */
  const hasRunnableQuery = $derived(q.trim() !== '' || filters.length > 0);

  const activeCount = $derived.by(() => {
    if (totalCountCapped) return '10,000+';
    return totalCount.toLocaleString();
  });

  // dsl mode kicks in when the URL had ?dsl= — from the advanced builder
  // panel or a "Find similar assets" nav.
  let dslMode = $state(false);

  // The kind chips. THREE of them and no more, because these are the
  // three things a hit can be. They stay separate from the facet
  // buckets: `?types=` asks what KIND of row you want, the buckets ask
  // which rows, and a user who unticks "Posts" has not filtered by a
  // property of anything.
  const kindOptions: Array<{ id: HitType; labelKey: string }> = [
    { id: 'asset', labelKey: 'search.kind.assets' },
    { id: 'post', labelKey: 'search.kind.posts' },
    { id: 'collection', labelKey: 'search.kind.collections' },
  ];

  const facetTotal = $derived(
    Object.values(facets).reduce((n, f) => n + (f.buckets?.length ?? 0), 0),
  );

  // Facet group headings. The old rail printed the raw aggregator key —
  // `ASSET_TYPE`, `EXTENSION` — which is a wire identifier, not a word.
  // Unknown keys fall back to the key so a new aggregator is visible
  // (untranslated) rather than invisible.
  const FACET_LABELS: Record<string, string> = {
    asset_type: 'search.facet.asset_type',
    tag: 'search.facet.tag',
    sensitivity: 'search.facet.sensitivity',
    owner: 'search.facet.owner',
    extension: 'search.facet.extension',
    collection: 'search.facet.collection',
  };
  function facetLabel(key: string): string {
    const k = FACET_LABELS[key];
    return k ? t(k) : key;
  }

  /** The active filters, resolved back to the human labels the buckets
   *  displayed. A bucket's VALUE is often opaque — an asset_type ref, a
   *  user ref — so a chip printing the raw token would say
   *  `owner:12`. Falls back to the raw value when the bucket is no
   *  longer in the response (a filter narrow enough to remove its own
   *  bucket from another dimension's list). */
  const activeFilters = $derived(
    filters.map((token) => {
      const idx = token.indexOf(':');
      const type = token.slice(0, idx);
      const value = token.slice(idx + 1);
      const bucket = facets[type]?.buckets?.find((b) => b.value === value);
      return {
        token,
        type,
        value,
        label: collectionNames[value] ?? bucket?.label ?? value,
      };
    }),
  );

  /** Collection scope names (#910).
   *
   *  Every other dimension gets its human label from the BUCKET the
   *  caller ticked — the counts endpoint hands back `{value, label}`
   *  together. `collection` is filter-only and has no aggregator by
   *  design (a bucket list would enumerate every collection beside every
   *  search), so nothing hands this page a name and the chip would
   *  otherwise read `IN COLLECTION 3f2b…`, which tells a user nothing
   *  about what is narrowing their results.
   *
   *  Driven off `filters` rather than from the places that SET filters,
   *  which since #1053 are four (URL adoption, a bucket tick, clear-all,
   *  snapshot restore) and would each have to remember. The effect below
   *  reads `filters` and nothing else: the lookup's own read of
   *  `collectionNames` — which is also its write target, and would
   *  therefore be a loop — happens inside [untrack], the same discipline
   *  the URL watcher uses for the same reason.
   *
   *  A collection that 404s (deleted, or no longer visible to this
   *  caller) simply keeps the raw id in the chip. That is deliberate as
   *  well as convenient: the server has already decided what such a
   *  scope returns, and printing a name for a collection the caller
   *  cannot open would be a leak this page has no business inventing. */
  let collectionNames = $state<Record<string, string>>({});
  async function resolveCollectionNames(ids: string[]) {
    for (const cid of ids) {
      if (cid in collectionNames) continue;
      try {
        const resp = await fetch(`/api/v1/collections/${cid}`, { credentials: 'include' });
        if (!resp.ok) continue;
        const body = (await resp.json()) as { name?: string };
        if (body?.name) collectionNames = { ...collectionNames, [cid]: body.name };
      } catch {
        // Same as a 404: the chip keeps the raw id.
      }
    }
  }
  $effect(() => {
    const ids = filters
      .filter((tk) => tk.startsWith('collection:'))
      .map((tk) => tk.slice('collection:'.length))
      .filter(Boolean);
    if (ids.length === 0) return;
    untrack(() => void resolveCollectionNames(ids));
  });

  // Hits mapped to card rows ONCE per result set rather than per render.
  // ContentGrid keys on `id`, and the mapped row carries the hit beside
  // its card props so the snippet reads one object instead of calling
  // three mappers inline. `position` is the 1-based rank the relevance
  // feedback endpoint records — derived here because ContentGrid's card
  // snippet is (item, mode) and deliberately has no index.
  const cards = $derived(
    hits.map((h, i) => ({
      id: h.id,
      hit: h,
      position: i + 1,
      asset: h.type === 'asset' ? hitAsCardAsset(h) : null,
      post: h.type === 'post' ? hitAsPost(h) : null,
      memberCount: h.type === 'post' ? hitMemberCount(h) : 0,
      collection: h.type === 'collection' ? hitAsCollection(h) : null,
    })),
  );
  type SearchCardRow = (typeof cards)[number];

  /** Bumped by every runSearch and by snapshot restoration, so a
   *  result set that has been superseded can't land on top of a newer
   *  one.
   *
   *  The ordering this used to arbitrate — mount fetch versus
   *  `snapshot.restore` — is decided rather than raced now: a
   *  back/forward adoption defers its fetch until the navigation has
   *  finished, which is after SvelteKit has run the restore (see the URL
   *  watcher below). What is left is the genuine race: hitting Back
   *  while a search started BEFORE the navigation is still in flight.
   *  That response must not land on top of what was just restored. */
  let searchGen = 0;

  /** The parameters the result set ON SCREEN was fetched with.
   *
   *  Deliberately not the live `q` / `kinds` / `filters`, which run
   *  AHEAD of the results: a kind chip flips `kinds` the instant it is
   *  clicked, and the page's own search box writes `q` on every
   *  keystroke. The snapshot captured for a history entry has to
   *  describe one coherent result set — the entry's own — and this is
   *  the only record of which one that is (#1060). */
  let resultParams: { q: string; dsl: boolean; kinds: HitType[]; filters: string[] } = {
    q: '',
    dsl: false,
    kinds: [],
    filters: [],
  };

  /** Infinite scroll (#1354), the browse wall's rig rather than a
   *  second one. See `$lib/util/infiniteScroll.svelte` for why the
   *  observer is rooted on the scrollport and why the observer alone is
   *  not the whole trigger. */
  const searchScroll = createInfiniteScroll({
    sentinel: () => sentinel,
    more: () => cursor !== '',
    // BOTH flags. `loading` is a fresh query and `loadingMore` is an
    // append, and a pump that only watched the second would fire a
    // second page against a first page that has not landed.
    busy: () => loading || loadingMore,
    load: () => void runSearch(q, { append: true }),
  });

  async function runSearch(query: string, opts: { append?: boolean } = {}) {
    const gen = ++searchGen;
    // ⭐ REFINING IS A NEW ADDRESS (#1298, ADR 0056 §3c's 2026-08-28
    // amendment): the results region goes back to its first row, and
    // the page chrome does not move. An append is not a refine — it is
    // the reader continuing down the list they are already reading — so
    // the reset is on this arm only.
    //
    // MEASURED before it was written. Six accumulated pages at
    // `main.scrollTop` 4511 of 6088, refined to a 25-hit query: the
    // offset landed on 330, which is exactly `scrollHeight -
    // clientHeight`. The reader was at the BOTTOM of the new list,
    // looking at its last part-filled row, with every hit they had just
    // asked for above the fold.
    //
    // Ordered BEFORE the fetch rather than after the hits land, for two
    // reasons. It is immediate feedback that the refine registered,
    // instead of a jump arriving 100ms later. And at offset 0 Chrome's
    // scroll anchoring has nothing to compensate, so the swap cannot
    // re-resolve the offset against reflowed content — which is the
    // mechanism #1298 measured landing on 0 here, 184 on a second run
    // and 279 on the CI runner, none of them decided by this page.
    if (!opts.append) resetResultsScroll(resultsEl);
    // #1157 — a filter selection is a runnable query with no text. The
    // clear-and-return below is for a genuinely EMPTY address only.
    if (!query && filters.length === 0) {
      hits = [];
      totalCount = 0;
      totalCountCapped = false;
      cursor = '';
      error = '';
      facets = {};
      resultParams = { q: '', dsl: dslMode, kinds: [...kinds], filters: [...filters] };
      return;
    }
    if (opts.append) {
      loadingMore = true;
    } else {
      loading = true;
    }
    error = '';
    /** How many hits this call actually added. The pump's anti-runaway
     *  guard reads it; see the tail of this function. */
    let appended = 0;
    try {
      const params = new URLSearchParams({ limit: '25' });
      if (dslMode) params.set('dsl', query);
      else if (query !== '') params.set('q', query);
      if (kinds.length > 0) params.set('types', kinds.join(','));
      if (opts.append && cursor) params.set('cursor', cursor);
      // #907 — repeated, one per tick. Appended to BOTH requests: the
      // counts have to be computed against the same population as the
      // results, or the rail goes back to describing a page nobody is
      // looking at.
      for (const f of filters) params.append('filter', f);
      const facetParams = new URLSearchParams();
      if (query !== '') facetParams.set('q', query);
      for (const f of filters) facetParams.append('filter', f);
      const [searchResp, facetsResp] = await Promise.all([
        fetch(`/api/v1/search?${params.toString()}`, { credentials: 'include' }),
        opts.append || dslMode
          ? Promise.resolve(null)
          : fetch(`/api/v1/search/facets?${facetParams.toString()}`, { credentials: 'include' }),
      ]);
      if (gen !== searchGen) return;
      if (!searchResp.ok) {
        error = t('search.err_generic', { status: searchResp.status });
        return;
      }
      const data = (await searchResp.json()) as SearchResponse;
      if (gen !== searchGen) return;
      cursor = data.next_cursor || '';
      totalCount = data.total_count;
      totalCountCapped = data.total_count_capped;
      hits = opts.append ? [...hits, ...data.hits] : data.hits;
      appended = data.hits.length;
      // Recorded beside the hits it describes, never before them: an
      // aborted or superseded fetch (both return above) must leave the
      // previous result set and its parameters matching each other.
      resultParams = { q: query, dsl: dslMode, kinds: [...kinds], filters: [...filters] };
      if (facetsResp && facetsResp.ok) {
        const fd = (await facetsResp.json()) as FacetsResponse;
        facets = fd.facets ?? {};
      }
    } catch (e) {
      if (gen === searchGen) error = e instanceof Error ? e.message : String(e);
    } finally {
      if (gen === searchGen) {
        loading = false;
        loadingMore = false;
      }
    }
    // #1354 — top the buffer back up. The IntersectionObserver alone
    // cannot do this once the lookahead is deeper than one page is
    // tall: the sentinel is still inside the margin after an append, so
    // the intersection state never changes and no callback is queued.
    // See createInfiniteScroll.
    //
    // `appended === 0` is the anti-runaway guard and it is load-bearing
    // in the same way the wall's is: a search that answers with an
    // empty page and a non-empty cursor would otherwise leave the
    // buffer permanently short and pump forever. One empty page stops
    // the chase; the observer still re-arms it when the reader scrolls.
    if (gen !== searchGen || appended === 0) return;
    await tick();
    requestAnimationFrame(() => searchScroll.pump());
  }

  function submit(e: Event) {
    e.preventDefault();
    dslMode = false;
    pushQueryToURL(q, false);
  }

  /** Mirror the current query into the URL so a result page is a
   *  shareable, back-navigable address — including the kind filter,
   *  which is part of "what am I looking at".
   *
   *  This is ALL a control does. It writes the address and stops; the
   *  URL watcher below adopts it and runs the one search, the same way
   *  it does for a query typed into the global nav box.
   *
   *  It used to also mark the address as adopted and fire the fetch
   *  itself, which put the new result set on screen BEFORE the
   *  navigation committed — and SvelteKit captures the snapshot for the
   *  entry you are LEAVING part-way through that navigation. The entry
   *  for the old address ended up holding the new address's results, so
   *  going Back restored them (#1060). Nothing here may touch the result
   *  set until the address it belongs to exists.
   *
   *  Consequence worth knowing: submitting a query identical to the one
   *  in the address is now a no-op rather than a refetch. The address
   *  did not change, so neither did what it describes. */
  function pushQueryToURL(query: string, dsl: boolean) {
    const url = new URL(page.url);
    url.searchParams.delete('q');
    url.searchParams.delete('dsl');
    // An empty text term is ABSENT from the address rather than present
    // and blank: `?q=&filter=...` and `?filter=...` must be the same
    // page, and querySignature compares the rendered URL.
    if (query !== '') url.searchParams.set(dsl ? 'dsl' : 'q', query);
    if (kinds.length > 0) url.searchParams.set('types', kinds.join(','));
    else url.searchParams.delete('types');
    // The facet selection is part of "what am I looking at" too, so a
    // filtered result page is a shareable address like an unfiltered one.
    url.searchParams.delete('filter');
    for (const f of filters) url.searchParams.append('filter', f);
    url.searchParams.delete('advanced');
    void goto(url.pathname + url.search, { replaceState: false, noScroll: true });
  }

  // The chips and buckets below still set their own state before
  // navigating, so the control the user just clicked responds
  // immediately rather than a navigation later. That is presentation
  // only — the URL watcher sets the same values back a moment later, and
  // the RESULTS are left alone until it does.

  function toggleKind(kind: HitType) {
    kinds = kinds.includes(kind) ? kinds.filter((k) => k !== kind) : [...kinds, kind];
    if (!hasRunnableQuery) return;
    pushQueryToURL(q, dslMode);
  }

  function clearKinds() {
    if (kinds.length === 0) return;
    kinds = [];
    if (!hasRunnableQuery) return;
    pushQueryToURL(q, dslMode);
  }

  /** Tick or untick one bucket. Re-queries immediately rather than
   *  waiting for an "apply" button: the counts beside every other bucket
   *  are only true for the current selection, so leaving them stale
   *  while a pending tick sat unapplied would put the rail right back to
   *  describing a page nobody is looking at. */
  function toggleFilter(type: string, value: string) {
    const token = filterToken(type, value);
    filters = filters.includes(token)
      ? filters.filter((f) => f !== token)
      : [...filters, token];
    // Always pushed, never gated on `q`. The selection IS the query when
    // there is no text, so a tick with an empty box has to navigate —
    // and an untick that empties the selection has to navigate too, or
    // the URL keeps a filter the page no longer applies.
    pushQueryToURL(q, dslMode);
  }

  function clearFilters() {
    if (filters.length === 0) return;
    filters = [];
    pushQueryToURL(q, dslMode);
  }

  function runAdvanced(dsl: string) {
    advancedOpen = false;
    dslMode = true;
    q = dsl;
    pushQueryToURL(dsl, true);
  }

  onMount(() => {
    // The grids read tile size + mode from the same store browse does,
    // so a user's chosen view follows them into search.
    browseView.init();
    // A panel, not a query: it is opened by the address that asked for
    // it and closed by the user, and it never re-opens on a later URL
    // change (pushQueryToURL strips it).
    if (page.url.searchParams.get('advanced')) advancedOpen = true;
  });

  // ---------------------------------------------------------------------
  // The URL is this page's input (#1053)
  // ---------------------------------------------------------------------
  //
  // Adopting the URL used to happen in onMount, which was enough for as
  // long as a query could only arrive with a fresh page component: a
  // link, a reload, or a navigation FROM somewhere else. Typing into the
  // GLOBAL nav search box is neither — since #1053 it writes `q` onto
  // the result surface you are already on rather than bouncing you to
  // browse, and a same-route navigation does not remount the page. The
  // address would have changed with the results underneath it unmoved.
  //
  // So the adoption is an effect over the URL, and onMount keeps only
  // what is genuinely once-per-mount.

  /** One string for one result set, from its four parameters. Filters
   *  are sorted, so the same selection arriving in a different order is
   *  recognised as the same page rather than as a change worth
   *  re-querying.
   *
   *  Two things produce one of these — an address and a fetched result
   *  set — and comparing the two is how this page decides both what to
   *  query and what a snapshot is worth. */
  function signature(dsl: string, q: string, types: string, filters: string[]): string {
    return JSON.stringify([dsl, q, types, [...filters].sort()]);
  }

  /** The parameters this page's RESULT SET is a function of: the query
   *  itself, the kind chips, and the facet filter tokens (#907).
   *
   *  `post` (the card overlay) and `advanced` (a panel) are deliberately
   *  NOT in it — opening a post over the grid must not re-run the search
   *  underneath it. */
  function querySignature(url: URL): string {
    const p = url.searchParams;
    return signature(
      p.get('dsl') ?? '',
      p.get('q') ?? '',
      p.get('types') ?? '',
      p.getAll('filter'),
    );
  }

  /** The same signature, computed from a fetched result set's own
   *  parameters instead of from an address (#1060).
   *
   *  This is what makes a snapshot checkable. A captured payload carries
   *  the signature of the results INSIDE it, so restoring can ask the
   *  only question that matters — "are these the results for the address
   *  I am going back to?" — instead of inferring it from the order the
   *  restore and the URL watcher happened to run in. Mismatched payloads
   *  exist: a captured entry is only as good as what was on screen at
   *  the moment the navigation away from it committed. */
  function paramSignature(p: typeof resultParams): string {
    return signature(p.dsl ? p.q : '', p.dsl ? '' : p.q, p.kinds.join(','), p.filters);
  }

  /** The address this page has already applied. Plain `let`, not
   *  `$state`: it is bookkeeping FOR the effect below, and a reactive
   *  cell written from inside its own reader is a loop. */
  let appliedSignature: string | null = null;

  /** Mirror `url` into local state and run the search it describes.
   *
   *  `q` wins over `dsl` when the address carries both. This page never
   *  emits that combination (pushQueryToURL deletes the other one), so
   *  it can only mean the global search box typed a plain query over a
   *  DSL result — the newer input, and the one the user is watching
   *  themselves type. The rule is the same on a reload, so the address
   *  still reproduces one result set.
   *
   *  Mirrors ONLY. Whether the address it just mirrored also needs a
   *  fetch is the watcher's call below, because that answer depends on
   *  how the address arrived. */
  function adoptURL(url: URL) {
    const urlDSL = url.searchParams.get('dsl');
    const urlQ = url.searchParams.get('q');
    const urlTypes = url.searchParams.get('types');
    filters = url.searchParams.getAll('filter');
    kinds = (urlTypes ?? '')
      .split(',')
      .map((s) => s.trim())
      .filter((s): s is HitType => s === 'asset' || s === 'post' || s === 'collection');
    if (urlDSL !== null && urlQ === null) {
      dslMode = true;
      q = urlDSL;
    } else {
      dslMode = false;
      q = urlQ ?? '';
    }
  }

  /** Set when an adoption has mirrored an address but is holding its
   *  fetch back, waiting to see whether a snapshot restore hands it the
   *  results instead. Plain `let`, like `appliedSignature` and for the
   *  same reason. */
  let pendingQuery = false;

  $effect(() => {
    const sig = querySignature(page.url);
    if (sig === appliedSignature) return;
    appliedSignature = sig;
    // untrack: adoptURL writes — and runSearch reads — half the state on
    // this page, and effect dependency collection is call-frame deep.
    // The URL is the only thing this effect watches, `navigating`
    // included: it is read for what KIND of navigation this is, not to
    // be woken by it.
    untrack(() => {
      // Back or forward, and only then, SvelteKit may still restore a
      // snapshot for this entry — it does that at the END of the
      // navigation, after page state has updated and after this effect
      // has run. Results are paged behind a manual "load more", so a
      // fetch fired here would be a fetch whose only achievement is
      // throwing away pages the restore is about to put back (#584).
      //
      // So hold it, and let one of two things release it: a restore that
      // matches this address (which cancels it), or the navigation
      // finishing without one (which runs it). Both are observed. The
      // previous rule — "the first adoption yields, a later one queries"
      // — could not tell a genuine URL change from a back-navigation,
      // because after #1053 both are later adoptions (#1060).
      const restorable = navigating.type === 'popstate';
      adoptURL(page.url);
      if (restorable) pendingQuery = true;
      else void runSearch(q);
    });
  });

  // The release. `navigating` clears once the navigation is complete,
  // which SvelteKit does immediately after running snapshot restores —
  // so by the time this runs, a restore either happened or never will.
  $effect(() => {
    if (navigating.type !== null) return;
    untrack(() => {
      if (!pendingQuery) return;
      pendingQuery = false;
      void runSearch(q);
    });
  });

  // Back-navigation restoration (#584). Results are paged behind a
  // manual "load more", so the offset is only meaningful alongside the
  // hits it was measured against — restoring one without the other
  // would land the user in the middle of a shorter list.
  interface SearchSnapshot {
    /** Signature of the query these hits answer — the check that decides
     *  whether this payload belongs to the address being restored. */
    sig: string;
    q: string;
    dsl: boolean;
    kinds: HitType[];
    filters: string[];
    hits: SearchHit[];
    cursor: string;
    totalCount: number;
    totalCountCapped: boolean;
    facets: Record<string, FacetResult>;
  }
  export const snapshot = createScrollSnapshot<SearchSnapshot>({
    // Captured from `resultParams`, not from the live controls: this is
    // one result set and the parameters it answers, never a mixture of
    // an old result set and the controls that are about to replace it.
    capture: () => ({
      sig: paramSignature(resultParams),
      q: resultParams.q,
      dsl: resultParams.dsl,
      kinds: [...resultParams.kinds],
      filters: [...resultParams.filters],
      hits,
      cursor,
      totalCount,
      totalCountCapped,
      facets,
    }),
    restore: (saved) => {
      if (!saved || saved.hits.length === 0) return false;
      // The one question: are these the results for the address being
      // restored? A payload is captured while the navigation AWAY from
      // its entry is in flight, so a page whose results moved on before
      // that moment leaves one behind that describes the wrong query. It
      // is refused rather than shown, and the adoption's held-back fetch
      // is left to run — which is why refusing also declines the scroll
      // offset: the offset was measured against hits that are not coming
      // back, and the list about to arrive is a first page (#1060).
      if (saved.sig !== querySignature(page.url)) return false;
      // A search that was already in flight when Back was pressed must
      // not land on top of this.
      searchGen++;
      // Both halves of the adoption are now satisfied: the page state
      // matches this address, so the watcher must not re-run it, and the
      // fetch it was holding back is no longer needed.
      appliedSignature = querySignature(page.url);
      pendingQuery = false;
      q = saved.q;
      dslMode = saved.dsl;
      kinds = saved.kinds ?? [];
      filters = saved.filters ?? [];
      hits = saved.hits;
      cursor = saved.cursor;
      totalCount = saved.totalCount;
      totalCountCapped = saved.totalCountCapped;
      facets = saved.facets;
      resultParams = {
        q: saved.q,
        dsl: saved.dsl,
        kinds: [...(saved.kinds ?? [])],
        filters: [...(saved.filters ?? [])],
      };
      loading = false;
      loadingMore = false;
      return true;
    },
  });

  async function submitSave() {
    if (!saveName.trim()) return;
    saving = true;
    saveResult = '';
    try {
      const resp = await fetch('/api/v1/search/save-as-collection', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        // The filters travel with the query (#907): this button is ON
        // the filtered page, so saving the unfiltered set would persist
        // a collection nobody asked for.
        body: JSON.stringify({ name: saveName.trim(), q, types: ['asset'], filters }),
      });
      if (!resp.ok) {
        const err = await resp.text();
        saveResult = t('search.save_collection.save_failed', { status: resp.status, err });
        return;
      }
      const data = await resp.json();
      saveResult = data.truncated
        ? t('search.save_collection.saved_truncated', { n: data.saved_count })
        : t('search.save_collection.saved', { n: data.saved_count });
      setTimeout(() => {
        void goto(`/collections/${data.collection_id}`);
      }, 800);
    } catch (e) {
      saveResult = e instanceof Error ? e.message : String(e);
    } finally {
      saving = false;
    }
  }

  async function submitSaveSearch() {
    if (!saveSearchName.trim()) return;
    savingSearch = true;
    saveSearchResult = '';
    try {
      // ⛔ THE FILTERS TRAVEL WITH THE QUERY (#1368). Until this line
      // existed the body carried the query expression alone, so a search
      // narrowed on the rail was saved UNNARROWED: it replayed wider than
      // the page it was saved from, and the digest the owner is emailed
      // from it contained hits their own search excludes.
      //
      // Posted as `dimension:value` tokens — the same wire form the
      // save-as-collection button beside this one has posted since #907,
      // and the same one every GET surface takes. The server composes the
      // ONE canonical DSL string that gets stored (search.ComposeDSL): it
      // wraps this expression in parentheses so its own precedence
      // survives, and conjuncts the selection onto it. Composing the
      // string HERE would mean a second copy of the DSL's quoting rules
      // in TypeScript, which is the duplicate grammar ADR 0093 decision 3
      // refuses — and which cannot be derived from the Go lexer that
      // decides what a bare token may contain.
      //
      // Read off `resultParams` rather than the live controls, because
      // that is the pair (query, filters) the result set ON SCREEN was
      // fetched with — the same reason the history snapshot reads it.
      const resp = await fetch('/api/v1/search/saved', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({
          name: saveSearchName.trim(),
          dsl: resultParams.q,
          filters: [...resultParams.filters],
          notify_channel: saveSearchChannel,
          notify_interval_minutes: saveSearchInterval,
        }),
      });
      if (!resp.ok) {
        const err = await resp.text();
        saveSearchResult = t('search.save_search.save_failed', { status: resp.status, err });
        return;
      }
      const data = await resp.json();
      saveSearchResult = t('search.save_search.saved', { minutes: data.notify_interval_minutes });
      setTimeout(() => {
        void goto('/account/saved-searches');
      }, 1000);
    } catch (e) {
      saveSearchResult = e instanceof Error ? e.message : String(e);
    } finally {
      savingSearch = false;
    }
  }
</script>

<svelte:head><title>{t('search.page_title')} — {site.name}</title></svelte:head>

<!-- Full viewport width + the same px-4/sm:px-6 padding as the browse
     feed and the profile grids, so the shared TileGrid resolves the SAME
     column count and search reads as one grid system with them. -->
<div bind:this={resultsEl} class="w-full px-4 py-6 sm:px-6">
  <form onsubmit={submit} class="mb-4 flex flex-wrap gap-2">
    <input
      bind:value={q}
      type="search"
      placeholder={t('search.query_placeholder')}
      data-testid="search-input"
      class="min-h-11 min-w-0 flex-1 rounded-md border border-border-strong bg-surface px-3 py-2 text-sm text-fg
             focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    />
    <button
      type="submit"
      class="min-h-11 rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent hover:bg-accent/90"
    >{t('common.search')}</button>
  </form>

  <!-- The chip row: what you are looking at, and the two panels. It
       WRAPS — at 390px it reflows onto two lines instead of scrolling the
       page sideways, which is what the fixed rail it replaces could not
       do (#901). Every control is min-h-11 (44px), the coarse-pointer tap
       target, verified at 390px with a touch pointer. -->
  <div class="mb-4 flex flex-wrap items-center gap-2" data-testid="search-chips">
    <button
      type="button"
      onclick={clearKinds}
      aria-pressed={kinds.length === 0}
      class="inline-flex min-h-11 items-center rounded-full border px-3 py-1.5 text-sm transition-colors
             {kinds.length === 0
               ? 'border-accent bg-accent text-on-accent'
               : 'border-border bg-surface text-fg-muted hover:border-border-strong hover:text-fg'}"
      data-testid="kind-chip-all"
    >{t('search.kind.all')}</button>

    {#each kindOptions as k (k.id)}
      {@const active = kinds.includes(k.id)}
      <button
        type="button"
        onclick={() => toggleKind(k.id)}
        aria-pressed={active}
        class="inline-flex min-h-11 items-center gap-1.5 rounded-full border px-3 py-1.5 text-sm transition-colors
               {active
                 ? 'border-accent bg-accent text-on-accent'
                 : 'border-border bg-surface text-fg-muted hover:border-border-strong hover:text-fg'}"
        data-testid="kind-chip-{k.id}"
      >
        {t(k.labelKey)}
        {#if active}
          <!-- The removable half of the pill: an active chip states what
               it is filtering AND how to stop. -->
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true">
            <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
          </svg>
        {/if}
      </button>
    {/each}

    <span class="mx-1 hidden h-6 w-px bg-border sm:inline-block" aria-hidden="true"></span>

    <!-- Active facet filters, as removable chips (#907). They live OUT
         here rather than only in the panel because the panel is a
         slide-over: a filter you cannot see from the results is a filter
         you forget you set, and then the empty state reads as a broken
         search. Same pill shape as the kind chips, so "what is narrowing
         this page" is one row. -->
    {#each activeFilters as f (f.token)}
      <button
        type="button"
        onclick={() => toggleFilter(f.type, f.value)}
        class="inline-flex min-h-11 items-center gap-1.5 rounded-full border border-accent bg-accent
               px-3 py-1.5 text-sm text-on-accent"
        data-testid="filter-chip-{f.token}"
      >
        <span class="text-xs uppercase tracking-wide opacity-80">{facetLabel(f.type)}</span>
        <span class="max-w-[10rem] truncate">{f.label}</span>
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true">
          <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
        </svg>
      </button>
    {/each}
    {#if filters.length > 1}
      <button
        type="button"
        onclick={clearFilters}
        class="inline-flex min-h-11 items-center rounded-full border border-border bg-surface px-3 py-1.5
               text-sm text-fg-muted hover:border-border-strong hover:text-fg"
        data-testid="clear-filters"
      >{t('search.filters_clear')}</button>
    {/if}

    <button
      type="button"
      onclick={() => (facetsOpen = true)}
      disabled={facetTotal === 0 && filters.length === 0}
      class="inline-flex min-h-11 items-center gap-1.5 rounded-full border border-border bg-surface px-3 py-1.5
             text-sm text-fg-muted hover:border-border-strong hover:text-fg disabled:opacity-40"
      data-testid="open-facets"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <line x1="4" y1="6" x2="20" y2="6" /><line x1="7" y1="12" x2="17" y2="12" /><line x1="10" y1="18" x2="14" y2="18" />
      </svg>
      {t('search.facets_heading')}
    </button>

    <button
      type="button"
      onclick={() => (advancedOpen = true)}
      class="inline-flex min-h-11 items-center rounded-full border border-border bg-surface px-3 py-1.5 text-sm
             text-fg-muted hover:border-border-strong hover:text-fg"
      data-testid="open-advanced"
    >{t('search.advanced_builder')}</button>
  </div>

  {#if error}
    <div role="alert" class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">
      {error}
    </div>
  {/if}

  {#if !loading && hits.length > 0}
    <!-- The count, and beside it the two things you can DO with a result
         set. They are deliberately not chips: the row above filters what
         you are looking at, these two act on it, and mixing the two
         kinds in one wrapping row cost three lines of a 390px screen
         before any artwork appeared. -->
    <div class="mb-3 flex flex-wrap items-center gap-x-3 gap-y-2">
      <p class="text-sm text-fg-muted" data-testid="search-total-count">
        {t('search.counter', { n: hits.length, total: activeCount })}
      </p>
      <div class="ml-auto flex flex-wrap items-center gap-2">
        <button
          type="button"
          onclick={() => { saveSearchOpen = true; saveSearchName = q; }}
          class="inline-flex min-h-11 items-center rounded-md border border-border bg-surface px-3 py-1.5 text-sm
                 text-fg-muted hover:border-border-strong hover:text-fg"
          data-testid="save-search"
        >{t('search.save_search_button')}</button>
        <button
          type="button"
          onclick={() => { saveOpen = true; saveName = q; }}
          class="inline-flex min-h-11 items-center rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
          data-testid="save-as-collection"
        >{t('search.save_as_collection')}</button>
      </div>
    </div>
  {/if}

  {#if !loading && hits.length === 0 && hasRunnableQuery}
    <p class="text-sm text-fg-muted">{t('search.no_matches')}</p>
  {:else if !loading && !hasRunnableQuery}
    <!-- Landing on /search with no query. Not an "advanced search"
         headline page — an invitation to type, and nothing else on
         screen competing with the input above. -->
    <p class="text-sm text-fg-muted" data-testid="search-idle">{t('search.idle')}</p>
  {/if}

  <!-- ONE result surface: the same grid, the same cards, the same view
       modes as browse. The score is deliberately absent — an artist
       looking at their own work does not need `score 1.000` on every
       tile, and the ordering it describes is visible in the layout. -->
  <ContentGrid mode={browseView.mode} items={cards} tileMin={browseView.tileMin} {loading}>
    {#snippet card(item, mode)}
      {@const row = item as SearchCardRow}
      {#if row.post}
        <PostCard
          post={row.post}
          memberCount={row.memberCount}
          {mode}
          feed={mode === 'feed'}
          tileSizes={browseView.tileSizes}
        />
      {:else if row.collection}
        <CollectionCard collection={row.collection} />
      {:else if row.asset}
        <!-- Relevance feedback (Phase 1.16.B-5) as a hover OVERLAY, not a
             strip under the tile.

             A strip was the first attempt and it broke the thing this
             issue is about: grid is a zero-gap contact sheet (#555/#560),
             so a row of buttons below asset tiles — and not below post or
             collection tiles — made the wall ragged, and search stopped
             looking like browse the moment a mixed page loaded. The
             overlay costs the grid nothing.

             Bottom-LEFT, because every other corner is claimed: checkbox
             + media-type badge top-left of the THUMB, ⋮ menu top-right,
             multi-asset badge bottom-right. Revealed on hover AND on
             focus-within, so it is keyboard-reachable rather than
             pointer-only. -->
        <div class="group/hit relative">
          <AssetCard asset={row.asset} {mode} tileSizes={browseView.tileSizes} />
          {#if !row.hit.restricted}
            <!-- Offering feedback on a placeholder invites a request that
                 404s, so a withheld hit does not get the control. -->
            <div
              class="pointer-events-none absolute bottom-1 left-1 z-[3] rounded-md bg-black/60 px-1 opacity-0
                     backdrop-blur-sm transition-opacity duration-200
                     group-hover/hit:pointer-events-auto group-hover/hit:opacity-100
                     focus-within:pointer-events-auto focus-within:opacity-100"
            >
              <ThumbButtons dsl={q} hitAssetId={row.hit.id} hitPosition={row.position} />
            </div>
          {/if}
        </div>
      {/if}
    {/snippet}
  </ContentGrid>

  <!-- ⭐ THE PAGING SENTINEL (#1354), and it REPLACES the "Load more"
       button rather than sitting beside it.

       There used to be a button here and the browse wall beside it had
       none, on the same grid, with the same cards. The comment at the
       URL watcher does say results are "paged behind a manual load
       more", but it says that to explain a snapshot-restore
       interaction — it is a description of the state, not a decision
       that search should page differently from browse.

       ⚠️ NO MANUAL AFFORDANCE SURVIVES, and that is matching the wall
       rather than dropping a feature. #1354's rule is "keep one if the
       browse wall keeps one"; it keeps none (+page.svelte renders the
       same bare sentinel under the same `hasMore` guard), so inventing
       a different answer here is the thing to avoid. Both surfaces now
       end a finished list the same way, with nothing to click.

       Gated on `hasMore` so the observer has nothing to watch once the
       list is complete — the same guard, for the same reason, as the
       wall's. -->
  {#if hasMore}
    <div bind:this={sentinel} class="h-px w-full" aria-hidden="true"></div>
  {/if}

  {#if loadingMore}
    <p class="py-4 text-center text-xs text-fg-muted" data-testid="search-loading-more">
      {t('common.loading')}
    </p>
  {/if}
</div>

<!-- The same floating mode switcher + sort bar every other browse
     surface mounts (#511), so switching to masonry on the home feed and
     then searching lands you in masonry. -->
<ViewControls />

<PostParamHost />

<!-- Facet counts. A panel, not a rail: the rail was a `w-64 shrink-0`
     column that could not fit beside a grid at 390px (#901). -->
<SearchSlideOver open={facetsOpen} title={t('search.facets_heading')} onclose={() => (facetsOpen = false)}>
  <p class="mb-4 text-sm text-fg-muted">{t('search.facets_intro')}</p>
  <div class="space-y-4">
    {#each Object.entries(facets) as [type, res] (type)}
      {#if res.buckets && res.buckets.length > 0}
        <div class="rounded-md border border-border bg-surface-elevated p-3">
          <div class="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">{facetLabel(type)}</div>
          <ul class="space-y-1 text-sm">
            {#each res.buckets.slice(0, 10) as b (b.value)}
              {@const token = filterToken(type, b.value)}
              <!-- A LABEL wrapping a real checkbox, not a styled div with
                   a click handler: the whole row is the hit target (44px
                   at a coarse pointer), the control is keyboard-
                   reachable and announces its checked state, and the
                   count stays legible beside it. -->
              <li>
                <label
                  class="flex min-h-11 cursor-pointer items-center gap-2 rounded px-1 hover:bg-state-hover"
                  data-testid="facet-option-{token}"
                >
                  <input
                    type="checkbox"
                    class="h-4 w-4 shrink-0 accent-accent"
                    checked={filters.includes(token)}
                    onchange={() => toggleFilter(type, b.value)}
                  />
                  <span class="min-w-0 flex-1 truncate">{b.label ?? b.value}</span>
                  <span class="shrink-0 tabular-nums text-xs text-fg-muted">{b.count}</span>
                </label>
              </li>
            {/each}
          </ul>
        </div>
      {/if}
    {/each}
    {#if facetTotal === 0}
      <!-- Reachable now that the panel opens with filters active: a
           narrow enough selection empties every bucket, and a blank
           panel with no way back would be a dead end. -->
      <p class="text-sm text-fg-muted">{t('search.facets_empty')}</p>
    {/if}
  </div>
  {#snippet footer()}
    <button
      type="button"
      onclick={clearFilters}
      disabled={filters.length === 0}
      class="min-h-11 w-full rounded-md border border-border bg-surface px-3 py-1.5 text-sm
             hover:border-border-strong disabled:opacity-40"
      data-testid="facets-clear-all"
    >{t('search.filters_clear')}</button>
  {/snippet}
</SearchSlideOver>

<!-- The advanced builder — a panel composing the same query, hidden
     until asked for. It used to be /search/advanced, a separate
     destination with its own empty state; that made "advanced" a mode
     rather than an affordance. -->
<SearchSlideOver open={advancedOpen} title={t('search.advanced.heading')} onclose={() => (advancedOpen = false)}>
  <AdvancedQueryBuilder onsubmit={runAdvanced} />
</SearchSlideOver>

<!-- Save-as-collection modal -->
{#if saveOpen}
  <div
    role="dialog"
    aria-modal="true"
    aria-label={t('search.save_collection.dialog_label')}
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
    onclick={(e) => { if (e.target === e.currentTarget) saveOpen = false; }}
  >
    <div class="w-full max-w-md rounded-lg border border-border bg-surface p-5 shadow-xl">
      <h3 class="mb-3 text-lg font-semibold">{t('search.save_collection.heading')}</h3>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">{t('search.save_collection.name_label')}</span>
        <input
          bind:value={saveName}
          type="text"
          class="min-h-11 w-full rounded-md border border-border-strong bg-surface px-3 py-1.5 text-sm"
        />
      </label>
      {#if saveResult}
        <p class="mb-3 text-sm text-fg-muted" data-testid="save-result">{saveResult}</p>
      {/if}
      <div class="flex justify-end gap-2">
        <button
          type="button"
          onclick={() => (saveOpen = false)}
          class="min-h-11 rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
        >{t('common.cancel')}</button>
        <button
          type="button"
          onclick={submitSave}
          disabled={saving || !saveName.trim()}
          class="min-h-11 rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:opacity-50"
        >{saving ? t('common.saving') : t('search.save_collection.submit')}</button>
      </div>
    </div>
  </div>
{/if}

<!-- Save-search modal (Phase 1.16.B-4). Persists the query as a
     notification target rather than a snapshot; the coordinator
     runs it on the interval + emails when new hits appear. -->
{#if saveSearchOpen}
  <div
    role="dialog"
    aria-modal="true"
    aria-label={t('search.save_search.dialog_label')}
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
    onclick={(e) => { if (e.target === e.currentTarget) saveSearchOpen = false; }}
  >
    <div class="w-full max-w-md rounded-lg border border-border bg-surface p-5 shadow-xl">
      <h3 class="mb-3 text-lg font-semibold">{t('search.save_search.heading')}</h3>
      <p class="mb-3 text-sm text-fg-muted">
        {t('search.save_search.body')}
      </p>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">{t('search.save_search.name_label')}</span>
        <input
          bind:value={saveSearchName}
          type="text"
          class="min-h-11 w-full rounded-md border border-border-strong bg-surface px-3 py-1.5 text-sm"
        />
      </label>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">{t('search.save_search.interval_label')}</span>
        <input
          bind:value={saveSearchInterval}
          type="number"
          min="15"
          step="15"
          class="min-h-11 w-full rounded-md border border-border-strong bg-surface px-3 py-1.5 text-sm"
        />
      </label>
      <label class="mb-3 block text-sm">
        <span class="mb-1 block text-fg-muted">{t('search.save_search.channel_label')}</span>
        <select
          bind:value={saveSearchChannel}
          class="min-h-11 w-full rounded-md border border-border-strong bg-surface px-3 py-1.5 text-sm"
        >
          <option value="email">{t('search.save_search.channel_email')}</option>
          <option value="none">{t('search.save_search.channel_none')}</option>
        </select>
      </label>
      {#if saveSearchResult}
        <p class="mb-3 text-sm text-fg-muted" data-testid="save-search-result">{saveSearchResult}</p>
      {/if}
      <div class="flex justify-end gap-2">
        <button
          type="button"
          onclick={() => (saveSearchOpen = false)}
          class="min-h-11 rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
        >{t('common.cancel')}</button>
        <button
          type="button"
          onclick={submitSaveSearch}
          disabled={savingSearch || !saveSearchName.trim()}
          class="min-h-11 rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:opacity-50"
        >{savingSearch ? t('common.saving') : t('search.save_search.submit')}</button>
      </div>
    </div>
  </div>
{/if}
