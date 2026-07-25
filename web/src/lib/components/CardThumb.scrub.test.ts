// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Regression guard for the hover sprite-scrub preview (#595).
//
// The bug this exists to prevent recurring: video and 3D tiles stopped
// showing their hover preview, and it went unnoticed through four
// consecutive card refactors because nothing asserted the behaviour at
// any layer. The eventual cause was NOT in the render path — it was a
// surface feeding the card an object with `file_extension` missing, so
// CardThumb saw an untyped asset and correctly rendered no scrub.
//
// So the guard is deliberately in two parts, because the bug had two
// halves and either can regress alone:
//
//   1. THE CONTRACT — cardAsset.ts makes the presentation fields
//      required, so a surface can no longer drop one silently. That is
//      enforced by svelte-check, not by this file. It is the half that
//      would have caught the actual #595 defect.
//
//   2. THE RENDER PATH — these tests. Given a correctly-fed card, a
//      hover must actually produce an animating sprite layer with the
//      right frame geometry. The contract cannot catch someone deleting
//      the sprite markup or inverting the `hovering` gate.
//
// Frame geometry is asserted as concrete percentages rather than "some
// background-position": the 10x10 video sheet and the 6x6 3D turntable
// step at different rates, and getting the divisor wrong (cols vs
// cols-1) yields an animation that visibly clips the last frame — a
// regression a truthy check would wave through.

import { render, fireEvent } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import AssetCard from './AssetCard.svelte';
import type { CardAsset } from './cardAsset';

const ASSET_ID = '3f1b8e2c-0000-4000-8000-000000000001';

function asset(overrides: Partial<CardAsset> = {}): CardAsset {
  return {
    id: ASSET_ID,
    title: 'Turntable',
    asset_type: 1,
    created_at: '2026-07-01T12:00:00.000Z',
    file_hash: 'a'.repeat(64),
    file_extension: 'glb',
    thumbhash: null,
    preview_available: true,
    ...overrides,
  };
}

/** The sprite layer is the element carrying the sprite sheet as its
 *  background — the same thing a user sees animate. Queried by that
 *  rather than by a class so a styling refactor doesn't silently
 *  neuter the test. */
function spriteLayer(container: HTMLElement): HTMLElement | null {
  return container.querySelector<HTMLElement>('div[style*="sprites.jpg"]');
}

/** Hover is delivered to the stretched navigation link, which is where
 *  the real pointer lands and where the handlers live. */
async function hoverCard(container: HTMLElement) {
  const link = container.querySelector('a[href^="/assets/"]');
  expect(link, 'card should expose a stretched navigation link').toBeTruthy();
  await fireEvent.mouseEnter(link!);
}

async function unhoverCard(container: HTMLElement) {
  const link = container.querySelector('a[href^="/assets/"]');
  await fireEvent.mouseLeave(link!);
}

describe('CardThumb sprite scrub (#595)', () => {
  it('renders no sprite layer at rest', () => {
    const { container } = render(AssetCard, { asset: asset() });
    expect(spriteLayer(container)).toBeNull();
  });

  it('plays the 6x6 turntable when a 3D tile is hovered', async () => {
    const { container } = render(AssetCard, { asset: asset({ file_extension: 'glb' }) });
    await hoverCard(container);

    const layer = spriteLayer(container);
    expect(layer, '3D tile should mount a sprite layer on hover').toBeTruthy();
    expect(layer!.style.backgroundImage).toContain(
      `/api/v1/assets/${ASSET_ID}/variants/sprites.jpg`,
    );
    // 6x6 sheet: the sheet is scaled to 600% so one cell fills the box.
    expect(layer!.style.backgroundSize).toBe('600% 600%');
    // Frame 0 sits at the sheet's origin.
    expect(layer!.style.backgroundPosition).toBe('0% 0%');
  });

  it('plays the 10x10 sheet when a video tile is hovered', async () => {
    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);

    const layer = spriteLayer(container);
    expect(layer, 'video tile should mount a sprite layer on hover').toBeTruthy();
    expect(layer!.style.backgroundSize).toBe('1000% 1000%');
  });

  it('tears the sprite layer down when the pointer leaves', async () => {
    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    expect(spriteLayer(container)).toBeTruthy();

    await unhoverCard(container);
    expect(
      spriteLayer(container),
      'leaving the tile must stop the scrub, not leave it running off-screen',
    ).toBeNull();
  });

  it('leaves still images alone — no sprite request for an untyped tile', async () => {
    const { container } = render(AssetCard, { asset: asset({ file_extension: 'png' }) });
    await hoverCard(container);
    expect(
      spriteLayer(container),
      'a still image has no sprite sheet; hovering must not request one (#471 zero-404s)',
    ).toBeNull();
  });

  it('degrades silently when the extension is unknown', async () => {
    // The null case is a REAL answer, not a caller that forgot — see
    // cardAsset.ts. It must render a plain tile, never a 404-generating
    // sprite request.
    const { container } = render(AssetCard, { asset: asset({ file_extension: null }) });
    await hoverCard(container);
    expect(spriteLayer(container)).toBeNull();
  });
});
