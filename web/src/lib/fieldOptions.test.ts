// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, it, expect } from 'vitest';
import {
  normalizeOptions,
  serializeOptions,
  selectableOptions,
  optionLabel,
  isSelectable,
  isResolvable,
} from './fieldOptions';

describe('normalizeOptions', () => {
  it('reads the bare-slug form every seeded field actually stores', () => {
    // Verbatim from field_definition.options on a seeded instance.
    const opts = normalizeOptions({ values: ['sRGB', 'Linear', 'Raw', 'N/A'] });
    expect(opts).toHaveLength(4);
    expect(opts[0]).toEqual({ value: 'sRGB', label: 'sRGB', status: 'active' });
  });

  it('reads the object form ADR 0012 documents', () => {
    const opts = normalizeOptions({
      values: [{ value: 'low', label: 'Low' }, { value: 'high', label: 'High' }],
    });
    expect(opts[0]).toEqual({ value: 'low', label: 'Low', status: 'active' });
  });

  it('treats an absent status as active — this is what keeps live fields valid', () => {
    expect(normalizeOptions({ values: [{ value: 'a' }] })[0].status).toBe('active');
  });

  it('carries status and replaced_by through', () => {
    const [o] = normalizeOptions({
      values: [{ value: 'Raw', status: 'deprecated', replaced_by: 'Linear' }],
    });
    expect(o.status).toBe('deprecated');
    expect(o.replaced_by).toBe('Linear');
  });

  it('ignores an unknown status rather than rendering it as a lifecycle', () => {
    expect(normalizeOptions({ values: [{ value: 'a', status: 'retired' }] })[0].status).toBe('active');
  });

  it('returns empty for non-vocabulary documents', () => {
    expect(normalizeOptions(undefined)).toEqual([]);
    expect(normalizeOptions({})).toEqual([]);
    expect(normalizeOptions({ min: 0, max: 10 })).toEqual([]);
  });
});

describe('serializeOptions', () => {
  it('round-trips a pre-lifecycle document unchanged', () => {
    const values = ['sRGB', 'Linear', 'Raw', 'N/A'];
    expect(serializeOptions(normalizeOptions({ values }))).toEqual(values);
  });

  it('keeps a real label as an object', () => {
    expect(serializeOptions(normalizeOptions({ values: [{ value: 'low', label: 'Low' }] }))).toEqual([
      { value: 'low', label: 'Low' },
    ]);
  });

  it('only grows the entries that actually carry a lifecycle', () => {
    const opts = normalizeOptions({ values: ['sRGB', 'Raw'] });
    opts[1] = { ...opts[1], status: 'deprecated', replaced_by: 'sRGB' };
    expect(serializeOptions(opts)).toEqual([
      'sRGB',
      { value: 'Raw', status: 'deprecated', replaced_by: 'sRGB' },
    ]);
  });
});

describe('option lifecycle', () => {
  it('separates being offered from still resolving', () => {
    expect(isSelectable({ value: 'a', label: 'a', status: 'active' })).toBe(true);
    expect(isSelectable({ value: 'a', label: 'a', status: 'deprecated' })).toBe(false);
    expect(isSelectable({ value: 'a', label: 'a', status: 'archived' })).toBe(false);

    expect(isResolvable({ value: 'a', label: 'a', status: 'active' })).toBe(true);
    expect(isResolvable({ value: 'a', label: 'a', status: 'deprecated' })).toBe(true);
    expect(isResolvable({ value: 'a', label: 'a', status: 'archived' })).toBe(false);
  });
});

describe('selectableOptions', () => {
  const all = normalizeOptions({
    values: [
      'sRGB',
      'Linear',
      { value: 'Raw', status: 'deprecated', replaced_by: 'Linear' },
      { value: 'Gone', status: 'archived' },
    ],
  });

  it('drops deprecated and archived terms from a fresh picker', () => {
    expect(selectableOptions(all).map((o) => o.value)).toEqual(['sRGB', 'Linear']);
  });

  it('KEEPS a deprecated term the record already holds', () => {
    // Both halves matter. Dropping it here would blank the value on an
    // asset nobody edited — the failure ADR 0012 guards against.
    expect(selectableOptions(all, ['Raw']).map((o) => o.value)).toEqual(['sRGB', 'Linear', 'Raw']);
  });

  it('does not resurrect an archived term even when held', () => {
    expect(selectableOptions(all, ['Gone']).map((o) => o.value)).toEqual(['sRGB', 'Linear']);
  });
});

describe('optionLabel', () => {
  const all = normalizeOptions({ values: ['sRGB', { value: 'lin', label: 'Linear' }] });

  it('resolves a slug to its display label', () => {
    expect(optionLabel(all, 'lin')).toBe('Linear');
  });

  it('falls back to the slug for a value with no matching option', () => {
    expect(optionLabel(all, 'mystery')).toBe('mystery');
  });
});
