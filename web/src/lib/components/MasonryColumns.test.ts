// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #651 — masonry is column buckets, not a balanced multi-column flow.
//
// The bug is layout-over-time (appending re-sorted tiles the user was
// already looking at into other columns) and no static assertion can see
// it — that verification is a real browser with real scrolling, and the
// numbers are in the PR. What IS assertable here, and what a refactor
// would otherwise silently drop, is the two decisions the mechanism rests
// on:
//
//   1. `cardTileRatio` — the height prediction. It has to agree with
//      CardThumb's `declaredRatio` (same ladder precondition, same clamp,
//      same cover-asset resolution) or the bucketer balances columns
//      against heights nothing actually has. Two copies of that rule in
//      two files is exactly how it drifts, so it lives in one and this
//      pins it.
//   2. The accessibility contract. Column buckets mean DOM order is
//      column-major, not feed order. We accept that traversal order and
//      compensate with list semantics carrying the TRUE feed position;
//      if a later change drops the `role="list"` / `aria-posinset` pair
//      the layout still looks right and the compensation is just gone.

import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import MasonryColumns from './MasonryColumns.svelte';
import { cardTileRatio, RATIO_MAX, RATIO_MIN } from './cardAsset';
import { createRawSnippet } from 'svelte';

describe('cardTileRatio', () => {
  const assetRow = (over: Record<string, unknown> = {}) => ({
    id: 'a1',
    ladder_available: true,
    pixel_width: 1600,
    pixel_height: 300,
    ...over,
  });

  it('reads an asset row', () => {
    expect(cardTileRatio(assetRow(), true)).toBeCloseTo(1600 / 300);
  });

  it('resolves a post row through its cover asset', () => {
    const post = {
      id: 'p1',
      cover_asset_id: 'a2',
      members: [
        { asset_id: 'a1', asset: assetRow() },
        { asset_id: 'a2', asset: assetRow({ pixel_width: 800, pixel_height: 800 }) },
      ],
    };
    expect(cardTileRatio(post, true)).toBe(1);
  });

  it('falls back to the first member when no cover is set', () => {
    const post = { id: 'p1', members: [{ asset_id: 'a1', asset: assetRow() }] };
    expect(cardTileRatio(post, true)).toBeCloseTo(1600 / 300);
  });

  // The ladder preconditions, both of them. Without a responsive srcset
  // CardThumb can only request `col` — a 320x320 centre CROP — so the
  // SOURCE ratio is not the shape that will be on screen, and predicting
  // from it would balance the columns against a tile that never renders.
  it('declines without the per-asset ladder', () => {
    expect(cardTileRatio(assetRow({ ladder_available: false }), true)).toBeNull();
  });
  it('declines before the install ladder has loaded', () => {
    expect(cardTileRatio(assetRow(), false)).toBeNull();
  });

  it('declines with no recorded dimensions', () => {
    expect(cardTileRatio(assetRow({ pixel_width: null, pixel_height: null }), true)).toBeNull();
    expect(cardTileRatio(assetRow({ pixel_height: 0 }), true)).toBeNull();
  });

  it('declines on a row it cannot read at all', () => {
    expect(cardTileRatio({ id: 'c1', title: 'a collection' }, true)).toBeNull();
    expect(cardTileRatio(null, true)).toBeNull();
  });

  // Guards corrupt metadata, not a design choice — a 4000:1 would
  // compute a sub-pixel tile nobody can see or click.
  it('clamps values that could not be a picture', () => {
    expect(cardTileRatio(assetRow({ pixel_width: 4000, pixel_height: 1 }), true)).toBe(RATIO_MAX);
    expect(cardTileRatio(assetRow({ pixel_width: 1, pixel_height: 4000 }), true)).toBe(RATIO_MIN);
  });
});

describe('MasonryColumns', () => {
  const items = Array.from({ length: 5 }, (_, i) => ({ id: `i${i}` }));
  const card = createRawSnippet(() => ({ render: () => '<span>tile</span>' }));

  function wall() {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const { container } = render(MasonryColumns as any, {
      props: { items, tileMin: '22rem', card },
    });
    const el = container.querySelector('.posts-masonry');
    expect(el, 'renders a wall').toBeTruthy();
    return el as HTMLElement;
  }

  it('places every item in a column', () => {
    const el = wall();
    expect(el.querySelectorAll('[data-masonry-col]').length).toBeGreaterThanOrEqual(1);
    expect(el.querySelectorAll('[data-tile-id]').length).toBe(items.length);
  });

  // DOM order is column-major, so the feed position has to be carried
  // explicitly or it is simply lost. See the header comment.
  it('carries each tile\'s true feed position in list semantics', () => {
    const el = wall();
    expect(el.getAttribute('role')).toBe('list');
    for (const col of el.querySelectorAll('[data-masonry-col]')) {
      expect(col.getAttribute('role')).toBe('presentation');
    }
    const tiles = [...el.querySelectorAll<HTMLElement>('[role="listitem"]')];
    expect(tiles.length).toBe(items.length);
    for (const tile of tiles) {
      const idx = items.findIndex((it) => it.id === tile.dataset.tileId);
      expect(tile.getAttribute('aria-posinset')).toBe(String(idx + 1));
      expect(tile.getAttribute('aria-setsize')).toBe(String(items.length));
    }
  });
});
