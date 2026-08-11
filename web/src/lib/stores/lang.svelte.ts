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
   * Apply a locale: state, `<html lang>`, and optionally the cookie.
   *
   * ONE path, called by every path that changes the rendered locale, so
   * the three things that must agree cannot drift apart (#967).
   *
   * `<html lang>` is the part that was missing entirely. app.html
   * hardcodes `lang="en"` and nothing ever wrote it, so a fully French
   * render still announced itself as English. That is an accessibility
   * defect rather than a cosmetic one: screen readers pick voice and
   * pronunciation rules off that attribute and CSS `:lang()` keys off
   * it, so a French page read aloud in an English voice is worse than an
   * untranslated one — the user cannot tell it is wrong from the text.
   * It carries `resolved`, never `pref`: `pref` may be '' or a tag we
   * have no catalogue for, and the attribute must describe the language
   * actually on screen.
   *
   * `persist` is false for init() alone, and the asymmetry is
   * deliberate. init() READ the cookie; writing it straight back would
   * be a device-state write on a non-user action, and in the
   * no-cookie-no-account case it would pin a navigator-derived guess
   * into the cookie permanently — the same trap theme.syncFromAccount()
   * documents for its own default. Every path that changes the locale
   * to something the cookie does NOT already say persists it.
   */
  private apply(pref: string, resolved: string, persist: boolean): void {
    this.pref = pref;
    this.resolved = resolved;
    applyLangToDom(resolved);
    if (persist) writeCookie(COOKIE_NAME, pref, COOKIE_MAX_AGE);
  }

  /**
   * Initialise from cookie + user profile + navigator.language.
   * Called once from +layout.svelte's onMount.
   */
  init(): void {
    // 1. Cookie, else the browser's own preference. No network.
    const cookiePref = readCookie(COOKIE_NAME);
    this.apply(
      cookiePref,
      cookiePref ? resolveLocale(cookiePref) : resolveLocale(systemPref()),
      false,
    );
    // 2. The account, which outranks the device — the DB is canonical
    //    once signed in. Delegated rather than repeated: syncFromAccount
    //    runs on every path that publishes a user (#869) and this one
    //    runs at mount, so two copies of "does the account win" would be
    //    two rules to keep in step, and the drift would present as the
    //    same user getting a different language depending on which ran
    //    last. Whichever fires here is a no-op when auth.user is null.
    this.syncFromAccount();

    // Apply the cached overrides synchronously, then refresh from the
    // server. Mirrors appearance.init() — same first-paint problem,
    // same shape of answer.
    this.overrides = readOverrideCache();
    void this.refreshOverrides();
  }

  /**
   * Adopt the signed-in account's `language` (#869).
   *
   * Called from AuthState.adopt(), which is the one place `user` is
   * assigned — so this runs on the sign-in path (login()), the boot
   * path (hydrateFrom(), via +layout.ts) and the re-fetch path
   * (refresh()) alike, and cannot be reached by a fourth path that
   * forgets to call it.
   *
   * init() alone does not cover this and cannot. The root layout mounts
   * ONCE, so init() runs against whatever `auth.user` held at that
   * moment: for a visitor who lands on /login that is null, and their
   * account language would then never be applied until a full reload
   * picked it up off the profile. theme.syncFromAccount() exists for
   * exactly the same gap; this is the language half of it.
   *
   * PRECEDENCE IS THE OPPOSITE OF THEME'S, and deliberately so. Theme
   * returns early when the device cookie is set — the device has spoken
   * and the account does not argue. Language overwrites it: the ACCOUNT
   * is canonical once signed in, which is the precedence init() has
   * always applied (`profilePref || cookiePref`) and which now lives
   * here alone. A language is a property of the person; a colour scheme
   * is a property of the screen they happen to be sitting at.
   *
   * It sends no PATCH — mirroring a value the server just told us back
   * to the server is a write nobody asked for.
   *
   * It DOES now write the cookie, which is the reversal #967 asked for
   * and the whole of the first-paint fix. This is a static build: the
   * page paints before /auth/me answers, so the account language cannot
   * be known before hydration, and without a cookie a French account got
   * an English first paint on EVERY cold load and then swapped. The
   * cookie is the only thing app.html's pre-paint script can read.
   *
   * The sprint that found this declined the write for a real reason — it
   * changes device state as a side effect of a non-user action, and it
   * OUTLIVES LOGOUT, so a shared machine would carry one account's
   * language into the next visitor's first paint. That objection is
   * answered rather than overruled: reset() clears the cookie, and
   * logout() calls it. The write is safe because its lifetime is now
   * bounded by the session that caused it.
   */
  syncFromAccount(): void {
    const account = (auth.user as unknown as { language?: string | null } | null)?.language ?? '';
    // Null / absent / empty is the NORMAL state, not a fault: it is the
    // stored value for every account that has never picked a language,
    // and it means "follow this device". Falling back to the cookie
    // then navigator.language then en is correct, and silent, because
    // nothing was asked for and nothing is being ignored.
    if (!account) return;
    const resolved = resolveLocale(account);
    // An UNRECOGNISED tag is a different thing and gets a warning.
    // UserProfileUpdate.language is validated only by maxLength server-
    // side, so `de` or `en_US` or a typo can genuinely be stored; the
    // account asked for something and is about to be handed English
    // instead. That is the right fallback and the wrong thing to do
    // quietly — the person sees a language they did not choose and has
    // no way to find out why.
    if (resolved !== account && resolved !== account.split('-')[0]) {
      console.warn(
        `[i18n] account language ${JSON.stringify(account)} is not a supported locale `
        + `(${SUPPORTED_LOCALES.map((l) => l.code).join(', ')}); rendering ${resolved}`,
      );
    }
    this.apply(account, resolved, true);
  }

  /**
   * Forget the device's language and go back to the default (#967).
   *
   * Called from AuthState.logout() and from nowhere else, and the "and
   * from nowhere else" is the rule, not an implementation detail.
   *
   * It is NOT tied to being anonymous. An anonymous visitor who picks
   * French from the picker keeps French across reloads for a year —
   * that is their explicit choice and it is what set() stored. What gets
   * cleared is a language this device only knows because an ACCOUNT was
   * signed in on it, and the moment that account signs out the device
   * has no business still speaking it: the next person at a shared
   * machine gets the default, not a stranger's language.
   *
   * Deliberately not called from clear(), the 401 path. An expired
   * session is not somebody leaving; wiping their language because a
   * token aged out would be a surprise mid-visit, and they are about to
   * sign in again and re-adopt it anyway.
   */
  reset(): void {
    clearCookie(COOKIE_NAME);
    this.apply('', resolveLocale(systemPref()), false);
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
    this.apply(pref, pref ? resolveLocale(pref) : resolveLocale(systemPref()), true);
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

/** Expire the cookie now. Same name and Path as writeCookie — a cookie
 *  written at `Path=/` is not removed by expiring one at a different
 *  path, and the leftover would keep painting the old language. */
function clearCookie(name: string): void {
  if (typeof document === 'undefined') return;
  document.cookie = `${name}=; Path=/; Max-Age=0; SameSite=Lax`;
}

/**
 * Write the active locale onto `<html lang>` (#967).
 *
 * MUST stay in lockstep with the pre-paint script in app.html, which
 * sets the same attribute from the same cookie before hydration. If the
 * two disagree the attribute changes after paint — assistive tech that
 * already chose a voice does not necessarily revisit that choice, which
 * is the flash-of-wrong-language equivalent the script exists to
 * prevent. `langScript.test.ts` pins them together.
 */
function applyLangToDom(resolved: string): void {
  if (typeof document === 'undefined') return;
  document.documentElement.setAttribute('lang', resolved);
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
