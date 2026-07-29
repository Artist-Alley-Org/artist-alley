#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Does a pool render actually LOSE content? (#572)
//
// THIS IS THE CROP GATE. If a brief or an acceptance criterion wants
// "prove the renders are not cropped", it means this script. Its
// predecessor, detect_cropped_renders.mjs (#630), was retired in #685 —
// see WHY THE EDGE HEURISTIC IS GONE below. The surviving trio is:
//
//   probe_render_loss.mjs     is artwork missing?   needs the SOURCE
//   detect_oversized_canvas   is the frame too big? PNG only
//   kenney_hq.py verify       is the pool complete? manifest
//
// WHY THIS EXISTS. detect_cropped_renders.mjs flagged any edge whose
// solid-pixel coverage landed between 5% and 95% — meant as the signature
// of a shape cut mid-body. After #679 tightened the frame around the
// drawing, that band also catches every render whose artwork legitimately
// REACHES its border, which is what a tight frame is. It fired on 41% of
// a pool with six real defects in it.
//
// WHY THE EDGE HEURISTIC IS GONE RATHER THAN RETUNED (#685). It was swept
// against ground truth — this script's own verdict — over 9,504
// combinations of band edges, alpha cutoff (1..250, i.e. counting or
// discounting anti-aliased fringe), how many edges must agree, and an
// absolute solid-pixel floor so small sprites could not swing on a
// handful of pixels. Result: NOT ONE of the 9,504 found all six lossy
// files, and the best precision reached anywhere in the sweep was 0.043.
// The reason is structural, not a matter of tuning — edge coverage
// measures the drawing's SILHOUETTE where it meets the frame, and a
// silhouette is the same whether the frame was correct or cut. The two
// worst files in the pool, at 8.8% and 19.7% of their artwork retained,
// score as ordinary full-bleed sheets.
//
// So this asks the only question that separates them, and it needs the
// source to do it: is there any artwork OUTSIDE the frame we rendered?
//
// METHOD. Re-render the source with its viewBox expanded by `--pad`
// (default 12%) on all four sides. Everything the normal render produced
// now sits in the middle; the added ring is territory the normal render
// could not show. Count solid pixels in that ring:
//
//     ring empty      nothing was lost — the render is tight, not cropped
//     ring populated  content exists beyond the frame — a real crop
//
// WHAT THIS COVERS, AND WHY IT IS NOT JUST viewBox SOURCES (#685). A
// source declares its canvas one of three ways, and the pool splits
// very unevenly between them:
//
//   viewBox                 30 / 1031   expand the declared box
//   width+height, no viewBox 776 / 1031  implicit viewBox "0 0 w h"
//   neither                 225 / 1031  no canvas to expand — skipped
//
// The middle class is 75% of the pool and rasterises down librsvg's
// DIMENSIONED path, using width/height as the canvas exactly as if a
// viewBox said so. The pack writes them unitless ("32", not "32px"), so
// "0 0 width height" is that canvas, not an approximation. Probing only
// the 30 explicit-viewBox files would have covered 2.9% of the pool and
// called it a gate; it is also how #685's genuinely lossy files hid.
//
// The third class has no canvas to expand, so this probe used to skip it
// outright. It no longer needs to: the frame comes from svg_frame.mjs's
// `pipelineFrame`, which is the same function rasterize_svg.mjs renders
// with, so "the frame the render used" is knowable for every source and
// all 1031 get checked. `skipped` now means only that a source declares a
// canvas in a unit this cannot resolve (`2cm`), which the pool has none
// of. Sources whose measured extent cannot be trusted are reported LOST
// with an error, not skipped — a frame nobody can verify is not a pass.
//
// Usage:
//   # the whole pool — this is the gate
//   node probe_render_loss.mjs --pack <bundle-root> \
//        --pool-manifest seed/upgrades/kenney-hq-pool.json
//   # audit the SOURCES instead: which declare a canvas that cuts them?
//   node probe_render_loss.mjs --pack <bundle-root> --frame declared \
//        --pool-manifest seed/upgrades/kenney-hq-pool.json
//   # named sources
//   node probe_render_loss.mjs --pack <bundle-root> --sources a.svg,b.svg
//   # triage a flag list from detect_oversized_canvas.mjs
//   node probe_render_loss.mjs --pack <bundle-root> --from-report r.json \
//        --pool-manifest seed/upgrades/kenney-hq-pool.json [--limit 40]
//
// Output: JSON on stdout { checked, lost, skipped, files: [...] }.
// Exit 0 when nothing lost content, 1 when something did.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

let sharp;
try {
  sharp = (await import('sharp')).default;
} catch (err) {
  console.error(`probe_render_loss.mjs: cannot load sharp: ${err.message}`);
  process.exit(3);
}

// The SAME framing decision rasterize_svg.mjs renders with. Measuring
// loss against a frame the pipeline does not actually use is how this
// script would have reported every #685-repaired file as broken forever.
const { pipelineFrame, padRect, withViewBox } = await import('./svg_frame.mjs');

const args = process.argv.slice(2);
const opt = (name, dflt) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && args[i + 1] !== undefined ? args[i + 1] : dflt;
};
const pack = opt('pack', null);
if (!pack) {
  console.error('usage: node probe_render_loss.mjs --pack <bundle-root> ' +
                '(--pool-manifest m.json [--from-report r.json] | --sources a.svg,b.svg)');
  process.exit(2);
}
const pad = Number(opt('pad', 12)) / 100;
const px = Number(opt('px', 512));
const limit = Number(opt('limit', 0));
// Material loss, not any loss. A tight render of a dimensioned source
// routinely puts a one-pixel anti-aliased fringe outside the declared
// viewBox — measured at 0.08-0.09% of the ring on the Toon Characters
// sheets, i.e. ~60 pixels out of 68,000. Visible clipping starts two
// orders of magnitude higher (2.3-25%), so the bar sits between them.
const minRingPct = Number(opt('min-ring-pct', 1));
// GUARD BAND (#685). The inner boundary of the ring lands on a fractional
// pixel — at px=512 and pad=12% the original region ends at x=462.45 —
// so rounding it to a whole pixel puts a one-pixel sliver of the ORIGINAL
// content inside the ring. On a full-bleed source (a flag, a pattern
// tile, a brick) that sliver is the entire perimeter: measured 1652 ring
// pixels on Flag Pack/AF.svg, every single one at distance exactly 1, for
// a ringPct of 1.79% against a 1% bar. That is how 410 healthy files
// landed in the 1-5% band and made the probe read as "half the pool is
// lossy". Widening the excluded region by a couple of pixels costs ~4% of
// the ring's area and removes the artefact entirely; real losses measure
// 7.5-47.8% of the ring, so there is no sensitivity worth defending here.
const guardPx = Number(opt('guard-px', 2));
// WHICH FRAME IS THE RING MEASURED AROUND (#685). Two different questions
// wear the same shape, and conflating them is what made the old numbers
// unreadable:
//
//   --frame pipeline  (default)  the frame rasterize_svg.mjs will ACTUALLY
//                                render into. "Is the pool losing artwork?"
//                                This is the gate. A stale artboard passes,
//                                because the rasteriser reframes it.
//   --frame declared             the canvas the SOURCE claims, whatever the
//                                pipeline then does about it. "Which sources
//                                are lying about their extent?" This is the
//                                audit, and it is what found the six
//                                Flash-exported sprite sheets.
//
// Run the gate in CI; run the audit when you want to know what the
// rasteriser is having to work around.
const frameMode = opt('frame', 'pipeline');
if (!['pipeline', 'declared'].includes(frameMode)) {
  console.error(`probe_render_loss.mjs: --frame must be 'pipeline' or 'declared', got '${frameMode}'`);
  process.exit(2);
}

let sources = [];
const list = opt('sources', null);
const manifestPath = opt('pool-manifest', null);
const reportPath = opt('from-report', null);
if (list) {
  sources = list.split(',').filter(Boolean);
} else if (manifestPath) {
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
  if (reportPath) {
    // Triage mode: only what some other detector flagged.
    const report = JSON.parse(readFileSync(reportPath, 'utf8'));
    const byName = new Map(manifest.entries.map((e) => [e.name, e.source]));
    sources = report.files.filter((f) => f.flagged)
      .map((f) => byName.get(f.file)).filter(Boolean);
  } else {
    // Gate mode (#685): the whole pool. Sweeping everything is what
    // found the six lossy files — every earlier sweep started from
    // another detector's flag list and inherited its blind spots.
    // A pool manifest also lists bitmaps, which are copied rather than
    // rendered and have no frame to lose anything from; counting those
    // 86 as `skipped` made the gate's own summary line read as if it had
    // given up on part of the pool.
    sources = [...new Set(manifest.entries
      .filter((e) => (e.kind ? e.kind === 'vector' : /\.svg$/i.test(e.source)))
      .map((e) => e.source))];
  }
} else {
  console.error('probe_render_loss.mjs: need --sources or --pool-manifest');
  process.exit(2);
}
if (limit > 0) sources = sources.slice(0, limit);

/** The frame the pipeline will render `svgText` into, expanded by `pad`
 *  on every side. Everything the real render produced sits in the middle
 *  of this; the added ring is territory the real render could not show.
 *  Returns null when the source declares a canvas this cannot resolve
 *  (a unit like `2cm`) — probing a ring in the wrong coordinate space is
 *  worse than admitting the file was not checked. */
async function paddedFrame(svgText) {
  const { rect, reason, declared } = await pipelineFrame(svgText);
  // reason 'declared' means the pipeline hands the file to sharp
  // untouched, so the frame IS the declared canvas.
  const frame = frameMode === 'declared' ? declared : (rect ?? declared);
  if (!frame) return null;
  const padded = padRect(frame, pad);
  // Density derived from the frame, exactly as the rasteriser does it, so
  // a 2248-unit sprite sheet does not ask libvips for a 12,000-pixel edge
  // on the way to a 512-pixel measurement.
  const density = Math.min(384, (72 * 2048) / Math.max(padded.w, padded.h));
  return { reason, density, svg: withViewBox(svgText, padded) };
}

const out = { checked: 0, lost: 0, skipped: 0, frameMode, pad, minRingPct, guardPx, files: [] };

for (const src of sources) {
  const text = readFileSync(join(pack, src), 'utf8');
  let framed;
  try {
    framed = await paddedFrame(text);
  } catch (err) {
    // measuredExtent throws rather than hand back a frame that might
    // still be cutting something — that is a finding, not a skip.
    out.checked++;
    out.lost++;
    out.files.push({ source: src, lost: true, error: err.message });
    continue;
  }
  if (!framed) {
    out.skipped++;
    out.files.push({ source: src, skipped: 'unresolvable-canvas' });
    continue;
  }
  let data, info;
  try {
    ({ data, info } = await sharp(Buffer.from(framed.svg), { density: framed.density })
      .resize({ width: px, height: px, fit: 'inside', withoutEnlargement: false })
      .raw().toBuffer({ resolveWithObject: true }));
  } catch (err) {
    out.files.push({ source: src, error: err.message });
    continue;
  }
  const { width, height, channels } = info;
  // The ring is everything outside the region the ORIGINAL viewBox
  // occupies inside the padded one: pad/(1+2*pad) of each dimension.
  // The excluded region is deliberately rounded OUTWARD (floor/ceil) and
  // then widened by `guardPx`, so it is a strict superset of the original
  // content's footprint — see GUARD BAND above. Rounding to nearest, as
  // this did, makes it a near-miss subset and the perimeter of every
  // full-bleed render reads as loss.
  const fx = pad / (1 + 2 * pad);
  const x0 = Math.max(0, Math.floor(width * fx) - guardPx);
  const x1 = Math.min(width, Math.ceil(width * (1 - fx)) + guardPx);
  const y0 = Math.max(0, Math.floor(height * fx) - guardPx);
  const y1 = Math.min(height, Math.ceil(height * (1 - fx)) + guardPx);
  let ring = 0, ringTotal = 0;
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      const inside = x >= x0 && x < x1 && y >= y0 && y < y1;
      if (inside) continue;
      ringTotal++;
      if (channels < 4 || data[(y * width + x) * channels + 3] > 40) ring++;
    }
  }
  const ratio = ringTotal ? (100 * ring) / ringTotal : 0;
  const lost = ratio >= minRingPct;
  out.checked++;
  if (lost) out.lost++;
  out.files.push({ source: src, frame: framed.reason, ringSolidPixels: ring,
                   ringPixels: ringTotal, ringPct: Math.round(ratio * 100) / 100,
                   lost });
}

console.error(`probed ${out.checked} sources at +${Math.round(pad * 100)}% of their ${frameMode} frame: ` +
              `${out.lost} lost >= ${minRingPct}% of the ring, ` +
              `${out.skipped} skipped (unresolvable canvas)`);
process.stdout.write(JSON.stringify(out, null, 1) + '\n');
process.exit(out.lost ? 1 : 0);
