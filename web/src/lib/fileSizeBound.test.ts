// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 18d — the file-size conversion, asserted as DIGIT
// STRINGS.
//
// ⛔ EVERY EXPECTATION HERE IS A STRING COMPARISON, and that is the
// point rather than a style. `expect(bytes).toBe(9223372036854775807n)`
// would pass against an implementation that routed the value through a
// `number`, because the literal on the right would be rounded the same
// way the implementation rounded it — two wrongs agreeing. Comparing the
// rendered decimal to a hand-written digit string is the only assertion
// a float cannot satisfy.

import { describe, it, expect } from 'vitest';
import {
  fileSizeToBytes,
  fileSizeTerm,
  INT64_MAX,
  DEFAULT_FILE_SIZE_UNIT,
} from './fileSizeBound';

/** The exact byte count, as digits, or the failure reason. */
function digits(raw: string, unit: Parameters<typeof fileSizeToBytes>[1], edge: 'lower' | 'upper') {
  const r = fileSizeToBytes(raw, unit, edge);
  return r.ok ? r.bytes.toString() : `!${r.reason}`;
}

describe('fileSizeToBytes — the 1024 convention', () => {
  it('multiplies by the product’s existing 1024-based units', () => {
    expect(digits('1', 'B', 'lower')).toBe('1');
    expect(digits('1', 'KB', 'lower')).toBe('1024');
    expect(digits('1', 'MB', 'lower')).toBe('1048576');
    expect(digits('1', 'GB', 'lower')).toBe('1073741824');
  });

  it('defaults to MB', () => {
    expect(DEFAULT_FILE_SIZE_UNIT).toBe('MB');
  });
});

describe('fileSizeToBytes — the rounding directions are opposite', () => {
  // 1000.25 bytes: the brief's own worked example, and the smallest
  // case that separates the two edges.
  it('a lower bound CEILS and an upper bound FLOORS', () => {
    expect(digits('1000.25', 'B', 'lower')).toBe('1001');
    expect(digits('1000.25', 'B', 'upper')).toBe('1000');
  });

  it('an exact value rounds nowhere', () => {
    expect(digits('1000', 'B', 'lower')).toBe('1000');
    expect(digits('1000', 'B', 'upper')).toBe('1000');
  });

  it('a fractional unit value is exact in both directions', () => {
    // 1.5 KB = 1536 bytes exactly.
    expect(digits('1.5', 'KB', 'lower')).toBe('1536');
    expect(digits('1.5', 'KB', 'upper')).toBe('1536');
    // 0.0009765625 KB = 1 byte exactly; anything less than that ceils
    // to 1 and floors to 0.
    expect(digits('0.0009765625', 'KB', 'lower')).toBe('1');
    expect(digits('0.0009765625', 'KB', 'upper')).toBe('1');
    expect(digits('0.00048828125', 'KB', 'lower')).toBe('1');
    expect(digits('0.00048828125', 'KB', 'upper')).toBe('0');
  });
});

describe('fileSizeToBytes — exactness past 2^53', () => {
  // ⛔ THE MUTATION THESE CATCH. Route the value through a `number` and
  // every one of these comes back off by a few — silently, and only
  // here, which is why a magnitude assertion would not notice.
  it('int64 max is ACCEPTED and survives verbatim', () => {
    expect(digits('9223372036854775807', 'B', 'lower')).toBe('9223372036854775807');
    expect(digits('9223372036854775807', 'B', 'upper')).toBe('9223372036854775807');
    expect(INT64_MAX.toString()).toBe('9223372036854775807');
  });

  it('int64 max PLUS ONE is refused', () => {
    expect(digits('9223372036854775808', 'B', 'lower')).toBe('!out_of_range');
    expect(digits('9223372036854775808', 'B', 'upper')).toBe('!out_of_range');
  });

  it('a value above 2^53 below int64 max is exact, not rounded', () => {
    // 2^53 + 1 = 9007199254740993. A double cannot represent it: it
    // rounds to 9007199254740992.
    expect(digits('9007199254740993', 'B', 'lower')).toBe('9007199254740993');
    expect(digits('9007199254740993', 'B', 'upper')).toBe('9007199254740993');
    // And one that is not adjacent to the boundary either.
    expect(digits('12345678901234567', 'B', 'lower')).toBe('12345678901234567');
  });

  it('a large value in a large unit stays exact', () => {
    // 8589934591 GB = 9223372036854775808 - 1073741824 bytes.
    expect(digits('8589934591', 'GB', 'lower')).toBe('9223372035781033984');
    // One more GB overflows.
    expect(digits('8589934592', 'GB', 'lower')).toBe('!out_of_range');
  });
});

describe('fileSizeToBytes — what is refused', () => {
  it('empty is “no bound”, not an error', () => {
    expect(digits('', 'MB', 'lower')).toBe('!empty');
    expect(digits('   ', 'MB', 'lower')).toBe('!empty');
  });

  it('anything but plain base-10 digits is malformed', () => {
    for (const bad of ['1e3', '+5', '-5', '0x10', '1_000', '1,000', 'Infinity', 'NaN', '1.2.3', 'abc', '5MB', '.']) {
      expect(digits(bad, 'MB', 'lower'), `${bad} must be refused`).toBe('!malformed');
    }
  });

  it('zero is a legal minimum', () => {
    expect(digits('0', 'MB', 'lower')).toBe('0');
    expect(digits('0', 'MB', 'upper')).toBe('0');
  });

  it('a bare decimal point form is accepted on both sides', () => {
    expect(digits('.5', 'KB', 'lower')).toBe('512');
    expect(digits('5.', 'B', 'lower')).toBe('5');
  });
});

describe('fileSizeTerm — the wire token', () => {
  it('leads with the operator and carries a bare bound', () => {
    expect(fileSizeTerm('1', 'MB', 'lower')).toBe('file_size:>=1048576');
    expect(fileSizeTerm('1', 'MB', 'upper')).toBe('file_size:<=1048576');
  });

  it('emits NOTHING for an empty or invalid value', () => {
    expect(fileSizeTerm('', 'MB', 'lower')).toBeNull();
    expect(fileSizeTerm('1e3', 'MB', 'lower')).toBeNull();
    expect(fileSizeTerm('9223372036854775808', 'B', 'lower')).toBeNull();
  });
});
