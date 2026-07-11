// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Client-side sprite exporters — GIF / packed sheet / individual
// PNGs in a zip. All run in the browser, no backend round-trip,
// which keeps the federation story trivial: every node can
// export from any sprite asset it can read.
//
// The encoders chosen:
// - `gifenc` (MIT, ~10 kB) — modern GIF encoder using a quantised
//   palette per frame; faster than gif.js and produces smaller
//   files. The library exposes GIFEncoder(), quantize(), and
//   applyPalette() — we feed it the rendered RGBA, build a palette,
//   index the frame, write the delay, finish.
// - `fflate` (MIT, ~12 kB) — pure-JS deflate + zip. Used to bundle
//   individual frame PNGs into a single download.
//
// Frame definitions come in as the same shape SpriteSession uses:
// { sx, sy, sw, sh, duration? }. The caller passes the loaded
// sheet Image so we can sample pixels via an OffscreenCanvas.

import { GIFEncoder, quantize, applyPalette } from 'gifenc';
import { zipSync, strToU8 } from 'fflate';

export interface ExportFrame {
  /** Source rect in the sheet. */
  sx: number; sy: number; sw: number; sh: number;
  /** Per-frame duration in ms. Falls back to the session FPS when
   *  the metadata didn't carry per-frame timing. */
  duration?: number;
  /** Per-frame transforms (Phase 8). Applied during encode so a
   *  mirrored / rotated frame exports as those pixels — the source
   *  sheet stays untouched. */
  flipH?: boolean;
  flipV?: boolean;
  rotate?: 0 | 90 | 180 | 270;
}

/** Draw a source rect with optional flip + rotate around the dest
 *  centre. Mirrors the canvas-side helper in SpriteCanvas so render
 *  and export stay pixel-identical. */
function drawTransformed(
  ctx: OffscreenCanvasRenderingContext2D | CanvasRenderingContext2D,
  img: CanvasImageSource,
  f: ExportFrame,
  dx: number, dy: number, dw: number, dh: number,
): void {
  const flipH = !!f.flipH;
  const flipV = !!f.flipV;
  const rot = f.rotate ?? 0;
  if (!flipH && !flipV && rot === 0) {
    ctx.drawImage(img, f.sx, f.sy, f.sw, f.sh, dx, dy, dw, dh);
    return;
  }
  ctx.save();
  ctx.translate(dx + dw / 2, dy + dh / 2);
  if (rot !== 0) ctx.rotate((rot * Math.PI) / 180);
  ctx.scale(flipH ? -1 : 1, flipV ? -1 : 1);
  ctx.drawImage(img, f.sx, f.sy, f.sw, f.sh, -dw / 2, -dh / 2, dw, dh);
  ctx.restore();
}

export interface ExportOptions {
  /** Default frame duration (ms) for frames without their own. */
  defaultFrameMs: number;
  /** Optional integer scale factor — useful for sharing 32×32
   *  sprite animations as bigger GIFs without external scaling. */
  scale?: number;
  /** Per-frame progress callback (0..1). Called as each frame
   *  finishes encoding so the panel can show a progress bar. */
  onProgress?: (done: number, total: number) => void;
}

// ── GIF ──────────────────────────────────────────────────────────

/** Encode an animated GIF from the supplied frames. Returns a Blob
 *  the caller can hand to URL.createObjectURL + an <a download>. */
export async function exportGIF(
  img: HTMLImageElement,
  frames: ExportFrame[],
  opts: ExportOptions,
): Promise<Blob> {
  if (frames.length === 0) throw new Error('exportGIF: no frames to encode');
  const scale = Math.max(1, Math.floor(opts.scale ?? 1));
  // Pick the GIF's canvas dims from the widest / tallest frame —
  // frames in non-uniform sprite sheets can differ in size. We
  // centre each frame within the canvas at encode time.
  let maxW = 0, maxH = 0;
  for (const f of frames) {
    if (f.sw > maxW) maxW = f.sw;
    if (f.sh > maxH) maxH = f.sh;
  }
  const outW = maxW * scale;
  const outH = maxH * scale;

  const work = new OffscreenCanvas(outW, outH);
  const ctx = work.getContext('2d', { willReadFrequently: true });
  if (!ctx) throw new Error('exportGIF: 2D context unavailable');
  ctx.imageSmoothingEnabled = false;

  const enc = GIFEncoder();
  let done = 0;
  // Yield to the event loop every 10 frames so the UI's "Saving…"
  // text can repaint and the user sees progress instead of a
  // frozen tab. Without this, a 150-frame encode locks the main
  // thread for ~4 s and the panel buttons look hung.
  const YIELD_EVERY = 10;
  for (const f of frames) {
    ctx.clearRect(0, 0, outW, outH);
    const dx = Math.floor((outW - f.sw * scale) / 2);
    const dy = Math.floor((outH - f.sh * scale) / 2);
    drawTransformed(ctx, img, f, dx, dy, f.sw * scale, f.sh * scale);
    const data = ctx.getImageData(0, 0, outW, outH).data;
    // Quantise with oneBitAlpha so transparent pixels are
    // explicitly preserved AND the encoder can pick a guaranteed
    // transparent palette entry. Without this, the quantiser picks
    // colours by RGB distance only and the transparentIndex we
    // pass below points at whatever happens to be at index 0 —
    // commonly an opaque colour, stamping visible holes through
    // the sprite where its real pixels matched that index.
    //
    // 255 max colours (not 256) leaves headroom for the reserved
    // transparent entry. clearAlphaThreshold 128 routes alpha < 128
    // to the transparent slot — matches what most sprite sheets
    // mean by "this pixel is empty."
    const palette = quantize(data, 255, {
      format: 'rgba4444',
      oneBitAlpha: true,
      clearAlpha: true,
      clearAlphaThreshold: 128,
    });
    const index = applyPalette(data, palette, 'rgba4444');
    // gifenc places the transparent entry at the last index when
    // oneBitAlpha is on. Find it (alpha === 0) rather than
    // hard-coding so future library changes don't silently break.
    let transparentIndex = palette.findIndex((p) => p[3] === 0);
    if (transparentIndex < 0) transparentIndex = 0;
    enc.writeFrame(index, outW, outH, {
      palette,
      delay: Math.max(20, Math.round(f.duration ?? opts.defaultFrameMs)),
      transparent: true,
      transparentIndex,
      dispose: 2,
    });
    done++;
    opts.onProgress?.(done, frames.length);
    if (done % YIELD_EVERY === 0) {
      await new Promise((res) => setTimeout(res, 0));
    }
  }
  enc.finish();
  // gifenc returns a Uint8Array<ArrayBufferLike>; Blob's typings
  // want a strict ArrayBuffer-backed view. Copy into a fresh
  // Uint8Array<ArrayBuffer> to satisfy TS without a runtime cost
  // beyond a single memcpy.
  const out = enc.bytesView();
  const buf = new Uint8Array(out.byteLength);
  buf.set(out);
  return new Blob([buf], { type: 'image/gif' });
}

// ── Packed sprite sheet ─────────────────────────────────────────

/** Pack frames into a single PNG laid out as a row-major grid,
 *  with the cell size matching the largest frame. Returns the PNG
 *  blob + a TexturePacker JSON Hash blob describing the frame
 *  rects on the packed output. */
export async function exportSpriteSheet(
  img: HTMLImageElement,
  frames: ExportFrame[],
): Promise<{ png: Blob; json: Blob }> {
  if (frames.length === 0) throw new Error('exportSpriteSheet: no frames');

  // Shelf-pack — sort frames by height descending, then place
  // left-to-right on rows. When the next frame doesn't fit on the
  // current row's remaining width, open a new row at the previous
  // row's bottom. Width is bounded by max(maxFrameW, 512) so a
  // single huge frame doesn't force a 1-px-wide sheet, and small
  // sprites get many per row. Way more efficient than the old
  // uniform-cell layout, which wasted ~90% of the canvas when
  // frame sizes varied.
  //
  // Trade-off: shelf-pack is greedy + not optimal. A proper
  // bin-pack (MaxRects, Skyline) would be tighter on irregular
  // size distributions but adds material code complexity. Shelf
  // is enough for the common sprite-sheet case where many frames
  // share similar heights anyway.
  const idx = frames.map((_, i) => i);
  idx.sort((a, b) => frames[b].sh - frames[a].sh);
  let maxFrameW = 0;
  for (const f of frames) if (f.sw > maxFrameW) maxFrameW = f.sw;
  const shelfMaxW = Math.max(maxFrameW, 512);
  // First pass: compute placements + the resulting sheet bounds.
  const placements = new Array<{ x: number; y: number }>(frames.length);
  let cursorX = 0;
  let cursorY = 0;
  let rowH = 0;
  let outW = 0;
  for (const i of idx) {
    const f = frames[i];
    if (cursorX + f.sw > shelfMaxW && cursorX > 0) {
      cursorY += rowH;
      cursorX = 0;
      rowH = 0;
    }
    placements[i] = { x: cursorX, y: cursorY };
    cursorX += f.sw;
    if (f.sh > rowH) rowH = f.sh;
    if (cursorX > outW) outW = cursorX;
  }
  const outH = cursorY + rowH;

  const work = new OffscreenCanvas(outW, outH);
  const ctx = work.getContext('2d');
  if (!ctx) throw new Error('exportSpriteSheet: 2D context unavailable');
  ctx.imageSmoothingEnabled = false;
  ctx.clearRect(0, 0, outW, outH);

  const framesOut: Record<string, unknown> = {};
  for (let i = 0; i < frames.length; i++) {
    const f = frames[i];
    const p = placements[i];
    drawTransformed(ctx, img, f, p.x, p.y, f.sw, f.sh);
    const name = `frame_${String(i).padStart(3, '0')}.png`;
    framesOut[name] = {
      frame: { x: p.x, y: p.y, w: f.sw, h: f.sh },
      rotated: false,
      trimmed: false,
      spriteSourceSize: { x: 0, y: 0, w: f.sw, h: f.sh },
      sourceSize: { w: f.sw, h: f.sh },
      duration: f.duration ?? undefined,
    };
  }
  const png = await work.convertToBlob({ type: 'image/png' });
  const meta = {
    frames: framesOut,
    meta: {
      app: 'artist-alley sprite exporter',
      version: '1',
      image: 'spritesheet.png',
      size: { w: outW, h: outH },
      scale: '1',
    },
  };
  const json = new Blob([JSON.stringify(meta, null, 2)], { type: 'application/json' });
  return { png, json };
}

// ── Individual PNGs (zip) ───────────────────────────────────────

/** Render every frame as an independent PNG, bundle them into a
 *  zip. Files are named `frame_000.png`, `frame_001.png`, … so
 *  importers that walk a sequence pick them up in order. */
export async function exportPNGsZip(
  img: HTMLImageElement,
  frames: ExportFrame[],
): Promise<Blob> {
  if (frames.length === 0) throw new Error('exportPNGsZip: no frames');
  const files: Record<string, Uint8Array> = {};
  for (let i = 0; i < frames.length; i++) {
    const f = frames[i];
    const work = new OffscreenCanvas(f.sw, f.sh);
    const ctx = work.getContext('2d');
    if (!ctx) throw new Error('exportPNGsZip: 2D context unavailable');
    ctx.imageSmoothingEnabled = false;
    drawTransformed(ctx, img, f, 0, 0, f.sw, f.sh);
    const blob = await work.convertToBlob({ type: 'image/png' });
    const bytes = new Uint8Array(await blob.arrayBuffer());
    files[`frame_${String(i).padStart(3, '0')}.png`] = bytes;
  }
  // A README inside the zip explains the frame ordering convention
  // so a user opening it cold knows what they're looking at.
  files['README.txt'] = strToU8(
    'Exported by artist-alley sprite tools.\n' +
    `${frames.length} frames, ordered by filename.\n`,
  );
  const zipped = zipSync(files);
  return new Blob([zipped], { type: 'application/zip' });
}

// ── Helper — kick the browser to download a Blob ─────────────────

export function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Defer revoke so older browsers finish the download handshake
  // before the URL is invalidated.
  setTimeout(() => URL.revokeObjectURL(url), 4000);
}
