// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * `edit_tab` bucketing (#1173, #1119, ADR 0099 §9).
 *
 * `edit_tab` shipped in sprint 18b with ZERO consumers: an operator could
 * assign a field to a tab and nothing anywhere changed. These are the
 * tests for the rule that reads it.
 *
 * Two of the cases here are CLASS B, marked individually, because they
 * describe today's behaviour (no strip, every control reachable) and pass
 * before and after. The rest are CLASS A: they need bucketing to exist.
 */

import { describe, it, expect } from 'vitest';
import { bucketFields, tabStripVisible, resolveTabSelection, groupFields } from './fieldTabs';

type F = { code: string; edit_tab?: string | null; display_order?: number };

const f = (code: string, tab?: string | null, order = 100): F => ({
  code,
  edit_tab: tab ?? null,
  display_order: order,
});

const codesOf = (fields: F[]) => fields.map((x) => x.code);

describe('the bucket floors, one per amount of chrome', () => {
  it('no composition-eligible fields -> 0 buckets, no phantom default tab', () => {
    // CLASS B in spirit and CLASS A in mechanism: today there are no
    // buckets at all, so what this pins is that the NEW rule does not
    // invent a default tab out of an empty list.
    const buckets = bucketFields<F>([]);
    expect(buckets).toEqual([]);
    expect(tabStripVisible(buckets)).toBe(false);
  });

  it('all unassigned -> 1 default bucket, NO strip, every field reachable', () => {
    const defs = [f('a'), f('b'), f('c')];
    const buckets = bucketFields(defs);
    expect(buckets).toHaveLength(1);
    expect(buckets[0].id).toBe('');
    expect(buckets[0].name).toBeNull();
    expect(codesOf(buckets[0].fields)).toEqual(['a', 'b', 'c']);
    expect(tabStripVisible(buckets)).toBe(false);
  });

  it('one named only -> 1 bucket, NO strip', () => {
    const buckets = bucketFields([f('a', 'Print'), f('b', 'Print')]);
    expect(buckets).toHaveLength(1);
    expect(buckets[0].name).toBe('Print');
    expect(tabStripVisible(buckets)).toBe(false);
  });

  it('one named PLUS unassigned -> 2 buckets, STRIP, default FIRST', () => {
    const buckets = bucketFields([f('a', 'Print'), f('b')]);
    expect(buckets).toHaveLength(2);
    expect(buckets[0].name).toBeNull();
    expect(buckets[1].name).toBe('Print');
    expect(tabStripVisible(buckets)).toBe(true);
  });

  it('two named -> 2 buckets, STRIP, no phantom default', () => {
    const buckets = bucketFields([f('a', 'Print', 10), f('b', 'Rights', 20)]);
    expect(buckets).toHaveLength(2);
    expect(buckets.map((b) => b.name)).toEqual(['Print', 'Rights']);
    expect(tabStripVisible(buckets)).toBe(true);
  });

  it('two named PLUS unassigned -> 3 buckets, STRIP', () => {
    const buckets = bucketFields([f('a', 'Print', 10), f('b', 'Rights', 20), f('c')]);
    expect(buckets).toHaveLength(3);
    expect(buckets.map((b) => b.name)).toEqual([null, 'Print', 'Rights']);
    expect(tabStripVisible(buckets)).toBe(true);
  });
});

describe('NO UNASSIGNED FIELD MAY DISAPPEAR', () => {
  it('every input field lands in exactly one bucket', () => {
    const defs = [f('a', 'Print'), f('b'), f('c', 'Rights'), f('d'), f('e', 'Print')];
    const buckets = bucketFields(defs);
    const landed = buckets.flatMap((b) => codesOf(b.fields)).sort();
    expect(landed).toEqual(['a', 'b', 'c', 'd', 'e']);
    // ...and in exactly one, so nothing is duplicated across tabs either.
    expect(new Set(landed).size).toBe(landed.length);
  });
});

describe('ORDER: default first, then named by MINIMUM MEMBER display_order, then name', () => {
  it('orders named tabs by their strongest member, not alphabetically', () => {
    // "Zebra" holds the field with the lowest display_order, so it comes
    // FIRST despite sorting last by name. Ordering by the operator's own
    // display_order is what makes the strip agree with the rest of the
    // form.
    const buckets = bucketFields([
      f('a', 'Alpha', 50),
      f('b', 'Zebra', 10),
      f('c', 'Zebra', 90),
    ]);
    expect(buckets.map((b) => b.name)).toEqual(['Zebra', 'Alpha']);
  });

  it('falls back to the tab NAME when the minimum orders tie', () => {
    const buckets = bucketFields([f('a', 'Beta', 10), f('b', 'Alpha', 10)]);
    expect(buckets.map((b) => b.name)).toEqual(['Alpha', 'Beta']);
  });

  it('is deterministic across repeated calls, so a reload does not reshuffle', () => {
    const defs = [f('a', 'Beta', 10), f('b', 'Alpha', 10), f('c', 'Gamma', 10), f('d')];
    const once = bucketFields(defs).map((b) => b.id);
    const twice = bucketFields(defs).map((b) => b.id);
    expect(once).toEqual(twice);
    expect(once).toEqual(['', 'Alpha', 'Beta', 'Gamma']);
  });

  it('keeps member order within a bucket, which is the caller’s display_order', () => {
    const buckets = bucketFields([f('a', 'Print', 10), f('b', 'Print', 20), f('c', 'Print', 30)]);
    expect(codesOf(buckets[0].fields)).toEqual(['a', 'b', 'c']);
  });
});

describe('a blank edit_tab is treated as unassigned', () => {
  // The server refuses one on write and the CHECK refuses it in storage,
  // so this is belt and braces for a definition that arrived some other
  // way. What it buys is that a stray space can never mint a tab nobody
  // can navigate to.
  it.each([undefined, null, '', '   '])('%p lands in the default bucket', (tab) => {
    const buckets = bucketFields([{ code: 'a', edit_tab: tab as string | null | undefined }]);
    expect(buckets).toHaveLength(1);
    expect(buckets[0].id).toBe('');
    expect(buckets[0].name).toBeNull();
  });
});

describe('resolveTabSelection', () => {
  const buckets = [{ id: '' }, { id: 'Print' }, { id: 'Rights' }];

  it('defaults to the FIRST bucket when nothing is selected', () => {
    expect(resolveTabSelection(null, buckets)).toBe('');
  });

  it('keeps a selection that still names a bucket', () => {
    expect(resolveTabSelection('Rights', buckets)).toBe('Rights');
  });

  it('moves to the first bucket when the selected tab STOPS EXISTING', () => {
    expect(resolveTabSelection('Gone', buckets)).toBe('');
  });

  it('answers null when there are no buckets at all', () => {
    expect(resolveTabSelection('Print', [])).toBeNull();
  });

  it('POLICY B: a tab that merely EMPTIES OUT keeps the selection', () => {
    // The bucket still exists, because buckets come from DEFINITIONS and
    // not from visible controls. This is the assertion a naive "reset the
    // selection when the tab has nothing in it" implementation fails, and
    // it is what stops the strip moving under a person while they type.
    const stillThere = [{ id: '' }, { id: 'Print' }];
    expect(resolveTabSelection('Print', stillThere)).toBe('Print');
  });
});

describe('tabStripVisible', () => {
  it('is false for zero and one bucket, true from two up', () => {
    expect(tabStripVisible([])).toBe(false);
    expect(tabStripVisible([{ id: '' }])).toBe(false);
    expect(tabStripVisible([{ id: '' }, { id: 'Print' }])).toBe(true);
    expect(tabStripVisible([{ id: '' }, { id: 'Print' }, { id: 'Rights' }])).toBe(true);
  });
});

describe('groupFields — the display_group layer INSIDE a tab', () => {
  // ONE implementation, called by FieldValuesSection AND by /create. The
  // create page rendered a flat list before this existed, so
  // `display_group` structured one surface and merely ordered the other
  // from the same definitions.
  type G = { code: string; display_group?: string };

  it('splits fields into their groups, preserving first-appearance order', () => {
    const out = groupFields<G>([
      { code: 'a', display_group: 'core' },
      { code: 'b', display_group: 'rights' },
      { code: 'c', display_group: 'core' },
    ]);
    expect(out.map((g) => g.name)).toEqual(['core', 'rights']);
    expect(out[0].fields.map((f) => f.code)).toEqual(['a', 'c']);
    expect(out[1].fields.map((f) => f.code)).toEqual(['b']);
  });

  it('preserves the caller’s order within a group, which is display_order', () => {
    const out = groupFields<G>([
      { code: 'first', display_group: 'core' },
      { code: 'second', display_group: 'core' },
      { code: 'third', display_group: 'core' },
    ]);
    expect(out).toHaveLength(1);
    expect(out[0].fields.map((f) => f.code)).toEqual(['first', 'second', 'third']);
  });

  it.each([undefined, ''])('%p falls back to `general`, matching the column default', (g) => {
    const out = groupFields<G>([{ code: 'a', display_group: g as string | undefined }]);
    expect(out).toHaveLength(1);
    expect(out[0].name).toBe('general');
  });

  it('mixes an unset group with named ones without losing anything', () => {
    const out = groupFields<G>([
      { code: 'a' },
      { code: 'b', display_group: 'rights' },
      { code: 'c' },
    ]);
    expect(out.map((g) => g.name)).toEqual(['general', 'rights']);
    expect(out.flatMap((g) => g.fields.map((f) => f.code)).sort()).toEqual(['a', 'b', 'c']);
  });

  it('is empty for no fields, so a tab with nothing visible draws no fieldset', () => {
    expect(groupFields<G>([])).toEqual([]);
  });
});

describe('the full nesting: bucket first, then group', () => {
  // The hierarchy is asset type (on /create) -> edit-tab bucket ->
  // display_group -> display_order. This pins the two middle layers
  // composing in that order and no other: a group NEVER spans two tabs,
  // because the tab is chosen first.
  type N = { code: string; edit_tab?: string | null; display_order?: number; display_group?: string };

  const defs: N[] = [
    { code: 'loose_core', display_order: 10, display_group: 'core' },
    { code: 'print_core', edit_tab: 'Print', display_order: 20, display_group: 'core' },
    { code: 'print_rights', edit_tab: 'Print', display_order: 30, display_group: 'rights' },
    { code: 'print_core2', edit_tab: 'Print', display_order: 40, display_group: 'core' },
  ];

  it('the Print bucket holds TWO distinct display_group fieldsets', () => {
    const buckets = bucketFields(defs);
    expect(buckets.map((b) => b.id)).toEqual(['', 'Print']);
    const print = buckets.find((b) => b.id === 'Print')!;
    const groups = groupFields(print.fields);
    expect(groups.map((g) => g.name)).toEqual(['core', 'rights']);
    expect(groups[0].fields.map((f) => f.code)).toEqual(['print_core', 'print_core2']);
    expect(groups[1].fields.map((f) => f.code)).toEqual(['print_rights']);
  });

  it('a group never spans two tabs: `core` exists in BOTH buckets, separately', () => {
    const buckets = bucketFields(defs);
    const dflt = groupFields(buckets.find((b) => b.id === '')!.fields);
    const print = groupFields(buckets.find((b) => b.id === 'Print')!.fields);
    expect(dflt.map((g) => g.name)).toEqual(['core']);
    expect(dflt[0].fields.map((f) => f.code)).toEqual(['loose_core']);
    // Same group NAME, different bucket, and the members do not mix.
    expect(print.find((g) => g.name === 'core')!.fields.map((f) => f.code)).toEqual([
      'print_core',
      'print_core2',
    ]);
  });
});
