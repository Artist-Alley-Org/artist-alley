// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * The kind → icon map (#1111).
 *
 * One place that answers "which glyph stands for this kind of asset",
 * so a surface that needs the answer imports it rather than drawing a
 * second set. `ViewKind` is the vocabulary — the same enum the viewer
 * router and CardFallback resolve from `kindForAsset` — so a new kind
 * added there fails to compile here until someone picks its icon,
 * which is the point of typing the record exhaustively.
 *
 * # Deep imports only (#1111, owner decision 2026-08-15)
 *
 * Every icon is imported by its own path — `@lucide/svelte/icons/x` —
 * and NEVER from the package root. The barrel re-exports all ~1,600
 * icons; in Vite's dev server that is 1,600 modules pulled into the
 * graph the first time any component asks for one glyph, and the page
 * that pays for it is whichever one loaded first. The production build
 * tree-shakes either way, so the cost is invisible in CI and lands
 * entirely on contributors. See CONTRIBUTING.md.
 *
 * # Why `@lucide/svelte` and not `lucide-svelte`
 *
 * `lucide-svelte` is DEPRECATED upstream ("Please use @lucide/svelte
 * instead", npm, checked 2026-08-15) and its last publish predates the
 * runes-era Svelte 5 line; it declares `svelte: ^3 || ^4 ||
 * ^5.0.0-next.42`. `@lucide/svelte` declares `svelte: ^5` and is the
 * maintained package. The two are not interchangeable and the old name
 * is the one most search results still show, which is why this note
 * exists rather than just the import.
 *
 * # Existing inline glyphs stay
 *
 * CardFallback's GLYPHS map and ViewControls' `gallery-thumbnails` are
 * NOT migrated here. That is the owner's explicit call: opportunistic
 * migration when those components are next touched, no migration
 * sprint. Do not read the overlap as an oversight.
 */

import type { Component } from 'svelte';
import type { ViewKind } from './viewers/controller';

import Image from '@lucide/svelte/icons/image';
import Video from '@lucide/svelte/icons/video';
import FileText from '@lucide/svelte/icons/file-text';
import AudioLines from '@lucide/svelte/icons/audio-lines';
import Film from '@lucide/svelte/icons/film';
import Type from '@lucide/svelte/icons/type';
import Grid2x2 from '@lucide/svelte/icons/grid-2x2';
import Box from '@lucide/svelte/icons/box';
import BookOpen from '@lucide/svelte/icons/book-open';
import Headphones from '@lucide/svelte/icons/headphones';
import Archive from '@lucide/svelte/icons/archive';
import File from '@lucide/svelte/icons/file';
import Shapes from '@lucide/svelte/icons/shapes';

/** The multi-asset glyph (#1111). A post holding more than one asset
 *  shows THIS rather than any one member's kind, with the count to its
 *  LEFT — the set is the fact, and picking one member's icon to stand
 *  for a mixed bundle would state something untrue about the other
 *  members. */
export const MultiAssetIcon = Shapes;

/**
 * `Component` rather than the package's own per-icon type: every lucide
 * icon shares one props shape, and naming the union here would tie this
 * map to an internal type path the package is free to move.
 */
export const KIND_ICON: Record<ViewKind, Component> = {
  image: Image,
  video: Video,
  // A page of text is a page of text whether it came from a .docx or a
  // .pdf; lucide's `file-type`/`file-code` family distinguishes formats,
  // and the format already has its own home — CardFallback prints the
  // extension as a wordmark. This icon answers "what kind of thing",
  // which for both of these is "a document".
  pdf: FileText,
  doc: FileText,
  audio: AudioLines,
  // `film`, not `images`: a sequence is frames in time, which is what
  // separates it from a folder of pictures.
  sequence: Film,
  font: Type,
  // `grid-2x2` reads as an atlas — cells on a sheet — which is exactly
  // what a sprite sheet is.
  sprite: Grid2x2,
  '3d': Box,
  ebook: BookOpen,
  audiobook: Headphones,
  archive: Archive,
  // The kind resolver's own "I could not tell" answer. A blank page is
  // the honest glyph for it; anything more specific would be a guess
  // rendered as a statement.
  placeholder: File,
};

/** The icon for one kind, never undefined — an unknown string (a kind
 *  arriving from an older server, say) falls back to the same glyph the
 *  resolver's own `placeholder` uses. */
export function iconForKind(kind: ViewKind): Component {
  return KIND_ICON[kind] ?? File;
}

/**
 * The kinds the browse footer's type filter offers (#1166).
 *
 * DERIVED from KIND_ICON rather than listed again, which is the whole
 * point of that map being exhaustive over `ViewKind`: adding a kind to
 * the union puts it in the filter automatically, and the compiler
 * already forced someone to pick its glyph. The label comes from the
 * same place the badge's accessible name does —
 * `card.fallback.kind.<kind>` — so the checkbox and the badge it
 * selects for are never named differently.
 *
 * Two subtractions, both because the filter selects on a COVER ASSET's
 * kind and neither of these can be one:
 *
 *   `sequence`  no single asset resolves to it — `kindForAsset` never
 *               returns it, so its checkbox could only ever produce an
 *               empty wall.
 *   `placeholder` the resolver's "I could not tell" answer, and a filter
 *               is a question about what something IS. Its label is
 *               "File", which as a checkbox reads like a catch-all it is
 *               not. A post whose cover lands there is still reachable —
 *               all-checked means no filter, so it is on the unfiltered
 *               wall like everything else.
 *
 * The order is KIND_ICON's declaration order, which groups the visual
 * kinds first and is the order the checkbox list renders in.
 */
const NOT_FILTERABLE: ReadonlySet<ViewKind> = new Set<ViewKind>(['sequence', 'placeholder']);

export const FILTERABLE_KINDS: readonly ViewKind[] = (
  Object.keys(KIND_ICON) as ViewKind[]
).filter((k) => !NOT_FILTERABLE.has(k));
