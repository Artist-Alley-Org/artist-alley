// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, expect, it } from 'vitest';
import { isValidSlug, slugify, isValidUUID } from './teams';

describe('isValidSlug', () => {
  it.each([
    ['design-vfx', true],
    ['a', true],
    ['team1', true],
    ['1team', true],
    ['lowercase-only-with-digits-1234567890', true],
  ])('accepts %s', (input, want) => {
    expect(isValidSlug(input)).toBe(want);
  });

  it.each([
    ['', 'empty'],
    ['Design-VFX', 'uppercase'],
    ['design vfx', 'space'],
    ['design_vfx', 'underscore'],
    ['-leading-dash', 'leading dash'],
    ['design!', 'special char'],
    ['a'.repeat(81), 'too long (81 chars)'],
  ])('rejects %s (%s)', (input) => {
    expect(isValidSlug(input)).toBe(false);
  });

  it('accepts exact 80-char limit', () => {
    expect(isValidSlug('a' + '1'.repeat(79))).toBe(true);
  });
});

describe('slugify', () => {
  it.each([
    ['Design / VFX', 'design-vfx'],
    ['Hello World', 'hello-world'],
    ['  trim  spaces  ', 'trim-spaces'],
    ['UPPER_lower', 'upper-lower'],
    ['multi---dash', 'multi-dash'],
    ['emoji 🎨 art', 'emoji-art'],
    ['', ''],
  ])('slugifies %j to %j', (input, want) => {
    expect(slugify(input)).toBe(want);
  });

  it('caps at 80 chars', () => {
    const long = 'a'.repeat(200);
    expect(slugify(long).length).toBe(80);
  });

  it('produces output that satisfies isValidSlug', () => {
    for (const name of ['Design / VFX', 'Marketing Team 2', 'A & B', 'team1']) {
      const slug = slugify(name);
      if (slug !== '') {
        expect(isValidSlug(slug)).toBe(true);
      }
    }
  });
});

describe('isValidUUID', () => {
  it.each([
    '00000000-0000-0000-0000-000000000000',
    'a1b2c3d4-e5f6-1234-9abc-def012345678',
    'A1B2C3D4-E5F6-1234-9ABC-DEF012345678', // case-insensitive
  ])('accepts %s', (input) => {
    expect(isValidUUID(input)).toBe(true);
  });

  it.each([
    '',
    'not-a-uuid',
    '12345678-1234-1234-1234-12345678901',  // 35 chars
    '12345678-1234-1234-1234-1234567890123', // 37 chars
    '12345678_1234_1234_1234_123456789012', // underscores instead of dashes
  ])('rejects %s', (input) => {
    expect(isValidUUID(input)).toBe(false);
  });
});
