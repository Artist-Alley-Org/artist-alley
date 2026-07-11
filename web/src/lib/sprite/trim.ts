// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Sprite Phase 11 — trim transparent margins.
//
// Given a frame's source rect on the sheet, walk its pixels and
// return the tightest bounding box around non-transparent content.
// Pure metadata: the source PNG never changes, the frame's sx / sy /
// sw / sh just point at a smaller rect.
//
// Used by:
//   - Frame ops "Trim" button (one frame at a time)
//   - Frame ops "Trim all" (every frame in metadataFrames)
//
// Caller reads the original sheet pixels once and passes the
// ImageData in, so a bulk trim doesn't re-getImageData per frame.

export interface TrimRect { sx: number; sy: number; sw: number; sh: number; }

/** Find the tight non-transparent bounding box inside the source
 *  rect. Returns null when the rect is fully transparent — callers
 *  should leave such a frame alone rather than collapsing to zero.
 *  alphaThreshold defaults to 1 (any > 0 alpha counts as content). */
export function trimSourceRect(
  imageData: ImageData,
  rect: TrimRect,
  alphaThreshold: number = 1,
): TrimRect | null {
  const { width, data } = imageData;
  const x0 = Math.max(0, rect.sx);
  const y0 = Math.max(0, rect.sy);
  const x1 = Math.min(imageData.width, rect.sx + rect.sw);
  const y1 = Math.min(imageData.height, rect.sy + rect.sh);
  if (x1 <= x0 || y1 <= y0) return null;

  let minX = x1, minY = y1, maxX = x0 - 1, maxY = y0 - 1;
  for (let y = y0; y < y1; y++) {
    const rowOffset = y * width * 4;
    for (let x = x0; x < x1; x++) {
      const a = data[rowOffset + x * 4 + 3];
      if (a < alphaThreshold) continue;
      if (x < minX) minX = x;
      if (x > maxX) maxX = x;
      if (y < minY) minY = y;
      if (y > maxY) maxY = y;
    }
  }
  if (maxX < minX || maxY < minY) return null;
  return { sx: minX, sy: minY, sw: maxX - minX + 1, sh: maxY - minY + 1 };
}

/** Read the full-sheet ImageData once — bulk trim avoids paying
 *  the OffscreenCanvas + getImageData cost per frame. */
export function readSheetImageData(img: HTMLImageElement): ImageData | null {
  const w = img.naturalWidth;
  const h = img.naturalHeight;
  if (w === 0 || h === 0) return null;
  const canvas: OffscreenCanvas | HTMLCanvasElement =
    typeof OffscreenCanvas !== 'undefined'
      ? new OffscreenCanvas(w, h)
      : Object.assign(document.createElement('canvas'), { width: w, height: h });
  const ctx = (canvas as OffscreenCanvas).getContext('2d', { willReadFrequently: true }) as
    | OffscreenCanvasRenderingContext2D
    | CanvasRenderingContext2D
    | null;
  if (!ctx) return null;
  ctx.drawImage(img, 0, 0);
  return ctx.getImageData(0, 0, w, h);
}
