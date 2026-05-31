<script lang="ts">
  // Top-of-viewer menubar — Photoshop / OS-style. Renders File / Edit /
  // About dropdowns, an asset info strip, a Review-mode toggle, and the
  // quick-action icon row that used to live as a floating column over
  // the canvas. Hoisting the icons into the bar means they don't
  // overlap content the user is trying to inspect, and the menus give
  // a real home for actions (download, copy link, edit metadata, file
  // info) that previously had no surface.
  //
  // Stays on a single 36px row at all viewport widths — the asset info
  // strip in the middle truncates with ellipsis rather than wrapping.
  // The right-side icon group always stays visible because that's the
  // panel toggle the user needs to find when an unfamiliar layout
  // has them confused.

  import type { Snippet } from 'svelte';
  import Menu from '$components/Menu.svelte';
  import { t } from '$stores/lang.svelte';
  import type { ViewAsset, ViewController } from './controller';

  interface Props {
    asset: ViewAsset;
    controller: ViewController;
    reviewMode: boolean;
    paneCollapsed: boolean;
    paneEnabled: boolean;
    isFullscreen: boolean;
    /** Centered title. Overrides the default filename strip — post
        hosts pass a snippet like "<post title> — by <author>". */
    titleSlot?: Snippet;
    /** Window-chrome state — drives the maximize / restore icon. The
        shell owns the actual dialog mode; the bar just fires the
        toggle callback. */
    maximized?: boolean;
    onToggleMaximize?: () => void;
    onResetView: () => void;
    onToggleFullscreen: () => void;
    onTogglePane: () => void;
    onToggleReview: () => void;
    /** Optional — when set, ViewerMenuBar shows a close button in the
        window-controls zone (right edge) AND a "Close" entry in the
        File menu. Hosts that own their own close affordance omit this. */
    onClose?: () => void;
    /** Optional — Edit menu's "Add to collection…" item is enabled
        when the host wires this callback. The host opens its own
        CollectionPicker so a single picker component drives both
        per-asset and per-playlist add flows. */
    onAddToCollection?: () => void;
    /** Optional — Edit menu's "Recreate previews" item appears when
        wired. Triggers the host's re-enqueue logic for the current
        asset's preview job. */
    onRecreatePreviews?: () => void;
    /** Optional per-asset Edit/File menu hooks. Each enables its
        menu item when the host wires it; without the hook the item
        stays disabled with a "coming soon" tooltip. */
    onEditTags?: () => void;
    onEditMetadata?: () => void;
    onDownloadVariant?: () => void;
    onShareAsset?: () => void;
    onDeleteAsset?: () => void;
    /** Optional — when wired, the Tools dropdown's Whiteboard item
        becomes active. Currently host-provided by PostHost (post-level
        whiteboards); future hosts can ignore it. */
    onToggleWhiteboard?: () => void;
    /** Currently-on state for the Whiteboard toggle (drives the
        Tools-menu checkmark). */
    whiteboardOpen?: boolean;
  }

  let {
    asset,
    controller,
    reviewMode,
    paneCollapsed,
    paneEnabled,
    isFullscreen,
    titleSlot,
    maximized = false,
    onToggleMaximize,
    onResetView,
    onToggleFullscreen,
    onTogglePane,
    onToggleReview,
    onClose,
    onAddToCollection,
    onRecreatePreviews,
    onEditTags,
    onEditMetadata,
    onDownloadVariant,
    onShareAsset,
    onDeleteAsset,
    onToggleWhiteboard,
    whiteboardOpen = false,
  }: Props = $props();

  // ── Derived asset display values ──────────────────────────────────
  const ext = $derived(asset.file_extension?.toLowerCase() ?? '');
  const filename = $derived(() => {
    const title = (asset.title ?? '').trim() || t('viewer_menu.untitled');
    // Don't double-append the extension if the title already includes it.
    if (ext && !title.toLowerCase().endsWith('.' + ext)) return `${title}.${ext}`;
    return title;
  });
  const extLabel = $derived(ext ? ext.toUpperCase() : '');

  // hudExtra is the per-kind dimension/codec readout (W×H for images,
  // sample rate for audio, page count for PDF, etc) the view body
  // writes into the controller on mount.
  const dimensions = $derived(controller.hudExtra ?? '');

  // Tools-dropdown "any tool currently active" indicator — drives a
  // subtle tint on the dropdown trigger so the user can see "something
  // is on" from a closed bar.
  const toolsActive = $derived(reviewMode || whiteboardOpen);

  // ── Actions ───────────────────────────────────────────────────────
  function downloadOriginal() {
    window.open(`/api/v1/assets/${asset.id}/file`, '_blank');
  }
  function copyLink() {
    void navigator.clipboard?.writeText(window.location.href);
  }
  function openInNewTab() {
    window.open(window.location.href, '_blank');
  }

  // Menu-trigger class. Helper exists because Tailwind's slash-syntax
  // utilities (bg-white/10) break Svelte's `class:` directive parser —
  // the slash is read as a tag-close. Building the class string in JS
  // sidesteps the directive entirely.
  function triggerClass(open: boolean): string {
    const base = 'inline-block rounded px-2.5 py-1 hover:bg-white/10';
    return open ? `${base} bg-white/10` : base;
  }
</script>

<!--
  Three-zone toolbar — the app-window aesthetic the design brief asks
  for (macOS / VS Code / Photoshop title-bar shape).

    [ ☰ menus · bulk actions ]   ───   [ title ]   ───   [ controls × ]
        ↑ left zone                 centered             ↑ right zone

  flex-1 spacers either side of the centered title keep the title
  optically centred regardless of how much left/right content there is
  (a long bulk-action row or a missing close button doesn't shift the
  title off-axis). The center zone truncates with ellipsis so a long
  post title can't push the right-side controls off-screen.
-->
<div
  class="relative z-30 flex h-9 shrink-0 items-center gap-0.5 border-b border-white/10 bg-black/85 px-1 text-xs text-white/90 backdrop-blur"
>
  <!-- File menu -->
  <Menu align="left">
    {#snippet trigger({ open })}
      <span class={triggerClass(open)}>{t('viewer_menu.file')}</span>
    {/snippet}
    <button
      type="button"
      role="menuitem"
      onclick={downloadOriginal}
      class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
    >
      {t('viewer_menu.download_original')}
    </button>
    {#if onDownloadVariant}
      <button
        type="button"
        role="menuitem"
        onclick={onDownloadVariant}
        class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
      >
        {t('viewer_menu.download_variant')}
      </button>
    {/if}
    <button
      type="button"
      role="menuitem"
      onclick={copyLink}
      class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
    >
      {t('viewer_menu.copy_link')}
    </button>
    <button
      type="button"
      role="menuitem"
      onclick={openInNewTab}
      class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
    >
      {t('viewer_menu.open_in_new_tab')}
    </button>
    {#if onShareAsset}
      <button
        type="button"
        role="menuitem"
        onclick={onShareAsset}
        class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
      >
        {t('viewer_menu.share_asset')}
      </button>
    {/if}
    {#if onClose}
      <div class="my-1 h-px bg-border"></div>
      <button
        type="button"
        role="menuitem"
        onclick={onClose}
        class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
      >
        {t('viewer_menu.close')}
      </button>
    {/if}
  </Menu>

  <!-- Edit menu — stubs for now. The actual editors land with the
       per-surface phases (tags / metadata / collections editors). -->
  <Menu align="left">
    {#snippet trigger({ open })}
      <span class={triggerClass(open)}>{t('viewer_menu.edit')}</span>
    {/snippet}
    {#if onEditTags}
      <button
        type="button"
        role="menuitem"
        onclick={onEditTags}
        class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
      >
        {t('viewer_menu.edit_tags')}
      </button>
    {:else}
      <button
        type="button"
        role="menuitem"
        disabled
        class="block w-full cursor-not-allowed px-3 py-1.5 text-left text-sm text-fg-muted opacity-60"
        title={t('viewer_menu.coming_soon')}
      >
        {t('viewer_menu.edit_tags')}
      </button>
    {/if}
    {#if onEditMetadata}
      <button
        type="button"
        role="menuitem"
        onclick={onEditMetadata}
        class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
      >
        {t('viewer_menu.edit_metadata')}
      </button>
    {:else}
      <button
        type="button"
        role="menuitem"
        disabled
        class="block w-full cursor-not-allowed px-3 py-1.5 text-left text-sm text-fg-muted opacity-60"
        title={t('viewer_menu.coming_soon')}
      >
        {t('viewer_menu.edit_metadata')}
      </button>
    {/if}
    {#if onAddToCollection}
      <button
        type="button"
        role="menuitem"
        onclick={onAddToCollection}
        class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
      >
        {t('viewer_menu.add_to_collection')}
      </button>
    {:else}
      <button
        type="button"
        role="menuitem"
        disabled
        class="block w-full cursor-not-allowed px-3 py-1.5 text-left text-sm text-fg-muted opacity-60"
        title={t('viewer_menu.coming_soon')}
      >
        {t('viewer_menu.add_to_collection')}
      </button>
    {/if}
    {#if onRecreatePreviews}
      <div class="my-1 h-px bg-border"></div>
      <button
        type="button"
        role="menuitem"
        onclick={onRecreatePreviews}
        class="block w-full px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
      >
        {t('viewer_menu.recreate_previews')}
      </button>
    {/if}
    {#if onDeleteAsset}
      <div class="my-1 h-px bg-border"></div>
      <button
        type="button"
        role="menuitem"
        onclick={onDeleteAsset}
        class="block w-full px-3 py-1.5 text-left text-sm text-danger hover:bg-danger-container"
      >
        {t('viewer_menu.delete_asset')}
      </button>
    {/if}
  </Menu>

  <!-- About menu — opens an inline file-info panel inside the dropdown.
       Photoshop calls this "File Info"; "About" matches the OS menubar
       idiom the user asked for and reads more naturally for end users
       than "Info". -->
  <Menu align="left" panelClass="min-w-[18rem]">
    {#snippet trigger({ open })}
      <span class={triggerClass(open)}>{t('viewer_menu.about')}</span>
    {/snippet}
    <div class="px-3 py-2 text-xs leading-relaxed text-fg-muted">
      <div class="mb-2 break-all text-sm font-medium text-fg">{filename()}</div>
      {#if extLabel}
        <div><span class="text-fg-muted">{t('viewer_menu.about_type')}:</span> {extLabel}</div>
      {/if}
      {#if dimensions}
        <div>
          <span class="text-fg-muted">{t('viewer_menu.about_dimensions')}:</span>
          {dimensions}
        </div>
      {/if}
      {#if controller.hasTimeline && controller.totalFrames > 0}
        <div>
          <span class="text-fg-muted">{t('viewer_menu.about_frames')}:</span>
          {controller.totalFrames}
        </div>
        <div>
          <span class="text-fg-muted">{t('viewer_menu.about_fps')}:</span>
          {controller.fps}
        </div>
      {/if}
      {#if asset.file_hash}
        <div class="mt-2 font-mono text-[10px] text-fg-muted">
          {t('viewer_menu.about_sha256')}: {asset.file_hash.slice(0, 16)}…
        </div>
      {/if}
    </div>
  </Menu>

  <!-- Spacer between left zone and centered title. The left zone is
       reserved for the *current asset* — File/Edit/About menus that
       act on whichever asset the cursor is on. Playlist-wide actions
       (Add all, Download ZIP, etc.) live in the sidebar host menu so
       the top bar stays single-asset focused. -->
  <div class="min-w-2 flex-1"></div>

  <!-- Centered title. Hosts pass a snippet (post title + author);
       fallback is the filename + dimensions strip. Constrained to
       half the bar so a giant title can't shove the controls. -->
  <div class="flex min-w-0 max-w-[50%] items-center justify-center px-2 text-center">
    {#if titleSlot}
      {@render titleSlot()}
    {:else}
      <span class="truncate text-white/80" title={filename()}>{filename()}</span>
      {#if dimensions}
        <span class="ml-2 hidden whitespace-nowrap text-white/50 sm:inline"
          >· {dimensions}</span
        >
      {/if}
    {/if}
  </div>

  <!-- Spacer between centered title and right zone. -->
  <div class="min-w-2 flex-1"></div>

  <!-- Tools dropdown — replaces the standalone Review button. Single
       toolbox glyph opens a small menu with Review · Whiteboard ·
       (future: Annotate · Compare). The dropdown highlights when any
       tool inside is active so the user can see "something's on"
       from the bar itself. -->
  <Menu align="right" panelClass="min-w-[12rem]">
    {#snippet trigger({ open })}
      <!-- bg-white/10 in the hover state can't go through class:
           directive (Tailwind slash-syntax fights Svelte's parser);
           build the class string in JS the same way triggerClass()
           does for the File/Edit/About menus above. -->
      {@const cls = (() => {
        const base = 'inline-flex h-7 items-center gap-1 rounded px-2 hover:bg-white/10';
        if (open) return `${base} bg-white/10`;
        if (toolsActive) return `${base} bg-accent text-on-accent`;
        return base;
      })()}
      <span
        class={cls}
        title={t('viewer_menu.tools')}
        aria-label={t('viewer_menu.tools')}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <!-- Toolbox -->
          <path d="M3 9h18v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1z" />
          <path d="M8 9V6a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v3" />
        </svg>
        <span class="hidden text-xs md:inline">{t('viewer_menu.tools')}</span>
      </span>
    {/snippet}
    <!-- Review mode item -->
    <button
      type="button"
      role="menuitem"
      onclick={onToggleReview}
      class="flex w-full items-center justify-between px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
    >
      <span class="inline-flex items-center gap-2">
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="11" cy="11" r="7" />
          <path d="m21 21-4.3-4.3" />
        </svg>
        {t('viewer_menu.review')}
      </span>
      {#if reviewMode}
        <span class="text-accent">●</span>
      {/if}
    </button>
    <!-- Whiteboard item — disabled when the host hasn't wired one
         (e.g. the standalone /assets/[id] route has no post to bind
         a whiteboard to). -->
    {#if onToggleWhiteboard}
      <button
        type="button"
        role="menuitem"
        onclick={onToggleWhiteboard}
        class="flex w-full items-center justify-between px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
      >
        <span class="inline-flex items-center gap-2">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <!-- Presentation board -->
            <rect x="2" y="3" width="20" height="14" rx="2" />
            <line x1="8" y1="21" x2="16" y2="21" />
            <line x1="12" y1="17" x2="12" y2="21" />
          </svg>
          {t('viewer_menu.whiteboard')}
        </span>
        {#if whiteboardOpen}
          <span class="text-accent">●</span>
        {/if}
      </button>
    {:else}
      <button
        type="button"
        role="menuitem"
        disabled
        class="flex w-full cursor-not-allowed items-center justify-between px-3 py-1.5 text-left text-sm text-fg-muted opacity-60"
        title={t('viewer_menu.coming_soon')}
      >
        <span class="inline-flex items-center gap-2">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="2" y="3" width="20" height="14" rx="2" />
            <line x1="8" y1="21" x2="16" y2="21" />
            <line x1="12" y1="17" x2="12" y2="21" />
          </svg>
          {t('viewer_menu.whiteboard')}
        </span>
      </button>
    {/if}
    <!-- Reserved slots for the next phases — disabled placeholders so
         the menu shape is set and the user sees what's coming. -->
    <div class="my-1 h-px bg-border"></div>
    <button
      type="button"
      role="menuitem"
      disabled
      class="block w-full cursor-not-allowed px-3 py-1.5 text-left text-sm text-fg-muted opacity-60"
      title={t('viewer_menu.coming_soon')}
    >
      {t('viewer_menu.annotate')}
    </button>
    <button
      type="button"
      role="menuitem"
      disabled
      class="block w-full cursor-not-allowed px-3 py-1.5 text-left text-sm text-fg-muted opacity-60"
      title={t('viewer_menu.coming_soon')}
    >
      {t('viewer_menu.compare')}
    </button>
  </Menu>

  <!-- Quick-action icon row -->
  <button
    type="button"
    onclick={onResetView}
    class="rounded p-1.5 hover:bg-white/10"
    title={t('viewer_menu.reset_view')}
    aria-label={t('viewer_menu.reset_view')}
  >
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width="14"
      height="14"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <polyline points="1 4 1 10 7 10" />
      <path d="M3.51 15a9 9 0 1 0 2.13-9.36L1 10" />
    </svg>
  </button>
  <button
    type="button"
    onclick={onToggleFullscreen}
    class="rounded p-1.5 hover:bg-white/10"
    title={isFullscreen ? t('viewer_menu.fullscreen_exit') : t('viewer_menu.fullscreen_enter')}
    aria-label={isFullscreen ? t('viewer_menu.fullscreen_exit') : t('viewer_menu.fullscreen_enter')}
  >
    {#if isFullscreen}
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="14"
        height="14"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <path d="M8 3v3a2 2 0 0 1-2 2H3" />
        <path d="M21 8h-3a2 2 0 0 1-2-2V3" />
        <path d="M3 16h3a2 2 0 0 1 2 2v3" />
        <path d="M16 21v-3a2 2 0 0 1 2-2h3" />
      </svg>
    {:else}
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="14"
        height="14"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <path d="M3 7V3h4" />
        <path d="M21 7V3h-4" />
        <path d="M3 17v4h4" />
        <path d="M21 17v4h-4" />
      </svg>
    {/if}
  </button>
  {#if paneEnabled}
    <button
      type="button"
      onclick={onTogglePane}
      class="rounded p-1.5 hover:bg-white/10"
      title={paneCollapsed ? t('viewer_menu.pane_show') : t('viewer_menu.pane_hide')}
      aria-label={paneCollapsed ? t('viewer_menu.pane_show') : t('viewer_menu.pane_hide')}
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="14"
        height="14"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <rect x="3" y="3" width="18" height="18" rx="2" />
        <line x1="15" y1="3" x2="15" y2="21" />
      </svg>
    </button>
  {/if}

  <!-- Window controls — maximize / restore + close. Mirrors the
       title-bar idiom of every modern desktop app: rightmost icons
       are reserved for the window itself, not the document. The
       maximize icon is two stacked squares when windowed (= "fill")
       and a single inset square when maximized (= "restore"). -->
  {#if onToggleMaximize}
    <button
      type="button"
      onclick={onToggleMaximize}
      class="rounded p-1.5 hover:bg-white/10"
      title={maximized ? t('viewer_menu.window_restore') : t('viewer_menu.window_maximize')}
      aria-label={maximized ? t('viewer_menu.window_restore') : t('viewer_menu.window_maximize')}
    >
      {#if maximized}
        <!-- Restore: two overlapping squares (front + offset back). -->
        <svg
          xmlns="http://www.w3.org/2000/svg"
          width="14"
          height="14"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <rect x="8" y="3" width="13" height="13" rx="1" />
          <path d="M3 8h2v13h13v-2" />
        </svg>
      {:else}
        <!-- Maximize: single square. -->
        <svg
          xmlns="http://www.w3.org/2000/svg"
          width="14"
          height="14"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <rect x="3" y="3" width="18" height="18" rx="1" />
        </svg>
      {/if}
    </button>
  {/if}
  {#if onClose}
    <button
      type="button"
      onclick={onClose}
      class="rounded p-1.5 hover:bg-danger hover:text-white"
      title={t('viewer_menu.window_close')}
      aria-label={t('viewer_menu.window_close')}
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="14"
        height="14"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <line x1="18" y1="6" x2="6" y2="18" />
        <line x1="6" y1="6" x2="18" y2="18" />
      </svg>
    </button>
  {/if}
</div>
