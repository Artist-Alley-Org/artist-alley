// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, expect, it } from 'vitest';

import {
  CARD_ASPECT,
  cropWindow,
  focalFromOrigin,
  hasTravel,
  marqueeOrigin,
  objectPosition,
} from './featuredCrop';

describe('#1207 featured-crop geometry', () => {
  describe('cropWindow', () => {
    it('trims the WIDTH of a picture wider than the card and keeps its full height', () => {
      // 2:1 against 1.78:1 — the card is narrower, so the sides go.
      const win = cropWindow(2);
      expect(win.h).toBe(1);
      expect(win.w).toBeCloseTo(CARD_ASPECT / 2, 12);
    });

    it('trims the HEIGHT of a portrait picture and keeps its full width', () => {
      const win = cropWindow(0.5);
      expect(win.w).toBe(1);
      expect(win.h).toBeCloseTo(0.5 / CARD_ASPECT, 12);
    });

    it('shows an already-card-shaped picture whole on both axes', () => {
      const win = cropWindow(CARD_ASPECT);
      expect(win.w).toBeCloseTo(1, 12);
      expect(win.h).toBe(1);
    });

    // A picture whose natural size has not arrived yet reports 0, and a
    // NaN aspect would propagate silently into a style attribute.
    it('degrades to the whole picture rather than NaN for a bad aspect', () => {
      expect(cropWindow(0)).toEqual({ w: 1, h: 1 });
      expect(cropWindow(Number.NaN)).toEqual({ w: 1, h: 1 });
    });
  });

  describe('marqueeOrigin / focalFromOrigin', () => {
    // This is the pair the drag depends on: the pointer produces an
    // origin, the model stores a focal, and the marquee is drawn from
    // the focal again. If they are not exact inverses the marquee
    // creeps away from the pointer over a long drag.
    it('round-trips every focal through an origin and back', () => {
      const win = cropWindow(2).w; // 0.89
      for (const focal of [0, 0.1, 0.25, 0.5, 0.75, 0.9, 1]) {
        expect(focalFromOrigin(marqueeOrigin(focal, win), win)).toBeCloseTo(focal, 12);
      }
    });

    it('pins focal 0 to the left edge and focal 1 flush to the right', () => {
      const win = 0.6;
      expect(marqueeOrigin(0, win)).toBe(0);
      expect(marqueeOrigin(1, win)).toBeCloseTo(1 - win, 12);
      // Flush right means the marquee's far edge lands exactly on the
      // picture's — the property that keeps a drag from revealing a
      // strip of nothing.
      expect(marqueeOrigin(1, win) + win).toBeCloseTo(1, 12);
    });

    it('centres focal 0.5', () => {
      expect(marqueeOrigin(0.5, 0.5)).toBeCloseTo(0.25, 12);
    });

    it('clamps a focal outside 0..1 instead of letting the marquee leave the picture', () => {
      expect(marqueeOrigin(-3, 0.5)).toBe(0);
      expect(marqueeOrigin(4, 0.5)).toBeCloseTo(0.5, 12);
      expect(focalFromOrigin(-1, 0.5)).toBe(0);
      expect(focalFromOrigin(99, 0.5)).toBe(1);
    });

    it('answers centre rather than dividing by zero on an axis with no travel', () => {
      expect(focalFromOrigin(0, 1)).toBe(0.5);
      expect(marqueeOrigin(0.3, 1)).toBe(0);
    });
  });

  describe('hasTravel', () => {
    it('is false for a picture already card-shaped on that axis', () => {
      expect(hasTravel(1)).toBe(false);
      expect(hasTravel(cropWindow(CARD_ASPECT).h)).toBe(false);
    });
    it('is true as soon as there is a pixel to move on a 1000px stage', () => {
      expect(hasTravel(0.999)).toBe(true);
      expect(hasTravel(cropWindow(2).w)).toBe(true);
    });
  });

  describe('objectPosition', () => {
    // ONE place decides what null means. A second copy of `?? 0.5` in a
    // component is the copy that goes stale.
    it('renders null as the CSS default, centre', () => {
      expect(objectPosition(null, null)).toBe('50% 50%');
      expect(objectPosition(undefined, undefined)).toBe('50% 50%');
    });
    it('renders a stored pair as percentages', () => {
      expect(objectPosition(0.25, 0.75)).toBe('25% 75%');
      expect(objectPosition(0, 1)).toBe('0% 100%');
    });
    it('clamps rather than emitting an off-picture percentage', () => {
      expect(objectPosition(-2, 5)).toBe('0% 100%');
    });
  });
});
