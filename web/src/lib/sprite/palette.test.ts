// Sprite Phase 9 palette helpers. The remapping itself (applyPaletteRemap)
// runs against an OffscreenCanvas; covered via integration. These pure
// helpers parse + format hex colors and are the bit that frequently
// regresses when the panel's <input type=color> handlers get touched.

import { describe, expect, it } from 'vitest';
import { parseHexColor, entryToHex, type PaletteEntry } from './palette';

describe('parseHexColor', () => {
  it.each([
    ['#fff', { r: 255, g: 255, b: 255, a: 255 }],
    ['#F0A', { r: 255, g: 0, b: 170, a: 255 }],
    ['#ff8800', { r: 255, g: 136, b: 0, a: 255 }],
    ['ff8800', { r: 255, g: 136, b: 0, a: 255 }],
    ['#ff8800cc', { r: 255, g: 136, b: 0, a: 204 }],
    ['  #ff8800  ', { r: 255, g: 136, b: 0, a: 255 }],
  ])('parses %s', (input, want) => {
    expect(parseHexColor(input)).toEqual(want);
  });

  it.each(['', '#', '#ab', '#abcde', '#xyzxyz', '#1234567'])(
    'returns null for invalid input %s',
    (input) => {
      expect(parseHexColor(input)).toBeNull();
    },
  );
});

describe('entryToHex', () => {
  it.each<[PaletteEntry, string]>([
    [{ r: 255, g: 255, b: 255, a: 255 }, '#ffffff'],
    [{ r: 0, g: 0, b: 0, a: 255 }, '#000000'],
    [{ r: 255, g: 136, b: 0, a: 128 }, '#ff8800'], // alpha intentionally dropped
    [{ r: 1, g: 2, b: 3, a: 4 }, '#010203'],       // zero-pads each byte
  ])('formats %j as %s', (input, want) => {
    expect(entryToHex(input)).toBe(want);
  });

  // Round-trip — parsing the formatted hex of an entry must reproduce
  // the same RGB triple (alpha resets to 255 because entryToHex drops it).
  it('round-trips RGB through parseHexColor', () => {
    const original: PaletteEntry = { r: 12, g: 200, b: 45, a: 99 };
    const parsed = parseHexColor(entryToHex(original));
    expect(parsed).toEqual({ r: 12, g: 200, b: 45, a: 255 });
  });
});
