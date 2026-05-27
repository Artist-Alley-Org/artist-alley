<script lang="ts">
  // Post detail modal — used both as an overlay over the browse feed
  // (?post={id}) and as the standalone /posts/[id] page. ArtStation-
  // style layout: large preview pane (left, ~70% on desktop) plus a
  // collapsible sidebar (right) with author / metadata / actions.
  // When a post has multiple member assets, a bottom strip renders the
  // members as thumbnails; clicking one swaps the preview in-place
  // without a URL change.
  //
  // F-1 shipped the modal shell (dialog, ESC close, dual URL). F-2
  // (this file) lands the real contents. F-3 wires like + comments
  // (the read-only counts here become active buttons + a thread).
  //
  // Borrowing patterns from RS/BARTS view.php research:
  //   - Sidebar lives as a sibling of the preview (not buried inside
  //     the preview wrapper like BARTS does). Cleaner layout, no
  //     CSS `:has()` scoping required.
  //   - Member-strip thumbnails carry no per-hover AJAX — all the data
  //     needed for the preview swap is in the post.members payload.
  //   - Clicking a tag goes to /?tag=foo (real search semantics, not
  //     RS's free-text `?search=<value>`).
  //   - Sidebar collapsed state persists per-browser via localStorage.
  //   - Sidebar toggle hotkey is `i` (matches BARTS keybindings —
  //     the one good idiom worth keeping).

  import { onMount, onDestroy } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';

  interface Props {
    postId: string;
    onClose: () => void;
    standalone?: boolean;
  }

  let { postId, onClose, standalone = false }: Props = $props();

  // ---- Types --------------------------------------------------------------
  // Mirror the API shape locally — openapi-typescript types are
  // available via the `api` client but typing the full discriminated
  // union for components is more noise than value.

  interface AssetSummary {
    id: string;
    title?: string;
    file_hash?: string | null;
    file_extension?: string | null;
  }
  interface PostMember {
    asset_id: string;
    sort_order: number;
    asset: AssetSummary;
  }
  interface Post {
    id: string;
    author_user_ref: number;
    title: string;
    description: string;
    visibility: 'private' | 'followers' | 'public';
    cover_asset_id?: string | null;
    posted_at: string;
    like_count: number;
    comment_count: number;
    tags: string[];
    members: PostMember[];
    team_id?: string | null;
    created_at: string;
    updated_at: string;
  }
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

  // ---- State --------------------------------------------------------------

  let dialogEl: HTMLDialogElement | undefined = $state();

  let post = $state<Post | null>(null);
  let author = $state<UserPublic | null>(null);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Which member is the preview showing. Defaults to the cover, or
  // the first member if no cover is pinned.
  let selectedIdx = $state(0);

  // Sidebar collapsed state. Persists per-browser. `null` until the
  // mount effect resolves it — until then we render expanded.
  let sidebarCollapsed = $state(false);

  // ---- Derived ------------------------------------------------------------

  const currentMember = $derived(post?.members?.[selectedIdx] ?? null);
  const currentAssetId = $derived(currentMember?.asset_id ?? null);
  const previewColUrl = $derived(
    currentAssetId ? `/api/v1/assets/${currentAssetId}/variants/col` : '',
  );
  const previewFileUrl = $derived(
    currentAssetId ? `/api/v1/assets/${currentAssetId}/file` : '',
  );
  const hasMultipleMembers = $derived((post?.members?.length ?? 0) > 1);

  const isOwner = $derived(
    !!auth.user && !!post && auth.user.ref === post.author_user_ref,
  );

  const postedRelative = $derived(post ? relativeTime(post.posted_at) : '');
  const postedAbsolute = $derived(post ? new Date(post.posted_at).toLocaleString() : '');

  // ---- Lifecycle ----------------------------------------------------------

  onMount(() => {
    dialogEl?.showModal();
    document.body.classList.add('overflow-hidden');
    // Restore sidebar collapsed state.
    const saved = localStorage.getItem('postModal.sidebarCollapsed');
    if (saved === '1') sidebarCollapsed = true;
  });

  onDestroy(() => {
    document.body.classList.remove('overflow-hidden');
  });

  // Fetch post when postId changes (covers the standalone-page param
  // case AND the rare case of clicking another post while the modal
  // is open — though today nothing links to another post from inside
  // the modal).
  $effect(() => {
    const id = postId;
    void loadPost(id);
  });

  async function loadPost(id: string) {
    loading = true;
    error = null;
    post = null;
    author = null;
    selectedIdx = 0;
    try {
      const { data, error: apiErr } = await api.GET('/posts/{id}', {
        params: { path: { id } },
      });
      if (apiErr || !data) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? 'Failed to load post',
        );
      }
      post = data as Post;
      // Default the preview to the cover if there is one, else the
      // first member by sort_order (the API already returns members
      // sorted).
      if (post.cover_asset_id) {
        const idx = post.members.findIndex(
          (m) => m.asset_id === post!.cover_asset_id,
        );
        if (idx >= 0) selectedIdx = idx;
      }
      // Author lookup — independent fetch. Surface failure as just
      // a placeholder header; the post itself is the main payload.
      void loadAuthor(post.author_user_ref);
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to load';
    } finally {
      loading = false;
    }
  }

  async function loadAuthor(ref: number) {
    try {
      const { data } = await api.GET('/users/{ref}', {
        params: { path: { ref } },
      });
      if (data) author = data as UserPublic;
    } catch {
      // Soft failure — author header renders a placeholder.
    }
  }

  // ---- Handlers -----------------------------------------------------------

  function handleDialogClose() {
    if (dialogEl?.open === false) onClose();
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === dialogEl) onClose();
  }

  function handleClose() {
    dialogEl?.close();
    onClose();
  }

  function toggleSidebar() {
    sidebarCollapsed = !sidebarCollapsed;
    localStorage.setItem(
      'postModal.sidebarCollapsed',
      sidebarCollapsed ? '1' : '0',
    );
  }

  function selectMember(idx: number) {
    if (!post) return;
    const n = post.members.length;
    if (n === 0) return;
    selectedIdx = ((idx % n) + n) % n; // wrap, handles negative
  }

  function handleKeydown(e: KeyboardEvent) {
    // Ignore key handling while focus is in a text input — comment
    // composer (F-3) will live inside the modal.
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
      return;
    }
    switch (e.key) {
      case 'ArrowLeft':
        if (hasMultipleMembers) {
          e.preventDefault();
          selectMember(selectedIdx - 1);
        }
        break;
      case 'ArrowRight':
        if (hasMultipleMembers) {
          e.preventDefault();
          selectMember(selectedIdx + 1);
        }
        break;
      case 'i':
      case 'I':
        e.preventDefault();
        toggleSidebar();
        break;
    }
  }

  // Image fallback: col variant first, then full file, then placeholder.
  let imgError = $state(false);
  let triedFallback = $state(false);
  $effect(() => {
    // Reset error state whenever the selected member changes.
    void currentAssetId;
    imgError = false;
    triedFallback = false;
  });
  function handleImgError(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    if (!triedFallback && previewFileUrl) {
      triedFallback = true;
      img.src = previewFileUrl;
      return;
    }
    imgError = true;
  }

  function colVariantUrl(assetId: string): string {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  // Tag click → /?tag=foo (real search semantics; ?tag is already a
  // server-side filter on /posts).
  async function gotoTag(tag: string) {
    const target = new URL(page.url);
    target.pathname = '/';
    target.searchParams.delete('post');
    target.searchParams.set('tag', tag);
    await goto(target.pathname + target.search);
  }

  // ---- Formatters ---------------------------------------------------------

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
</script>

<svelte:window onkeydown={handleKeydown} />

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<dialog
  bind:this={dialogEl}
  onclose={handleDialogClose}
  onclick={handleBackdropClick}
  class="post-modal m-0 max-h-none max-w-none w-full h-full bg-transparent p-0 backdrop:bg-black/80 backdrop:backdrop-blur-sm"
  aria-labelledby="post-modal-title"
>
  <div
    class="relative mx-auto flex h-full w-full max-w-screen-2xl flex-col overflow-hidden bg-surface text-fg shadow-2xl sm:my-4 sm:h-[calc(100vh-2rem)] sm:rounded-lg"
    role="presentation"
  >
    <!-- Close / back button — pinned top-right of the modal frame. -->
    <button
      type="button"
      onclick={handleClose}
      class="absolute right-4 top-4 z-30 inline-flex h-9 w-9 items-center justify-center rounded-full bg-black/60 text-white backdrop-blur-sm transition-colors hover:bg-black/80"
      aria-label={standalone ? 'Back' : 'Close'}
    >
      {#if standalone}
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="m15 18-6-6 6-6" />
        </svg>
      {:else}
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="M18 6 6 18M6 6l12 12" />
        </svg>
      {/if}
    </button>

    {#if loading}
      <!-- Skeleton: preview area + sidebar shimmer. -->
      <div class="flex flex-1 flex-col md:flex-row">
        <div class="flex-1 animate-pulse bg-black/30"></div>
        <aside class="md:w-96 border-l border-border bg-surface-elevated">
          <div class="space-y-4 p-6">
            <div class="h-12 w-12 animate-pulse rounded-full bg-border"></div>
            <div class="h-6 w-3/4 animate-pulse rounded bg-border"></div>
            <div class="h-4 w-1/2 animate-pulse rounded bg-border"></div>
            <div class="space-y-2 pt-4">
              <div class="h-3 w-full animate-pulse rounded bg-border"></div>
              <div class="h-3 w-5/6 animate-pulse rounded bg-border"></div>
              <div class="h-3 w-2/3 animate-pulse rounded bg-border"></div>
            </div>
          </div>
        </aside>
      </div>
    {:else if error}
      <div class="flex flex-1 items-center justify-center p-8">
        <div role="alert" class="max-w-md rounded-md border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-600 dark:text-red-300">
          {error}
        </div>
      </div>
    {:else if post}
      <div class="flex flex-1 flex-col overflow-hidden md:flex-row">
        <!-- Preview pane (left). Always dark backdrop — galleries look
             best on dark regardless of the surrounding theme. -->
        <div class="relative flex flex-1 items-center justify-center overflow-hidden bg-neutral-950">
          {#if currentAssetId && !imgError}
            <img
              src={previewColUrl}
              alt={currentMember?.asset?.title || post.title}
              class="max-h-full max-w-full object-contain"
              onerror={handleImgError}
            />
          {:else}
            <div class="text-neutral-500">
              <svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" stroke-linecap="round" stroke-linejoin="round">
                <rect x="3" y="3" width="18" height="18" rx="2" />
                <circle cx="9" cy="9" r="2" />
                <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
              </svg>
            </div>
          {/if}

          <!-- Member nav arrows: visible only when there's >1 member. -->
          {#if hasMultipleMembers}
            <button
              type="button"
              onclick={() => selectMember(selectedIdx - 1)}
              class="absolute left-3 top-1/2 z-10 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur-sm transition-colors hover:bg-black/75"
              aria-label="Previous asset"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                <path d="m15 18-6-6 6-6" />
              </svg>
            </button>
            <button
              type="button"
              onclick={() => selectMember(selectedIdx + 1)}
              class="absolute right-3 top-1/2 z-10 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur-sm transition-colors hover:bg-black/75"
              aria-label="Next asset"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                <path d="m9 18 6-6-6-6" />
              </svg>
            </button>
            <!-- Position indicator (n / total) — bottom-center. -->
            <div class="pointer-events-none absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full bg-black/60 px-3 py-1 text-xs font-medium text-white backdrop-blur-sm">
              {selectedIdx + 1} / {post.members.length}
            </div>
          {/if}

          <!-- Sidebar collapsed: show a re-open chevron tab in the
               preview's right edge so the user can get it back. -->
          {#if sidebarCollapsed}
            <button
              type="button"
              onclick={toggleSidebar}
              class="absolute right-0 top-1/2 z-10 -translate-y-1/2 rounded-l-md bg-black/60 px-2 py-3 text-white backdrop-blur-sm transition-colors hover:bg-black/80"
              aria-label="Show details"
              title="Show details (i)"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="m15 18-6-6 6-6" />
              </svg>
            </button>
          {/if}
        </div>

        <!-- Sidebar (right). Collapsible. -->
        {#if !sidebarCollapsed}
          <aside class="flex flex-col border-t border-border bg-surface md:w-96 md:border-l md:border-t-0">
            <!-- Sticky author header. -->
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
              <button
                type="button"
                onclick={toggleSidebar}
                class="ml-auto inline-flex h-7 w-7 shrink-0 items-center justify-center rounded text-fg-muted hover:bg-surface-elevated hover:text-fg"
                aria-label="Hide details"
                title="Hide details (i)"
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <path d="m9 18 6-6-6-6" />
                </svg>
              </button>
            </header>

            <!-- Scrollable body. -->
            <div class="flex-1 overflow-y-auto p-4 text-sm">
              {#if post.title}
                <h1 id="post-modal-title" class="mb-2 text-lg font-semibold text-fg">
                  {post.title}
                </h1>
              {/if}

              <!-- Visibility + team chips. -->
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

              <!-- Engagement counts (read-only this phase; F-3 wires
                   the like + comment buttons). -->
              <div class="mt-4 flex items-center gap-4 border-t border-border pt-4 text-xs text-fg-muted">
                <span class="inline-flex items-center gap-1">
                  <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z" />
                  </svg>
                  {post.like_count}
                </span>
                <span class="inline-flex items-center gap-1">
                  <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
                  </svg>
                  {post.comment_count}
                </span>
                <span class="ml-auto" title={postedAbsolute}>{postedRelative}</span>
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
                    class="rounded-md border border-red-500/40 px-3 py-1.5 text-xs text-red-500 transition-colors hover:bg-red-500/10"
                    disabled
                    title="Deletion lands in a later phase"
                  >
                    Delete
                  </button>
                </div>
              {/if}

              <!-- F-3 placeholder for the comment thread. -->
              <div class="mt-6 rounded-md border border-dashed border-border p-4 text-center text-xs text-fg-muted">
                Comments thread + like button arrive in F-3.
              </div>
            </div>
          </aside>
        {/if}
      </div>

      <!-- Bottom member strip — only for posts with >1 member. -->
      {#if hasMultipleMembers}
        <div class="border-t border-border bg-surface-elevated px-2 py-2">
          <div class="flex gap-2 overflow-x-auto">
            {#each post.members as member, i (member.asset_id)}
              <button
                type="button"
                onclick={() => selectMember(i)}
                class="relative h-16 w-16 shrink-0 overflow-hidden rounded border-2 transition-all"
                class:border-accent={i === selectedIdx}
                class:opacity-100={i === selectedIdx}
                class:border-transparent={i !== selectedIdx}
                class:opacity-50={i !== selectedIdx}
                class:hover:opacity-100={i !== selectedIdx}
                aria-label="Show asset {i + 1}"
                aria-current={i === selectedIdx ? 'true' : undefined}
              >
                {#if member.asset?.file_hash}
                  <img
                    src={colVariantUrl(member.asset_id)}
                    alt=""
                    loading="lazy"
                    class="h-full w-full object-cover"
                  />
                {:else}
                  <div class="flex h-full w-full items-center justify-center bg-surface text-fg-muted/40">
                    <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
                      <rect x="3" y="3" width="18" height="18" rx="2" />
                    </svg>
                  </div>
                {/if}
              </button>
            {/each}
          </div>
        </div>
      {/if}
    {/if}
  </div>
</dialog>

<style>
  dialog.post-modal {
    border: none;
    inset: 0;
  }
  dialog.post-modal:not([open]) {
    display: none;
  }
</style>
