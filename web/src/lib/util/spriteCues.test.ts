// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The scrub cue parser (#835).
//
// These are the arithmetic the card's hover preview is now built on, and
// the two things they pin are the two things that were wrong before:
// HOW MANY cells to cycle, and WHERE each one is. The component test
// (CardThumb.scrub.test.ts) asserts the rendered result; this asserts
// the rules in isolation, including the malformed inputs a component
// test would be a clumsy place to enumerate.

import { describe, expect, it } from 'vitest';
import { parseSpriteVtt, cueBackgroundStyle } from './spriteCues';

/** A cue file exactly as preview/video.go writes it. */
const REAL_VTT = `WEBVTT

00:00:00.000 --> 00:00:00.299
sprites.jpg#xywh=0,0,240,134

00:00:00.299 --> 00:00:00.599
sprites.jpg#xywh=240,0,240,134

00:00:00.599 --> 00:00:00.899
sprites.jpg#xywh=480,0,240,134
`;

describe('parseSpriteVtt', () => {
  it('reads the rect off every cue, in order', () => {
    const cues = parseSpriteVtt(REAL_VTT);
    expect(cues).toEqual([
      { x: 0, y: 0, w: 240, h: 134 },
      { x: 240, y: 0, w: 240, h: 134 },
      { x: 480, y: 0, w: 240, h: 134 },
    ]);
  });

  it('drops the trailing zero-length cue pre-#835 sheets carry', () => {
    // THE NO-RE-RENDER PROPERTY. The old writer ran the full grid and
    // broke on `start >= duration` AFTER emitting that cue, so every
    // stored VTT for a short clip ends with an empty-window cue pointing
    // at the first cell ffmpeg padded with black. Dropping it here is
    // what fixes existing sheets without touching storage.
    const withTrailer =
      REAL_VTT + '\n00:00:00.899 --> 00:00:00.899\nsprites.jpg#xywh=720,0,240,134\n';
    expect(parseSpriteVtt(withTrailer)).toHaveLength(3);
  });

  it('keeps cues whose timings are unreadable', () => {
    // Tolerance, not laxity: a rect we can paint is worth showing even
    // if the timestamps are junk. The zero-length filter only fires when
    // BOTH timings parsed and the window is genuinely empty.
    const odd = 'WEBVTT\n\nnot:a:time --> also-not\nsprites.jpg#xywh=0,0,10,10\n';
    expect(parseSpriteVtt(odd)).toEqual([{ x: 0, y: 0, w: 10, h: 10 }]);
  });

  it('skips cues with no rect, a zero-area rect, or no payload', () => {
    const junk = `WEBVTT

00:00:00.000 --> 00:00:01.000
sprites.jpg

00:00:01.000 --> 00:00:02.000
sprites.jpg#xywh=0,0,0,134

00:00:02.000 --> 00:00:03.000
sprites.jpg#xywh=0,0,240,134
`;
    expect(parseSpriteVtt(junk)).toEqual([{ x: 0, y: 0, w: 240, h: 134 }]);
  });

  it('survives an empty or non-VTT body', () => {
    expect(parseSpriteVtt('')).toEqual([]);
    expect(parseSpriteVtt('<!doctype html><h1>404</h1>')).toEqual([]);
  });

  it('handles CRLF, which a proxy may introduce', () => {
    expect(parseSpriteVtt(REAL_VTT.replace(/\n/g, '\r\n'))).toHaveLength(3);
  });
});

describe('cueBackgroundStyle', () => {
  it('reproduces the pre-#835 hardcoded grid arithmetic for a full sheet', () => {
    // The old code computed `col * (100 / (cols - 1))` from a constant.
    // Deriving it from the sheet must give the identical answer, or every
    // existing sheet shifts by a fraction of a cell.
    const sheetW = 2400;
    const sheetH = 1340; // 10x10 of 240x134
    // Cell 34 = row 3, column 4.
    const style = cueBackgroundStyle({ x: 4 * 240, y: 3 * 134, w: 240, h: 134 }, sheetW, sheetH);
    expect(style.size).toBe('1000% 1000%');
    const [xPct, yPct] = style.position.split(' ').map(parseFloat);
    expect(xPct).toBeCloseTo(4 * (100 / 9), 6);
    expect(yPct).toBeCloseTo(3 * (100 / 9), 6);
  });

  it('pins the origin cell at 0% 0%', () => {
    expect(cueBackgroundStyle({ x: 0, y: 0, w: 240, h: 240 }, 1440, 1440).position).toBe('0% 0%');
  });

  it('does not divide by zero on a single-cell sheet', () => {
    // A sheet exactly one cell wide has no span to position within. The
    // percentage is undefined there; 0 is the only sane answer and NaN
    // would blank the layer.
    const style = cueBackgroundStyle({ x: 0, y: 0, w: 240, h: 134 }, 240, 134);
    expect(style.position).toBe('0% 0%');
    expect(style.size).toBe('100% 100%');
  });
});
