// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, expect, it } from 'vitest';

import {
  CursorTrail,
  HISTORY_PAGE_SIZE,
  bytesLabel,
  eventDetails,
  outcomeLabel,
  parseAssetId,
  rangeLabel,
  reasonLabel,
  shortDigest,
  statusLabel,
  variantLabel,
  type PreservationEvent,
} from './preservationHistory';

describe('parseAssetId', () => {
  it('accepts a UUID, trimmed and lower-cased', () => {
    expect(parseAssetId('  5F0C1E2A-7B3D-4C1E-9A2B-0123456789AB ')).toBe('5f0c1e2a-7b3d-4c1e-9a2b-0123456789ab');
  });
  it('refuses anything else', () => {
    for (const bad of ['', 'abc', '5f0c1e2a-7b3d-4c1e-9a2b', '../etc/passwd', '5f0c1e2a7b3d4c1e9a2b0123456789ab']) {
      expect(parseAssetId(bad)).toBeNull();
    }
  });
});

describe('labels', () => {
  const reasons: PreservationEvent['outcome_reason'][] = [
    'digest_match', 'no_recorded_digest', 'checksum_mismatch', 'size_mismatch', 'missing_object', 'read_error',
  ];
  it('gives every outcome a word, so colour is never the only signal', () => {
    expect(outcomeLabel('success')).toBe('Passed');
    expect(outcomeLabel('warning')).toBe('Warning');
    expect(outcomeLabel('failure')).toBe('Failed');
  });
  it('explains every reason in a sentence', () => {
    for (const r of reasons) {
      const s = reasonLabel(r);
      expect(s.length).toBeGreaterThan(10);
      expect(s.endsWith('.')).toBe(true);
    }
  });
  it('never calls a file without a recorded checksum verified', () => {
    expect(reasonLabel('no_recorded_digest')).toMatch(/could not be confirmed/);
    expect(statusLabel('unverifiable')).not.toMatch(/^verified/);
  });
  it('keeps the unverifiable status neutral about its cause', () => {
    // A row is unverifiable for more than one reason (no recorded
    // checksum, an interrupted write, a failed read); the status must
    // not assert any one of them. The reason carries the cause.
    expect(statusLabel('unverifiable')).toBe('not verified');
    expect(statusLabel('unverifiable')).not.toMatch(/checksum|digest/i);
  });
  it('names originals and derivatives', () => {
    expect(variantLabel({ is_original: true, variant_key: 'original' })).toBe('Original');
    expect(variantLabel({ is_original: false, variant_key: 'hls/1080p.mp4' })).toBe('Derivative: hls/1080p.mp4');
  });
  it('shortens digests and sizes for display', () => {
    const d = 'a'.repeat(60) + 'beef';
    expect(shortDigest(d)).toBe('aaaaaaaaaaaa…beef');
    expect(shortDigest(null)).toBe('none');
    expect(bytesLabel(null)).toBe('none');
    expect(bytesLabel(512)).toBe('512 B');
    expect(bytesLabel(1536)).toBe('1.5 KiB');
    expect(bytesLabel(50 * 1024 * 1024)).toBe('50 MiB');
  });
});

const D1 = 'd'.repeat(60) + 'd1d1';
function event(over: Partial<PreservationEvent>): PreservationEvent {
  return {
    id: '00000000-0000-4000-8000-000000000001',
    source: 'local',
    event_type: 'fixity-check',
    occurred_at: '2026-11-19T12:00:00Z',
    outcome: 'success',
    outcome_reason: 'digest_match',
    object_hash: 'a'.repeat(64),
    variant_key: 'preview',
    is_original: false,
    observed_generation: 2,
    fixity_status: 'verified',
    sweep_run_present: true,
    sweep_run_id: '11111111-2222-4333-8444-555555555555',
    agent: { name: 'artist-alley', version: '0.11.2' },
    ...over,
  };
}
function text(e: PreservationEvent): string {
  return eventDetails(e).map((d) => `${d.label}: ${d.value}`).join('\n');
}

describe('eventDetails', () => {
  it('shows a recorded checksum on an unverifiable row whose read failed', () => {
    // An interrupted write or a failed read leaves the row unverifiable
    // while its recorded checksum is still there; nothing may say it is
    // absent.
    const e = event({
      outcome: 'failure',
      outcome_reason: 'read_error',
      fixity_status: 'unverifiable',
      recorded_sha256: D1,
      observed_sha256: null,
      observed_size_bytes: null,
      algorithm: null,
    });
    const visible = text(e) + '\n' + reasonLabel(e.outcome_reason);
    expect(visible).toContain('Recorded: dddddddddddd…d1d1');
    expect(visible).toContain('Status after the check: not verified');
    expect(visible).toContain('Read: nothing read');
    expect(visible).not.toMatch(/no recorded checksum/i);
    expect(eventDetails(e).find((d) => d.label === 'Recorded')?.title).toBe(D1);
  });
  it('names an absent recorded checksum only when it is absent', () => {
    const e = event({
      outcome: 'warning',
      outcome_reason: 'no_recorded_digest',
      fixity_status: 'unverifiable',
      recorded_sha256: null,
      observed_sha256: D1,
      observed_size_bytes: 2048,
      algorithm: 'sha256',
    });
    expect(text(e)).toContain('Recorded: no recorded checksum');
    expect(text(e)).toContain('Read: sha256 dddddddddddd…d1d1 · 2.0 KiB');
    expect(reasonLabel(e.outcome_reason)).toMatch(/no recorded checksum to compare them with, so they could not be confirmed/);
  });
  it('marks a deleted sweep run and the software agent', () => {
    expect(text(event({ sweep_run_present: false }))).toContain('Sweep run: 11111111 (run record deleted)');
    expect(text(event({ sweep_run_id: null }))).toContain('Sweep run: none');
    expect(text(event({ agent: { name: 'artist-alley' } }))).toContain('Recorded by: artist-alley');
  });
});

describe('CursorTrail', () => {
  it('walks older pages forward and back to the newest', () => {
    const t = new CursorTrail();
    expect(t.current).toBeNull();
    expect(t.page).toBe(1);
    expect(t.canGoNewer).toBe(false);
    expect(t.previous).toBeNull();
    t.older('c2');
    expect(t.previous).toBeNull(); // "Newer" from page 2 asks for the newest page
    t.older('c3');
    expect(t.previous).toBe('c2');
    expect(t.current).toBe('c3');
    expect(t.page).toBe(3);
    t.newer();
    expect(t.current).toBe('c2');
    t.newer();
    t.newer(); // already at the newest page: stays there
    expect(t.current).toBeNull();
    expect(t.page).toBe(1);
    t.older('x');
    t.reset();
    expect(t.page).toBe(1);
    expect(t.current).toBeNull();
  });
});

describe('rangeLabel', () => {
  it('counts from the page position', () => {
    expect(rangeLabel(1, HISTORY_PAGE_SIZE, 60)).toBe(`Events 1–${HISTORY_PAGE_SIZE} of 60`);
    expect(rangeLabel(3, 10, 60)).toBe(`Events ${2 * HISTORY_PAGE_SIZE + 1}–${2 * HISTORY_PAGE_SIZE + 10} of 60`);
    expect(rangeLabel(1, 0, 0)).toBe('No events (0 in total)');
  });
});
