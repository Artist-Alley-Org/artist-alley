#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// SVG -> PNG batch rasteriser for the kenney-hq image pool (#604).
//
// Reads a JSON job list on stdin: [{src, dst, px}, ...] and renders each
// SVG so its LONG edge is `px`, preserving aspect ratio and alpha.
//
// Why sharp: it is libvips with prebuilt binaries, so `npm install sharp`
// needs no sudo, no system libvips, and no GUI. The Kenney launcher
// AppImage is an Electron shell around this same library plus a bundled
// ffmpeg and exposes no CLI, so there is nothing to automate there —
// driving sharp directly is both simpler and reproducible on a runner.
//
// Why the long edge rather than a fixed box: the pack's vectors are
// wildly non-square (flags are wide, characters are tall). Fitting each
// into a fixed square would pad most of them with transparent margin,
// and a tile that is 60% empty reads as a broken thumbnail in the grid.
//
// `fit: 'inside'` + `withoutEnlargement: false` means small vectors are
// rendered UP to the target — which is the whole point, since the source
// is resolution-independent. That is also why the caller must never gate
// selection on byte size: a 512px flat-colour render is routinely under
// 10 KB (see RULE 2 in kenney_hq.py).

import { readFileSync } from 'node:fs';
import { mkdirSync } from 'node:fs';
import { dirname } from 'node:path';

// WHAT FRAME DOES A FILE GET RENDERED INTO. All three answers, and the
// measurement that picks between them, live in svg_frame.mjs — shared
// with probe_render_loss.mjs so the script that CHOOSES the frame and the
// script that VERIFIES it cannot drift apart (they had; see #685). This
// file's job is to take that decision and produce bytes.
//
//   'declared'   the source states a canvas and the drawing fits inside
//                it. Handed to sharp untouched — output is byte-for-byte
//                what this script produced before #630, which is ~97% of
//                the pool and must stay that way.
//   'measured'   the source states no canvas at all (#630). librsvg would
//                guess one, and its guess under-measures curve extents:
//                pipes_green.svg spans 1440x600 units, guessed 1402x523,
//                so solid pixels ran off two edges of the render. Density
//                cannot fix it — the guess scales with density, so clipped
//                stays clipped at every resolution.
//   'overflow'   the source states a canvas AND THE CANVAS IS WRONG
//                (#685). The Flash/Animate sprite-sheet exports carry a
//                stale artboard: `viewBox="0 0 550 400"` over a drawing
//                spanning 2248x1120. librsvg honours it and clips the
//                rest, so the shipped render held 8.8% of its artwork and
//                looked like a legitimately cropped sheet. #630 never
//                looked, because the file "declared" a canvas.
//
// The last two both render to the drawing's measured extent, at a density
// chosen so the long edge lands near 2048px before the same resize-to-px
// the declared path uses, capped at 384 so a huge bbox cannot demand a
// gigapixel render.

let sharp;
try {
  sharp = (await import('sharp')).default;
} catch (err) {
  console.error(
    'rasterize_svg.mjs: cannot load sharp.\n' +
    '  Install it (no sudo required):  cd seed/scripts && npm install sharp\n' +
    `  Underlying error: ${err.message}`,
  );
  process.exit(3);
}

// Imported AFTER the sharp check so a missing dependency produces the
// message above rather than this module's terser throw.
const { pipelineFrame, withViewBox } = await import('./svg_frame.mjs');

const raw = readFileSync(0, 'utf8').trim();
if (!raw) process.exit(0);

let jobs;
try {
  jobs = JSON.parse(raw);
} catch (err) {
  console.error(`rasterize_svg.mjs: stdin is not valid JSON: ${err.message}`);
  process.exit(2);
}
if (!Array.isArray(jobs)) {
  console.error('rasterize_svg.mjs: stdin must be a JSON array of jobs');
  process.exit(2);
}

// Bounded concurrency. libvips is already threaded per image, so a large
// pool here mostly buys context switching; 8 keeps a 5k-file run busy
// without thrashing a runner that has other work to do.
const CONCURRENCY = 8;

let failed = 0;
let done = 0;

async function render({ src, dst, px }) {
  mkdirSync(dirname(dst), { recursive: true });

  const svgText = readFileSync(src, 'utf8');
  // ONE decision, made in svg_frame.mjs so probe_render_loss.mjs measures
  // loss against the same frame this renders into.
  const { rect, reason, declared } = await pipelineFrame(svgText);

  if (reason === 'declared') {
    // Dimensioned path — UNCHANGED, byte-for-byte, from before #630.
    // density scales the SVG rasterisation grid. Rendering a 24px icon at
    // the default 72dpi and then upscaling produces a blurred mess; asking
    // libvips for a high density up front makes it rasterise the vector at
    // the target resolution instead of resampling a small bitmap.
    await sharp(src, { density: 384 })
      .resize({ width: px, height: px, fit: 'inside', withoutEnlargement: false })
      .png({ compressionLevel: 9 })
      .toFile(dst);
    return;
  }

  if (reason === 'overflow') {
    // Loud on purpose: this is a source defect being worked around, and a
    // build that silently repairs things teaches nobody anything (#685).
    console.error(`  reframed: ${src}\n    content extends beyond its declared ` +
      `${declared.w}x${declared.h} canvas; rendering to the measured ` +
      `${Math.round(rect.w)}x${Math.round(rect.h)} extent (#685)`);
  }

  // PRECISE: render the measured frame at a density that lands the long
  // edge near 2048px before the same resize-to-px the dimensioned path
  // uses, capped at the pipeline's own 384 so a huge bbox cannot demand a
  // gigapixel render.
  const density = Math.min(384, (72 * 2048) / Math.max(rect.w, rect.h));
  await sharp(Buffer.from(withViewBox(svgText, rect)), { density })
    .resize({ width: px, height: px, fit: 'inside', withoutEnlargement: false })
    .png({ compressionLevel: 9 })
    .toFile(dst);
}

const queue = [...jobs];
async function worker() {
  for (;;) {
    const job = queue.shift();
    if (!job) return;
    try {
      await render(job);
      done += 1;
    } catch (err) {
      failed += 1;
      // Keep going: one malformed SVG in a 5,000-file pack must not
      // abort the whole build. The pool verifier downstream catches a
      // short pool by count, so a silent partial result is not possible.
      console.error(`  render failed: ${job.src}\n    ${err.message}`);
    }
  }
}

await Promise.all(Array.from({ length: CONCURRENCY }, worker));

console.error(`rasterised ${done}/${jobs.length} vectors` +
  (failed ? `, ${failed} failed` : ''));
process.exit(failed ? 1 : 0);
