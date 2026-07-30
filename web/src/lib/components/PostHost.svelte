<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // PostHost — the post-specific host for AssetPlaylist.
  //
  // A Post is, structurally, an AssetPlaylist with social skin: its
  // members ARE the playlist, and the sidebar of author / description
  // / likes / comments / tags / edit-delete is the skin painted on
  // top. PostHost builds the PostPlaylistSource and provides the
  // social-skin snippet; the generic AssetPlaylist shell handles all
  // the playlist behaviour (cursor, filmstrip, navigation, etc).
  //
  // Replaces the old PostModal: same UX, same data, but the playlist
  // concerns are now in AssetPlaylist + the post concerns are isolated
  // here so the shell is reusable for CollectionHost / ReviewHost /
  // SearchHost in follow-up commits.

  import { onMount, onDestroy } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import AssetPlaylist from './AssetPlaylist.svelte';
  import CollectionPicker from './CollectionPicker.svelte';
  import CommentsThread from './CommentsThread.svelte';
  import FollowButton from './FollowButton.svelte';
  import Menu from './Menu.svelte';
  import WhiteboardCanvas from './whiteboard/WhiteboardCanvas.svelte';
  import BrushCanvas from './whiteboard/BrushCanvas.svelte';
  import DetailsIcon from './viewers/tools/DetailsTool/Icon.svelte';
  import { DetailsTips } from './viewers/tools/DetailsTool';
  import { defineSnippetTool } from './viewers/tools/snippet-tool';
  import { snippetToolHookKey } from './viewers/tools/contract';
  import { createPostPlaylistSource } from '$lib/playlist/postSource.svelte';
  import { createWhiteboardSession } from '$lib/whiteboard/session.svelte';
  import { normalizeDoc, type BrushContent } from '$lib/whiteboard/types';
  import { t } from '$stores/lang.svelte';

  interface Props {
    postId: string;
    onClose: () => void;
    standalone?: boolean;
    /** Optional sibling-post navigator. Wired by the browse-feed
        overlay (sibling = next/prev post in the current feed page);
        the standalone /posts/{id} route omits it since there's no
        sibling context. AssetPlaylist binds ← / → to this. */
    onNavigateSibling?: (dir: 'prev' | 'next') => void;
  }

  let { postId, onClose, standalone = false, onNavigateSibling }: Props = $props();

  // ── Source ────────────────────────────────────────────────────────
  // The PostPlaylistSource owns the post fetch + member list. The
  // shell binds to `pl.source`; we read `pl.aux.post` to render the
  // social skin.
  //
  // We keep the same source instance for the host's whole lifetime
  // and re-target it via setPostId() on postId change. That preserves
  // the shell's mounted state (AssetViewer, ViewerMenuBar, dialog
  // chrome) — old post stays on screen until the new data arrives,
  // then swaps atomically. Browse-feed ←/→ navigation looks "static"
  // to the user: only the asset / sidebar contents change, not the
  // surrounding chrome.
  const pl = createPostPlaylistSource(postId);
  $effect(() => {
    pl.setPostId(postId);
  });

  // ── Post-specific side state ──────────────────────────────────────

  interface UserPublic {
    ref: number;
    username: string;
    display_name: string;
    bio?: string;
    avatar_url?: string | null;
    location?: string;
    member_since: string;
    post_count: number;
  }

  interface AssetFieldValue {
    field_id: string;
    field_code: string;
    field_label?: string;
    type: 'text' | 'longtext' | 'rich_text' | 'number' | 'boolean'
        | 'date' | 'datetime' | 'select' | 'multi_select' | 'tree' | 'reference';
    value_text?: string | null;
    value_num?: number | null;
    value_date?: string | null;
    value_options?: string[] | null;
    value_ref?: string | null;
    set_by: string;
    set_at: string;
  }

  let author = $state<UserPublic | null>(null);
  let liked = $state(false);
  let likeBusy = $state(false);

  // Per-asset field values, keyed by asset_id so going back to a
  // previously-viewed member serves from cache. Refetched as the
  // cursor moves to a not-yet-loaded asset.
  let fieldsByAsset = $state<Map<string, AssetFieldValue[]>>(new Map());

  // ── Whiteboard state ──────────────────────────────────────────────
  // Whiteboards are per-post brush sketches. The Tools dropdown's
  // Whiteboard item flips `whiteboardOpen`; the overlay component
  // owns the drawing surface and POSTs on Save, then onSaved fires
  // back here to refresh the sidebar list. We treat the API response
  // as a generic Comment row (annotation_type='whiteboard',
  // annotation_data carries the BrushContent JSON) and unpack here.
  interface WhiteboardRow {
    id: string;
    author_user_ref: number;
    body: string; // title (empty if untitled)
    created_at: string;
    annotation_data?: BrushContent | null;
  }
  let whiteboardOpen = $state(false);
  let whiteboards = $state<WhiteboardRow[]>([]);
  let whiteboardLoading = $state(false);
  let previewedWhiteboard = $state<WhiteboardRow | null>(null);
  // The session is the single source of truth for the whiteboard
  // canvas + tool panel. Created lazily so we don't allocate one
  // unless the user opens the surface; recreated on close so the
  // next session starts fresh (drafts aren't preserved across
  // close/reopen — that lands with the autosave phase).
  const WB_SOURCE_W = 1920;
  const WB_SOURCE_H = 1080;
  let whiteboardSession = $state<ReturnType<typeof createWhiteboardSession> | null>(null);
  let whiteboardSaving = $state(false);
  let whiteboardSaveError = $state<string | null>(null);
  // Whiteboard compact / rail state retired here — ToolPanelShell
  // owns the toggle (chevron in the panel header) + the localStorage
  // persistence under its own key (aa.viewer.paneCompact). Removing
  // the duplicate keeps a single source of truth for the flag.


  // OVERRIDE the built-in Details tool. The merge logic in
  // AssetViewer replaces any built-in tool whose id matches a host-
  // injected one, so registering Details here with id='details' takes
  // the Details slot entirely — post info / likes / comments /
  // whiteboard list / metadata are what "Details" means in the post
  // context. The standalone /assets/[id] route doesn't register this
  // and falls back to the generic asset-info Details body.
  //
  // Body lives as a snippet in this file's scope (postSocialPane
  // below) so the 25+ reactive references to PostHost-local state
  // (post, author, isOwner, whiteboards, currentFields, …) stay in
  // scope without a giant props surface. The SnippetBody adapter
  // resolves the snippet through hostHooks under the conventional
  // `tool:details` key.
  const postDetailsTool = defineSnippetTool({
    id: 'details',
    label: 'Details',
    order: 0,
    Icon: DetailsIcon,
    // Dynamic label — panel header + menubar trigger read
    // "{post title} Details" so the user always knows which post
    // they're viewing without the title also being duplicated in
    // the body. Falls back to plain "Details" on the brief render
    // tick before `post` has loaded.
    labelFn: () => (post?.title ? `${post.title} Details` : 'Details'),
    // Share the built-in Details Tips so the global gestures
    // (pan / zoom / reset / fullscreen / tile-toggle) stay
    // documented when PostHost takes the Details slot.
    Tips: DetailsTips,
  });

  // Read-only session for the sidebar-click preview overlay. Lazily
  // built from the previewed whiteboard's annotation_data; null when
  // no whiteboard is being previewed.
  let previewSession = $state<ReturnType<typeof createWhiteboardSession> | null>(null);
  $effect(() => {
    const wb = previewedWhiteboard;
    if (wb?.annotation_data) {
      const normalized = normalizeDoc(wb.annotation_data);
      const s = createWhiteboardSession(normalized.source_w || 1920, normalized.source_h || 1080);
      s.load(normalized);
      previewSession = s;
    } else {
      previewSession = null;
    }
  });

  function toggleWhiteboard() {
    if (previewedWhiteboard) previewedWhiteboard = null;
    if (whiteboardOpen) {
      // Closing — let the session die.
      whiteboardOpen = false;
      whiteboardSession = null;
      whiteboardSaveError = null;
    } else {
      // Opening — fresh session.
      whiteboardSession = createWhiteboardSession(WB_SOURCE_W, WB_SOURCE_H);
      whiteboardSaveError = null;
      whiteboardOpen = true;
    }
  }

  async function saveWhiteboard() {
    if (!whiteboardSession || !post || whiteboardSaving) return;
    const doc = whiteboardSession.doc;
    const empty = doc.layers.every((l) => l.items.length === 0);
    if (empty) {
      whiteboardSaveError = 'Draw something first.';
      return;
    }
    whiteboardSaving = true;
    whiteboardSaveError = null;
    try {
      const { error } = await api.POST('/posts/{id}/whiteboards', {
        params: { path: { id: post.id } },
        body: {
          content: {
            source_w: doc.source_w,
            source_h: doc.source_h,
            layers: doc.layers.map((l) => ({
              id: l.id,
              name: l.name,
              visible: l.visible,
              opacity: l.opacity,
              locked: l.locked ?? false,
              items: l.items.map((it) => ({ ...it })),
            })),
          },
        } as unknown as never,
      });
      if (error) {
        whiteboardSaveError = (error as { error?: string } | undefined)?.error ?? 'Save failed.';
        return;
      }
      // Refresh the sidebar list, then close the whiteboard back to
      // the regular sidebar — the user explicitly saved so they're
      // done with this sketch. They can open a fresh one anytime.
      onWhiteboardSaved();
      whiteboardOpen = false;
      whiteboardSession = null;
    } catch (e) {
      whiteboardSaveError = e instanceof Error ? e.message : 'Save failed.';
    } finally {
      whiteboardSaving = false;
    }
  }

  async function loadWhiteboards(id: string) {
    whiteboardLoading = true;
    try {
      const { data } = await api.GET('/posts/{id}/whiteboards', {
        params: { path: { id } },
      });
      if (data) {
        whiteboards = (data as unknown as WhiteboardRow[]).map((c) => ({
          id: c.id,
          author_user_ref: c.author_user_ref,
          body: c.body ?? '',
          created_at: c.created_at,
          annotation_data: c.annotation_data ?? null,
        }));
      }
    } catch {
      whiteboards = [];
    } finally {
      whiteboardLoading = false;
    }
  }

  // Refetch when the post id changes.
  $effect(() => {
    if (!post) return;
    void loadWhiteboards(post.id);
  });

  function onWhiteboardSaved() {
    if (post) void loadWhiteboards(post.id);
  }

  async function deleteWhiteboard(id: string) {
    if (!confirm('Delete this whiteboard?')) return;
    await api.DELETE('/comments/{id}', { params: { path: { id } } });
    if (post) void loadWhiteboards(post.id);
    if (previewedWhiteboard?.id === id) previewedWhiteboard = null;
  }

  const post = $derived(pl.aux.post);
  const currentItem = $derived(pl.source.items[pl.source.cursor] ?? null);
  const currentAssetId = $derived(currentItem?.id ?? null);
  // Title of the asset under the cursor, trimmed. Empty when there's
  // no item or the asset has no title set. The title surfaces in both
  // the top-bar title slot and the sidebar h1's subhead so the user
  // sees feedback as they navigate ↑/↓ through a multi-asset post.
  const currentAssetTitle = $derived((currentItem?.asset?.title ?? '').trim());
  // Whether to surface the asset title as distinct from the post
  // title — for single-asset posts they're often identical and
  // duplicating the text just adds noise.
  const showAssetSubtitle = $derived(
    currentAssetTitle !== '' && currentAssetTitle !== (post?.title ?? '').trim(),
  );
  const currentFields = $derived<AssetFieldValue[]>(
    currentAssetId ? (fieldsByAsset.get(currentAssetId) ?? []) : [],
  );
  const isOwner = $derived(
    !!auth.user && !!post && auth.user.ref === post.author_user_ref,
  );
  const postedRelative = $derived(post ? relativeTime(post.posted_at) : '');
  const postedAbsolute = $derived(post ? new Date(post.posted_at).toLocaleString() : '');

  // Load author + like state once the post resolves.
  $effect(() => {
    if (!post) return;
    void loadAuthor(post.author_user_ref);
    void loadLikeState(post.id);
  });

  // Load per-asset field values as the cursor moves.
  $effect(() => {
    const aid = currentAssetId;
    if (!aid) return;
    if (fieldsByAsset.has(aid)) return;
    void loadFields(aid);
  });

  async function loadAuthor(ref: number) {
    try {
      const { data } = await api.GET('/users/{ref}', {
        params: { path: { ref } },
      });
      if (data) author = data as UserPublic;
    } catch {
      // Soft-fail — author header renders a placeholder.
    }
  }

  async function loadLikeState(id: string) {
    try {
      const { data } = await api.GET('/posts/{id}/like', {
        params: { path: { id } },
      });
      if (data) liked = data.liked;
    } catch {
      liked = false;
    }
  }

  async function loadFields(assetId: string) {
    try {
      const { data } = await api.GET('/assets/{id}/fields', {
        params: { path: { id: assetId } },
      });
      if (data) {
        fieldsByAsset.set(assetId, data as AssetFieldValue[]);
        // Reassign to trigger reactive update.
        fieldsByAsset = new Map(fieldsByAsset);
      }
    } catch {
      // Soft-fail.
    }
  }

  async function toggleLike() {
    if (!post || likeBusy) return;
    likeBusy = true;
    const wasLiked = liked;
    liked = !wasLiked;
    post.like_count = Math.max(post.like_count + (wasLiked ? -1 : 1), 0);
    try {
      const path = { id: post.id };
      const { error: apiErr } = wasLiked
        ? await api.DELETE('/posts/{id}/like', { params: { path } })
        : await api.POST('/posts/{id}/like', { params: { path } });
      if (apiErr) {
        liked = wasLiked;
        post.like_count = Math.max(post.like_count + (wasLiked ? 1 : -1), 0);
      }
    } catch {
      liked = wasLiked;
      if (post) post.like_count = Math.max(post.like_count + (wasLiked ? 1 : -1), 0);
    } finally {
      likeBusy = false;
    }
  }

  async function gotoTag(tag: string) {
    const target = new URL(page.url);
    target.pathname = '/';
    target.searchParams.delete('post');
    target.searchParams.set('tag', tag);
    await goto(target.pathname + target.search);
  }

  // ── Formatters ────────────────────────────────────────────────────

  function relativeTime(iso: string): string {
    const d = new Date(iso).getTime();
    const now = Date.now();
    const sec = Math.round((now - d) / 1000);
    if (sec < 60) return 'just now';
    const min = Math.round(sec / 60);
    if (min < 60) return `${min}m ago`;
    const hr = Math.round(min / 60);
    if (hr < 24) return `${hr}h ago`;
    const day = Math.round(hr / 24);
    if (day < 30) return `${day}d ago`;
    const mo = Math.round(day / 30);
    if (mo < 12) return `${mo}mo ago`;
    return `${Math.round(mo / 12)}y ago`;
  }

  function initials(name: string): string {
    const parts = name.trim().split(/\s+/).slice(0, 2);
    return parts.map((p) => p[0]?.toUpperCase() ?? '').join('') || '?';
  }

  function formatFieldValue(f: AssetFieldValue): string {
    switch (f.type) {
      case 'text':
      case 'longtext':
      case 'rich_text':
        return (f.value_text ?? '').trim();
      case 'number':
        return f.value_num == null ? '' : String(f.value_num);
      case 'boolean':
        return f.value_text === 'true' ? 'Yes' : f.value_text === 'false' ? 'No' : '';
      case 'date':
      case 'datetime': {
        if (!f.value_date) return '';
        const d = new Date(f.value_date);
        if (isNaN(d.getTime())) return '';
        if (f.field_code === 'pokemon_release_date') return String(d.getUTCFullYear());
        return f.type === 'date' ? d.toLocaleDateString() : d.toLocaleString();
      }
      case 'select':
        return f.value_text ?? '';
      case 'multi_select':
        return (f.value_options ?? []).join(', ');
      case 'tree':
      case 'reference':
        return f.value_ref ?? '';
      default:
        return '';
    }
  }

  // Lifecycle housekeeping the shell doesn't own (the host owns body
  // scroll-lock indirectly via shell's onMount; nothing post-specific
  // to do here at the moment).
  onMount(() => {});
  onDestroy(() => {});

  // ── Collection-picker state ───────────────────────────────────────
  // pickerAssetIds is the working set the picker operates on. For
  // single-asset (Edit menu) we set it to [currentAssetId]; for the
  // bulk action we set it to every item id. Driving both flows from
  // one state field keeps the modal rendered at most once.
  let pickerAssetIds = $state<string[]>([]);
  let pickerOpen = $state(false);

  function openPickerForCurrent(assetId: string) {
    pickerAssetIds = [assetId];
    pickerOpen = true;
  }
  function openPickerForAll() {
    pickerAssetIds = pl.source.items.map((it) => it.id);
    pickerOpen = true;
  }
  function closePicker() {
    pickerOpen = false;
  }

  // ── Recreate previews ─────────────────────────────────────────────
  // Calls POST /assets/{id}/preview — the server enqueues the right
  // preview.<kind> job at PriorityHigh with force=true (the endpoint's
  // default), which re-renders outputs that already exist.
  //
  // It did not always. Until #760 the job hit each handler's "this
  // variant is already in storage" check, skipped everything and
  // completed successfully, so this menu item returned 202 and changed
  // nothing — the click was a no-op wearing a success response. Do not
  // pass force:false here without meaning it.
  let recreating = $state<Record<string, boolean>>({});
  async function recreatePreviews(assetId: string) {
    if (recreating[assetId]) return;
    recreating[assetId] = true;
    const { error } = await api.POST('/assets/{id}/preview', {
      params: { path: { id: assetId } },
    });
    recreating[assetId] = false;
    if (error) {
      // Loud-fail so the user knows the click didn't take effect.
      // No toast utility yet; alert is the simplest visible feedback
      // until a proper notifications system lands.
      alert(
        'Recreate previews failed: ' +
          ((error as { error?: string }).error ?? 'unknown error'),
      );
    }
    // Success is silent — the variant URLs will refresh on next
    // browser load; the worker writes new bytes when it finishes.
  }

  // ── Stub actions ──────────────────────────────────────────────────
  // Hook slots exist so AssetViewer's Edit/File menus surface every
  // planned per-asset action. PostHost stubs each here with an alert
  // until the real implementations land. When a real implementation
  // arrives, swap the stub here — no component changes needed.
  function stubAction(label: string) {
    alert(`${label} — coming soon (stub).`);
  }
  function editTags(_assetId: string) {
    stubAction('Edit tags');
  }
  function editMetadata(_assetId: string) {
    stubAction('Edit metadata');
  }
  function downloadVariant(_assetId: string) {
    stubAction('Download variant');
  }
  function shareAsset(_assetId: string) {
    stubAction('Share asset');
  }
  function deleteAsset(_assetId: string) {
    stubAction('Delete asset');
  }
  function bulkDownloadZip() {
    stubAction('Download all as ZIP');
  }
  function bulkTag() {
    stubAction('Tag all');
  }
  function sharePlaylist() {
    stubAction('Share playlist');
  }
  function editPost() {
    stubAction('Edit post');
  }
  function deletePost() {
    stubAction('Delete post');
  }
</script>

<AssetPlaylist
  source={pl.source}
  {onClose}
  {standalone}
  {onNavigateSibling}
  customTools={[postDetailsTool]}
  whiteboardSession={whiteboardOpen ? (whiteboardSession ?? undefined) : undefined}
  hostHooks={{
    [snippetToolHookKey('details')]: { body: postSocialPane },
    whiteboard: {
      // onActivate fires when the user picks Whiteboard from the
      // menubar's Tools picker. Open the overlay only if it isn't
      // already on so re-selecting the active tool stays a no-op.
      onActivate: () => { if (!whiteboardOpen) toggleWhiteboard(); },
      // onClose fires when the user switches away from Whiteboard
      // (picks Details / Sprite). Toggle the overlay off.
      onClose: toggleWhiteboard,
      // Save state — only meaningful while the overlay is mounted.
      // Compact pane state moved out of hostHooks: the shell owns
      // paneCompact now (bindable on AssetPlaylist below) so
      // there's a single source of truth across tools.
      saving: whiteboardSaving,
      saveError: whiteboardSaveError,
      onSave: whiteboardSession ? saveWhiteboard : undefined,
    },
  }}
  canvasOverlay={whiteboardOpen && whiteboardSession ? whiteboardCanvasSlot : undefined}
  titleSlot={postTitleSlot}
  onAddToCollection={openPickerForCurrent}
  onRecreatePreviews={isOwner ? recreatePreviews : undefined}
  onEditTags={isOwner ? editTags : undefined}
  onEditMetadata={isOwner ? editMetadata : undefined}
  onDownloadVariant={downloadVariant}
  onShareAsset={shareAsset}
  onDeleteAsset={isOwner ? deleteAsset : undefined}
/>

<!-- Canvas overlay snippet — rendered INSIDE AssetViewer's canvas
     area via the canvasOverlay slot. Covers only the asset, leaves
     the sidebar + top toolbar reachable. The tool panel lives in
     the playlist's right pane via metadataSlot swap (the
     whiteboardToolsPane snippet below). -->
{#snippet whiteboardCanvasSlot()}
  {#if whiteboardSession}
    <WhiteboardCanvas
      session={whiteboardSession}
      onClose={toggleWhiteboard}
    />
  {/if}
{/snippet}

<!-- whiteboardToolsPane + whiteboardHotkeys snippets retired:
     - The tool body lives in WhiteboardTool/Body.svelte, which
       AssetViewer mounts through the registry. The session +
       save/close/compact hooks reach it via hostHooks.whiteboard.
     - The hotkey list lives in WhiteboardTool/Tips.svelte and
       renders into the global TipsSection footer the shell owns.
     PostHost only keeps the canvas overlay snippet above, which
     still threads through AssetViewer's canvasOverlay slot. -->


{#if previewedWhiteboard && previewedWhiteboard.annotation_data && previewSession}
  <!-- Read-only preview of a saved whiteboard. Lightweight modal
       wrapping a non-interactive BrushCanvas — no toolbar, just the
       sketch + a Close button. Clicking the backdrop closes. -->
  <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
  <div
    class="fixed inset-0 z-[60] flex items-center justify-center bg-black/80 backdrop-blur-sm"
    onclick={(e) => { if (e.target === e.currentTarget) previewedWhiteboard = null; }}
    role="dialog"
    aria-label={t('whiteboard.preview_dialog_label')}
  >
    <div class="relative max-h-[90vh] max-w-[90vw] rounded-lg border border-border bg-surface shadow-2xl">
      <header class="flex items-center justify-between border-b border-border px-3 py-2 text-xs">
        <span class="truncate text-fg">{previewedWhiteboard.body || t('whiteboard.untitled_sketch')}</span>
        <button
          type="button"
          onclick={() => (previewedWhiteboard = null)}
          class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-danger hover:text-white"
          title={t('whiteboard.close_esc_title')}
          aria-label={t('whiteboard.close_preview')}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
        </button>
      </header>
      <div
        class="relative bg-white"
        style:width="80vw"
        style:height="calc(80vw * {previewedWhiteboard.annotation_data.source_h / previewedWhiteboard.annotation_data.source_w})"
        style:max-height="80vh"
      >
        <BrushCanvas session={previewSession} readOnly />
      </div>
    </div>
  </div>
{/if}

{#if pickerOpen}
  <CollectionPicker
    assetIds={pickerAssetIds}
    onClose={closePicker}
  />
{/if}

<!-- Centered title for the viewer's title bar — "<post title> — by
     <author>". Both pieces truncate independently so a long title on
     a narrow viewport still leaves room for the author. The author
     link target follows the same /users/by-username/ route the
     sidebar header uses. -->
{#snippet postTitleSlot()}
  {#if post}
    <span class="truncate text-sm font-medium text-white/90" title={post.title || t('viewer_menu.untitled')}>
      {post.title || t('viewer_menu.untitled')}
    </span>
    {#if whiteboardOpen}
      <!-- Whiteboard mode replaces the asset subtitle with a static
           "Whiteboard" so the title bar reads "<post> / Whiteboard"
           and the user knows the right pane is the canvas tool, not
           an asset viewer. -->
      <span class="mx-1.5 text-white/30">/</span>
      <span class="truncate text-xs text-white/70">{t('whiteboard.title_slot_label')}</span>
    {:else if showAssetSubtitle}
      <!-- Current asset's title — changes as the cursor moves between
           assets within the post (↑ / ↓). Slash separator instead of
           middle-dot to read as "post / asset" hierarchy. -->
      <span class="mx-1.5 text-white/30">/</span>
      <span class="truncate text-xs text-white/70" title={currentAssetTitle}>
        {currentAssetTitle}
      </span>
    {/if}
    {#if author}
      <span class="mx-1.5 text-white/40">·</span>
      <a
        href="/users/by-username/{author.username}"
        class="truncate text-xs text-white/60 hover:text-white/90"
        title={author.display_name}
      >
        {author.display_name}
      </a>
    {/if}
  {/if}
{/snippet}


<!-- Post-specific sidebar content. AssetPlaylist threads this through
     into AssetViewer's metadataSlot prop. Closure-captures post +
     author + currentFields so it stays reactive as the cursor moves
     through the playlist. -->
{#snippet postSocialPane()}
  {#if post}
    <!-- Sidebar header — avatar + author + 3-dot menu. The menu
         carries PLAYLIST-LEVEL actions (operate on every asset in the
         post, or on the post itself). Per-asset actions live in the
         top toolbar's File/Edit menus where they belong; keeping the
         two surfaces separated avoids accidentally bulk-editing
         when the user meant to touch just one asset. Owner-gated
         items render only when the viewer is the post owner — non-
         owners see a shorter menu of read-only actions. -->
    <header class="flex items-start gap-3 border-b border-border p-4">
      <div class="flex h-12 w-12 shrink-0 items-center justify-center rounded-full bg-accent/20 text-sm font-semibold text-accent">
        {#if author?.avatar_url}
          <img src={author.avatar_url} alt="" class="h-full w-full rounded-full object-cover" />
        {:else if author}
          {initials(author.display_name)}
        {:else}
          ?
        {/if}
      </div>
      <div class="min-w-0 flex-1">
        <a
          href="/users/by-username/{author?.username ?? ''}"
          class="block truncate text-sm font-semibold text-fg hover:underline"
          title={author?.display_name}
        >
          {author?.display_name ?? `user ${post.author_user_ref}`}
        </a>
        <p class="truncate text-xs text-fg-muted">
          {#if author}@{author.username} · {t('user_meta.post_count', { n: author.post_count })}{/if}
        </p>
        {#if post.author_user_ref}
          <!-- FollowButton self-hides when the viewer IS the author,
               so this surface stays clean for the post-owner case
               without a parent-side conditional. -->
          <div class="mt-1.5">
            <FollowButton targetRef={post.author_user_ref} compact />
          </div>
        {/if}
      </div>
      <Menu align="right" panelClass="min-w-[12rem]">
        {#snippet trigger({ open })}
          <span
            class="inline-flex h-8 w-8 items-center justify-center rounded-full text-fg-muted hover:bg-surface-elevated hover:text-fg"
            class:bg-surface-elevated={open}
            aria-label={t('post_menu.actions_button')}
            title={t('post_menu.actions_button')}
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <circle cx="12" cy="5" r="1.5" />
              <circle cx="12" cy="12" r="1.5" />
              <circle cx="12" cy="19" r="1.5" />
            </svg>
          </span>
        {/snippet}
        {#if pl.source.items.length > 0}
          <button type="button" role="menuitem" onclick={openPickerForAll} class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated">
            {t('playlist_actions.add_all_to_collection')}
          </button>
          <button type="button" role="menuitem" onclick={bulkDownloadZip} class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated">
            {t('playlist_actions.download_all_zip')}
          </button>
          <button type="button" role="menuitem" onclick={sharePlaylist} class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated">
            {t('playlist_actions.share_playlist')}
          </button>
          {#if isOwner}
            <div class="my-1 h-px bg-border"></div>
            <button type="button" role="menuitem" onclick={bulkTag} class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated">
              {t('playlist_actions.bulk_tag')}
            </button>
            <button type="button" role="menuitem" onclick={editPost} class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated">
              {t('post_menu.edit_post')}
            </button>
            <button type="button" role="menuitem" onclick={deletePost} class="block w-full px-3 py-1.5 text-left text-sm text-danger hover:bg-danger-container">
              {t('post_menu.delete_post')}
            </button>
          {/if}
        {/if}
      </Menu>
    </header>

    <div class="p-4 text-sm">
      <!-- Post title retired from the body — the panel header now
           reads "{post title} Details" so duplicating it here was
           noise. Visibility chips + description stay (they're
           post-level, not redundant with the header). -->

      <div class="mb-3 flex flex-wrap gap-1.5">
        <span class="inline-flex items-center rounded-full bg-surface-elevated px-2 py-0.5 text-xs text-fg-muted">
          {post.visibility}
        </span>
        {#if post.team_id}
          <span class="inline-flex items-center rounded-full bg-accent/20 px-2 py-0.5 text-xs text-accent">
            {t('post_badges.team')}
          </span>
        {/if}
      </div>

      {#if post.description}
        <p class="mb-4 whitespace-pre-wrap text-fg-muted">{post.description}</p>
      {/if}

      {#if showAssetSubtitle}
        <!-- Per-asset subhead — updates as ↑/↓ moves between assets
             within the post. Grouped under an "Asset Details"
             section header so the user sees this block is about the
             specific asset they're focused on (versus the post as a
             whole, which the header + chips above represent). -->
        <section class="mb-4 border-t border-border pt-3">
          <h3 class="mb-1 text-xs font-medium uppercase tracking-wide text-fg-muted">{t('post_host.asset_details_heading')}</h3>
          <p class="text-sm text-fg" title={currentAssetTitle}>
            {#if pl.source.items.length > 1}
              <span class="font-mono text-xs text-fg-muted/70">{pl.source.cursor + 1}/{pl.source.items.length}</span>
              <span class="mx-1.5 text-fg-muted/40">·</span>
            {/if}
            <span class="break-words">{currentAssetTitle}</span>
          </p>
        </section>
      {/if}

      {#if whiteboards.length > 0 || whiteboardLoading}
        <!-- Whiteboards section — list of saved brush sketches on
             this post. Collapsed by default so the sidebar leads
             with the social context. Click a row → mounts the
             read-only preview overlay. Owners get a small delete
             icon next to each row. -->
        <details class="mb-4 border-t border-border pt-3 text-xs aa-collapse">
          <summary class="cursor-pointer list-none text-xs font-medium uppercase tracking-wide text-fg-muted hover:text-fg">
            <span class="inline-flex items-center gap-1">
              <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" class="aa-chevron transition-transform">
                <polyline points="9 18 15 12 9 6" />
              </svg>
              {t('post_host.whiteboards_heading')}
              <span class="text-fg-muted/60">({whiteboards.length})</span>
            </span>
          </summary>
          <ul class="mt-3 space-y-1.5">
            {#each whiteboards as wb (wb.id)}
              <li class="group flex items-center gap-2 rounded p-1.5 hover:bg-surface-elevated">
                <button
                  type="button"
                  onclick={() => (previewedWhiteboard = previewedWhiteboard?.id === wb.id ? null : wb)}
                  class="flex flex-1 items-center gap-2 text-left"
                  title={t('whiteboard.preview_button_title')}
                >
                  <div class="flex h-10 w-14 shrink-0 items-center justify-center rounded border border-border bg-surface text-fg-muted">
                    <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                      <rect x="2" y="3" width="20" height="14" rx="2" />
                      <line x1="8" y1="21" x2="16" y2="21" />
                      <line x1="12" y1="17" x2="12" y2="21" />
                    </svg>
                  </div>
                  <div class="min-w-0 flex-1">
                    <div class="truncate text-xs font-medium text-fg">
                      {wb.body || t('whiteboard.untitled_sketch')}
                    </div>
                    <div class="text-[10px] text-fg-muted">
                      {relativeTime(wb.created_at)}
                    </div>
                  </div>
                </button>
                {#if auth.user?.ref === wb.author_user_ref || isOwner}
                  <button
                    type="button"
                    onclick={() => deleteWhiteboard(wb.id)}
                    class="opacity-0 group-hover:opacity-100 hover:text-danger"
                    title={t('whiteboard.delete_button')}
                    aria-label={t('whiteboard.delete_button')}
                  >
                    <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                      <polyline points="3 6 5 6 21 6" />
                      <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
                    </svg>
                  </button>
                {/if}
              </li>
            {/each}
            {#if whiteboards.length === 0 && !whiteboardLoading}
              <li class="px-1 py-1 text-[11px] italic text-fg-muted">
                {t('post_host.whiteboards_empty')}
              </li>
            {/if}
          </ul>
        </details>
      {/if}

      {#if currentFields.length > 0}
        <!-- Per-asset metadata — collapsed by default so the sidebar
             leads with the social context. Users who want the full
             field dump click to expand; their open/closed choice
             persists per-browser via <details>'s name attribute is
             scoped by Svelte's reactivity, so the OS keeps it the
             way they left it across reloads of the same key. -->
        <details class="mb-4 border-t border-border pt-3 text-xs aa-collapse">
          <summary class="cursor-pointer list-none text-xs font-medium uppercase tracking-wide text-fg-muted hover:text-fg">
            <span class="inline-flex items-center gap-1">
              <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" class="aa-chevron transition-transform">
                <polyline points="9 18 15 12 9 6" />
              </svg>
              {t('post_host.metadata_heading')}
              <span class="text-fg-muted/60">({currentFields.filter((f) => formatFieldValue(f) !== '').length})</span>
            </span>
          </summary>
          <dl class="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
            {#each currentFields as f (f.field_id)}
              {@const val = formatFieldValue(f)}
              {#if val}
                <dt class="truncate text-fg-muted" title={f.field_label || f.field_code}>
                  {f.field_label || f.field_code}
                </dt>
                <dd class="min-w-0 break-words text-fg" class:whitespace-pre-wrap={f.type === 'longtext' || f.type === 'rich_text'}>
                  {val}
                </dd>
              {/if}
            {/each}
          </dl>
        </details>
      {/if}

      {#if post.tags.length > 0}
        <div class="mb-4 flex flex-wrap gap-1.5">
          {#each post.tags as tag}
            <button
              type="button"
              onclick={() => gotoTag(tag)}
              class="rounded-full border border-border bg-surface-elevated px-2 py-0.5 text-xs text-fg-muted transition-colors hover:border-fg-muted/60 hover:text-fg"
            >
              #{tag}
            </button>
          {/each}
        </div>
      {/if}

      <div class="mt-4 flex items-center gap-3 border-t border-border pt-4">
        <button
          type="button"
          onclick={toggleLike}
          disabled={likeBusy}
          class="inline-flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-sm transition-colors disabled:opacity-50"
          class:text-fg={!liked}
          class:text-danger={liked}
          class:border-danger={liked}
          class:bg-danger-container={liked}
          aria-pressed={liked}
          title={liked ? t('post_host.unlike_button') : t('post_host.like_button')}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill={liked ? 'currentColor' : 'none'} stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z" />
          </svg>
          {post.like_count}
        </button>
        <span class="inline-flex items-center gap-1 text-sm text-fg-muted">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
          </svg>
          {post.comment_count}
        </span>
        <span class="ml-auto text-xs text-fg-muted" title={postedAbsolute}>{postedRelative}</span>
      </div>

      <div class="mt-6">
        <CommentsThread postId={post.id} />
      </div>
    </div>
  {/if}
{/snippet}

<style>
  /* Collapse-section chevron — points right when closed, rotates 90°
     down when the parent <details> is open. Plain CSS rather than a
     Tailwind arbitrary-variant because Tailwind v3 has no built-in
     `open:` modifier for native <details>. */
  details.aa-collapse[open] > summary .aa-chevron {
    transform: rotate(90deg);
  }
  details.aa-collapse > summary::-webkit-details-marker {
    display: none;
  }
</style>
