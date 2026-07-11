// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// ArchiveTool — available whenever the host wired an ArchiveSession
// (kind === 'archive'). Order 10 alongside the other per-kind tools.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

export const archiveTool: ToolDef = {
  id: 'archive',
  label: 'Archive',
  order: 10,
  Icon,
  Body,
  Tips,
  isAvailable: (ctx) => !!ctx.archiveSession,
};
