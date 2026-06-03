// Sprite auto-detection — pure-algorithm tests against synthetic
// ImageData. We don't render a real sheet; we paint the pixels by
// hand and verify the BFS + merge + sort pipeline catches the
// expected boxes. happy-dom doesn't ship a canvas so the tests
// don't try to use one.

import { describe, expect, it } from 'vitest';
import { detectSprites, sortBoxes, type DetectedBox, type DetectOptions } from './detect';

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

const defaultOpts: DetectOptions = {
  bgColor: null, bgTolerance: 0, mergeGap: 0,
  minW: 1, minH: 1, maxW: 9999, maxH: 9999,
};

describe('detectSprites', () => {
  it('finds two well-separated boxes via the alpha channel', () => {
    // 16x8 sheet with two 3x3 opaque squares.
    const img = makeImageData(16, 8, (x, y) => {
      if (x >= 1 && x <= 3 && y >= 1 && y <= 3) return [255, 0, 0, 255];
      if (x >= 10 && x <= 12 && y >= 1 && y <= 3) return [0, 0, 255, 255];
      return null;
    });
    const boxes = detectSprites(img, defaultOpts);
    expect(boxes).toHaveLength(2);
    expect(boxes).toEqual(expect.arrayContaining([
      { x: 1, y: 1, w: 3, h: 3 },
      { x: 10, y: 1, w: 3, h: 3 },
    ]));
  });

  it('treats fully transparent input as zero boxes', () => {
    const img = makeImageData(8, 8, () => null);
    expect(detectSprites(img, defaultOpts)).toHaveLength(0);
  });

  it('uses chroma-key + tolerance when bgColor is provided', () => {
    // Whole sheet is solid #f0f magenta; one foreground 2x2 block.
    const img = makeImageData(6, 6, (x, y) => {
      if (x >= 2 && x <= 3 && y >= 2 && y <= 3) return [0, 0, 0, 255];
      return [255, 0, 255, 255];
    });
    const boxes = detectSprites(img, { ...defaultOpts, bgColor: { r: 255, g: 0, b: 255 }, bgTolerance: 0 });
    expect(boxes).toEqual([{ x: 2, y: 2, w: 2, h: 2 }]);
  });

  it('merges boxes within mergeGap distance', () => {
    // Two 2x2 squares separated by a 2-pixel transparent gap. With
    // mergeGap=0 they're two boxes; with mergeGap=3 they collapse.
    const img = makeImageData(10, 6, (x, y) => {
      if (x >= 0 && x <= 1 && y >= 0 && y <= 1) return [255, 0, 0, 255];
      if (x >= 4 && x <= 5 && y >= 0 && y <= 1) return [0, 0, 255, 255];
      return null;
    });
    expect(detectSprites(img, { ...defaultOpts, mergeGap: 0 })).toHaveLength(2);
    const merged = detectSprites(img, { ...defaultOpts, mergeGap: 3 });
    expect(merged).toHaveLength(1);
    expect(merged[0]).toEqual({ x: 0, y: 0, w: 6, h: 2 });
  });

  it('filters out boxes below minW/minH', () => {
    // One 1x1 speckle + one 3x3 sprite. Min 2 should drop the speckle.
    const img = makeImageData(10, 6, (x, y) => {
      if (x === 0 && y === 0) return [255, 0, 0, 255];
      if (x >= 5 && x <= 7 && y >= 1 && y <= 3) return [0, 255, 0, 255];
      return null;
    });
    const boxes = detectSprites(img, { ...defaultOpts, minW: 2, minH: 2 });
    expect(boxes).toEqual([{ x: 5, y: 1, w: 3, h: 3 }]);
  });

  it('filters out boxes above maxW/maxH', () => {
    // The entire sheet is filled — one giant component. maxW < width
    // drops it, leaving nothing.
    const img = makeImageData(8, 8, () => [255, 0, 0, 255]);
    expect(detectSprites(img, { ...defaultOpts, maxW: 4, maxH: 4 })).toHaveLength(0);
  });
});

describe('sortBoxes', () => {
  const boxes: DetectedBox[] = [
    { x: 10, y: 0, w: 4, h: 4 },
    { x: 0, y: 0, w: 4, h: 4 },
    { x: 0, y: 10, w: 4, h: 4 },
    { x: 10, y: 10, w: 8, h: 8 }, // biggest area
  ];

  it('position: y first then x', () => {
    const out = sortBoxes(boxes, 'position');
    expect(out.map((b) => `${b.x},${b.y}`)).toEqual(['0,0', '10,0', '0,10', '10,10']);
  });

  it('sizeDesc: biggest area first', () => {
    const out = sortBoxes(boxes, 'sizeDesc');
    expect(out[0]).toEqual({ x: 10, y: 10, w: 8, h: 8 });
  });

  it('widthAsc / heightAsc', () => {
    const out = sortBoxes(boxes, 'widthAsc');
    expect(out[out.length - 1].w).toBe(8);
    const out2 = sortBoxes(boxes, 'heightAsc');
    expect(out2[out2.length - 1].h).toBe(8);
  });

  it('does not mutate the input array', () => {
    const original = [...boxes];
    sortBoxes(boxes, 'position');
    expect(boxes).toEqual(original);
  });

  it('animationRows clusters overlapping-y boxes together', () => {
    // Three boxes on row 0 (y in 0..4), three on row 1 (y in 10..14).
    // Within each row they should be x-sorted; rows in y order.
    const input: DetectedBox[] = [
      { x: 30, y: 0, w: 4, h: 4 },
      { x: 0, y: 12, w: 4, h: 4 },
      { x: 0, y: 0, w: 4, h: 4 },
      { x: 30, y: 10, w: 4, h: 4 },
      { x: 15, y: 0, w: 4, h: 4 },
      { x: 15, y: 11, w: 4, h: 4 },
    ];
    const out = sortBoxes(input, 'animationRows');
    expect(out.map((b) => `${b.x},${b.y}`)).toEqual([
      '0,0', '15,0', '30,0',
      '0,12', '15,11', '30,10',
    ]);
  });
});
