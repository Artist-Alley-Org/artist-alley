// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #640 — the masonry tile takes the shape of its image.
//
// Before this, the thumb frame was hard-coded `aspect-square` in every
// mode. Measured at 1680px, a 5.33:1 audio waveform and a 1:1 sprite
// rendered as identical 315x315 boxes: masonry was a multi-column grid
// of squares, which is the one thing a masonry is not.
//
// The tests here pin the DECISION, not the pixels — jsdom has no layout
// engine, so what is assertable (and what actually broke) is whether the
// component declares an `aspect-ratio` at all, and from what. Three
// rules, each of which was a real judgement call:
//
//   1. masonry with recorded dimensions declares the ratio BEFORE the
//      image loads. That reservation is the entire reason the server
//      projection in #640 exists; without it the wall reflows as each
//      image arrives.
//   2. grid and thumbnail keep their square. Both are deliberate
//      (#555/#588 contact sheet, #556 details card) and a change to the
//      shared frame must not leak into them.
//   3. no responsive ladder ⇒ no declared ratio, even WITH dimensions.
//      Without the ladder the card can only request `col`, a 320x320
//      centre CROP, and sizing that tile from the source's 5.33:1 would
//      letterbox a square inside a billboard. The tile follows the
//      image it renders, not the image it came from.

import { render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import AssetCard from './AssetCard.svelte';
import { previewLadder } from '$stores/previewLadder.svelte';
import { MASONRY_MIN_TILE_REM, type CardAsset } from './cardAsset';
import type { ViewMode } from '$stores/browseView.svelte';

const ASSET_ID = '3f1b8e2c-0000-4000-8000-000000000042';

function asset(overrides: Partial<CardAsset> = {}): CardAsset {
  return {
    id: ASSET_ID,
    title: 'Waveform',
    asset_type: 1,
    created_at: '2026-07-01T12:00:00.000Z',
    file_hash: 'b'.repeat(64),
    file_extension: 'png',
    thumbhash: null,
    preview_available: true,
    ladder_available: true,
    pixel_width: 1600,
    pixel_height: 300,
    ...overrides,
  };
}

/** The thumb frame — the element whose height the masonry column
 *  actually stacks. Queried by the test hook rather than by class so a
 *  Tailwind refactor doesn't quietly neuter every assertion here. */
function thumb(container: HTMLElement): HTMLElement {
  const el = container.querySelector<HTMLElement>('[data-card-thumb]');
  expect(el, 'card should render a thumb frame').toBeTruthy();
  return el!;
}

function declaredRatio(container: HTMLElement): number | null {
  const raw = thumb(container).style.aspectRatio;
  if (!raw) return null;
  const [w, h] = raw.split('/').map((s) => parseFloat(s.trim()));
  return h ? w / h : w;
}

function card(mode: ViewMode, overrides: Partial<CardAsset> = {}) {
  return render(AssetCard, { asset: asset(overrides), mode });
}

describe('CardThumb tile shape (#640)', () => {
  beforeEach(() => {
    // Simulate an install whose contain ladder is readable, which is
    // what licenses the responsive srcset — and therefore what makes the
    // recorded dimensions describe the image being rendered.
    previewLadder.rungs = [
      { key: 'preview', maxDim: 640 },
      { key: 'screen', maxDim: 1280 },
    ];
  });
  afterEach(() => {
    previewLadder.rungs = [];
  });

  it('declares the recorded ratio in masonry, before any image loads', () => {
    const { container } = card('masonry');
    // 1600x300 — the tile is over five times wider than it is tall, and
    // nothing has loaded yet.
    expect(declaredRatio(container)).toBeCloseTo(16 / 3, 3);
    expect(thumb(container).className).not.toContain('aspect-square');
  });

  it('keeps grid square — the contact sheet is not negotiable', () => {
    const { container } = card('grid');
    expect(declaredRatio(container)).toBeNull();
    expect(thumb(container).className).toContain('aspect-square');
  });

  it('keeps the thumbnail details card square', () => {
    const { container } = card('thumbnail');
    expect(declaredRatio(container)).toBeNull();
    expect(thumb(container).className).toContain('aspect-square');
  });

  it('stays square in masonry when nothing was ever measured', () => {
    // The common case, and the reason the fallback matters: audio,
    // video, 3D, fonts and every draft raster have no recorded
    // dimensions. The tile reserves a square and corrects itself once
    // the image reports its own size.
    const { container } = card('masonry', { pixel_width: null, pixel_height: null });
    expect(declaredRatio(container)).toBeNull();
    expect(thumb(container).className).toContain('aspect-square');
  });

  it('ignores recorded dimensions when only the col CROP is servable', () => {
    previewLadder.rungs = [];
    const { container } = card('masonry', { ladder_available: false });
    expect(
      declaredRatio(container),
      'sizing a tile 16:3 while serving a 1:1 crop would letterbox a square inside a billboard',
    ).toBeNull();
  });

  it('ignores a nonsense measurement rather than collapsing the tile', () => {
    const { container } = card('masonry', { pixel_width: 1600, pixel_height: 0 });
    expect(declaredRatio(container)).toBeNull();
  });
});

// #652 — the same frame, one issue later. Giving every tile its true
// ratio made the thin ones genuinely thin: measured at 1440px, 45 of 216
// tiles came in under 60px and the shortest was 30px, while the two
// overlay controls that must live inside are 44x44 each. The controls
// were hanging out of the artwork.
//
// jsdom has no layout, so what is assertable is the same thing that
// actually broke: whether the frame DECLARES the floor, and whether it
// is scoped to masonry. The pixel proof is a real browser and lives in
// the PR.
describe('CardThumb tile floor (#652)', () => {
  beforeEach(() => {
    previewLadder.rungs = [
      { key: 'preview', maxDim: 640 },
      { key: 'screen', maxDim: 1280 },
    ];
  });
  afterEach(() => {
    previewLadder.rungs = [];
  });

  const minHeight = (container: HTMLElement) => thumb(container).style.minHeight;

  it('floors a masonry tile at the control band', () => {
    const { container } = card('masonry');
    expect(minHeight(container)).toBe(`${MASONRY_MIN_TILE_REM}rem`);
    // The floor must not cost the ratio — #646 holds above it.
    expect(declaredRatio(container)).toBeCloseTo(16 / 3, 3);
  });

  it('floors a masonry tile that has no ratio yet', () => {
    // The ratio can arrive later (measured on load) or never. A floor
    // that has to be re-decided is a floor that gets missed.
    const { container } = card('masonry', { pixel_width: null, pixel_height: null });
    expect(minHeight(container)).toBe(`${MASONRY_MIN_TILE_REM}rem`);
  });

  it('leaves grid and thumbnail alone — their tiles are already big', () => {
    expect(minHeight(card('grid').container)).toBe('');
    expect(minHeight(card('thumbnail').container)).toBe('');
  });
});

// #652 — "only keep the options and checkbox". Everything else painted
// over a masonry tile comes off, because at 60px there is no room for it
// and covering the artwork to caption it is what makes the mode useless.
describe('masonry overlay contents (#652)', () => {
  beforeEach(() => {
    previewLadder.rungs = [{ key: 'preview', maxDim: 640 }];
  });
  afterEach(() => {
    previewLadder.rungs = [];
  });

  it('keeps the ⋮ menu and the checkbox', () => {
    const { container } = card('masonry');
    expect(container.querySelector('[data-testid="card-menu-trigger"]')).toBeTruthy();
    // The checkbox self-gates on auth (see CardCheckbox); assert the
    // slot the card gives it rather than the store state.
    expect(container.querySelector('[data-card-thumb]')).toBeTruthy();
  });

  it('drops the title overlay that covers the artwork', () => {
    const { container } = card('masonry');
    // The gradient caption strip — present in grid, gone in masonry.
    const gradient = (c: HTMLElement) =>
      [...c.querySelectorAll('div')].filter((d) => d.className.includes('bg-gradient-to-t')).length;
    expect(gradient(card('grid').container)).toBeGreaterThan(0);
    expect(gradient(container)).toBe(0);
  });
});
