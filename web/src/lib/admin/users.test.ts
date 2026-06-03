import { describe, expect, it } from 'vitest';
import {
  statusBadgeClass,
  relativeAgo,
  buildListQuery,
  type AdminUserStatus,
} from './users';

describe('statusBadgeClass', () => {
  it.each<[AdminUserStatus, string]>([
    ['active', 'success'],
    ['pending', 'warning'],
    ['disabled', 'danger'],
  ])('maps %s to the %s palette', (status, palette) => {
    const cls = statusBadgeClass(status);
    expect(cls).toContain(palette);
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
