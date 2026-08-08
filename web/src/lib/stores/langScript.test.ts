// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #967 — the pre-paint language script in app.html, pinned against the
// registry it duplicates.
//
// That script runs before any module loads, so it CANNOT import
// SUPPORTED_LOCALES; it carries its own copy of the locale list and its
// own copy of resolveLocale's matching rule. Two copies of one fact is
// exactly the shape that rots, and it rots silently: add a catalogue,
// forget this file, and the new locale simply never survives a cold load
// — everything still works after hydration, so nothing looks broken.
//
// So this parses the real app.html and asserts:
//
//   1. the script's locale list is exactly SUPPORTED_LOCALES,
//   2. its fallback is DEFAULT_LOCALE,
//   3. its resolution — re-executed here, not re-implemented — agrees
//      with resolveLocale() on exact matches, subtag matches, unknown
//      tags and the empty string.
//
// (3) is the part that makes this worth having: a list check alone would
// pass a script whose matching logic had drifted.

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

import { DEFAULT_LOCALE, SUPPORTED_LOCALES, resolveLocale } from '$lib/i18n/locales';

const APP_HTML = resolve(__dirname, '../../app.html');
const html = readFileSync(APP_HTML, 'utf8');

/** The `var supported = [...]` literal out of the pre-paint script. */
function scriptLocales(): string[] {
  const m = html.match(/var supported = \[([^\]]*)\]/);
  if (!m) throw new Error('pre-paint language script not found in app.html');
  return m[1]
    .split(',')
    .map((s) => s.trim().replace(/^['"]|['"]$/g, ''))
    .filter(Boolean);
}

function scriptFallback(): string {
  const m = html.match(/var fallback = '([^']*)'/);
  if (!m) throw new Error("pre-paint language script has no `var fallback`");
  return m[1];
}

/**
 * Re-run the script's OWN resolution, rather than a restatement of it.
 *
 * The body is lifted from app.html by regex and evaluated against a
 * stubbed cookie + navigator, so a change to the script's matching rule
 * is picked up here even though the rule is never written down twice.
 */
function runScript(cookieValue: string | null, navigatorLanguage: string): string {
  const supported = scriptLocales();
  const fallback = scriptFallback();

  // The matching block, verbatim from the file.
  const m = html.match(/var resolved = fallback;([\s\S]*?)document\.documentElement\.setAttribute\('lang', resolved\);/);
  if (!m) throw new Error('pre-paint language script matching block not found');
  const body = m[1];

  const pref = cookieValue || navigatorLanguage || fallback;
  // eslint-disable-next-line no-new-func
  const fn = new Function(
    'supported',
    'fallback',
    'pref',
    `var resolved = fallback;${body}return resolved;`,
  ) as (s: string[], f: string, p: string) => string;
  return fn(supported, fallback, pref);
}

describe('app.html pre-paint language script', () => {
  it('exists at all', () => {
    expect(html).toContain("aa_lang=");
    expect(html).toContain("document.documentElement.setAttribute('lang', resolved)");
  });

  it('carries exactly the locales the app has catalogues for', () => {
    expect(scriptLocales()).toEqual(SUPPORTED_LOCALES.map((l) => l.code));
  });

  it('falls back to DEFAULT_LOCALE', () => {
    expect(scriptFallback()).toBe(DEFAULT_LOCALE);
  });

  it('resolves the same way resolveLocale() does', () => {
    // Cases chosen to cover each branch: exact hit, subtag hit,
    // unsupported language, and "no preference at all".
    const cases: Array<[cookie: string | null, nav: string]> = [
      ['fr', 'en-US'],
      ['es', 'en-US'],
      ['fr-CA', 'en-US'],
      ['de', 'en-US'],
      ['', 'fr-FR'],
      ['', 'en-GB'],
      ['', 'ja'],
    ];
    for (const [cookie, nav] of cases) {
      const pref = cookie || nav;
      expect({ cookie, nav, got: runScript(cookie, nav) })
        .toEqual({ cookie, nav, got: resolveLocale(pref) });
    }
  });

  it('leaves a hardcoded lang attribute on <html> for the script to overwrite', () => {
    // The static shell has to say something before the script runs, and
    // DEFAULT_LOCALE is the honest thing for it to say.
    expect(html).toMatch(new RegExp(`<html lang="${DEFAULT_LOCALE}"`));
  });
});
