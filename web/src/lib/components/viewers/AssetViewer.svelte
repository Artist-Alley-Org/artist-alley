<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Universal asset viewer — one shell for image / video / PDF /
  // audio / 3D / sequence. The shell owns chrome (HUD, scrubber,
  // transport bar, fullscreen, hotkeys, jump-to-frame, pan + zoom)
  // and delegates the actual surface to a per-kind <ViewBody>
  // component that writes into a shared ViewController.
  //
  // Why this shape: when annotations (1.18.B-6) and presentation
  // rooms (1.18.B-5) land, they wire to the shell ONCE; every kind
  // of asset gets them for free.

  import { onMount, onDestroy, type Snippet } from 'svelte';
  import type { ViewController } from './controller';
  import { defaultController, kindForAsset } from './controller';
  import ImageView from './ImageView.svelte';
  import MediaView from './MediaView.svelte';
  import ModelView from './ModelView.svelte';
  import PDFView from './PDFView.svelte';
  import FontView from './FontView.svelte';
  import EpubView from './EpubView.svelte';
  import DocView from './DocView.svelte';
  import AudiobookView from './AudiobookView.svelte';
  import ArchiveView from './ArchiveView.svelte';
  import SpriteCanvas from './SpriteCanvas.svelte';
  import { createSpriteSession, type SpriteSessionInstance } from '$lib/sprite/session.svelte';
  import { createEbookSession, type EbookSessionInstance } from '$lib/ebook/session.svelte';
  import { createModelSession, type ModelSessionInstance } from '$lib/3d/session.svelte';
  import { createDocSession, type DocSessionInstance } from '$lib/doc/session.svelte';
  import { createAudiobookSession, type AudiobookSessionInstance } from '$lib/audiobook/session.svelte';
  import { createArchiveSession, type ArchiveSessionInstance } from '$lib/archive/session.svelte';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';
  import PlaceholderView from './PlaceholderView.svelte';
  import ViewerMenuBar from './ViewerMenuBar.svelte';
  import ToolPanelShell from './ToolPanelShell.svelte';
  import Img2ImgPopover from './Img2ImgPopover.svelte';
  import { TOOLS } from './tools/registry';
  import type { ToolContext, ToolDef } from './tools/contract';

  // Native + three-loader 3D paths we ship today. Other 3D
  // extensions (mview, blend, mb, ma, max, usd*) fall through to
  // the placeholder until 1.18.B-10/11 ship dedicated paths.
  // 'mview' is here too — ModelView routes that one to marmoset.js
  // instead of the three.js stack.
  const SUPPORTED_3D = new Set(['glb', 'gltf', 'fbx', 'obj', 'mview']);

  // Shared shape lives in controller.ts so every view body sees
  // the exact same Asset and TS can prop-bind without drift.
  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    /** True when this viewer is the focused slide. Hotkeys + autoplay
        only fire for the focused viewer so two carousel slides don't
        fight for the keyboard. */
    active?: boolean;
    /** Centered title bar content. Threaded to ViewerMenuBar to
        replace the default filename strip in the title bar (post
        hosts use this for "title — by author"). */
    titleSlot?: Snippet;
    /** Extra Tips rows rendered below the active tool's Tips in
        the shell footer. Hosts use this for nav shortcuts (A/D
        within-playlist, ←/→ sibling-nav) that aren't owned by any
        specific tool. */
    extraTips?: Snippet;
    /** Host-injected tools that merge into the registry at shell
        mount. Hosts that own richer surfaces (PostHost has post
        details with likes / comments / cover-picker) register
        their own ToolDef with the appropriate order. The built-in
        Details tool stays in the dropdown alongside — the user
        picks which surface they want from the dropdown. */
    customTools?: ToolDef[];
    /** Whiteboard session — wired by hosts that mount a whiteboard
        surface (post-anchored today; any asset eventually). Passed
        through into the ToolContext so the WhiteboardTool body
        picks it up. */
    whiteboardSession?: WhiteboardSession;
    /** Host hook bag forwarded to every tool via ToolContext.
        Conventions:
          - `hostHooks.whiteboard`: { saving, saveError, onSave,
            onClose, compact, onToggleCompact } consumed by
            WhiteboardTool.
          - `hostHooks.details`: free-form host-specific surface
            for custom DetailsTool bodies.
        Other tools that want host integration claim their own
        namespace key. */
    hostHooks?: Record<string, unknown>;
    /** Overlay rendered ABOVE the asset canvas (between the asset
        and the annotation layer) so tool surfaces — whiteboard,
        annotations — can paint over the asset without hiding the
        sidebar or top toolbar. The host gets a positioned canvas
        area to fill. Rendered inside the same container the asset
        sits in; absolute-position it to `inset-0` to take the
        whole canvas region. */
    canvasOverlay?: Snippet;
    /** Window-chrome state. When true the host's dialog covers the
        whole viewport; when false it sits inside the host's chrome.
        The shell owns the dialog mode — the viewer just renders the
        restore / maximize icon and fires onToggleMaximize. */
    maximized?: boolean;
    onToggleMaximize?: () => void;
    /** Bindable pane open/closed state so the host can persist the
        user's preference in its own localStorage. Default open. */
    paneCollapsed?: boolean;
    /** Bindable compact-rail state for tools that opt in
        (Whiteboard). Shell shrinks the aside to ~3.5rem and the
        tool body switches to its icon-rail layout. */
    paneCompact?: boolean;
    /** Optional close handler. When set, ViewerMenuBar shows a close
        button in the window-controls zone *and* a "Close" entry in
        the File menu. Hosts that own their own close affordance
        (a fullscreen non-dialog page, e.g.) omit this. */
    onClose?: () => void;
    /** Optional per-asset action hooks. Each enables the
        corresponding ViewerMenuBar item; without a hook the item
        stays disabled / hidden depending on the menu. Host typically
        opens its own modal (CollectionPicker / TagsEditor / etc.)
        for the assetId the cursor is on. */
    onAddToCollection?: () => void;
    onRecreatePreviews?: () => void;
    onEditTags?: () => void;
    onEditMetadata?: () => void;
    onDownloadVariant?: () => void;
    onShareAsset?: () => void;
    onDeleteAsset?: () => void;
  }

  let {
    asset,
    active = true,
    titleSlot,
    extraTips,
    customTools = [],
    whiteboardSession,
    hostHooks,
    canvasOverlay,
    maximized = false,
    onToggleMaximize,
    paneCollapsed = $bindable(false),
    paneCompact = $bindable(false),
    onClose,
    onAddToCollection,
    onRecreatePreviews,
    onEditTags,
    onEditMetadata,
    onDownloadVariant,
    onShareAsset,
    onDeleteAsset,
  }: Props = $props();

  // Derived: is whiteboard mode currently active? Source-of-truth
  // is the session existing. The selection-orchestration effect
  // below uses this to decide whether to fire onActivate / onClose.
  const whiteboardOpen = $derived(!!whiteboardSession);

  // User-driven "treat this as a sprite" override. Lets the user
  // open SpriteCanvas's slicer + playback tools on any raster
  // image, not just assets explicitly classified as Sprite. Resets
  // when the mounted asset changes — the override is per-view-
  // session, not persisted on the asset.
  let spriteOverride = $state(false);
  let lastAssetIdForSprite = '';
  $effect(() => {
    if (asset.id !== lastAssetIdForSprite) {
      lastAssetIdForSprite = asset.id;
      spriteOverride = false;
    }
  });
  const detectedKind = $derived(kindForAsset(asset));
  const kind = $derived(spriteOverride && detectedKind === 'image' ? 'sprite' : detectedKind);

  // The pane shows whenever the registry + host-injected tools
  // have at least one available entry for this asset. Built-in
  // tools (Details) are always available, so the pane shows for
  // every non-placeholder kind. The shell handles the collapse-
  // to-rail state internally.
  // Side panel always shows — Details tool is unconditionally
  // available, so even placeholder-kind assets (formats with no
  // viewer body, formats whose preview pipeline hasn't run yet)
  // get the metadata + download link surface. Hiding the panel
  // left those assets with no chrome to act on.
  const paneEnabled = true;
  // Auto-expand once when a sprite or 3D kind comes into view so
  // the user sees the dedicated tool immediately. Only force-open
  // on the transition INTO a tools kind; the user can re-collapse.
  const kindHasRichTools = $derived(kind === 'sprite' || kind === '3d' || kind === 'doc' || kind === 'ebook' || kind === 'audiobook' || kind === 'archive');
  let hadRichToolsKind = false;
  $effect(() => {
    if (kindHasRichTools && !hadRichToolsKind && paneCollapsed) {
      paneCollapsed = false;
    }
    hadRichToolsKind = kindHasRichTools;
  });
  const paneOpen = $derived(paneEnabled && !paneCollapsed);

  function togglePane() {
    paneCollapsed = !paneCollapsed;
  }
  // A fresh sprite session per mounted asset. Both SpriteCanvas
  // (in the canvas area) and SpriteToolPanel (in the outer right
  // pane) read + write through this same object — change cell
  // width in the panel and the canvas re-slices instantly. The
  // session is rebuilt whenever the asset changes so navigating
  // between sprites doesn't bleed slicer / playhead state.
  let spriteSession = $state<SpriteSessionInstance | null>(null);
  let lastAssetIdForSession = '';
  $effect(() => {
    if (kind === 'sprite' && asset.id !== lastAssetIdForSession) {
      lastAssetIdForSession = asset.id;
      spriteSession = createSpriteSession({ assetId: asset.id });
    } else if (kind !== 'sprite' && spriteSession) {
      spriteSession = null;
      lastAssetIdForSession = '';
    }
  });

  // Ebook session — same per-asset rebuild pattern. EpubView reads
  // / writes currentIdx + chapter state; the EbookTool's side-panel
  // body binds the same instance for TOC / search / bookmarks /
  // reading settings.
  let ebookSession = $state<EbookSessionInstance | null>(null);
  let lastAssetIdForEbook = '';
  $effect(() => {
    if (kind === 'ebook' && asset.id !== lastAssetIdForEbook) {
      lastAssetIdForEbook = asset.id;
      ebookSession = createEbookSession({ assetId: asset.id });
    } else if (kind !== 'ebook' && ebookSession) {
      ebookSession = null;
      lastAssetIdForEbook = '';
    }
  });

  // Model session — same per-asset rebuild pattern. ModelView reads
  // / writes camera + env + lighting state; the ModelTool side panel
  // binds the same instance for the rich 3D viewer surface.
  let modelSession = $state<ModelSessionInstance | null>(null);
  let lastAssetIdForModel = '';
  $effect(() => {
    if (kind === '3d' && asset.id !== lastAssetIdForModel) {
      lastAssetIdForModel = asset.id;
      modelSession = createModelSession({ assetId: asset.id });
    } else if (kind !== '3d' && modelSession) {
      modelSession = null;
      lastAssetIdForModel = '';
    }
  });

  // Doc session — same per-asset rebuild for txt / md / code files.
  // DocView reads + writes through it for CodeMirror state; the
  // DocTool side panel binds the same instance for reading prefs /
  // outline / find / bookmarks / stats.
  let docSession = $state<DocSessionInstance | null>(null);
  let lastAssetIdForDoc = '';
  $effect(() => {
    if (kind === 'doc' && asset.id !== lastAssetIdForDoc) {
      lastAssetIdForDoc = asset.id;
      docSession = createDocSession({ assetId: asset.id });
    } else if (kind !== 'doc' && docSession) {
      docSession = null;
      lastAssetIdForDoc = '';
    }
  });

  // Audiobook session — per-asset rebuild for .m4b / asset_type=11.
  // AudiobookView reads + writes through it for playback state;
  // the AudiobookTool side panel binds the same instance for the
  // chapter list / speed / sleep timer / bookmarks.
  let audiobookSession = $state<AudiobookSessionInstance | null>(null);
  let lastAssetIdForAudiobook = '';
  $effect(() => {
    if (kind === 'audiobook' && asset.id !== lastAssetIdForAudiobook) {
      lastAssetIdForAudiobook = asset.id;
      audiobookSession = createAudiobookSession({ assetId: asset.id });
    } else if (kind !== 'audiobook' && audiobookSession) {
      audiobookSession = null;
      lastAssetIdForAudiobook = '';
    }
  });

  // Archive session — per-asset rebuild for .zip / .tar / .tar.gz
  // and the rest of the archive family. ArchiveView renders the
  // file tree + entry preview; the ArchiveTool side panel reads
  // the same instance for Stats / filter helpers.
  let archiveSession = $state<ArchiveSessionInstance | null>(null);
  let lastAssetIdForArchive = '';
  $effect(() => {
    if (kind === 'archive' && asset.id !== lastAssetIdForArchive) {
      lastAssetIdForArchive = asset.id;
      archiveSession = createArchiveSession({ assetId: asset.id });
    } else if (kind !== 'archive' && archiveSession) {
      archiveSession = null;
      lastAssetIdForArchive = '';
    }
  });
  // The Tools-menu "Slice as sprite" entry only makes sense for
  // PNGs. Sprite sheets in the wild are essentially all PNG (lossless
  // + alpha); JPG/WEBP/etc. images may be photos / illustrations
  // where treating them as a sliced grid would be nonsense. Locking
  // to .png keeps the menu item out of users' way except where it
  // actually applies.
  const canOverrideToSprite = $derived(
    detectedKind === 'image'
    && (asset.file_extension?.toLowerCase().replace(/^\./, '') === 'png'),
  );
  let controller = $state(defaultController());

  // ---- pan + zoom (shell-owned) -----------------------------------------

  let canvasEl: HTMLDivElement | undefined = $state();
  let containerEl: HTMLDivElement | undefined = $state();
  let zoom = $state(1);
  let panX = $state(0);
  let panY = $state(0);
  let dragging = $state(false);

  function onCanvasWheel(e: WheelEvent) {
    // 3D bodies own all input (orbit controls handle wheel-as-dolly
    // natively — the camera moves toward the model rather than the
    // viewport canvas scaling, which is the per-kind "pan/zoom on the
    // object, not the viewer" semantics 3D needs). Skip our outer
    // transform entirely.
    if (kind === '3d') return;
    // Font view has its own scroll + size-slider UX. Wheel-zooming
    // the whole pane would fight the user's scroll through the
    // specimen page and the in-view size control. Let the inner
    // overflow-auto handle the wheel.
    if (kind === 'font') return;
    // Sprite view owns its own integer-step zoom + scroll for the
    // canvas; outer wheel-zoom would smush the pixel-perfect rendering.
    if (kind === 'sprite') return;
    // Doc view (CodeMirror) owns wheel/select for scroll + drag-to-
    // mark and Cmd+wheel-zoom comes from the editor's own keymap.
    // Skip the shell's outer transform entirely.
    if (kind === 'doc') return;
    // Archive view (file tree + entry preview) scrolls inside both
    // panes — outer wheel-zoom would fight it.
    if (kind === 'archive') return;
    // Timeline kinds (video, audio, paged PDF later) treat plain wheel
    // as one frame's worth of scrub — the muscle memory for review
    // work. Ctrl/⌘ + wheel still zooms so the user can inspect a
    // single frame without losing the scrub gesture.
    if (controller.hasTimeline && !e.ctrlKey && !e.metaKey) {
      e.preventDefault();
      controller.stepFrames(e.deltaY > 0 ? 1 : -1);
      return;
    }
    // Static kinds (image, font, PDF when we drop scroll-paged in a
    // later commit): wheel always zooms. No modifier required, no
    // "enter review mode first" gate — the user expects to inspect
    // the asset the moment it loads.
    e.preventDefault();
    const next = Math.max(1, Math.min(20, zoom * (e.deltaY > 0 ? 0.9 : 1.1)));
    if (canvasEl) {
      const rect = canvasEl.getBoundingClientRect();
      const cx = e.clientX - rect.left - rect.width / 2;
      const cy = e.clientY - rect.top - rect.height / 2;
      const factor = next / zoom;
      panX = cx - (cx - panX) * factor;
      panY = cy - (cy - panY) * factor;
    }
    zoom = next;
  }

  // Pending single-click toggle. We delay timeline-kind togglePlay()
  // by ~220ms so a quick second click resolves as a dblclick (review-
  // mode toggle) without also flipping play state. 220ms is the
  // browser-default dblclick interval on most platforms.
  let pendingClickTimer: ReturnType<typeof setTimeout> | undefined;

  function onCanvasMouseDown(e: MouseEvent) {
    if (e.button !== 0) return;
    // 3D bodies own all drag (orbit). Leave the outer transform alone.
    if (kind === '3d') return;
    // Font view is a scrollable specimen page, not a draggable
    // raster. Drag-to-pan would slide the whole page around like
    // an image, which reads as broken UX. Skip.
    if (kind === 'font') return;
    // Sprite view: scrollable canvas centred in the viewport;
    // drag-to-pan doesn't apply. Same reasoning as font.
    if (kind === 'sprite') return;
    // Doc view (CodeMirror) handles its own selection / drag-to-
    // select gestures. Outer pan/drag would fight the text-select
    // behaviour users expect from an editor surface.
    if (kind === 'doc') return;
    // Archive view owns its tree + preview pane click/drag.
    if (kind === 'archive') return;
    // Timeline kinds (video, audio) don't need pan/zoom — the wheel
    // already specialises to scrub frames, and the canvas surface is
    // a video frame or a waveform-with-cover, neither of which is a
    // scrollable / panable image. Dragging would slide the cover art
    // around like it's a static image, which reads as broken UX. We
    // still need the click-to-toggle-play gesture, so do that here
    // directly (no need to track movement when there's no pan to
    // arm); the 220 ms defer keeps double-click-to-review working.
    if (controller.hasTimeline) {
      pendingClickTimer = setTimeout(() => {
        controller.togglePlay();
        pendingClickTimer = undefined;
      }, 220);
      return;
    }
    const startX = e.clientX;
    const startY = e.clientY;
    const initialPanX = panX;
    const initialPanY = panY;
    dragging = false;
    const move = (mv: MouseEvent) => {
      const dx = mv.clientX - startX;
      const dy = mv.clientY - startY;
      if (!dragging && Math.hypot(dx, dy) > 4) dragging = true;
      if (dragging) {
        panX = initialPanX + dx;
        panY = initialPanY + dy;
      }
    };
    const up = () => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
      setTimeout(() => { dragging = false; }, 0);
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  }

  function resetView() {
    zoom = 1;
    panX = 0;
    panY = 0;
  }

  // ── Tile mode (image / texture only) ───────────────────────────
  // Two states: off (single pan-zoomable image, default) and
  // tile (repeat across the full canvas, both directions, so the
  // user can preview seamless tileability). Driven by the 't'
  // hotkey + a small menubar button. Persisted per-tab in
  // localStorage so navigating to the next texture keeps the
  // user's last preference — they probably want to review a
  // batch of textures the same way. Only the 'image' kind exposes
  // it; for other kinds the menubar button is hidden and the
  // hotkey is a no-op (the stored state still survives so the
  // next image asset picks it up).
  type TileMode = 'off' | 'tile';
  const TILE_KEY = 'aa.viewer.tileMode';
  let tileMode = $state<TileMode>('off');
  const canTile = $derived(kind === 'image');
  onMount(() => {
    try {
      if (localStorage.getItem(TILE_KEY) === 'tile') tileMode = 'tile';
    } catch { /* ignore */ }
  });
  $effect(() => {
    try {
      localStorage.setItem(TILE_KEY, tileMode);
    } catch { /* ignore */ }
  });
  function toggleTileMode() {
    if (!canTile) return;
    tileMode = tileMode === 'off' ? 'tile' : 'off';
  }

  // setZoom keeps the visual centre stable while changing scale —
  // same arithmetic the wheel handler uses, factored out so the
  // tools panel's zoom-preset buttons land consistently with the
  // wheel gesture (no jump-to-corner surprises).
  function setZoom(next: number) {
    const clamped = Math.max(0.05, Math.min(20, next));
    if (canvasEl) {
      const factor = clamped / zoom;
      panX = panX * factor;
      panY = panY * factor;
    }
    zoom = clamped;
  }

  // Zoom presets the Tools panel renders for any 2D kind. Fit is just
  // an alias for resetView (the canvas already centres + scales-to-fit
  // its contents at zoom=1 via the absolute inset-0 wrapper).
  const zoomPresets = [
    { label: 'Fit', factor: null as number | null },
    { label: '50%', factor: 0.5 },
    { label: '100%', factor: 1 },
    { label: '200%', factor: 2 },
    { label: '400%', factor: 4 },
  ];

  // ToolPanelShell consumes this. Built fresh on every reactive
  // change so the shell's $derived(isAvailable) fires when sessions
  // come / go (sprite spin-up, whiteboard open). zoomPresets is
  // declared above so this $derived can reference it without
  // tripping TDZ during initial evaluation.
  const toolCtx = $derived<ToolContext>({
    asset,
    controller,
    spriteSession: spriteSession ?? undefined,
    ebookSession: ebookSession ?? undefined,
    modelSession: modelSession ?? undefined,
    docSession: docSession ?? undefined,
    audiobookSession: audiobookSession ?? undefined,
    archiveSession: archiveSession ?? undefined,
    whiteboardSession,
    hostHooks,
    shellState: {
      zoom,
      setZoom,
      resetView,
      zoomPresets,
      // paneCompact is a placeholder here — the shell overrides
      // it with the resolved (and tool-gated) value before
      // mounting the active Body so tools don't have to know
      // about supportsCompact.
      paneCompact: false,
    },
  });
  // Built-in registry + host-injected tools. Hosts REPLACE built-in
  // tools by id (PostHost overrides Details so the body renders
  // post info instead of the generic asset-info stub). Unmatched
  // host tools simply append.
  const mergedTools = $derived.by<ToolDef[]>(() => {
    const customIds = new Set(customTools.map((t) => t.id));
    return [...TOOLS.filter((t) => !customIds.has(t.id)), ...customTools];
  });
  // Filter to what's available for the current asset, sorted in
  // dropdown order. Shared by the menubar (picker items) + the
  // shell (Body / Tips mounter) so both ends agree on what the
  // user can switch to.
  const availableTools = $derived(
    mergedTools.filter((t) => t.isAvailable(toolCtx)).sort((a, b) => a.order - b.order),
  );
  // Active tool id — persisted per-tab in localStorage and shared
  // with the menubar's Tools menu. Auto-falls-back when the
  // persisted id isn't valid for the current asset (shell does
  // the write-back).
  const ACTIVE_TOOL_KEY = 'aa.viewer.activeTool';
  let activeToolId = $state<string>('details');
  onMount(() => {
    try {
      const stored = localStorage.getItem(ACTIVE_TOOL_KEY);
      if (stored) activeToolId = stored;
    } catch { /* ignore — private browsing */ }
  });
  $effect(() => {
    try {
      localStorage.setItem(ACTIVE_TOOL_KEY, activeToolId);
    } catch { /* ignore */ }
  });
  function selectTool(id: string) {
    activeToolId = id;
  }
  // Resolved label for the active tool — labelFn overrides label
  // when present so the menubar trigger ("Tools • Details" / "Tools
  // • Sprite Viewer" / etc.) reflects what the panel header shows.
  const activeToolLabel = $derived.by(() => {
    const t = availableTools.find((x) => x.id === activeToolId);
    if (!t) return '';
    return t.labelFn ? t.labelFn(toolCtx) : t.label;
  });

  // Tool-selection orchestration. Two tools need side effects on
  // becoming active (or going inactive):
  //
  //   Sprite Viewer — flips spriteOverride so the canvas re-mounts
  //   as SpriteCanvas and a session spawns. Reverts on deselection
  //   so picking Details / Whiteboard restores the original kind.
  //
  //   Whiteboard — calls the host's onActivate hook to open the
  //   canvas overlay + create the WhiteboardSession. Calls onClose
  //   on deselection so picking Details / Sprite Viewer also exits
  //   whiteboard mode.
  //
  // Effect reads activeToolId reactively + the previous value via a
  // closure so we only fire on transitions, not on every reactive
  // tick.
  let lastActiveTool: string | null = null;
  $effect(() => {
    const next = activeToolId;
    const prev = lastActiveTool;
    if (next === prev) return;
    lastActiveTool = next;
    // Sprite Viewer transitions
    if (next === 'sprite' && detectedKind === 'image' && !spriteOverride) {
      spriteOverride = true;
    } else if (prev === 'sprite' && spriteOverride) {
      spriteOverride = false;
    }
    // Whiteboard transitions
    const wb = hostHooks?.whiteboard as { onActivate?: () => void; onClose?: () => void } | undefined;
    if (next === 'whiteboard' && wb?.onActivate && !whiteboardOpen) {
      wb.onActivate();
    } else if (prev === 'whiteboard' && wb?.onClose && whiteboardOpen) {
      wb.onClose();
    }
  });

  // Canvas double-click as a review-mode toggle was retired —
  // users kept landing on it accidentally (panel swap mid-scroll,
  // sprite-slice override flipped under them, etc.) and the
  // Tools-menu "Review" entry plus its hotkey already cover the
  // gesture. The handler binding below is left wired so timeline
  // kinds can still cancel their pending-click before a dblclick
  // resolves as something else; it no longer touches review mode.
  function onCanvasDoubleClick(e: MouseEvent) {
    if (kind === '3d') return;
    if (pendingClickTimer !== undefined) {
      clearTimeout(pendingClickTimer);
      pendingClickTimer = undefined;
    }
    e.preventDefault();
  }

  // ---- fullscreen --------------------------------------------------------

  let isFullscreen = $state(false);
  function toggleFullscreen() {
    if (!containerEl) return;
    if (!document.fullscreenElement) {
      void containerEl.requestFullscreen?.();
    } else {
      void document.exitFullscreen?.();
    }
  }
  function onFullscreenChange() {
    isFullscreen = !!document.fullscreenElement;
  }

  // ---- scrubber (sprite preview) ----------------------------------------

  interface SpriteCue { start: number; end: number; src: string; x: number; y: number; w: number; h: number; }
  let sprites = $state<SpriteCue[]>([]);
  let hoverSprite = $state<SpriteCue | null>(null);
  let hoverLeftPx = $state(0);
  let hoverTime = $state(0);
  let scrubberHovering = $state(false);
  let scrubberEl: HTMLDivElement | undefined = $state();

  $effect(() => {
    if (controller.spritesVttUrl) {
      void loadSprites(controller.spritesVttUrl, controller.spritesUrl ?? '');
    } else {
      sprites = [];
    }
  });

  async function loadSprites(vttUrl: string, baseHref: string) {
    try {
      const r = await fetch(vttUrl, { credentials: 'include' });
      if (!r.ok) { sprites = []; return; }
      sprites = parseVTTSprites(await r.text(), baseHref);
    } catch {
      sprites = [];
    }
  }

  function parseVTTSprites(vtt: string, baseHref: string): SpriteCue[] {
    const out: SpriteCue[] = [];
    const lines = vtt.split(/\r?\n/);
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      const m = line.match(/(\d+:\d+:\d+\.\d+)\s+-->\s+(\d+:\d+:\d+\.\d+)/);
      if (!m) continue;
      const start = parseVTTTime(m[1]);
      const end = parseVTTTime(m[2]);
      const xy = (lines[i + 1] || '').match(/#xywh=(\d+),(\d+),(\d+),(\d+)/);
      if (!xy) continue;
      out.push({ start, end, src: baseHref, x: +xy[1], y: +xy[2], w: +xy[3], h: +xy[4] });
    }
    return out;
  }
  function parseVTTTime(s: string): number {
    const [h, m, rest] = s.split(':');
    return +h * 3600 + +m * 60 + parseFloat(rest);
  }

  function clamp(n: number, lo: number, hi: number) { return Math.max(lo, Math.min(hi, n)); }

  function onScrubberMove(e: MouseEvent) {
    if (!scrubberEl) return;
    const rect = scrubberEl.getBoundingClientRect();
    const pct = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    // Pixel offset from the scrubber RAIL's left edge, not a percentage of
    // the zoomed inner track. The tooltip lives outside the scroll
    // container (it has to — see the markup), so a percentage would drift
    // from the pointer as soon as the rail is zoomed and scrolled.
    hoverLeftPx = e.clientX - (scrubberScrollEl?.getBoundingClientRect().left ?? rect.left);
    hoverTime = pct * controller.duration;
    hoverSprite = sprites.find((c) => hoverTime >= c.start && hoverTime < c.end) ?? null;
  }
  function onScrubberLeave() { scrubberHovering = false; hoverSprite = null; }
  function onScrubberClick(e: MouseEvent) {
    if (!scrubberEl) return;
    const rect = scrubberEl.getBoundingClientRect();
    const pct = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    controller.seekToFrame(Math.round(pct * controller.totalFrames));
  }

  // ---- loop region ------------------------------------------------------

  // Loop state lives on the controller now so view bodies (MediaView's
  // waveform with shift-drag region) can read + write the same range
  // the shell's transport bar shows. Local aliases keep the existing
  // template + hotkey + button bindings concise.
  const loopIn = $derived(controller.loopIn);
  const loopOut = $derived(controller.loopOut);

  // Scrubber zoom — same idiom as MediaView's waveform zoom, but on
  // the shell's narrow timeline bar so video gets the zoom-into-a-
  // section affordance too. Ctrl/Cmd + wheel on the scrubber zooms;
  // bare wheel falls through to the canvas wheel handler (which
  // step-scrubs one frame for timeline kinds). Container becomes
  // overflow-x-auto when zoomed > 1×; an auto-scroll keeps the
  // playhead in the visible band.
  let scrubberScrollEl: HTMLDivElement | undefined = $state();
  let scrubberZoom = $state(1);
  const SCRUBBER_ZOOM_MIN = 1;
  const SCRUBBER_ZOOM_MAX = 16;

  function onScrubberWheel(e: WheelEvent) {
    if (!(e.ctrlKey || e.metaKey)) return;
    e.preventDefault();
    e.stopPropagation();
    if (!scrubberScrollEl) return;
    const inner = e.currentTarget as HTMLElement;
    const innerRect = inner.getBoundingClientRect();
    const pointerRatio = Math.max(0, Math.min(1,
      (e.clientX - innerRect.left) / innerRect.width));
    const prev = scrubberZoom;
    const next = Math.max(SCRUBBER_ZOOM_MIN,
      Math.min(SCRUBBER_ZOOM_MAX,
        scrubberZoom * (e.deltaY > 0 ? 0.8 : 1.25)));
    scrubberZoom = next;
    if (prev === next) return;
    requestAnimationFrame(() => {
      if (!scrubberScrollEl) return;
      const containerW = scrubberScrollEl.clientWidth;
      const newInnerW = containerW * next;
      const pointerLocal = e.clientX - scrubberScrollEl.getBoundingClientRect().left;
      scrubberScrollEl.scrollLeft = (pointerRatio * newInnerW) - pointerLocal;
    });
  }

  $effect(() => {
    if (!scrubberScrollEl || scrubberZoom <= 1) return;
    const viewportW = scrubberScrollEl.clientWidth;
    const innerW = viewportW * scrubberZoom;
    const playheadX = (playheadPct / 100) * innerW;
    const left = scrubberScrollEl.scrollLeft;
    const right = left + viewportW;
    const margin = viewportW * 0.15;
    if (playheadX < left + margin) {
      scrubberScrollEl.scrollLeft = Math.max(0, playheadX - margin);
    } else if (playheadX > right - margin) {
      scrubberScrollEl.scrollLeft = Math.min(innerW - viewportW, playheadX - viewportW + margin);
    }
  });

  function resetScrubberZoom() {
    scrubberZoom = 1;
    if (scrubberScrollEl) scrubberScrollEl.scrollLeft = 0;
  }

  function enforceLoop() {
    if (!controller.hasTimeline) return;
    if (controller.loopIn === null || controller.loopOut === null || controller.loopOut <= controller.loopIn) return;
    if (controller.currentFrame > controller.loopOut) controller.seekToFrame(controller.loopIn);
  }
  $effect(enforceLoop);

  // ---- jump-to-frame ----------------------------------------------------

  let goToOpen = $state(false);
  let goToValue = $state('');
  function commitGoTo() {
    const s = goToValue.trim();
    if (!s) { goToOpen = false; return; }
    let frame = NaN;
    const tcM = s.match(/^(?:(\d+):)?(?:(\d+):)?(\d+)[:.,](\d+)$/);
    const secM = s.match(/^(\d+(?:\.\d+)?)\s*s?$/);
    if (/^\d+$/.test(s)) {
      frame = parseInt(s, 10);
    } else if (tcM) {
      const h = parseInt(tcM[1] || '0', 10);
      const m = parseInt(tcM[2] || '0', 10);
      const sec = parseInt(tcM[3], 10);
      const f = parseInt(tcM[4], 10);
      const fpsR = Math.max(1, Math.round(controller.fps));
      frame = ((h * 3600 + m * 60 + sec) * fpsR) + f;
    } else if (secM) {
      const fpsR = Math.max(1, controller.fps || 1);
      frame = Math.round(parseFloat(secM[1]) * fpsR);
    }
    if (Number.isFinite(frame)) {
      controller.seekToFrame(clamp(Math.round(frame), 0, controller.totalFrames));
    }
    goToValue = '';
    goToOpen = false;
  }

  // ---- hotkeys ----------------------------------------------------------

  function handleKey(e: KeyboardEvent) {
    if (!active) return;
    if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    const k = e.key.toLowerCase();
    switch (k) {
      case ' ': if (controller.hasTimeline) { e.preventDefault(); controller.togglePlay(); } break;
      case 'k': if (controller.hasTimeline) { e.preventDefault(); controller.pause(); } break;
      case 'l': if (controller.hasTimeline) { e.preventDefault(); controller.play(); } break;
      case 'j': if (controller.hasTimeline) { e.preventDefault(); controller.stepFrames(-1); } break;
      case ',':
      case 'arrowleft': if (controller.hasTimeline) { e.preventDefault(); controller.stepFrames(e.shiftKey ? -10 : -1); } break;
      case '.':
      case 'arrowright': if (controller.hasTimeline) { e.preventDefault(); controller.stepFrames(e.shiftKey ? 10 : 1); } break;
      case 'i': if (controller.hasTimeline) { e.preventDefault(); controller.loopIn = controller.currentFrame; } break;
      case 'o': if (controller.hasTimeline) { e.preventDefault(); controller.loopOut = controller.currentFrame; } break;
      case 'backspace': e.preventDefault(); controller.loopIn = null; controller.loopOut = null; break;
      case '1': if (controller.hasTimeline) controller.setRate(0.25); break;
      case '2': if (controller.hasTimeline) controller.setRate(0.5); break;
      case '3': if (controller.hasTimeline) controller.setRate(1); break;
      case '4': if (controller.hasTimeline) controller.setRate(2); break;
      case '5': if (controller.hasTimeline) controller.setRate(4); break;
      case 'f': e.preventDefault(); toggleFullscreen(); break;
      case 'r': e.preventDefault(); resetView(); break;
      case 'g': if (controller.hasTimeline) { e.preventDefault(); goToOpen = true; } break;
      case 't': if (canTile) { e.preventDefault(); toggleTileMode(); } break;
    }
  }

  onMount(() => {
    document.addEventListener('keydown', handleKey);
    document.addEventListener('fullscreenchange', onFullscreenChange);
  });
  onDestroy(() => {
    document.removeEventListener('keydown', handleKey);
    document.removeEventListener('fullscreenchange', onFullscreenChange);
  });

  // Derived UI values
  const playheadPct = $derived(controller.totalFrames > 0 ? (controller.currentFrame / controller.totalFrames) * 100 : 0);
  const loopInPct = $derived(controller.totalFrames > 0 && loopIn !== null ? (loopIn / controller.totalFrames) * 100 : 0);
  const loopOutPct = $derived(controller.totalFrames > 0 && loopOut !== null ? (loopOut / controller.totalFrames) * 100 : 0);
  const playRateChips = [0.25, 0.5, 1, 2, 4];
</script>

<div bind:this={containerEl} class="flex h-full w-full flex-col bg-black text-white">
  <!-- Top toolbar — File / Edit / About menus + asset info strip +
       Review toggle + reset/fullscreen/pane quick-actions. Replaces
       the old floating top-right button column (which overlaid the
       asset and had no home for non-icon actions). -->
  <ViewerMenuBar
    {asset}
    {controller}
    {paneCollapsed}
    {paneEnabled}
    {isFullscreen}
    {titleSlot}
    {maximized}
    {onToggleMaximize}
    onResetView={resetView}
    onToggleFullscreen={toggleFullscreen}
    onTogglePane={togglePane}
    {onClose}
    {onAddToCollection}
    {onRecreatePreviews}
    {onEditTags}
    {onEditMetadata}
    {onDownloadVariant}
    {onShareAsset}
    {onDeleteAsset}
    sidePanelTools={availableTools}
    sidePanelToolCtx={toolCtx}
    sidePanelActiveTool={activeToolId}
    sidePanelActiveToolLabel={activeToolLabel}
    onSelectSidePanelTool={selectTool}
    {canTile}
    {tileMode}
    onToggleTileMode={toggleTileMode}
  />

  <!-- Canvas + pane row. The pane is a flex sibling so it pushes the
       canvas's width rather than overlaying part of it — otherwise the
       asset visually drifts off-center every time the user opens the
       pane. Width-animated slide-in keeps the smooth transition the
       overlay version had. -->
  <div class="flex min-h-0 flex-1">

  <!-- Canvas (pan + zoom transform wraps the view body) -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={canvasEl}
    class="relative min-w-0 flex-1 overflow-hidden bg-black"
    class:cursor-grabbing={dragging}
    class:cursor-grab={!dragging && zoom > 1}
    onwheel={onCanvasWheel}
    onmousedown={onCanvasMouseDown}
    ondblclick={onCanvasDoubleClick}
  >
    <!-- Font + Sprite views bypass the pan/zoom transform wrapper.
         Both own their own zoom semantics (the font has a size
         slider; the sprite uses integer-step pixel-perfect zoom),
         and both render scrollable bodies that want the full canvas
         area, not a centred raster behind the viewer's outer
         translate/scale. Each lives as an absolute sibling layer
         to the transform; on those kinds the transform layer is
         empty. -->
    {#if kind === 'font'}
      {#key asset.id}
        <div class="absolute inset-0">
          <FontView {asset} bind:controller />
        </div>
      {/key}
    {:else if kind === 'ebook' && ebookSession}
      <!-- Ebook reader bypasses pan/zoom — the body owns its own
           page-by-page layout (chapter iframe + TOC popdown).
           Session is shared with the side-panel EbookTool. -->
      {#key asset.id}
        <div class="absolute inset-0">
          <EpubView {asset} bind:controller bind:session={ebookSession} />
        </div>
      {/key}
    {:else if kind === 'doc' && docSession}
      <!-- Document viewer (CodeMirror) bypasses pan/zoom — the
           editor owns wheel/drag/select for find/replace etc.
           Session shared with the side-panel DocTool. -->
      {#key asset.id}
        <div class="absolute inset-0">
          <DocView {asset} bind:controller bind:session={docSession} />
        </div>
      {/key}
    {:else if kind === 'audiobook' && audiobookSession}
      <!-- Audiobook reader (large cover + chapter strip + big skip
           buttons) bypasses pan/zoom — the surface is its own
           Audiobookshelf-style chrome. Session shared with the
           side-panel AudiobookTool. -->
      {#key asset.id}
        <div class="absolute inset-0">
          <AudiobookView {asset} bind:controller bind:session={audiobookSession} />
        </div>
      {/key}
    {:else if kind === 'archive' && archiveSession}
      <!-- Archive browser (file tree + entry preview) bypasses
           pan/zoom — owns its own scrollable layout. Session shared
           with the side-panel ArchiveTool. -->
      {#key asset.id}
        <div class="absolute inset-0">
          <ArchiveView {asset} bind:controller bind:session={archiveSession} />
        </div>
      {/key}
    {:else if kind === 'sprite' && spriteSession}
      {#key asset.id}
        <div class="absolute inset-0">
          <SpriteCanvas {asset} bind:session={spriteSession} bind:controller />
        </div>
      {/key}
    {:else if kind === 'image' && tileMode !== 'off'}
      <!-- Tile mode bypasses the pan/zoom transform — a repeating
           texture wrapped in translate/scale would have edges
           flying around as the user panned. The tile fills the
           full canvas area so the user can preview seamless
           tileability at a real on-screen size. -->
      {#key asset.id}
        <div class="absolute inset-0">
          <ImageView {asset} bind:controller {tileMode} />
        </div>
      {/key}
    {:else}
    <div
      class="absolute inset-0 flex items-center justify-center"
      style="transform: translate({panX}px, {panY}px) scale({zoom}); transform-origin: center center;"
    >
      <!-- {#key} forces a fresh view-body mount when the asset id
           changes. Without this, the three.js viewer keeps the old
           scene loaded when a host swaps assets without unmounting
           the AssetViewer (the common multi-asset carousel pattern).
           Side-effect: pan/zoom resets per asset, which is what users
           expect when navigating to a new image anyway. -->
      {#key asset.id}
        {#if kind === 'video' || kind === 'audio'}
          <MediaView {asset} bind:controller />
        {:else if kind === 'image'}
          <ImageView {asset} bind:controller {tileMode} />
        {:else if kind === 'pdf'}
          <PDFView {asset} bind:controller />
        {:else if kind === '3d' && SUPPORTED_3D.has((asset.file_extension || '').toLowerCase().replace(/^\./, '')) && modelSession}
          <ModelView {asset} bind:controller bind:session={modelSession} />
        {:else}
          <PlaceholderView {asset} bind:controller />
        {/if}
      {/key}
    </div>
    {/if}

    <!-- Phase 1.14.E-1 — Generate variation (AI) trigger. Image
         assets only; the popover handles its own state, auth +
         server-not-configured errors surface in-popover. -->
    {#if kind === 'image'}
      <div class="pointer-events-auto absolute right-3 top-3 z-10">
        <Img2ImgPopover assetId={asset.id} />
      </div>
    {/if}

    <!-- HUD: live frame counter / zoom %. The static asset info
         (filename, dimensions, codec) is in the toolbar above; the
         HUD here only shows values that change as the user
         interacts — frame position on timeline kinds, zoom % when
         it's not 100%. Bottom-left so it doesn't fight the toolbar
         or the pane. -->
    {#if controller.hasTimeline || zoom !== 1}
      <div
        class="pointer-events-none absolute bottom-3 left-3 rounded bg-black/70 px-2 py-1 font-mono text-xs"
      >
        {#if controller.hasTimeline}
          {controller.formatAnchor(controller.currentFrame)}
          {#if controller.kind !== 'audio' && controller.kind !== 'audiobook'}
            <!-- Frame counter — only meaningful for video / pdf / etc.
                 Audio + audiobook run at 1000 fps (1 ms per "frame")
                 so "f95000" reads as noise; the M:SS.mmm timecode
                 already carries the precise position. -->
            · f{controller.currentFrame}{controller.totalFrames > 0 ? `/${controller.totalFrames}` : ''}
          {/if}
        {/if}
        {#if zoom !== 1}
          {controller.hasTimeline ? ' · ' : ''}{Math.round(zoom * 100)}%
        {/if}
      </div>
    {/if}

    <!-- Jump-to-frame quick-action — only visible on timeline kinds.
         Floated top-right of the canvas (under the toolbar). Reset
         and fullscreen used to live here too; they moved into the
         toolbar's quick-action group. -->
    {#if controller.hasTimeline}
      <button
        type="button"
        onclick={() => (goToOpen = !goToOpen)}
        class="absolute top-3 right-3 rounded bg-black/70 p-1.5 text-xs hover:bg-black/90"
        class:right-[25rem]={paneOpen}
        title="Jump to frame (G)"
        aria-label="Jump to frame"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
      </button>
    {/if}

    <!-- Jump-to-frame input -->
    {#if goToOpen}
      <div class="absolute left-1/2 top-12 -translate-x-1/2 rounded bg-black/85 p-2 text-xs shadow-xl">
        <form onsubmit={(e) => { e.preventDefault(); commitGoTo(); }} class="flex items-center gap-2">
          <span class="text-zinc-400">Go to</span>
          <input
            type="text"
            bind:value={goToValue}
            placeholder="frame, mm:ss, or 5.2s"
            class="w-44 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 font-mono text-xs text-white focus:border-accent focus:outline-none"
            autofocus
            onkeydown={(e) => { if (e.key === 'Escape') { goToOpen = false; goToValue = ''; } }}
          />
          <button type="submit" class="rounded bg-accent px-2 py-1 text-xs font-medium text-on-accent">Go</button>
        </form>
      </div>
    {/if}

    <!-- Annotation overlay layer (placeholder for Phase 1.18.B-6).
         Sized to the viewport for now; B-6 will narrow it to the
         asset's rendered rect (image bounds, video frame, etc.) so
         annotations land on content, not letterboxing. -->
    <div class="pointer-events-none absolute inset-0 z-20" data-role="annotation-layer"></div>

    <!-- Host-provided canvas overlay (whiteboard, annotations).
         Renders OVER the asset but BELOW the pane-re-open tab, so
         the sidebar's tool panel stays reachable. The host is
         responsible for the overlay's own positioning + pointer-
         event handling. -->
    {#if canvasOverlay}
      <div class="absolute inset-0 z-25">
        {@render canvasOverlay()}
      </div>
    {/if}

    {#if paneEnabled && paneCollapsed}
      <!-- Re-open tab on the right edge so the user can recover the
           pane after collapsing it. Lives inside the canvas (not the
           aside) because the aside collapses to width=0. -->
      <button
        type="button"
        onclick={togglePane}
        class="absolute right-0 top-1/2 z-30 -translate-y-1/2 rounded-l-md bg-black/60 px-2 py-3 text-white backdrop-blur-sm transition-colors hover:bg-black/80"
        aria-label="Show panel"
        title="Show panel (i)"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="m15 18-6-6 6-6" />
        </svg>
      </button>
    {/if}
  </div><!-- /canvas -->

    <!-- Right pane: flex sibling of the canvas so opening it shrinks
         the canvas (asset stays centred in the visible region rather
         than drifting behind an overlay). Width-animates between
         w-96 (open) and w-0 (collapsed) for the same smooth slide the
         old translate-based overlay gave us. -->
    {#if paneEnabled}
      <ToolPanelShell
        ctx={toolCtx}
        tools={mergedTools}
        bind:activeToolId
        bind:paneCollapsed
        bind:paneCompact
        onTogglePane={togglePane}
        {extraTips}
      />
    {/if}
  </div><!-- /canvas+pane row -->

  <!-- Transport rail (only when the body has a timeline). Wrapped in
       a horizontally-scrollable container so Ctrl/Cmd + wheel can
       zoom the scrubber into a section of the timeline — same idiom
       MediaView uses on its waveform. The reset chip appears only
       when zoomed > 1×.

       Audiobook routes the same rail too — AudiobookView delegates
       transport to the shell so users get one consistent set of
       controls across video / audio / audiobook. The view itself
       just renders cover + meta + chapter-strip context. -->
  {#if controller.hasTimeline}
    <!-- The rail wrapper exists so the hover thumbnail can escape the
         scroll container below it. `overflow-x-auto` (added with scrubber
         zoom) makes that container a clipping context on BOTH axes — CSS
         forces the other axis to `auto` when one is not `visible` — and it
         is `h-3`, so a 90-190px tall preview inside it was clipped to 12px
         and never appeared at all. -->
    <div class="relative">
    <div
      bind:this={scrubberScrollEl}
      class="relative h-3 overflow-x-auto overflow-y-hidden bg-zinc-900"
    >
      {#if scrubberZoom > 1}
        <button
          type="button"
          onclick={resetScrubberZoom}
          class="sticky left-2 top-0 z-30 -mt-0.5 inline-flex h-3 items-center gap-1 rounded-b bg-black/80 px-1.5 text-[10px] font-mono leading-none text-white/90 hover:bg-black"
          title="Reset scrubber zoom (Ctrl/Cmd + Wheel to zoom)"
        >
          {scrubberZoom.toFixed(1)}× — reset
        </button>
      {/if}
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <div
        bind:this={scrubberEl}
        class="relative h-full cursor-crosshair"
        style:width={`${100 * scrubberZoom}%`}
        onmouseenter={() => (scrubberHovering = true)}
        onmousemove={onScrubberMove}
        onmouseleave={onScrubberLeave}
        onwheel={onScrubberWheel}
        onclick={onScrubberClick}
        role="slider"
        aria-valuenow={controller.currentFrame}
        aria-valuemin={0}
        aria-valuemax={controller.totalFrames || 0}
        aria-label="Scrubber"
        tabindex="0"
      >
        <div class="absolute inset-y-0 left-0 bg-accent/60" style="width: {playheadPct}%"></div>
        {#if loopIn !== null && loopOut !== null && loopOut > loopIn}
          <div class="absolute inset-y-0 bg-yellow-500/30" style="left: {loopInPct}%; width: {loopOutPct - loopInPct}%"></div>
        {/if}
        <!-- Chapter ticks (audiobook only) — thin vertical lines at
             each chapter boundary so the user can visually orient
             "I'm 2/3 through chapter 4" without scrubbing. The
             dedicated AudiobookView used to ship its own scrubber
             with these ticks; we moved them onto the shell's rail
             so there's exactly one progress bar. -->
        {#if kind === 'audiobook' && audiobookSession && audiobookSession.chapters.length > 1 && audiobookSession.durationS > 0}
          {#each audiobookSession.chapters as ch, i (i)}
            {#if i > 0}
              <span
                class="pointer-events-none absolute inset-y-0 w-px bg-white/40"
                style="left: {(ch.start / audiobookSession.durationS) * 100}%"
              ></span>
            {/if}
          {/each}
        {/if}
        <!-- Bookmark diamonds (audiobook only). Click to jump back. -->
        {#if kind === 'audiobook' && audiobookSession && audiobookSession.durationS > 0}
          {#each audiobookSession.bookmarks as bm (bm.createdAt)}
            <button
              type="button"
              onclick={(ev) => { ev.stopPropagation(); audiobookSession?.seekTo?.(bm.time); }}
              class="absolute top-[-3px] h-[9px] w-[9px] -translate-x-1/2 rotate-45 cursor-pointer rounded-sm bg-yellow-400 shadow hover:scale-150"
              style="left: {(bm.time / audiobookSession.durationS) * 100}%"
              title={bm.note ? `${Math.round(bm.time)}s — ${bm.note}` : `Bookmark at ${Math.round(bm.time)}s`}
              aria-label="Jump to bookmark"
            ></button>
          {/each}
        {/if}
        <div class="absolute inset-y-0 w-px bg-white" style="left: {playheadPct}%"></div>
      </div>
    </div>
    {#if scrubberHovering && hoverSprite}
      <!-- Sized from the VTT's own `#xywh` rectangle, never from a
           hardcoded cell size — the sheet's cells take the source's
           aspect ratio (#761), so a portrait clip's preview is a
           portrait box, and the cell's pixel size is free to move (it
           went 160 -> 240 in #811 with no change here). -->
      <div class="pointer-events-none absolute bottom-4 z-30 -translate-x-1/2 rounded border border-zinc-700 bg-black p-1 shadow-xl" style="left: {hoverLeftPx}px">
        <div class="bg-zinc-950" style="width: {hoverSprite.w}px; height: {hoverSprite.h}px; background-image: url({hoverSprite.src}); background-position: -{hoverSprite.x}px -{hoverSprite.y}px;"></div>
        <div class="mt-1 text-center font-mono text-[10px]">{controller.formatAnchor(Math.round(hoverTime * controller.fps))}</div>
      </div>
    {/if}
    </div>
    <div class="flex items-center gap-3 border-t border-zinc-800 bg-zinc-950 px-3 py-2 text-sm">
      <button type="button" onclick={() => controller.stepFrames(-10)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="−10 (Shift+←)">⏮</button>
      <button type="button" onclick={() => controller.stepFrames(-1)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="Step back (,)">◀|</button>
      <button type="button" onclick={() => controller.togglePlay()} class="rounded bg-zinc-800 px-3 py-1 font-medium hover:bg-zinc-700" title="Play/Pause (K)">
        {controller.playing ? '⏸' : '▶'}
      </button>
      <button type="button" onclick={() => controller.stepFrames(1)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="Step fwd (.)">|▶</button>
      <button type="button" onclick={() => controller.stepFrames(10)} class="px-1.5 py-0.5 hover:bg-zinc-800" title="+10 (Shift+→)">⏭</button>
      <span class="mx-2 h-4 w-px bg-zinc-800"></span>
      {#each playRateChips as r}
        <button type="button" onclick={() => controller.setRate(r)} class="px-1.5 py-0.5 text-xs hover:bg-zinc-800" class:bg-zinc-800={controller.rate === r} title="Speed {r}×">{r}×</button>
      {/each}
      <span class="mx-2 h-4 w-px bg-zinc-800"></span>
      <button type="button" onclick={() => (controller.loopIn = controller.currentFrame)} class="px-1.5 py-0.5 text-xs hover:bg-zinc-800" title="Mark loop in (I)">
        Loop in {loopIn !== null ? `(f${loopIn})` : ''}
      </button>
      <button type="button" onclick={() => (controller.loopOut = controller.currentFrame)} class="px-1.5 py-0.5 text-xs hover:bg-zinc-800" title="Mark loop out (O)">
        Loop out {loopOut !== null ? `(f${loopOut})` : ''}
      </button>
      {#if loopIn !== null || loopOut !== null}
        <button type="button" onclick={() => { controller.loopIn = null; controller.loopOut = null; }} class="px-1.5 py-0.5 text-xs text-zinc-400 hover:text-white" title="Clear loop (⌫)">clear</button>
      {/if}
      <span class="ml-auto font-mono text-xs text-zinc-400">
        JKL · ⇧← → · I/O loop · 1-5 speed · G goto · F fullscreen · ⌘wheel zoom
      </span>
    </div>
  {/if}
</div>
