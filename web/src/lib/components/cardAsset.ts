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
  /** A `sprites.vtt` hover-scrub cue file exists for this asset AND the
   *  caller may read it (#835). REQUIRED, like the two flags above: it
   *  is the card's only licence to request the sprite sheet, and a
   *  surface that hand-maps a narrower object and drops it silently
   *  turns off the hover preview — which is exactly how #595 happened
   *  to `file_extension`. */
  scrub_available: boolean;
  /** Recorded source pixel dimensions, or null (#640). Masonry sizes
   *  each tile from this ratio BEFORE the image loads, so the column
   *  heights are right from first paint and the wall doesn't reflow as
   *  72 images arrive. Null for everything the EXIF pass hasn't
   *  measured — draft rasters and every non-raster kind — where the
   *  card falls back to measuring the image on load. */
  pixel_width: number | null;
  pixel_height: number | null;
  /** The viewer may not see this item, and the server sent a placeholder
   *  rather than the row (#883). OPTIONAL, unlike everything above,
   *  because it is a property of MEMBERSHIP: only a container surface (a
   *  collection's contents, a post's members) can produce one. Browse and
   *  the profile grids list assets in their own right and never set it.
   *
   *  When true, none of the fields above carry real values — the API
   *  omitted them — and CardThumb short-circuits to the restricted plate
   *  before reading any of them. */
  restricted?: boolean;
  /** The asset owner's display name: the ONE asset-derived value a
   *  restricted placeholder is allowed to carry. Null/absent when the
   *  server could not resolve one. */
  owner_display_name?: string | null;
  /** Who owns this asset, when the surface knows (#549). Drives the
   *  card's edit affordance and nothing about presentation, which is
   *  why it is OPTIONAL where the four presentation fields above are
   *  required: a hand-mapped row that omits it loses a menu item on
   *  that one surface, not the tile's identity, and `/assets/{id}/edit`
   *  remains reachable and answers the permission question itself.
   *
   *  Nullable as well as optional, because the column is: an asset can
   *  genuinely have no owner (only system.admin may mutate one), and
   *  `null` must never read as "mine". Absent on a `restricted`
   *  placeholder by design — see owner_display_name above; a ref there
   *  would be a second way to ask who holds a withheld row. */
  owner_user_ref?: number | null;
  /** The at-a-glance field values an operator marked `show_on_card`
   *  (#552), already resolved to display strings and in the order the
   *  field definitions declare.
   *
   *  OPTIONAL, like owner_user_ref and unlike the presentation fields
   *  above, and that is the contract rather than an oversight: the flag
   *  is a DISPLAY HINT in `display_order`'s class (ADR 0012), so a
   *  surface that hand-maps a narrower row and drops it renders a
   *  PLAINER card, never a wrong one. The card falls back to its own
   *  default whenever this is absent or empty, which is the same
   *  behaviour it had before the flag existed. Absent on a `restricted`
   *  placeholder by design — see owner_display_name. */
  card_fields?: CardFieldValue[] | null;
  /** The renderable identity behind `owner_user_ref` — the artist block
   *  on a card (#1047). Server-resolved, batched once per page, and the
   *  SAME shape a post carries as `author`.
   *
   *  ABSENT MEANS "NOT DISCLOSED", NOT "NO OWNER", exactly as it does on
   *  a post: the owner set `hide_from_anonymous` and this reader is
   *  anonymous (ADR 0024 / ADR 0070 §3), or the account is gone. The card
   *  then draws NO artist block — never a placeholder identity, because
   *  "someone who opted out owns this" still discloses that they own it.
   *
   *  OPTIONAL like the two hints below it: a surface that hand-maps a
   *  narrower row loses the artist block on that surface and renders a
   *  plainer card, not a wrong one. Absent on a `restricted` placeholder
   *  by design — that row's identity is `owner_display_name`, under its
   *  own narrower allow-list. */
  owner?: CardAuthor | null;
  /** Which peer this asset came from, or absent/null when it is ours
   *  (#552).
   *
   *  The card must answer "whose is this?" without making remote work
   *  look lesser: same tile, same layout, same hints — plus an
   *  attribution line. Making federated content look DIFFERENT is the
   *  wrong reading of the constraint; making it look identical and
   *  UNATTRIBUTED is the other wrong reading. */
  origin?: ContentOrigin | null;
}

/** The renderable identity a card draws — a face, a name, and somewhere
 *  to click, and nothing else (the server's `PostAuthor` allow-list).
 *
 *  ONE TYPE for a post's author and an asset's owner, because they are
 *  one shape resolved by one function (`users.LookupAuthors`) and drawn
 *  by one component (CardAuthorLink). `display_name` is the SERVER's
 *  resolution and is rendered verbatim: the ladder's rung 2 is
 *  authenticated-only, so re-deriving it here would leak real names to
 *  anonymous readers (#1023). */
export interface CardAuthor {
  ref: number;
  username: string;
  display_name: string;
  avatar_url?: string | null;
}

/** One at-a-glance field value on a card. `value` is a display string,
 *  never a stored slug: the server resolves the vocabulary label, so a
 *  tile shows "Pass 1" and not `pass-1`. */
export interface CardFieldValue {
  code: string;
  label: string;
  value: string;
}

/** Attribution for content that came from a federated peer. The name is
 *  the one the operator gave that peer at handshake; a UUID answers
 *  "whose is this?" with nothing a person can read. */
export interface ContentOrigin {
  peer_id: string;
  display_name: string;
  instance_url?: string;
}

/** The asset payload joined into a post member — the same presentation
 *  fields, minus the ones PostCard reads off the post itself (title,
 *  created_at) rather than off the cover asset. */
export interface CardCoverAsset {
  id: string;
  /** The asset-type ref, which OVERRIDES the extension when resolving
   *  what kind of thing this is (#1111).
   *
   *  This was deliberately absent until the grid overlay needed it. The
   *  argument for leaving it out — recorded on CardThumb's `assetType`
   *  prop — was that the only kind `asset_type` changes is a sprite
   *  atlas, which is a raster, always has a preview, and so never
   *  reaches the no-preview plate that was the sole consumer. That note
   *  ends "widen the contract if that stops being true", and #1111 is
   *  where it stops: the overlay labels EVERY grid tile with its kind,
   *  preview or not, so a sprite sheet resolved from `.png` alone would
   *  be captioned "Image" on a wall of them.
   *
   *  Required-but-nullable like its neighbours: null is a real answer
   *  (a row that genuinely has no type), a missing key is a caller that
   *  forgot, and only the first is expressible. */
  asset_type: number | null;
  file_hash: string | null;
  file_extension: string | null;
  thumbhash: string | null;
  preview_available: boolean;
  /** See CardAsset.ladder_available. */
  ladder_available: boolean;
  /** See CardAsset.scrub_available. */
  scrub_available: boolean;
  /** See CardAsset.pixel_width. */
  pixel_width: number | null;
  pixel_height: number | null;
}

// ── Tile shape, shared (#651) ────────────────────────────────────────
//
// CardThumb decides a masonry tile's aspect ratio from the recorded
// dimensions (see its `tileRatio` block for the full argument). The
// column bucketer in MasonryColumns has to predict the SAME number one
// layer up, before the card renders, so it can pick the shortest column.
//
// Both now read the rule from here. Two copies of "when does a tile
// declare a ratio, and what clamp applies" would drift, and a bucketer
// that disagrees with the renderer produces a wall that is balanced
// against heights nothing actually has.

/** Guard against corrupt metadata, not a design choice: a 4000:1 would
 *  compute a sub-pixel tile nobody can see or click. Deliberately wider
 *  than any real content measured on the dev library (8.8:1 / 1:2) —
 *  widen it again rather than distort a tile. */
export const RATIO_MIN = 1 / 12;
export const RATIO_MAX = 12;
export const clampRatio = (r: number): number => Math.min(RATIO_MAX, Math.max(RATIO_MIN, r));

// ── The masonry tile floor (#652) ────────────────────────────────────
//
// #646 gave every masonry tile its TRUE ratio, which is right, and had
// one consequence nobody had priced: an audio waveform is genuinely
// thin. Measured at 1440px on the dev library, the shortest tile was
// 30px tall and 45 of 216 were under 60px — while the two controls that
// have to live inside it are 44x44 each (WCAG 2.5.8). The checkbox and
// the ⋮ menu were literally hanging out of the artwork.
//
// So masonry tiles get a floor, and it is DERIVED rather than picked:
// the controls are `h-11` (2.75rem) inset `top-2 / left-2 / right-2`
// (0.5rem). They are horizontally OPPOSED — checkbox left, menu right —
// so they share one 44px band rather than stacking, and the floor is
// one control plus its inset above and below. 3.75rem, i.e. 60px at a
// 16px root. In rem and not px because the controls are themselves in
// rem: a user at a 20px root gets 55px controls, and a hardcoded 60px
// floor would put them straight back outside the tile.
//
// The cost, taken deliberately (owner's explicit call): the very
// thinnest assets stop being exactly true-to-aspect and letterbox
// inside a slightly taller box. A tile too small to interact with is
// worse than one taller than its content. Every tile ABOVE the floor is
// untouched, so #646 holds everywhere it is visible.
//
// This lives here for the same reason `cardTileRatio` does: CardThumb
// applies the floor in CSS and MasonryColumns has to predict the same
// number when it buckets, one layer up and before the card renders. A
// clamp applied in only one of the two desynchronises the columns and
// reintroduces the append instability #651 removed.

/** Tap-target edge of the overlay controls, in rem (Tailwind `h-11`). */
export const MASONRY_CONTROL_REM = 2.75;
/** Their inset from the tile edge, in rem (Tailwind `top-2`/`left-2`). */
export const MASONRY_CONTROL_INSET_REM = 0.5;
/** The floor a masonry tile may not go under: one control band plus its
 *  inset top and bottom. 3.75rem = 60px at a 16px root. */
export const MASONRY_MIN_TILE_REM = MASONRY_CONTROL_REM + 2 * MASONRY_CONTROL_INSET_REM;

/** `MASONRY_MIN_TILE_REM` resolved against the document's root font
 *  size, for the bucketer's arithmetic. Resolved rather than assumed
 *  16px so the prediction tracks whatever the CSS floor actually
 *  computes to for this user. */
export function masonryMinTilePx(): number {
  const fallback = MASONRY_MIN_TILE_REM * 16;
  if (typeof document === 'undefined' || typeof getComputedStyle !== 'function') return fallback;
  const root = parseFloat(getComputedStyle(document.documentElement).fontSize);
  return root > 0 ? MASONRY_MIN_TILE_REM * root : fallback;
}

/** The height a masonry tile of `ratio` will occupy in a `colWidth`
 *  column, floored at `minPx`. THE one place the floor is applied, so
 *  the renderer's `min-height` and the bucketer's prediction cannot
 *  disagree. `ratio` null ⇒ the tile reserves a square (see
 *  CardThumb's `aspect-square` default). */
export function masonryTileHeight(colWidth: number, ratio: number | null, minPx: number): number {
  const natural = ratio === null || ratio <= 0 ? colWidth : colWidth / ratio;
  return Math.max(natural, minPx);
}

/** The shape a tile's ratio can be read off — an asset row, or the cover
 *  asset joined into a post member. */
interface RatioSource {
  ladder_available?: boolean | null;
  pixel_width?: number | null;
  pixel_height?: number | null;
}

/** The aspect ratio a masonry tile for `item` will DECLARE, or null when
 *  it will render as a square.
 *
 *  Mirrors CardThumb's `declaredRatio` exactly, including the ladder
 *  precondition: without the responsive srcset the card can only request
 *  `col`, a 320x320 centre CROP, so the recorded source ratio is not the
 *  shape that will be on screen. `ladderReady` is the caller's read of
 *  `previewLadder.rungs.length > 0` — passed in rather than imported so
 *  this stays a pure function the tests can drive.
 *
 *  Duck-typed on purpose. ContentGrid's `items` are deliberately loose
 *  (Post / Asset / Collection rows, per surface), and the alternative —
 *  a per-caller ratio callback — would put four copies of this lookup in
 *  four route files, which is exactly the class of drift #595 exists to
 *  stop. */
export function cardTileRatio(item: unknown, ladderReady: boolean): number | null {
  if (!ladderReady || !item || typeof item !== 'object') return null;
  const row = item as Record<string, unknown>;

  let src: RatioSource | null = null;
  if ('pixel_width' in row) {
    // Asset-shaped row (browse-by-asset, profile assets, collection members).
    src = row as RatioSource;
  } else if (Array.isArray(row.members)) {
    // Post-shaped row — the tile shows the COVER asset, same resolution
    // order PostCard uses (explicit cover → first member → nothing).
    const members = row.members as Array<{ asset_id?: string; asset?: RatioSource }>;
    const coverId = (row.cover_asset_id as string | null | undefined) ?? members[0]?.asset_id;
    src = members.find((m) => m.asset_id === coverId)?.asset ?? null;
  }
  if (!src || src.ladder_available !== true) return null;

  const w = src.pixel_width;
  const h = src.pixel_height;
  if (typeof w !== 'number' || typeof h !== 'number' || w <= 0 || h <= 0) return null;
  return clampRatio(w / h);
}
