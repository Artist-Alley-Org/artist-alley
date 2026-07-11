// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// DocTool — available whenever the host wired a DocSession (kind
// === 'doc'). Sits at order 10 alongside Ebook / Sprite / Model.
//
// Phase A surface: Reading / Outline / Find / Bookmarks / Stats.
// Phase B adds the Annotations section + selection-driven
// highlight / strikethrough / underline / comment / note tools.
// Phase C adds the Lint diagnostics panel.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

export const docTool: ToolDef = {
  id: 'doc',
  label: 'Document',
  order: 10,
  Icon,
  Body,
  Tips,
  isAvailable: (ctx) => !!ctx.docSession,
};
