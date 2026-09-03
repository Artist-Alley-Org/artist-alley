// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { describe, it, expect, afterAll, beforeEach } from 'vitest';
import { formatFieldValue, isFieldValueEmpty, type AssetFieldValue } from './fieldDisplay';
import emptinessCases from './fieldEmptiness.cases.json';

/** The one list both language planes are tested against; see the file. */
const sharedCases = emptinessCases.rich_text as Array<{
  input: string;
  stored: string;
  empty: boolean;
}>;

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

  it('multi_select joins resolved labels AND exposes them as parts', () => {
    const v = value({
      type: 'multi_select',
      value_options: ['a', 'b'],
      resolved_options: {
        a: { label: 'Alpha', status: 'active' },
        b: { label: 'Beta', status: 'active' },
      },
    });
    // `text` is unchanged — the flat rendering stays authoritative, so
    // a caller that only wants a string (and the "has this field got a
    // value" test, which is `text !== ''`) keeps working. `parts` is
    // additive, for a surface that can render a set as a set.
    expect(formatFieldValue(v, t)).toEqual({ text: 'Alpha, Beta', parts: ['Alpha', 'Beta'] });
  });

  it('multi_select parts carry the deprecation marker per term', () => {
    const v = value({
      type: 'multi_select',
      value_options: ['a', 'b'],
      resolved_options: {
        a: { label: 'Alpha', status: 'active' },
        b: { label: 'Beta', status: 'deprecated' },
      },
    });
    expect(formatFieldValue(v, t).parts).toEqual(['Alpha', 'Beta (deprecated)']);
  });

  it('an empty multi_select is still empty', () => {
    // parts=[] with text='' — the caller's `if (val.text)` guard is
    // what keeps an unset field out of the panel, and adding parts
    // must not smuggle an empty chip row past it.
    const v = value({ type: 'multi_select', value_options: [] });
    expect(formatFieldValue(v, t)).toEqual({ text: '', parts: [] });
  });

  it('no other type emits parts', () => {
    // The chip renderer branches on `parts`, so any type that grew one
    // by accident would silently start rendering as chips.
    const types = [
      'text', 'longtext', 'rich_text', 'number', 'boolean',
      'date', 'datetime', 'select', 'tree', 'reference',
    ] as const;
    for (const type of types) {
      const v = value({
        type,
        value_text: 'x',
        value_num: 1,
        value_date: '2026-01-01T00:00:00Z',
        value_ref: 'r-1',
        resolved_options: { x: { label: 'X', status: 'active' } },
        resolved_reference: { id: 'r-1', title: 'Ref' },
      });
      expect(formatFieldValue(v, t).parts, type).toBeUndefined();
    }
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

// ── #816's boundary: rich_text renders as markup ────────────────────
//
// This block used to pin the opposite — that rich_text came back as
// escaped source — and it held that line until the sanitisation
// boundary was decided. It is decided: the SERVER sanitises, on write
// and on read (app/internal/richtext), and the API's guarantee is that
// rich_text HTML on the wire is already policy-clean. So the formatter
// hands the caller an `html` descriptor and the caller {@html}s it.
//
// There is deliberately no client-side scrub, and these cases are
// written so nobody adds one by mistake: the formatter is asserted to
// pass the server's HTML through UNCHANGED. A test that expected
// `<script>` to disappear here would be quietly asserting a second
// sanitiser into existence, and two sanitisers is one policy too many.

describe('formatFieldValue — rich_text renders as markup', () => {
  it('returns the server HTML on `html`, with a plain reading on `text`', () => {
    const v = value({
      type: 'rich_text',
      value_text: '<p>Cleared for <strong>internal</strong> use.</p>',
    });
    expect(formatFieldValue(v, t)).toEqual({
      html: '<p>Cleared for <strong>internal</strong> use.</p>',
      text: 'Cleared for internal use.',
    });
  });

  it('never emits an href — a rich_text link lives inside the markup', () => {
    const v = value({
      type: 'rich_text',
      value_text: '<p><a href="https://example.test/" rel="noopener noreferrer">terms</a></p>',
    });
    expect(formatFieldValue(v, t).href).toBeUndefined();
  });

  it('passes the server payload through byte-for-byte', () => {
    // The single most important case in this file. If this ever starts
    // failing because someone made the expectation "safer", read the
    // block comment above: sanitising here would be the second policy.
    const src = '<p>a &amp; b</p><ul><li>one</li><li>two</li></ul>';
    expect(formatFieldValue(value({ type: 'rich_text', value_text: src }), t).html).toBe(src);
  });

  it('trims, and treats an empty or markup-only value as unset', () => {
    expect(formatFieldValue(value({ type: 'rich_text', value_text: '  <p>x</p>  ' }), t)).toEqual({
      html: '<p>x</p>',
      text: 'x',
    });
    for (const empty of ['', '   ', '<p></p>', '<br>']) {
      expect(formatFieldValue(value({ type: 'rich_text', value_text: empty }), t)).toEqual({
        text: '',
      });
    }
    expect(formatFieldValue(value({ type: 'rich_text', value_text: null }), t)).toEqual({
      text: '',
    });
  });

  it('decodes entities for the plain reading without double-decoding', () => {
    // `text` feeds the field count and the "is this set" test, and is
    // always interpolated. It is legibility, never a safety boundary.
    const v = value({ type: 'rich_text', value_text: '<p>a &amp;lt; b</p>' });
    expect(formatFieldValue(v, t).text).toBe('a &lt; b');
  });
});

// The types that must NOT have gained a markup descriptor. rich_text
// is the one field type whose value is markup; an `html` appearing on
// any other type is a second {@html} surface nobody designed.
describe('formatFieldValue — no other type emits html', () => {
  it('leaves every non-rich_text value without an html descriptor', () => {
    const cases: Partial<AssetFieldValue>[] = [
      { type: 'text', value_text: '<b>x</b>' },
      { type: 'longtext', value_text: '<b>x</b>' },
      { type: 'select', value_text: 'slug' },
      { type: 'tree', value_text: 'slug' },
      { type: 'multi_select', value_options: ['a', 'b'] },
      { type: 'number', value_num: 1 },
      { type: 'boolean', value_num: 1 },
      { type: 'date', value_date: '2026-01-01T00:00:00Z' },
      { type: 'datetime', value_date: '2026-01-01T00:00:00Z' },
      { type: 'reference', value_ref: 'a-1' },
    ];
    for (const c of cases) {
      expect(formatFieldValue(value(c), t).html, `type ${c.type}`).toBeUndefined();
    }
  });

  it('keeps `text` and `longtext` as literal source, tags and all', () => {
    const src = 'a < b && <div>kept</div>';
    for (const type of ['text', 'longtext'] as const) {
      expect(formatFieldValue(value({ type, value_text: src }), t)).toEqual({ text: src });
    }
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

// ---------------------------------------------------------------------------
// isFieldValueEmpty — the write-side twin of the "is this set" test (#1389)
// ---------------------------------------------------------------------------

describe('isFieldValueEmpty', () => {
  // The rich_text cases ARE the point. Every one of these survives the
  // server's sanitiser unchanged (measured against the shipped policy),
  // so a rule written as a trim accepts a value that renders as nothing
  // — and an editor built on that rule sends a typed Set where the
  // person meant "remove this".
  // The CASES ARE NOT WRITTEN HERE. `rich_text` emptiness is ONE rule
  // with two implementations: this one, and app/internal/richtext's
  // IsEmpty, which is what actually refuses the write. They must agree
  // or a required field renders blank while the server considers it
  // filled.
  //
  // Both suites read the same file, so a change that moves one plane
  // and not the other fails on the other plane's test. `stored` in it
  // is MEASURED: what the server's sanitiser actually produced for
  // `input`. Note the &nbsp; case, whose entity reaches storage as a
  // literal U+00A0, which is why neither predicate may be written
  // against the entity string.
  it('agrees with the server plane over the SHARED case list', () => {
    expect(sharedCases.length, 'the shared case list is empty').toBeGreaterThan(0);
    const empties = sharedCases.filter((c) => c.empty).length;
    expect(empties, 'the list must cover BOTH verdicts').toBeGreaterThan(0);
    expect(empties, 'the list must cover BOTH verdicts').toBeLessThan(sharedCases.length);
    for (const c of sharedCases) {
      expect(
        isFieldValueEmpty('rich_text', { value_text: c.stored }),
        `${JSON.stringify(c.input)} stored as ${JSON.stringify(c.stored)}`,
      ).toBe(c.empty);
    }
  });

  it('treats FALSE as a real boolean value and only null as empty', () => {
    expect(isFieldValueEmpty('boolean', { value_num: 0 })).toBe(false);
    expect(isFieldValueEmpty('boolean', { value_num: 1 })).toBe(false);
    expect(isFieldValueEmpty('boolean', { value_num: null })).toBe(true);
    // Same for a number: zero is a value.
    expect(isFieldValueEmpty('number', { value_num: 0 })).toBe(false);
    expect(isFieldValueEmpty('number', { value_num: null })).toBe(true);
  });

  it('reads the remaining types per their storage column', () => {
    for (const type of ['text', 'longtext', 'select', 'tree'] as const) {
      expect(isFieldValueEmpty(type, { value_text: null })).toBe(true);
      expect(isFieldValueEmpty(type, { value_text: '   ' })).toBe(true);
      expect(isFieldValueEmpty(type, { value_text: 'x' })).toBe(false);
    }
    expect(isFieldValueEmpty('multi_select', { value_options: null })).toBe(true);
    expect(isFieldValueEmpty('multi_select', { value_options: [] })).toBe(true);
    expect(isFieldValueEmpty('multi_select', { value_options: ['a'] })).toBe(false);
    expect(isFieldValueEmpty('date', { value_date: null })).toBe(true);
    expect(isFieldValueEmpty('datetime', { value_date: '2026-01-01T00:00:00Z' })).toBe(false);
    expect(isFieldValueEmpty('reference', { value_ref: null })).toBe(true);
    expect(isFieldValueEmpty('reference', { value_ref: 'abc' })).toBe(false);
  });

  // The read side and the write side must agree, or a required field
  // renders blank while the editor believes it holds something.
  it('agrees with formatFieldValue about which rich_text values are set', () => {
    // The write-side predicate and the read-side "does this field have
    // a value" test (`text !== ''`) are the same question, and a field
    // that displays as filled while the editor treats it as removable
    // is the divergence this pair exists to prevent.
    for (const c of sharedCases) {
      const showsBlank =
        formatFieldValue(value({ type: 'rich_text', value_text: c.stored }), t).text === '';
      expect(isFieldValueEmpty('rich_text', { value_text: c.stored }), c.stored).toBe(showsBlank);
    }
  });
});
