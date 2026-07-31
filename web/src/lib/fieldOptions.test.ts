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
  flattenOptions,
  selectableTreeOptions,
  VALUE_COLUMN,
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

  it('resolves a NESTED slug, which it previously could not', () => {
    // optionLabel only scanned the top level, so every term below the
    // root of a tree field's vocabulary rendered as its raw slug.
    const vocab = normalizeOptions({
      values: [
        { value: 'europe', label: 'Europe', children: [{ value: 'london', label: 'London' }] },
      ],
    });
    expect(optionLabel(vocab, 'london')).toBe('London');
  });
});

// ---------------------------------------------------------------------------
// The tree half (#778)
// ---------------------------------------------------------------------------

describe('flattenOptions', () => {
  it('leaves a flat vocabulary flat, so callers need not know which kind they hold', () => {
    const flat = flattenOptions(normalizeOptions({ values: ['sRGB', 'Linear'] }));
    expect(flat.map((o) => o.value)).toEqual(['sRGB', 'Linear']);
    expect(flat.every((o) => o.depth === 0)).toBe(true);
    expect(flat[0].path).toEqual(['sRGB']);
  });

  it('walks a nested vocabulary depth-first carrying depth and ancestor labels', () => {
    const opts = normalizeOptions({
      values: [
        {
          value: 'europe',
          label: 'Europe',
          children: [
            {
              value: 'uk',
              label: 'United Kingdom',
              children: [{ value: 'london', label: 'London' }],
            },
          ],
        },
        { value: 'asia', label: 'Asia' },
      ],
    });
    const flat = flattenOptions(opts);
    expect(flat.map((o) => [o.value, o.depth])).toEqual([
      ['europe', 0],
      ['uk', 1],
      ['london', 2],
      ['asia', 0],
    ]);
    expect(flat[2].path).toEqual(['Europe', 'United Kingdom', 'London']);
  });
});

describe('selectableTreeOptions', () => {
  const vocab = normalizeOptions({
    values: [
      {
        value: 'europe',
        label: 'Europe',
        children: [
          { value: 'london', label: 'London' },
          { value: 'old', label: 'Old', status: 'deprecated' },
          { value: 'gone', label: 'Gone', status: 'archived' },
        ],
      },
    ],
  });

  it('offers branches as well as leaves — a branch is a legitimate answer', () => {
    expect(selectableTreeOptions(vocab).map((o) => o.value)).toEqual(['europe', 'london']);
  });

  it('keeps a deprecated term a record already holds, at any depth', () => {
    // The nested case is the one that mattered: the flat picker never
    // descended, so a held nested term silently vanished from the list.
    expect(selectableTreeOptions(vocab, ['old']).map((o) => o.value)).toContain('old');
  });

  it('does not resurrect an archived term even when held', () => {
    expect(selectableTreeOptions(vocab, ['gone']).map((o) => o.value)).not.toContain('gone');
  });
});

describe('VALUE_COLUMN', () => {
  // The pin that stops #778 recurring on the frontend: ONE table, so an
  // editing surface and a display surface cannot disagree about which
  // column a type's value lives in. Mirrors valueColumnFor in
  // app/internal/metadata/valuecolumn_test.go — change one, change both.
  it('agrees with the server on every field type', () => {
    expect(VALUE_COLUMN).toEqual({
      text: 'value_text',
      longtext: 'value_text',
      rich_text: 'value_text',
      select: 'value_text',
      // NOT value_options (this editor's old answer) and NOT value_ref
      // (the display's old answer). One slug, in value_text.
      tree: 'value_text',
      number: 'value_num',
      // The asset WRITE path stores 0/1 in value_num while every
      // display reads "true"/"false" out of value_text. Same defect
      // class as tree, tracked separately; recorded here so it is
      // visible rather than surprising.
      boolean: 'value_text',
      date: 'value_date',
      datetime: 'value_date',
      multi_select: 'value_options',
      reference: 'value_ref',
    });
  });

  it('covers every type the field_definition CHECK constraint accepts', () => {
    expect(Object.keys(VALUE_COLUMN).sort()).toEqual(
      [
        'boolean',
        'date',
        'datetime',
        'longtext',
        'multi_select',
        'number',
        'reference',
        'rich_text',
        'select',
        'text',
        'tree',
      ].sort(),
    );
  });
});
