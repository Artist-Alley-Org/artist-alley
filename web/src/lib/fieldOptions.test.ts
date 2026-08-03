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
  encodeBoolean,
  decodeBoolean,
  findOption,
  slugify,
  resolveTerm,
  allOptionSlugs,
  childrenAtPath,
  containsPath,
  insertOptionAtPath,
  moveDestinations,
  moveOptionWithinSiblings,
  optionAtPath,
  removeOptionAtPath,
  reparentOption,
  updateOptionAtPath,
  type FieldOption,
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
      // 1/0 in value_num, per ADR 0012. This said value_text until
      // #791 — matching three frontend surfaces and contradicting the
      // API, which has only ever accepted value_num for a boolean.
      boolean: 'value_num',
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

describe('boolean encoding', () => {
  // The frontend half of #791's encoding pin. Agreeing with the server
  // on the COLUMN is not enough: three surfaces wrote and read the
  // strings "true"/"false", and had they written those into value_num
  // they would still have been wrong. So pin the representation, and
  // pin it in one place every boolean surface imports.

  it('writes 1 for true and 0 for false', () => {
    expect(encodeBoolean(true)).toBe(1);
    expect(encodeBoolean(false)).toBe(0);
  });

  it('reads 1 and 0 back, and nothing else', () => {
    expect(decodeBoolean(1)).toBe(true);
    expect(decodeBoolean(0)).toBe(false);
    // The pre-#791 encoding, arriving in the numeric column.
    expect(decodeBoolean(NaN)).toBeNull();
    expect(decodeBoolean(2)).toBeNull();
  });

  it('distinguishes "not set" from "false"', () => {
    // The reason this is a function and not `!!f.value_num`: 0 is
    // falsy, so a naive truthiness test renders a set `false` and an
    // unset field identically. A display shows "No" for one and
    // nothing at all for the other.
    expect(decodeBoolean(null)).toBeNull();
    expect(decodeBoolean(undefined)).toBeNull();
    expect(decodeBoolean(0)).toBe(false);
  });

  it('round-trips', () => {
    expect(decodeBoolean(encodeBoolean(true))).toBe(true);
    expect(decodeBoolean(encodeBoolean(false))).toBe(false);
  });
});

// ── Open vocabularies (#830/#846) ───────────────────────────────────
//
// These two functions are the browser's copy of the server's write
// rule — Slugify and indexVocabulary/resolveOrMint in
// app/internal/metadata/open_vocabulary.go. They exist so a picker can
// PREVIEW what a typed term will become, and a preview that disagrees
// with the write tells the operator a specific untruth about their own
// catalogue. So these cases are deliberately the server's cases,
// asserted against the answers the server gives.

describe('slugify', () => {
  it('lowercases and hyphenates runs of non-alphanumerics', () => {
    expect(slugify('Macro Detail')).toBe('macro-detail');
    expect(slugify('Black & White')).toBe('black-white');
    expect(slugify('black_and_white')).toBe('black-and-white');
    expect(slugify('  Sunset   Over  Water  ')).toBe('sunset-over-water');
  });

  it('trims leading and trailing hyphens', () => {
    expect(slugify('!!!hello!!!')).toBe('hello');
    expect(slugify('-a-')).toBe('a');
  });

  it('returns empty for a term with no addressable form', () => {
    // The server treats this as a term that cannot be created, and so
    // must the picker — otherwise it offers to create nothing.
    expect(slugify('!!!')).toBe('');
    expect(slugify('   ')).toBe('');
  });

  it('caps at 80 characters without leaving a trailing hyphen', () => {
    const long = 'a'.repeat(100);
    expect(slugify(long)).toHaveLength(80);
    // A cut landing on a separator must not leave the hyphen behind.
    expect(slugify('a'.repeat(79) + ' tail')).toBe('a'.repeat(79));
  });
});

describe('resolveTerm', () => {
  const vocab = normalizeOptions({
    values: [
      { value: 'landscape', label: 'Landscape' },
      { value: 'black-and-white', label: 'Black and White' },
      { value: 'sepia', label: 'Sepia', status: 'deprecated' },
      { value: 'daguerreotype', label: 'Daguerreotype', status: 'archived' },
    ],
  });

  it('matches a slug case-insensitively and whitespace-trimmed', () => {
    // Acceptance: typing "LANDSCAPE" must offer the existing term, not
    // a second one spelled in capitals.
    for (const spelling of ['landscape', 'LANDSCAPE', '  Landscape  ']) {
      const r = resolveTerm(vocab, spelling);
      expect(r.matched, spelling).toBe(true);
      expect(r.slug, spelling).toBe('landscape');
    }
  });

  it('matches a label the same way', () => {
    const r = resolveTerm(vocab, 'black and white');
    expect(r.matched).toBe(true);
    expect(r.slug).toBe('black-and-white');
  });

  it('matches a term that merely SLUGIFIES onto an existing one', () => {
    // The server checks this before minting, which is what stops
    // "black_and_white" becoming a second term meaning the same thing.
    const r = resolveTerm(vocab, 'Black_and_White');
    expect(r.matched).toBe(true);
    expect(r.slug).toBe('black-and-white');
  });

  it('matches a deprecated term, and says it is deprecated', () => {
    // Matching and OFFERING are different questions. The term
    // resolves; whether it may be chosen is the lifecycle rule's call,
    // and the caller needs the status to tell "that is not a term" from
    // "that term was retired".
    const r = resolveTerm(vocab, 'Sepia');
    expect(r.matched).toBe(true);
    expect(r.option?.status).toBe('deprecated');
  });

  it('does not match an archived term by name, but its slug is taken', () => {
    // An archived term is one an operator retired hard; typing its
    // label must not resurrect it, and minting a near-miss would leave
    // the catalogue with two terms meaning one thing.
    const r = resolveTerm(vocab, 'Daguerreotype');
    expect(r.matched).toBe(true);
    expect(r.option?.status).toBe('archived');
  });

  it('reports an unmatched term with the slug it would become', () => {
    const r = resolveTerm(vocab, 'Macro Detail');
    expect(r.matched).toBe(false);
    expect(r.slug).toBe('macro-detail');
  });

  it('reports a term with no addressable form as uncreatable', () => {
    const r = resolveTerm(vocab, '!!!');
    expect(r.matched).toBe(false);
    expect(r.slug).toBe('');
  });

  it('searches the whole tree, not just the top level', () => {
    const tree = normalizeOptions({
      values: [
        { value: 'europe', label: 'Europe', children: [{ value: 'london', label: 'London' }] },
      ],
    });
    const r = resolveTerm(tree, 'london');
    expect(r.matched).toBe(true);
    expect(r.slug).toBe('london');
  });

  it('resolves nothing against an empty vocabulary', () => {
    expect(resolveTerm([], 'anything')).toEqual({ matched: false, slug: 'anything' });
  });
});

describe('findOption', () => {
  const vocab = normalizeOptions({
    values: [{ value: 'europe', label: 'Europe', children: [{ value: 'london', label: 'London' }] }],
  });

  it('finds a term at any depth', () => {
    expect(findOption(vocab, 'london')?.label).toBe('London');
  });

  it('returns undefined for a term the vocabulary does not carry', () => {
    // Which is how a picker tells a term being CREATED from one that
    // already exists.
    expect(findOption(vocab, 'atlantis')).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Path-addressed editing (#779 / #825)
// ---------------------------------------------------------------------------

/**
 * `country` as migration 00024 ships it: 24 nations under 5 continents,
 * object-form entries, exactly two levels. Copied rather than
 * approximated because the hazard these tests exist for is a LEAF
 * COUNT, and a three-term stand-in cannot express it.
 */
const COUNTRY = {
  values: [
    {
      value: 'africa',
      label: 'Africa',
      children: [
        { value: 'eg', label: 'Egypt' },
        { value: 'ke', label: 'Kenya' },
        { value: 'ma', label: 'Morocco' },
        { value: 'ng', label: 'Nigeria' },
        { value: 'za', label: 'South Africa' },
      ],
    },
    {
      value: 'americas',
      label: 'Americas',
      children: [
        { value: 'ar', label: 'Argentina' },
        { value: 'br', label: 'Brazil' },
        { value: 'ca', label: 'Canada' },
        { value: 'mx', label: 'Mexico' },
        { value: 'us', label: 'United States' },
      ],
    },
    {
      value: 'asia',
      label: 'Asia',
      children: [
        { value: 'cn', label: 'China' },
        { value: 'in', label: 'India' },
        { value: 'jp', label: 'Japan' },
        { value: 'kr', label: 'South Korea' },
        { value: 'sg', label: 'Singapore' },
      ],
    },
    {
      value: 'europe',
      label: 'Europe',
      children: [
        { value: 'fr', label: 'France' },
        { value: 'de', label: 'Germany' },
        { value: 'it', label: 'Italy' },
        { value: 'nl', label: 'Netherlands' },
        { value: 'es', label: 'Spain' },
        { value: 'se', label: 'Sweden' },
        { value: 'gb', label: 'United Kingdom' },
      ],
    },
    {
      value: 'oceania',
      label: 'Oceania',
      children: [
        { value: 'au', label: 'Australia' },
        { value: 'nz', label: 'New Zealand' },
      ],
    },
  ],
};

/** Every slug in a SERIALISED document, at any depth, in order. */
function serialisedSlugs(opts: FieldOption[]): string[] {
  const walk = (entries: unknown[]): string[] =>
    entries.flatMap((e) => {
      if (typeof e === 'string') return [e];
      const o = e as { value: string; children?: unknown[] };
      return [o.value, ...(o.children ? walk(o.children) : [])];
    });
  return walk(serializeOptions(opts));
}

const LEAVES = [
  'eg', 'ke', 'ma', 'ng', 'za',
  'ar', 'br', 'ca', 'mx', 'us',
  'cn', 'in', 'jp', 'kr', 'sg',
  'fr', 'de', 'it', 'nl', 'es', 'se', 'gb',
  'au', 'nz',
];

describe('path addressing', () => {
  const vocab = normalizeOptions(COUNTRY);

  it('resolves a term by its index path at any depth', () => {
    expect(optionAtPath(vocab, [3])?.value).toBe('europe');
    expect(optionAtPath(vocab, [3, 6])?.value).toBe('gb');
    expect(optionAtPath(vocab, [3, 99])).toBeUndefined();
    // The empty path addresses the root LIST, not a term.
    expect(optionAtPath(vocab, [])).toBeUndefined();
  });

  it('hands back the sibling list a path sits in, root included', () => {
    expect(childrenAtPath(vocab, []).map((o) => o.value)).toEqual([
      'africa', 'americas', 'asia', 'europe', 'oceania',
    ]);
    expect(childrenAtPath(vocab, [4]).map((o) => o.value)).toEqual(['au', 'nz']);
    expect(childrenAtPath(vocab, [4, 0])).toEqual([]);
  });

  it('reports containment, which is the whole cycle guard', () => {
    expect(containsPath([3], [3, 6])).toBe(true);
    expect(containsPath([3], [3])).toBe(true); // a node contains itself
    expect(containsPath([3], [4])).toBe(false);
    expect(containsPath([3, 6], [3])).toBe(false); // a child does not contain its parent
    // The root contains everything — which is why "top level" is
    // always a legal destination and never filtered out.
    expect(containsPath([], [0, 1])).toBe(true);
  });

  it('carries an index path out of flattenOptions', () => {
    const flat = flattenOptions(vocab);
    expect(flat.find((o) => o.value === 'gb')?.indexPath).toEqual([3, 6]);
    expect(flat.find((o) => o.value === 'africa')?.indexPath).toEqual([0]);
  });

  it('collects every slug tree-wide, which is the uniqueness rule', () => {
    const slugs = allOptionSlugs(vocab);
    expect(slugs.size).toBe(29); // 5 continents + 24 nations
    expect(slugs.has('gb')).toBe(true);
    expect(slugs.has('europe')).toBe(true);
  });
});

describe('updateOptionAtPath', () => {
  // THE #825 PIN. A flat editor wired carelessly to a nested document
  // writes back the branch it edited and drops everything under it —
  // silently, because the branch it edited looks right afterwards.
  // Assert the LEAF COUNT, not the edit.
  it('a parent-only edit leaves all 24 leaves in place', () => {
    const vocab = normalizeOptions(COUNTRY);
    const next = updateOptionAtPath(vocab, [3], (o) => ({ ...o, label: 'Europe (EU)' }));

    expect(optionAtPath(next, [3])?.label).toBe('Europe (EU)');
    const slugs = serialisedSlugs(next);
    for (const leaf of LEAVES) {
      expect(slugs, `leaf ${leaf} was dropped by a parent-only edit`).toContain(leaf);
    }
    expect(slugs.filter((s) => LEAVES.includes(s))).toHaveLength(24);
  });

  it('relabels a leaf without touching its slug — the value keeps resolving', () => {
    const vocab = normalizeOptions(COUNTRY);
    const next = updateOptionAtPath(vocab, [3, 6], (o) => ({ ...o, label: 'Great Britain' }));
    expect(optionAtPath(next, [3, 6])).toEqual({
      value: 'gb',
      label: 'Great Britain',
      status: 'active',
    });
    expect(findOption(next, 'gb')?.label).toBe('Great Britain');
    expect(allOptionSlugs(next).has('gb')).toBe(true);
  });

  it('does not mutate the document it was given', () => {
    const vocab = normalizeOptions(COUNTRY);
    const before = JSON.stringify(serializeOptions(vocab));
    updateOptionAtPath(vocab, [3, 6], (o) => ({ ...o, status: 'archived' }));
    expect(JSON.stringify(serializeOptions(vocab))).toBe(before);
  });

  it('sets a lifecycle on a deep node, and it survives serialisation', () => {
    const vocab = normalizeOptions(COUNTRY);
    const next = updateOptionAtPath(vocab, [3, 6], (o) => ({
      ...o,
      status: 'deprecated' as const,
      replaced_by: 'fr',
    }));
    const round = normalizeOptions({ values: serializeOptions(next) });
    const gb = findOption(round, 'gb');
    expect(gb?.status).toBe('deprecated');
    expect(gb?.replaced_by).toBe('fr');
    // And a deprecated leaf stops being offered without vanishing.
    expect(selectableTreeOptions(round).map((o) => o.value)).not.toContain('gb');
    expect(selectableTreeOptions(round, ['gb']).map((o) => o.value)).toContain('gb');
  });
});

describe('insertOptionAtPath', () => {
  const vocab = normalizeOptions(COUNTRY);
  const node = { value: 'ie', label: 'Ireland', status: 'active' as const };

  it('appends a child under a branch', () => {
    const next = insertOptionAtPath(vocab, [3], Infinity, node);
    expect(childrenAtPath(next, [3]).map((o) => o.value)).toEqual([
      'fr', 'de', 'it', 'nl', 'es', 'se', 'gb', 'ie',
    ]);
  });

  it('inserts a sibling at a position rather than at the end', () => {
    const next = insertOptionAtPath(vocab, [3], 1, node);
    expect(childrenAtPath(next, [3]).map((o) => o.value)).toEqual([
      'fr', 'ie', 'de', 'it', 'nl', 'es', 'se', 'gb',
    ]);
  });

  it('inserts at the top level for the root path', () => {
    const next = insertOptionAtPath(vocab, [], Infinity, {
      value: 'antarctica',
      label: 'Antarctica',
      status: 'active',
    });
    expect(next.map((o) => o.value)).toEqual([
      'africa', 'americas', 'asia', 'europe', 'oceania', 'antarctica',
    ]);
  });

  it('gives a childless leaf its first child', () => {
    const next = insertOptionAtPath(vocab, [3, 6], Infinity, {
      value: 'sct',
      label: 'Scotland',
      status: 'active',
    });
    expect(childrenAtPath(next, [3, 6]).map((o) => o.value)).toEqual(['sct']);
    // Depth is not capped — the server does not cap it either, so a
    // client-only limit would make a legal catalogue uneditable.
    expect(flattenOptions(next).find((o) => o.value === 'sct')?.depth).toBe(2);
  });
});

describe('moveOptionWithinSiblings', () => {
  const vocab = normalizeOptions(COUNTRY);

  it('reorders siblings at depth without leaving the branch', () => {
    const next = moveOptionWithinSiblings(vocab, [3, 6], -1);
    expect(childrenAtPath(next, [3]).map((o) => o.value)).toEqual([
      'fr', 'de', 'it', 'nl', 'es', 'gb', 'se',
    ]);
    // Every other branch is untouched.
    expect(childrenAtPath(next, [0]).map((o) => o.value)).toEqual(['eg', 'ke', 'ma', 'ng', 'za']);
  });

  it('reorders the top level, which is what the flat editor always did', () => {
    const next = moveOptionWithinSiblings(vocab, [0], 1);
    expect(next.map((o) => o.value)).toEqual(['americas', 'africa', 'asia', 'europe', 'oceania']);
  });

  it('is a no-op past either end of the sibling list', () => {
    expect(moveOptionWithinSiblings(vocab, [0], -1)).toBe(vocab);
    expect(moveOptionWithinSiblings(vocab, [3, 6], 1)).toBe(vocab);
  });
});

describe('reparentOption', () => {
  it('moves a leaf to another branch, slug and all', () => {
    const vocab = normalizeOptions(COUNTRY);
    const next = reparentOption(vocab, [3, 6], [1]); // gb → Americas
    expect(childrenAtPath(next, [3]).map((o) => o.value)).toEqual([
      'fr', 'de', 'it', 'nl', 'es', 'se',
    ]);
    expect(childrenAtPath(next, [1]).map((o) => o.value)).toEqual([
      'ar', 'br', 'ca', 'mx', 'us', 'gb',
    ]);
    // The slug did not change, so nothing an asset stores changed.
    expect(findOption(next, 'gb')?.label).toBe('United Kingdom');
    expect(allOptionSlugs(next).size).toBe(29);
    // And the picker still offers it, at its new depth.
    expect(selectableTreeOptions(next).map((o) => o.value)).toContain('gb');
  });

  it('promotes a leaf to the top level', () => {
    const vocab = normalizeOptions(COUNTRY);
    const next = reparentOption(vocab, [3, 6], []);
    expect(next.map((o) => o.value)).toEqual([
      'africa', 'americas', 'asia', 'europe', 'oceania', 'gb',
    ]);
    expect(flattenOptions(next).find((o) => o.value === 'gb')?.depth).toBe(0);
  });

  it('carries a whole subtree, not just the node', () => {
    const vocab = normalizeOptions(COUNTRY);
    const next = reparentOption(vocab, [3], [4]); // Europe under Oceania
    expect(childrenAtPath(next, [3]).map((o) => o.value)).toEqual(['au', 'nz', 'europe']);
    expect(childrenAtPath(next, [3, 2]).map((o) => o.value)).toEqual([
      'fr', 'de', 'it', 'nl', 'es', 'se', 'gb',
    ]);
    expect(allOptionSlugs(next).size).toBe(29);
  });

  it('lands in the right slot when the destination sits AFTER the source', () => {
    // The index-adjustment case. Moving africa (index 0) under
    // oceania (index 4) — after the removal oceania is index 3, and an
    // unadjusted insert would splice the subtree under europe instead.
    const vocab = normalizeOptions(COUNTRY);
    const next = reparentOption(vocab, [0], [4]);
    expect(next.map((o) => o.value)).toEqual(['americas', 'asia', 'europe', 'oceania']);
    expect(childrenAtPath(next, [3]).map((o) => o.value)).toEqual(['au', 'nz', 'africa']);
    expect(childrenAtPath(next, [2]).map((o) => o.value)).toEqual([
      'fr', 'de', 'it', 'nl', 'es', 'se', 'gb',
    ]);
  });

  it('REFUSES a move into the node’s own subtree', () => {
    // The document would otherwise splice europe into itself: every
    // slug below it duplicated or orphaned, and NormalizeOptionsDoc
    // would reject the save with a duplicate that is hard to trace
    // back to the gesture that caused it.
    const vocab = normalizeOptions(COUNTRY);
    expect(reparentOption(vocab, [3], [3, 6])).toBe(vocab);
    expect(reparentOption(vocab, [3], [3])).toBe(vocab);
  });

  it('never duplicates or loses a slug, whichever way it is driven', () => {
    let vocab = normalizeOptions(COUNTRY);
    vocab = reparentOption(vocab, [3, 6], [0]); // gb → Africa
    vocab = reparentOption(vocab, [0, 5], []); // gb → top level
    vocab = reparentOption(vocab, [5], [2, 0]); // gb → under China
    const slugs = flattenOptions(vocab).map((o) => o.value);
    expect(new Set(slugs).size).toBe(slugs.length);
    expect(slugs).toHaveLength(29);
    expect(findOption(vocab, 'gb')?.label).toBe('United Kingdom');
  });
});

describe('moveDestinations', () => {
  const vocab = normalizeOptions(COUNTRY);

  it('excludes the node itself and everything under it', () => {
    const dests = moveDestinations(vocab, [3]).map((o) => o.value);
    expect(dests).not.toContain('europe');
    for (const child of ['fr', 'de', 'it', 'nl', 'es', 'se', 'gb']) {
      expect(dests, `${child} is inside the moving subtree`).not.toContain(child);
    }
    expect(dests).toContain('africa');
    expect(dests).toContain('us');
  });

  it('offers a leaf every other term in the vocabulary', () => {
    const dests = moveDestinations(vocab, [3, 6]);
    expect(dests.map((o) => o.value)).not.toContain('gb');
    expect(dests).toHaveLength(28); // 29 terms minus gb itself
    // Including its current parent — re-picking it moves the term to
    // the end of the branch, which is a reorder, not an error.
    expect(dests.map((o) => o.value)).toContain('europe');
  });
});

describe('removeOptionAtPath', () => {
  // Not reachable from the UI — options are never hard-deleted (ADR
  // 0012) — but it is the first half of every reparent, so its
  // behaviour is load-bearing.
  const vocab = normalizeOptions(COUNTRY);

  it('hands back the removed subtree intact', () => {
    const { options, removed } = removeOptionAtPath(vocab, [3]);
    expect(removed?.value).toBe('europe');
    expect(removed?.children).toHaveLength(7);
    expect(options.map((o) => o.value)).toEqual(['africa', 'americas', 'asia', 'oceania']);
  });

  it('drops the children key when the last child goes', () => {
    const { options } = removeOptionAtPath(removeOptionAtPath(vocab, [4, 0]).options, [4, 0]);
    expect(optionAtPath(options, [4])?.value).toBe('oceania');
    expect(optionAtPath(options, [4])?.children).toBeUndefined();
    // Which means it serialises back to the narrowest form.
    expect(serializeOptions([optionAtPath(options, [4])!])).toEqual([
      { value: 'oceania', label: 'Oceania' },
    ]);
  });
});
