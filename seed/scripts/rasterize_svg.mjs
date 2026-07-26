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
  // density scales the SVG rasterisation grid. Rendering a 24px icon at
  // the default 72dpi and then upscaling produces a blurred mess; asking
  // libvips for a high density up front makes it rasterise the vector at
  // the target resolution instead of resampling a small bitmap.
  await sharp(src, { density: 384 })
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
