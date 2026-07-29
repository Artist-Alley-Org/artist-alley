// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for rasterize_svg.mjs (#630, #672, #685), probe_render_loss.mjs
// (#572, #685) and detect_oversized_canvas.mjs (#672).
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
// THE THIRD BUG UNDER TEST (#685): #630 checked librsvg's canvas GUESS
// and trusted every canvas a source declared for itself. Six pool sources
// — Flash/Animate sprite-sheet exports — declare `viewBox="0 0 550 400"`
// over a drawing spanning up to 2248x1120 units, and librsvg dutifully
// clipped the rest: the shipped Platformer Pack Remastered background
// sheet held 8.8% of its own artwork. Every dimensioned source now gets
// an overflow probe before it is trusted.
//
// AND THE PROBE'S OWN BUG (#685): probe_render_loss.mjs rounded the inner
// boundary of its measurement ring to the nearest pixel, which put a
// one-pixel sliver of the ORIGINAL render inside the ring. Harmless on
// art with a margin, fatal on full-bleed art, where that sliver is the
// whole perimeter — 410 healthy pool files read as lossy at 1-5%. It also
// only probed sources with an explicit viewBox, which is 30 of the 1031;
// the other 776 dimensioned ones declare width/height instead and were
// silently skipped, which is where the six real defects were hiding.
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

/** A source whose content EXACTLY fills the canvas it declares — the
 *  full-bleed class (flags, patterns, brick tiles: 141+66+59 of the pool).
 *  Nothing is lost rendering this, and saying otherwise is the #685
 *  false positive. `decl` picks how the canvas is declared. */
function fullBleedSvg(decl) {
  const root = decl === 'viewbox'
    ? `<svg viewBox="0 0 64 64" xmlns="http://www.w3.org/2000/svg">`
    : `<svg width="64" height="64" xmlns="http://www.w3.org/2000/svg">`;
  return `${root}<rect x="0" y="0" width="64" height="64" fill="#527914"/></svg>`;
}

/** The #685 defect, in the shape of the Flash-exported sprite sheets: a
 *  stale artboard declaring a fraction of the drawing it contains. The
 *  declared 64x64 holds one tile of a 3x3 grid spanning 192x192, so a
 *  render that honours the declaration keeps 1/9 of the artwork. */
function staleArtboardSvg(decl) {
  const root = decl === 'viewbox'
    ? `<svg viewBox="0 0 64 64" xmlns="http://www.w3.org/2000/svg">`
    : `<svg width="64" height="64" xmlns="http://www.w3.org/2000/svg">`;
  let tiles = '';
  for (let r = 0; r < 3; r++) {
    for (let c = 0; c < 3; c++) {
      tiles += `<rect x="${c * 64 + 4}" y="${r * 64 + 4}" width="56" height="56" fill="#527914"/>`;
    }
  }
  return `${root}${tiles}</svg>`;
}

/** The anti-aliasing tail (#685): a shape that ENDS on the declared edge
 *  and whose stroke leaves a hairline just outside it. 0.5 units past a
 *  64-unit canvas is 0.78%, right in the 0.46-0.68% band the real pool
 *  showed. Overflow, technically. Not a defect, and reframing it would
 *  rewrite 12 correct files to shift an invisible hairline. */
const FRINGE_OVERFLOW_SVG =
  `<svg width="64" height="64" xmlns="http://www.w3.org/2000/svg">` +
  `<rect x="0" y="0" width="64" height="64" fill="#527914" ` +
  `stroke="#77b255" stroke-width="1"/></svg>`;

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

test('a declared canvas smaller than its drawing is reframed, not honoured (#685)', skipOpts, async () => {
  // The Flash/Animate sprite-sheet class: `viewBox="0 0 64 64"` over a
  // 3x3 grid of tiles spanning 192x192 units. librsvg honours the
  // declaration and renders the top-left tile only — the shipped
  // Platformer Pack Remastered sheet kept 8.8% of its artwork this way,
  // and #630's fix did not look because the source "declared" a canvas.
  for (const decl of ['viewbox', 'wh']) {
    const dir = mkdtempSync(join(tmpdir(), 'rast-'));
    try {
      const src = join(dir, 'stale.svg');
      const dst = join(dir, 'stale.png');
      writeFileSync(src, staleArtboardSvg(decl));
      renderJobs([{ src, dst, px: 512 }]);

      const scan = await edgeScan(dst);
      // 3x3 grid of tiles with a gutter: correctly framed it is square.
      // Honouring the stale 64x64 artboard also gives a square, so the
      // shape proves nothing — the tile COUNT below is the discriminator.
      const { data, info } = await sharp(dst).raw().toBuffer({ resolveWithObject: true });
      // Count solid runs along the horizontal and vertical centre lines.
      // The full sheet crosses three tiles on each; the clipped render
      // crosses one.
      const runs = (coords) => {
        let n = 0, prev = false;
        for (const [x, y] of coords) {
          const on = data[(y * info.width + x) * info.channels + 3] > 40;
          if (on && !prev) n++;
          prev = on;
        }
        return n;
      };
      const across = runs(Array.from({ length: info.width }, (_, x) => [x, Math.floor(info.height / 2)]));
      const down = runs(Array.from({ length: info.height }, (_, y) => [Math.floor(info.width / 2), y]));
      assert.equal(across, 3,
        `${decl}: ${across} tiles across the middle, expected 3 — the render is ` +
        `still framed to the stale artboard and is dropping artwork (#685)`);
      assert.equal(down, 3,
        `${decl}: ${down} tiles down the middle, expected 3 (#685)`);
      // Reframed tightly, not merely widened: the outer tiles' 4-unit
      // gutter is the only margin, so nothing sits on an edge.
      for (const [edge, solid] of Object.entries(scan.edges)) {
        assert.equal(solid, 0, `${decl}: ${edge} edge has ${solid} solid pixels — still cutting`);
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }
});

test('a declared canvas that FITS its drawing is left alone (#685)', skipOpts, async () => {
  // The overflow probe must not become a licence to reframe everything.
  // 800 of the pool's 806 dimensioned sources have zero content outside
  // their declared canvas, and their bytes must not move — the
  // byte-identity test above covers the common case; this covers the
  // adversarial one, art that ENDS exactly on the declared edge and so
  // leaves an anti-aliased fringe just outside it.
  const dir = mkdtempSync(join(tmpdir(), 'rast-'));
  try {
    const src = join(dir, 'bleed.svg');
    writeFileSync(src, fullBleedSvg('viewbox'));
    const expected = await sharp(src, { density: 384 })
      .resize({ width: 512, height: 512, fit: 'inside', withoutEnlargement: false })
      .png({ compressionLevel: 9 })
      .toBuffer();
    const dst = join(dir, 'bleed.png');
    renderJobs([{ src, dst, px: 512 }]);
    assert.ok(readFileSync(dst).equals(expected),
      'full-bleed art reframed — the overflow probe is firing on a fringe, ' +
      'which would rewrite the flag/pattern/brick packs for no reason (#685)');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a hairline past the declared edge is not an overflow (#685)', skipOpts, async () => {
  // The overflow check must fire on a stale artboard and NOT on the
  // anti-aliasing tail. Sized at the probe's own resolution — three probe
  // pixels, which is what this first shipped as — it sits below the tail
  // and reframes 18 pool sources instead of the 6 that are actually
  // broken. A threshold at the noise floor is not a threshold.
  const dir = mkdtempSync(join(tmpdir(), 'rast-'));
  try {
    const src = join(dir, 'fringe.svg');
    writeFileSync(src, FRINGE_OVERFLOW_SVG);
    const expected = await sharp(src, { density: 384 })
      .resize({ width: 512, height: 512, fit: 'inside', withoutEnlargement: false })
      .png({ compressionLevel: 9 })
      .toBuffer();
    const dst = join(dir, 'fringe.png');
    renderJobs([{ src, dst, px: 512 }]);
    assert.ok(readFileSync(dst).equals(expected),
      'a 0.78%-of-long-edge stroke fringe reframed the file — the overflow ' +
      'tolerance is back at probe resolution and 12 correct pool files will ' +
      'be rewritten with it (#685)');
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
// probe_render_loss.mjs (#572, #685) — THE CROP GATE
//
// detect_cropped_renders.mjs used to live here. It was retired in #685:
// swept against this probe's verdict over 9,504 combinations of band
// edges, alpha cutoff, minimum agreeing edges and an absolute-pixel
// floor, not one found all six genuinely lossy pool files, and the best
// precision anywhere in the sweep was 0.043. Its tests are gone with it
// rather than being kept green against a script nobody may act on.
// ---------------------------------------------------------------------------

function probe(packDir, sources, extra = []) {
  const run = (args) => execFileSync('node', [join(HERE, 'probe_render_loss.mjs'), ...args],
    { cwd: HERE, stdio: ['ignore', 'pipe', 'pipe'] });
  const args = ['--pack', packDir, '--sources', sources.join(','), ...extra];
  try {
    return JSON.parse(run(args).toString());
  } catch (err) {
    // Exit 1 means "something lost content" — a verdict, not a crash.
    if (err.status === 1 && err.stdout) return JSON.parse(err.stdout.toString());
    throw err;
  }
}

test('full-bleed art loses nothing, however it declares its canvas (#685)', skipOpts, () => {
  const dir = mkdtempSync(join(tmpdir(), 'probe-'));
  try {
    writeFileSync(join(dir, 'bleed_vb.svg'), fullBleedSvg('viewbox'));
    writeFileSync(join(dir, 'bleed_wh.svg'), fullBleedSvg('wh'));
    const report = probe(dir, ['bleed_vb.svg', 'bleed_wh.svg']);

    // BOTH must be probed. Before #685 the width/height file reported
    // `skipped: no-viewbox` — 776 of the pool's 1031 sources took that
    // exit, which is how six lossy files went unexamined.
    assert.equal(report.skipped, 0,
      'a width/height canvas is a canvas — skipping it leaves 75% of the pool unchecked');
    assert.equal(report.checked, 2);

    // And neither may be called lossy. Before #685 the ring boundary was
    // rounded to the nearest pixel, so the outermost row of the ORIGINAL
    // render fell inside the ring: on full-bleed art that is the entire
    // perimeter, measured at 1.79% against a 1% bar.
    for (const f of report.files) {
      assert.equal(f.lost, false,
        `${f.source} reported ${f.ringPct}% of the ring — art that fills its ` +
        `declared canvas exactly loses nothing; this is the boundary-rounding ` +
        `artefact that flagged 410 healthy pool files (#685)`);
      assert.ok(f.ringPct < 0.5,
        `${f.source} ringPct ${f.ringPct} — expected ~0, not a near-miss under the bar`);
    }
    assert.equal(report.lost, 0);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a stale artboard is a SOURCE defect the pipeline absorbs (#685)', skipOpts, () => {
  const dir = mkdtempSync(join(tmpdir(), 'probe-'));
  try {
    writeFileSync(join(dir, 'stale_vb.svg'), staleArtboardSvg('viewbox'));
    writeFileSync(join(dir, 'stale_wh.svg'), staleArtboardSvg('wh'));
    const srcs = ['stale_vb.svg', 'stale_wh.svg'];

    // --frame declared: what does the SOURCE claim? It claims a 64x64
    // canvas over a 192x192 drawing, so measured against that claim two
    // thirds of the artwork is outside the frame. This is the audit that
    // found the six Flash-exported sheets, and it must keep working —
    // the guard band must not have blunted the probe into missing a real
    // cut while it was busy removing the full-bleed false positives.
    const audit = probe(dir, srcs, ['--frame', 'declared']);
    assert.equal(audit.checked, 2);
    assert.equal(audit.lost, 2, 'content 3x wider and taller than the declared canvas is outside it');
    for (const f of audit.files) {
      assert.ok(f.ringPct > 10, `${f.source} ringPct ${f.ringPct} — expected a large, obvious ring`);
    }

    // Default --frame pipeline: what does the RENDER lose? Nothing —
    // rasterize_svg.mjs reframes this class rather than honouring the
    // stale claim. The gate must say so, or every file #685 repaired
    // would report as broken for the rest of time, which is precisely
    // the failure mode this issue exists to end.
    const gate = probe(dir, srcs);
    assert.equal(gate.lost, 0, 'the pipeline reframes these; the gate must not re-flag them');
    for (const f of gate.files) {
      assert.equal(f.frame, 'overflow',
        `${f.source} framed as '${f.frame}' — the gate is not measuring the frame ` +
        `the rasteriser actually uses, so the two can silently disagree`);
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a frame that still cuts the drawing is reported lost (#685)', skipOpts, () => {
  // The gate's teeth. `--frame declared` forces the measurement onto a
  // canvas the pipeline would have rejected, which is the closest thing
  // to "the rasteriser regressed and went back to honouring a bad
  // declaration" that can be staged without breaking the rasteriser.
  const dir = mkdtempSync(join(tmpdir(), 'probe-'));
  try {
    writeFileSync(join(dir, 'stale.svg'), staleArtboardSvg('wh'));
    const report = probe(dir, ['stale.svg'], ['--frame', 'declared']);
    assert.equal(report.frameMode, 'declared');
    assert.equal(report.lost, 1);
    // The per-file `frame` still records what the PIPELINE decided, so a
    // report read months later says both what was measured and what the
    // rasteriser would have done about it.
    assert.equal(report.files[0].frame, 'overflow');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the probe exits 1 on loss and 0 on none — it is a gate (#685)', skipOpts, () => {
  const dir = mkdtempSync(join(tmpdir(), 'probe-'));
  try {
    writeFileSync(join(dir, 'clean.svg'), fullBleedSvg('wh'));
    writeFileSync(join(dir, 'stale.svg'), staleArtboardSvg('wh'));
    const run = (args) => execFileSync('node',
      [join(HERE, 'probe_render_loss.mjs'), '--pack', dir, ...args],
      { cwd: HERE, stdio: ['ignore', 'pipe', 'pipe'] });

    run(['--sources', 'clean.svg']); // exit 0, or execFileSync throws
    let code = 0;
    try { run(['--sources', 'stale.svg', '--frame', 'declared']); } catch (e) { code = e.status; }
    assert.equal(code, 1, 'a lossy source must exit non-zero or it cannot gate anything');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('--pool-manifest with no report sweeps the whole pool (#685)', skipOpts, () => {
  const dir = mkdtempSync(join(tmpdir(), 'probe-'));
  try {
    writeFileSync(join(dir, 'a.svg'), fullBleedSvg('wh'));
    writeFileSync(join(dir, 'b.svg'), staleArtboardSvg('wh'));
    // A dimensionless source too: this class was `skipped: no-viewbox`
    // before #685 — 225 of the pool's 1031 — because there was no
    // declared canvas to expand. There is a knowable render frame for it
    // all the same, so it is checked like everything else now.
    writeFileSync(join(dir, 'c.svg'), DIMENSIONLESS_SMALL_SVG);
    const manifest = join(dir, 'manifest.json');
    writeFileSync(manifest, JSON.stringify({ entries: [
      { source: 'a.svg', name: 'a.png' },
      { source: 'b.svg', name: 'b.png' },
      { source: 'c.svg', name: 'c.png' },
    ] }));
    const run = (extra) => {
      try {
        return JSON.parse(execFileSync('node', [join(HERE, 'probe_render_loss.mjs'),
          '--pack', dir, '--pool-manifest', manifest, ...extra],
          { cwd: HERE, stdio: ['ignore', 'pipe', 'pipe'] }).toString());
      } catch (e) { return JSON.parse(e.stdout.toString()); }
    };
    // Every entry, not just what another detector happened to flag. The
    // manifest-plus-report path could only ever inherit that detector's
    // blind spots.
    const gate = run([]);
    assert.equal(gate.checked, 3);
    assert.equal(gate.skipped, 0, 'a dimensionless source has a render frame like any other');
    assert.equal(gate.lost, 0, 'the pipeline frames all three correctly');

    const audit = run(['--frame', 'declared']);
    assert.equal(audit.lost, 1, 'exactly one of these lies about its extent');
    // The dimensionless one declares nothing to audit, so it is the one
    // legitimate skip.
    assert.equal(audit.skipped, 1);
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

test('an oversized canvas is not a loss — the two scripts do not overlap', skipOpts, async () => {
  // Why both scripts exist, stated as the property that survives #685's
  // retirement of the third. A marooned graphic loses NOTHING: every
  // pixel of the drawing is in the frame, there is just too much frame.
  // probe_render_loss.mjs is therefore structurally blind to it, and
  // always will be — it measures artwork outside the frame, and there
  // is none. That is what detect_oversized_canvas.mjs is for.
  const dir = mkdtempSync(join(tmpdir(), 'oversize-'));
  try {
    // The render side: a 20px graphic on a 200px canvas, both anchored
    // and centred, since #672's write-up got the corner case wrong once.
    await tile(dir, 'marooned-centre.png', 200, 20, 90, 90);
    await tile(dir, 'marooned-corner.png', 200, 20, 0, 0);
    const over = JSON.parse(
      execFileSync('node', [join(HERE, 'detect_oversized_canvas.mjs'), dir], { cwd: HERE }).toString());
    assert.equal(over.flagged, 2, 'both variants are the #672 defect');

    // The source side: the same shape as an SVG — a small drawing inside
    // a large declared canvas — loses nothing, and the loss probe must
    // say so rather than inventing a flag it cannot justify.
    writeFileSync(join(dir, 'marooned.svg'),
      `<svg width="200" height="200" xmlns="http://www.w3.org/2000/svg">` +
      `<rect x="90" y="90" width="20" height="20" fill="#527914"/></svg>`);
    const loss = probe(dir, ['marooned.svg']);
    assert.equal(loss.lost, 0,
      'nothing is outside this frame; a loss probe that flags it is measuring ' +
      'something other than loss');
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
