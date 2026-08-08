// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Search hit → card row (#850).
//
// /search used to render its own text cards because a hit did not carry
// enough to render anything else. It does now: the engine ships a
// card-shaped `extra` bag whose fields are named exactly as the list
// endpoints name them (see app/internal/search/cards.go), so this module
// is a narrowing pass rather than a translation.
//
// It exists as ONE module, annotated with the card contracts, for the
// reason #595 records: an un-annotated object literal is assignable to a
// card prop, so a surface that drops `file_extension` or `thumbhash`
// silently loses the media-type badge and the blur-up and nothing fails.
// With `CardAsset` as the declared return type, forgetting a
// presentation field is a type error here instead of a missing badge in
// somebody's browser.
//
// ⛔ RESTRICTED HITS. A hit for an asset the caller may not open carries
// `type`, `id`, `restricted`, `owner_display_name` and the scores — and
// nothing else. Every mapper below therefore branches on `restricted`
// FIRST and fills the card contract's required shape with values the
// restricted plate never reads. Do not "simplify" that into `?? ''`
// fallbacks on the normal branch: that turns a withheld title into a
// blank one, which is a different statement.

import type { CardAsset, CardCoverAsset } from '$components/cardAsset';

/** The three things a search result can be. */
export type HitType = 'asset' | 'collection' | 'post';

/**
 * A single hit as GET /api/v1/search serialises it.
 *
 * Everything except `type`, `id`, `restricted` and the scores is
 * OPTIONAL on the wire (#899): a hit for an asset the caller cannot open
 * carries no columns, so declaring `title: string` here would let a
 * template read a field the server deliberately did not send.
 */
export interface SearchHit {
  type: HitType;
  id: string;
  /** True when the caller may not see this asset's columns. */
  restricted?: boolean;
  /** Set only when `restricted` — the one asset-derived value the
   *  placeholder carries, so it can say whose work it is. */
  owner_display_name?: string;
  title?: string;
  summary?: string;
  score: number;
  created_at?: string;
  updated_at?: string;
  owner_user_ref?: number;
  origin_server_id?: string;
  /** The per-type card payload. Absent on a restricted hit, by
   *  construction on the server — its absence is the withholding. */
  extra?: Record<string, unknown>;
}

/** The post shape PostCard consumes. Structural, not imported: PostCard
 *  declares its props inline, and a hit is not an openapi Post. */
export interface SearchPostRow {
  id: string;
  title: string;
  cover_asset_id?: string | null;
  created_at: string;
  like_count: number;
  comment_count: number;
  members: Array<{
    asset_id: string;
    asset?: CardCoverAsset;
    restricted?: boolean;
    owner_display_name?: string;
  }>;
}

/** The collection shape CollectionCard consumes. */
export interface SearchCollectionRow {
  id: string;
  name: string;
  description: string;
  visibility: string;
  owner_user_ref: number;
  created_at: string;
}

/** Reads one key out of a hit's `extra` bag with a typed default. */
function extraOf(h: SearchHit): Record<string, unknown> {
  return (h.extra ?? {}) as Record<string, unknown>;
}
function str(v: unknown): string | null {
  return typeof v === 'string' && v !== '' ? v : null;
}
function num(v: unknown): number | null {
  return typeof v === 'number' ? v : null;
}
function bool(v: unknown): boolean {
  return v === true;
}

/**
 * An asset hit as AssetCard's row.
 *
 * `created_at` falls back to the epoch on a restricted hit rather than to
 * "now": the card formats it for a footer the restricted plate does not
 * render, and a value that changes on every render would make the tile
 * non-deterministic in a screenshot diff.
 */
export function hitAsCardAsset(h: SearchHit): CardAsset {
  if (h.restricted) {
    return {
      id: h.id,
      title: '',
      asset_type: 0,
      created_at: new Date(0).toISOString(),
      file_hash: null,
      file_extension: null,
      thumbhash: null,
      preview_available: false,
      ladder_available: false,
      scrub_available: false,
      pixel_width: null,
      pixel_height: null,
      restricted: true,
      owner_display_name: h.owner_display_name ?? null,
    };
  }
  const e = extraOf(h);
  return {
    id: h.id,
    title: h.title ?? '',
    asset_type: num(e.asset_type) ?? 0,
    created_at: h.created_at ?? new Date(0).toISOString(),
    file_hash: str(e.file_hash),
    file_extension: str(e.file_extension),
    thumbhash: str(e.thumbhash),
    preview_available: bool(e.preview_available),
    ladder_available: bool(e.ladder_available),
    scrub_available: bool(e.scrub_available),
    pixel_width: num(e.pixel_width),
    pixel_height: num(e.pixel_height),
    restricted: false,
    // Off the HIT, not out of `extra` — the search response carries it
    // as a top-level field on every non-restricted row. Copied because
    // the card's edit affordance reads it (#549); dropping it here is
    // the #595 shape exactly (the value is on the wire, the hand-map
    // forgets it, and the tile silently loses a feature with no type
    // error), which is why the restricted branch above omits it
    // DELIBERATELY and this one does not omit it at all.
    owner_user_ref: h.owner_user_ref ?? null,
  };
}

/**
 * A post hit as PostCard's row.
 *
 * The payload ships ONE member — the cover — plus the true `member_count`
 * beside it, because a tile shows one image and the engine will not join
 * a whole membership per hit for pixels nobody renders. PostCard takes
 * the count as a prop for exactly this case; see `hitMemberCount`.
 */
export function hitAsPost(h: SearchHit): SearchPostRow {
  const e = extraOf(h);
  const rawMembers = Array.isArray(e.members) ? (e.members as Record<string, unknown>[]) : [];
  return {
    id: h.id,
    title: h.title ?? '',
    cover_asset_id: str(e.cover_asset_id),
    created_at: h.created_at ?? new Date(0).toISOString(),
    like_count: num(e.like_count) ?? 0,
    comment_count: num(e.comment_count) ?? 0,
    members: rawMembers.map((m) => {
      const assetId = str(m.asset_id) ?? '';
      if (m.restricted === true) {
        // The #883 member placeholder — `asset` is absent, not blanked.
        return {
          asset_id: assetId,
          restricted: true,
          owner_display_name: str(m.owner_display_name) ?? undefined,
        };
      }
      const a = (m.asset ?? {}) as Record<string, unknown>;
      const asset: CardCoverAsset = {
        id: str(a.id) ?? assetId,
        file_hash: str(a.file_hash),
        file_extension: str(a.file_extension),
        thumbhash: str(a.thumbhash),
        preview_available: bool(a.preview_available),
        ladder_available: bool(a.ladder_available),
        scrub_available: bool(a.scrub_available),
        pixel_width: num(a.pixel_width),
        pixel_height: num(a.pixel_height),
      };
      return { asset_id: assetId, asset };
    }),
  };
}

/** The true size of a post hit's membership, for the multi-asset badge. */
export function hitMemberCount(h: SearchHit): number {
  return num(extraOf(h).member_count) ?? 0;
}

/** A collection hit as CollectionCard's row. */
export function hitAsCollection(h: SearchHit): SearchCollectionRow {
  const e = extraOf(h);
  return {
    id: h.id,
    name: h.title ?? '',
    description: h.summary ?? '',
    visibility: str(e.visibility) ?? 'private',
    owner_user_ref: h.owner_user_ref ?? 0,
    created_at: h.created_at ?? new Date(0).toISOString(),
  };
}
