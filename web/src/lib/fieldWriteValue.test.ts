// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The ONE type→member mapping (#1119, #1173).
//
// This module was extracted from FieldValuesSection's `setBody` so the
// batch editor writes the same member the single-value editor does. The
// point of the test is the EXHAUSTIVENESS: eleven types, five members,
// and no type that quietly falls through to an empty body.

import { describe, expect, it } from 'vitest';
import {
  fieldWriteBody,
  fieldWriteValuePresent,
  writeMemberFor,
  type FieldWriteType,
} from './fieldWriteValue';

const ALL: FieldWriteType[] = [
  'text', 'longtext', 'rich_text', 'number', 'boolean',
  'date', 'datetime', 'select', 'multi_select', 'tree', 'reference',
];

describe('writeMemberFor', () => {
  it('matches the mapping both write schemas describe', () => {
    expect(ALL.map(writeMemberFor)).toEqual([
      'value_text', 'value_text', 'value_text', 'value_num', 'value_num',
      'value_date', 'value_date', 'value_text', 'value_options', 'value_text',
      'value_ref',
    ]);
  });
});

describe('fieldWriteBody', () => {
  it('emits EXACTLY ONE member for every one of the eleven types', () => {
    for (const type of ALL) {
      const body = fieldWriteBody(type, {
        value_text: 'x',
        value_num: 1,
        value_date: '2026-01-01T00:00:00Z',
        value_options: ['a'],
        value_ref: '11111111-1111-4111-8111-111111111111',
      });
      expect(Object.keys(body), type).toEqual([writeMemberFor(type)]);
    }
  });

  it('never omits the member on an emptied control', () => {
    // The bug this shape exists to make impossible: a body with no
    // value member at all, which both validators refuse.
    for (const type of ALL) {
      const body = fieldWriteBody(type, {});
      expect(Object.keys(body), type).toHaveLength(1);
    }
  });

  it('says empty with "" on the five types that CAN', () => {
    for (const type of ['text', 'longtext', 'rich_text', 'select', 'tree'] as FieldWriteType[]) {
      expect(fieldWriteBody(type, {}), type).toEqual({ value_text: '' });
    }
  });

  it('does NOT invent a clear out of an empty multi_select', () => {
    // An empty option set is 400 `value_type_mismatch` at the server,
    // and it stays that. `remove` is the mode that removes.
    expect(fieldWriteBody('multi_select', {})).toEqual({ value_options: [] });
    expect(fieldWriteValuePresent('multi_select', {})).toBe(false);
    expect(fieldWriteValuePresent('multi_select', { value_options: [] })).toBe(false);
    expect(fieldWriteValuePresent('multi_select', { value_options: ['a'] })).toBe(true);
  });
});

describe('fieldWriteValuePresent', () => {
  it('the five types that cannot express emptiness need their member', () => {
    expect(fieldWriteValuePresent('number', {})).toBe(false);
    expect(fieldWriteValuePresent('boolean', {})).toBe(false);
    expect(fieldWriteValuePresent('date', {})).toBe(false);
    expect(fieldWriteValuePresent('datetime', {})).toBe(false);
    expect(fieldWriteValuePresent('reference', {})).toBe(false);
  });

  it('zero and false are VALUES, not emptiness', () => {
    expect(fieldWriteValuePresent('number', { value_num: 0 })).toBe(true);
    expect(fieldWriteValuePresent('boolean', { value_num: 0 })).toBe(true);
  });

  it('the text-shaped types are always sendable, because "" is a value', () => {
    for (const type of ['text', 'longtext', 'rich_text', 'select', 'tree'] as FieldWriteType[]) {
      expect(fieldWriteValuePresent(type, {}), type).toBe(true);
    }
  });
});
