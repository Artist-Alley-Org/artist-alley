// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Google Fonts lazy loader.
//
// Picking a font from the typography selector loads its CSS once
// via a <link> appended to <head>. Multiple selectors / sessions
// share one load per family — we dedupe by family name.
//
// We deliberately don't bundle every face in the catalogue: that's
// hundreds of KB of CSS + woff2 the user hasn't asked for. Loading
// on demand means a session that only uses Inter never pays the
// Limelight + Pacifico + Permanent-Marker tax.
//
// Weights are requested 400 + 700 (regular + bold) when both are
// available; display / handwriting faces that only ship 400 get a
// 400 link. The bold toggle on the text tool maps to the 700
// variant when present; falls back to faux-bold via the canvas
// `font` shorthand otherwise (the browser synthesizes).
//
// Why .svelte.ts: the `loaded` set is reactive so the toolbar can
// show a small loading indicator next to a font that hasn't
// resolved yet (avoids a flash of system-ui).

import type { FontEntry } from './types';
import { GOOGLE_FONTS } from './types';

const loaded = $state(new Set<string>());

function familyToHref(entry: FontEntry): string {
  const family = entry.family.replace(/ /g, '+');
  const weights = entry.weights.join(';');
  // display=swap lets the canvas render with a system fallback
  // while the woff2 is still in flight, then re-flow on load —
  // matches the user's expectation that "the font name I picked is
  // already showing".
  return `https://fonts.googleapis.com/css2?family=${family}:wght@${weights}&display=swap`;
}

/** Idempotent — calling more than once for the same family is a
 *  no-op. Returns a Promise that resolves once the font is
 *  available so callers can re-render text items. */
export function loadFont(family: string): Promise<void> {
  const entry = GOOGLE_FONTS.find((f) => f.family === family);
  if (!entry) return Promise.resolve();
  if (loaded.has(family)) return Promise.resolve();
  // Append the <link> first so the browser starts the fetch
  // immediately; the FontFace API resolves once the file lands.
  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = familyToHref(entry);
  document.head.appendChild(link);
  loaded.add(family);
  // document.fonts.load is the canonical "wait until this face is
  // usable" hook. Probe each requested weight; the promise resolves
  // as soon as any of them is available so we can re-render.
  const probes = entry.weights.map((w) =>
    document.fonts.load(`${w} 32px "${entry.family}"`).then(
      () => undefined,
      () => undefined,
    ),
  );
  return Promise.race(probes);
}

/** Reactive set of family names that have been loaded so far. */
export function loadedFonts(): Set<string> {
  return loaded;
}

/** Preload every family in the catalogue. Not currently called —
 *  callers can opt in when they want the picker preview to render
 *  instantly. For now we lazy-load on selection. */
export function loadAllFonts(): void {
  for (const f of GOOGLE_FONTS) void loadFont(f.family);
}
