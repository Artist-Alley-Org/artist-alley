// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #558 — the no-preview tile.
//
// jsdom has no layout engine and no container queries, so the SIZE
// tiers (one line at the 60px masonry floor, the full stack above
// 11rem) are not assertable here — they are verified in the browser,
// against a real tile pinned to the floor, and the screenshots are on
// the PR. What IS assertable, and what the two previous fallbacks got
// wrong, is the DECISION layer:
//
//   * every no-preview path lands on the plate, not just the doc one.
//     The old code had a typed card for `doc` and a bare 48px landscape
//     icon for everything else, so a failed 3D turntable and a failed
//     JPEG derivative rendered identically and said "image missing"
//     about a CAD model.
//   * the plate names the FORMAT and the KIND. Those are the only two
//     facts a tile with no bytes to show actually has.
//   * the title appears exactly when the card is not already printing
//     it next to the box, which is the one thing that differs per mode.
//
// The three fixtures below are the three real no-preview assets on the
// dev library (one `md`, one `gltf`, one `jpg`), not invented ones.

import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import AssetCard from './AssetCard.svelte';
import type { CardAsset } from './cardAsset';
import type { ViewMode } from '$stores/browseView.svelte';

function asset(overrides: Partial<CardAsset> = {}): CardAsset {
  return {
    id: '3f1b8e2c-0000-4000-8000-000000000558',
    title: 'Sponza — Khronos canonical scene',
    asset_type: 5,
    created_at: '2026-01-19T02:31:22.000Z',
    file_hash: 'c'.repeat(64),
    file_extension: 'gltf',
    thumbhash: null,
    // The state under test: the server confirms there is no servable
    // `col`, so no byte request is made and there is nothing to show.
    preview_available: false,
    ladder_available: false,
    pixel_width: null,
    pixel_height: null,
    ...overrides,
  };
}

function plate(container: HTMLElement): HTMLElement {
  const el = container.querySelector<HTMLElement>('[data-card-fallback]');
  expect(el, 'a card with no preview should render the plate').toBeTruthy();
  return el!;
}

function card(mode: ViewMode = 'grid', overrides: Partial<CardAsset> = {}) {
  return render(AssetCard, { asset: asset(overrides), mode });
}

describe('CardFallback (#558)', () => {
  it('renders for a failed 3D asset, naming the format and the kind', () => {
    const { container } = card();
    const p = plate(container);
    expect(p.dataset.cardFallback).toBe('3d');
    expect(p.textContent).toContain('GLTF');
    expect(p.textContent).toContain('3D model');
  });

  it('renders for a text asset, which never gets a raster variant', () => {
    const { container } = card('grid', { file_extension: 'md', asset_type: 2 });
    expect(plate(container).dataset.cardFallback).toBe('doc');
    expect(plate(container).textContent).toContain('MD');
  });

  it('renders for a raster whose derivative FAILED, not just for docs', () => {
    // The regression that made this issue read as "broken": an image
    // with no `col` fell to a generic landscape icon, indistinguishable
    // from the 3D case above.
    const { container } = card('grid', { file_extension: 'jpg', asset_type: 1 });
    const p = plate(container);
    expect(p.dataset.cardFallback).toBe('image');
    expect(p.textContent).toContain('JPG');
  });

  it('prints the extension verbatim, not the kind name', () => {
    // An operator who uploaded `.blend` should read BLEND. Both are
    // `3d`, and collapsing them to the kind would lose the one fact the
    // tile has that the tooltip does not.
    const { container } = card('grid', { file_extension: 'blend' });
    expect(plate(container).textContent).toContain('BLEND');
  });

  it('does not restate the kind when the format already is it', () => {
    const { container } = card('grid', { file_extension: 'pdf', asset_type: 1 });
    const text = plate(container).textContent ?? '';
    expect(text).toContain('PDF');
    expect(text.match(/PDF/gi)?.length, 'PDF / PDF reads as a bug').toBe(1);
  });

  it('carries the title where the card only shows it on hover', () => {
    // grid, masonry and feed all hide the title until hover (masonry
    // moves it into the cursor tooltip), so a no-preview tile at rest
    // is otherwise unidentifiable — and a document is mostly its name.
    for (const mode of ['grid', 'masonry', 'feed'] as ViewMode[]) {
      const { container } = card(mode);
      expect(plate(container).textContent, mode).toContain('Sponza');
    }
  });

  it('drops the title in thumbnail mode, where the header already has it', () => {
    const { container } = card('thumbnail');
    expect(
      plate(container).textContent,
      'the details card prints the title 8px above this box',
    ).not.toContain('Sponza');
    // …and the card itself still shows it.
    expect(container.textContent).toContain('Sponza');
  });

  it('falls back to the kind when the asset has no extension at all', () => {
    const { container } = card('grid', { file_extension: null, asset_type: 0 });
    const p = plate(container);
    expect(p.dataset.cardFallback).toBe('placeholder');
    expect(p.textContent).toContain('FILE');
  });

  it('yields to a thumbhash — a blurred real image beats a plate', () => {
    // Gated / not-yet-generated assets still have ~30 bytes of the real
    // artwork. That is more informative than any typography.
    const { container } = card('grid', { thumbhash: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAA' });
    expect(container.querySelector('[data-card-fallback]')).toBeNull();
  });
});
