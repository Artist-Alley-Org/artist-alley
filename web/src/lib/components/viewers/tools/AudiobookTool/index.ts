// AudiobookTool — available whenever the host wired an Audiobook-
// Session (kind === 'audiobook'). Order 10 alongside the other
// per-kind tools.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

export const audiobookTool: ToolDef = {
  id: 'audiobook',
  label: 'Audiobook',
  order: 10,
  Icon,
  Body,
  Tips,
  isAvailable: (ctx) => !!ctx.audiobookSession,
};
