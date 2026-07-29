// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// What frame does an SVG actually get rendered into? (#685)
//
// Shared by rasterize_svg.mjs, which USES the answer, and
// probe_render_loss.mjs, which VERIFIES it. They had drifted into two
// copies of the same regex and two ideas of what "the canvas" means, and
// the drift is not academic: the probe measured loss against the canvas a
// source DECLARES while the rasteriser had started ignoring that
// declaration when it was wrong, so every correctly repaired file would
// have reported as broken forever after. A gate that fires on its own
// pipeline's known-good output is the #685 disease itself.
//
// Nothing here decides policy. The rasteriser decides what to render and
// the probe decides what counts as loss; this module only answers three
// factual questions — what canvas is declared, what does the drawing
// actually span, and is the second inside the first.

let sharp;
try {
  sharp = (await import('sharp')).default;
} catch (err) {
  throw new Error(`svg_frame.mjs requires sharp: ${err.message}`);
}

/** The probe render's long edge, in pixels. Every measurement below is
 *  quantised to `max(rect.w, rect.h) / PROBE_PX` user units, which is
 *  what makes the safety margins proportional to the drawing rather than
 *  to whatever canvas it was measured on (#672). */
export const PROBE_PX = 1024;

/** True iff the root declares no viewBox and not both of width/height —
 *  the class librsvg has to guess a canvas for (#630). */
export function isDimensionless(svgText) {
  const m = svgText.match(/<svg\b[^>]*>/i);
  if (!m) return false; // not detectably an svg; let sharp error normally
  const tag = m[0];
  const hasViewBox = /\bviewBox\s*=/.test(tag);
  const hasWH = /\bwidth\s*=/.test(tag) && /\bheight\s*=/.test(tag);
  return !hasViewBox && !hasWH;
}

/** The canvas the root declares, in user units, or null when it declares
 *  none this module can resolve. viewBox wins; failing that width and
 *  height, unitless or in px — the Kenney pack writes them unitless
 *  ("32", not "32px"), so "0 0 width height" is that canvas exactly and
 *  not an approximation. Anything else (`2cm`, `50%`) returns null rather
 *  than guess: a canvas measured in the wrong coordinate space is worse
 *  than no measurement. */
export function declaredCanvas(svgText) {
  const m = svgText.match(/<svg\b[^>]*>/i);
  if (!m) return null;
  const tag = m[0];
  const vb = tag.match(/viewBox\s*=\s*["']\s*([-\d.eE]+)[,\s]+([-\d.eE]+)[,\s]+([-\d.eE]+)[,\s]+([-\d.eE]+)\s*["']/i);
  if (vb) {
    const [x, y, w, h] = vb.slice(1, 5).map(Number);
    return w > 0 && h > 0 ? { x, y, w, h } : null;
  }
  const wm = tag.match(/\swidth\s*=\s*["']\s*([\d.eE+-]+)(?:px)?\s*["']/i);
  const hm = tag.match(/\sheight\s*=\s*["']\s*([\d.eE+-]+)(?:px)?\s*["']/i);
  if (!wm || !hm) return null;
  const w = Number(wm[1]), h = Number(hm[1]);
  return w > 0 && h > 0 ? { x: 0, y: 0, w, h } : null;
}

/** Replace the root canvas with `rect`. width/height are stripped as well
 *  as viewBox rewritten: left in place they letterbox the patched box back
 *  to the declared aspect, which silently defeats the measurement. */
export function withViewBox(svgText, rect) {
  const vb = `viewBox="${rect.x} ${rect.y} ${rect.w} ${rect.h}"`;
  const m = svgText.match(/<svg\b[^>]*>/i);
  if (!m) return svgText.replace(/<svg\b/, `<svg ${vb}`);
  let tag = m[0]
    .replace(/\swidth\s*=\s*["'][^"']*["']/i, '')
    .replace(/\sheight\s*=\s*["'][^"']*["']/i, '');
  tag = /viewBox\s*=/i.test(tag)
    ? tag.replace(/viewBox\s*=\s*["'][^"']*["']/i, vb)
    : tag.replace(/<svg\b/i, `<svg ${vb}`);
  return svgText.replace(m[0], tag);
}

/** Grow a rect by `frac` of its long edge on every side. */
export function padRect(rect, frac) {
  const pad = Math.max(rect.w, rect.h) * frac;
  return { x: rect.x - pad, y: rect.y - pad, w: rect.w + 2 * pad, h: rect.h + 2 * pad };
}

/** The square, negative-inclusive search frames the first pass uses. */
export const searchRect = (origin, units) => ({ x: origin, y: origin, w: units, h: units });

/** Measure the content bounding box (in user units) by rendering a probe
 *  frame over `rect` and scanning the alpha channel. The probe's long edge
 *  is always PROBE_PX, so one probe pixel is `max(w,h)/PROBE_PX` units and
 *  the returned box carries a two-of-those safety margin — proportional to
 *  the region being measured, which is the point of the refine pass
 *  (#672). Returns null when the frame contains no content at all.
 *
 *  NEVER call this with a huge rect and a high density: 8192 units at
 *  384dpi is a ~44k-pixel edge, gigabytes of RGBA. The density is derived
 *  from the rect so the probe is always PROBE_PX. */
export async function measureBBox(svgText, rect) {
  const units = Math.max(rect.w, rect.h);
  const unitsPerPx = units / PROBE_PX;
  const patched = withViewBox(svgText, rect);
  const { data, info } = await sharp(Buffer.from(patched), {
    density: (72 * PROBE_PX) / units,
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

/** The drawing's true extent, found without consulting any canvas the
 *  file declares or librsvg guessed — search, then refine at the
 *  drawing's own scale. Throws rather than return a frame that might
 *  still be cutting something. */
export async function measuredExtent(svgText) {
  // Search canvas 1: -2048..6144 (negative-inclusive, because "the
  // observed clipping is positive-quadrant" is an observation, not a
  // guarantee). Canvas 2, if content runs off the first: 4x wider.
  let bbox = await measureBBox(svgText, searchRect(-2048, 8192));
  if (bbox && bbox.touchesEdge) bbox = await measureBBox(svgText, searchRect(-8192, 32768));
  if (!bbox) throw new Error('SVG rendered no content in the probe frame');
  if (bbox.touchesEdge) {
    throw new Error('content exceeds a 32768-unit probe canvas; refusing to render a silently-cropped frame');
  }
  // REFINE (#672): re-measure at the drawing's own scale so the safety
  // margin is proportional. A refine probe that finds nothing, or finds
  // content on its own edge, is not trusted — the search bbox is already
  // a correct-but-loose frame, so fall back to it rather than risk a
  // tighter frame that clips.
  const refined = await measureBBox(svgText, padRect(bbox, 0.02));
  return refined && !refined.touchesEdge ? refined : bbox;
}

/** The margin the overflow probe leaves around the declared canvas, as a
 *  fraction of its long edge. Only wide enough to tell "content stops at
 *  the declared edge" from "content carries on past it"; content that
 *  runs past even this reads as touchesEdge, which is the same answer. */
export const OVERFLOW_PROBE_PAD = 0.25;

/** How far outside its declared canvas a drawing may reach before the
 *  declaration is treated as wrong, as a fraction of the canvas's long
 *  edge. Measured on the 806 dimensioned pool sources, this separates
 *  cleanly by an order of magnitude:
 *
 *    0.46-0.68%   12 sources — a shape that ENDS on the declared edge,
 *                 whose anti-aliased fringe lands a fraction of a unit
 *                 outside it. Not a defect; reframing them would rewrite
 *                 correct files to move an invisible hairline.
 *    5.8-25.3%     6 sources — the stale Flash artboards, the #685 defect.
 *
 *  A tolerance set at the probe's own resolution instead (three probe
 *  pixels, which is what this first shipped as) sits *below* the fringe
 *  tail and reframes all 18. That is the same mistake as measuring a ring
 *  boundary to the nearest pixel: a threshold at the noise floor is not a
 *  threshold. `probe_render_loss.mjs --frame declared` still reports the
 *  tail for anyone who wants to look at it. */
export const OVERFLOW_MATERIAL_FRAC = 0.02;

/** Does anything MATERIALLY render outside the canvas the source
 *  declares? (#685) Measured, not assumed. The tolerance is the larger of
 *  OVERFLOW_MATERIAL_FRAC of the declared long edge and three probe
 *  pixels — the latter only so a tiny canvas cannot ask for a tolerance
 *  finer than the measurement that feeds it (two probe pixels to undo
 *  measureBBox's own safety margin, one for the fringe). */
export async function overflowsDeclaredCanvas(svgText, box) {
  const rect = padRect(box, OVERFLOW_PROBE_PAD);
  const bbox = await measureBBox(svgText, rect);
  if (!bbox) return false;           // renders nothing; not this check's call
  if (bbox.touchesEdge) return true; // runs past even the padded probe
  const tol = Math.max(
    OVERFLOW_MATERIAL_FRAC * Math.max(box.w, box.h),
    3 * (Math.max(rect.w, rect.h) / PROBE_PX),
  );
  return bbox.x < box.x - tol ||
         bbox.y < box.y - tol ||
         bbox.x + bbox.w > box.x + box.w + tol ||
         bbox.y + bbox.h > box.y + box.h + tol;
}

/** The frame rasterize_svg.mjs will actually render `svgText` into, and
 *  why. This is the single definition of the pipeline's framing decision;
 *  the rasteriser renders the result and the loss probe measures against
 *  it, so the two cannot disagree about what was rendered.
 *
 *  Returns { rect, reason } where reason is one of:
 *    'declared'      the source's own canvas, used as-is (the common case)
 *    'measured'      no resolvable canvas — librsvg would have guessed
 *    'overflow'      a canvas was declared and it cuts the drawing
 *  `rect` is null only for 'declared', where the caller should hand the
 *  file to sharp untouched and keep byte-for-byte output. */
export async function pipelineFrame(svgText) {
  if (isDimensionless(svgText)) {
    return { rect: await measuredExtent(svgText), reason: 'measured' };
  }
  const declared = declaredCanvas(svgText);
  if (declared && await overflowsDeclaredCanvas(svgText, declared)) {
    return { rect: await measuredExtent(svgText), reason: 'overflow', declared };
  }
  return { rect: null, reason: 'declared', declared };
}
