// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Default lighting rig — the single source of truth for the "freshly
// opened model" look (#509, ADR 0069's WYSIWYG intent).
//
// ADR 0069 promised the interactive viewer's DEFAULT look matches the
// browse-grid thumbnail. The thumbnail is rendered headless by
// scripts/threejs/render.html; the viewer by ModelView.svelte. Because
// those two live in different build worlds — the thumbnail is a static
// page served to a headless Chromium via an importmap `three`, the
// viewer is a Vite-bundled Svelte component importing the npm `three`,
// and the web container only mounts ./web — they cannot import one live
// module. So this file is the web-side source of truth, and
// render.html carries a matching literal rig with a pointer back here.
//
// KEEP IN SYNC: scripts/threejs/render.html (its key/fill/rim/ambient +
// tone-mapping/exposure lines). If you change a number here, change it
// there too, or the thumbnail and the viewer's default drift apart and
// ADR 0069's WYSIWYG promise breaks.
//
// The rig is a three-point directional setup over a RoomEnvironment IBL
// (the viewer's `studio` env preset IS RoomEnvironment at roughness
// 0.04 — identical to the thumbnail), plus a low ambient floor so the
// darkest faces don't crush to black. ACES Filmic + exposure 1.0 is the
// tone curve both render through.

import type { ToneMappingId } from './environments';

/** Key (main) directional light intensity. */
export const DEFAULT_KEY_INTENSITY = 2.2;
/** Fill directional light intensity (opposite the key). */
export const DEFAULT_FILL_INTENSITY = 0.8;
/** Rim/back directional light intensity. */
export const DEFAULT_RIM_INTENSITY = 1.0;
/** Ambient floor so shadowed faces read, not crush to black. */
export const DEFAULT_AMBIENT_INTENSITY = 0.25;
/** Tone-mapping curve both surfaces render through. */
export const DEFAULT_TONE_MAPPING: ToneMappingId = 'aces';
/** Tone-mapping exposure. */
export const DEFAULT_EXPOSURE = 1.0;
