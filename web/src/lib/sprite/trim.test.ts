// Sprite Phase 11 — trimSourceRect tests. Pure ImageData → BoundingBox;
// happy-dom doesn't ship a canvas so we synthesise ImageData ourselves.

import { describe, expect, it } from 'vitest';
import { trimSourceRect } from './trim';

/** Build a fake ImageData of `w x h` RGBA pixels. `paint` lets the
 *  caller set per-pixel RGBA via (x,y) → [r,g,b,a]; pixels not
 *  touched default to fully transparent. */
function makeImageData(w: number, h: number, paint: (x: number, y: number) => [number, number, number, number] | null): ImageData {
  const data = new Uint8ClampedArray(w * h * 4);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const rgba = paint(x, y);
      if (!rgba) continue;
      const i = (y * w + x) * 4;
      data[i] = rgba[0]; data[i + 1] = rgba[1]; data[i + 2] = rgba[2]; data[i + 3] = rgba[3];
    }
  }
  return { width: w, height: h, data, colorSpace: 'srgb' } as ImageData;
}

describe('trimSourceRect', () => {
  it('shrinks to the tight bounding box of opaque pixels', () => {
    // 8x8 sheet, one frame covering the whole sheet, only the
    // 2..4 / 3..5 inner rect is opaque.
    const img = makeImageData(8, 8, (x, y) => (x >= 2 && x <= 4 && y >= 3 && y <= 5 ? [255, 0, 0, 255] : null));
    const got = trimSourceRect(img, { sx: 0, sy: 0, sw: 8, sh: 8 });
    expect(got).toEqual({ sx: 2, sy: 3, sw: 3, sh: 3 });
  });

  it('returns null when the source rect is fully transparent', () => {
    const img = makeImageData(4, 4, () => null);
    expect(trimSourceRect(img, { sx: 0, sy: 0, sw: 4, sh: 4 })).toBeNull();
  });

  it('clamps the source rect to the image bounds before scanning', () => {
    // Frame rect extends beyond the image; only the in-bounds slice
    // gets considered.
    const img = makeImageData(4, 4, (x, y) => (x === 3 && y === 3 ? [10, 20, 30, 255] : null));
    const got = trimSourceRect(img, { sx: 2, sy: 2, sw: 10, sh: 10 });
    expect(got).toEqual({ sx: 3, sy: 3, sw: 1, sh: 1 });
  });

  it('respects the alphaThreshold', () => {
    // Pixel at (2,2) has alpha 5; default threshold (1) keeps it,
    // raising threshold above 5 should drop it.
    const img = makeImageData(4, 4, (x, y) => (x === 2 && y === 2 ? [0, 0, 0, 5] : null));
    expect(trimSourceRect(img, { sx: 0, sy: 0, sw: 4, sh: 4 })).toEqual({ sx: 2, sy: 2, sw: 1, sh: 1 });
    expect(trimSourceRect(img, { sx: 0, sy: 0, sw: 4, sh: 4 }, 10)).toBeNull();
  });

  it('returns null when the rect has zero or negative area', () => {
    const img = makeImageData(8, 8, () => [255, 255, 255, 255]);
    expect(trimSourceRect(img, { sx: 0, sy: 0, sw: 0, sh: 0 })).toBeNull();
    expect(trimSourceRect(img, { sx: 5, sy: 5, sw: -1, sh: 1 })).toBeNull();
  });

  it('handles a frame in the middle of a multi-frame sheet', () => {
    // 16x8 sheet: frame at (4,0,8,8), the body is at (6,2)..(8,5).
    const img = makeImageData(16, 8, (x, y) => {
      if (x >= 6 && x <= 8 && y >= 2 && y <= 5) return [0, 128, 255, 255];
      return null;
    });
    const got = trimSourceRect(img, { sx: 4, sy: 0, sw: 8, sh: 8 });
    expect(got).toEqual({ sx: 6, sy: 2, sw: 3, sh: 4 });
  });
});
