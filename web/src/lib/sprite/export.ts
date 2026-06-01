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
}

export interface ExportOptions {
  /** Default frame duration (ms) for frames without their own. */
  defaultFrameMs: number;
  /** Optional integer scale factor — useful for sharing 32×32
   *  sprite animations as bigger GIFs without external scaling. */
  scale?: number;
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
  for (const f of frames) {
    ctx.clearRect(0, 0, outW, outH);
    const dx = Math.floor((outW - f.sw * scale) / 2);
    const dy = Math.floor((outH - f.sh * scale) / 2);
    ctx.drawImage(img, f.sx, f.sy, f.sw, f.sh, dx, dy, f.sw * scale, f.sh * scale);
    const data = ctx.getImageData(0, 0, outW, outH).data;
    // 256-colour palette per frame — gifenc's defaults give a clean
    // result on pixel art (limited palette to begin with). For
    // photographic input we'd want a shared palette across frames;
    // sprite sheets don't need that complexity.
    const palette = quantize(data, 256, { format: 'rgba4444' });
    const index = applyPalette(data, palette, 'rgba4444');
    enc.writeFrame(index, outW, outH, {
      palette,
      delay: Math.max(20, Math.round(f.duration ?? opts.defaultFrameMs)),
      transparent: true,
      transparentIndex: 0,
      dispose: 2,
    });
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
  // Uniform cell size = the biggest frame's W × H. Simpler than
  // bin-packing; matches what most sprite-sheet readers expect.
  // A future commit can add the packed/bin-packed option.
  let cellW = 0, cellH = 0;
  for (const f of frames) {
    if (f.sw > cellW) cellW = f.sw;
    if (f.sh > cellH) cellH = f.sh;
  }
  // Choose a roughly-square layout. cols = ceil(sqrt(N)).
  const cols = Math.max(1, Math.ceil(Math.sqrt(frames.length)));
  const rows = Math.ceil(frames.length / cols);
  const outW = cols * cellW;
  const outH = rows * cellH;

  const work = new OffscreenCanvas(outW, outH);
  const ctx = work.getContext('2d');
  if (!ctx) throw new Error('exportSpriteSheet: 2D context unavailable');
  ctx.imageSmoothingEnabled = false;
  ctx.clearRect(0, 0, outW, outH);

  const framesOut: Record<string, unknown> = {};
  for (let i = 0; i < frames.length; i++) {
    const f = frames[i];
    const col = i % cols;
    const row = Math.floor(i / cols);
    const dx = col * cellW + Math.floor((cellW - f.sw) / 2);
    const dy = row * cellH + Math.floor((cellH - f.sh) / 2);
    ctx.drawImage(img, f.sx, f.sy, f.sw, f.sh, dx, dy, f.sw, f.sh);
    const name = `frame_${String(i).padStart(3, '0')}.png`;
    framesOut[name] = {
      frame: { x: dx, y: dy, w: f.sw, h: f.sh },
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
    ctx.drawImage(img, f.sx, f.sy, f.sw, f.sh, 0, 0, f.sw, f.sh);
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
