// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, expect, it } from 'vitest';

import {
  CARD_ASPECT,
  MAX_ZOOM,
  MIN_ZOOM,
  clampZoom,
  coverPlacement,
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

  // ── #1212: zoom ─────────────────────────────────────────────────────

  describe('clampZoom', () => {
    it('reads every flavour of "not stored" as the fit', () => {
      expect(clampZoom(null)).toBe(MIN_ZOOM);
      expect(clampZoom(undefined)).toBe(MIN_ZOOM);
      expect(clampZoom(NaN)).toBe(MIN_ZOOM);
    });
    it('holds the ladder-derived bounds rather than passing a bad value through', () => {
      expect(clampZoom(0.2)).toBe(MIN_ZOOM);
      expect(clampZoom(40)).toBe(MAX_ZOOM);
      expect(clampZoom(2.5)).toBe(2.5);
    });
    it('keeps an explicit 1, which is a value and not an absence', () => {
      // The distinction the editor's Reset depends on: null is "never
      // framed", 1 is "framed, and the answer was the fit".
      expect(clampZoom(1)).toBe(1);
    });
  });

  describe('cropWindow with a zoom', () => {
    it('is unchanged by a null zoom — the regression that matters', () => {
      for (const a of [0.5, 1, 1.5, 2, 4 / 3, 890 / 500]) {
        for (const card of [CARD_ASPECT, 4 / 3, 1]) {
          expect(cropWindow(a, card, null)).toEqual(cropWindow(a, card));
          expect(cropWindow(a, card, 1)).toEqual(cropWindow(a, card));
        }
      }
    });

    it('divides BOTH axes, which is what gives a pinned axis travel', () => {
      // A 1:2 portrait in the 890:500 card: at the fit the width is the
      // whole picture, so there is nothing to drag horizontally — the
      // exact complaint #1212 is about.
      const fit = cropWindow(0.5, CARD_ASPECT);
      expect(fit.w).toBe(1);
      expect(hasTravel(fit.w)).toBe(false);

      const zoomed = cropWindow(0.5, CARD_ASPECT, 2);
      expect(zoomed.w).toBeCloseTo(0.5, 12);
      expect(zoomed.h).toBeCloseTo(fit.h / 2, 12);
      expect(hasTravel(zoomed.w)).toBe(true);
      expect(hasTravel(zoomed.h)).toBe(true);
    });

    it('never asks for a window larger than the picture', () => {
      for (const z of [-5, 0, 0.25, 1, 4, 99]) {
        const win = cropWindow(0.5, CARD_ASPECT, z);
        expect(win.w).toBeLessThanOrEqual(1);
        expect(win.h).toBeLessThanOrEqual(1);
      }
    });
  });

  describe('coverPlacement', () => {
    it('emits the pre-zoom box exactly when nothing is stored', () => {
      // The byte-identical claim, asserted rather than assumed: an
      // absolutely positioned image at 100%/100%/0/0 with a centred
      // object-position occupies precisely the box its `inset-0
      // h-full w-full object-cover` predecessor did, and
      // transform-origin 50% 50% is the CSS default.
      const at = coverPlacement(null, null, null);
      expect(at).toBe(
        'position: absolute; max-width: none; max-height: none; width: 100%; height: 100%; left: 0%; top: 0%; ' +
          'object-position: 50% 50%; transform-origin: 50% 50%',
      );
      expect(coverPlacement(null, null, 1)).toBe(at);
      // A positioned-but-unzoomed cover keeps its old object-position
      // and gains nothing else.
      expect(coverPlacement(0.25, 0.75, null)).toBe(
        'position: absolute; max-width: none; max-height: none; width: 100%; height: 100%; left: 0%; top: 0%; ' +
          'object-position: 25% 75%; transform-origin: 50% 50%',
      );
    });

    it('lays the picture out z times the box and offsets it by the focal share', () => {
      expect(coverPlacement(0.5, 0.5, 2)).toBe(
        'position: absolute; max-width: none; max-height: none; width: 200%; height: 200%; left: -50%; top: -50%; ' +
          'object-position: 50% 50%; transform-origin: 50% 50%',
      );
      // Pinned hard left: no offset at all, so the left edge of the
      // enlarged picture lines up with the left edge of the box.
      expect(coverPlacement(0, 0, 4)).toBe(
        'position: absolute; max-width: none; max-height: none; width: 400%; height: 400%; left: 0%; top: 0%; ' +
          'object-position: 0% 0%; transform-origin: 12.5% 12.5%',
      );
    });

    it('AGREES WITH THE MARQUEE — the two are one equation, not two that match', () => {
      // The editor draws the marquee at marqueeOrigin(focal, fit/z) over
      // a window of fit/z. The surface positions the picture with two
      // independent halves: object-position inside a virtual box of z
      // times the real one, and left/top sliding that box across the
      // real one. Their sum has to be the marquee's origin, or the
      // preview lies about the card.
      for (const [naturalAspect, card] of [
        [0.5, CARD_ASPECT],
        [2, CARD_ASPECT],
        [0.75, 4 / 3],
        [3, 4 / 3],
      ] as const) {
        for (const z of [1, 1.5, 2.75, 4]) {
          for (const focal of [0, 0.25, 0.5, 0.9, 1]) {
            const fit = cropWindow(naturalAspect, card, 1);
            const win = cropWindow(naturalAspect, card, z);

            // What coverPlacement actually does, read back off its own
            // output rather than recomputed from the same expression.
            const css = coverPlacement(focal, focal, z);
            const left = -Number(/left: (-?[\d.]+)%/.exec(css)![1]) / 100;
            const objX = Number(/object-position: ([\d.]+)%/.exec(css)![1]) / 100;

            // object-position slides the picture over the FIT window's
            // travel. `left` is measured in BOX widths, and one box is
            // fit.w / z of the picture, so it contributes that much.
            const fromObjectPosition = objX * (1 - fit.w);
            const fromOffset = (left * fit.w) / z;
            expect(fromObjectPosition + fromOffset).toBeCloseTo(marqueeOrigin(focal, win.w), 10);
          }
        }
      }
    });

    it('clamps a stored value that is out of range instead of painting off-picture', () => {
      expect(coverPlacement(-3, 9, 400)).toBe(
        'position: absolute; max-width: none; max-height: none; width: 400%; height: 400%; left: 0%; top: -300%; ' +
          'object-position: 0% 100%; transform-origin: 12.5% 87.5%',
      );
    });
  });
});
