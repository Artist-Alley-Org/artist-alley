#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Does a pool render actually LOSE content? (#572)
//
// WHY THIS EXISTS. detect_cropped_renders.mjs (#630) flags any edge whose
// solid-pixel coverage lands between 5% and 95% — the signature of a
// shape cut mid-body. After #679 tightened the frame around the drawing,
// that band also catches every render whose artwork legitimately REACHES
// its border, which is what a tight frame is. Measured on this repo's
// pool the two are indistinguishable by flag count:
//
//     pre-existing pool (already shipped, #679-rendered)  234 / 570  41.1%
//     #572 additions                                      223 / 517  43.1%
//
// A number that fires on 41% of known-good output is not a gate, so
// "clean on the additions" needs a test that separates *touching* the
// frame from *running off* it. This is that test, and it asks the only
// question that matters: is there any artwork OUTSIDE the frame we
// rendered?
//
// METHOD. Re-render the source with its viewBox expanded by `--pad`
// (default 12%) on all four sides. Everything the normal render produced
// now sits in the middle; the added ring is territory the normal render
// could not show. Count solid pixels in that ring:
//
//     ring empty      nothing was lost — the render is tight, not cropped
//     ring populated  content exists beyond the frame — a real crop
//
// Dimensionless sources (no width/height/viewBox) are reported as
// `skipped: no-viewbox`: there is nothing to expand, and #630/#679 own
// that path with their own measure-then-render probe.
//
// Usage:
//   node probe_render_loss.mjs --pack <bundle-root> --sources a.svg,b.svg
//   node probe_render_loss.mjs --pack <bundle-root> --from-report <detect_cropped.json> \
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

const args = process.argv.slice(2);
const opt = (name, dflt) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && args[i + 1] !== undefined ? args[i + 1] : dflt;
};
const pack = opt('pack', null);
if (!pack) {
  console.error('usage: node probe_render_loss.mjs --pack <bundle-root> ' +
                '(--sources a.svg,b.svg | --from-report r.json --pool-manifest m.json)');
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

let sources = [];
const list = opt('sources', null);
if (list) {
  sources = list.split(',').filter(Boolean);
} else {
  const report = JSON.parse(readFileSync(opt('from-report'), 'utf8'));
  const manifest = JSON.parse(readFileSync(opt('pool-manifest'), 'utf8'));
  const byName = new Map(manifest.entries.map((e) => [e.name, e.source]));
  sources = report.files.filter((f) => f.flagged)
    .map((f) => byName.get(f.file)).filter(Boolean);
}
if (limit > 0) sources = sources.slice(0, limit);

// The root <svg> viewBox, expanded by `pad` on each side. Returns null
// when the source declares none — that is #630's path, not this one.
function expandViewBox(svgText) {
  const m = svgText.match(/<svg\b[^>]*>/i);
  if (!m) return null;
  const vb = m[0].match(/viewBox\s*=\s*["']\s*([-\d.eE]+)[,\s]+([-\d.eE]+)[,\s]+([-\d.eE]+)[,\s]+([-\d.eE]+)\s*["']/i);
  if (!vb) return null;
  const [x, y, w, h] = vb.slice(1, 5).map(Number);
  if (!(w > 0 && h > 0)) return null;
  const nx = x - w * pad, ny = y - h * pad;
  const nw = w * (1 + 2 * pad), nh = h * (1 + 2 * pad);
  const tag = m[0]
    .replace(/viewBox\s*=\s*["'][^"']*["']/i, `viewBox="${nx} ${ny} ${nw} ${nh}"`)
    .replace(/\swidth\s*=\s*["'][^"']*["']/i, '')
    .replace(/\sheight\s*=\s*["'][^"']*["']/i, '');
  return svgText.replace(m[0], tag);
}

const out = { checked: 0, lost: 0, skipped: 0, pad, minRingPct, files: [] };

for (const src of sources) {
  const text = readFileSync(join(pack, src), 'utf8');
  const padded = expandViewBox(text);
  if (!padded) {
    out.skipped++;
    out.files.push({ source: src, skipped: 'no-viewbox' });
    continue;
  }
  let data, info;
  try {
    ({ data, info } = await sharp(Buffer.from(padded), { density: 384 })
      .resize({ width: px, height: px, fit: 'inside', withoutEnlargement: false })
      .raw().toBuffer({ resolveWithObject: true }));
  } catch (err) {
    out.files.push({ source: src, error: err.message });
    continue;
  }
  const { width, height, channels } = info;
  // The ring is everything outside the region the ORIGINAL viewBox
  // occupies inside the padded one: pad/(1+2*pad) of each dimension.
  const fx = pad / (1 + 2 * pad);
  const x0 = Math.round(width * fx), x1 = width - x0;
  const y0 = Math.round(height * fx), y1 = height - y0;
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
  out.files.push({ source: src, ringSolidPixels: ring,
                   ringPixels: ringTotal, ringPct: Math.round(ratio * 100) / 100,
                   lost });
}

console.error(`probed ${out.checked} dimensioned sources at +${Math.round(pad * 100)}% viewBox: ` +
              `${out.lost} lost >= ${minRingPct}% of the ring, ${out.skipped} skipped (no viewBox)`);
process.stdout.write(JSON.stringify(out, null, 1) + '\n');
process.exit(out.lost ? 1 : 0);
