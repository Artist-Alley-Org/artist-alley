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

import type { Component } from 'svelte';
import type { SpriteSessionInstance } from '$lib/sprite/session.svelte';
import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';
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
}

/** One tool entry in the registry. The shell renders the picker by
 *  reading label + icon + order; mounts the active tool by reading
 *  Body; renders the pinned footer by reading Tips. */
export interface ToolDef {
  /** Stable id — used as the selected-tool key in URL / localStorage
   *  for future tool-persistence. Lowercase, ascii. */
  id: string;
  /** Shown in the dropdown trigger + the active-tool header. */
  label: string;
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
}

/** Public re-export of the global TipsSection title default. Kept
 *  here so per-tool snippets can override it consistently. */
export const DEFAULT_TIPS_TITLE = 'Tips & shortcuts';
