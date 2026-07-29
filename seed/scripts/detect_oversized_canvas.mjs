#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Standalone oversized-canvas detector (#672).
//
// Scans a directory of PNGs and reports, per file, the ALPHA BOUNDING
// BOX RATIO: the area of the tightest box containing every non-transparent
// pixel, as a percentage of the canvas area.
//
//   ~100%   content fills its frame — a tight render
//   ~50%    content fills half its frame — a visible margin all round
//   ~1%     a graphic marooned in a mostly-empty canvas — the #672 class
//
// WHY THIS AND NOT probe_render_loss.mjs. That script asks whether a
// render CUT anything off, by re-rendering the source with a wider frame
// and looking for artwork outside the original. #672 is the opposite
// failure: artwork far SMALLER than its canvas, entirely inside the
// frame, losing nothing — and therefore invisible to a loss probe. It is
// also, unlike loss, a property of the PNG alone, so this script needs no
// source and runs over a pool directory. The two see different failures;
// neither subsumes the other, and the pool needs both run over it.
//
// (There was a third, detect_cropped_renders.mjs, which flagged edges
// with 5-95% solid-pixel coverage. It was retired in #685: swept against
// ground truth over 9,504 threshold/alpha/edge-count combinations, not
// one of them found all six genuinely lossy pool files, and the best
// precision any of them reached was 0.043. Edge coverage is a property of
// the drawing's silhouette, not of whether it was cut.)
//
// WHY NOT BYTES PER PIXEL. Tried, and it produces false positives: the
// Flag-pack and Pattern-pack renders compress to almost nothing because
// they are flat colour, while measuring 100% content. Byte size is a good
// verifier and a terrible selector; the reliable selector is structural.
//
// WHAT THIS SCRIPT CANNOT DECIDE FOR YOU. A low ratio is not by itself a
// defect. Two different things produce one:
//
//   a) the render pipeline chose the canvas and chose it too big — our
//      bug, always wrong, fix it;
//   b) the SOURCE declares a padded canvas — a sprite or icon with a
//      deliberate transparent margin it needs for alignment. Trimming
//      those silently changes their composition.
//
// Measured on the kenney-hq pool, (b) goes as low as 15.6% (a colon glyph
// centred in a square icon canvas) and is entirely legitimate, while the
// #672 defect measured 1.3%. So the DEFAULT --min-ratio is 10: the widest
// band that flags nothing legitimate in the real pool while sitting ~8x
// above the defect. When sweeping only renders whose canvas the PIPELINE
// chose — SVG sources that declare no width/height/viewBox, where every
// pixel of margin is invented by us — run it far stricter, `--min-ratio
// 90`; those measure >=98% once correctly framed.
//
// Deliberately standalone: no pipeline coupling, no manifest knowledge,
// no dataset path baked in, only sharp (already this directory's one
// dependency).
//
// Usage:
//   node detect_oversized_canvas.mjs <dir> [--min-ratio 10]
//
// Output: one JSON document on stdout:
//   { scanned, flagged, thresholds, files: [ { file, width, height,
//     content: {x, y, width, height}, ratio, linearRatio,
//     margins: {top,bottom,left,right}, anchoredCorner, flagged } ] }
// Progress and per-file read errors go to stderr; unreadable files are
// reported and skipped rather than aborting the sweep. Exit 0 on a
// completed scan (flagged files are data, not an error), 2 on bad usage.

import { readdirSync } from 'node:fs';
import { join } from 'node:path';

let sharp;
try {
  sharp = (await import('sharp')).default;
} catch (err) {
  console.error(
    'detect_oversized_canvas.mjs: cannot load sharp.\n' +
    '  Install it (no sudo required):  cd seed/scripts && npm install sharp\n' +
    `  Underlying error: ${err.message}`,
  );
  process.exit(3);
}

const args = process.argv.slice(2);
const dir = args.find((a) => !a.startsWith('--'));
if (!dir) {
  console.error('usage: node detect_oversized_canvas.mjs <dir> [--min-ratio N]');
  process.exit(2);
}
const flag = (name, dflt) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && args[i + 1] ? Number(args[i + 1]) : dflt;
};
const minRatio = flag('min-ratio', 10);

// Any non-zero alpha counts toward the bounding box. This is deliberately
// NOT the alpha>40 "solid" bar detect_cropped_renders.mjs uses: there, a
// faint anti-aliased fringe on an edge is noise to be rejected; here, a
// faint fringe is real content and including it can only make the box
// bigger, i.e. the measurement more conservative about calling something
// broken. It also matches PIL's getbbox(), so a number this script prints
// is reproducible with a three-line Python check.
const ALPHA_PRESENT = 0;

const files = readdirSync(dir)
  .filter((f) => f.toLowerCase().endsWith('.png'))
  .sort();

const report = {
  scanned: 0,
  flagged: 0,
  thresholds: { minRatio, alpha: ALPHA_PRESENT },
  files: [],
};

const round1 = (n) => Math.round(n * 10) / 10;

for (const file of files) {
  let data, info;
  try {
    ({ data, info } = await sharp(join(dir, file)).raw().toBuffer({ resolveWithObject: true }));
  } catch (err) {
    console.error(`  unreadable, skipped: ${file}: ${err.message}`);
    continue;
  }
  const { width, height, channels } = info;

  let minX = width, minY = height, maxX = -1, maxY = -1;
  if (channels < 4) {
    // No alpha channel: every pixel is opaque, so the content box is the
    // whole canvas by construction. Report 100% rather than dividing by
    // an empty box.
    minX = 0; minY = 0; maxX = width - 1; maxY = height - 1;
  } else {
    for (let y = 0; y < height; y++) {
      for (let x = 0; x < width; x++) {
        if (data[(y * width + x) * channels + 3] > ALPHA_PRESENT) {
          if (x < minX) minX = x;
          if (x > maxX) maxX = x;
          if (y < minY) minY = y;
          if (y > maxY) maxY = y;
        }
      }
    }
  }

  const empty = maxX < 0;
  const cw = empty ? 0 : maxX - minX + 1;
  const ch = empty ? 0 : maxY - minY + 1;
  const ratio = (100 * cw * ch) / (width * height);
  // Reported alongside the area ratio because area falls off as the
  // SQUARE of the padding: a frame that is 30% too wide and 30% too tall
  // in each direction reads as ~50% by area, which sounds far worse than
  // it looks. The linear number is what a person sees as "margin".
  const linearRatio = (100 * Math.max(cw / width, ch / height));
  const margins = empty
    ? { top: 0, bottom: 0, left: 0, right: 0 }
    : { top: minY, bottom: height - 1 - maxY, left: minX, right: width - 1 - maxX };
  // A pipeline that guessed the canvas wrong tends to anchor content at
  // an origin corner and leave all the slack on the far sides; a source
  // with deliberate padding is usually roughly centred. Reported, never
  // gated on — it is a tie-breaker for a human triaging a flag list, not
  // a second threshold hiding in the output.
  const anchoredCorner = !empty &&
    (margins.left === 0 || margins.right === 0) &&
    (margins.top === 0 || margins.bottom === 0);

  const flagged = ratio < minRatio;
  report.files.push({
    file, width, height,
    content: { x: empty ? 0 : minX, y: empty ? 0 : minY, width: cw, height: ch },
    ratio: round1(ratio),
    linearRatio: round1(linearRatio),
    margins,
    anchoredCorner,
    flagged,
  });
  report.scanned++;
  if (flagged) report.flagged++;
}

console.error(`scanned ${report.scanned} PNGs, flagged ${report.flagged} below ${minRatio}% content`);
process.stdout.write(JSON.stringify(report, null, 1) + '\n');
