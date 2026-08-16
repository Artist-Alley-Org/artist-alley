// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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

import { thumbHashToAverageRGBA, thumbHashToDataURL } from 'thumbhash';

const CACHE_MAX = 512;
const cache = new Map<string, string>();
/** Separate from `cache` because it stores NULLs (a hash with no usable
 *  average is a permanent answer, and re-deriving it per card per frame
 *  is the cost this whole module exists to avoid). */
const matteCache = new Map<string, string | null>();

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

/**
 * The AVERAGE colour of a thumbhash, as a CSS `rgb(...)` string (#1136).
 *
 * Thumbnail view letterboxes the preview on a matte, and the reference
 * panel this density is modelled on paints that matte with a colour
 * SAMPLED FROM THE IMAGE rather than a neutral grey. On a shelf of
 * mixed-aspect work that is the difference between a wall of grey
 * rectangles with pictures in them and a wall that reads as the pictures
 * themselves — the letterbox stops announcing itself.
 *
 * The sample is free: a thumbhash's first bytes ARE the average colour,
 * which is what `thumbHashToAverageRGBA` reads. There is no canvas, no
 * image decode, and no second request — the same ~30 bytes the card
 * already has for its loading placeholder.
 *
 * DESATURATED AND DARKENED toward the page rather than used raw. A raw
 * average is often a mid-tone that competes with the artwork sitting on
 * it, and a bright one turns a dark theme's card into a lightbox. The
 * mix below keeps the hue — which is the whole point, the matte should
 * feel like it belongs to this picture — while pulling the value most of
 * the way to a neutral, so the matte stays a matte.
 *
 * Returns null for a missing or malformed hash, and for SSR (`atob` is
 * unavailable), so callers fall back to the neutral `bg-thumb-matte`
 * with no branch of their own beyond `?? undefined`.
 */
export function thumbhashMatteColor(b64: string | null | undefined): string | null {
  if (!b64) return null;
  const hit = matteCache.get(b64);
  if (hit !== undefined) return hit;
  if (typeof atob !== 'function') return null;
  let out: string | null = null;
  try {
    const { r, g, b } = thumbHashToAverageRGBA(base64ToBytes(b64));
    // r/g/b arrive 0..1. MATTE_MIX is how much of the sampled colour
    // survives; the rest is the page's own dark neutral. 0.35 was picked
    // by looking at a seeded shelf: at 1.0 the matte is a colour field
    // and the art floats on it, at 0.15 it is indistinguishable from the
    // neutral it replaces.
    const MATTE_MIX = 0.35;
    const NEUTRAL = 32; // the dark matte's own value, 0..255
    const mix = (c: number) => Math.round(c * 255 * MATTE_MIX + NEUTRAL * (1 - MATTE_MIX));
    out = `rgb(${mix(r)} ${mix(g)} ${mix(b)})`;
  } catch {
    out = null;
  }
  if (matteCache.size >= CACHE_MAX) {
    const first = matteCache.keys().next().value;
    if (first !== undefined) matteCache.delete(first);
  }
  matteCache.set(b64, out);
  return out;
}
