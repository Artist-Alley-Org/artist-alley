// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// EbookTool — available whenever the host wired an EbookSession
// via hostHooks.ebook (AssetViewer does this for kind='ebook'
// assets). Sits at order 10 — same slot Sprite occupies for
// sprite assets — so the kind's primary tool follows Details in
// the dropdown.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

export const ebookTool: ToolDef = {
  id: 'ebook',
  label: 'Reader',
  order: 10,
  Icon,
  Body,
  Tips,
  isAvailable: (ctx) => !!ctx.ebookSession,
};
