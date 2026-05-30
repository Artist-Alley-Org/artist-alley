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
  import { createPostPlaylistSource } from '$lib/playlist/postSource.svelte';
  import { t } from '$stores/lang.svelte';

  interface Props {
    postId: string;
    onClose: () => void;
    standalone?: boolean;
  }

  let { postId, onClose, standalone = false }: Props = $props();

  // ── Source ────────────────────────────────────────────────────────
  // The PostPlaylistSource owns the post fetch + member list. The
  // shell binds to `pl.source`; we read `pl.aux.post` to render the
  // social skin.
  //
  // Re-create when postId changes — currently rare (nothing links
  // to another post from inside the modal) but keeps the right
  // semantics if a future "next post" affordance lands.
  let pl = $state(createPostPlaylistSource(postId));
  $effect(() => {
    const id = postId;
    pl = createPostPlaylistSource(id);
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

  const post = $derived(pl.aux.post);
  const currentItem = $derived(pl.source.items[pl.source.cursor] ?? null);
  const currentAssetId = $derived(currentItem?.id ?? null);
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
  // Stubbed for now — the backend re-enqueue endpoint lands in the
  // next commit. Wiring the callback here so the Edit-menu item
  // surfaces enabled; the no-op + console-info is a temporary
  // placeholder.
  function recreatePreviews(assetId: string) {
    // TODO: POST to /assets/{id}/recreate-previews (next commit).
    console.info('recreatePreviews stub', assetId);
  }
</script>

<AssetPlaylist
  source={pl.source}
  {onClose}
  {standalone}
  contextSlot={postSocialPane}
  toolbarActions={postBulkActions}
  onAddToCollection={openPickerForCurrent}
  onRecreatePreviews={recreatePreviews}
/>

{#if pickerOpen}
  <CollectionPicker
    assetIds={pickerAssetIds}
    onClose={closePicker}
  />
{/if}

<!-- Per-playlist bulk actions threaded into AssetPlaylist's top
     toolbar via the toolbarActions snippet slot. Currently just
     "Add all to collection"; downstream commits add Download-as-ZIP,
     Bulk-tag, Save-as-review, etc. -->
{#snippet postBulkActions()}
  {#if pl.source.items.length > 0}
    <button
      type="button"
      onclick={openPickerForAll}
      class="inline-flex items-center gap-1.5 rounded-md bg-black/60 px-3 py-1.5 text-xs text-white backdrop-blur-sm transition-colors hover:bg-black/80"
      title={t('playlist_actions.add_all_to_collection')}
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
        <polyline points="17 8 12 3 7 8" />
        <line x1="12" y1="3" x2="12" y2="15" />
      </svg>
      {t('playlist_actions.add_all_to_collection')}
    </button>
  {/if}
{/snippet}

<!-- Post-specific sidebar content. AssetPlaylist threads this through
     into AssetViewer's metadataSlot prop. Closure-captures post +
     author + currentFields so it stays reactive as the cursor moves
     through the playlist. -->
{#snippet postSocialPane()}
  {#if post}
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
          {#if author}@{author.username} · {author.post_count} post{author.post_count === 1 ? '' : 's'}{/if}
        </p>
      </div>
    </header>

    <div class="p-4 text-sm">
      {#if post.title}
        <h1 id="asset-playlist-title" class="mb-2 text-lg font-semibold text-fg">
          {post.title}
        </h1>
      {/if}

      <div class="mb-3 flex flex-wrap gap-1.5">
        <span class="inline-flex items-center rounded-full bg-surface-elevated px-2 py-0.5 text-xs text-fg-muted">
          {post.visibility}
        </span>
        {#if post.team_id}
          <span class="inline-flex items-center rounded-full bg-accent/20 px-2 py-0.5 text-xs text-accent">
            team
          </span>
        {/if}
      </div>

      {#if post.description}
        <p class="mb-4 whitespace-pre-wrap text-fg-muted">{post.description}</p>
      {/if}

      {#if currentFields.length > 0}
        <dl class="mb-4 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 border-t border-border pt-4 text-xs">
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
          title={liked ? 'Unlike' : 'Like'}
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

      {#if isOwner}
        <div class="mt-4 flex gap-2 border-t border-border pt-4">
          <button
            type="button"
            class="rounded-md border border-border px-3 py-1.5 text-xs text-fg-muted transition-colors hover:border-fg-muted/60 hover:text-fg"
            disabled
            title="Editing lands in a later phase"
          >
            Edit
          </button>
          <button
            type="button"
            class="rounded-md border border-danger/40 px-3 py-1.5 text-xs text-danger transition-colors hover:bg-danger-container"
            disabled
            title="Deletion lands in a later phase"
          >
            Delete
          </button>
        </div>
      {/if}

      <div class="mt-6">
        <CommentsThread postId={post.id} />
      </div>
    </div>
  {/if}
{/snippet}
