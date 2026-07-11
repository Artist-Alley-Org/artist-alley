// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
// `completionPct` is computed at module load from the bundled
// catalogues — the share of `en` leaf keys that the locale also
// defines. The picker renders it as "(N%)" beside the locale name so
// users know how complete each translation is.

import enDict from './en.json';
import esDict from './es.json';
import frDict from './fr.json';

export interface Locale {
  /** IETF BCP47 code (e.g. "en", "es-AR"). */
  readonly code: string;
  /** English name ("Spanish (Argentina)"). */
  readonly name: string;
  /** Endonym ("Español (Argentina)"). */
  readonly nativeName: string;
  /** Optional regional sub-tag ("AR"). */
  readonly region?: string;
  /** 0..100, share of `en` keys translated. Computed at load. */
  readonly completionPct: number;
}

function leafCount(src: Record<string, unknown>): number {
  let n = 0;
  for (const v of Object.values(src)) {
    n += v != null && typeof v === 'object' && !Array.isArray(v) ? leafCount(v as Record<string, unknown>) : 1;
  }
  return n;
}

const EN_LEAVES = leafCount(enDict as Record<string, unknown>);

// Share of en keys a locale covers. This over-counts slightly (a
// locale key absent from en still counts toward its own leaf total),
// but the parity guard asserts zero orphan keys, so in practice a
// locale's leaves are a subset of en's — the ratio is exact.
function pct(dict: Record<string, unknown>): number {
  if (EN_LEAVES === 0) return 0;
  return Math.round((leafCount(dict) / EN_LEAVES) * 100);
}

export const SUPPORTED_LOCALES: readonly Locale[] = [
  { code: 'en', name: 'English', nativeName: 'English', completionPct: 100 },
  { code: 'es', name: 'Spanish', nativeName: 'Español', completionPct: pct(esDict as Record<string, unknown>) },
  { code: 'fr', name: 'French', nativeName: 'Français', completionPct: pct(frDict as Record<string, unknown>) },
];

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
