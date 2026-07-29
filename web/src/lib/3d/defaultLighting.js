// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Default lighting rig — the single source of truth for the "freshly
// opened model" look (#509, ADR 0069's WYSIWYG intent).
//
// ADR 0069 promised the interactive viewer's DEFAULT look matches the
// browse-grid thumbnail. The thumbnail is rendered headless by
// scripts/threejs/render.html; the viewer by ModelView.svelte. Both now
// IMPORT these constants rather than carrying their own copy — that is
// why this file is plain JS: render.html is a static page served to a
// headless Chromium with an importmap and no build step, so it cannot
// import TypeScript. worker.mjs serves this directory at /shared/ and
// the Dockerfiles copy the file to /app/threejs/shared/. Same plumbing
// carries modelLoader.js, the shared load path (#689).
//
// The rig is a three-point directional setup over a RoomEnvironment IBL
// (the viewer's `studio` env preset IS RoomEnvironment at roughness
// DEFAULT_ENV_ROUGHNESS — identical to the thumbnail), plus a low
// ambient floor so the darkest faces don't crush to black. ACES Filmic
// + exposure 1.0 is the tone curve both render through.

/** Key (main) directional light intensity. */
export const DEFAULT_KEY_INTENSITY = 2.2;
/** Fill directional light intensity (opposite the key). */
export const DEFAULT_FILL_INTENSITY = 0.8;
/** Rim/back directional light intensity. */
export const DEFAULT_RIM_INTENSITY = 1.0;
/** Ambient floor so shadowed faces read, not crush to black. */
export const DEFAULT_AMBIENT_INTENSITY = 0.25;
/** PMREM roughness the RoomEnvironment IBL is prefiltered at. */
export const DEFAULT_ENV_ROUGHNESS = 0.04;
/** Tone-mapping curve both surfaces render through.
 *  @type {import('./environments').ToneMappingId} */
export const DEFAULT_TONE_MAPPING = 'aces';
/** Tone-mapping exposure. */
export const DEFAULT_EXPOSURE = 1.0;
