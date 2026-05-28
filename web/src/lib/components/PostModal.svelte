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
  import CommentsThread from './CommentsThread.svelte';

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

  // ---- State --------------------------------------------------------------

  let dialogEl: HTMLDialogElement | undefined = $state();

  let post = $state<Post | null>(null);
  let author = $state<UserPublic | null>(null);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Like state for the current viewer. Loaded on post fetch, toggled
  // optimistically on click, reverted on error.
  let liked = $state(false);
  let likeBusy = $state(false);

  // Per-member asset metadata. Refetched whenever the selected member
  // changes (the user navigates between assets in a multi-asset post).
  // Keyed by asset_id so going back to a previously-viewed member
  // serves from local cache without a re-fetch.
  let fieldsByAsset = $state<Map<string, AssetFieldValue[]>>(new Map());
  let fieldsLoading = $state(false);

  // Which member is the preview showing. Defaults to the cover, or
  // the first member if no cover is pinned. For multi-asset posts
  // this is kept in sync with which slide is currently centered in
  // the vertical scroller (see the IntersectionObserver effect).
  let selectedIdx = $state(0);

  // The vertical scroller that hosts the per-member slides. One slide
  // per member, full-height, scroll-snap-aligned. Bound from the
  // template; populated only when post.members.length > 1.
  let scrollerEl = $state<HTMLDivElement | undefined>(undefined);

  // While a programmatic scroll (e.g. arrow nav, thumb-strip click,
  // or initial cover-asset snap) is in flight, suppress the
  // observer-driven selectedIdx update so we don't fight ourselves.
  let suppressScrollSync = $state(false);

  // Per-slide image-error tracking. When the col variant 404s for an
  // asset we swap to /file (full-res) once; if that also fails we
  // mark the asset broken and render the placeholder icon instead.
  let brokenSlides = $state<Set<string>>(new Set());

  // Review mode — same modal, sidebar hidden, the preview pane
  // becomes a zoom/pan/tile canvas. Toggled by the dedicated review
  // button in the toolbar. Tile mode and the zoom/pan transform only
  // apply while reviewMode is true; exiting review resets them.
  let reviewMode = $state(false);
  let tileMode = $state(false);

  // Zoom/pan transform — only meaningful while reviewMode is true.
  // zoom is multiplicative; panX/panY are translate offsets relative
  // to the canvas center. drag* fields capture mousedown anchors so
  // movement is computed as delta-from-anchor.
  let zoom = $state(1);
  let panX = $state(0);
  let panY = $state(0);
  let dragging = $state(false);
  let dragStartX = 0;
  let dragStartY = 0;
  let panStartX = 0;
  let panStartY = 0;

  // Canvas ref + intrinsic dimensions of the loaded review image.
  // Tile-mode uses the natural pixel size so the repeating pattern
  // looks correct.
  let canvasEl = $state<HTMLDivElement | undefined>(undefined);
  let imgNaturalW = $state(0);
  let imgNaturalH = $state(0);

  // Sidebar collapsed state. Persists per-browser. `null` until the
  // mount effect resolves it — until then we render expanded.
  let sidebarCollapsed = $state(false);

  // Footer thumb strip collapsed state. Persists per-browser. The
  // collapsed state shows just a chevron + "n / total" so the
  // viewer always knows where they are even with the strip hidden.
  let stripCollapsed = $state(false);

  // ---- Derived ------------------------------------------------------------

  const currentMember = $derived(post?.members?.[selectedIdx] ?? null);
  const currentAssetId = $derived(currentMember?.asset_id ?? null);
  const hasMultipleMembers = $derived((post?.members?.length ?? 0) > 1);
  const currentIsImage = $derived(isImageExt(currentMember?.asset?.file_extension ?? null));

  const isOwner = $derived(
    !!auth.user && !!post && auth.user.ref === post.author_user_ref,
  );

  const postedRelative = $derived(post ? relativeTime(post.posted_at) : '');
  const postedAbsolute = $derived(post ? new Date(post.posted_at).toLocaleString() : '');

  // The current asset's metadata field values, ready to render.
  // Empty array while loading or when the asset has no fields.
  const currentFields = $derived<AssetFieldValue[]>(
    currentAssetId ? (fieldsByAsset.get(currentAssetId) ?? []) : [],
  );

  // ---- Lifecycle ----------------------------------------------------------

  onMount(() => {
    dialogEl?.showModal();
    document.body.classList.add('overflow-hidden');
    // Restore sidebar collapsed state.
    if (localStorage.getItem('postModal.sidebarCollapsed') === '1') sidebarCollapsed = true;
    if (localStorage.getItem('postModal.stripCollapsed') === '1') stripCollapsed = true;
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

  // Fetch field values for whichever member is currently selected.
  // Cached in fieldsByAsset; flipping back to a previously-viewed
  // member is instant.
  $effect(() => {
    const aid = currentAssetId;
    if (!aid) return;
    if (fieldsByAsset.has(aid)) return;
    void loadFields(aid);
  });

  async function loadFields(assetId: string) {
    fieldsLoading = true;
    try {
      const { data } = await api.GET('/assets/{id}/fields', {
        params: { path: { id: assetId } },
      });
      if (data) {
        fieldsByAsset.set(assetId, data as AssetFieldValue[]);
        // Reassign to trigger reactive update of the derived currentFields.
        fieldsByAsset = new Map(fieldsByAsset);
      }
    } catch {
      // Soft fail — sidebar just renders without the metadata block.
    } finally {
      fieldsLoading = false;
    }
  }

  async function loadPost(id: string) {
    loading = true;
    error = null;
    post = null;
    author = null;
    selectedIdx = 0;
    brokenSlides = new Set();
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
      // Like state for the current viewer. Initialized parallel to
      // the post fetch so the button shows the right state on open.
      void loadLikeState(id);
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to load';
    } finally {
      loading = false;
    }
  }

  async function loadLikeState(id: string) {
    try {
      const { data } = await api.GET('/posts/{id}/like', {
        params: { path: { id } },
      });
      if (data) liked = data.liked;
    } catch {
      // Soft-fail; default false is the safe stance.
      liked = false;
    }
  }

  async function toggleLike() {
    if (!post || likeBusy) return;
    likeBusy = true;
    // Optimistic toggle.
    const wasLiked = liked;
    liked = !wasLiked;
    post.like_count = Math.max(post.like_count + (wasLiked ? -1 : 1), 0);
    try {
      const path = { id: post.id };
      const { error: apiErr } = wasLiked
        ? await api.DELETE('/posts/{id}/like', { params: { path } })
        : await api.POST('/posts/{id}/like', { params: { path } });
      if (apiErr) {
        // Revert.
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

  // Jump to a member by index. For multi-asset posts this scrolls the
  // matching slide into view (which the IntersectionObserver picks up
  // and mirrors back into selectedIdx) — for single-asset posts it's
  // just a state change. Clamped, not wrapped: scroll-snap UX prefers
  // a hard stop at the edges over jumping back to the top/bottom.
  function goToMember(idx: number) {
    if (!post) return;
    const n = post.members.length;
    if (n === 0) return;
    const clamped = Math.max(0, Math.min(n - 1, idx));
    if (reviewMode) {
      // Review mode bypasses the scroller (there isn't one in this
      // mode); just swap the asset and reset the view transform so
      // the next image opens at 1:1, untransformed.
      selectedIdx = clamped;
      resetReviewView();
      const nextExt = post.members[clamped]?.asset?.file_extension ?? null;
      if (tileMode && !isImageExt(nextExt)) tileMode = false;
    } else if (hasMultipleMembers) {
      scrollToMember(clamped);
    } else {
      selectedIdx = clamped;
    }
  }

  // Programmatic scroll to a slide. Disables the observer briefly so
  // the smooth-scroll doesn't fight with the selectedIdx we just set.
  function scrollToMember(idx: number) {
    const el = scrollerEl;
    if (!el) {
      selectedIdx = idx;
      return;
    }
    const slide = el.querySelector<HTMLElement>(`[data-slide-idx="${idx}"]`);
    if (!slide) {
      selectedIdx = idx;
      return;
    }
    suppressScrollSync = true;
    selectedIdx = idx;
    slide.scrollIntoView({ behavior: 'smooth', block: 'start' });
    // Smooth-scroll has no completion event; 500ms is a generous
    // upper bound for the distances we ever scroll inside the modal.
    setTimeout(() => {
      suppressScrollSync = false;
    }, 500);
  }

  // Watch the vertical scroller and mirror whichever slide is
  // currently centered into `selectedIdx`. Re-runs when post.members
  // changes (a new post is loaded) so observers are wired against the
  // new slide nodes.
  $effect(() => {
    const el = scrollerEl;
    const memberCount = post?.members?.length ?? 0;
    if (!el || memberCount <= 1) return;
    const slides = el.querySelectorAll<HTMLElement>('[data-slide-idx]');
    if (slides.length === 0) return;
    const io = new IntersectionObserver(
      (entries) => {
        if (suppressScrollSync) return;
        // Pick the entry with the highest intersectionRatio that is
        // currently intersecting; that's the slide the user is
        // looking at.
        let best: IntersectionObserverEntry | null = null;
        for (const e of entries) {
          if (!e.isIntersecting) continue;
          if (!best || e.intersectionRatio > best.intersectionRatio) best = e;
        }
        if (!best) return;
        const idx = Number((best.target as HTMLElement).dataset.slideIdx);
        if (!Number.isNaN(idx) && idx !== selectedIdx) selectedIdx = idx;
      },
      { root: el, threshold: [0.5, 0.75, 1] },
    );
    for (const s of slides) io.observe(s);
    return () => io.disconnect();
  });

  // When a new post loads with a cover asset that isn't the first
  // member, snap the scroller to that slide on mount. One-shot per
  // post: the effect tracks post.id so it fires only on post change,
  // not on every selectedIdx update (which would chase the observer
  // and cause jitter).
  let lastSnappedPostId = $state<string | null>(null);
  $effect(() => {
    const el = scrollerEl;
    if (!el || !post) return;
    if (post.members.length <= 1) return;
    if (post.id === lastSnappedPostId) return;
    lastSnappedPostId = post.id;
    if (selectedIdx === 0) return;
    const slide = el.querySelector<HTMLElement>(`[data-slide-idx="${selectedIdx}"]`);
    if (slide) {
      suppressScrollSync = true;
      slide.scrollIntoView({ behavior: 'instant' as ScrollBehavior, block: 'start' });
      setTimeout(() => {
        suppressScrollSync = false;
      }, 0);
    }
  });

  function handleKeydown(e: KeyboardEvent) {
    // Ignore key handling while focus is in a text input — comment
    // composer (F-3) will live inside the modal.
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
      return;
    }
    switch (e.key) {
      case 'ArrowLeft':
      case 'ArrowUp':
        if (hasMultipleMembers) {
          e.preventDefault();
          goToMember(selectedIdx - 1);
        }
        break;
      case 'ArrowRight':
      case 'ArrowDown':
        if (hasMultipleMembers) {
          e.preventDefault();
          goToMember(selectedIdx + 1);
        }
        break;
      case 'i':
      case 'I':
        if (!reviewMode) {
          e.preventDefault();
          toggleSidebar();
        }
        break;
      case 't':
      case 'T':
        if (reviewMode && currentIsImage) {
          e.preventDefault();
          tileMode = !tileMode;
        }
        break;
      case 'r':
      case 'R':
        if (reviewMode) {
          e.preventDefault();
          resetReviewView();
        }
        break;
      case 'Escape':
        if (reviewMode) {
          e.preventDefault();
          exitReview();
        }
        // ESC outside review mode falls through to the dialog's
        // native close behavior (the platform's ESC-closes-dialog).
        break;
    }
  }

  // ---- Review mode --------------------------------------------------------

  function enterReview() {
    if (!post) return;
    reviewMode = true;
    resetReviewView();
  }

  function exitReview() {
    reviewMode = false;
    tileMode = false;
    resetReviewView();
  }

  function resetReviewView() {
    zoom = 1;
    panX = 0;
    panY = 0;
  }

  // Wheel zoom — zoom toward cursor. Disabled in tile mode (the
  // pattern is fixed-size by definition; scaling it would just
  // scale the view, not reveal anything new).
  function handleWheel(e: WheelEvent) {
    if (!reviewMode || tileMode) return;
    const el = canvasEl;
    if (!el) return;
    e.preventDefault();
    const rect = el.getBoundingClientRect();
    const cx = e.clientX - rect.left - rect.width / 2;
    const cy = e.clientY - rect.top - rect.height / 2;
    const factor = e.deltaY < 0 ? 1.15 : 1 / 1.15;
    const newZoom = Math.max(0.1, Math.min(20, zoom * factor));
    // image_pt = (cursor - pan) / zoom  →  pan' = cursor - image_pt * zoom'
    const ratio = newZoom / zoom;
    panX = cx - (cx - panX) * ratio;
    panY = cy - (cy - panY) * ratio;
    zoom = newZoom;
  }

  function handleCanvasMouseDown(e: MouseEvent) {
    if (!reviewMode || tileMode || e.button !== 0) return;
    dragging = true;
    dragStartX = e.clientX;
    dragStartY = e.clientY;
    panStartX = panX;
    panStartY = panY;
  }

  function handleWindowMouseMove(e: MouseEvent) {
    if (!dragging) return;
    panX = panStartX + (e.clientX - dragStartX);
    panY = panStartY + (e.clientY - dragStartY);
  }

  function handleWindowMouseUp() {
    dragging = false;
  }

  function handleReviewImgLoad(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    imgNaturalW = img.naturalWidth;
    imgNaturalH = img.naturalHeight;
  }

  // hires → /file fallback. The worker may not have generated the
  // hires variant yet (just-uploaded), and some originals don't have
  // hires at all (non-raster types). Either case: fall back to the
  // original. Triggered once per <img>; the `dataset.fallback` flag
  // prevents loops if /file is also broken.
  function handleReviewImgError(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    if (img.dataset.fallback) return;
    img.dataset.fallback = '1';
    if (currentAssetId) {
      img.src = `/api/v1/assets/${currentAssetId}/file`;
    }
  }

  function toggleStrip() {
    stripCollapsed = !stripCollapsed;
    localStorage.setItem('postModal.stripCollapsed', stripCollapsed ? '1' : '0');
  }

  // Image-type gate — keeps T (tile) disabled for non-image
  // resources (video, 3D, PDF).
  function isImageExt(ext: string | null): boolean {
    if (!ext) return false;
    const e = ext.toLowerCase().replace(/^\./, '');
    return [
      'jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'tiff', 'tif',
      'avif', 'heic', 'heif',
    ].includes(e);
  }

  // Per-slide image error handling. First failure swaps col → /file
  // (full original); second failure marks the asset broken so the
  // template renders a placeholder icon. Keyed by asset_id on a data
  // attribute so one handler covers every slide.
  function handleSlideImgError(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    const aid = img.dataset.assetId;
    if (!aid) return;
    if (img.dataset.fallback === '1') {
      brokenSlides.add(aid);
      brokenSlides = new Set(brokenSlides);
      return;
    }
    img.dataset.fallback = '1';
    img.src = `/api/v1/assets/${aid}/file`;
  }

  function colVariantUrl(assetId: string): string {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  // hires is the modal's main image — ~4K longest side at JPEG q95.
  // Falls back to /file on 404 (worker may still be generating).
  function hiresVariantUrl(assetId: string): string {
    return `/api/v1/assets/${assetId}/variants/hires`;
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

  // Pretty-print a field value based on its declared type. Returns the
  // empty string for "no value" so the caller can drop the field.
  // release_date intentionally renders as just the year — that's the
  // sensible granularity for trading-card data.
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
</script>

<svelte:window
  onkeydown={handleKeydown}
  onmousemove={handleWindowMouseMove}
  onmouseup={handleWindowMouseUp}
/>

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
    class="relative flex h-full w-full flex-col overflow-hidden bg-surface text-fg shadow-2xl sm:my-4 sm:h-[calc(100vh-2rem)] sm:rounded-lg"
    role="presentation"
  >
    <!-- Top toolbar — review enter/exit (left), close (right). The
         review button is the *only* way to enter review mode (per
         user preference: no click-the-image affordance anywhere). -->
    {#if post && !loading && !error}
      <div class="pointer-events-none absolute left-0 right-0 top-0 z-30 flex items-start justify-between p-4">
        <div class="pointer-events-auto flex items-center gap-2">
          {#if !reviewMode}
            {#if currentIsImage}
              <button
                type="button"
                onclick={enterReview}
                class="inline-flex items-center gap-1.5 rounded-md bg-black/60 px-3 py-1.5 text-xs text-white backdrop-blur-sm transition-colors hover:bg-black/80"
                title="Open review (zoom, pan, tile)"
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <circle cx="11" cy="11" r="8" />
                  <path d="m21 21-4.3-4.3" />
                </svg>
                Review
              </button>
            {/if}
          {:else}
            <button
              type="button"
              onclick={exitReview}
              class="inline-flex items-center gap-1.5 rounded-md bg-black/60 px-3 py-1.5 text-xs text-white backdrop-blur-sm transition-colors hover:bg-black/80"
              title="Back to post (Esc)"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="m15 18-6-6 6-6" />
              </svg>
              Back
            </button>
            {#if currentIsImage}
              <button
                type="button"
                onclick={() => (tileMode = !tileMode)}
                class={`inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-xs text-white backdrop-blur-sm transition-colors ${tileMode ? 'bg-accent' : 'bg-black/60 hover:bg-black/80'}`}
                aria-pressed={tileMode}
                title="Tile (T) — texture pattern view"
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <rect x="3" y="3" width="7" height="7" />
                  <rect x="14" y="3" width="7" height="7" />
                  <rect x="3" y="14" width="7" height="7" />
                  <rect x="14" y="14" width="7" height="7" />
                </svg>
                Tile
              </button>
            {/if}
            <button
              type="button"
              onclick={resetReviewView}
              class="inline-flex items-center gap-1.5 rounded-md bg-black/60 px-3 py-1.5 text-xs text-white backdrop-blur-sm transition-colors hover:bg-black/80"
              title="Reset zoom & pan (R)"
            >
              Reset
            </button>
            <span class="rounded-md bg-black/60 px-3 py-1.5 text-xs text-white/80 backdrop-blur-sm">
              Wheel: zoom · Drag: pan
            </span>
          {/if}
        </div>
      </div>
    {/if}

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
        <div role="alert" class="max-w-md rounded-md border border-danger/40 bg-danger-container px-4 py-3 text-sm text-danger">
          {error}
        </div>
      </div>
    {:else if post}
      <div class="flex flex-1 flex-col overflow-hidden md:flex-row">
        <!-- Preview pane (left). Always dark backdrop — galleries look
             best on dark regardless of the surrounding theme.
             For multi-asset posts this is a vertical scroller: one
             slide per member, scroll-snap aligned. The arrow buttons
             and footer thumbnails both call goToMember(), which
             smooth-scrolls the matching slide into view. The reverse
             direction (manual scrolling drives selectedIdx) is wired
             via an IntersectionObserver in the script.
             In review mode the scroller is replaced with a single
             zoom/pan canvas for the current asset. -->
        <div class="relative flex flex-1 overflow-hidden bg-black">
          {#if reviewMode && currentAssetId}
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
              bind:this={canvasEl}
              onwheel={handleWheel}
              onmousedown={handleCanvasMouseDown}
              class="relative h-full w-full overflow-hidden"
              class:cursor-grab={!dragging && !tileMode && currentIsImage}
              class:cursor-grabbing={dragging}
            >
              {#if tileMode && currentIsImage}
                <div
                  class="absolute inset-0"
                  style="background-image: url('/api/v1/assets/{currentAssetId}/file'); background-repeat: repeat; background-size: {imgNaturalW || 'auto'}px {imgNaturalH || 'auto'}px; background-position: center center;"
                ></div>
                <img
                  src={hiresVariantUrl(currentAssetId)}
                  onerror={handleReviewImgError}
                  alt=""
                  onload={handleReviewImgLoad}
                  class="pointer-events-none invisible absolute"
                />
              {:else}
                <img
                  src={hiresVariantUrl(currentAssetId)}
                  onerror={handleReviewImgError}
                  alt={currentMember?.asset?.title || post.title}
                  onload={handleReviewImgLoad}
                  draggable="false"
                  class="pointer-events-none absolute left-1/2 top-1/2 max-h-none max-w-none select-none"
                  style="transform: translate(calc(-50% + {panX}px), calc(-50% + {panY}px)) scale({zoom}); transform-origin: center center;"
                />
              {/if}
            </div>
          {:else if hasMultipleMembers}
            <div
              bind:this={scrollerEl}
              class="post-modal-scroller h-full w-full snap-y snap-mandatory overflow-y-auto"
            >
              {#each post.members as member, i (member.asset_id)}
                {@const slideIsImage = isImageExt(member.asset?.file_extension ?? null)}
                <div
                  data-slide-idx={i}
                  class="flex h-full w-full shrink-0 snap-start items-center justify-center"
                >
                  {#if brokenSlides.has(member.asset_id)}
                    <div class="text-fg-muted">
                      <svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" stroke-linecap="round" stroke-linejoin="round">
                        <rect x="3" y="3" width="18" height="18" rx="2" />
                        <circle cx="9" cy="9" r="2" />
                        <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
                      </svg>
                    </div>
                  {:else}
                    <img
                      src={hiresVariantUrl(member.asset_id)}
                      alt={member.asset?.title || post.title}
                      data-asset-id={member.asset_id}
                      loading={i === selectedIdx ? 'eager' : 'lazy'}
                      class="h-full w-full object-contain"
                      onerror={handleSlideImgError}
                    />
                  {/if}
                </div>
              {/each}
            </div>
          {:else if currentAssetId}
            <div class="flex h-full w-full items-center justify-center">
              {#if currentAssetId && brokenSlides.has(currentAssetId)}
                <div class="text-fg-muted">
                  <svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" stroke-linecap="round" stroke-linejoin="round">
                    <rect x="3" y="3" width="18" height="18" rx="2" />
                    <circle cx="9" cy="9" r="2" />
                    <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
                  </svg>
                </div>
              {:else}
                <img
                  src={hiresVariantUrl(currentAssetId)}
                  alt={currentMember?.asset?.title || post.title}
                  data-asset-id={currentAssetId}
                  class="h-full w-full object-contain"
                  onerror={handleSlideImgError}
                />
              {/if}
            </div>
          {:else}
            <div class="flex h-full w-full items-center justify-center text-fg-muted">
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
              onclick={() => goToMember(selectedIdx - 1)}
              disabled={selectedIdx === 0}
              class="absolute left-3 top-1/2 z-10 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur-sm transition-colors hover:bg-black/75 disabled:opacity-30 disabled:hover:bg-black/50"
              aria-label="Previous asset"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                <path d={reviewMode ? 'm15 18-6-6 6-6' : 'm18 15-6-6-6 6'} />
              </svg>
            </button>
            <button
              type="button"
              onclick={() => goToMember(selectedIdx + 1)}
              disabled={selectedIdx === post.members.length - 1}
              class="absolute right-3 top-1/2 z-10 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur-sm transition-colors hover:bg-black/75 disabled:opacity-30 disabled:hover:bg-black/50"
              aria-label="Next asset"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                <path d={reviewMode ? 'm9 18 6-6-6-6' : 'm6 9 6 6 6-6'} />
              </svg>
            </button>
            <!-- Position indicator (n / total) — bottom-center.
                 Hidden in review mode because the footer strip
                 collapsed-state already carries n / total. -->
            {#if !reviewMode}
              <div class="pointer-events-none absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full bg-black/60 px-3 py-1 text-xs font-medium text-white backdrop-blur-sm">
                {selectedIdx + 1} / {post.members.length}
              </div>
            {/if}
          {/if}

          <!-- Sidebar collapsed: show a re-open chevron tab in the
               preview's right edge so the user can get it back.
               Suppressed in review mode (no sidebar there). -->
          {#if sidebarCollapsed && !reviewMode}
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

        <!-- Sidebar (right). Hidden entirely in review mode so the
             canvas can use the full modal width for zoom/pan. -->
        {#if !sidebarCollapsed && !reviewMode}
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

              <!-- Per-asset metadata. Only the currently-selected
                   member's values are shown; switching members swaps
                   the block. Empty values are dropped so a card with
                   no flavor text doesn't get a blank row. -->
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

              <!-- Engagement: active like + comment-count display. -->
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

              <!-- Comment thread + composer. -->
              <div class="mt-6">
                <CommentsThread postId={post.id} />
              </div>
            </div>
          </aside>
        {/if}
      </div>

      <!-- Bottom member strip — only for posts with >1 member.
           Collapsible: the chevron toggle hides the thumbnail row
           and leaves a slim header showing just "n / total" so the
           viewer always knows where they are. In review mode the
           strip is the only nav for switching assets (no scroll
           snap), so the collapsed-state still shows position. -->
      {#if hasMultipleMembers}
        <div class="border-t border-border bg-surface-elevated">
          <button
            type="button"
            onclick={toggleStrip}
            class="flex w-full items-center justify-center gap-1 py-1 text-xs text-fg-muted hover:bg-surface"
            aria-expanded={!stripCollapsed}
            aria-label={stripCollapsed ? 'Show asset strip' : 'Hide asset strip'}
          >
            {#if stripCollapsed}
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m18 15-6-6-6 6" /></svg>
              <span>{selectedIdx + 1} / {post.members.length}</span>
            {:else}
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6" /></svg>
            {/if}
          </button>
          {#if !stripCollapsed}
            <div class="flex gap-2 overflow-x-auto px-2 pb-2">
              {#each post.members as member, i (member.asset_id)}
                <button
                  type="button"
                  onclick={() => goToMember(i)}
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
                      onerror={(e) => {
                        const img = e.currentTarget as HTMLImageElement;
                        if (!img.dataset.fallback) {
                          img.dataset.fallback = '1';
                          img.src = `/api/v1/assets/${member.asset_id}/file`;
                        }
                      }}
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
          {/if}
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
  /* Hide the native scrollbar on the per-member scroller — the
     position indicator + footer strip already communicate position,
     and a visible scrollbar overlapping the image looks unfinished. */
  :global(.post-modal-scroller) {
    scrollbar-width: none;
  }
  :global(.post-modal-scroller::-webkit-scrollbar) {
    display: none;
  }
</style>
