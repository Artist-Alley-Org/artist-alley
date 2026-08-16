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
  /** `fit: cover` rungs only, ascending by max_dim — the square crops
   *  (`col` by default). Kept separately rather than mixed into `rungs`
   *  because the two answer different questions: a contain rung's
   *  max_dim is its LONG side, a cover rung's is its square EDGE, so a
   *  single width descriptor cannot be derived from both the same way.
   *  Read only by `coverSrcsetFor` (#1169). */
  coverRungs = $state<LadderRung[]>([]);
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
        this.coverRungs = data.variants
          .filter((v) => v.fit === 'cover' && v.max_dim > 0)
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

  /** A `srcset` for a tile that CROPS its image to a square — grid's
   *  `fill` mode (#1169) — or null when nothing usable can be offered.
   *
   *  WHY THIS IS NOT `srcsetFor`. A width descriptor states how many
   *  source pixels a candidate can put across the slot. In a contain
   *  slot that is the file's own width; in a COVER slot the picture is
   *  scaled until it fills the box and the overflow is clipped, so a
   *  square tile of side S is filled by the image's SHORT axis. A
   *  contain rung capped at `max_dim` on its LONG side therefore offers
   *  only `max_dim × short/long` usable pixels, and handing the browser
   *  its `max_dim` instead would over-promise by the aspect ratio — a
   *  16:9 `preview` (1024) would claim 1024 and deliver 576.
   *
   *  That single correction is what unblocks the ladder for grid. Before
   *  it, `col` (320²) was the only rung whose descriptor could be stated
   *  for a cropped slot, so every grid tile on every surface decoded a
   *  320px image no matter how wide the tile was.
   *
   *  Both rung families are offered, and `col` staying a candidate is
   *  why this is never worse than what it replaces: a small tile still
   *  picks the 320px square, a large one now has somewhere to go.
   *
   *  SOURCE DIMENSIONS ARE REQUIRED FOR THE CONTAIN RUNGS and are not a
   *  nice-to-have: without them the crop fraction is unknown and every
   *  descriptor would be a guess. Null dimensions fall back to the cover
   *  rungs alone, which is exactly today's behaviour on the default
   *  ladder (`col` only).
   *
   *  They also CAP each rung, because the renderer skips upscales
   *  (`SkipUpscale`, sysconfig/previews.go): a 400px source stored in
   *  the 4096 `hires` rung is 400px, and claiming 4096 would park every
   *  large tile on a file that cannot fill it.
   *
   *  ⚠️ The slot is assumed SQUARE, which `fill` is: grid's frame is
   *  `aspect-square` and `variableAspect` is masonry-only. A future
   *  non-square cropping tile needs the tile ratio passed in — the
   *  usable width is then `min(w, h × tileRatio)`, not `min(w, h)`. */
  coverSrcsetFor(
    assetId: string,
    pixelWidth?: number | null,
    pixelHeight?: number | null,
  ): string | null {
    const w = pixelWidth ?? 0;
    const h = pixelHeight ?? 0;
    const known = w > 0 && h > 0;

    // `cover` sorts before `contain` at equal width: at the same usable
    // width the square crop is the smaller file, so a tie should not
    // cost bytes.
    const cands: Array<{ key: string; width: number; cover: boolean }> = [];
    for (const r of this.coverRungs) {
      // A cover rung is cropped to a square, then scaled to max_dim
      // unless that would upscale — so its edge is capped by the
      // source's SHORT side.
      cands.push({
        key: r.key,
        width: known ? Math.min(r.maxDim, Math.min(w, h)) : r.maxDim,
        cover: true,
      });
    }
    if (known) {
      const short = Math.min(w, h);
      const long = Math.max(w, h);
      for (const r of this.rungs) {
        cands.push({
          key: r.key,
          width: Math.round((Math.min(r.maxDim, long) * short) / long),
          cover: false,
        });
      }
    }

    // One candidate per width. Duplicates are routine rather than
    // exotic — a source smaller than two rungs' caps lands both on the
    // source's own size — and a srcset with a repeated descriptor makes
    // the choice arbitrary.
    cands.sort((a, b) => a.width - b.width || Number(b.cover) - Number(a.cover));
    const out: string[] = [];
    let last = -1;
    for (const c of cands) {
      if (c.width <= 0 || c.width === last) continue;
      last = c.width;
      out.push(`/api/v1/assets/${assetId}/variants/${c.key} ${c.width}w`);
    }
    return out.length > 0 ? out.join(', ') : null;
  }

  /** The smallest contain rung — the `src` fallback for browsers that
   *  ignore srcset, and the sensible default when the slot is small.
   *  Null when there is no ladder. */
  smallestKey(): string | null {
    return this.rungs[0]?.key ?? null;
  }
}

export const previewLadder = new PreviewLadderState();
