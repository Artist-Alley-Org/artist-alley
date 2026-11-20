// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, expect, it } from 'vitest';

import { declaredEntries, formatBytes, originalUrl, verifiedHere } from './received';

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
