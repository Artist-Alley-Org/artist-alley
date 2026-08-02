// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Language / i18n store — singleton, runes-backed. Mirrors the
// shape of theme.svelte.ts so the patterns rhyme.
//
//   pref     — user choice. '' means "follow system / browser".
//   resolved — the concrete locale code we'll render in (always
//              non-empty). Computed from pref → cookie → user
//              profile → navigator.language → DEFAULT_LOCALE,
//              filtered through resolveLocale().
//   dict     — the flat dotted-key dictionary for `resolved`,
//              built from the nested JSON catalogue on locale change.
//   overrides — operator replacements fetched from GET /site-text
//              (#794, ADR 0081 §1), same language → key → string
//              shape as the bundled catalogues. Consulted BEFORE
//              the shipped dictionary for the active locale.
//   t(key, vars?) — flat lookup with `{var}` interpolation;
//              falls back to the en catalogue and ultimately to the
//              key string itself when nothing matches.
//
// Persistence:
//   - Cookie  (aa_lang) — survives across sessions for anon users.
//   - PATCH user_profiles.language — once signed in, the cookie is
//                                    mirrored to the profile so it
//                                    follows the user across browsers.
//
// The catalogues themselves are ES module JSON imports — bundled at
// build time. Adding a locale = add a JSON file + an entry in
// locales.ts + a stub on the server registry.

import { api } from '$api/client';
import { auth } from '$stores/auth.svelte';
import { DEFAULT_LOCALE, SUPPORTED_LOCALES, resolveLocale } from '$lib/i18n/locales';

import enDict from '$lib/i18n/en.json';
import esDict from '$lib/i18n/es.json';
import frDict from '$lib/i18n/fr.json';

const COOKIE_NAME = 'aa_lang';
const COOKIE_MAX_AGE = 60 * 60 * 24 * 365; // 1 year

// Operator overrides are cached to localStorage for the same reason
// the appearance store caches its font picks: the boot fetch lands
// AFTER the first paint, so without a cache an install that renamed
// "Collections" to "Libraries" would flash the shipped word on every
// page load. The cache is a render hint, never the source of truth —
// refresh() overwrites it wholesale, including with an empty map when
// the last override is reverted.
const OVERRIDE_STORAGE_KEY = 'aa_site_text';

/** language → dotted key → replacement string. */
export type SiteTextOverrides = Record<string, Record<string, string>>;

function readOverrideCache(): SiteTextOverrides {
  if (typeof localStorage === 'undefined') return {};
  try {
    const raw = localStorage.getItem(OVERRIDE_STORAGE_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw) as unknown;
    return parsed && typeof parsed === 'object' ? (parsed as SiteTextOverrides) : {};
  } catch {
    return {};
  }
}

function writeOverrideCache(v: SiteTextOverrides): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(OVERRIDE_STORAGE_KEY, JSON.stringify(v));
  } catch {
    // localStorage may be disabled / quota'd — ignore.
  }
}

const catalogues: Record<string, Record<string, unknown>> = {
  en: enDict as Record<string, unknown>,
  es: esDict as Record<string, unknown>,
  fr: frDict as Record<string, unknown>,
};

// Flatten a nested object to dotted-key string map. Numbers/booleans
// stringify; nested arrays are joined for now (we don't have any
// today). Computed once per catalogue.
function flatten(src: Record<string, unknown>, prefix = '', out: Record<string, string> = {}): Record<string, string> {
  for (const [k, v] of Object.entries(src)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (v != null && typeof v === 'object' && !Array.isArray(v)) {
      flatten(v as Record<string, unknown>, key, out);
    } else {
      out[key] = String(v);
    }
  }
  return out;
}

const FLAT: Record<string, Record<string, string>> = {};
for (const [code, src] of Object.entries(catalogues)) {
  FLAT[code] = flatten(src);
}

// `{var}` interpolation. `vars` values render with String().
function interpolate(s: string, vars?: Record<string, string | number>): string {
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, (m, name) => {
    const v = vars[name];
    return v == null ? m : String(v);
  });
}

class LangState {
  /** User pref. '' = follow system. */
  pref = $state<string>('');
  /** Concrete locale code currently active. */
  resolved = $state<string>(DEFAULT_LOCALE);

  /**
   * Operator overrides, language → key → string (#794).
   *
   * Reactive, and read inside t(), so flipping an override in the
   * admin page re-renders every `t()` call site in the app without any
   * of them changing — the merge lives here, not at the call sites.
   */
  overrides = $state<SiteTextOverrides>({});

  /** Convenience for templates that need to render against the list. */
  get locales() {
    return SUPPORTED_LOCALES;
  }

  /**
   * Initialise from cookie + user profile + navigator.language.
   * Called once from +layout.svelte's onMount.
   */
  init(): void {
    // 1. Cookie wins for the initial paint (no network).
    const cookiePref = readCookie(COOKIE_NAME);
    // 2. Auth user profile — overrides cookie if non-empty (DB is
    //    the canonical source once signed in).
    const profilePref = (auth.user as unknown as { language?: string } | null)?.language ?? '';

    const chosen = profilePref || cookiePref || '';
    this.pref = chosen;
    this.resolved = chosen ? resolveLocale(chosen) : resolveLocale(systemPref());

    // Apply the cached overrides synchronously, then refresh from the
    // server. Mirrors appearance.init() — same first-paint problem,
    // same shape of answer.
    this.overrides = readOverrideCache();
    void this.refreshOverrides();
  }

  /**
   * Pull the operator override map from the public GET /site-text.
   *
   * Anonymous-readable, so this runs for logged-out visitors too: the
   * navbar a guest reads is the same navbar an admin reads, and copy
   * that only appeared after sign-in would be the feature half-working
   * in the case operators check first.
   *
   * A failure leaves whatever is already in `overrides` (usually the
   * localStorage cache) in place. Shipped strings are always a valid
   * render, so there is nothing to report to the user here.
   */
  async refreshOverrides(): Promise<void> {
    try {
      const { data } = await api.GET('/site-text');
      if (!data) return;
      const next = (data.overrides ?? {}) as SiteTextOverrides;
      this.overrides = next;
      writeOverrideCache(next);
    } catch {
      // Network/parse failure — keep the cached map.
    }
  }

  /**
   * Record an override locally after a successful write, so the admin
   * page's own chrome updates without a reload. The server is still
   * the source of truth; this only saves a round-trip.
   */
  applyOverride(language: string, key: string, value: string): void {
    const next: SiteTextOverrides = { ...this.overrides, [language]: { ...(this.overrides[language] ?? {}), [key]: value } };
    this.overrides = next;
    writeOverrideCache(next);
  }

  /** Mirror of applyOverride for a revert. */
  clearOverride(language: string, key: string): void {
    const forLang = { ...(this.overrides[language] ?? {}) };
    delete forLang[key];
    const next: SiteTextOverrides = { ...this.overrides };
    if (Object.keys(forLang).length > 0) next[language] = forLang;
    else delete next[language];
    this.overrides = next;
    writeOverrideCache(next);
  }

  /**
   * Set the user pref. Empty string = "follow system / browser".
   * Persists to cookie, and (if signed in) PATCH the user profile so
   * the choice follows the user across browsers.
   */
  async set(pref: string): Promise<void> {
    this.pref = pref;
    this.resolved = pref ? resolveLocale(pref) : resolveLocale(systemPref());
    writeCookie(COOKIE_NAME, pref, COOKIE_MAX_AGE);
    if (auth.user) {
      try {
        await api.PATCH('/users/{ref}', {
          params: { path: { ref: auth.user.ref } },
          body: { language: pref } as never,
        });
      } catch {
        // Soft fail — the cookie + local state still drive UI.
      }
    }
  }

  /**
   * Flat key lookup with `{var}` interpolation.
   *
   * Resolution order, which is the shipped chain with an override rung
   * inserted directly above each dictionary it replaces:
   *
   *   1. operator override for the active locale
   *   2. shipped catalogue for the active locale
   *   3. operator override for `en`
   *   4. shipped `en` catalogue
   *   5. the key itself
   *
   * Rungs 3 and 4 are the load-bearing choice. An `en` override does
   * NOT outrank a real Spanish translation — overriding an English
   * label must not silently un-translate the Spanish UI. But it DOES
   * back a locale that has no translation for the key, because in that
   * case the app was already rendering the English string, and the
   * operator changed what that English string says. The override sits
   * exactly where the string it replaces sat.
   *
   * Pinned by tests in lang.test.ts — the fallback rule is the part of
   * this feature that is easiest to "simplify" into being wrong.
   */
  t = (key: string, vars?: Record<string, string | number>): string => {
    // Subscribe to the reactive `resolved` + `overrides` so $derived
    // expressions and component renders re-run when either changes.
    // The direct property reads ARE the subscription.
    const code = this.resolved;
    const ov = this.overrides;

    const overridden = ov[code]?.[key];
    if (overridden !== undefined) return interpolate(overridden, vars);
    const hit = FLAT[code]?.[key];
    if (hit !== undefined) return interpolate(hit, vars);
    const overriddenEn = ov[DEFAULT_LOCALE]?.[key];
    if (overriddenEn !== undefined) return interpolate(overriddenEn, vars);
    const en = FLAT[DEFAULT_LOCALE]?.[key];
    if (en !== undefined) return interpolate(en, vars);
    return key;
  };
}

function systemPref(): string {
  if (typeof navigator === 'undefined') return DEFAULT_LOCALE;
  return navigator.language || DEFAULT_LOCALE;
}

function readCookie(name: string): string {
  if (typeof document === 'undefined') return '';
  const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'));
  return m ? decodeURIComponent(m[1]) : '';
}

function writeCookie(name: string, value: string, maxAgeSeconds: number): void {
  if (typeof document === 'undefined') return;
  const sec = ['', name + '=' + encodeURIComponent(value), 'Path=/', `Max-Age=${maxAgeSeconds}`, 'SameSite=Lax'];
  document.cookie = sec.filter(Boolean).join('; ');
}

export const lang = new LangState();

/**
 * The shipped, flattened catalogue for one locale — dotted key →
 * English (or translated) string.
 *
 * Exported for /admin/site-text, which lists every overridable key
 * beside what it ships as. It reads the SAME `FLAT` dictionaries `t()`
 * resolves against, so the "shipped value" column cannot drift from
 * what the app actually renders when an override is removed.
 *
 * Returns the live object rather than a copy — callers must treat it
 * as read-only.
 */
export function shippedStrings(code: string = DEFAULT_LOCALE): Record<string, string> {
  return FLAT[code] ?? {};
}

/**
 * Convenience top-level `t()` so components can `import { t } from
 * '$stores/lang.svelte'` and call it directly. Bound so callers
 * don't have to do `lang.t(...)`.
 */
export const t = lang.t;
