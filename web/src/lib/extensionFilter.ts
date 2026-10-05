// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * Frontend normalization for the `extension:` filter (#1173, sprint
 * 18d).
 *
 * # ⛔ WHY THIS IS THE FRONTEND'S JOB AND NOT THE SERVER'S
 *
 * `FacetExtension` has NO `CanonicalValue` case: it falls through to the
 * default `return v, true`, so the value on the wire is whatever was
 * sent. The SQL then matches `LOWER(a.file_extension) = LOWER($n)`, so
 * `PNG` and `png` already select the same rows — the server is
 * case-insensitive at MATCH time and case-preserving at IDENTITY time.
 *
 * Teaching the server to canonicalise instead would change
 * `Selection.CacheKey` for every stored search that already carries an
 * extension term, which is a behavioural change to saved searches
 * arriving as a side effect of a UI sprint. So it stays here, and the
 * report-only finding is recorded in the sprint's handoff rather than
 * fixed in passing.
 *
 * What this buys, concretely: `PNG` and `png` ticked together must
 * become ONE term. Two terms would still return the right rows, but they
 * would produce a different cache key, a different saved-search
 * spelling, and a duplicate chip on screen.
 *
 * # The rule, exactly
 *
 *   trim → strip ONE leading `.` → lowercase → drop empty and dot-only
 *   → deduplicate, first occurrence winning
 *
 * ONE leading dot, not all of them: a person types `.png` because that
 * is how a file is named, and a name that genuinely begins with a dot
 * after that is not something this control invents a meaning for.
 */

/**
 * Normalize one typed extension. Returns `null` for anything that
 * carries no extension at all — an empty string, whitespace, or a run of
 * dots.
 */
export function normalizeExtension(raw: string): string | null {
  let s = raw.trim();
  if (s.startsWith('.')) s = s.slice(1);
  s = s.toLowerCase();
  if (s === '') return null;
  // A value that is nothing but dots names no extension. `.` reaches
  // here as ``, `..` as `.`, and both mean the same thing.
  if (/^\.+$/.test(s)) return null;
  return s;
}

/**
 * Add one typed extension to a selection, normalizing it and collapsing
 * a duplicate. Returns the list unchanged when the input normalizes to
 * nothing or to a value already held.
 */
export function addExtension(current: readonly string[], raw: string): string[] {
  const v = normalizeExtension(raw);
  if (v === null) return [...current];
  if (current.includes(v)) return [...current];
  return [...current, v];
}

/**
 * Normalize a whole list, dropping empties and duplicates while keeping
 * first-occurrence order. Used when a list arrives from anywhere other
 * than a single keystroke — a paste, a comma-separated entry, or the
 * facet buckets.
 */
export function normalizeExtensions(raw: readonly string[]): string[] {
  const out: string[] = [];
  for (const r of raw) {
    const v = normalizeExtension(r);
    if (v !== null && !out.includes(v)) out.push(v);
  }
  return out;
}
