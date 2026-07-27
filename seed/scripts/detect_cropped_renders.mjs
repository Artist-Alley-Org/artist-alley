#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Standalone cropped-render detector (#630).
//
// Scans a directory of PNGs and reports per-file EDGE COVERAGE: for each
// of the four edges, the percentage of pixels that are solid
// (alpha > 40). The signature of a cropped render is an edge with
// PARTIAL coverage — content visibly running off the frame:
//
//   ~0%     clean margin        artwork ends before the frame — fine
//   ~100%   full bleed          tilesheets, patterns, texture plates —
//                               legitimately touch every edge — fine
//   5–95%   partial coverage    something was cut mid-shape — FLAGGED
//
// The live #630 case reads: bottom 16.8%, right 39.8% — both squarely in
// the flag band; its fixed re-render reads 0% on all four.
//
// Deliberately standalone: no pipeline coupling, no manifest knowledge,
// only sharp (already this directory's one dependency; raw() pixel
// access, no node-canvas). It is run against a directory of already-
// rendered pool images to build a re-render list.
//
// Usage:
//   node detect_cropped_renders.mjs <dir> [--threshold-low 5] [--threshold-high 95]
//
// Output: one JSON document on stdout:
//   { scanned, flagged, thresholds, files: [ { file, width, height,
//     edges: {top,bottom,left,right}, flagged, flaggedEdges } ] }
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
    'detect_cropped_renders.mjs: cannot load sharp.\n' +
    '  Install it (no sudo required):  cd seed/scripts && npm install sharp\n' +
    `  Underlying error: ${err.message}`,
  );
  process.exit(3);
}

const args = process.argv.slice(2);
const dir = args.find((a) => !a.startsWith('--'));
if (!dir) {
  console.error('usage: node detect_cropped_renders.mjs <dir> [--threshold-low N] [--threshold-high N]');
  process.exit(2);
}
const flag = (name, dflt) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && args[i + 1] ? Number(args[i + 1]) : dflt;
};
// The band edges. Below low = clean margin; above high = full bleed;
// between = partial coverage, the crop signature.
const low = flag('threshold-low', 5);
const high = flag('threshold-high', 95);

// Solid = alpha above ~15% opacity — the same bar the #630 edge scans
// used. Anti-aliased fringes of a shape that legitimately ENDS at the
// frame sit far below it; a shape cut mid-body sits far above.
const ALPHA_SOLID = 40;

const files = readdirSync(dir)
  .filter((f) => f.toLowerCase().endsWith('.png'))
  .sort();

const report = { scanned: 0, flagged: 0, thresholds: { low, high, alpha: ALPHA_SOLID }, files: [] };

for (const file of files) {
  let data, info;
  try {
    ({ data, info } = await sharp(join(dir, file)).raw().toBuffer({ resolveWithObject: true }));
  } catch (err) {
    console.error(`  unreadable, skipped: ${file}: ${err.message}`);
    continue;
  }
  if (info.channels < 4) {
    // No alpha channel at all: every pixel is "solid", i.e. full bleed
    // by construction. Report it as such rather than dividing by zero.
    report.files.push({
      file, width: info.width, height: info.height,
      edges: { top: 100, bottom: 100, left: 100, right: 100 },
      flagged: false, flaggedEdges: [],
    });
    report.scanned++;
    continue;
  }
  const alphaAt = (x, y) => data[(y * info.width + x) * info.channels + 3];
  const pct = (coords) => {
    let n = 0;
    for (const [x, y] of coords) if (alphaAt(x, y) > ALPHA_SOLID) n++;
    return (100 * n) / coords.length;
  };
  const edges = {
    top: pct(Array.from({ length: info.width }, (_, x) => [x, 0])),
    bottom: pct(Array.from({ length: info.width }, (_, x) => [x, info.height - 1])),
    left: pct(Array.from({ length: info.height }, (_, y) => [0, y])),
    right: pct(Array.from({ length: info.height }, (_, y) => [info.width - 1, y])),
  };
  const flaggedEdges = Object.entries(edges)
    .filter(([, p]) => p > low && p < high)
    .map(([name]) => name);
  const flagged = flaggedEdges.length > 0;
  report.files.push({
    file, width: info.width, height: info.height,
    edges: Object.fromEntries(Object.entries(edges).map(([k, v]) => [k, Math.round(v * 10) / 10])),
    flagged, flaggedEdges,
  });
  report.scanned++;
  if (flagged) report.flagged++;
}

console.error(`scanned ${report.scanned} PNGs, flagged ${report.flagged}`);
process.stdout.write(JSON.stringify(report, null, 1) + '\n');
