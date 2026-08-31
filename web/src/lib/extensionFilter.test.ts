// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 18d — extension normalization.

import { describe, it, expect } from 'vitest';
import { normalizeExtension, addExtension, normalizeExtensions } from './extensionFilter';

describe('normalizeExtension', () => {
  it('trims, strips ONE leading dot and lowercases', () => {
    expect(normalizeExtension('PNG')).toBe('png');
    expect(normalizeExtension('.PNG')).toBe('png');
    expect(normalizeExtension('  .Png  ')).toBe('png');
    expect(normalizeExtension('tar.gz')).toBe('tar.gz');
    expect(normalizeExtension('.tar.GZ')).toBe('tar.gz');
  });

  it('drops empty and dot-only input', () => {
    expect(normalizeExtension('')).toBeNull();
    expect(normalizeExtension('   ')).toBeNull();
    expect(normalizeExtension('.')).toBeNull();
    expect(normalizeExtension('..')).toBeNull();
    expect(normalizeExtension('...')).toBeNull();
  });
});

describe('addExtension — PNG and .PNG collapse to ONE term', () => {
  it('deduplicates across spellings', () => {
    let sel: string[] = [];
    sel = addExtension(sel, 'png');
    sel = addExtension(sel, 'PNG');
    sel = addExtension(sel, '.PNG');
    sel = addExtension(sel, ' .png ');
    expect(sel).toEqual(['png']);
  });

  it('keeps distinct extensions and their order', () => {
    let sel: string[] = [];
    sel = addExtension(sel, '.PNG');
    sel = addExtension(sel, 'jpg');
    expect(sel).toEqual(['png', 'jpg']);
  });

  it('adds nothing for input that normalizes away', () => {
    expect(addExtension(['png'], '.')).toEqual(['png']);
    expect(addExtension(['png'], '  ')).toEqual(['png']);
  });
});

describe('normalizeExtensions', () => {
  it('collapses a list, first occurrence winning', () => {
    expect(normalizeExtensions(['.PNG', 'png', '', 'JPG', '.', 'jpg'])).toEqual(['png', 'jpg']);
  });
});
