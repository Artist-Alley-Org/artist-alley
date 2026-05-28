// Locale registry — the locales the frontend has a catalogue for.
//
// Kept in sync by hand with the server-side registry in
// `app/internal/i18n/handler.go` and with the JSON files in this
// directory. Adding a locale means:
//   1. Drop a {code}.json catalogue here (start as `{}` and fall back
//      to en for missing keys).
//   2. Add the row below.
//   3. Add the matching row on the server.
//
// `completionPct` is computed dynamically at build time later; today
// it's a static estimate the picker renders as "(5%)" beside the
// locale name so users know it's a stub.

export interface Locale {
  /** IETF BCP47 code (e.g. "en", "es-AR"). */
  readonly code: string;
  /** English name ("Spanish (Argentina)"). */
  readonly name: string;
  /** Endonym ("Español (Argentina)"). */
  readonly nativeName: string;
  /** Optional regional sub-tag ("AR"). */
  readonly region?: string;
  /** 0..100, share of `en` keys translated. */
  readonly completionPct: number;
}

export const SUPPORTED_LOCALES: readonly Locale[] = [
  { code: 'en', name: 'English', nativeName: 'English', completionPct: 100 },
  { code: 'es', name: 'Spanish', nativeName: 'Español', completionPct: 5 },
  { code: 'fr', name: 'French', nativeName: 'Français', completionPct: 5 },
] as const;

export const DEFAULT_LOCALE = 'en' as const;

/**
 * Match a user-preferred locale (e.g. "es-AR") against the supported
 * list. Resolution order matches Unicode CLDR conventions:
 *   1. Exact match ("es-AR" → "es-AR" if we have it)
 *   2. Language match ("es-AR" → "es" if we have it)
 *   3. DEFAULT_LOCALE
 */
export function resolveLocale(pref: string): string {
  if (!pref) return DEFAULT_LOCALE;
  const exact = SUPPORTED_LOCALES.find((l) => l.code === pref);
  if (exact) return exact.code;
  const lang = pref.split('-')[0];
  const fam = SUPPORTED_LOCALES.find((l) => l.code === lang);
  if (fam) return fam.code;
  return DEFAULT_LOCALE;
}
