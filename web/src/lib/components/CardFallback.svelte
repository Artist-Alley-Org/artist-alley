<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The no-preview tile (#558).
  //
  // WHAT IT REPLACES. CardThumb had two unrelated fallbacks and they
  // were both anonymous:
  //
  //   * the "typed doc card" — a generic file glyph with the extension
  //     on a black chip, shown for every text/code asset;
  //   * the icon placeholder — a 48px landscape glyph at 40% opacity,
  //     shown for ANYTHING else with no `col` variant and no thumbhash.
  //
  // The second is the one that reads as broken: a failed 3D turntable
  // and a failed JPEG derivative render the identical grey landscape
  // icon, so the tile says nothing about what the asset is, and says it
  // in the visual language of a missing image. Both are now this
  // component, which states the two facts it actually has — the format
  // and the kind — as type rather than as an icon.
  //
  // WHAT IT IS DESIGNED FOR. Measured on dev, 1004 of 1007 assets have a
  // thumbnail; the fallback is 0.3% of tiles, not a wall (#558 comment,
  // 2026-07-27). So this is NOT designed for density or for scanning a
  // grid of identical glyphs — that grid does not exist here. It is
  // designed to look deliberate when one lands between real thumbnails,
  // and to hold up on an install whose catalogue is document- or
  // CAD-heavy, where it is a much larger share of the page than it is
  // for us.
  //
  // WHY IT SIZES ITSELF. `compact` was not enough. A masonry tile can be
  // 60px tall (the control floor, #652) or 700px in the feed column, and
  // the same stack cannot serve both — at 60px a 40px glyph plus three
  // lines of type clips, and at 700px a 12px extension chip is lost. So
  // the plate is a size CONTAINER and each element declares the size it
  // needs, rather than the card mode deciding for it. A caller that
  // invents a new mode gets the right treatment without touching this
  // file, and the 60px floor is verified by the tile actually being
  // 60px, not by a prop that says it is.
  //
  // Three tiers, keyed on the container's own box:
  //   floor  (< 7rem tall)  one line: FORMAT · kind, inset clear of the
  //                         checkbox and ⋮ menu, which are 44px wide at
  //                         left-2 / right-2 and own that band
  //   plate  (≥ 7rem)       the framed glyph, the format wordmark, the
  //                         kind label
  //   full   (≥ 11rem tall  + the title
  //          and ≥ 9rem wide)
  //
  // THE TITLE IS CONDITIONAL, not decorative. In grid / masonry / feed
  // the card shows the title only on hover (or, in masonry, in the
  // cursor tooltip), so a no-preview tile at rest is otherwise
  // unidentifiable — and unlike a painting, a document IS mostly its
  // name. In `thumbnail` mode the card already prints the title in its
  // persistent header directly above this box, so printing it again
  // inside would be the same string twice, 8px apart. `titleAdjacent`
  // is the caller answering "is the title already visible next to me".

  import { kindForAsset, type ViewKind } from './viewers/controller';
  import { t } from '$stores/lang.svelte';

  interface Props {
    /** Asset (or post) title. Rendered only in the `full` tier and only
     *  when `titleAdjacent` is false — see the header note. */
    title: string;
    fileExtension?: string | null;
    /** Asset-type ref. Overrides the extension for the kind lookup the
     *  same way the viewer does — a PNG uploaded as a sprite atlas is a
     *  sprite sheet, not an image. */
    assetType?: number | null;
    /** The card already prints the title next to this box (thumbnail
     *  mode's header). Suppresses the in-plate title. */
    titleAdjacent?: boolean;
  }

  let { title, fileExtension = null, assetType = null, titleAdjacent = false }: Props = $props();

  const kind = $derived<ViewKind>(
    kindForAsset({ asset_type: assetType, file_extension: fileExtension }),
  );

  /** The format wordmark. The extension verbatim, uppercased, dot
   *  stripped — an operator who uploaded `.blend` should read BLEND and
   *  not the kind's generic name. Falls back to the kind when the asset
   *  has no extension at all (federated rows, sourceless drafts). */
  const format = $derived(
    fileExtension
      ? fileExtension.replace(/^\./, '').toUpperCase()
      : t(`card.fallback.kind.${kind}`).toUpperCase(),
  );

  /** The plain-language kind under the wordmark. `GLTF` alone is
   *  cryptic to anyone who is not a 3D artist; `GLTF / 3D model` is not.
   *  Suppressed when it would just restate the wordmark (a `.pdf` under
   *  a PDF heading). */
  const kindLabel = $derived(t(`card.fallback.kind.${kind}`));
  const showKind = $derived(kindLabel.toUpperCase() !== format);

  // Kind glyphs, lucide 24x24 stroke geometry. A map rather than a
  // component per kind: they are one <path> list each and they exist
  // only here.
  const GLYPHS: Record<ViewKind, string[]> = {
    doc: [
      'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z',
      'M14 2v6h6',
      'M8 13h8',
      'M8 17h8',
      'M8 9h2',
    ],
    pdf: [
      'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z',
      'M14 2v6h6',
      'M8 13h8',
      'M8 17h5',
    ],
    image: ['M3 3h18v18H3z', 'M8.5 10a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3', 'm21 15-5-5L5 21'],
    video: ['M3 4h18v16H3z', 'M7 4v16', 'M17 4v16', 'M3 12h18', 'M3 8h4', 'M3 16h4', 'M17 8h4', 'M17 16h4'],
    audio: ['M2 13a2 2 0 0 0 2-2V7a2 2 0 0 1 4 0v13a2 2 0 0 0 4 0V4a2 2 0 0 1 4 0v13a2 2 0 0 0 4 0v-4a2 2 0 0 1 2-2'],
    audiobook: [
      'M3 14h3a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-5a9 9 0 0 1 18 0v5a2 2 0 0 1-2 2h-1a2 2 0 0 1-2-2v-3a2 2 0 0 1 2-2h3',
    ],
    ebook: [
      'M12 7v14',
      'M3 18a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h5a4 4 0 0 1 4 4 4 4 0 0 1 4-4h5a1 1 0 0 1 1 1v13a1 1 0 0 1-1 1h-6a3 3 0 0 0-3 3 3 3 0 0 0-3-3z',
    ],
    font: ['M4 7V4h16v3', 'M9 20h6', 'M12 4v16'],
    '3d': [
      'M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z',
      'm3.3 7 8.7 5 8.7-5',
      'M12 22V12',
    ],
    sprite: ['M3 3h7v7H3z', 'M14 3h7v7h-7z', 'M14 14h7v7h-7z', 'M3 14h7v7H3z'],
    sequence: ['M3 3h7v7H3z', 'M14 3h7v7h-7z', 'M14 14h7v7h-7z', 'M3 14h7v7H3z'],
    archive: ['M3 4h18v5H3z', 'M5 9v10a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9', 'M10 13h4'],
    placeholder: ['M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7z', 'M14 2v6h6'],
  };
  const paths = $derived(GLYPHS[kind] ?? GLYPHS.placeholder);
</script>

<!--
  The plate reads as a thing ON the matte rather than a replacement for
  it: the frame's `bg-thumb-matte` shows through, and the only surface
  treatment is a hairline hatch at ~5% — the drafting convention for
  "deliberately blank", and the cheapest possible signal that nothing
  failed to load here. It is the one flourish; everything else is type.
-->
<div class="plate absolute inset-0 text-fg-muted" data-card-fallback={kind}>
  <div class="stack">
    <span class="glyph" aria-hidden="true">
      <svg
        xmlns="http://www.w3.org/2000/svg"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.3"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        {#each paths as d (d)}<path {d} />{/each}
      </svg>
    </span>
    <span class="format">{format}</span>
    {#if showKind}<span class="kind">{kindLabel}</span>{/if}
    {#if !titleAdjacent}<span class="title">{title}</span>{/if}
  </div>
</div>

<style>
  /* `size`, not `inline-size`: the tier that matters most is decided by
     HEIGHT. A masonry tile at the 60px control floor (#652) is short and
     wide — 60 x ~270 — so a width query would put it in the largest tier
     and clip everything. The box is absolutely positioned to its
     frame's four edges and the frame's height is always definite
     (aspect-ratio, or aspect-square), so size containment has an
     external size to read and cannot collapse. */
  .plate {
    container-type: size;
    container-name: plate;
  }

  /* Hatch. currentColor via the plate's own text colour so it inverts
     with the theme without a second token, and 0.055 because at 0.1 it
     competes with the type and at 0.02 it is not there at all. */
  .plate::before {
    content: '';
    position: absolute;
    inset: 0;
    background-image: repeating-linear-gradient(
      135deg,
      currentColor 0 1px,
      transparent 1px 9px
    );
    opacity: 0.055;
    pointer-events: none;
  }

  .stack {
    position: absolute;
    inset: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    text-align: center;
    /* Floor tier: one row. */
    flex-direction: row;
    gap: 0.5ch;
    /* Clear of the two overlay controls, which are 44px wide at an
       0.5rem inset — 3.25rem of dead band at each end of a short tile.
       Above the floor the stack is vertically centred and passes under
       them anyway, so the inset drops back to something typographic. */
    padding-inline: 3.25rem;
  }

  .glyph {
    display: none;
    color: color-mix(in oklab, currentColor 55%, transparent);
  }
  /* The caps only bite in the feed column (734px), where 21cqmin would
     be a 154px glyph. Every tile mode lands under them, so raising a cap
     changes the feed and nothing else. */
  .glyph svg {
    width: clamp(1.5rem, 21cqmin, 4.5rem);
    height: clamp(1.5rem, 21cqmin, 4.5rem);
  }

  /* The format wordmark is the plate's subject, so it gets the mono face
     (a format name is a token, not prose), the tracking that makes four
     capitals read as a wordmark rather than an abbreviation, and full
     foreground contrast — 17:1 dark / 12.8:1 light on the matte. */
  .format {
    font-family: var(--font-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, 'Liberation Mono', monospace);
    font-size: clamp(0.75rem, 10cqmin, 2.25rem);
    line-height: 1.1;
    letter-spacing: 0.14em;
    /* Tracking adds space AFTER the last glyph too, which shifts a
       centred wordmark left by that much. Pull it back. */
    margin-inline-end: -0.14em;
    color: var(--color-fg);
    white-space: nowrap;
  }

  .kind {
    font-size: clamp(0.6875rem, 4.5cqmin, 0.875rem);
    line-height: 1.2;
    letter-spacing: 0.02em;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Hidden until the full tier, where it switches to `-webkit-box` —
     the only display value `line-clamp` works under. Two lines then
     ellipsis: a plate is an identifier, not a reading surface. */
  .title {
    display: none;
    max-width: 22ch;
    font-size: clamp(0.8125rem, 5cqmin, 1rem);
    line-height: 1.35;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    overflow: hidden;
    overflow-wrap: anywhere;
    transition: opacity 150ms;
  }

  /* On hover the CARD takes over naming itself — grid and feed raise the
     gradient title overlay, masonry raises the cursor tooltip — so the
     plate's copy would be the same string twice at once. It yields.
     `:global` because the `group` class is the card's, one component up;
     Svelte still scopes the `.title` half to this file. */
  :global(.group:hover) .title {
    opacity: 0;
  }

  /* Separator between FORMAT and kind on the one-line floor tier. */
  .kind::before {
    content: '·';
    margin-inline-end: 0.5ch;
  }

  /* ── plate tier — the tile is tall enough for a composed stack. */
  @container plate (min-height: 7rem) {
    .stack {
      flex-direction: column;
      gap: 0.45em;
      padding-inline: 1.25rem;
    }
    .glyph {
      display: block;
      margin-bottom: 0.15em;
    }
    .kind::before {
      content: none;
      margin: 0;
    }
    .kind {
      font-size: clamp(0.6875rem, 3.4cqmin, 0.8125rem);
      letter-spacing: 0.08em;
      text-transform: uppercase;
    }
  }

  /* ── full tier — room for the name as well as the format. */
  @container plate (min-height: 11rem) and (min-width: 9rem) {
    .title {
      display: -webkit-box;
      margin-top: 0.35em;
    }
  }
</style>
