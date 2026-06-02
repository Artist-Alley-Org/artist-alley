<script lang="ts">
  // AssetPlaylist — generic viewer shell.
  //
  // Takes a source-agnostic PlaylistSource and renders:
  //   - the AssetViewer pinned to source.items[source.cursor]
  //   - bottom thumb-strip filmstrip for cursor navigation
  //   - floating prev/next arrows + position indicator
  //   - keyboard navigation (← / →, i, Esc)
  //   - close button (× for overlay, back-arrow for standalone)
  //   - host-supplied contextSlot threaded into AssetViewer's
  //     metadataSlot (used for post-as-playlist's author header,
  //     collection-as-playlist's description, etc.)
  //   - host-supplied toolbarActions threaded into the top bar
  //
  // Filmstrip + nav arrows + position indicator AUTO-HIDE when
  // items.length <= 1 — that makes a "playlist of 1" (a standalone
  // asset link) collapse to the same shell with no chrome wasted.
  //
  // The shell owns: dialog plumbing, cursor state, keyboard nav,
  // pane-collapsed persistence, review-mode toggle, strip-collapsed
  // persistence. The shell does NOT own: data fetching, social
  // metadata, comments — those belong to the host.
  //
  // Renamed + carved out from the original PostModal (commit history
  // in feat/viewer-polish). The "post" surface is now a thin host
  // (PostHost.svelte) that builds a PostPlaylistSource and provides
  // a contextSlot for the post-specific sidebar (author / likes /
  // comments / tags / edit / delete / etc).

  import { onDestroy, onMount, type Snippet } from 'svelte';
  import AssetViewer from './viewers/AssetViewer.svelte';
  import { kindForAsset } from './viewers/controller';
  import type { ToolDef } from './viewers/tools/contract';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';
  import type { PlaylistSource } from '$lib/playlist/types';
  import { t } from '$stores/lang.svelte';

  interface Props {
    source: PlaylistSource;
    /** Centered title bar content. Rendered in the middle zone of the
        top toolbar (between the File/Edit/About menus and the window
        controls), replacing the default filename strip. Post hosts
        pass a snippet rendering "<post title> — by <author>"; other
        hosts can omit and fall back to the filename. */
    titleSlot?: Snippet;
    /** Pass-through to AssetViewer's canvasOverlay slot — host
        renders a brush/annotation surface over the asset canvas
        without losing the sidebar or top toolbar. */
    canvasOverlay?: Snippet;
    /** Called when the user closes the playlist (× / ESC / backdrop). */
    onClose: () => void;
    /** True when the playlist is a full-page route (e.g. /posts/[id])
        rather than an overlay over the browse feed. Drives the close
        button affordance — back-arrow vs ×. */
    standalone?: boolean;
    /** Optional sibling-playlist navigator. ← / → call this with
        the requested direction so the shell can jump to the next /
        previous *playlist* in the surrounding context (next post in
        the browse feed, next collection in a curator's stash, next
        search result, etc). Hosts that lack sibling context (e.g.
        the standalone /posts/{id} route hit by direct nav) omit
        the prop and ← / → become no-op. Within-playlist asset
        navigation stays bound to A / D. */
    onNavigateSibling?: (dir: 'prev' | 'next') => void;
    /** Optional per-asset host hooks threaded into AssetViewer's
        Edit / File menus. The host supplies these to wire actions
        that target the *current* asset (the one the cursor is on).
        When omitted the corresponding menu item stays disabled with
        a "coming soon" tooltip — keeps the menu shape stable across
        hosts that don't yet implement a given action. */
    onAddToCollection?: (assetId: string) => void;
    onRecreatePreviews?: (assetId: string) => void;
    onEditTags?: (assetId: string) => void;
    onEditMetadata?: (assetId: string) => void;
    onDownloadVariant?: (assetId: string) => void;
    onShareAsset?: (assetId: string) => void;
    onDeleteAsset?: (assetId: string) => void;
    /** Extra rows to append to the global TipsSection footer at the
        bottom of the side panel. Hosts that own mode-specific tool
        surfaces (whiteboard, annotation, future review modes) pass
        a snippet that renders `<dt>/<dd>` rows + an uppercase
        section header so every tool's tips stay in one consolidated
        reference inside the shell's single Tips footer. */
    extraTips?: Snippet;
    /** Host-injected tools — appended to the registry at shell
        mount. Hosts that own rich detail surfaces (PostHost's post
        details / likes / comments / cover-picker) register their
        own ToolDef with the right order. The built-in Details
        tool stays in the dropdown alongside. */
    customTools?: ToolDef[];
    /** Whiteboard session passed through to the WhiteboardTool
        when the host has wired one (post-anchored today). */
    whiteboardSession?: WhiteboardSession;
    /** Host hook bag forwarded into the ToolContext. See
        AssetViewer's prop docs for the conventional namespaces
        (hostHooks.whiteboard, hostHooks.details, ...). */
    hostHooks?: Record<string, unknown>;
  }

  let {
    source,
    titleSlot,
    canvasOverlay,
    onClose,
    standalone = false,
    onNavigateSibling,
    onAddToCollection,
    onRecreatePreviews,
    onEditTags,
    onEditMetadata,
    onDownloadVariant,
    onShareAsset,
    onDeleteAsset,
    extraTips,
    customTools = [],
    whiteboardSession,
    hostHooks,
  }: Props = $props();

  // ---- Local state ---------------------------------------------------------

  let dialogEl: HTMLDialogElement | undefined = $state();

  // Review mode — passed through to AssetViewer. When on, the viewer
  // captures input (orbit / pan / scrub) and swaps the right pane to
  // its kind-aware tools panel. Toggled by the Review button or by
  // double-clicking the asset.

  // Pane open/closed state for AssetViewer's right pane. Bindable
  // through so we can drive it from the 'i' hotkey here and persist
  // it across sessions. Default open; restored from localStorage.
  let paneCollapsed = $state(false);

  // Footer thumb strip collapsed state. Persists per-browser. The
  // collapsed state shows just a chevron + "n / total" so the
  // viewer always knows where they are even with the strip hidden.
  let stripCollapsed = $state(false);

  // Footer thumb-strip height in CSS pixels (expanded state only).
  // The user can drag the top edge of the strip up/down to resize.
  // Capped at 25% of viewport height so it never eats the viewer.
  // Floor matches the default — shrinking below that is what the
  // collapse chevron is for. Persists per-browser via localStorage.
  const STRIP_MIN = 96;
  const STRIP_DEFAULT = 96;
  let stripHeight = $state(STRIP_DEFAULT);
  function stripMax(): number {
    // Re-evaluated on every drag start + window resize — 25vh tracks
    // the live viewport, so resizing the browser doesn't trap the
    // user with a strip that's now too tall to fit.
    return Math.floor(window.innerHeight * 0.25);
  }

  // Window-chrome state. The shell renders like a modern app window
  // (Photoshop / VS Code / Figma vibe): rounded corners, drop
  // shadow, sits under the global navbar so links stay reachable.
  // The maximize button covers the navbar for a full-bleed view.
  //
  // Defaults:
  //   - standalone (full-page /posts/{id})  → maximized
  //   - overlay    (?post= over the feed)   → windowed
  //
  // Persisted per-browser so a user who prefers maximized always
  // gets it back on the next open. Standalone never falls below
  // maximized=true since "windowed" inside a route with no
  // underlying page is a worse UX.
  let maximized = $state<boolean>(standalone);

  // ---- Derived -------------------------------------------------------------

  const currentItem = $derived(source.items[source.cursor] ?? null);
  const hasMultipleItems = $derived(source.items.length > 1);
  // Current asset's kind drives the contextual hotkey legend below —
  // we surface different shortcut sections for video/audio (timeline
  // playback + loop) vs static kinds. Falls back to 'placeholder'
  // when no item is mounted yet.
  const currentKind = $derived(
    currentItem ? kindForAsset(currentItem.asset) : 'placeholder',
  );
  const isTimelineKind = $derived(currentKind === 'video' || currentKind === 'audio');

  // ---- Lifecycle -----------------------------------------------------------

  // Navbar bottom tracking — keep the windowed viewer's top edge
  // glued to the navbar's bottom even as the navbar grows (the user
  // is planning to expand it downward for advanced search / filter
  // panels). Written to --aa-navbar-bottom on the root and consumed
  // by the dialog's CSS. Falls back to 53px if the header isn't
  // findable (defensive — auth routes have no header).
  let navbarObserver: ResizeObserver | undefined;
  function trackNavbarBottom() {
    const header = document.querySelector('header');
    if (!header) {
      document.documentElement.style.setProperty('--aa-navbar-bottom', '53px');
      return;
    }
    const apply = () => {
      const h = Math.round(header.getBoundingClientRect().height);
      document.documentElement.style.setProperty('--aa-navbar-bottom', `${h}px`);
    };
    apply();
    navbarObserver = new ResizeObserver(apply);
    navbarObserver.observe(header);
  }

  onMount(() => {
    // Restore pane + strip prefs first so the initial open matches
    // last session.
    if (localStorage.getItem('assetPlaylist.paneCollapsed') === '1') paneCollapsed = true;
    if (localStorage.getItem('assetPlaylist.stripCollapsed') === '1') stripCollapsed = true;
    const savedHeight = parseInt(localStorage.getItem('assetPlaylist.stripHeight') ?? '', 10);
    if (Number.isFinite(savedHeight) && savedHeight >= STRIP_MIN) {
      // Re-clamp on restore — the user could have shrunk the viewport
      // since the height was saved, making it now exceed 25vh.
      stripHeight = Math.min(savedHeight, stripMax());
    }
    if (!standalone) {
      // Overlay mode: read the windowed/maximized pref. Standalone
      // stays at its default (always maximized) regardless.
      const pref = localStorage.getItem('assetPlaylist.maximized');
      if (pref === '1') maximized = true;
      else if (pref === '0') maximized = false;
    }
    trackNavbarBottom();
    openDialog();
    // overflow-hidden on the body only when we're covering the whole
    // viewport — windowed mode leaves the navbar interactive so the
    // body scroll behaviour should stay normal.
    if (maximized) document.body.classList.add('overflow-hidden');
  });

  // Open the dialog in the right mode. showModal() blocks page
  // interaction (correct for maximized = "this is the world now");
  // show() doesn't (correct for windowed = "I'm sitting on top, but
  // the navbar behind me is still clickable").
  function openDialog() {
    if (!dialogEl) return;
    if (maximized) {
      dialogEl.showModal();
    } else {
      dialogEl.show();
    }
  }

  function toggleMaximize() {
    maximized = !maximized;
    if (!standalone) {
      localStorage.setItem('assetPlaylist.maximized', maximized ? '1' : '0');
    }
    // Swap the dialog mode by closing + reopening — there's no
    // showModal()/show() in-place switch.
    if (dialogEl?.open) dialogEl.close();
    openDialog();
    if (maximized) document.body.classList.add('overflow-hidden');
    else document.body.classList.remove('overflow-hidden');
  }

  $effect(() => {
    localStorage.setItem('assetPlaylist.paneCollapsed', paneCollapsed ? '1' : '0');
  });

  onDestroy(() => {
    document.body.classList.remove('overflow-hidden');
    navbarObserver?.disconnect();
  });

  // Clamp the cursor if the source's items array shrinks while open
  // (host deleted an item, for example). Never let cursor point past
  // items.length-1.
  $effect(() => {
    const n = source.items.length;
    if (n === 0) return;
    if (source.cursor > n - 1) source.cursor = n - 1;
    if (source.cursor < 0) source.cursor = 0;
  });

  // Infinite-scroll hook for search/gallery sources. When the cursor
  // approaches the last item and the source has a loadMore handler,
  // fire it so navigation can spill into the next page.
  $effect(() => {
    if (!source.loadMore) return;
    if (source.loading) return;
    if (source.items.length === 0) return;
    if (source.cursor >= source.items.length - 2) {
      void source.loadMore();
    }
  });

  // ---- Handlers ------------------------------------------------------------

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

  /** Jump to a cursor position. Clamps to [0, items.length-1]; just
      mutates source.cursor — the {#key} on AssetViewer remounts the
      view body so each asset gets a fresh viewer instance (important
      for 3D / video tear-down). */
  function goTo(idx: number) {
    const n = source.items.length;
    if (n === 0) return;
    source.cursor = Math.max(0, Math.min(n - 1, idx));
  }

  function handleKeydown(e: KeyboardEvent) {
    // Ignore key handling while focus is in a text input — comment
    // composers / search boxes inside contextSlot need to type freely.
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
      return;
    }
    switch (e.key) {
      // ← / → navigate between sibling PLAYLISTS in the surrounding
      // context (next post in the feed, next collection, etc). The
      // host wires this when it has a sibling concept; no-op
      // otherwise.
      case 'ArrowLeft':
        if (onNavigateSibling) {
          e.preventDefault();
          onNavigateSibling('prev');
        }
        break;
      case 'ArrowRight':
        if (onNavigateSibling) {
          e.preventDefault();
          onNavigateSibling('next');
        }
        break;
      // ↑ / ↓ navigate between assets WITHIN the current playlist.
      // Separated from ← / → so users in a feed-overlay context can
      // both flip through posts (← →) AND scrub through a multi-
      // asset post (↑ ↓) without losing their place in the feed.
      // The two axes (horizontal = sibling playlists, vertical = items
      // within a playlist) mirror how users mentally model the feed:
      // posts in a row, assets stacked beneath each post.
      case 'ArrowUp':
        if (hasMultipleItems) {
          e.preventDefault();
          goTo(source.cursor - 1);
        }
        break;
      case 'ArrowDown':
        if (hasMultipleItems) {
          e.preventDefault();
          goTo(source.cursor + 1);
        }
        break;
      case 'i':
      case 'I':
        e.preventDefault();
        paneCollapsed = !paneCollapsed;
        break;
      case 'Escape':
        // <dialog> only handles ESC natively when opened via
        // showModal() (maximized mode). Windowed mode uses show(),
        // which has no native ESC. Close explicitly so the gesture
        // works the same in both modes.
        e.preventDefault();
        handleClose();
        break;
    }
  }

  function toggleStrip() {
    stripCollapsed = !stripCollapsed;
    localStorage.setItem('assetPlaylist.stripCollapsed', stripCollapsed ? '1' : '0');
  }

  // Drag-to-resize the bottom thumb strip. Tracks mouse delta from
  // mousedown position (drag up = positive Δ = grow); clamps to the
  // viewport-relative range each frame so resizing the browser mid-
  // drag stays honest. Persist on mouseup so a partial drag the user
  // bails out of (Esc) doesn't pollute their saved preference.
  function startStripResize(e: MouseEvent) {
    e.preventDefault();
    const startY = e.clientY;
    const startHeight = stripHeight;
    const move = (mv: MouseEvent) => {
      const dy = startY - mv.clientY;
      stripHeight = Math.max(STRIP_MIN, Math.min(stripMax(), startHeight + dy));
    };
    const up = () => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
      localStorage.setItem('assetPlaylist.stripHeight', String(stripHeight));
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  }

  function colVariantUrl(assetId: string): string {
    return `/api/v1/assets/${assetId}/variants/col`;
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<dialog
  bind:this={dialogEl}
  onclose={handleDialogClose}
  onclick={handleBackdropClick}
  class="asset-playlist m-0 bg-transparent p-0 outline-none"
  class:max={maximized}
  class:windowed={!maximized}
  aria-labelledby="asset-playlist-title"
>
  <div
    class="relative flex h-full w-full flex-col overflow-hidden bg-surface text-fg shadow-2xl"
    role="presentation"
  >

    {#if source.loading && source.items.length === 0}
      <!-- Skeleton — only on the very first load (no items yet). On
           subsequent re-targets (e.g. browse-feed ←/→ swapping to
           the next post) the source's load() keeps the previous
           items on screen until the new data arrives, so the
           AssetViewer + ViewerMenuBar stay mounted and there's no
           chrome flicker. -->
      <div class="flex flex-1">
        <div class="flex-1 animate-pulse bg-black/30"></div>
      </div>
    {:else if source.error}
      <div class="flex flex-1 items-center justify-center p-8">
        <div role="alert" class="max-w-md rounded-md border border-danger/40 bg-danger-container px-4 py-3 text-sm text-danger">
          {source.error}
        </div>
      </div>
    {:else if currentItem}
      <div class="relative flex flex-1 overflow-hidden bg-black">
        {#if currentItem.asset.file_hash}
          <!-- AssetViewer owns the canvas double-click gesture
               (toggles reviewMode). Wrapping it in another dblclick
               handler here would fight the toggle. -->
          <div class="flex h-full w-full items-center justify-center">
            <AssetViewer
              asset={currentItem.asset}
              active={true}
              bind:paneCollapsed
              {customTools}
              {whiteboardSession}
              {hostHooks}
              {titleSlot}
              {canvasOverlay}
              extraTips={playlistHotkeys}
              {maximized}
              onToggleMaximize={toggleMaximize}
              onClose={handleClose}
              onAddToCollection={onAddToCollection
                ? () => onAddToCollection(currentItem.asset.id)
                : undefined}
              onRecreatePreviews={onRecreatePreviews
                ? () => onRecreatePreviews(currentItem.asset.id)
                : undefined}
              onEditTags={onEditTags
                ? () => onEditTags(currentItem.asset.id)
                : undefined}
              onEditMetadata={onEditMetadata
                ? () => onEditMetadata(currentItem.asset.id)
                : undefined}
              onDownloadVariant={onDownloadVariant
                ? () => onDownloadVariant(currentItem.asset.id)
                : undefined}
              onShareAsset={onShareAsset
                ? () => onShareAsset(currentItem.asset.id)
                : undefined}
              onDeleteAsset={onDeleteAsset
                ? () => onDeleteAsset(currentItem.asset.id)
                : undefined}
            />
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

        <!-- Member nav arrows: visible only when there's >1 item.
             These shift left when the pane is open so they don't
             vanish behind it. -->
        {#if hasMultipleItems}
          <button
            type="button"
            onclick={() => goTo(source.cursor - 1)}
            disabled={source.cursor === 0}
            class="absolute left-3 top-1/2 z-20 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur-sm transition-colors hover:bg-black/75 disabled:opacity-30 disabled:hover:bg-black/50"
            aria-label="Previous asset"
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
              <path d="m15 18-6-6 6-6" />
            </svg>
          </button>
          <button
            type="button"
            onclick={() => goTo(source.cursor + 1)}
            disabled={source.cursor === source.items.length - 1}
            class="absolute top-1/2 z-20 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur-sm transition-[right] duration-200 hover:bg-black/75 disabled:opacity-30 disabled:hover:bg-black/50"
            class:right-3={paneCollapsed}
            class:right-[25rem]={!paneCollapsed}
            aria-label="Next asset"
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
              <path d="m9 18 6-6-6-6" />
            </svg>
          </button>
          <!-- Position indicator (n / total) — bottom-center. -->
          <div class="pointer-events-none absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full bg-black/60 px-3 py-1 text-xs font-medium text-white backdrop-blur-sm">
            {source.cursor + 1} / {source.items.length}
          </div>
        {/if}
      </div>

      <!-- Bottom thumb strip — only when >1 item. Collapsible AND
           drag-resizable: the user can grab the top edge of the strip
           and pull it upward to grow the thumbnails (capped at 25vh
           so the viewer never gets squeezed out). Two separate
           affordances:
             - The chevron row toggles collapsed (no thumbs at all);
             - The handle above it resizes the expanded strip.
           Both persist independently in localStorage. -->
      {#if hasMultipleItems}
        <div
          class="flex shrink-0 flex-col border-t border-border bg-surface-elevated"
          style={stripCollapsed ? '' : `height: ${stripHeight}px`}
        >
          {#if !stripCollapsed}
            <!-- Resize handle — thin draggable bar at the very top of
                 the strip. cursor: ns-resize signals the gesture; the
                 visible grip widens + tints on hover so the affordance
                 isn't entirely cursor-based (accessibility). -->
            <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
            <div
              role="separator"
              aria-orientation="horizontal"
              aria-label="Resize playlist strip"
              aria-valuenow={stripHeight}
              aria-valuemin={STRIP_MIN}
              onmousedown={startStripResize}
              class="group flex h-1.5 shrink-0 cursor-ns-resize items-center justify-center hover:bg-accent/30"
            >
              <div class="h-0.5 w-10 rounded-full bg-fg-muted/40 group-hover:bg-accent/70"></div>
            </div>
          {/if}
          <button
            type="button"
            onclick={toggleStrip}
            class="flex w-full shrink-0 items-center justify-center gap-1 py-1 text-xs text-fg-muted hover:bg-surface"
            aria-expanded={!stripCollapsed}
            aria-label={stripCollapsed ? 'Show asset strip' : 'Hide asset strip'}
          >
            {#if stripCollapsed}
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m18 15-6-6-6 6" /></svg>
              <span>{source.cursor + 1} / {source.items.length}</span>
            {:else}
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6" /></svg>
            {/if}
          </button>
          {#if !stripCollapsed}
            <!-- Thumbnails fill the remaining vertical space inside
                 the sized wrapper (flex-1 + min-h-0). aspect-square
                 keeps them square; h-full sizes them off the row so
                 dragging the handle scales every thumb in lockstep. -->
            <div class="flex min-h-0 flex-1 gap-2 overflow-x-auto overflow-y-hidden px-2 pb-2">
              {#each source.items as item, i (item.id)}
                <button
                  type="button"
                  onclick={() => goTo(i)}
                  class="relative aspect-square h-full shrink-0 overflow-hidden rounded border-2 transition-all"
                  class:border-accent={i === source.cursor}
                  class:opacity-100={i === source.cursor}
                  class:border-transparent={i !== source.cursor}
                  class:opacity-50={i !== source.cursor}
                  class:hover:opacity-100={i !== source.cursor}
                  aria-label="Show asset {i + 1}"
                  aria-current={i === source.cursor ? 'true' : undefined}
                >
                  {#if item.asset.file_hash}
                    <img
                      src={colVariantUrl(item.asset.id)}
                      alt=""
                      loading="lazy"
                      class="h-full w-full object-cover"
                      onerror={(e) => {
                        const img = e.currentTarget as HTMLImageElement;
                        if (!img.dataset.fallback) {
                          img.dataset.fallback = '1';
                          img.src = `/api/v1/assets/${item.asset.id}/file`;
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
    {:else}
      <!-- Loaded but no items: friendly empty state. -->
      <div class="flex flex-1 items-center justify-center p-8 text-fg-muted">
        <p>No assets in this playlist.</p>
      </div>
    {/if}
  </div>
</dialog>

<!-- Hotkey legend — passed through AssetViewer and rendered as the
     pinned footer of the right pane. The keys shown are filtered by
     context: A/D only appear in multi-asset playlists; ←/→ only when
     the host wired sibling-nav (browse-feed overlay does; standalone
     /posts/{id} doesn't). -->
<!-- Nav / shell hotkey rows. Emits `<dt>/<dd>` pairs directly into
     the shell's TipsSection <dl> grid (no outer <details> — the
     shell owns the accordion). Section dividers use the
     col-span-2 sub-heading convention. The host's own
     extraTips (e.g. PostHost's mode-specific shortcuts) appends
     below this via AssetViewer's extraTips prop. -->
{#snippet playlistHotkeys()}
  <dt class="col-span-2 mt-1 text-fg-muted/70">{t('viewer_hotkeys.title')}</dt>
  {#if hasMultipleItems}
    <dt class="font-mono text-fg">↑ · ↓</dt>
    <dd class="text-fg-muted">{t('viewer_hotkeys.prev_asset')} · {t('viewer_hotkeys.next_asset')}</dd>
  {/if}
  {#if onNavigateSibling}
    <dt class="font-mono text-fg">← · →</dt>
    <dd class="text-fg-muted">{t('viewer_hotkeys.prev_post')} · {t('viewer_hotkeys.next_post')}</dd>
  {/if}
  <dt class="font-mono text-fg">I</dt><dd class="text-fg-muted">{t('viewer_hotkeys.toggle_panel')}</dd>
  <dt class="font-mono text-fg">F</dt><dd class="text-fg-muted">{t('viewer_hotkeys.fullscreen')}</dd>
  <dt class="font-mono text-fg">R</dt><dd class="text-fg-muted">{t('viewer_hotkeys.reset_view')}</dd>
  <dt class="font-mono text-fg">Esc</dt><dd class="text-fg-muted">{t('viewer_hotkeys.close')}</dd>
  {#if isTimelineKind}
    <dt class="col-span-2 mt-1 text-fg-muted/70">{t('viewer_hotkeys.section_playback')}</dt>
    <dt class="font-mono text-fg">Space · K</dt><dd class="text-fg-muted">{t('viewer_hotkeys.play_pause')}</dd>
    <dt class="font-mono text-fg">J · L</dt><dd class="text-fg-muted">{t('viewer_hotkeys.rewind_forward')}</dd>
    <dt class="font-mono text-fg">, · .</dt><dd class="text-fg-muted">{t('viewer_hotkeys.step_back_forward')}</dd>
    <dt class="font-mono text-fg">⇧ + , · .</dt><dd class="text-fg-muted">{t('viewer_hotkeys.step_back_forward_10')}</dd>
    <dt class="font-mono text-fg">1 – 5</dt><dd class="text-fg-muted">{t('viewer_hotkeys.speed_range')}</dd>
    <dt class="font-mono text-fg">G</dt><dd class="text-fg-muted">{t('viewer_hotkeys.goto_frame')}</dd>
    <dt class="font-mono text-fg">I · O</dt><dd class="text-fg-muted">{t('viewer_hotkeys.loop_in_out')}</dd>
    <dt class="font-mono text-fg">⌫</dt><dd class="text-fg-muted">{t('viewer_hotkeys.loop_clear')}</dd>
    <dt class="font-mono text-fg">Ctrl/⌘ + wheel</dt><dd class="text-fg-muted">{t('viewer_hotkeys.zoom_scrubber')}</dd>
  {/if}
  {#if currentKind === 'audio'}
    <dt class="col-span-2 mt-1 text-fg-muted/70">{t('viewer_hotkeys.section_waveform')}</dt>
    <dt class="font-mono text-fg">Click</dt><dd class="text-fg-muted">{t('viewer_hotkeys.wave_seek')}</dd>
    <dt class="font-mono text-fg">⇧ + drag</dt><dd class="text-fg-muted">{t('viewer_hotkeys.wave_select_loop')}</dd>
    <dt class="font-mono text-fg">Ctrl/⌘ + wheel</dt><dd class="text-fg-muted">{t('viewer_hotkeys.wave_zoom')}</dd>
  {/if}
  {#if extraTips}
    {@render extraTips()}
  {/if}
{/snippet}

<style>
  dialog.asset-playlist {
    border: none;
    outline: none;
    /* Sit above any in-page chrome (the BrowseFooter is z-20, theme
       toaster z-30, etc). Native non-modal <dialog> doesn't auto-
       promote to the top layer the way showModal() does, so we
       enforce the layering ourselves. */
    z-index: 40;
  }
  /* Maximized: full viewport, modal backdrop. */
  dialog.asset-playlist.max {
    top: 0;
    right: 0;
    bottom: 0;
    left: 0;
    width: 100%;
    height: 100%;
    max-width: none;
    max-height: none;
  }
  dialog.asset-playlist.max::backdrop {
    background: rgba(0, 0, 0, 0.8);
    backdrop-filter: blur(4px);
  }
  /* Windowed: flush against the navbar bottom + viewport edges. The
     viewer behaves like a route below the navbar — covers the
     BrowseFooter and any other in-page chrome, leaves the navbar
     interactive (non-modal dialog). --aa-navbar-bottom is set from
     JS via a ResizeObserver on <header>, so when the navbar grows
     downward later (advanced search drawer, etc.) the viewer top
     follows it without code change. */
  dialog.asset-playlist.windowed {
    top: var(--aa-navbar-bottom, 53px);
    right: 0;
    bottom: 0;
    left: 0;
    width: 100%;
    height: auto;
    max-width: none;
    max-height: none;
  }
  dialog.asset-playlist:not([open]) {
    display: none;
  }
  /* Collapse-section chevron — points right when closed, rotates 90°
     down when the parent <details> is open. Same idiom PostHost uses
     for the Metadata section; centralising it here so any future
     <details class="aa-collapse"> rendered inside this component
     gets the same affordance for free. */
  :global(details.aa-collapse[open] > summary .aa-chevron) {
    transform: rotate(90deg);
  }
  :global(details.aa-collapse > summary::-webkit-details-marker) {
    display: none;
  }
</style>
