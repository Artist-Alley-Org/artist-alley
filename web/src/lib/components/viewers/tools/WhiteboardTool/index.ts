// WhiteboardTool — available when ctx.whiteboardSession is set
// AND the host wired the required hooks (save / close / compact).
// Order 20 — sits below Sprite, above any host-injected tools.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

// Whiteboard tool is available whenever the host wired the
// activate hook (onActivate opens the overlay + creates the
// session). Selecting Whiteboard in the menubar IS the entry
// point — no separate "Open whiteboard" menu item. PostHost wires
// it; standalone /assets/[id] doesn't (no post anchor to bind
// the brushstrokes to), and the tool is hidden there.
export const whiteboardTool: ToolDef = {
  id: 'whiteboard',
  label: 'Whiteboard',
  order: 20,
  Icon,
  Body,
  Tips,
  // Losing the toolbox mid-stroke would be miserable UX — the
  // shell hides its full-collapse chevron while Whiteboard is
  // active. supportsCompact gives the user a "minimised but not
  // hidden" state: the shell shrinks the aside to an icon rail
  // and the Body reads ctx.shellState.paneCompact to render its
  // own compact (icon-strip) layout.
  noCollapse: true,
  supportsCompact: true,
  isAvailable: (ctx) =>
    !!(ctx.hostHooks?.whiteboard as { onActivate?: unknown } | undefined)?.onActivate,
};
