// SpriteTool — sprite-aware tool: available iff ctx.spriteSession
// is set (Sprite asset_type, or user opted in via "Slice as
// sprite" on a PNG). Order 10 so it sits right after Details.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

export const spriteTool: ToolDef = {
  id: 'sprite',
  label: 'Sprite',
  order: 10,
  Icon,
  Body,
  Tips,
  isAvailable: (ctx) => !!ctx.spriteSession,
};
