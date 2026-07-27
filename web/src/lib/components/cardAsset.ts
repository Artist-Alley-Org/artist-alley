// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The card feed contract (#595).
//
// Every asset-showing surface — browse, the profile pages, a collection,
// post-by-asset — renders through the same CardThumb. CardThumb decides
// what a tile IS from four fields, and only these four:
//
//   file_hash        does this asset have bytes at all
//   file_extension   what media TYPE it is — the video / 3D badge AND the
//                    sprite-scrub hover preview key off this alone
//   thumbhash        the blur-up placeholder shown before the image loads
//   preview_available whether a `col` variant may be requested (#471)
//
// THIS TYPE EXISTS TO MAKE THOSE FIELDS IMPOSSIBLE TO DROP SILENTLY.
//
// The bug it guards: the collection page built its member tiles by
// hand-mapping the API row into a narrower object literal, and simply
// did not copy file_extension or thumbhash. Nothing failed. The API
// types had them as `file_extension?: string | null`, so an object
// without the key was still assignable, and the card props were
// optional too — so the type system had no opinion at any of the four
// layers the value passes through. The tiles just quietly rendered as
// untyped stills: no video / 3D badge, no hover scrub, no blur-up. That
// survived four consecutive card refactors because nothing anywhere
// asserted the field had to be there.
//
// The fields are REQUIRED here and nullable rather than optional, which
// is the whole point: `null` is a real answer ("this asset has no
// extension"), while a missing key is a caller that forgot. Only the
// first is expressible now. The matching OpenAPI schemas (Asset,
// CollectionResource) list them as required-but-nullable for the same
// reason, so a surface passing an API row through verbatim satisfies
// this contract for free and only hand-mapped literals have to think.
//
// If you add a field CardThumb reads to decide presentation, add it
// here too — that is what keeps the next refactor honest.

/** The minimum an asset-shaped row must carry to render as a card. */
export interface CardAsset {
  id: string;
  title: string;
  asset_type: number;
  created_at: string;
  file_hash: string | null;
  file_extension: string | null;
  thumbhash: string | null;
  preview_available: boolean;
  /** Every rung of the operator's CONFIGURED ladder exists for this
   *  asset (#610). Distinct from preview_available, which promises only
   *  `col` — a 320x320 cover CROP. This is what licenses a responsive
   *  srcset over the wider `contain` rungs; without it a card can only
   *  safely request `col`, which is why widescreen art was being
   *  square-cropped (#502/#589). */
  ladder_available: boolean;
  /** Recorded source pixel dimensions, or null (#640). Masonry sizes
   *  each tile from this ratio BEFORE the image loads, so the column
   *  heights are right from first paint and the wall doesn't reflow as
   *  72 images arrive. Null for everything the EXIF pass hasn't
   *  measured — draft rasters and every non-raster kind — where the
   *  card falls back to measuring the image on load. */
  pixel_width: number | null;
  pixel_height: number | null;
}

/** The asset payload joined into a post member — the same presentation
 *  fields, minus the ones PostCard reads off the post itself (title,
 *  created_at) rather than off the cover asset. */
export interface CardCoverAsset {
  id: string;
  file_hash: string | null;
  file_extension: string | null;
  thumbhash: string | null;
  preview_available: boolean;
  /** See CardAsset.ladder_available. */
  ladder_available: boolean;
  /** See CardAsset.pixel_width. */
  pixel_width: number | null;
  pixel_height: number | null;
}
