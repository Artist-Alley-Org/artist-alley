// Tool registry — the source of truth for what shows up in the
// AssetViewer side-panel dropdown. Order here doesn't matter (the
// shell sorts by ToolDef.order); presence here does. Adding a new
// tool: implement ToolDef, import the entry, append it. No other
// file needs to change.
//
// Host-injected tools (passed via AssetViewer's customTools prop)
// merge into this list at shell-mount time, so a host that owns
// post details just registers its own ToolDef without forking the
// viewer.

import type { ToolDef } from './contract';
import { detailsTool } from './DetailsTool/index';
import { spriteTool } from './SpriteTool/index';
import { whiteboardTool } from './WhiteboardTool/index';
import { viewTool } from './ViewTool/index';

export const TOOLS: ToolDef[] = [
  detailsTool,
  viewTool,
  spriteTool,
  whiteboardTool,
];
