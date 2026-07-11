// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Sprite Phase 9 — palette remap mockup.
//
// Takes the loaded sprite Image + a mapping of source colour → target
// colour, walks every pixel of the full sheet (not just the visible
// frame range — the alt-file is a sibling of the WHOLE asset, not a
// single frame slice), and produces a new PNG Blob with the colours
// swapped. Source rect for the alt-file is always the full sheet so
// existing companion-JSON frame coordinates stay valid against it.
//
// Two-pass: collect remap targets keyed by the packed RGBA so the
// inner loop is a single map lookup per pixel. Pixel-art sheets
// usually have ~10–50 distinct colours so the map stays tiny even
// for a 1024×1024 sheet.

export interface PaletteEntry {
  r: number; g: number; b: number; a: number;
}

export interface RemapPair {
  from: PaletteEntry;
  to: PaletteEntry;
}

/** Pack an RGBA quadruple into a single int32 — same encoding the
 *  analyzer uses, so map keys interop with palette extraction. */
function packRGBA(r: number, g: number, b: number, a: number): number {
  return ((r & 0xff) << 24) | ((g & 0xff) << 16) | ((b & 0xff) << 8) | (a & 0xff);
}

/** Hex parser used by the panel's <input type=color> + alpha slider.
 *  Accepts #rgb / #rrggbb / #rrggbbaa. Returns an opaque colour when
 *  alpha is absent. */
export function parseHexColor(hex: string): PaletteEntry | null {
  const s = hex.trim().replace(/^#/, '');
  if (s.length === 3) {
    const r = parseInt(s[0] + s[0], 16);
    const g = parseInt(s[1] + s[1], 16);
    const b = parseInt(s[2] + s[2], 16);
    if ([r, g, b].some(Number.isNaN)) return null;
    return { r, g, b, a: 255 };
  }
  if (s.length === 6 || s.length === 8) {
    const r = parseInt(s.slice(0, 2), 16);
    const g = parseInt(s.slice(2, 4), 16);
    const b = parseInt(s.slice(4, 6), 16);
    const a = s.length === 8 ? parseInt(s.slice(6, 8), 16) : 255;
    if ([r, g, b, a].some(Number.isNaN)) return null;
    return { r, g, b, a };
  }
  return null;
}

/** Pretty-print an entry as #rrggbb (alpha dropped — colour pickers
 *  don't speak alpha and the alpha is preserved per-pixel anyway). */
export function entryToHex(e: PaletteEntry): string {
  const h = (n: number) => n.toString(16).padStart(2, '0');
  return `#${h(e.r)}${h(e.g)}${h(e.b)}`;
}

/** Apply a colour remap to the full sheet image, return a PNG Blob.
 *  Pixels whose RGB matches a source entry get rewritten to the
 *  target's RGB; alpha is preserved (matters for semi-transparent
 *  pixels that should keep their original opacity). */
export async function applyPaletteRemap(
  img: HTMLImageElement,
  mapping: RemapPair[],
): Promise<Blob> {
  if (mapping.length === 0) throw new Error('applyPaletteRemap: empty mapping');
  const w = img.naturalWidth;
  const h = img.naturalHeight;
  if (w === 0 || h === 0) throw new Error('applyPaletteRemap: image not loaded');

  const work = new OffscreenCanvas(w, h);
  const ctx = work.getContext('2d', { willReadFrequently: true });
  if (!ctx) throw new Error('applyPaletteRemap: 2D context unavailable');
  ctx.imageSmoothingEnabled = false;
  ctx.drawImage(img, 0, 0);
  const imageData = ctx.getImageData(0, 0, w, h);
  const data = imageData.data;

  // Build a packed-key map for O(1) per-pixel lookups. We key on RGB
  // only (alpha excluded) so semi-transparent pixels still map by
  // colour identity — alpha is preserved as part of the per-pixel
  // copy below.
  const remap = new Map<number, PaletteEntry>();
  for (const pair of mapping) {
    remap.set(packRGBA(pair.from.r, pair.from.g, pair.from.b, 0), pair.to);
  }

  for (let i = 0; i < data.length; i += 4) {
    const a = data[i + 3];
    if (a === 0) continue;
    const key = packRGBA(data[i], data[i + 1], data[i + 2], 0);
    const t = remap.get(key);
    if (!t) continue;
    data[i]     = t.r;
    data[i + 1] = t.g;
    data[i + 2] = t.b;
    // Alpha intentionally untouched.
  }
  ctx.putImageData(imageData, 0, 0);
  return await work.convertToBlob({ type: 'image/png' });
}
