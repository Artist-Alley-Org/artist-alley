// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tool contract — the single shape every entry in the side-panel
// registry implements. The shell (ToolPanelShell.svelte) consumes
// this; the registry (registry.ts) is just `ToolDef[]`. Adding a
// new tool means "implement this interface + export the registry
// entry"; the shell doesn't need to know what kind of tool it is.
//
// Why no per-asset-type branching: tools advertise their own
// availability via isAvailable(ctx). The shell filters the registry
// against the current ctx every time the asset / session set
// changes, then renders whatever's left. A tool that depends on a
// SpriteSession returns false when ctx.spriteSession is undefined;
// the shell hides its dropdown entry automatically.

import type { Component, Snippet } from 'svelte';
import type { SpriteSessionInstance } from '$lib/sprite/session.svelte';
import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';
import type { EbookSessionInstance } from '$lib/ebook/session.svelte';
import type { ModelSessionInstance } from '$lib/3d/session.svelte';
import type { DocSessionInstance } from '$lib/doc/session.svelte';
import type { AudiobookSessionInstance } from '$lib/audiobook/session.svelte';
import type { ArchiveSessionInstance } from '$lib/archive/session.svelte';
import type { ViewAsset, ViewController } from '../controller';

/** Everything a tool body needs to do its job. Threaded through
 *  the shell as a single object so the per-tool component prop
 *  surface stays uniform — every tool reads { ctx } and pulls only
 *  the fields it cares about. */
export interface ToolContext {
  asset: ViewAsset;
  controller: ViewController;
  /** Sprite session — present when the asset is a sprite (or the
   *  user toggled "Slice as sprite" on a PNG). Sprite-only tools
   *  gate on this being defined. */
  spriteSession?: SpriteSessionInstance;
  /** Whiteboard session — present when the host wired one (post-
   *  anchored today; eventually any asset). Whiteboard tools gate
   *  on this. */
  whiteboardSession?: WhiteboardSession;
  /** Ebook session — present when kind === 'ebook'. EpubView and
   *  the EbookTool's side-panel body bind the same instance so
   *  TOC / search / bookmarks / reading settings stay in sync. */
  ebookSession?: EbookSessionInstance;
  /** Model session — present when kind === '3d'. ModelView and the
   *  ModelTool side-panel body bind the same instance so the user's
   *  env / lighting / display / camera / material picks land in the
   *  three.js scene live. */
  modelSession?: ModelSessionInstance;
  /** Document session — present when kind === 'doc'. DocView and
   *  the DocTool side-panel body bind the same instance for
   *  reading prefs / outline / find / bookmarks / stats. */
  docSession?: DocSessionInstance;
  /** Audiobook session — present when kind === 'audiobook'. The
   *  AudiobookView player and AudiobookTool side panel bind the
   *  same instance so the chapter list, playback speed, sleep
   *  timer, and bookmarks stay in sync. */
  audiobookSession?: AudiobookSessionInstance;
  /** Archive session — present when kind === 'archive' (.zip /
   *  .tar / .tar.gz / .jar / ...). The ArchiveView file tree and
   *  the ArchiveTool side panel bind the same instance so
   *  filtering / selection / expand-state stay in sync. */
  archiveSession?: ArchiveSessionInstance;
  /** Free-form bag of host-provided callbacks. Hosts that own
   *  details (e.g. PostHost has likes / comments / cover-picker)
   *  pass their handlers in; the matching tool reads them. Kept
   *  loosely typed so adding a hook doesn't ripple through every
   *  unrelated tool. */
  hostHooks?: Record<string, unknown>;
  /** Shell-owned reactive state (canvas zoom + transform). The
   *  ViewTool reads this to drive zoom presets; other tools
   *  ignore it. Populated by AssetViewer. */
  shellState?: ShellState;
  /** Id of the tool the shell currently has selected. Set by
   *  the shell so adapter components (SnippetBody / SnippetTips
   *  in tools/snippet-tool.ts) can resolve their host-provided
   *  snippet from `hostHooks[`tool:${activeToolId}`]`. */
  activeToolId?: string;
}

/** Convention for host-injected snippet tools: hosts register a
 *  ToolDef built via `defineSnippetTool({ id, ... })` and pass the
 *  snippet body / tips through `hostHooks[`tool:${id}`]`. Typed
 *  here so hosts + adapters agree without each one re-declaring
 *  the shape. */
export interface SnippetToolHooks {
  body?: Snippet<[ToolContext]>;
  tips?: Snippet<[ToolContext]>;
}

/** Compose the hostHooks key the snippet-tool adapter looks up
 *  for a given tool id. Hosts use the same helper when populating
 *  hostHooks so the two ends stay in sync. */
export function snippetToolHookKey(toolId: string): string {
  return `tool:${toolId}`;
}

/** Surface the AssetViewer shell exposes to tools that drive the
 *  canvas (just View today). Method calls flow through the same
 *  setters the shell uses internally so wheel-zoom + tool-button
 *  stay consistent. */
export interface ShellState {
  zoom: number;
  setZoom(z: number): void;
  resetView(): void;
  zoomPresets: Array<{ label: string; factor: number | null }>;
  /** True when the shell is rendering in compact-rail width.
   *  Tools that opt in via `supportsCompact` read this and switch
   *  their Body to a narrow layout. Always false for tools that
   *  didn't opt in (the shell ignores compact for them). */
  paneCompact: boolean;
}

/** One tool entry in the registry. The shell renders the picker by
 *  reading label + icon + order; mounts the active tool by reading
 *  Body; renders the pinned footer by reading Tips. */
export interface ToolDef {
  /** Stable id — used as the selected-tool key in URL / localStorage
   *  for future tool-persistence. Lowercase, ascii. */
  id: string;
  /** Static fallback label. Always present so a tool can list in
   *  the picker before any ctx-derived data is ready. */
  label: string;
  /** Optional dynamic label — when supplied, the shell + menubar
   *  prefer this over `label`. Lets a tool's header reflect ctx
   *  state (PostHost's Details tool returns "{post title} Details"
   *  so the panel header carries the post the user's on). Called
   *  on every reactive read of ctx; keep it cheap. */
  labelFn?: (ctx: ToolContext) => string;
  /** 14×14 inline-SVG component receiving { ctx } so the icon can
   *  react to ctx (e.g. show an "unsaved" dot). Component (not
   *  Snippet) so per-tool icons live in tiny .svelte files and
   *  TypeScript stays simple — no createRawSnippet gymnastics. */
  Icon: Component<{ ctx: ToolContext }>;
  /** Display order in the dropdown. Lower first. Built-in Details
   *  tool is 0; asset-specific tools occupy 10–90; host-injected
   *  custom tools default to 100+. */
  order: number;
  /** Per-mount gate. Called when the shell decides which tools to
   *  show. Pure / cheap — runs on every reactive change of ctx. */
  isAvailable(ctx: ToolContext): boolean;
  /** Body component — receives the ToolContext as its only prop. */
  Body: Component<{ ctx: ToolContext }>;
  /** Tips component — rendered INSIDE the shell's TipsSection's
   *  <dl> grid. Outputs <dt>/<dd> pairs as its top-level content;
   *  section dividers use `<dt class="col-span-2 mt-1 text-fg-
   *  muted/70">Heading</dt>`. Optional — tools without shortcuts
   *  omit it and the footer collapses. */
  Tips?: Component<{ ctx: ToolContext }>;
  /** When true, the shell hides its collapse chevron + refuses to
   *  honor paneCollapsed for this tool. The Whiteboard tool sets
   *  this so a user can't accidentally hide their entire toolbox
   *  mid-stroke. Defaults to false — most tools are fine to
   *  collapse to a rail. */
  noCollapse?: boolean;
  /** Tool wants to support a "compact" pane state — the shell
   *  shrinks the aside width to an icon-rail (~3.5rem) and sets
   *  ctx.shellState.paneCompact=true so the Body can render its
   *  icon-rail layout. The shell renders a shrink/expand chevron
   *  in the header that toggles this state. Whiteboard sets this
   *  so the user has a "minimised but not hidden" option that
   *  still leaves the toolbox reachable. */
  supportsCompact?: boolean;
}

/** Public re-export of the global TipsSection title default. Kept
 *  here so per-tool snippets can override it consistently. */
export const DEFAULT_TIPS_TITLE = 'Tips & shortcuts';
