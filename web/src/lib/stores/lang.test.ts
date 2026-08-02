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

import { afterEach, describe, expect, it } from 'vitest';
import { lang, shippedStrings, t } from './lang.svelte';

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

// Operator string overrides (#794, ADR 0081 §1).
//
// These PIN THE FALLBACK RULE, which is the part of the feature most
// likely to be "simplified" into being wrong. Overrides are set on the
// singleton directly — populating them normally means a network fetch,
// and the merge under test lives in t(), not in the fetch.
describe('t() with operator overrides', () => {
  const origLocale = lang.resolved;
  const origOverrides = lang.overrides;

  afterEach(() => {
    lang.resolved = origLocale;
    lang.overrides = origOverrides;
  });

  it('prefers an override for the active locale over the shipped string', () => {
    const shipped = t('nav.upload');
    lang.overrides = { en: { 'nav.upload': 'Send us a file' } };
    expect(t('nav.upload')).toBe('Send us a file');
    expect(t('nav.upload')).not.toBe(shipped);
  });

  it('falls back to the shipped string when the override is removed', () => {
    const shipped = t('nav.upload');
    lang.overrides = { en: { 'nav.upload': 'Send us a file' } };
    lang.overrides = {};
    expect(t('nav.upload')).toBe(shipped);
  });

  it('interpolates {vars} inside an override', () => {
    lang.overrides = { en: { 'user_menu.signed_in_as': 'Hello, {username}!' } };
    expect(t('user_menu.signed_in_as', { username: 'alice' })).toBe('Hello, alice!');
  });

  it('scopes an override to its own locale', () => {
    lang.overrides = { es: { 'nav.upload': 'Subir archivo' } };
    // Active locale is en — the es override must not reach it.
    lang.resolved = 'en';
    expect(t('nav.upload')).not.toBe('Subir archivo');
    lang.resolved = 'es';
    expect(t('nav.upload')).toBe('Subir archivo');
  });

  // The chosen fallback rule, stated as two tests because it has two
  // halves and only one of them is obvious.
  it('lets an en override back a locale that does not translate the key', () => {
    // A key en has and es does not, so es was ALREADY rendering the
    // English string. Changing what that English string says must
    // change what es renders. Picked dynamically because es.json grows
    // as #289 progresses and a hardcoded key would silently stop
    // testing this rung the day it got translated.
    const es = shippedStrings('es');
    const untranslated = Object.keys(shippedStrings('en')).find((k) => es[k] === undefined);
    expect(untranslated).toBeTruthy();

    lang.resolved = 'es';
    lang.overrides = { en: { [untranslated as string]: 'BACKED BY EN' } };
    expect(t(untranslated as string)).toBe('BACKED BY EN');
  });

  it('does NOT let an en override outrank a real translation', () => {
    // A key es DOES translate. An English override must not silently
    // un-translate the Spanish UI — the override replaces the string
    // it sits beside, not the one below it.
    const esKey = Object.keys(shippedStrings('es'))[0];
    expect(esKey).toBeTruthy();
    const esShipped = shippedStrings('es')[esKey];

    lang.resolved = 'es';
    lang.overrides = { en: { [esKey]: 'ENGLISH OVERRIDE' } };
    expect(t(esKey)).toBe(esShipped);
  });
});

describe('shippedStrings()', () => {
  it('returns the flattened catalogue the admin page lists', () => {
    const en = shippedStrings('en');
    expect(en['nav.upload']).toBeTruthy();
    expect(Object.keys(en).length).toBeGreaterThan(1000);
  });

  it('returns an empty map for an unknown locale rather than throwing', () => {
    expect(shippedStrings('zz')).toEqual({});
  });
});
