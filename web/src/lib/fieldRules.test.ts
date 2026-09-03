// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The client-side reading of `regexp_filter` (#1173).
//
// Everything here is about a CONVENIENCE check. The server matches
// `\A(?:pattern)\z` with RE2 and answers 422 `pattern_mismatch`, and
// the cases that matter most below are the ones where this check
// declines to run — because that is when the server's refusal is the
// only thing standing between a person and a value the operator's
// pattern forbids.

import { describe, it, expect } from 'vitest';
import { compileFieldPattern, fieldPatternApplies, fieldPatternViolated } from './fieldRules';

describe('fieldPatternApplies', () => {
  it('is text and longtext only, matching regexpFilterApplies server-side', () => {
    const p = '[A-Z]{3}';
    expect(fieldPatternApplies({ type: 'text', regexp_filter: p })).toBe(true);
    expect(fieldPatternApplies({ type: 'longtext', regexp_filter: p })).toBe(true);
    // rich_text shares the storage column and is deliberately excluded:
    // what is stored is sanitised markup, so a pattern would match tags.
    for (const type of [
      'rich_text', 'number', 'boolean', 'date', 'datetime',
      'select', 'multi_select', 'tree', 'reference',
    ]) {
      expect(fieldPatternApplies({ type, regexp_filter: p }), type).toBe(false);
    }
  });

  it('is false with no pattern configured, which is the ONLY "no constraint"', () => {
    expect(fieldPatternApplies({ type: 'text' })).toBe(false);
    expect(fieldPatternApplies({ type: 'text', regexp_filter: null })).toBe(false);
  });
});

describe('fieldPatternViolated', () => {
  const def = { type: 'text', regexp_filter: '[A-Z]{3}_[0-9]{4}' };

  it('matches the WHOLE value, not a substring', () => {
    expect(fieldPatternViolated(def, 'AAA_0010')).toBe(false);
    expect(fieldPatternViolated(def, 'prefix AAA_0010 suffix')).toBe(true);
  });

  it('wraps an alternation so it means the whole value', () => {
    // Without the non-capturing group, `^a|b$` would mean "starts with
    // a, or ends with b" — the trap the server's \A(?:…)\z exists for.
    const alt = { type: 'text', regexp_filter: 'a|b' };
    expect(fieldPatternViolated(alt, 'a')).toBe(false);
    expect(fieldPatternViolated(alt, 'b')).toBe(false);
    expect(fieldPatternViolated(alt, 'axx')).toBe(true);
    expect(fieldPatternViolated(alt, 'xxb')).toBe(true);
  });

  it('never reports an EMPTY value', () => {
    // Removing a value is a Clear, which carries nothing to match, and
    // `required` — not the pattern — decides whether that is allowed.
    expect(fieldPatternViolated(def, '')).toBe(false);
  });

  it('declines to run rather than reporting a pattern it cannot compile', () => {
    // RE2 accepts syntax JavaScript rejects and vice versa. Showing a
    // person a regexp error for a pattern their operator wrote and the
    // server accepted would report our incompatibility as their
    // mistake, so the check stands down and the 422 is what they see.
    const bad = { type: 'text', regexp_filter: '([' };
    expect(compileFieldPattern(bad)).toBeNull();
    expect(fieldPatternViolated(bad, 'anything')).toBe(false);
  });

  it('ignores a pattern on a type that does not honour one', () => {
    expect(fieldPatternViolated({ type: 'rich_text', regexp_filter: '^[A-Z]' }, '<p>x</p>')).toBe(
      false,
    );
  });
});
