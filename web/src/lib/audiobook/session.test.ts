// Audiobook duration formatters. Pure, used to render the player
// chrome and the chapter list — a regression flips the chapter
// timestamps to gibberish or shows the wrong clock at playback.

import { describe, expect, it } from 'vitest';
import { fmtClock, fmtSpan } from './session.svelte';

describe('fmtClock', () => {
  it.each([
    [0, '0:00'],
    [5, '0:05'],
    [65, '1:05'],
    [125, '2:05'],
    [3600, '1:00:00'],
    [3665, '1:01:05'],
    // H:MM:SS form switches at 1 hour. Minutes get zero-padded only
    // when an hour component is present (the chapter list shows
    // "5:23" not "05:23" when there's no hour to disambiguate).
    [600, '10:00'],
    [3540, '59:00'],
  ])('formats %i seconds as %s', (sec, want) => {
    expect(fmtClock(sec)).toBe(want);
  });

  it('treats negative + non-finite values as 0:00', () => {
    expect(fmtClock(-5)).toBe('0:00');
    expect(fmtClock(Number.NaN)).toBe('0:00');
    expect(fmtClock(Number.POSITIVE_INFINITY)).toBe('0:00');
  });

  it('truncates fractional seconds toward zero', () => {
    expect(fmtClock(59.9)).toBe('0:59');
    expect(fmtClock(60.4)).toBe('1:00');
  });
});

describe('fmtSpan', () => {
  it.each([
    [0, '0s'],
    [1, '1s'],
    [45, '45s'],
    [60, '1m 0s'],
    [125, '2m 5s'],
    [3600, '1h 0m'],
    [3700, '1h 1m'],
    [7200, '2h 0m'],
  ])('formats %i seconds as %s', (sec, want) => {
    expect(fmtSpan(sec)).toBe(want);
  });

  it('treats negative + zero + non-finite as "0s"', () => {
    expect(fmtSpan(0)).toBe('0s');
    expect(fmtSpan(-5)).toBe('0s');
    expect(fmtSpan(Number.NaN)).toBe('0s');
  });
});
