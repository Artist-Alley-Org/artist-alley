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
import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
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
    ladder_available: true,
    pixel_width: null,
    pixel_height: null,
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

/** jsdom's `Image` never fetches, so the card's sheet measurement can
 *  never resolve on its own. Swap in a loader that reports the size the
 *  test names, which is how a real sheet of that shape would behave. */
function stubSpriteSheetSize(naturalWidth: number, naturalHeight: number) {
  class FakeImage {
    naturalWidth = 0;
    naturalHeight = 0;
    onload: (() => void) | null = null;
    #src = '';
    get src() {
      return this.#src;
    }
    set src(v: string) {
      this.#src = v;
      this.naturalWidth = naturalWidth;
      this.naturalHeight = naturalHeight;
      queueMicrotask(() => this.onload?.());
    }
  }
  vi.stubGlobal('Image', FakeImage);
}

/** Let the stubbed load callback and the resulting state update land. */
async function flushSpriteLoad() {
  await new Promise((r) => queueMicrotask(() => r(null)));
  await tick();
}

afterEach(() => {
  vi.unstubAllGlobals();
});

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

  it('sizes the scrub box to the sheet the server actually generated (#761)', async () => {
    // Sprite cells stopped being a fixed 16:9 in #761 — the sheet is now
    // fitted to the source, so a portrait clip's cells are portrait. The
    // card used to paint them into a hardcoded `aspect-video` box, which
    // reintroduced the exact squash the backend fix removed, just one
    // layer up.
    //
    // The grid is 10x10, so the sheet's aspect ratio IS the cell's; the
    // card measures the image rather than trusting recorded pixel dims
    // (which are the coded frame size, wrong for a rotated phone clip).
    // jsdom never loads images, so the loader is stubbed to report a
    // 900x1600 sheet — a real portrait sheet, per the Go-side
    // TestWriteSprites_RealFFmpeg.
    stubSpriteSheetSize(900, 1600);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushSpriteLoad();

    const layer = spriteLayer(container);
    expect(layer, 'video tile should mount a sprite layer on hover').toBeTruthy();
    // 900/1600 — portrait, not 16:9.
    expect(parseFloat(layer!.style.aspectRatio)).toBeCloseTo(900 / 1600, 4);
    // A portrait cell is bound by the tile's HEIGHT and pillarboxed;
    // width-bound would overflow the square slot.
    expect(layer!.className).toContain('h-full');
    expect(layer!.className).not.toContain('w-full');
  });

  it('keeps the landscape scrub box width-bound at 16:9 (#761 no-regression)', async () => {
    stubSpriteSheetSize(1600, 900);

    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);
    await flushSpriteLoad();

    const layer = spriteLayer(container);
    expect(parseFloat(layer!.style.aspectRatio)).toBeCloseTo(16 / 9, 4);
    expect(layer!.className).toContain('w-full');
  });

  it('falls back to 16:9 before the sheet has reported its size', async () => {
    // No stub: the image never loads, which is also the real first-paint
    // state. The box must not collapse to zero height while it waits.
    const { container } = render(AssetCard, { asset: asset({ file_extension: 'mp4' }) });
    await hoverCard(container);

    const layer = spriteLayer(container);
    expect(parseFloat(layer!.style.aspectRatio)).toBeCloseTo(16 / 9, 4);
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
