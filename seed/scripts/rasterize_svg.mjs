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

// #630 — dimensionless SVGs (no width/height/viewBox on the root; ~765
// of the pack's 5,279 vectors) make librsvg GUESS a canvas, and the
// guess under-measures curve extents, clipping content at render time.
// pipes_green.svg: true extent 1440x600 units, guessed 1402x523 — a
// 512x191 render with solid pixels running off two edges. Density can't
// fix it: the guessed canvas scales with density, so clipped stays
// clipped at every resolution.
//
// The fix is search-refine-precise, for dimensionless files ONLY:
//   1. SEARCH: patch a large negative-inclusive viewBox onto the root
//      and render small (1024px over 8192 units = 8 units/px), then
//      measure the content bounding box from the raw alpha channel.
//      Negative-inclusive because "the observed clipping is
//      positive-quadrant" is an observation, not a guarantee — content
//      at negative coordinates is found, not assumed away. If the bbox
//      touches the probe edge the canvas widens 4x and probes once
//      more; still touching after that is a per-file failure, loudly.
//   2. REFINE (#672): re-probe the region the search found, so the
//      measurement resolution scales with the DRAWING rather than with
//      the search canvas — see the block below.
//   3. PRECISE: re-render with the viewBox set to the refined bbox plus
//      a 2-probe-pixel safety margin (covers sub-probe-pixel edges), at
//      a density chosen to land the long edge at ~2048px before the
//      same resize-to-px the dimensioned path uses, capped at the
//      pipeline's own 384 so a huge bbox cannot demand a gigapixel
//      render.
//
// NEVER combine the big probe viewBox with density 384 — 8192 units at
// 384dpi is a ~44k-pixel edge, gigabytes of RGBA. The probe density is
// derived from the canvas so the probe is always ~1024px.
//
// #672 — WHY THE REFINE PASS EXISTS. The search probe spreads 1024
// pixels over an 8192-unit canvas, so one probe pixel is 8 user units
// and the 2-pixel safety margin is 16 user units NO MATTER HOW BIG THE
// DRAWING IS. That is the ~1% of frame the original comment claimed
// only when the drawing nearly fills the search canvas. The Kenney
// splat pack's vectors span ~100 units, so 16 units became a ~15%
// margin PER SIDE and the artwork filled 50% of its own frame — a
// splat floating in a box on the browse grid. Re-probing the found
// region (padded 2% so content cannot sit on the refine frame's edge)
// puts 1024 pixels across the DRAWING, so the same 2-pixel margin is
// ~0.2% of the long edge and the frame is tight at any extent.
// This is the same "measure the real extent" principle as #630, applied
// at a resolution that is actually proportionate to what is measured.
//
// Dimensioned SVGs — anything with a viewBox, or with both width and
// height — take EXACTLY the code path this script always had, so their
// output bytes are unchanged.

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

/** True iff the root <svg> tag declares no viewBox and not both of
 *  width/height — the class librsvg has to guess a canvas for. */
function isDimensionless(svgText) {
  const m = svgText.match(/<svg\b[^>]*>/);
  if (!m) return false; // not detectably an svg; let sharp error normally
  const tag = m[0];
  const hasViewBox = /\bviewBox\s*=/.test(tag);
  const hasWH = /\bwidth\s*=/.test(tag) && /\bheight\s*=/.test(tag);
  return !hasViewBox && !hasWH;
}

function withViewBox(svgText, vb) {
  return svgText.replace(/<svg\b/, `<svg viewBox="${vb.join(' ')}"`);
}

/** Measure the content bounding box (in SVG user units) by rendering a
 *  probe frame over `rect` ({x, y, w, h} in user units) and scanning the
 *  alpha channel. The probe's LONG edge is always ~1024px, so one probe
 *  pixel is `max(w,h)/1024` units and the returned safety margin is two
 *  of those — proportional to the region being measured, which is the
 *  whole point of the refine pass (#672). Returns null when the frame
 *  contains no content at all. */
async function measureBBox(svgText, rect) {
  const probePx = 1024;
  const units = Math.max(rect.w, rect.h);
  const unitsPerPx = units / probePx;
  const patched = withViewBox(svgText, [rect.x, rect.y, rect.w, rect.h]);
  const { data, info } = await sharp(Buffer.from(patched), {
    density: (72 * probePx) / units,
  }).raw().toBuffer({ resolveWithObject: true });
  let minX = Infinity, minY = Infinity, maxX = -1, maxY = -1;
  for (let y = 0; y < info.height; y++) {
    for (let x = 0; x < info.width; x++) {
      if (data[(y * info.width + x) * info.channels + 3] > 0) {
        if (x < minX) minX = x;
        if (x > maxX) maxX = x;
        if (y < minY) minY = y;
        if (y > maxY) maxY = y;
      }
    }
  }
  if (maxX < 0) return null;
  const touchesEdge = minX === 0 || minY === 0 ||
    maxX === info.width - 1 || maxY === info.height - 1;
  const margin = 2 * unitsPerPx;
  return {
    touchesEdge,
    x: rect.x + minX * unitsPerPx - margin,
    y: rect.y + minY * unitsPerPx - margin,
    w: (maxX - minX + 1) * unitsPerPx + 2 * margin,
    h: (maxY - minY + 1) * unitsPerPx + 2 * margin,
  };
}

/** The square, negative-inclusive search frames the first pass uses. */
const searchRect = (origin, units) => ({ x: origin, y: origin, w: units, h: units });

/** Grow a rect by `frac` of its long edge on every side. The refine probe
 *  runs over this, not over the raw search bbox, so content is guaranteed
 *  to sit off the refine frame's edges — a bbox that touched the frame
 *  would mean "possibly still clipped", which is exactly the ambiguity
 *  #630 exists to remove. */
function padRect(rect, frac) {
  const pad = Math.max(rect.w, rect.h) * frac;
  return { x: rect.x - pad, y: rect.y - pad, w: rect.w + 2 * pad, h: rect.h + 2 * pad };
}

async function render({ src, dst, px }) {
  mkdirSync(dirname(dst), { recursive: true });

  const svgText = readFileSync(src, 'utf8');
  if (isDimensionless(svgText)) {
    // Search canvas 1: -2048..6144 (negative-inclusive). Canvas 2, if the
    // content runs off the first: 4x wider.
    let bbox = await measureBBox(svgText, searchRect(-2048, 8192));
    if (bbox && bbox.touchesEdge) bbox = await measureBBox(svgText, searchRect(-8192, 32768));
    if (!bbox) throw new Error('dimensionless SVG rendered no content in the probe frame');
    if (bbox.touchesEdge) {
      throw new Error('content exceeds a 32768-unit probe canvas; refusing to render a silently-cropped frame');
    }
    // REFINE (#672): re-measure at the drawing's own scale so the safety
    // margin is proportional. A refine probe that somehow finds nothing,
    // or finds content on its own edge, is not trusted — the search bbox
    // is already a correct-but-loose frame, so fall back to it rather
    // than risk a tighter frame that clips.
    const refined = await measureBBox(svgText, padRect(bbox, 0.02));
    if (refined && !refined.touchesEdge) bbox = refined;

    const longUnits = Math.max(bbox.w, bbox.h);
    const density = Math.min(384, (72 * 2048) / longUnits);
    await sharp(Buffer.from(withViewBox(svgText, [bbox.x, bbox.y, bbox.w, bbox.h])), { density })
      .resize({ width: px, height: px, fit: 'inside', withoutEnlargement: false })
      .png({ compressionLevel: 9 })
      .toFile(dst);
    return;
  }

  // Dimensioned path — UNCHANGED, byte-for-byte, from before #630.
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
