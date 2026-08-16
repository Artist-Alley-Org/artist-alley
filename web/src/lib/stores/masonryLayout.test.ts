// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1139 — the masonry overlay's three tiers.
//
// The bug this pins was a MISSING DIMENSION, not a wrong number: the
// gate asked only about width, and a wide masonry tile is SHORT by
// construction, so the one shape that could not hold the overlay was
// the one that cleared the gate most easily. Every case below therefore
// varies width and height INDEPENDENTLY — a test that only walked the
// diagonal would pass on the broken rule.
//
// The boundary cases are exact, on purpose. These three numbers were
// calibrated against screenshots (see masonryLayout's own docs for the
// measurements) and a silent drift of a few pixels is the failure mode
// that brings the clip back: 1px under the floor is a different tier.

import { describe, it, expect } from 'vitest';

import {
  masonryOverlayTier,
  MASONRY_OVERLAY_MIN_W_PX,
  MASONRY_OVERLAY_MIN_H_PX,
  MASONRY_OVERLAY_COMPRESSED_MIN_H_PX,
} from './masonryLayout.svelte';

const W = MASONRY_OVERLAY_MIN_W_PX;
const H = MASONRY_OVERLAY_MIN_H_PX;
const C = MASONRY_OVERLAY_COMPRESSED_MIN_H_PX;

describe('masonryOverlayTier', () => {
  it('an unmeasured tile is minimal, so nothing flashes a full overlay before it is sized', () => {
    expect(masonryOverlayTier(null)).toBe('minimal');
  });

  it('width alone never qualifies — the #1139 case', () => {
    // The owner's tile, measured: 738x89, two columns wide. It clears
    // the width gate by 458px and cannot contain the overlay stack.
    // This assertion IS the bug: it returned the full overlay before.
    expect(masonryOverlayTier({ w: 738, h: 89 })).toBe('minimal');
  });

  it('height alone never qualifies either — a tall narrow tile has no text lane', () => {
    // The converse, and it is not symmetric hand-waving: a 200px-wide
    // tile has nothing to compress INTO, which is why there is no
    // narrow tier. 900px of height does not buy it one.
    expect(masonryOverlayTier({ w: W - 1, h: 900 })).toBe('minimal');
  });

  it('full needs both gates', () => {
    expect(masonryOverlayTier({ w: W, h: H })).toBe('full');
    expect(masonryOverlayTier({ w: 2000, h: 2000 })).toBe('full');
  });

  describe('the height boundaries are exact', () => {
    it('one pixel under the full floor compresses rather than clipping', () => {
      expect(masonryOverlayTier({ w: W, h: H })).toBe('full');
      expect(masonryOverlayTier({ w: W, h: H - 1 })).toBe('compressed');
    });

    it('one pixel under the compressed floor goes art-only', () => {
      expect(masonryOverlayTier({ w: W, h: C })).toBe('compressed');
      expect(masonryOverlayTier({ w: W, h: C - 1 })).toBe('minimal');
    });
  });

  it('the width boundary is exact and applies at every height', () => {
    expect(masonryOverlayTier({ w: W, h: H })).toBe('full');
    expect(masonryOverlayTier({ w: W - 1, h: H })).toBe('minimal');
    // Under the width floor there is no compressed fallback either —
    // it drops straight to minimal, because the tier below `full` is
    // still a caption and a caption needs a lane.
    expect(masonryOverlayTier({ w: W - 1, h: C })).toBe('minimal');
  });

  it('the tiers are ordered — the compressed band sits strictly between', () => {
    // Not a tautology: an edit that set the compressed floor above the
    // full floor would make the middle tier unreachable, and every
    // assertion above would still pass.
    expect(C).toBeLessThan(H);
    expect(masonryOverlayTier({ w: W, h: (C + H) / 2 })).toBe('compressed');
  });

  it('real tiles measured on the dev wall land in the expected tiers', () => {
    // Captured at 1920 and 2560 across rungs 2-7 (#1139 grounding).
    // Keeping the real numbers means a recalibration has to look at
    // what it does to actual tiles rather than to invented ones.
    const cases: Array<[number, number, string]> = [
      [738, 89, 'minimal'], // span-2, the reported clip
      [365, 106, 'minimal'], // span-1, short: two blocks would abut
      [925, 110, 'compressed'], // span-2 at 1920, rung 6
      [458, 132, 'compressed'], // span-1 at 1920, rung 6
      [493, 142, 'compressed'], // span-1 at 2560, rung 6
      [365, 194, 'full'], // the shortest ordinary tile at the default rung
      [365, 646, 'full'],
      [168, 168, 'minimal'], // 390px viewport — no lane at any height
    ];
    for (const [w, h, want] of cases) {
      expect(masonryOverlayTier({ w, h }), `${w}x${h}`).toBe(want);
    }
  });
});
