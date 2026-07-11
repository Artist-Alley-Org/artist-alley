// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// ModelTool — available whenever the host wired a ModelSession
// via AssetViewer's kind='3d' path. Sits at order 10 — same slot
// Sprite + Ebook occupy for their kinds — so the kind's primary
// tool follows Details in the dropdown.
//
// Exclusion: .mview assets render through Marmoset Toolbag's
// closed-source WebViewer, which doesn't expose env / lighting /
// material / animation APIs. Marmoset ships its own in-canvas
// chrome (orbit, lighting modes, animation transport) that the
// user reaches by hovering the viewport. We hide our tool so
// users aren't pointed at a sidebar of dead controls.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Tips from './Tips.svelte';
import Icon from './Icon.svelte';

export const modelTool: ToolDef = {
  id: 'model',
  label: '3D Viewer',
  order: 10,
  Icon,
  Body,
  Tips,
  isAvailable: (ctx) => {
    if (!ctx.modelSession) return false;
    const ext = (ctx.asset.file_extension || '').toLowerCase().replace(/^\./, '');
    return ext !== 'mview';
  },
};
