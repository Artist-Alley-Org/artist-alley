<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Collections hub. The page is the user's "all places to put
  // posts" entry point. Five tabs (mine / featured / public /
  // shared / all) live on top; the body is a CollectionCard grid;
  // a header search filters by name; a "New" button opens the
  // create-collection modal.
  //
  // Tabs map to the `tab` query param on GET /collections so the
  // backend does the filtering — keeps the hub responsive even
  // when the install grows past a few hundred collections.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import CollectionCard from '$components/CollectionCard.svelte';
  import NewCollectionModal from '$components/NewCollectionModal.svelte';

  type Tab = 'mine' | 'featured' | 'public' | 'shared' | 'all';

  interface CollectionRow {
    id: string;
    name: string;
    description: string;
    visibility: string;
    featured: boolean;
    owner_user_ref: number;
    created_at: string;
  }

  const TABS: { id: Tab; key: string }[] = [
    { id: 'mine', key: 'collections.tab_mine' },
    { id: 'featured', key: 'collections.tab_featured' },
    { id: 'public', key: 'collections.tab_public' },
    { id: 'shared', key: 'collections.tab_shared' },
    { id: 'all', key: 'collections.tab_all' },
  ];

  let tab = $state<Tab>('mine');
  let q = $state('');
  let collections = $state<CollectionRow[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);
  let newOpen = $state(false);
  // Phase 1.55.C-1b: admin toggle to include soft-deleted collections
  // in the list. Non-admin sessions never see the toggle; the backend
  // ignores the query param even if it were sent.
  let includeDeleted = $state(false);

  let searchDebounce: ReturnType<typeof setTimeout> | null = null;

  onMount(() => {
    const initial = (page.url.searchParams.get('tab') as Tab) ?? 'mine';
    tab = TABS.some((t) => t.id === initial) ? initial : 'mine';
    q = page.url.searchParams.get('q') ?? '';
    void load();
  });

  function syncUrl() {
    const url = new URL(page.url);
    if (tab === 'mine') url.searchParams.delete('tab');
    else url.searchParams.set('tab', tab);
    if (q.trim()) url.searchParams.set('q', q.trim());
    else url.searchParams.delete('q');
    void goto(url.pathname + url.search, { replaceState: true, keepFocus: true, noScroll: true });
  }

  async function load() {
    loading = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.GET('/collections', {
        params: {
          query: {
            tab,
            q: q.trim() || undefined,
            limit: 200,
            include_deleted: includeDeleted || undefined,
          },
        },
      });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('collections.error_load');
        return;
      }
      collections = (data?.items ?? []) as CollectionRow[];
    } finally {
      loading = false;
    }
  }

  function setTab(next: Tab) {
    if (next === tab) return;
    tab = next;
    syncUrl();
    void load();
  }

  function onSearchInput() {
    if (searchDebounce) clearTimeout(searchDebounce);
    searchDebounce = setTimeout(() => {
      syncUrl();
      void load();
    }, 250);
  }

  function handleCreated(c: CollectionRow) {
    collections = [c, ...collections];
  }

  const visibleTabs = $derived.by(() => {
    if (auth.user) return TABS;
    return TABS.filter((t) => t.id !== 'mine' && t.id !== 'shared');
  });
</script>

<svelte:head>
  <title>{t('collections.title')} — {site.name}</title>
</svelte:head>

<div class="w-full px-4 py-6 sm:px-6">
  <!-- Header: title + new button + search -->
  <header class="mb-5">
    <div class="flex items-center justify-between gap-4">
      <div>
        <h1 class="text-2xl font-semibold">{t('collections.title')}</h1>
        <p class="mt-1 text-sm text-fg-muted">{t('collections.tagline')}</p>
      </div>
      {#if auth.user}
        <button
          type="button"
          onclick={() => (newOpen = true)}
          class="inline-flex items-center gap-1.5 rounded-md bg-accent px-3 py-2 text-sm font-medium text-on-accent shadow-sm hover:bg-accent/90"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <line x1="12" y1="5" x2="12" y2="19" />
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
          {t('collections.new')}
        </button>
      {/if}
    </div>

    <!-- Tabs + search bar -->
    <div class="mt-4 flex flex-wrap items-center justify-between gap-3">
      <div role="tablist" class="-mb-px flex flex-wrap gap-1">
        {#each visibleTabs as tDef (tDef.id)}
          <button
            type="button"
            role="tab"
            aria-selected={tab === tDef.id}
            onclick={() => setTab(tDef.id)}
            class="rounded-t-md border-b-2 px-3 py-1.5 text-sm font-medium transition-colors"
            class:border-accent={tab === tDef.id}
            class:text-accent={tab === tDef.id}
            class:border-transparent={tab !== tDef.id}
            class:text-fg-muted={tab !== tDef.id}
            class:hover:text-fg={tab !== tDef.id}
          >
            {t(tDef.key)}
          </button>
        {/each}
      </div>

      <div class="flex items-center gap-3">
        {#if auth.can('system.admin')}
          <label class="inline-flex cursor-pointer items-center gap-1.5 text-xs text-fg-muted">
            <input
              type="checkbox"
              bind:checked={includeDeleted}
              onchange={() => void load()}
              class="h-3.5 w-3.5 rounded border-border"
            />
            {t('collections.include_deleted')}
          </label>
        {/if}
        <div class="relative w-full max-w-xs">
          <input
            type="search"
            bind:value={q}
            oninput={onSearchInput}
            placeholder={t('collections.search_placeholder')}
            class="w-full rounded-md border border-border bg-surface py-1.5 pl-9 pr-3 text-sm focus-visible:border-border-strong focus:outline-none"
          />
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-muted">
            <circle cx="11" cy="11" r="8" />
            <path d="m21 21-4.3-4.3" />
          </svg>
        </div>
      </div>
    </div>
  </header>

  <!-- Body -->
  {#if loading}
    <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 2xl:grid-cols-7">
      {#each { length: 8 } as _, i (i)}
        <div class="overflow-hidden rounded-xl border border-border bg-surface-elevated">
          <div class="aspect-[4/3] animate-pulse bg-surface"></div>
          <div class="p-3">
            <div class="h-3 w-2/3 animate-pulse rounded bg-surface"></div>
            <div class="mt-2 h-3 w-1/2 animate-pulse rounded bg-surface"></div>
          </div>
        </div>
      {/each}
    </div>
  {:else if error}
    <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
      {error}
    </p>
  {:else if collections.length === 0}
    <div class="rounded-lg border border-dashed border-border bg-surface-elevated/50 px-6 py-12 text-center">
      <p class="text-sm text-fg-muted">{t(`collections.empty_${tab}`)}</p>
      {#if auth.user && (tab === 'mine' || tab === 'all')}
        <button
          type="button"
          onclick={() => (newOpen = true)}
          class="mt-3 rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
        >
          {t('collections.new_first')}
        </button>
      {/if}
    </div>
  {:else}
    <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 2xl:grid-cols-7">
      {#each collections as c (c.id)}
        <CollectionCard collection={c} />
      {/each}
    </div>
  {/if}
</div>

<NewCollectionModal
  open={newOpen}
  onclose={() => (newOpen = false)}
  oncreate={handleCreated}
/>
