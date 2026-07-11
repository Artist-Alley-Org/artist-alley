// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, expect, it } from 'vitest';
import {
  statusBadgeClass,
  relativeAgo,
  buildListQuery,
  validTargetsFrom,
  transitionVerb,
  type AdminUserStatus,
} from './users';

describe('statusBadgeClass', () => {
  it.each<[AdminUserStatus, string]>([
    ['active', 'success'],
    ['pending', 'warning'],
    ['disabled', 'danger'],
    ['archived', 'muted'],
  ])('maps %s to the %s palette', (status, palette) => {
    const cls = statusBadgeClass(status);
    expect(cls).toContain(palette);
  });
});

describe('validTargetsFrom — transition matrix', () => {
  it.each<[AdminUserStatus, AdminUserStatus[]]>([
    ['pending', ['active']],
    ['active', ['disabled', 'archived']],
    ['disabled', ['active', 'archived']],
    ['archived', ['active']],
  ])('%s → %j', (from, expected) => {
    expect(validTargetsFrom(from)).toEqual(expected);
  });

  it('rejects every pair not in the matrix', () => {
    // Mirrors the Go-side ValidateTransition coverage. Pending
    // can't go straight to disabled or archived; active can't
    // step back to pending; archived must restore via active.
    const rejected: [AdminUserStatus, AdminUserStatus][] = [
      ['pending', 'disabled'],
      ['pending', 'archived'],
      ['active', 'pending'],
      ['disabled', 'pending'],
      ['archived', 'pending'],
      ['archived', 'disabled'],
    ];
    for (const [from, to] of rejected) {
      expect(validTargetsFrom(from), `${from} → ${to} should NOT be in valid targets`).not.toContain(to);
    }
  });
});

describe('transitionVerb', () => {
  it.each<[AdminUserStatus, AdminUserStatus, string]>([
    ['pending', 'active', 'approve'],
    ['disabled', 'active', 'restore'],
    ['archived', 'active', 'restore'],
    ['active', 'disabled', 'disable'],
    ['active', 'archived', 'archive'],
    ['disabled', 'archived', 'archive'],
  ])('%s → %s = %s', (from, to, verb) => {
    expect(transitionVerb(from, to)).toBe(verb);
  });
});

describe('relativeAgo', () => {
  // Anchor "now" so the test is deterministic regardless of when CI runs.
  const NOW = new Date('2026-06-03T12:00:00Z');

  it('returns empty for null/undefined/invalid', () => {
    expect(relativeAgo(null, NOW)).toBe('');
    expect(relativeAgo(undefined, NOW)).toBe('');
    expect(relativeAgo('not-a-date', NOW)).toBe('');
  });

  it('snaps very-recent times to "just now"', () => {
    expect(relativeAgo('2026-06-03T11:59:30Z', NOW)).toBe('just now');
    expect(relativeAgo('2026-06-03T11:59:01Z', NOW)).toBe('just now');
  });

  it('formats minute / hour / day buckets', () => {
    expect(relativeAgo('2026-06-03T11:55:00Z', NOW)).toBe('5m ago');
    expect(relativeAgo('2026-06-03T08:00:00Z', NOW)).toBe('4h ago');
    expect(relativeAgo('2026-06-01T12:00:00Z', NOW)).toBe('2d ago');
  });

  it('rolls into month / year buckets past 30 / 365 days', () => {
    expect(relativeAgo('2026-04-15T12:00:00Z', NOW)).toBe('1mo ago');
    expect(relativeAgo('2024-06-03T12:00:00Z', NOW)).toBe('2y ago');
  });

  it('clamps future timestamps to "just now" rather than negative', () => {
    expect(relativeAgo('2026-06-03T13:00:00Z', NOW)).toBe('just now');
  });
});

describe('buildListQuery', () => {
  it('omits empty / null / undefined fields', () => {
    expect(buildListQuery({})).toEqual({});
    expect(buildListQuery({ q: '', status: '', cursor: null, limit: 0 })).toEqual({});
    expect(buildListQuery({ q: '   ' })).toEqual({});
  });

  it('preserves only the populated fields', () => {
    expect(buildListQuery({ q: 'alice' })).toEqual({ q: 'alice' });
    expect(buildListQuery({ status: 'pending' })).toEqual({ status: 'pending' });
    expect(buildListQuery({ cursor: 'abc' })).toEqual({ cursor: 'abc' });
    expect(buildListQuery({ limit: 25 })).toEqual({ limit: 25 });
  });

  it('trims surrounding whitespace from q', () => {
    expect(buildListQuery({ q: '  bob  ' })).toEqual({ q: 'bob' });
  });

  it('combines every populated field', () => {
    expect(buildListQuery({
      q: 'alice',
      status: 'active',
      cursor: 'tok',
      limit: 100,
    })).toEqual({
      q: 'alice',
      status: 'active',
      cursor: 'tok',
      limit: 100,
    });
  });
});
