// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, it, expect, afterAll, beforeEach } from 'vitest';
import { formatFieldValue, type AssetFieldValue } from './fieldDisplay';

// The formatter takes t() as a parameter precisely so a test does not
// have to boot the lang store (which pulls in the API client + auth).
// Echoing the key back is enough for every assertion here; the two
// cases that interpolate are asserted on their interpolated output.
const t = (key: string, vars?: Record<string, string | number>): string => {
  if (key === 'common.yes') return 'Yes';
  if (key === 'common.no') return 'No';
  if (key === 'common.option_deprecated') return `${vars?.label} (deprecated)`;
  return key;
};

function value(over: Partial<AssetFieldValue>): AssetFieldValue {
  return {
    field_id: 'f-1',
    field_code: 'test_field',
    type: 'text',
    set_by: 'manual',
    set_at: '2026-01-01T00:00:00Z',
    ...over,
  } as AssetFieldValue;
}

// ── Timezone harness (#815) ─────────────────────────────────────────
//
// Node re-reads process.env.TZ on assignment, so setting it between
// cases genuinely moves the runtime clock. That is load-bearing and
// NOT assumed: assertTimezoneIsLive below proves the switch took
// effect before any case relies on it. Without that control a
// "two-timezone" test that silently failed to change zone would pass
// twice in the same zone and prove nothing — which is exactly the
// shape of the bug it exists to catch.
const ORIGINAL_TZ = process.env.TZ;

const WEST = 'America/Los_Angeles'; // UTC−8 in January
const EAST = 'Asia/Tokyo'; // UTC+9, no DST

function setTZ(tz: string) {
  process.env.TZ = tz;
}

afterAll(() => {
  if (ORIGINAL_TZ === undefined) delete process.env.TZ;
  else process.env.TZ = ORIGINAL_TZ;
});

/**
 * Control assertion: the midnight-UTC instant must land on a
 * DIFFERENT local calendar day in the two zones. If this fails, the TZ
 * switch is a no-op on this platform and every "both sides of the
 * meridian" claim below is vacuous.
 */
function localDayOfNewYearUTC(): number {
  return new Date('2026-01-01T00:00:00Z').getDate();
}

describe('timezone harness', () => {
  it('actually changes the runtime zone (control for the date tests)', () => {
    setTZ(WEST);
    const west = localDayOfNewYearUTC();
    setTZ(EAST);
    const east = localDayOfNewYearUTC();

    // West of UTC the instant is still Dec 31; east of UTC it is Jan 1.
    expect(west).toBe(31);
    expect(east).toBe(1);
    expect(west).not.toBe(east);
  });
});

describe('formatFieldValue — date (#815)', () => {
  // The exact seeded value from the issue.
  const licenseExpires = value({
    field_code: 'license_expires',
    type: 'date',
    value_date: '2026-01-01T00:00:00Z',
  });

  it('renders the calendar date at UTC−8', () => {
    setTZ(WEST);
    // Pre-fix this printed "12/31/2025" — the previous day, which
    // reads as an already-expired license.
    expect(formatFieldValue(licenseExpires, t).text).toBe('2026-01-01');
  });

  it('renders the same calendar date at UTC+9', () => {
    setTZ(EAST);
    expect(formatFieldValue(licenseExpires, t).text).toBe('2026-01-01');
  });

  it('is identical on both sides of the meridian', () => {
    setTZ(WEST);
    const west = formatFieldValue(licenseExpires, t).text;
    setTZ(EAST);
    const east = formatFieldValue(licenseExpires, t).text;
    expect(west).toBe(east);
    expect(west).toBe('2026-01-01');
  });

  it('does not shift a late-evening UTC instant either', () => {
    // The mirror of the original bug: an instant late in the UTC day
    // rolls FORWARD a day east of UTC. The calendar date is still the
    // stored one.
    const v = value({ type: 'date', value_date: '2026-03-15T23:30:00Z' });
    setTZ(WEST);
    expect(formatFieldValue(v, t).text).toBe('2026-03-15');
    setTZ(EAST);
    expect(formatFieldValue(v, t).text).toBe('2026-03-15');
  });

  it('renders nothing for an absent or unparseable value', () => {
    expect(formatFieldValue(value({ type: 'date', value_date: null }), t).text).toBe('');
    expect(formatFieldValue(value({ type: 'date', value_date: 'not-a-date' }), t).text).toBe('');
  });

  it('never returns an href', () => {
    setTZ(WEST);
    expect(formatFieldValue(licenseExpires, t).href).toBeUndefined();
  });
});

describe('formatFieldValue — datetime still localises (#815)', () => {
  const ingestedAt = value({
    field_code: 'ingested_at',
    type: 'datetime',
    value_date: '2026-01-01T00:00:00Z',
  });

  it('converts to the viewer clock — the two zones disagree', () => {
    setTZ(WEST);
    const west = formatFieldValue(ingestedAt, t).text;
    setTZ(EAST);
    const east = formatFieldValue(ingestedAt, t).text;

    // This is the whole point of the date/datetime split: for a
    // datetime the instant IS the information, so it MUST differ.
    expect(west).not.toBe(east);
    expect(west).not.toBe('');
    expect(east).not.toBe('');
  });

  it('matches toLocaleString — unchanged behaviour', () => {
    setTZ(EAST);
    expect(formatFieldValue(ingestedAt, t).text).toBe(
      new Date('2026-01-01T00:00:00Z').toLocaleString(),
    );
  });
});

// ── References (#817) ───────────────────────────────────────────────

describe('formatFieldValue — reference (#817)', () => {
  const TARGET = '11111111-2222-3333-4444-555555555555';

  it('renders the resolved title as a link to the asset', () => {
    const v = value({
      field_code: 'derived_from',
      type: 'reference',
      value_ref: TARGET,
      resolved_reference: { id: TARGET, title: 'Concept Sheet 04' },
    });
    expect(formatFieldValue(v, t)).toEqual({
      text: 'Concept Sheet 04',
      href: `/assets/${TARGET}`,
    });
  });

  it('degrades to the bare id, unlinked, when the target did not resolve', () => {
    // Soft-deleted target, or a dangling ref: the server omits
    // resolved_reference and the panel stays intact. No href — a link
    // to a row that did not resolve is a promise of a 404.
    const v = value({
      field_code: 'derived_from',
      type: 'reference',
      value_ref: TARGET,
      resolved_reference: null,
    });
    expect(formatFieldValue(v, t)).toEqual({ text: TARGET });
    expect(formatFieldValue(v, t).href).toBeUndefined();
  });

  it('degrades the same way when the key is absent entirely', () => {
    const v = value({ type: 'reference', value_ref: TARGET });
    expect(formatFieldValue(v, t)).toEqual({ text: TARGET });
  });

  it('falls back to the id as the label for an untitled target', () => {
    // assets.title DEFAULT '' — an untitled asset is ordinary, and an
    // empty link label would not be clickable. The link stays.
    const v = value({
      type: 'reference',
      value_ref: TARGET,
      resolved_reference: { id: TARGET, title: '   ' },
    });
    expect(formatFieldValue(v, t)).toEqual({ text: TARGET, href: `/assets/${TARGET}` });
  });

  it('renders nothing for an unset reference', () => {
    expect(formatFieldValue(value({ type: 'reference' }), t)).toEqual({ text: '' });
  });
});

// ── Regression guard: the seam #817 changed is shared (#775/#776) ────

describe('formatFieldValue — resolved_options behaviour is unchanged', () => {
  it('select renders the resolved label', () => {
    const v = value({
      type: 'select',
      value_text: 'srgb',
      resolved_options: { srgb: { label: 'sRGB', status: 'active' } },
    });
    expect(formatFieldValue(v, t)).toEqual({ text: 'sRGB' });
  });

  it('select falls back to the slug when it does not resolve', () => {
    expect(formatFieldValue(value({ type: 'select', value_text: 'srgb' }), t)).toEqual({
      text: 'srgb',
    });
  });

  it('select marks a deprecated term', () => {
    const v = value({
      type: 'select',
      value_text: 'srgb',
      resolved_options: { srgb: { label: 'sRGB', status: 'deprecated' } },
    });
    expect(formatFieldValue(v, t).text).toBe('sRGB (deprecated)');
  });

  it('multi_select joins resolved labels', () => {
    const v = value({
      type: 'multi_select',
      value_options: ['a', 'b'],
      resolved_options: {
        a: { label: 'Alpha', status: 'active' },
        b: { label: 'Beta', status: 'active' },
      },
    });
    expect(formatFieldValue(v, t)).toEqual({ text: 'Alpha, Beta' });
  });

  it('tree renders the full ancestor path', () => {
    const v = value({
      type: 'tree',
      value_text: 'london',
      resolved_options: {
        london: {
          label: 'London',
          status: 'active',
          path: ['Europe', 'United Kingdom', 'London'],
        },
      },
    });
    expect(formatFieldValue(v, t)).toEqual({ text: 'Europe / United Kingdom / London' });
  });

  it('tree renders a top-level term as its bare label', () => {
    const v = value({
      type: 'tree',
      value_text: 'europe',
      resolved_options: { europe: { label: 'Europe', status: 'active', path: ['Europe'] } },
    });
    expect(formatFieldValue(v, t)).toEqual({ text: 'Europe' });
  });

  it('none of the vocabulary types ever emit an href', () => {
    for (const type of ['select', 'multi_select', 'tree'] as const) {
      expect(formatFieldValue(value({ type, value_text: 'x', value_options: ['x'] }), t).href)
        .toBeUndefined();
    }
  });
});

// ── #816's boundary: this PR must not move it ───────────────────────

describe('formatFieldValue — rich_text stays escaped text', () => {
  it('returns the source as plain text with no href', () => {
    // The contract change to { text, href? } deliberately carries no
    // component/markup descriptor. Rendering rich_text as markup is
    // #816's sprint and needs a sanitiser boundary designed for it.
    const src = '# Heading\n\n<b>bold</b> & <script>alert(1)</script>';
    const v = value({ type: 'rich_text', value_text: src });
    expect(formatFieldValue(v, t)).toEqual({ text: src });
  });
});

describe('formatFieldValue — scalar types', () => {
  beforeEach(() => setTZ(WEST));

  it('trims text', () => {
    expect(formatFieldValue(value({ type: 'text', value_text: '  hi  ' }), t)).toEqual({
      text: 'hi',
    });
  });

  it('renders numbers, including zero', () => {
    expect(formatFieldValue(value({ type: 'number', value_num: 0 }), t)).toEqual({ text: '0' });
    expect(formatFieldValue(value({ type: 'number', value_num: 4.5 }), t)).toEqual({ text: '4.5' });
    expect(formatFieldValue(value({ type: 'number', value_num: null }), t)).toEqual({ text: '' });
  });

  it('renders booleans from value_num, and false is an answer', () => {
    expect(formatFieldValue(value({ type: 'boolean', value_num: 1 }), t)).toEqual({ text: 'Yes' });
    expect(formatFieldValue(value({ type: 'boolean', value_num: 0 }), t)).toEqual({ text: 'No' });
    expect(formatFieldValue(value({ type: 'boolean', value_num: null }), t)).toEqual({ text: '' });
  });
});
