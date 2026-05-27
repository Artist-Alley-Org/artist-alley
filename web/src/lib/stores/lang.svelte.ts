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
   * Flat key lookup with `{var}` interpolation. Resolves against the
   * active locale, then en, then the key itself.
   */
  t = (key: string, vars?: Record<string, string | number>): string => {
    // Subscribe to the reactive `resolved` so $derived expressions
    // and component renders re-run when the locale changes. The
    // direct property read is the subscription.
    const code = this.resolved;
    const hit = FLAT[code]?.[key];
    if (hit !== undefined) return interpolate(hit, vars);
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
 * Convenience top-level `t()` so components can `import { t } from
 * '$stores/lang.svelte'` and call it directly. Bound so callers
 * don't have to do `lang.t(...)`.
 */
export const t = lang.t;
