// Tiny wrapper around the `thumbhash` npm decoder.
//
// The backend (assets.handler.go) stores a base64 thumbhash on every
// image asset — ~30 bytes decoded. This module turns that into a
// `data:image/png;base64,...` URI suitable for a CSS `background-image`
// so a card renders with a blurred placeholder instantly, no HTTP
// round-trip required.
//
// Decoding is cached per-string in a small LRU so flipping back and
// forth between feed pages doesn't re-decode the same hashes.

import { thumbHashToDataURL } from 'thumbhash';

const CACHE_MAX = 512;
const cache = new Map<string, string>();

function base64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/**
 * decode turns a base64 thumbhash into a data URI. Returns null on
 * invalid input or empty string, so callers can `if (uri) {...}`.
 *
 * Safe for SSR: returns null when `atob` is unavailable.
 */
export function decodeThumbhash(b64: string | null | undefined): string | null {
  if (!b64) return null;
  const hit = cache.get(b64);
  if (hit) return hit;
  if (typeof atob !== 'function') return null;
  try {
    const bytes = base64ToBytes(b64);
    const dataURL = thumbHashToDataURL(bytes);
    // Lightweight LRU: drop the oldest if we hit the cap.
    if (cache.size >= CACHE_MAX) {
      const first = cache.keys().next().value;
      if (first !== undefined) cache.delete(first);
    }
    cache.set(b64, dataURL);
    return dataURL;
  } catch {
    return null;
  }
}
