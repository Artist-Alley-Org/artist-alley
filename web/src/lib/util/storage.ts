// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE web-storage accessor (#1255).
//
// ⛔ `localStorage.getItem` DOES NOT MERELY RETURN NULL WHEN STORAGE IS
// UNAVAILABLE — IT THROWS, and so does `setItem`.
//
//   - a `SecurityError` in any context where the user has blocked site
//     data ("Block third-party cookies and site data", enterprise
//     policy, a sandboxed iframe without `allow-same-origin`);
//   - a `QuotaExceededError` from Safari's private windows, which
//     historically threw on the first WRITE while reads kept working;
//   - a bare `ReferenceError` where the global does not exist at all
//     (server-side rendering, a worker).
//
// So the failure is not "the preference comes back empty". It is an
// exception thrown out of whatever function touched storage — and the
// functions that touch storage are the ones that run at mount, so the
// throw escapes `onMount` and the component never renders. #1251 slice 3
// found four of those in `browseView.init()` and a blank browse wall
// behind them; this module exists so the fix is a call site, not a
// remembered `try`.
//
// # Every accessor fails toward the CALLER'S DEFAULT
//
// A read that cannot answer returns the fallback the caller passed, and
// a write that cannot land is dropped. That is the only direction that
// cannot silently apply a setting the reader never chose: an unreadable
// store means NO STORED CHOICE, never "the opposite of the default".
// A control whose write is dropped still works for the session — the
// in-memory state is authoritative and always was — it just forgets.
//
// # Why there is no `browser` guard here
//
// `$app/environment`'s `browser` would short-circuit the SSR case one
// step earlier, and it is not needed: the `ReferenceError` an absent
// global raises lands in the same `catch` as a `SecurityError` and
// produces the same fallback. Leaving it out keeps this module free of
// any SvelteKit dependency, so plain-node consumers and tests can use
// it without the kit's virtual modules.

/** One raw string, or `fallback` when storage cannot answer.
 *
 *  `null` is a real answer here — "no such key" — and callers that treat
 *  absent and unreadable differently should pass a sentinel fallback
 *  rather than trying to tell them apart afterwards. Nothing in the tree
 *  needs to: every consumer resolves both to "no stored choice". */
export function readStored(key: string, fallback: string | null = null): string | null {
  try {
    return localStorage.getItem(key) ?? fallback;
  } catch {
    return fallback;
  }
}

/** Store one raw string. Dropped silently when storage refuses. */
export function writeStored(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* quota / disabled — the in-memory value stands */
  }
}

/** Drop one key. Dropped silently when storage refuses. */
export function removeStored(key: string): void {
  try {
    localStorage.removeItem(key);
  } catch {
    /* quota / disabled */
  }
}

/** A JSON-encoded value, or `fallback` when the key is absent, storage
 *  is unreadable, OR the stored text does not parse.
 *
 *  The parse failure shares the fallback deliberately: a corrupt entry
 *  (a half-written value, a key an older build wrote in another shape)
 *  is no more of an answer than an unreadable store, and a component
 *  that crashed on it would be broken until the user cleared their site
 *  data by hand. */
export function readStoredJSON<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    if (raw == null) return fallback;
    return JSON.parse(raw) as T;
  } catch {
    return fallback;
  }
}

/** Store a JSON-encodable value. Dropped silently when storage refuses
 *  — or when the value itself cannot be serialised, which `JSON.
 *  stringify` raises for a cycle and which must not take the caller
 *  down either. */
export function writeStoredJSON(key: string, value: unknown): void {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    /* quota / disabled / unserialisable */
  }
}
