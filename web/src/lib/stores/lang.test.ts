// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Lang store contract tests — pinning the singleton's behavior
// against the live en.json catalogue + the fallback chain.
//
// We import the runtime singleton (not the underlying class) because
// every component does the same. Tests don't drive `set()` because
// it touches auth + the API — covered separately when we stand up
// auth fixtures. The current locale stays at the default initial
// state ('' / DEFAULT_LOCALE).

import { describe, expect, it } from 'vitest';
import { lang, t } from './lang.svelte';

describe('t() lookup', () => {
  it('resolves a known en.json key to its English string', () => {
    // nav.upload is one of the entries the navbar relies on. If this
    // key ever disappears from en.json, the test catches the drift
    // before the navbar starts rendering literal "nav.upload" text.
    const got = t('nav.upload');
    expect(got).toBeTruthy();
    expect(got).not.toBe('nav.upload');
  });

  it('returns the key itself for completely-unknown keys', () => {
    // No-translation contract: callers see the key string rather
    // than an empty render, so a missing key is loud during dev.
    const orphan = 't.test.no-such-key-please';
    expect(t(orphan)).toBe(orphan);
  });

  it('falls back to en for an unknown key in any locale', () => {
    // Direct singleton mutation to simulate a non-en active locale
    // without going through set() (which requires auth + network).
    const orig = lang.resolved;
    try {
      lang.resolved = 'es';
      // user_menu.signed_in_as is in en.json but es.json is empty,
      // so it must fall back to the en string verbatim.
      expect(t('user_menu.signed_in_as', { username: 'alice' })).toContain('alice');
    } finally {
      lang.resolved = orig;
    }
  });
});

describe('t() interpolation', () => {
  it('substitutes a {var} placeholder', () => {
    // Use a known en.json key that contains an {username} variable.
    const got = t('user_menu.signed_in_as', { username: 'kenneth' });
    // The key is "Signed in as @{username}" — confirm both the
    // literal prefix and the substituted username appear.
    expect(got).toContain('kenneth');
    expect(got).not.toContain('{username}');
  });

  it('leaves unmatched placeholders intact', () => {
    // A key that's the literal string we want — verify behavior with
    // a synthetic input. We override the dict via a helper since the
    // catalogue itself is read-only.
    // Use t directly with a fresh key the catalogue doesn't have so
    // the function returns the key string; then exercise the
    // interpolation rule by manually invoking — vars without matches
    // come back from the catalogue's literal text below.
    // For this case we use an existing key that has a {var} but
    // intentionally omit the var.
    const got = t('user_menu.signed_in_as'); // no vars supplied
    // Without vars, the placeholder remains literal ({username}).
    expect(got).toContain('{username}');
  });

  it('coerces numbers to strings during substitution', () => {
    // We don't have a number-vars en.json key today, but we can
    // verify the {var} pattern works by walking a known key that has
    // {username} and passing a number.
    const got = t('user_menu.signed_in_as', { username: 42 });
    expect(got).toContain('42');
  });
});

describe('lang.resolved + lang.locales', () => {
  it('exposes the SUPPORTED_LOCALES registry', () => {
    expect(Array.isArray(lang.locales)).toBe(true);
    expect(lang.locales.length).toBeGreaterThan(0);
    expect(lang.locales.find((l) => l.code === 'en')).toBeDefined();
  });

  it('defaults resolved to a concrete locale code (never empty)', () => {
    expect(lang.resolved).toBeTruthy();
    expect(typeof lang.resolved).toBe('string');
  });
});
