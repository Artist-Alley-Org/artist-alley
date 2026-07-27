<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Collection detail page.
  //
  // Top section is the header bar — breadcrumbs + name + visibility
  // badge + owner. Below it sits an action toolbar (Upload here,
  // Share, Edit, More menu). The body is the member grid: for now
  // the asset-level membership table (`collection_resources`) since
  // post-level membership lands in a follow-up commit.
  //
  // The member grid renders through the shared ContentGrid + the
  // floating ViewControls bar (#582), the same chrome browse and the
  // profile pages use — a collection is an asset-showing surface, so it
  // gets the mode switcher, tile size and sort like every other one.
  // Previously this route hand-rolled its own responsive grid and had no
  // controls at all.
  //
  // Modals for Edit and Share live inline so closing them doesn't
  // unmount the page.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { upload } from '$stores/upload.svelte';
  import { t } from '$stores/lang.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import { invalidate as invalidateCovers } from '$stores/collectionCovers.svelte';
  import AssetCard from '$components/AssetCard.svelte';
  import type { CardAsset } from '$components/cardAsset';
  import ContentGrid from '$components/ContentGrid.svelte';
  import ViewControls from '$components/ViewControls.svelte';
  import Menu from '$components/Menu.svelte';
  import EditCollectionModal from '$components/EditCollectionModal.svelte';
  import ShareCollectionModal from '$components/ShareCollectionModal.svelte';

  interface Collection {
    id: string;
    name: string;
    description: string;
    visibility: string;
    owner_user_ref: number;
    created_at: string;
    updated_at: string;
    deleted_at?: string | null;
    deleted_reason?: string | null;
  }

  interface MemberRow {
    asset_id: string;
    title: string;
    asset_type: number;
    file_hash: string | null;
    // Media type + blur-up (#595). A member tile renders through the
    // same CardThumb as a browse tile, which reads the media TYPE off
    // the extension alone — that is what puts the video / 3D badge on
    // the tile and what makes the hover sprite-scrub preview play.
    // These were missing from both this row type and the API response,
    // so every video and 3D member rendered as an untyped still.
    file_extension: string | null;
    thumbhash: string | null;
    sort_order: number;
    added_at: string;
    asset_created_at?: string | null;
    preview_available?: boolean;
    /** Every configured rung exists (#610). Feeds the card's responsive
     *  srcset (#502) — the API row carries it, so pass it through rather
     *  than letting the tile fall back to the square `col` crop. */
    ladder_available?: boolean;
  }

  let collection = $state<Collection | null>(null);
  let members = $state<MemberRow[]>([]);
  let loading = $state(true);
  let membersLoading = $state(true);
  let error = $state<string | null>(null);
  // Separate from `error` on purpose: one is "we could not load this",
  // the other is "this is not yours to see", and they should not look
  // the same to a visitor.
  let notFound = $state(false);
  let editOpen = $state(false);
  let shareOpen = $state(false);
  let copyFeedback = $state(false);

  const id = $derived(page.params.id ?? '');
  const isOwner = $derived(!!collection && !!auth.user && collection.owner_user_ref === auth.user.ref);

  // Mode + sort come from the GLOBAL browseView store (localStorage), so
  // a collection shares the view preference with browse and the profile
  // pages. SEAM: per-surface view state is probably reworked in the
  // future; the coupling to the one store is the simplest-correct option
  // for now — same call UserProfile documents.
  //
  // The BASE order is the curator's `sort_order` (ADR 0009 — a collection
  // is "an ordered, optionally-shared" set, and the server already
  // returns `ORDER BY cr.sort_order ASC, cr.added_at ASC`). The sort
  // toggle REVERSES that order; it deliberately does not re-sort by date,
  // which would throw away the curation.
  const sortedMembers = $derived(
    browseView.feedDir === 'asc' ? [...members].reverse() : members,
  );

  // ContentGrid keys rows by `id`; a member row is keyed by asset_id, so
  // map once here rather than teaching the grid about two shapes.
  //
  // This literal is annotated `CardAsset[]` on purpose (#595). It is the
  // one place on this page that RE-SHAPES an API row into card props,
  // and it is exactly where the media-type + blur-up fields were lost:
  // an un-annotated object literal is assignable to a card prop that
  // only asks for optional fields, so dropping two of them raised
  // nothing. With the annotation, forgetting a presentation field is a
  // type error here rather than a missing badge in the browser.
  const memberItems = $derived<CardAsset[]>(
    sortedMembers.map((m) => ({
      id: m.asset_id,
      title: m.title,
      file_hash: m.file_hash,
      file_extension: m.file_extension,
      thumbhash: m.thumbhash,
      asset_type: m.asset_type,
      created_at: m.asset_created_at ?? m.added_at,
      preview_available: !!m.preview_available,
      ladder_available: !!m.ladder_available,
    })),
  );

  onMount(() => {
    browseView.init(); // pick up the user's tile-size + mode preference
    void load();
  });

  async function load() {
    loading = true;
    error = null;
    notFound = false;
    try {
      const { data, error: apiErr, response } = await api.GET('/collections/{id}', {
        params: { path: { id } },
      });
      if (apiErr || !data) {
        // 404 is not a failure (#416). The visibility predicate returns
        // 404 rather than 403 for a collection the caller may not see,
        // so "private" and "deleted" are indistinguishable by design —
        // and with public mode on, a signed-out visitor following a
        // link to a private collection hits this on a completely normal
        // path. Rendering the API's raw string in a red danger banner
        // told them something had gone wrong. Nothing had.
        if (response?.status === 404) {
          notFound = true;
          return;
        }
        error = (apiErr as { error?: string } | undefined)?.error ?? t('collections.error_not_found');
        return;
      }
      collection = data as Collection;
    } finally {
      loading = false;
    }
    void loadMembers();
  }

  async function loadMembers() {
    membersLoading = true;
    try {
      const { data } = await api.GET('/collections/{id}/resources', {
        params: { path: { id }, query: { limit: 200 } },
      });
      members = ((data?.items ?? []) as MemberRow[]);
    } finally {
      membersLoading = false;
    }
  }

  function uploadHere() {
    upload.open_({ collectionId: id });
  }

  async function copyLink() {
    const link = `${location.origin}/collections/${id}`;
    try {
      await navigator.clipboard.writeText(link);
      copyFeedback = true;
      setTimeout(() => (copyFeedback = false), 1800);
    } catch {
      // No-op on clipboard failure.
    }
  }

  // Phase 1.55.C-1b: admin restore of a soft-deleted collection.
  // Only surfaced when the row IS soft-deleted AND the caller is
  // system.admin. Delegates to POST /admin/collections/{id}/restore.
  let restoreBusy = $state(false);
  let restoreError = $state<string | null>(null);
  async function restore() {
    if (!collection || !collection.deleted_at) return;
    restoreBusy = true;
    restoreError = null;
    try {
      const { error: apiErr } = await api.POST('/admin/collections/{id}/restore', {
        params: { path: { id } },
      });
      if (apiErr) {
        restoreError = (apiErr as { error?: string }).error ?? t('collections.restore_failed');
        return;
      }
      // Reload — the row is now live.
      await load();
    } finally {
      restoreBusy = false;
    }
  }

  function handleSaved(updated: Collection) {
    collection = updated;
    invalidateCovers(updated.id);
  }

  const visibilityLabel = $derived(
    collection?.visibility === 'public'
      ? t('collections.vis_public')
      : collection?.visibility === 'shared'
        ? t('collections.vis_shared')
        : t('collections.vis_private'),
  );
</script>

<svelte:head>
  <title>{collection?.name ?? t('collections.title')} — {site.name}</title>
</svelte:head>

<div class="w-full px-4 py-6 sm:px-6">
  {#if loading}
    <p class="text-fg-muted">{t('common.loading')}</p>
  {:else if notFound}
    <!-- Calm empty state, not an alert. See the 404 branch in load(). -->
    <div class="py-16 text-center" data-testid="collection-unavailable">
      <p class="text-base font-medium text-fg">{t('collections.not_found_or_private')}</p>
      <p class="mt-1 text-sm text-fg-muted">{t('collections.not_found_or_private_hint')}</p>
      {#if !auth.user}
        <p class="mt-1 text-sm text-fg-muted">{t('collections.not_found_sign_in_hint')}</p>
        <a
          href="/login"
          class="mt-4 inline-flex items-center rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent"
        >
          {t('user_menu.sign_in')}
        </a>
      {/if}
      <p class="mt-4">
        <a href="/collections" class="text-sm text-accent hover:underline">{t('collections.title')}</a>
      </p>
    </div>
  {:else if error}
    <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
      {error}
    </p>
  {:else if collection}
    <!-- Header -->
    <header class="mb-4">
      <nav class="text-xs text-fg-muted">
        <a href="/collections" class="hover:underline">{t('collections.title')}</a>
        <span class="px-1">/</span>
        <span>{collection.name}</span>
      </nav>
      <div class="mt-2 flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <h1 class="truncate text-2xl font-semibold">{collection.name}</h1>
            <span class="rounded-full bg-surface-elevated px-2 py-0.5 text-xs text-fg-muted">
              {visibilityLabel}
            </span>
          </div>
          {#if collection.description}
            <p class="mt-2 max-w-3xl text-sm text-fg-muted">{collection.description}</p>
          {/if}
        </div>
      </div>
    </header>

    <!-- Action toolbar -->
    <div class="mb-6 flex flex-wrap items-center gap-2 border-b border-border pb-3">
      <button
        type="button"
        disabled
        title={t('collections.add_posts_soon')}
        class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-3 py-1.5 text-sm text-fg-muted opacity-60"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <line x1="12" y1="5" x2="12" y2="19" />
          <line x1="5" y1="12" x2="19" y2="12" />
        </svg>
        {t('collections.add_posts')}
      </button>

      {#if collection?.deleted_at && auth.can('system.admin')}
        <div class="flex-1 rounded-md border border-warning/40 bg-warning-container/50 px-3 py-1.5 text-xs">
          <div class="font-medium text-warning">
            {t('collections.deleted_at_banner', { date: new Date(collection.deleted_at).toLocaleDateString() })}
            {#if collection.deleted_reason}
              {t('collections.deleted_reason', { reason: collection.deleted_reason })}
            {/if}
          </div>
          {#if restoreError}
            <div class="mt-1 text-danger">{restoreError}</div>
          {/if}
        </div>
        <button
          type="button"
          disabled={restoreBusy}
          onclick={() => void restore()}
          data-testid="collection-detail-restore-button"
          class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated disabled:opacity-50"
        >
          {restoreBusy ? t('collections.restoring') : t('collections.restore')}
        </button>
      {/if}

      {#if isOwner && !collection?.deleted_at}
        <button
          type="button"
          onclick={uploadHere}
          class="inline-flex items-center gap-1.5 rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
            <polyline points="17 8 12 3 7 8" />
            <line x1="12" y1="3" x2="12" y2="15" />
          </svg>
          {t('collections.upload_here')}
        </button>
      {/if}

      <button
        type="button"
        onclick={() => (shareOpen = true)}
        class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="18" cy="5" r="3" />
          <circle cx="6" cy="12" r="3" />
          <circle cx="18" cy="19" r="3" />
          <line x1="8.59" y1="13.51" x2="15.42" y2="17.49" />
          <line x1="15.41" y1="6.51" x2="8.59" y2="10.49" />
        </svg>
        {t('collections.share')}
      </button>

      <button
        type="button"
        onclick={copyLink}
        class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
          <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
        </svg>
        {copyFeedback ? t('common.copied') : t('common.copy_link')}
      </button>

      {#if isOwner}
        <Menu align="right">
          {#snippet trigger({ open })}
            <button
              type="button"
              aria-label={t('collections.more')}
              aria-haspopup="menu"
              aria-expanded={open}
              data-testid="collection-detail-more-button"
              class="inline-flex items-center rounded-md border border-border bg-surface px-2 py-1.5 text-sm hover:bg-surface-elevated"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <circle cx="12" cy="12" r="1" />
                <circle cx="19" cy="12" r="1" />
                <circle cx="5" cy="12" r="1" />
              </svg>
            </button>
          {/snippet}
          <button
            type="button"
            role="menuitem"
            onclick={() => (editOpen = true)}
            data-testid="collection-detail-edit-menuitem"
            class="block w-full px-3 py-1.5 text-left text-sm hover:bg-surface"
          >
            {t('collections.edit')}
          </button>
          <button
            type="button"
            role="menuitem"
            disabled
            class="block w-full px-3 py-1.5 text-left text-sm text-fg-muted opacity-60"
            title={t('collections.manage_members_soon')}
          >
            {t('collections.manage_members')}
          </button>
          <button
            type="button"
            role="menuitem"
            disabled
            class="block w-full px-3 py-1.5 text-left text-sm text-fg-muted opacity-60"
            title={t('collections.set_cover_soon')}
          >
            {t('collections.set_cover')}
          </button>
          <hr class="my-1 border-border" />
          <button
            type="button"
            role="menuitem"
            disabled
            class="block w-full px-3 py-1.5 text-left text-sm text-danger opacity-60"
            title={t('collections.delete_soon')}
          >
            {t('collections.delete')}
          </button>
        </Menu>
      {/if}
    </div>

    <!-- Member grid -->
    {#if membersLoading}
      <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 2xl:grid-cols-7">
        {#each { length: 10 } as _, i (i)}
          <div class="aspect-square animate-pulse rounded-lg bg-surface-elevated"></div>
        {/each}
      </div>
    {:else if members.length === 0}
      <section class="rounded-lg border border-dashed border-border bg-surface-elevated/50 px-6 py-12 text-center">
        <p class="text-sm text-fg-muted">{t('collections.detail_empty')}</p>
        {#if isOwner}
          <button
            type="button"
            onclick={uploadHere}
            class="mt-3 rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
          >
            {t('collections.upload_first')}
          </button>
        {/if}
      </section>
    {:else}
      <!-- Shared grid (#511/#582), so mode + tile size + sort match
           browse. Assets carry no list table, so `list` falls back to the
           grid here exactly as it does in UserProfile's asset section. -->
      <ContentGrid mode={browseView.mode} items={memberItems} tileMin={browseView.tileMin}>
        {#snippet card(item, mode)}
          <AssetCard asset={item} {mode} />
        {/snippet}
      </ContentGrid>
    {/if}
  {/if}
</div>

{#if collection}
  <!-- The shared floating view controls (mode switcher + tile size +
       sort), same bar as browse and the profile pages (#511/#582). No
       feed-filter `middle` snippet — Team/Trending/Latest/Following is
       meaningless for a fixed curated set.
       Mounted whenever the collection loaded, including when it is
       EMPTY: the controls carry no per-member state, and hiding them on
       an empty collection would make the chrome flicker in and out as
       the owner adds the first asset. -->
  <ViewControls />

  <EditCollectionModal
    open={editOpen}
    collection={collection}
    onclose={() => (editOpen = false)}
    onsaved={handleSaved}
  />
  <ShareCollectionModal
    open={shareOpen}
    collectionId={collection.id}
    onclose={() => (shareOpen = false)}
  />
{/if}
