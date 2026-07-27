// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Preview-ladder store — what image rungs this install actually
// generates, read once from `GET /previews` (#591 PR D, closing #502).
//
// WHY THIS EXISTS. Cards used to hardcode `/variants/col` as their only
// image URL. `col` is `fit: cover, max_dim: 320` — a 320x320 CENTRE
// CROP by construction — so a widescreen video poster or a panoramic
// still filled the card at the wrong ratio, visibly disagreeing with the
// hover sprite-scrub beside it (sprite frames are stored at native video
// aspect, so the scrub was right and the poster was wrong). The wider
// `contain` rungs existed all along; what was missing was any way for
// the client to know whether they were servable for a given asset —
// which `ladder_available` (#610) now answers per asset, and this
// endpoint answers per install.
//
// NEVER HARDCODE THE FOUR DEFAULT KEYS. That is #610's trap reproduced
// on the client side: the ladder is operator-configurable, so an install
// that dropped `hires` or renamed its rungs would have every card
// request bytes that 404. The rung keys and their max_dim come from the
// server, and a client that cannot read them falls back to `col`-only —
// the one rung `preview_available` already guarantees.
//
// FAILS TO COL-ONLY, DELIBERATELY. `/previews` is public-mode governed
// (#611): on a private install an anonymous visitor gets 401, which is
// correct and expected, not an error worth surfacing. Any failure —
// 401, offline, malformed — leaves `rungs` empty, which makes
// `srcsetFor()` return null and every card render exactly as it does
// today. Degraded, never broken.

import { browser } from '$app/environment';
import { api } from '$api/client';

/** One rung of the install's configured ladder. */
export interface LadderRung {
  key: string;
  maxDim: number;
}

class PreviewLadderState {
  /** `fit: contain` rungs only, ascending by max_dim.
   *
   *  Cover rungs are deliberately excluded: `col` is a square crop, so
   *  it cannot participate in a width-descriptor srcset — mixing it in
   *  would let the browser pick a differently-shaped image for the same
   *  slot. The grid's `fill` mode still uses `col` directly, because
   *  there the square crop is the intent. */
  rungs = $state<LadderRung[]>([]);
  loaded = $state(false);

  #inflight: Promise<void> | null = null;

  /** Fetch once per page load; concurrent callers share the flight.
   *  Cards call this on mount, so the first card triggers it and the
   *  other 199 await the same promise. */
  init(): void {
    if (!browser || this.loaded || this.#inflight) return;
    this.#inflight = this.#load().finally(() => {
      this.#inflight = null;
    });
  }

  async #load(): Promise<void> {
    try {
      const { data } = await api.GET('/previews');
      if (data?.variants) {
        this.rungs = data.variants
          .filter((v) => v.fit === 'contain' && v.max_dim > 0)
          .map((v) => ({ key: v.key, maxDim: v.max_dim }))
          .sort((a, b) => a.maxDim - b.maxDim);
      }
    } catch {
      // 401 on a private install pre-auth, offline, malformed — all
      // land here and all mean the same thing: no ladder, use col.
    } finally {
      // `loaded` means "we asked", not "we got rungs" — otherwise a
      // 401 would retry on every card mount.
      this.loaded = true;
    }
  }

  /** Build a `srcset` for an asset from the contain rungs, or null when
   *  there is no usable ladder (caller then falls back to `col`).
   *
   *  Width descriptors, not `x` descriptors: the browser knows the slot
   *  width from `sizes` and the device pixel ratio, and picks the
   *  smallest rung that covers it. That is what stops a 200px tile
   *  pulling `hires`. */
  srcsetFor(assetId: string): string | null {
    if (this.rungs.length === 0) return null;
    return this.rungs
      .map((r) => `/api/v1/assets/${assetId}/variants/${r.key} ${r.maxDim}w`)
      .join(', ');
  }

  /** The smallest contain rung — the `src` fallback for browsers that
   *  ignore srcset, and the sensible default when the slot is small.
   *  Null when there is no ladder. */
  smallestKey(): string | null {
    return this.rungs[0]?.key ?? null;
  }
}

export const previewLadder = new PreviewLadderState();
