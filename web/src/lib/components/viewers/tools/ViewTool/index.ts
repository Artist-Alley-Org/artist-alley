// ViewTool — universal "how I view this asset" surface. Owns:
//   * 2D zoom presets (driven by the shell's canvas transform)
//   * Per-kind controller.tools sections (Camera / Display /
//     Lighting / Auto-rotate) advertised by the mounted view body.
//
// Available for every kind EXCEPT sprite (where the dedicated
// SpriteTool already owns zoom + playback) and placeholder
// (nothing to view). Order 5 so it sits between Details (0) and
// asset-specific tools (10+).

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Icon from './Icon.svelte';

const HIDDEN_FOR_KINDS = new Set(['sprite', 'placeholder']);

export const viewTool: ToolDef = {
  id: 'view',
  label: 'View',
  order: 5,
  Icon,
  Body,
  isAvailable: (ctx) =>
    !HIDDEN_FOR_KINDS.has(ctx.controller.kind) &&
    (!!ctx.shellState || !!ctx.controller.tools),
};
