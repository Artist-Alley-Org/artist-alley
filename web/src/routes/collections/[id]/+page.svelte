<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Collection detail page.
  //
  // Top section is the header bar — breadcrumbs + name + visibility
  // badge + owner. Below it sits an action toolbar (Upload here,
  // Share, Edit, More menu). The body is the membership, which is TWO
  // tables: posts (`collection_posts`, #882) and assets
  // (`collection_resources`).
  //
  // Two sections rather than one merged grid, deliberately. They are
  // different entities with different cards, different detail routes
  // and independent curator orderings — `sort_order` is per-table, so
  // there is no single sequence to interleave them into that the
  // curator ever arranged. A merged wall would have to invent one.
  //
  // Neither section renders when it is empty, so a collection of only
  // assets looks exactly as it did before #882.
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
  import { createScrollSnapshot } from '$lib/util/scrollSnapshot';
  import AssetCard from '$components/AssetCard.svelte';
  import PostCard from '$components/PostCard.svelte';
  import type { CardAsset, CardCoverAsset } from '$components/cardAsset';
  import ContentGrid from '$components/ContentGrid.svelte';
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

  // #883 — every asset-derived field is OPTIONAL because a member the
  // viewer may not see arrives as a placeholder that omits all of them.
  // `restricted` is the flag that says which shape this row is; reading
  // any field below without checking it first is the bug this issue
  // closes.
  interface MemberRow {
    asset_id: string;
    restricted?: boolean;
    owner_display_name?: string | null;
    title?: string;
    asset_type?: number;
    file_hash?: string | null;
    // Media type + blur-up (#595). A member tile renders through the
    // same CardThumb as a browse tile, which reads the media TYPE off
    // the extension alone — that is what puts the video / 3D badge on
    // the tile and what makes the hover sprite-scrub preview play.
    // These were missing from both this row type and the API response,
    // so every video and 3D member rendered as an untyped still.
    file_extension?: string | null;
    thumbhash?: string | null;
    sort_order: number;
    added_at: string;
    asset_created_at?: string | null;
    preview_available?: boolean;
    /** Every configured rung exists (#610). Feeds the card's responsive
     *  srcset (#502) — the API row carries it, so pass it through rather
     *  than letting the tile fall back to the square `col` crop. */
    ladder_available?: boolean;
    /** A `sprites.vtt` hover-scrub cue file exists (#835). The gate the
     *  card's hover preview now reads instead of guessing from the file
     *  extension; the CollectionResource row carries it. */
    scrub_available?: boolean;
    /** Recorded source dimensions (#640) — the masonry tile's aspect
     *  ratio, carried by the CollectionResource row. */
    pixel_width?: number | null;
    pixel_height?: number | null;
    /** The at-a-glance `show_on_card` strip (#552), server-resolved to
     *  display strings (#1133). Absent until this page's API row
     *  started carrying it, which is why the flag rendered on browse
     *  and on nothing here for a year. */
    card_fields?: Array<{ code: string; label: string; value: string }> | null;
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
  interface PostRow {
    id: string;
    title: string;
    author_user_ref: number;
    cover_asset_id?: string | null;
    created_at: string;
    like_count: number;
    comment_count: number;
    members: PostMemberRow[];
  }

  let collection = $state<Collection | null>(null);
  let members = $state<MemberRow[]>([]);
  let posts = $state<PostRow[]>([]);
  let loading = $state(true);
  let membersLoading = $state(true);
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
    sortedMembers.map((m) =>
      // #883 — a member the viewer may not see arrives as a placeholder:
      // `restricted: true`, the owner's display name, and NOT ONE of the
      // asset fields below. The zeros and empty strings here are the
      // card contract's required shape being satisfied with values the
      // restricted plate never reads; they are not data, and mapping the
      // absent API fields through `?? ''` on the normal branch instead
      // would have quietly turned a withheld title into a blank one.
      m.restricted
        ? {
            id: m.asset_id,
            title: '',
            file_hash: null,
            file_extension: null,
            thumbhash: null,
            asset_type: 0,
            created_at: m.added_at,
            preview_available: false,
            ladder_available: false,
            scrub_available: false,
            pixel_width: null,
            pixel_height: null,
            restricted: true,
            owner_display_name: m.owner_display_name ?? null,
          }
        : {
            id: m.asset_id,
            title: m.title ?? '',
            file_hash: m.file_hash ?? null,
            file_extension: m.file_extension ?? null,
            thumbhash: m.thumbhash ?? null,
            asset_type: m.asset_type ?? 0,
            created_at: m.asset_created_at ?? m.added_at,
            preview_available: !!m.preview_available,
            ladder_available: !!m.ladder_available,
            scrub_available: !!m.scrub_available,
            // #640 — the masonry tile's aspect ratio. The annotation
            // above is what forced this line to be written; without it
            // the member tiles would silently have gone back to being
            // squares in masonry while every other surface followed its
            // art.
            pixel_width: m.pixel_width ?? null,
            pixel_height: m.pixel_height ?? null,
            restricted: false,
            // #1133 — the at-a-glance strip. Passed through rather than
            // reconstructed: the server already resolved every slug to
            // its label (ADR 0012's rule, one home), so there is nothing
            // for this page to format.
            card_fields: m.card_fields ?? null,
          },
    ),
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
    void loadPosts();
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

  // Fired alongside loadMembers rather than after it — the two are
  // independent tables and neither section blocks the other's paint.
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
    {#if membersLoading || postsLoading}
      <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 2xl:grid-cols-7">
        {#each { length: 10 } as _, i (i)}
          <div class="aspect-square animate-pulse rounded-lg bg-surface-elevated"></div>
        {/each}
      </div>
    {:else if members.length === 0 && posts.length === 0}
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
      <!-- Posts first (#882). A collection that holds someone else's
           work holds it as a POST — that is the unit the author framed
           and the unit the reader saved — so it leads.

           Headings render only when BOTH sections have content: with one
           kind of member the labels are noise, and a collection of only
           assets must look exactly as it did before this landed. -->
      {#if posts.length > 0}
        {#if members.length > 0}
          <h2 class="mb-2 text-sm font-medium text-fg-muted">{t('collections.posts_heading')}</h2>
        {/if}
        <div data-testid="collection-posts">
          <ContentGrid mode={browseView.mode} items={posts} tileMin={browseView.tileMin}>
            {#snippet card(item, mode)}
              <PostCard post={item as PostRow} {mode} tileSizes={browseView.tileSizes} />
            {/snippet}
          </ContentGrid>
        </div>
      {/if}

      {#if members.length > 0}
        {#if posts.length > 0}
          <h2 class="mb-2 mt-6 text-sm font-medium text-fg-muted">{t('collections.assets_heading')}</h2>
        {/if}
        <!-- Shared grid (#511/#582), so mode + tile size + sort match
             browse. Assets carry no list table, so `list` falls back to the
             grid here exactly as it does in UserProfile's asset section. -->
        <div data-testid="collection-assets">
          <ContentGrid mode={browseView.mode} items={memberItems} tileMin={browseView.tileMin}>
            {#snippet card(item, mode)}
              <AssetCard asset={item} {mode} tileSizes={browseView.tileSizes} />
            {/snippet}
          </ContentGrid>
        </div>
      {/if}
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

     `ordered` is the pinned posts in the curator's order, so ← / → walk
     the collection. No `onEndReached`: the whole membership arrives in
     one request (limit 200), so there is no next page to spill into. -->
<PostParamHost ordered={() => posts.map((p) => p.id)} />

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
