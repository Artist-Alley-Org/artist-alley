// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1169 — the srcset a CROPPING tile (grid's `fill`) is allowed to use.
//
// The bug this pins: `col` — a 320x320 square — was the only rung a
// cropped tile could name, because a contain rung's `max_dim` describes
// its LONG side and a cover slot is filled by the image's SHORT one. So
// every grid tile on every surface decoded 320px however wide the tile
// got (measured 1.63x upscale at the default rung on a DPR-2 display,
// 2.89x at rung 8).
//
// What is asserted here is the DESCRIPTOR ARITHMETIC, because that is
// what makes the browser's pick right or wrong — an over-promised
// descriptor parks a large tile on a file that cannot fill it, which is
// the same defect wearing a different rung's name.

import { beforeEach, describe, expect, it } from 'vitest';
import { previewLadder } from './previewLadder.svelte';

const ID = 'aa11bb22-0000-4000-8000-00000000c0de';

/** Parse a srcset into `{ key: widthDescriptor }`. */
function parse(srcset: string | null): Record<string, number> {
  const out: Record<string, number> = {};
  if (!srcset) return out;
  for (const c of srcset.split(',')) {
    const [url, w] = c.trim().split(/\s+/);
    out[url.split('/').pop()!] = parseInt(w, 10);
  }
  return out;
}

/** The install's default ladder (sysconfig DefaultPreviewConfig). */
function defaultLadder() {
  previewLadder.coverRungs = [{ key: 'col', maxDim: 320 }];
  previewLadder.rungs = [
    { key: 'preview', maxDim: 1024 },
    { key: 'screen', maxDim: 1920 },
    { key: 'hires', maxDim: 4096 },
  ];
}

describe('previewLadder.coverSrcsetFor (#1169)', () => {
  beforeEach(() => {
    previewLadder.rungs = [];
    previewLadder.coverRungs = [];
  });

  it('states each contain rung at its CROPPED width, not its max_dim', () => {
    defaultLadder();
    // 16:9 source, larger than every rung: a rung capped at max_dim on
    // its long side offers max_dim * 9/16 across a square slot.
    const got = parse(previewLadder.coverSrcsetFor(ID, 5120, 2880));
    expect(got).toEqual({ col: 320, preview: 576, screen: 1080, hires: 2304 });
  });

  it('keeps `col` a candidate, so a small tile is never made to pay more', () => {
    defaultLadder();
    const got = parse(previewLadder.coverSrcsetFor(ID, 4000, 4000));
    expect(got.col).toBe(320);
    // A square source crops to nothing, so the contain rungs land at
    // their full max_dim.
    expect(got.preview).toBe(1024);
  });

  it('caps every rung at the SOURCE, because the renderer skips upscales', () => {
    defaultLadder();
    // 512x512 source: `preview`, `screen` and `hires` all store 512px,
    // and `col` stores 320 (its own cap is lower). Claiming 1024/1920/
    // 4096 would park a wide tile on a file that cannot fill it.
    const got = parse(previewLadder.coverSrcsetFor(ID, 512, 512));
    expect(got).toEqual({ col: 320, preview: 512 });
  });

  it('emits one candidate per width — a repeated descriptor is arbitrary', () => {
    defaultLadder();
    const srcset = previewLadder.coverSrcsetFor(ID, 512, 512)!;
    const widths = srcset.split(',').map((c) => c.trim().split(/\s+/)[1]);
    expect(new Set(widths).size).toBe(widths.length);
    // The duplicate is resolved toward the SMALLEST rung that offers
    // the width — the smallest file, not the first one listed.
    expect(srcset).toContain('/variants/preview 512w');
    expect(srcset).not.toContain('/variants/hires');
  });

  it('falls back to the cover rungs alone when dimensions are unknown', () => {
    defaultLadder();
    // Without the source shape the crop fraction is a guess, so the
    // contain rungs cannot be described. On the default ladder that is
    // exactly the pre-#1169 behaviour: `col` and nothing else.
    expect(parse(previewLadder.coverSrcsetFor(ID, null, null))).toEqual({ col: 320 });
  });

  it('returns null with no ladder at all, so the card falls back to col', () => {
    expect(previewLadder.coverSrcsetFor(ID, 1600, 900)).toBeNull();
  });

  it('never hardcodes `col` — an operator-renamed cover rung is used', () => {
    previewLadder.coverRungs = [{ key: 'square', maxDim: 480 }];
    previewLadder.rungs = [{ key: 'big', maxDim: 2400 }];
    const got = parse(previewLadder.coverSrcsetFor(ID, 2000, 1000));
    expect(got).toEqual({ square: 480, big: 1000 });
  });

  it('leaves the CONTAIN srcset stating full max_dim', () => {
    defaultLadder();
    // The contain slot is unchanged by #1169: there the file's own
    // width is what crosses the slot.
    expect(parse(previewLadder.srcsetFor(ID))).toEqual({
      preview: 1024,
      screen: 1920,
      hires: 4096,
    });
  });
});

// #1210: a FRAMED cropping tile, which cannot be offered a rung the
// server already cropped.
//
// The defect this pins is not a 404 and not a wasted byte: it is a crop
// landing somewhere nobody chose. A focal fraction is measured against
// the ORIGINAL picture, and `col` is a square taken at that picture's
// centre before the fraction could act, so `object-position` over it
// moves a crop of a crop. Leaving `col` in the srcset would make the
// framing correct at some tile widths and wrong at others, decided by
// the viewport, which is worse than either answer on its own.
describe('previewLadder.coverSrcsetFor with containOnly (#1210)', () => {
  beforeEach(() => {
    previewLadder.rungs = [];
    previewLadder.coverRungs = [];
  });

  it('drops the pre-cropped rungs and keeps the corrected descriptors', () => {
    defaultLadder();
    const got = parse(previewLadder.coverSrcsetFor(ID, 5120, 2880, true));
    // The same widths #1169 computes, minus `col`. The correction is
    // about the SLOT's shape, which framing does not change.
    expect(got).toEqual({ preview: 576, screen: 1080, hires: 2304 });
    expect(got.col).toBeUndefined();
  });

  it('never names an operator-renamed cover rung either', () => {
    previewLadder.coverRungs = [{ key: 'square', maxDim: 480 }];
    previewLadder.rungs = [{ key: 'big', maxDim: 2400 }];
    const got = parse(previewLadder.coverSrcsetFor(ID, 2000, 1000, true));
    expect(got).toEqual({ big: 1000 });
  });

  it('returns null without source dimensions, so the caller drops the framing', () => {
    defaultLadder();
    // No dimensions means no describable contain candidate, and the
    // only thing left would be the square the framing must not use.
    // Null is what makes the caller fall back to `col` CENTRED rather
    // than to `col` positioned.
    expect(previewLadder.coverSrcsetFor(ID, null, null, true)).toBeNull();
  });

  it('returns null with no contain rungs at all', () => {
    previewLadder.coverRungs = [{ key: 'col', maxDim: 320 }];
    previewLadder.rungs = [];
    expect(previewLadder.coverSrcsetFor(ID, 1600, 900, true)).toBeNull();
    // ...where the same call WITHOUT framing still has somewhere to go.
    expect(previewLadder.coverSrcsetFor(ID, 1600, 900, false)).not.toBeNull();
  });
});
