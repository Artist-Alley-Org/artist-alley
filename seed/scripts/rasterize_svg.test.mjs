// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for rasterize_svg.mjs (#630) and detect_cropped_renders.mjs.
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
