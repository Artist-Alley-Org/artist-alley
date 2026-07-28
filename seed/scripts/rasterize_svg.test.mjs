// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for rasterize_svg.mjs (#630, #672), detect_cropped_renders.mjs
// and detect_oversized_canvas.mjs.
//
// Run:  cd seed/scripts && node --test
//
// THE BUG UNDER TEST: an SVG with no width/height/viewBox on the root —
// ~765 of the Kenney pack's 5,279 vectors — makes librsvg GUESS a canvas,
// and the guess under-measures curve extents (quadratic control points
// beyond endpoints), so content is clipped at render time. Density can't
// fix it: the guessed canvas scales with density, so clipped stays
// clipped at every resolution. Live case: pipes_green.svg, true extent
// 1440x600 units, guessed 1402x523 → a 512x191 PNG with 86 solid pixels
// running off the bottom edge and 76 off the right.
//
// Fixtures are GENERATED HERE, in-repo, per the #614/#618 fixture rule —
// no NAS paths, nothing a runner can't reach. The dimensionless fixture
// reproduces the pipes class deterministically: a quadratic curve whose
// control extent exceeds librsvg's guess (verified: guess 1340x500,
// true 1440x600, old path clips 412 bottom / 91 right edge pixels).
//
// THE SECOND BUG UNDER TEST (#672): the fix above measures the drawing's
// extent on a 1024px probe spread over an 8192-unit search canvas, so its
// 2-pixel safety margin is a fixed 16 USER UNITS however big the drawing
// is. The pack's splat vectors span ~100 units, so 16 units became a ~15%
// margin per side and the artwork filled 50% of its own frame — a graphic
// floating in a box on the browse grid. A refine probe re-measures at the
// drawing's own scale.
//
// These tests drive the scripts THROUGH THEIR PUBLIC CONTRACT — spawned
// as child processes, jobs on stdin — so the pipeline shape (stdin JSON,
// exit codes, per-file tolerance) is pinned too, not just the pixels.
//
// sharp: skip-if-missing so a CI without seed/scripts/node_modules
// doesn't hard-fail; the PR records a local run where nothing skipped.
// A skipped test proving nothing is the #614 class — do not trust a run
// where these show as skipped.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));

let sharp = null;
try {
  sharp = (await import('sharp')).default;
} catch {
  /* skip below */
}

const skipOpts = sharp ? {} : { skip: 'sharp not installed (cd seed/scripts && npm install sharp)' };

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

/** The pipes class: dimensionless root, quadratic control points past the
 *  endpoint extents so librsvg's canvas guess falls short of the true
 *  1440x600-unit content. */
const DIMENSIONLESS_CLIPPING_SVG =
  `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">\n` +
  `<g><path fill="#527914" stroke="none" d="M100 100 L1000 100 1000 400 100 400 Z"/>` +
  `<path fill="#77b255" stroke="none" d="M1200 400 Q1440 400 1440 600 L1200 600 Z"/></g>\n</svg>`;

/** A dimensioned SVG in the pack's own shape (width/height, no viewBox) —
 *  the 85% majority whose output must not change at all. */
const DIMENSIONED_SVG =
  `<svg width="64" height="64" xmlns="http://www.w3.org/2000/svg">\n` +
  `<circle cx="32" cy="32" r="20" fill="#527914"/>\n</svg>`;

/** #672 — the SMALL-EXTENT dimensionless class. Same dimensionless root
 *  as the pipes fixture, but the drawing spans ~64 user units instead of
 *  ~1400. The search probe spreads 1024px over 8192 units, so its 2-pixel
 *  safety margin is a fixed 16 units: on a 64-unit drawing that is 25% of
 *  the frame per side, and the artwork ends up filling ~11% of its own
 *  canvas. Modelled on the Kenney splat pack, whose vectors span ~100
 *  units and measured 50% content before the refine pass. */
const DIMENSIONLESS_SMALL_SVG =
  `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">\n` +
  `<g><rect x="-32" y="-32" width="64" height="64" fill="#527914"/></g>\n</svg>`;

function renderJobs(jobs) {
  // Spawn the real script exactly as kenney_hq.py does: JSON on stdin.
  return execFileSync('node', [join(HERE, 'rasterize_svg.mjs')], {
    input: JSON.stringify(jobs),
    cwd: HERE,
  });
}

async function edgeScan(pngPath) {
  const { data, info } = await sharp(pngPath).raw().toBuffer({ resolveWithObject: true });
  const alphaAt = (x, y) => data[(y * info.width + x) * info.channels + 3];
  const count = (coords) => coords.reduce((n, [x, y]) => n + (alphaAt(x, y) > 40 ? 1 : 0), 0);
  let nonTransparent = 0;
  for (let i = 3; i < data.length; i += info.channels) if (data[i] > 40) nonTransparent++;
  return {
    width: info.width,
    height: info.height,
    nonTransparent,
    edges: {
      top: count(Array.from({ length: info.width }, (_, x) => [x, 0])),
      bottom: count(Array.from({ length: info.width }, (_, x) => [x, info.height - 1])),
      left: count(Array.from({ length: info.height }, (_, y) => [0, y])),
      right: count(Array.from({ length: info.height }, (_, y) => [info.width - 1, y])),
    },
  };
}

// ---------------------------------------------------------------------------
// rasterize_svg.mjs
// ---------------------------------------------------------------------------

test('dimensionless SVG renders without clipping any edge (#630)', skipOpts, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'rast-'));
  try {
    const src = join(dir, 'clipper.svg');
    const dst = join(dir, 'clipper.png');
    writeFileSync(src, DIMENSIONLESS_CLIPPING_SVG);
    renderJobs([{ src, dst, px: 512 }]);

    const scan = await edgeScan(dst);
    // The whole point: content that librsvg's guessed canvas cut off must
    // be inside the frame, and this artwork has natural margins on every
    // side, so a single solid edge pixel means we are still cropping.
    for (const [edge, solid] of Object.entries(scan.edges)) {
      assert.equal(solid, 0,
        `${edge} edge has ${solid} solid pixels — content is clipped, ` +
        `the librsvg canvas guess is still in charge (#630)`);
    }
    // Long edge contract unchanged.
    assert.equal(Math.max(scan.width, scan.height), 512);
    // Frame sanity: content spans 100..1440 x 100..600 units, so with
    // the ~2-probe-pixel safety margin the aspect lands near
    // 1372/532 = 2.58. A wide band on purpose — the edge scans above
    // are the discriminator (the old path put 412 solid pixels on the
    // bottom edge of this fixture); this only guards against a wildly
    // wrong frame.
    const aspect = scan.width / scan.height;
    assert.ok(aspect > 2.3 && aspect < 2.8,
      `aspect ${aspect.toFixed(2)} — frame does not match the ~2.58 true extent`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('dimensioned SVG output is byte-identical to the historical path (#630)', skipOpts, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'rast-'));
  try {
    const src = join(dir, 'dimensioned.svg');
    writeFileSync(src, DIMENSIONED_SVG);

    // What the pipeline produced BEFORE this change, computed inline with
    // the same sharp calls the script has always made for dimensioned
    // files. If the script's dimensioned branch drifts, these diverge.
    const expected = await sharp(src, { density: 384 })
      .resize({ width: 512, height: 512, fit: 'inside', withoutEnlargement: false })
      .png({ compressionLevel: 9 })
      .toBuffer();

    const dst = join(dir, 'dimensioned.png');
    renderJobs([{ src, dst, px: 512 }]);
    assert.ok(readFileSync(dst).equals(expected),
      'dimensioned SVG bytes changed — the fix must not perturb the ~85% that were fine');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('negative-coordinate content is not clipped either (#630)', skipOpts, async () => {
  // The observed pack clipping is positive-quadrant, but the probe is
  // negative-inclusive by construction — pin that rather than assume it.
  const dir = mkdtempSync(join(tmpdir(), 'rast-'));
  try {
    const src = join(dir, 'negative.svg');
    const dst = join(dir, 'negative.png');
    writeFileSync(src,
      `<svg xmlns="http://www.w3.org/2000/svg">` +
      `<rect x="-400" y="-300" width="800" height="600" fill="#527914"/></svg>`);
    renderJobs([{ src, dst, px: 512 }]);
    // An 800x600 rect centred on the origin. The old path keeps librsvg's
    // 0,0-anchored canvas, which drops the negative half: measured, it
    // fills only 61% of its frame, asymmetrically. Framed correctly the
    // rect fills ~91% (the remainder is the by-design safety margin, so
    // edges are NOT asserted full here) and is symmetric — solid pixels
    // in all four image quadrants in near-equal number.
    const { data, info } = await sharp(dst).raw().toBuffer({ resolveWithObject: true });
    const solidIn = (x0, y0, x1, y1) => {
      let n = 0;
      for (let y = y0; y < y1; y++)
        for (let x = x0; x < x1; x++)
          if (data[(y * info.width + x) * info.channels + 3] > 40) n++;
      return n;
    };
    const hw = Math.floor(info.width / 2), hh = Math.floor(info.height / 2);
    const q = [
      solidIn(0, 0, hw, hh), solidIn(hw, 0, info.width, hh),
      solidIn(0, hh, hw, info.height), solidIn(hw, hh, info.width, info.height),
    ];
    const total = q.reduce((a, b) => a + b, 0);
    assert.ok(total / (info.width * info.height) > 0.85,
      `only ${(100 * total / (info.width * info.height)).toFixed(0)}% solid — the old path measured 61% here`);
    const [mn, mx] = [Math.min(...q), Math.max(...q)];
    assert.ok(mn / mx > 0.9,
      `quadrant asymmetry ${q.join(',')} — negative-coordinate content was dropped`);
    const aspect = info.width / info.height;
    assert.ok(aspect > 1.26 && aspect < 1.38,
      `aspect ${aspect.toFixed(2)} — expected ~1.32 (4:3 plus the safety margin)`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

/** Alpha bounding box of a PNG, as a fraction of canvas area — the #672
 *  measurement, computed here rather than shelled out to the detector so
 *  this test fails for a rasteriser reason and not a detector reason. */
async function contentRatio(pngPath) {
  const { data, info } = await sharp(pngPath).raw().toBuffer({ resolveWithObject: true });
  let minX = info.width, minY = info.height, maxX = -1, maxY = -1;
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
  if (maxX < 0) return { ratio: 0, width: info.width, height: info.height };
  return {
    ratio: ((maxX - minX + 1) * (maxY - minY + 1)) / (info.width * info.height),
    width: info.width,
    height: info.height,
  };
}

test('a small-extent dimensionless SVG is framed tightly, not marooned (#672)', skipOpts, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'rast-'));
  try {
    const src = join(dir, 'small.svg');
    const dst = join(dir, 'small.png');
    writeFileSync(src, DIMENSIONLESS_SMALL_SVG);
    renderJobs([{ src, dst, px: 512 }]);

    const { ratio, width, height } = await contentRatio(dst);
    // A square drawing correctly framed is a square render. Before the
    // refine pass this measured 512x512 too — the frame was square but
    // four times too big — so the dimensions alone prove nothing and the
    // ratio below is the discriminator.
    assert.equal(width, 512);
    assert.equal(height, 512);
    // Search-probe-only framing puts 16 user units of margin on each side
    // of a 64-unit drawing: 64/96 linear, ~0.44 by area... and that is the
    // BEST case; measured on this fixture it lands near 0.11. The pack's
    // own splat vectors measured 0.50. Anything below ~0.9 means the
    // margin is scaled to the search canvas rather than to the drawing.
    assert.ok(ratio > 0.9,
      `content fills only ${(100 * ratio).toFixed(1)}% of its frame — the safety ` +
      `margin is still 2 pixels of the 8192-unit SEARCH canvas rather than 2 ` +
      `pixels of the drawing (#672)`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the refine pass does not reintroduce clipping on the #630 fixture', skipOpts, async () => {
  // Guard against the obvious way to over-fit #672: tighten the frame
  // until it cuts. Same fixture as the first test, asserted from the
  // other direction — tight AND uncut.
  const dir = mkdtempSync(join(tmpdir(), 'rast-'));
  try {
    const src = join(dir, 'clipper.svg');
    const dst = join(dir, 'clipper.png');
    writeFileSync(src, DIMENSIONLESS_CLIPPING_SVG);
    renderJobs([{ src, dst, px: 512 }]);
    const scan = await edgeScan(dst);
    for (const [edge, solid] of Object.entries(scan.edges)) {
      assert.equal(solid, 0, `${edge} edge has ${solid} solid pixels — tightening clipped it`);
    }
    const { ratio } = await contentRatio(dst);
    assert.ok(ratio > 0.9,
      `content fills only ${(100 * ratio).toFixed(1)}% of its frame (#672)`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('per-file failure tolerance survives a broken SVG in the batch', skipOpts, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'rast-'));
  try {
    const good = join(dir, 'good.svg');
    const bad = join(dir, 'bad.svg');
    writeFileSync(good, DIMENSIONED_SVG);
    writeFileSync(bad, '<svg not even close');
    let code = 0;
    try {
      renderJobs([
        { src: bad, dst: join(dir, 'bad.png'), px: 512 },
        { src: good, dst: join(dir, 'good.png'), px: 512 },
      ]);
    } catch (e) {
      code = e.status;
    }
    assert.equal(code, 1, 'batch with one failure must exit 1, not throw/2/3');
    const scan = await edgeScan(join(dir, 'good.png'));
    assert.ok(scan.nonTransparent > 0, 'the good file must still have rendered');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------
// detect_cropped_renders.mjs
// ---------------------------------------------------------------------------

async function makeDetectorFixtures(dir) {
  // cropped: content running off bottom+right, margins top+left — the
  // partial-edge signature (mirrors the live pipes_green render).
  await sharp({ create: { width: 100, height: 100, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
    .composite([{
      input: await sharp({ create: { width: 60, height: 60, channels: 4, background: { r: 80, g: 120, b: 20, alpha: 1 } } }).png().toBuffer(),
      left: 40, top: 40,
    }])
    .png().toFile(join(dir, 'cropped.png'));
  // full-bleed: every pixel solid — legitimate edge coverage, not a crop.
  await sharp({ create: { width: 100, height: 100, channels: 4, background: { r: 80, g: 120, b: 20, alpha: 1 } } })
    .png().toFile(join(dir, 'fullbleed.png'));
  // clean margin: solid centre, transparent border on all sides.
  await sharp({ create: { width: 100, height: 100, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
    .composite([{
      input: await sharp({ create: { width: 60, height: 60, channels: 4, background: { r: 80, g: 120, b: 20, alpha: 1 } } }).png().toBuffer(),
      left: 20, top: 20,
    }])
    .png().toFile(join(dir, 'clean.png'));
}

test('detector flags cropped, passes full-bleed and clean-margin', skipOpts, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'detect-'));
  try {
    await makeDetectorFixtures(dir);
    const out = execFileSync('node', [join(HERE, 'detect_cropped_renders.mjs'), dir], { cwd: HERE });
    const report = JSON.parse(out.toString());
    const byFile = Object.fromEntries(report.files.map((f) => [f.file, f]));

    assert.equal(byFile['cropped.png'].flagged, true,
      'partial-edge coverage (60%) must be flagged as likely cropped');
    assert.equal(byFile['fullbleed.png'].flagged, false,
      '~100% edge coverage is legitimate full bleed, not a crop');
    assert.equal(byFile['clean.png'].flagged, false,
      '0% edge coverage is a clean margin, not a crop');
    assert.equal(report.flagged, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------
// detect_oversized_canvas.mjs (#672)
// ---------------------------------------------------------------------------

/** Solid rect of `size` placed at (left, top) on a `canvas` square. */
async function tile(dir, name, canvas, size, left, top) {
  await sharp({ create: { width: canvas, height: canvas, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
    .composite([{
      input: await sharp({ create: { width: size, height: size, channels: 4, background: { r: 80, g: 120, b: 20, alpha: 1 } } }).png().toBuffer(),
      left, top,
    }])
    .png().toFile(join(dir, name));
}

test('oversized-canvas detector flags a marooned graphic, not a tight one (#672)', skipOpts, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'oversize-'));
  try {
    // The live #672 shape: content in the top-left corner of a canvas it
    // occupies ~1% of (measured 1.3% on splat02's stored render).
    await tile(dir, 'marooned-corner.png', 200, 20, 0, 0);
    // The same defect with the content NOT against a corner. This is the
    // variant the #630 edge detector genuinely cannot see (next test).
    await tile(dir, 'marooned-centre.png', 200, 20, 90, 90);
    // A correctly framed render: content fills essentially the whole frame.
    await tile(dir, 'tight.png', 200, 198, 1, 1);
    // A source-declared padded canvas — a centred sprite at 25% by area.
    // Legitimate composition; the real pool has these down to 15.6%, and
    // flagging them is the false-positive class the default guards against.
    await tile(dir, 'padded.png', 200, 100, 50, 50);

    const out = execFileSync('node', [join(HERE, 'detect_oversized_canvas.mjs'), dir], { cwd: HERE });
    const report = JSON.parse(out.toString());
    const byFile = Object.fromEntries(report.files.map((f) => [f.file, f]));

    assert.equal(byFile['marooned-corner.png'].flagged, true,
      '1% content must be flagged — this is the #672 defect');
    assert.equal(byFile['marooned-centre.png'].flagged, true,
      'the defect does not stop being one when the content is centred');
    assert.equal(byFile['tight.png'].flagged, false,
      'a full frame is what a correct render looks like');
    assert.equal(byFile['padded.png'].flagged, false,
      '25% centred content is deliberate source padding, not a defect — ' +
      'flagging it is how a repair ends up rewriting healthy files');
    assert.equal(report.flagged, 2);

    // The corner-anchor signal, reported for triage only.
    assert.equal(byFile['marooned-corner.png'].anchoredCorner, true);
    assert.equal(byFile['marooned-centre.png'].anchoredCorner, false);
    assert.equal(byFile['padded.png'].anchoredCorner, false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the crop detector cannot see a marooned graphic that touches no edge', skipOpts, async () => {
  // Why a second script exists. NOTE the precise claim: #672's write-up
  // says this class "scores 0% edge coverage" and that the #630 sweep
  // therefore passed over every instance. Half right. When the content is
  // marooned but still ANCHORED to a corner — which is what the live
  // splat02 render is — it puts partial coverage on two edges and the
  // #630 detector does flag it (measured: top 8%, left 6.6%). It is only
  // blind when the content touches no edge at all. Both cases are pinned
  // here so the boundary is recorded rather than assumed.
  const dir = mkdtempSync(join(tmpdir(), 'oversize-'));
  try {
    await tile(dir, 'marooned-centre.png', 200, 20, 90, 90);
    const crop = JSON.parse(
      execFileSync('node', [join(HERE, 'detect_cropped_renders.mjs'), dir], { cwd: HERE }).toString());
    assert.equal(crop.flagged, 0,
      'edge coverage is 0% here — this is the genuine blind spot #672 names');

    const over = JSON.parse(
      execFileSync('node', [join(HERE, 'detect_oversized_canvas.mjs'), dir], { cwd: HERE }).toString());
    assert.equal(over.flagged, 1, 'the ratio detector must see what the edge detector cannot');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a corner-anchored marooned graphic is visible to BOTH detectors', skipOpts, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'oversize-'));
  try {
    await tile(dir, 'marooned-corner.png', 200, 20, 0, 0);
    const crop = JSON.parse(
      execFileSync('node', [join(HERE, 'detect_cropped_renders.mjs'), dir], { cwd: HERE }).toString());
    assert.equal(crop.flagged, 1,
      'corner-anchored content puts partial coverage on two edges — the ' +
      '#630 detector is not blind to THIS variant, whatever #672 assumed');
    const over = JSON.parse(
      execFileSync('node', [join(HERE, 'detect_oversized_canvas.mjs'), dir], { cwd: HERE }).toString());
    assert.equal(over.flagged, 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('--min-ratio tightens the bar for pipeline-framed renders (#672)', skipOpts, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'oversize-'));
  try {
    // 50% by area — exactly what the splat pack measured with only the
    // search probe. Healthy under the pool-wide default, flagged under the
    // strict bar used when sweeping renders whose canvas we chose.
    await tile(dir, 'half.png', 200, 141, 30, 30);
    const lax = JSON.parse(
      execFileSync('node', [join(HERE, 'detect_oversized_canvas.mjs'), dir], { cwd: HERE }).toString());
    assert.equal(lax.flagged, 0);
    const strict = JSON.parse(
      execFileSync('node', [join(HERE, 'detect_oversized_canvas.mjs'), dir, '--min-ratio', '90'], { cwd: HERE }).toString());
    assert.equal(strict.flagged, 1);
    assert.ok(strict.files[0].ratio > 45 && strict.files[0].ratio < 55,
      `ratio ${strict.files[0].ratio} — expected ~50%`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
