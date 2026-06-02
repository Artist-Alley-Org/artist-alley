// Tool registry — the source of truth for what shows up in the
// menubar Tools picker. Order here doesn't matter (the shell sorts
// by ToolDef.order); presence here does. Adding a new tool:
// implement ToolDef, import the entry, append it. No other file
// needs to change.
//
// Host-injected tools (passed via AssetViewer's customTools prop)
// merge into this list at shell-mount time. Hosts that REPLACE a
// built-in tool (same id) take its slot — PostHost overrides
// Details so the body renders post info instead of generic asset
// info.

import type { ToolDef } from './contract';
import { detailsTool } from './DetailsTool/index';
import { spriteTool } from './SpriteTool/index';
import { whiteboardTool } from './WhiteboardTool/index';
import { ebookTool } from './EbookTool/index';

// View was retired — basic 2D zoom presets + per-kind 3D controls
// live inside DetailsTool's body now, since "Details" is the no-
// tool fallback that always renders for any kind.
export const TOOLS: ToolDef[] = [
  detailsTool,
  ebookTool,
  spriteTool,
  whiteboardTool,
];
