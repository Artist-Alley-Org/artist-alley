<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Collection detail page.
  //
  // Top section is the header bar — breadcrumbs + name + visibility
  // badge + owner. Below it sits an action toolbar (Upload here,
  // Share, Edit, More menu). The body is the collection's POSTS
  // (`collection_posts`, #882) and nothing else.
  //
  // #1185 — this page used to render a SECOND grid underneath, the
  // bare assets pinned through `collection_resources`. It is gone. A
  // collection is a wall of posts: a post is the unit an author framed
  // and the unit a reader saves, and a bare asset belongs to whoever
  // uploaded it, not to a shared curated surface. That ruling also
  // deletes the reason the two-section layout existed — with one kind
  // of member there is one ordering, the curator's, and no headings to
  // disambiguate.
  //
  // `collection_resources` still EXISTS: the table, the API endpoints
  // and the "save an asset to a collection" writers are all untouched,
  // and dropping them is #1161's job. This route simply no longer reads
  // them — `GET /collections/{id}/resources` has no caller here any more.
  //
  // The post grid renders through the shared ContentGrid + the
  // floating ViewControls bar (#582), the same chrome browse and the
  // profile pages use — a collection is a card-showing surface, so it
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
  import { createScrollSnapshot } from '$lib/util/scrollSnapshot';
  import PostCard from '$components/PostCard.svelte';
  import type { CardCoverAsset } from '$components/cardAsset';
  import ContentGrid from '$components/ContentGrid.svelte';
  import PostListTable from '$components/PostListTable.svelte';
  import ViewControls from '$components/ViewControls.svelte';
  import PostParamHost from '$components/PostParamHost.svelte';
  import Menu from '$components/Menu.svelte';
  import EditCollectionModal from '$components/EditCollectionModal.svelte';
  import ShareEntityModal from '$components/ShareEntityModal.svelte';
  import ConfirmDeleteDialog from '$components/ConfirmDeleteDialog.svelte';
  import { goto } from '$app/navigation';
  import { toasts } from '$stores/toasts.svelte';
  import { canDelete, deleteEntity, restoreEntity, shouldAskReason } from '$lib/deletable';

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
    // #1027 — the curator's chosen cover. Carried on this page's own
    // Collection so the edit modal opens with the current choice
    // already selected rather than reading "use mosaic" and clearing it
    // on the next unrelated save.
    cover_asset_id?: string | null;
  }

  // #882 — a post pinned in this collection. The API returns the FULL
  // `Post` schema (the same objects `GET /posts` returns), so this page
  // renders the same PostCard as browse instead of a second, narrower
  // post shape that would drift from it.
  //
  // The member's `asset` is the SHARED card feed contract (#595), same
  // as browse declares — not a re-spelled inline shape. That type is
  // what makes file_extension / thumbhash / the three availability
  // flags impossible to drop silently, and this page is the surface
  // #595 was written about.
  //
  // A post the viewer may not read is simply ABSENT from this list, not
  // a placeholder: membership never widens a post (#883), and unlike an
  // asset member there is no request-access flow for a placeholder to
  // lead to. `restricted` still appears on a MEMBER, though — a post
  // you may read can carry a cover you may not.
  interface PostMemberRow {
    asset_id: string;
    sort_order: number;
    asset?: CardCoverAsset;
    restricted?: boolean;
    owner_display_name?: string;
  }
  /** A post pinned in this collection.
   *
   *  ⚠️ WIDENED IN #1137, and the reason is the #1099 lesson rather than
   *  a new requirement. `GET /collections/{id}/posts` returns
   *  `PostList` — the SAME schema the browse feed reads — so every field
   *  below has always been on the wire. This interface simply declared
   *  the subset the card happened to render, and the moment the list
   *  table (which reads the rest) was pointed at these rows, TypeScript
   *  called it a shape mismatch. It was not: it was a local type that
   *  under-described real data, which is exactly how #1099's surface
   *  ended up unable to use a component it was already compatible with.
   *
   *  Add fields here from the schema, not from what the current card
   *  reads. */
  interface PostRow {
    id: string;
    title: string;
    description: string;
    visibility: string;
    author_user_ref: number;
    cover_asset_id?: string | null;
    posted_at: string;
    created_at: string;
    updated_at: string;
    like_count: number;
    comment_count: number;
    tags: string[];
    members: PostMemberRow[];
  }

  let collection = $state<Collection | null>(null);
  let posts = $state<PostRow[]>([]);
  let loading = $state(true);
  let postsLoading = $state(true);
  let error = $state<string | null>(null);
  // Separate from `error` on purpose: one is "we could not load this",
  // the other is "this is not yours to see", and they should not look
  // the same to a visitor.
  let notFound = $state(false);
  let editOpen = $state(false);
  // #1027 — which part of the edit modal to land on. Reset on close
  // rather than on open, so "Edit details" after "Set cover" starts at
  // the top of the form again instead of inheriting the last entry
  // point.
  let editFocusCover = $state(false);
  let shareOpen = $state(false);
  let copyFeedback = $state(false);

  const id = $derived(page.params.id ?? '');
  const isOwner = $derived(!!collection && !!auth.user && collection.owner_user_ref === auth.user.ref);

  // Come back from an asset with the grid where you left it (#584).
  // Scroll offset only: the whole membership arrives in one request, so
  // once that resolves the grid is exactly as tall as it was — nothing
  // to hand back. (createScrollSnapshot keeps re-applying the offset
  // while the fetch is in flight.)
  export const snapshot = createScrollSnapshot();

  // Mode + sort come from the GLOBAL browseView store (localStorage), so
  // a collection shares the view preference with browse and the profile
  // pages. SEAM: per-surface view state is probably reworked in the
  // future; the coupling to the one store is the simplest-correct option
  // for now — same call UserProfile documents.
  //
  // The BASE order is the curator's `sort_order` (ADR 0009 — a collection
  // is "an ordered, optionally-shared" set, and the server already
  // returns `ORDER BY cp.sort_order ASC, cp.added_at ASC`). The sort
  // toggle REVERSES that order; it deliberately does not re-sort by date,
  // which would throw away the curation.
  //
  // #1185 — this reversal used to be applied to the ASSET grid only, so
  // removing that grid would have left the footer's sort toggle wired to
  // nothing on this route. The rule was never about assets; it is the
  // curator-order rule, and the posts wall is now the curated sequence.
  const orderedPosts = $derived(
    browseView.feedDir === 'asc' ? [...posts].reverse() : posts,
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
    void loadPosts();
  }

  async function loadPosts() {
    postsLoading = true;
    try {
      const { data } = await api.GET('/collections/{id}/posts', {
        params: { path: { id }, query: { limit: 200 } },
      });
      posts = (data?.items ?? []) as unknown as PostRow[];
    } finally {
      postsLoading = false;
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
  }

  // ── Delete (#981) ─────────────────────────────────────────────────
  // This menu item was hardcoded `disabled` behind a `delete_soon`
  // tooltip. DELETE /collections/{id} has existed and been gated by
  // canMutateCollection the whole time; nothing in the product could
  // call it.
  //
  // The item renders for the owner or a GLOBAL collections.admin
  // holder — canMutateCollection's exact disjunction, which for
  // collections has no team-scoped branch at all, so unlike assets and
  // posts the client mirrors the server rule completely here.
  const canDeleteCollection = $derived(
    !!collection && !collection.deleted_at && canDelete('collection', collection.owner_user_ref),
  );

  let deleteOpen = $state(false);
  let deleteBusy = $state(false);
  let deleteError = $state<string | null>(null);

  async function confirmDelete(reason: string) {
    if (!collection || deleteBusy) return;
    deleteBusy = true;
    deleteError = null;
    const err = await deleteEntity('collection', collection.id, reason);
    deleteBusy = false;
    if (err) {
      deleteError = err;
      return;
    }
    const deletedId = collection.id;
    deleteOpen = false;
    toasts.push({
      message: t('delete_confirm.deleted_collection'),
      href: '/account/trash',
      linkLabel: t('delete_confirm.view_trash'),
      action: { label: t('delete_confirm.undo'), run: () => undoDelete(deletedId) },
    });
    // This route renders the thing that no longer exists, so it cannot
    // stay. The index is where the user came from.
    await goto('/collections');
  }

  // Safe to offer: we performed the delete, so we are the deleter, and
  // auth.CanRestoreDeleted grants restore to the deleter.
  async function undoDelete(collectionId: string) {
    const err = await restoreEntity('collection', collectionId);
    if (err) {
      toasts.push({
        message: t('delete_confirm.undo_error'),
        tone: 'error',
        href: '/account/trash',
        linkLabel: t('delete_confirm.view_trash'),
      });
      return;
    }
    toasts.push({ message: t('delete_confirm.undone'), href: `/collections/${collectionId}` });
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
      <!-- The disabled "Add posts" button that used to sit here is gone
           (#882). Post membership exists now, and it is reached from the
           thing being saved — a post's ⋮ menu, "Save to collection…" —
           which is where the picker already lives for assets. A second,
           reverse-direction picker (choose posts FROM here) is a
           different modal and a different sprint; leaving a permanently
           disabled control standing in for it advertised a feature that
           had shipped somewhere else. The empty state below says where
           to go instead. -->

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

      <!-- #918 — Share grants ACL rows, and POST /collections/{id}/acls
           refuses anyone who is not the owner. Offering the button to a
           reader made the 403 ("not the collection owner", surfaced by
           the #915 dialog) the FIRST thing they heard about it. Nothing
           leaked, but an action that cannot succeed should not be on
           offer.

           `isOwner` and not "owner or collections admin", even though the
           server also admits CapCollectionsAdmin / CapSystemAdmin: every
           other management affordance on this page (edit, manage members,
           set cover, delete) already gates on plain ownership, so
           widening this one alone would make Share the only admin-visible
           control on a toolbar of owner-only ones. Copy link stays
           ungated below — anyone who can read the page can link to it. -->
      {#if isOwner}
        <button
          type="button"
          onclick={() => (shareOpen = true)}
          data-testid="collection-detail-share-button"
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
      {/if}

      <!-- #910 — the end of the dead end. You could find a collection and
           then there was nowhere to go: no way to search within it.
           Ungated for the same reason Copy link is — anyone who can read
           this page can search inside it, and the server re-checks both
           the collection AND every member anyway.

           An <a>, not a fetch: the destination is a real, shareable
           address (`/search?filter=collection:<id>`), the scope is one
           more term in the same `filter=` vocabulary the facet chips
           use, and search has no new parameter to learn. It lands with
           the scope chip already showing and the query box empty, which
           is the honest state — a collection scope is not itself a
           query. -->
      <a
        href="/search?filter=collection:{collection.id}"
        data-testid="collection-detail-search-within"
        class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <circle cx="11" cy="11" r="8" />
          <line x1="21" y1="21" x2="16.65" y2="16.65" />
        </svg>
        {t('collections.search_within')}
      </a>

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

      <!-- The trigger opens for an owner OR for someone who may delete
           this (#981) — a global collections.admin holder, the instance
           moderator role. Every item inside is separately gated, so a
           non-owner deleter gets a one-item menu rather than an empty
           panel or a set of controls the server would refuse. -->
      {#if isOwner || canDeleteCollection}
        <Menu
          align="right"
          triggerTestId="collection-detail-more-button"
          triggerClass="inline-flex rounded-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {#snippet trigger({ open })}
            <!-- #1109 — a SPAN, and the testid + aria-haspopup /
                 aria-expanded move onto the button `Menu` already
                 renders around this snippet. It used to be a nested
                 `<button aria-haspopup="menu">`: invalid markup, a
                 duplicate menu trigger in the accessibility tree, and
                 an `aria-expanded` that had to be kept in sync by hand.
                 `collection-detail-more-button` is unchanged, so the
                 ui-18 / ui-35 / ui-36 specs still find it. -->
            <span
              aria-label={t('collections.more')}
              class="inline-flex items-center rounded-md border border-border bg-surface px-2 py-1.5 text-sm hover:bg-surface-elevated"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <circle cx="12" cy="12" r="1" />
                <circle cx="19" cy="12" r="1" />
                <circle cx="5" cy="12" r="1" />
              </svg>
            </span>
          {/snippet}
          {#if isOwner}
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
            <!-- #1027 — live as of the cover picker. This entry was a
                 disabled "coming soon" stub placed here in anticipation;
                 leaving it disabled in the same release that ships the
                 picker would tell a curator the feature does not exist
                 while the working control sat behind "Edit details".
                 It opens the SAME modal, focused on the cover section,
                 so there is one edit surface and one save path. -->
            <button
              type="button"
              role="menuitem"
              onclick={() => {
                editFocusCover = true;
                editOpen = true;
              }}
              data-testid="collection-detail-set-cover-menuitem"
              class="block w-full px-3 py-1.5 text-left text-sm hover:bg-surface"
            >
              {t('collections.set_cover')}
            </button>
          {/if}
          {#if canDeleteCollection}
            {#if isOwner}
              <hr class="my-1 border-border" />
            {/if}
            <button
              type="button"
              role="menuitem"
              onclick={() => {
                deleteError = null;
                deleteOpen = true;
              }}
              data-testid="collection-detail-delete-menuitem"
              class="block w-full px-3 py-1.5 text-left text-sm text-danger hover:bg-danger-container"
            >
              {t('collections.delete')}
            </button>
          {/if}
        </Menu>
      {/if}
    </div>

    <!-- Membership -->
    {#if postsLoading}
      <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 2xl:grid-cols-7">
        {#each { length: 10 } as _, i (i)}
          <div class="aspect-square animate-pulse rounded-lg bg-surface-elevated"></div>
        {/each}
      </div>
    {:else if posts.length === 0}
      <section class="rounded-lg border border-dashed border-border bg-surface-elevated/50 px-6 py-12 text-center">
        <p class="text-sm text-fg-muted">{t('collections.detail_empty')}</p>
        <p class="mt-1 text-xs text-fg-muted">{t('collections.detail_empty_hint')}</p>
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
      <!-- The whole body (#882, #1185). A collection that holds someone
           else's work holds it as a POST — that is the unit the author
           framed and the unit the reader saved.

           No heading: there is one kind of member, so a label over the
           only grid on the page is noise. It existed to disambiguate the
           posts wall from the assets grid below it, and that grid is
           gone. -->
      <div data-testid="collection-posts">
        <ContentGrid mode={browseView.mode} items={orderedPosts} tileMin={browseView.tileMin}>
          {#snippet card(item, mode)}
            <PostCard post={item as PostRow} {mode} tileSizes={browseView.tileSizes} />
          {/snippet}
          {#snippet list()}
            <!-- #1137. This snippet was simply MISSING, and its absence
                 is the whole of the reported bug for the posts half: a
                 collection's posts are the same rows the browse feed
                 passes to this exact table, so `list` mode fell through
                 to the grid branch and drew tiles while the control said
                 LIST. Not a payload problem and not a shape mismatch —
                 an omission. -->
            <PostListTable items={orderedPosts} loading={false} />
          {/snippet}
        </ContentGrid>
      </div>
    {/if}
  {/if}
</div>

<!-- #1130 — the `?post=` viewer host. A post card's primary click writes
     the param onto THIS url and expects something here to overlay the
     post; nothing did, so clicking a pinned post inside a collection
     changed the address bar and nothing else. Never a regression: the
     post grid arrived in #882 without a host and the gap shipped with
     it.

     Declared at the route's top level, NOT inside the `{#if collection}`
     block below with the edit / share / delete dialogs. Those are
     `<dialog>`s, and ADR 0067's amendment records what happens to a
     viewer declared inside one: `Modal` portals to the nearest open
     dialog resolved from where it is DECLARED, so it would render
     underneath their top layer — in the DOM, invisible on screen.

     `ordered` is the pinned posts AS THE WALL SHOWS THEM — the curator's
     order, reversed when the footer's sort toggle is flipped — so ← / →
     walk the collection in the sequence the reader is looking at rather
     than in the one the API happened to return. No `onEndReached`: the
     whole membership arrives in one request (limit 200), so there is no
     next page to spill into. -->
<PostParamHost ordered={() => orderedPosts.map((p) => p.id)} />

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
    focusCover={editFocusCover}
    onclose={() => {
      editOpen = false;
      editFocusCover = false;
    }}
    onsaved={handleSaved}
  />
  <ShareEntityModal
    open={shareOpen}
    kind="collection"
    id={collection.id}
    onclose={() => (shareOpen = false)}
  />
  <ConfirmDeleteDialog
    open={deleteOpen}
    kind="collection"
    title={collection.name}
    askReason={shouldAskReason(collection.owner_user_ref)}
    busy={deleteBusy}
    error={deleteError}
    onconfirm={confirmDelete}
    onclose={() => {
      if (!deleteBusy) deleteOpen = false;
    }}
  />
{/if}
