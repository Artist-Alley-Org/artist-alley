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
  import type { PlaylistSource } from '$lib/playlist/types';

  interface Props {
    source: PlaylistSource;
    /** Sidebar content threaded into AssetViewer's metadataSlot. */
    contextSlot?: Snippet;
    /** Top-of-viewer host-specific action buttons (Like, Approve,
        Remove-from-collection, etc.). Rendered on the left side of
        the top bar; the close button is fixed on the right. */
    toolbarActions?: Snippet;
    /** Called when the user closes the playlist (× / ESC / backdrop). */
    onClose: () => void;
    /** True when the playlist is a full-page route (e.g. /posts/[id])
        rather than an overlay over the browse feed. Drives the close
        button affordance — back-arrow vs ×. */
    standalone?: boolean;
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
  }

  let {
    source,
    contextSlot,
    toolbarActions,
    onClose,
    standalone = false,
    onAddToCollection,
    onRecreatePreviews,
    onEditTags,
    onEditMetadata,
    onDownloadVariant,
    onShareAsset,
    onDeleteAsset,
  }: Props = $props();

  // ---- Local state ---------------------------------------------------------

  let dialogEl: HTMLDialogElement | undefined = $state();

  // Review mode — passed through to AssetViewer. When on, the viewer
  // captures input (orbit / pan / scrub) and swaps the right pane to
  // its kind-aware tools panel. Toggled by the Review button or by
  // double-clicking the asset.
  let reviewMode = $state(false);

  // Pane open/closed state for AssetViewer's right pane. Bindable
  // through so we can drive it from the 'i' hotkey here and persist
  // it across sessions. Default open; restored from localStorage.
  let paneCollapsed = $state(false);

  // Footer thumb strip collapsed state. Persists per-browser. The
  // collapsed state shows just a chevron + "n / total" so the
  // viewer always knows where they are even with the strip hidden.
  let stripCollapsed = $state(false);

  // ---- Derived -------------------------------------------------------------

  const currentItem = $derived(source.items[source.cursor] ?? null);
  const hasMultipleItems = $derived(source.items.length > 1);

  // ---- Lifecycle -----------------------------------------------------------

  onMount(() => {
    dialogEl?.showModal();
    document.body.classList.add('overflow-hidden');
    // Restore pane + strip prefs from prior sessions.
    if (localStorage.getItem('assetPlaylist.paneCollapsed') === '1') paneCollapsed = true;
    if (localStorage.getItem('assetPlaylist.stripCollapsed') === '1') stripCollapsed = true;
  });

  $effect(() => {
    localStorage.setItem('assetPlaylist.paneCollapsed', paneCollapsed ? '1' : '0');
  });

  onDestroy(() => {
    document.body.classList.remove('overflow-hidden');
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
      case 'ArrowLeft':
      case 'ArrowUp':
        if (hasMultipleItems) {
          e.preventDefault();
          goTo(source.cursor - 1);
        }
        break;
      case 'ArrowRight':
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
        if (reviewMode) {
          e.preventDefault();
          reviewMode = false;
        }
        // ESC outside review falls through to the dialog's native
        // close behaviour (platform's "ESC closes dialog").
        break;
    }
  }

  function enterReview() {
    reviewMode = true;
  }
  function exitReview() {
    reviewMode = false;
  }

  function toggleStrip() {
    stripCollapsed = !stripCollapsed;
    localStorage.setItem('assetPlaylist.stripCollapsed', stripCollapsed ? '1' : '0');
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
  class="asset-playlist m-0 max-h-none max-w-none w-full h-full bg-transparent p-0 backdrop:bg-black/80 backdrop:backdrop-blur-sm"
  aria-labelledby="asset-playlist-title"
>
  <div
    class="relative flex h-full w-full flex-col overflow-hidden bg-surface text-fg shadow-2xl sm:my-4 sm:h-[calc(100vh-2rem)] sm:rounded-lg"
    role="presentation"
  >
    <!-- Top toolbar: review enter/exit (left) + host-supplied
         actions (left) + close (right). The Review button is the
         only way to enter review mode from chrome (per user
         preference: no "click the image" affordance). -->
    {#if !source.loading && !source.error && currentItem}
      <div class="pointer-events-none absolute left-0 right-0 top-0 z-30 flex items-start justify-between p-4">
        <div class="pointer-events-auto flex items-center gap-2">
          {#if !reviewMode}
            {#if currentItem.asset.file_hash}
              <button
                type="button"
                onclick={enterReview}
                class="inline-flex items-center gap-1.5 rounded-md bg-black/60 px-3 py-1.5 text-xs text-white backdrop-blur-sm transition-colors hover:bg-black/80"
                title="Open review (double-click the asset to do the same)"
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
              title="Back to playlist (Esc)"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="m15 18-6-6 6-6" />
              </svg>
              Back
            </button>
          {/if}
          {#if toolbarActions}
            {@render toolbarActions()}
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

    {#if source.loading}
      <!-- Skeleton: just the viewport pulse. -->
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
              bind:reviewMode
              bind:paneCollapsed
              metadataSlot={contextSlot}
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

      <!-- Bottom thumb strip — only when >1 item. Collapsible. -->
      {#if hasMultipleItems}
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
              <span>{source.cursor + 1} / {source.items.length}</span>
            {:else}
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6" /></svg>
            {/if}
          </button>
          {#if !stripCollapsed}
            <div class="flex gap-2 overflow-x-auto px-2 pb-2">
              {#each source.items as item, i (item.id)}
                <button
                  type="button"
                  onclick={() => goTo(i)}
                  class="relative h-16 w-16 shrink-0 overflow-hidden rounded border-2 transition-all"
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

<style>
  dialog.asset-playlist {
    border: none;
    inset: 0;
  }
  dialog.asset-playlist:not([open]) {
    display: none;
  }
</style>
