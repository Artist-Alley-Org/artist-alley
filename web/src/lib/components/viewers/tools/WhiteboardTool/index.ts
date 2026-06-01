// WhiteboardTool — available when ctx.whiteboardSession is set
// AND the host wired the required hooks (save / close / compact).
// Order 20 — sits below Sprite, above any host-injected tools.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

export const whiteboardTool: ToolDef = {
  id: 'whiteboard',
  label: 'Whiteboard',
  order: 20,
  Icon,
  Body,
  Tips,
  isAvailable: (ctx) =>
    !!ctx.whiteboardSession && !!(ctx.hostHooks?.whiteboard as { onSave?: unknown } | undefined)?.onSave,
};
