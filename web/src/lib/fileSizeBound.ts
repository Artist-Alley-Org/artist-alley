// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * Exact conversion from a typed file size to the byte bound the
 * `file_size:` dimension takes (#1173, sprint 18d).
 *
 * # ⛔ NO `number` IS INVOLVED, ANYWHERE ON THIS PATH
 *
 * `assets.file_size_bytes` is BIGINT and reaches past 2^53, where a
 * JavaScript `number` stops being able to tell consecutive integers
 * apart. ADR 0093's 18b amendment says so for the Go half and the same
 * fact holds one language over: a byte count that goes through a
 * `number` comes back as a DIFFERENT number, silently, and only for
 * large files — which is exactly where a size filter gets used.
 *
 * So the caller's decimal string is parsed as digits, multiplied as a
 * `BigInt`, and rendered back as base-10 digits. `parseFloat`,
 * `Number()`, `*`, `/` and `Math.round` are absent on purpose; a
 * refactor that reintroduces one breaks the exactness tests beside this
 * file rather than the product's visible behaviour, which is why those
 * tests assert DIGIT STRINGS and not magnitudes.
 *
 * # The rounding directions are NOT symmetric, and that is the contract
 *
 * A bound has to keep its own promise when the unit does not divide
 * evenly:
 *
 *   - a LOWER bound (`>=`) rounds UP. "at least 1000.25 bytes" admits
 *     1001 and must not admit 1000.
 *   - an UPPER bound (`<=`) rounds DOWN. "at most 1000.25 bytes" admits
 *     1000 and must not admit 1001.
 *
 * Rounding both the same way would let one end of a range match a file
 * the person excluded.
 */

/** The units offered, smallest first. */
export const FILE_SIZE_UNITS = ['B', 'KB', 'MB', 'GB'] as const;

export type FileSizeUnit = (typeof FILE_SIZE_UNITS)[number];

/**
 * The default unit. MB because it is the magnitude a person reaches for
 * when they think about "a big file" — the product's own
 * `humanSize` (UploadFileRow.svelte) crosses into MB at a megabyte and
 * stays there for most of a working corpus.
 */
export const DEFAULT_FILE_SIZE_UNIT: FileSizeUnit = 'MB';

/**
 * Bytes per unit, on the product's EXISTING 1024-based convention.
 *
 * Not a fresh decision: `humanSize` in UploadFileRow.svelte divides by
 * 1024, 1024², 1024³ to print the size beside a file being uploaded. A
 * filter that read KB as 1000 would disagree with the number the person
 * saw when they uploaded the file they are now looking for.
 */
const UNIT_BYTES: Record<FileSizeUnit, bigint> = {
  B: 1n,
  KB: 1024n,
  MB: 1048576n,
  GB: 1073741824n,
};

/** `2^63 - 1` — the largest value `file_size_bytes` can hold. */
export const INT64_MAX = 9223372036854775807n;

/**
 * Plain base-10, unsigned, with at most one decimal point.
 *
 * ⛔ Deliberately NARROW. `1e3`, `+5`, `-5`, `0x10`, `1_000`, `1,000`,
 * `Infinity` and `NaN` are all things `Number()` would happily accept
 * and none of them is a file size a person typed. A refusal here is an
 * inline validation message; it is never a term, because a bound that
 * quietly became something else is worse than no bound at all.
 */
const DECIMAL = /^(\d+)(?:\.(\d*))?$|^\.(\d+)$/;

export type FileSizeBoundResult =
  | { ok: true; bytes: bigint }
  /** `empty` means "no bound asked for" — not an error to show. */
  | { ok: false; reason: 'empty' | 'malformed' | 'out_of_range' };

/**
 * Convert one typed value in `unit` to an exact byte bound.
 *
 * `edge` picks the rounding direction: `'lower'` ceils, `'upper'`
 * floors. See the module doc for why they differ.
 */
export function fileSizeToBytes(
  raw: string,
  unit: FileSizeUnit,
  edge: 'lower' | 'upper',
): FileSizeBoundResult {
  const s = raw.trim();
  if (s === '') return { ok: false, reason: 'empty' };

  const m = DECIMAL.exec(s);
  if (!m) return { ok: false, reason: 'malformed' };

  const whole = m[1] ?? '0';
  const frac = m[2] ?? m[3] ?? '';

  // The value is `digits / 10^frac.length` units. Multiplying by the
  // unit BEFORE dividing is what keeps it exact: the division is the
  // only lossy step and it happens once, at the end, in the direction
  // the edge asks for.
  const digits = BigInt(whole + frac);
  const denominator = 10n ** BigInt(frac.length);
  const numerator = digits * UNIT_BYTES[unit];

  // Both operands are non-negative, so BigInt's truncating division IS
  // floor, and ceil is the standard `(a + b - 1) / b`.
  const bytes =
    edge === 'upper' ? numerator / denominator : (numerator + denominator - 1n) / denominator;

  if (bytes < 0n || bytes > INT64_MAX) return { ok: false, reason: 'out_of_range' };
  return { ok: true, bytes };
}

/**
 * The `filter=` wire token for one bound, or `null` when there is none
 * to emit.
 *
 * The value shape is 18b's: the operator LEADS and the bound is bare,
 * because `file_size` names exactly one column and has nothing to
 * disambiguate — unlike `field:`'s compound `code<op>value`.
 */
export function fileSizeTerm(
  raw: string,
  unit: FileSizeUnit,
  edge: 'lower' | 'upper',
): string | null {
  const r = fileSizeToBytes(raw, unit, edge);
  if (!r.ok) return null;
  return `file_size:${edge === 'lower' ? '>=' : '<='}${r.bytes.toString()}`;
}
