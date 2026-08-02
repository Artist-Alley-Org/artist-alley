// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The hover-scrub cue list (#835).
//
// WHAT THIS REPLACES. CardThumb used to walk a sprite sheet by assuming
// it was `cols x rows` frames and cycling all of them, with the grid
// picked from the FILE EXTENSION — 10x10 for a video, 6x6 for a 3D
// turntable. Two things were wrong with that:
//
//   1. The grid is not always full. The backend floors the cell interval
//      at 0.2s, so a clip shorter than grid x floor cannot fill the
//      sheet: a 5s video occupies 25 of 100 cells and ffmpeg's `tile`
//      filter pads the other 75 with black. Cycling all 100 meant three
//      quarters of the hover preview was blank, and nothing on the
//      client could tell padding from a genuinely dark frame.
//   2. The grid was hardcoded per extension, so a format that grew a
//      sheet but was not on the list — an animated GIF, since #832 —
//      could never scrub no matter what was in storage.
//
// Both dissolve if the client reads the cue file the backend already
// writes next to the sheet. `sprites.vtt` declares one cue per cell that
// ACTUALLY contains a frame, each carrying the exact pixel rect of that
// cell via the WebVTT media fragment syntax:
//
//   00:00:00.000 --> 00:00:00.299
//   sprites.jpg#xywh=0,0,240,134
//
// So the cue list is the frame count, the geometry, and the format gate,
// all from one 4 KB file the renderer already produced. Video, 3D and
// GIF become one code path, and short clips stop showing blanks WITHOUT
// re-rendering a single sheet.

/** One scrub cell: its pixel rect within the sprite sheet. */
export interface SpriteCue {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** `HH:MM:SS.mmm` or `MM:SS.mmm` → seconds. NaN when unparseable. */
function vttSeconds(stamp: string): number {
  const parts = stamp.trim().split(':');
  if (parts.length < 2 || parts.length > 3) return NaN;
  let total = 0;
  for (const p of parts) {
    const n = Number(p);
    if (!Number.isFinite(n)) return NaN;
    total = total * 60 + n;
  }
  return total;
}

const TIMING = /^\s*(\S+)\s+-->\s+(\S+)/;
const XYWH = /#xywh=(-?\d+),(-?\d+),(\d+),(\d+)/;

/**
 * Parse a sprites.vtt into the ordered list of cells to cycle.
 *
 * Deliberately tolerant — this is a display affordance, and a cue file
 * that half-parses should yield a shorter animation, never an exception
 * that takes the card down with it. Anything that does not carry a
 * readable `#xywh` rect is skipped.
 *
 * TWO CUES ARE DROPPED ON PURPOSE, and they are what makes this work on
 * sheets that were rendered before #835 and will never be re-rendered:
 *
 *   * ZERO-LENGTH cues (end <= start). The old writer ran the full grid
 *     and broke on `start >= duration` AFTER emitting that cue, so every
 *     short clip's VTT ends with one `05.000 --> 05.000` cue pointing at
 *     the first PADDING cell. Dropping it is the difference between a
 *     5s clip's hover ending on a black frame and not.
 *   * ZERO-AREA rects, which cannot be painted at all.
 *
 * The backend stopped emitting the first kind in #832's sprint, so on a
 * freshly rendered sheet this filter removes nothing. It exists for the
 * ~51 sheets already on any given install.
 */
export function parseSpriteVtt(text: string): SpriteCue[] {
  const cues: SpriteCue[] = [];
  const lines = text.split(/\r?\n/);
  for (let i = 0; i < lines.length; i++) {
    const timing = TIMING.exec(lines[i]);
    if (!timing) continue;
    const start = vttSeconds(timing[1]);
    const end = vttSeconds(timing[2]);
    // A cue whose window is empty describes no frame — see above.
    if (Number.isFinite(start) && Number.isFinite(end) && end <= start) continue;

    // The payload is the next non-empty line.
    let payload = '';
    for (let j = i + 1; j < lines.length && j < i + 4; j++) {
      if (lines[j].trim() !== '') {
        payload = lines[j];
        break;
      }
    }
    const rect = XYWH.exec(payload);
    if (!rect) continue;
    const w = Number(rect[3]);
    const h = Number(rect[4]);
    if (w <= 0 || h <= 0) continue;
    cues.push({ x: Number(rect[1]), y: Number(rect[2]), w, h });
  }
  return cues;
}

/**
 * Cue lists already fetched this session, keyed by asset id.
 *
 * Module-level and holding the PROMISE, not the result, so N cards
 * hovering the same asset — and one card hovered repeatedly — share a
 * single request. A cue file is a few KB and immutable for a given
 * asset's bytes, so there is nothing to invalidate.
 */
const cueCache = new Map<string, Promise<SpriteCue[]>>();

/**
 * Fetch and parse an asset's scrub cue list.
 *
 * ONLY CALL THIS WHEN THE SERVER HAS SAID THE FILE EXISTS
 * (`scrub_available`). It is not a probe: a blind fetch is a console 404
 * for every video whose expensive render has not drained yet, which is
 * the class #471 removed and there is no client-side way to silence one.
 *
 * A failure resolves to an empty list rather than rejecting — the caller
 * renders no scrub, which is the same graceful outcome as an asset that
 * never had a sheet.
 */
export function loadSpriteCues(assetId: string): Promise<SpriteCue[]> {
  const hit = cueCache.get(assetId);
  if (hit) return hit;
  const p = fetch(`/api/v1/assets/${assetId}/variants/sprites.vtt`)
    .then((r) => (r.ok ? r.text() : ''))
    .then(parseSpriteVtt)
    .catch(() => [] as SpriteCue[]);
  cueCache.set(assetId, p);
  return p;
}

/** Test seam — drops the session cache. */
export function _resetSpriteCueCache() {
  cueCache.clear();
}

/**
 * Where one cue's cell has to sit for a CSS background to show exactly
 * that cell, filling the box.
 *
 * Percentage units, not pixels, so the same values are correct at any
 * rendered tile size — the card is 60px in masonry and 320px in
 * thumbnail mode and neither knows the sheet's scale.
 *
 * The background-position percentage is the standard CSS one: 100% means
 * "align the image's right/bottom edge with the box's", so the divisor
 * is the sheet size MINUS one cell, not the sheet size. This reproduces
 * the old hardcoded `col * (100 / (cols - 1))` exactly for a full grid,
 * which is the arithmetic the pre-#835 tests pinned — it is just derived
 * from the sheet the server made instead of a constant.
 */
export function cueBackgroundStyle(
  cue: SpriteCue,
  sheetW: number,
  sheetH: number,
): { size: string; position: string } {
  const spanX = sheetW - cue.w;
  const spanY = sheetH - cue.h;
  return {
    size: `${(sheetW / cue.w) * 100}% ${(sheetH / cue.h) * 100}%`,
    position: `${spanX > 0 ? (cue.x / spanX) * 100 : 0}% ${spanY > 0 ? (cue.y / spanY) * 100 : 0}%`,
  };
}
