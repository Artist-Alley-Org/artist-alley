// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, expect, it } from 'vitest';

import { declaredEntries, formatBytes, languageEntries, originalUrl, verifiedHere } from './received';

describe('received asset helpers (#1574)', () => {
  it('formats byte sizes', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(2048)).toBe('2.0 KiB');
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MiB');
    expect(formatBytes(-1)).toBe('');
  });

  it('lists declared claims in a stable order, objects as JSON', () => {
    expect(declaredEntries({ rights: 'r', description: { title: 't' }, mature: false })).toEqual([
      { key: 'description', value: '{"title":"t"}' },
      { key: 'mature', value: 'false' },
      { key: 'rights', value: 'r' },
    ]);
    expect(declaredEntries(undefined)).toEqual([]);
  });

  it('treats only a verified original as established here', () => {
    expect(verifiedHere({ role: 'original', state: 'verified' })).toBe(true);
    expect(verifiedHere({ role: 'original', state: 'pending' })).toBe(false);
    expect(verifiedHere({ role: 'preservation', state: 'not_transferred' })).toBe(false);
    expect(verifiedHere({ role: 'access', state: 'verified' })).toBe(false);
  });

  it('builds the original URL under the API base, escaping the id', () => {
    expect(originalUrl('a/b')).toBe('/api/v1/federation/received-assets/a%2Fb/original');
  });
});

describe('languageEntries (#1577)', () => {
  it('lists every tag in a stable order with every text in order, without changing the map', () => {
    const map = { none: ['Studio A', 'Second'], fr: ['Crédit'], en: ['Credit'] };
    const before = JSON.stringify(map);
    expect(languageEntries(map)).toEqual([
      ['en', ['Credit']],
      ['fr', ['Crédit']],
      ['none', ['Studio A', 'Second']],
    ]);
    expect(JSON.stringify(map)).toBe(before);
    expect(Object.keys(map)).toEqual(['none', 'fr', 'en']);
  });

  it('has no entries for a missing map', () => {
    expect(languageEntries(undefined)).toEqual([]);
    expect(languageEntries(null)).toEqual([]);
  });
});
