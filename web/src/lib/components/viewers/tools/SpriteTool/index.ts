// SpriteTool — sprite-aware tool: available iff ctx.spriteSession
// is set (Sprite asset_type, or user opted in via "Slice as
// sprite" on a PNG). Order 10 so it sits right after Details.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

// Available when the asset is a sprite OR a PNG the user could
// reasonably slice as one. AssetViewer reacts to this becoming
// the active tool by flipping spriteOverride so the canvas re-
// mounts as SpriteCanvas + a session spins up. Picking Sprite
// Viewer IS the slicing entry point — no separate "Slice as
// sprite" toggle needed.
export const spriteTool: ToolDef = {
  id: 'sprite',
  label: 'Sprite Viewer',
  order: 10,
  Icon,
  Body,
  Tips,
  isAvailable: (ctx) => {
    if (ctx.controller.kind === 'sprite') return true;
    // PNG image — sprite sheets in the wild are basically all PNG
    // (lossless + alpha). JPG / WEBP photos shouldn't tempt the
    // user with Sprite Viewer.
    const ext = ctx.asset.file_extension?.toLowerCase().replace(/^\./, '');
    return ctx.controller.kind === 'image' && ext === 'png';
  },
};
