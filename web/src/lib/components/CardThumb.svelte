<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Shared grid-thumbnail frame for AssetCard + PostCard (#515 slice 1).
  //
  // Both cards previously carried a byte-identical copy of this block;
  // #511 shared the grid (ContentGrid/TileGrid) but the cards were still
  // forked. This is the one thumbnail treatment every asset-showing
  // surface renders — browse + profile + post-by-asset (via PostCard) and
  // the profile asset section + collections + asset detail (via
  // AssetCard) — so the gallery presentation lands everywhere at once.
  //
  // Gallery-matte thumbnail pattern: the artwork is
  // LETTERBOXED on a neutral matte inside a framed panel — never cropped —
  // so mixed-aspect art reads like a gallery wall. The previous treatment
  // painted the thumbhash as a `bg-cover` backdrop, so a contained image
  // sat on a blurred, zoomed CROP of itself; here the matte is a clean
  // neutral and the thumbhash is only the contained loading placeholder,
  // fading out once the real bytes arrive.
  //
  // Rendering layers (unchanged from the forked originals):
  //   1. Thumbhash placeholder — ~30-byte data URI decoded inline,
  //      shown contained until the col variant loads. No HTTP RTT.
  //   2. The col-sized JPEG variant, object-contain on the matte.
  //   3. Fallbacks: the no-preview plate (CardFallback, #558) whenever
  //      there is no servable variant AND no thumbhash, which covers
  //      text/code assets and failed derivatives alike; sprite-scrub
  //      hover preview for video/3D over the real image.
  //
  // preview_available gating (#471) is preserved: the <img> renders only
  // when the server confirms a servable `col` for THIS caller, so gated /
  // not-yet-generated / preview-less assets fire NO byte request that
  // would 404 (keeps the zero-console-404 tiles shipped in v0.6.0).

  import type { Snippet } from 'svelte';
  import { onMount } from 'svelte';
  import { decodeThumbhash } from '$lib/util/thumbhash';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { DEFAULT_TILE_SIZES } from '$stores/browseView.svelte';
  import { clampRatio, MASONRY_MIN_TILE_REM } from './cardAsset';
  import { isDocExt } from './viewers/controller';
  import {
    loadSpriteCues,
    cueBackgroundStyle,
    type SpriteCue,
  } from '$lib/util/spriteCues';
  import CardFallback from './CardFallback.svelte';
  import CardRestricted from './CardRestricted.svelte';

  interface Props {
    /** Asset whose variants back the thumbnail (the cover asset for a
     *  Post). Null → placeholder only. */
    assetId: string | null;
    /** Alt text + used by the caller's overlay. */
    title: string;
    thumbhash?: string | null;
    fileExtension?: string | null;
    /** Asset-type ref, for the no-preview plate's kind lookup (#558):
     *  a PNG uploaded as a sprite atlas is a sprite sheet, and the
     *  extension alone cannot say so. Only read when there is no
     *  preview to show.
     *
     *  PostCard deliberately passes nothing: its tile is a COVER asset
     *  (CardCoverAsset, #595), which carries only the fields the tile
     *  reads, and the one kind asset_type changes — a sprite atlas — is
     *  a raster that always has a preview and so never reaches the
     *  plate. Widen the contract if that stops being true. */
    assetType?: number | null;
    hasFileHash?: boolean;
    previewAvailable?: boolean;
    /** Every CONFIGURED rung exists for this asset (#610). Licenses the
     *  responsive srcset below; false → `col` only, exactly as before. */
    ladderAvailable?: boolean;
    /** A `sprites.vtt` hover-scrub cue file exists for this asset AND
     *  the caller may read it (#835). This is the ONLY licence to
     *  request the sheet — the scrub used to be gated on the file
     *  extension, which is a guess about storage made from a filename
     *  and 404s whenever the render has not drained yet. */
    scrubAvailable?: boolean;
    /** Slot width for `sizes`. The caller knows the layout (tile rung,
     *  feed column, masonry column); this component only knows it is a
     *  square-ish box. Defaults to the tile ladder's default rung.
     *
     *  A `sizes` LIST, not a single length — see browseView.tileSizes
     *  for what belongs in it and why. It leads with `auto`, which only
     *  works because the <img> below is `loading="lazy"`; see there
     *  before changing either. */
    sizesHint?: string;
    /** Card hover state, from the parent's interactive `<a>` (keeps the
     *  hover listeners on an interactive element, not this presentation
     *  frame). Drives the video/3D sprite-scrub animation. */
    hovering?: boolean;
    /** Draw the gallery-mount frame ring (#515 slice 4). ON in the
     *  "details" modes (thumbnail / masonry / feed); OFF in grid, which
     *  reads as a clean dense wall. The bg-surface matte stays either way
     *  so art always letterboxes and never crops (slice 1's value). */
    framed?: boolean;
    /** Fill the tile edge-to-edge instead of letterboxing (#561). ON in
     *  grid only — a contact sheet fills, a details view shows the whole
     *  work. This DELIBERATELY reverses slice 1's "letterbox, never crop"
     *  for that one mode: the inset matte ring left visible whitespace
     *  inside every tile, and a dense wall of art should butt edge to
     *  edge rather than sit in a grid of individually-padded boxes.
     *
     *  Applies to the real image variant ONLY. The typed-doc card, icon
     *  placeholder and thumbhash placeholder are GENERATED, not artwork —
     *  cropping them would clip a glyph or a file extension for no gain,
     *  so they stay centred on the matte in every mode. */
    fill?: boolean;
    /** Let the tile take the SHAPE OF ITS IMAGE instead of a square
     *  (#640). ON in masonry only — that layout exists to pack tiles of
     *  different heights, and with a fixed `aspect-square` it was a
     *  multi-column grid of identical boxes, which is what a masonry is
     *  defined by not being.
     *
     *  Deliberately not the default, and deliberately not derived from
     *  `mode` inside this component: grid is a contact sheet of squares
     *  by design (#555/#588) and thumbnail is a framed details card
     *  (#556). Both are correct as squares. See `tileRatio` below for
     *  where the ratio comes from. */
    variableAspect?: boolean;
    /** A floor on the variable-aspect ratio — the TALLEST the tile may
     *  get, expressed as width/height (#557). Only read when
     *  `variableAspect` is on.
     *
     *  Masonry passes nothing and wants nothing: a column is ~270px, so
     *  even a 1:4 portrait lands about a thousand pixels tall and that
     *  is what a masonry is for. The FEED card is the case that needs
     *  it — one column at a 46rem measure, where the same 1:4 image is
     *  nearly 3000px and the reader scrolls past a single post for
     *  three screens. Every social feed worth copying caps portrait at
     *  4:5 for exactly this reason.
     *
     *  It LETTERBOXES rather than crops, which is slice 1's rule
     *  everywhere `fill` is off: the art stays whole on the matte. The
     *  alternative — clipping to the cap — is what Instagram does and
     *  what this codebase deliberately does not, outside grid's contact
     *  sheet.
     *
     *  ⚠️ Not for a caller whose ratio is PREDICTED elsewhere.
     *  MasonryColumns buckets by `cardTileRatio` one layer up, and a
     *  floor applied only in CSS would desynchronise the columns (#651
     *  / #652). The feed is one column and predicts nothing, so it is
     *  safe there and would not be in masonry. */
    ratioFloor?: number | null;
    /** The tile may be only as tall as the control floor (#652) — set
     *  in masonry, where a 5.33:1 waveform lands at ~60px. Strips the
     *  chrome that cannot survive at that size to leave exactly the two
     *  controls the owner asked to keep (checkbox + ⋮ menu); everything
     *  else moves into the hover tooltip.
     *
     *  Separate from `variableAspect` even though both are set by the
     *  same mode: one is about SHAPE and one is about how much chrome
     *  fits. A future caller could reasonably want a variable-aspect
     *  tile with full chrome, and reading the mode in here is what
     *  #640 deliberately avoided. */
    compact?: boolean;
    /** A CSS colour to paint the letterbox matte with, instead of the
     *  neutral `bg-thumb-matte` token (#1136).
     *
     *  Thumbnail view passes a colour SAMPLED FROM THE IMAGE (see
     *  `thumbhashMatteColor`), which is the reference panel's own
     *  treatment and the detail the prior-art notes flagged as worth
     *  taking: on a shelf of mixed-aspect work it is the difference
     *  between a wall of grey rectangles containing pictures and a wall
     *  that reads as the pictures.
     *
     *  ⚠️ It paints the FRAME only, never the fallback plates inside it.
     *  The typed-doc card, the icon placeholder and the restricted plate
     *  are GENERATED artwork with their own designed backgrounds; tinting
     *  those would colour a thing that is not a photograph after a
     *  photograph it is not. Null / undefined ⇒ the neutral, which is
     *  every other mode and every asset with no thumbhash. */
    matteColor?: string | null;
    /** Recorded SOURCE dimensions for this asset, or null (#640). These
     *  are what let `variableAspect` reserve the tile's height before a
     *  single byte is requested — the difference between a wall that is
     *  the right shape at first paint and one that reflows 72 times as
     *  images arrive. Null is normal (see cardAsset.ts). */
    pixelWidth?: number | null;
    pixelHeight?: number | null;
    /** The card already prints the title immediately next to this box —
     *  thumbnail mode's persistent header (#556). Only the no-preview
     *  plate reads it, to avoid printing the same string twice 8px
     *  apart; see CardFallback. */
    titleAdjacent?: boolean;
    /** The caller may not see this member (#883). The server sent a
     *  placeholder — no title, no extension, no asset id, no thumbhash —
     *  so the tile states the restriction instead of rendering a thing
     *  it was not given. Takes priority over every other branch. */
    restricted?: boolean;
    /** The asset owner's display name, the only asset-derived value a
     *  restricted placeholder carries. Null when unresolvable. */
    restrictedOwnerName?: string | null;
    /** Offer "request access" on the restricted plate (#881), against
     *  this asset id. Null (the default) is a plate with no ask.
     *
     *  Separate from `assetId` on purpose. `assetId` is "what would this
     *  tile show bytes for", and it is deliberately never dereferenced
     *  on a restricted tile; this is "what is the viewer asking about".
     *  A PostCard passes a cover id as the former and nothing as the
     *  latter — see CardRestricted's prop note. */
    requestAssetId?: string | null;
    /** Card-specific chrome stacked over the thumb (multi-asset badge,
     *  hover title overlay, future tool row / checkbox). Rendered inside
     *  the same positioned frame so absolute overlays anchor to it. */
    children?: Snippet;
  }

  let {
    assetId,
    title,
    thumbhash = null,
    fileExtension = null,
    assetType = null,
    hasFileHash = false,
    previewAvailable = false,
    ladderAvailable = false,
    scrubAvailable = false,
    sizesHint = DEFAULT_TILE_SIZES,
    hovering = false,
    framed = true,
    fill = false,
    variableAspect = false,
    ratioFloor = null,
    compact = false,
    matteColor = null,
    pixelWidth = null,
    pixelHeight = null,
    titleAdjacent = false,
    restricted = false,
    restrictedOwnerName = null,
    requestAssetId = null,
    children,
  }: Props = $props();

  // A restricted tile never addresses the asset: no col variant, no
  // ladder rung, no sprite sheet. Killing the URL at the source rather
  // than relying on the render branch means an added branch cannot
  // reintroduce the request.
  const colUrl = $derived(assetId && !restricted ? `/api/v1/assets/${assetId}/variants/col` : '');

  // Responsive source set (#502/#589). Three conditions, all required:
  //
  //   ladderAvailable  the server confirms every configured rung exists
  //                    for THIS asset — without it, requesting anything
  //                    but `col` is the 404 class #471 removed
  //   !fill            grid's `fill` mode wants the SQUARE CROP. A
  //                    contact sheet is supposed to be a uniform wall,
  //                    so `col` is correct there and this deliberately
  //                    does not touch it (#561)
  //   rungs present    the install's ladder, read from GET /previews —
  //                    never hardcoded, or an operator who tuned their
  //                    rungs gets 404s (#610's trap, client side)
  //
  // When any fails, `srcset` stays empty and the <img> renders from
  // colUrl exactly as it did before this change.
  onMount(() => previewLadder.init());
  const srcset = $derived(
    ladderAvailable && !fill && assetId && !restricted
      ? (previewLadder.srcsetFor(assetId) ?? '')
      : '',
  );
  // `src` is the fallback for a browser that ignores srcset, and the
  // thing the loader uses before it picks a candidate. The smallest
  // CONTAIN rung, not col: mixing a square crop into a contain slot
  // would flash the wrong shape before swapping.
  const imgSrc = $derived.by(() => {
    if (!srcset) return colUrl;
    const smallest = previewLadder.smallestKey();
    return smallest ? `/api/v1/assets/${assetId}/variants/${smallest}` : colUrl;
  });

  // Decoded thumbhash → data URI. Lazy (post-mount) so the SSR snapshot
  // stays light; re-decode if the source asset changes.
  let placeholder = $state<string | null>(null);
  onMount(() => {
    placeholder = decodeThumbhash(thumbhash);
  });
  $effect(() => {
    void thumbhash;
    placeholder = decodeThumbhash(thumbhash);
  });

  const isDoc = $derived(isDocExt(fileExtension));

  // Render the <img> only when the server confirms a servable col for
  // this caller (preview_available, #471) and the asset actually has
  // bytes. Doc assets get a typed card instead of a raster preview.
  const showImage = $derived(!isDoc && !!assetId && !!hasFileHash && !!previewAvailable);

  let imgLoaded = $state(false);
  let imgError = $state(false);
  // The ratio measured off the bytes that actually arrived. Only used
  // when the server had nothing recorded — see tileRatio.
  let loadedRatio = $state<number | null>(null);
  $effect(() => {
    // Reset the fade-in whenever the target asset changes.
    void assetId;
    imgLoaded = false;
    imgError = false;
    loadedRatio = null;
  });
  function onLoad(e: Event) {
    imgLoaded = true;
    const el = e.currentTarget as HTMLImageElement | null;
    if (el && el.naturalWidth > 0 && el.naturalHeight > 0) {
      loadedRatio = el.naturalWidth / el.naturalHeight;
    }
  }
  // Defensive only: preview_available guarantees a servable col, so this
  // fires only on undecodable bytes — degrade to the icon placeholder.
  function onError() {
    imgError = true;
  }

  // ── Tile shape (#640) ────────────────────────────────────────────
  //
  // THE TILE FOLLOWS THE RATIO OF THE IMAGE IT ACTUALLY RENDERS. That
  // sentence is the whole rule, and the two clauses are both load-
  // bearing.
  //
  // "follows the ratio" — masonry is a layout for tiles of unequal
  // height. Until now the wrapper was hard-coded `aspect-square` with
  // the art letterboxed inside, so a 5.33:1 ultrawide and a 1:1 square
  // rendered as identical boxes and the mode was a grid wearing a
  // masonry's name (#640). Everywhere else the square is deliberate, so
  // this is opt-in per caller.
  //
  // "it actually renders" — the picture in the tile is not always the
  // source image. Without a full ladder the card can only request `col`,
  // which is a 320x320 centre-CROP (#471/#591), and sizing that tile
  // from the source's 5.33:1 would letterbox a square inside a
  // billboard. So the recorded dimensions are used only when the
  // responsive `srcset` is live, i.e. when the contain rungs — which do
  // preserve the source ratio — are what will be served.
  //
  // Resolution order, best information first:
  //
  //   1. recorded pixel_width/pixel_height — known BEFORE any request,
  //      so the space is reserved and nothing shifts on load. This is
  //      the reason #640 waited for #618's extraction fields to exist.
  //      Since #757 the preview pipeline records the shape of the image
  //      the contain rungs were built from, for EVERY format it can
  //      render — so an audio waveform (5.33:1), a video poster and a
  //      font sheet (16:9) all land here, not just EXIF-bearing rasters.
  //      Before that, nothing wrote these fields at all and every tile
  //      fell to case 3.
  //   2. the loaded image's own naturalWidth/naturalHeight — exact, but
  //      only knowable after the bytes arrive, so tiles that land here
  //      DO settle into shape as they load. That is a deliberate trade
  //      against the alternative, which is being confidently square and
  //      wrong. What still lands here is an asset whose preview predates
  //      #757 and has not been re-rendered since (see
  //      `aa rebuild-previews`).
  //   3. square — no image in the tile at all (typed-doc card, icon
  //      placeholder, gated/thumbhash-only). There is no ratio to
  //      follow, and a square is what those generated tiles are drawn
  //      for.
  //
  // The clamp is a guard against bad metadata, not a design choice: a
  // corrupt 4000:1 would compute a sub-pixel tile the user can neither
  // see nor click. It lives in cardAsset.ts now (#651) because the
  // masonry column bucketer has to predict this exact number one layer
  // up — see `cardTileRatio` there, which mirrors `declaredRatio` below
  // including the `srcset` precondition.
  const declaredRatio = $derived(
    srcset && pixelWidth && pixelHeight && pixelWidth > 0 && pixelHeight > 0
      ? clampRatio(pixelWidth / pixelHeight)
      : null,
  );
  const measuredRatio = $derived(loadedRatio === null ? null : clampRatio(loadedRatio));
  // `ratioFloor` caps how TALL the tile may get (see the prop). Applied
  // after the ratio resolves rather than inside clampRatio, because
  // clampRatio's bounds are a guard against corrupt metadata and this is
  // a per-caller layout decision — a 1:4 portrait is not bad data, it is
  // just too tall for one 46rem column.
  const tileRatio = $derived.by(() => {
    if (!variableAspect) return null;
    const r = declaredRatio ?? measuredRatio;
    if (r === null) return null;
    return ratioFloor && ratioFloor > 0 ? Math.max(r, ratioFloor) : r;
  });

  // The tile floor (#652). Applied to every variable-aspect tile, not
  // only the ones currently under it: the ratio can change under us
  // (declared → measured on load, or a resize), and a floor that has to
  // be re-decided is a floor that will be missed. Above it the
  // `aspect-ratio` is unaffected, so #646 holds everywhere it shows.
  //
  // The two rules — this and MasonryColumns' height prediction — read
  // the same constant from cardAsset.ts. Do not inline the number here:
  // a CSS-only clamp that the bucketer does not predict desynchronises
  // the columns and brings back the append instability #651 removed.
  //
  // ⚠️ `width: 100%` is LOAD-BEARING, not tidying. `aspect-ratio` plus
  // `min-height` on a block whose width is `auto` makes the engine
  // re-derive the INLINE size from the ratio once the floor clamps the
  // height: measured in Chromium, a 5.33:1 tile in a 269px column came
  // out 320x60 — 51px wider than the card it sits in. The card is
  // `overflow-hidden`, so the artwork was silently cropped on the right
  // and the ⋮ menu (inset from the FRAME's right edge, now 51px past
  // the card's) was clipped away entirely. A probe that measures the
  // controls against the frame sees nothing wrong, because the frame
  // moved with them; measure against the CARD.
  //
  // Making the width definite pins it: the ratio then only ever decides
  // the height, and the floor only ever raises it.
  const frameStyle = $derived.by(() => {
    const parts: string[] = [];
    if (tileRatio) parts.push(`aspect-ratio: ${tileRatio}`);
    if (variableAspect) parts.push(`min-height: ${MASONRY_MIN_TILE_REM}rem`, 'width: 100%');
    return parts.length > 0 ? `${parts.join('; ')};` : undefined;
  });

  // ── Reduced motion (#837) ────────────────────────────────────────
  //
  // WITH THE PREFERENCE SET, THE SCRUB NEVER MOUNTS — THE POSTER STAYS.
  //
  // The obvious reading of "show a single representative frame instead
  // of cycling" is to freeze the scrub on its first cue. Measured, that
  // is the worse of the two options and by a wide margin: cue 0 is the
  // clip's OPENING frame, and films open on black. On the seeded Sintel
  // 1080p the frozen first cue renders an entirely black tile, while the
  // still underneath it is a legible snow shot.
  //
  // The still is already the right answer. The poster the preview
  // pipeline picks (#818/#829) is chosen to be a representative frame —
  // that is its whole job — so "a single representative frame" is what
  // the card is showing before the pointer ever arrives. Suppressing the
  // scrub leaves it there.
  //
  // So this is NOT a hover that does nothing. It is a hover that does
  // not ANIMATE, over an image that was always there, and it costs a
  // reduced-motion visitor neither the sheet download nor the cue fetch —
  // both are gated on the same flag below.
  //
  // Reactive rather than a one-shot read at mount. The setting can be
  // toggled while the page is open (OS accessibility panel, devtools
  // emulation), and a wall of cards that keeps animating until the next
  // reload is precisely the experience the preference exists to prevent.
  // `matchMedia` is optional-chained for the SSR/jsdom case, where it is
  // absent and no-motion is the right default anyway.
  let reducedMotion = $state(false);
  onMount(() => {
    const mq = window.matchMedia?.('(prefers-reduced-motion: reduce)');
    if (!mq) return;
    reducedMotion = mq.matches;
    const onChange = (e: MediaQueryListEvent) => {
      reducedMotion = e.matches;
    };
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  });

  // ── Sprite-sheet hover preview (#835) ────────────────────────────
  //
  // THE CUE FILE DRIVES EVERYTHING. Frame count, cell geometry, and
  // whether there is a scrub at all now come from `sprites.vtt`, the
  // file the renderer writes next to the sheet. See spriteCues.ts for
  // what that fixes; the short version is that the previous code
  // hardcoded a 10x10 (video) or 6x6 (3D) grid keyed off the file
  // EXTENSION and cycled every cell of it, so a clip too short to fill
  // the grid hovered through ffmpeg's black padding, and a format that
  // was not on the list could not scrub even with a sheet in storage.
  //
  // The gate is `scrubAvailable`, a server flag (#835), not a fetch that
  // might 404 and not the extension. Extension is a guess about storage
  // made from a filename, and it is wrong in both directions: a video
  // whose expensive render has not drained yet has a card (the cheap
  // poster job, #818) and no sheet, and an animated GIF has had a sheet
  // since #832 and was never on the list.
  const spriteUrl = $derived(assetId ? `/api/v1/assets/${assetId}/variants/sprites.jpg` : '');

  let cues = $state<SpriteCue[]>([]);
  let spriteFrame = $state(0);
  // The sheet's own pixel size, measured off the bytes we are about to
  // paint. Needed because a cue's rect is in SHEET pixels and CSS
  // background percentages are relative to the whole image; measuring
  // beats deriving it from the cue list, which is truncated for a short
  // clip and so cannot report the sheet's real height.
  let sheetSize = $state<{ w: number; h: number } | null>(null);

  // Load both halves in parallel on first hover. The cue list is cached
  // per asset for the session; the sheet is an ordinary browser image
  // cache hit on every hover after the first.
  $effect(() => {
    if (!hovering || !scrubAvailable || !assetId || !spriteUrl || reducedMotion || restricted)
      return;
    let live = true;
    void loadSpriteCues(assetId).then((c) => {
      if (live) cues = c;
    });
    const img = new Image();
    img.onload = () => {
      if (live && img.naturalWidth > 0 && img.naturalHeight > 0) {
        sheetSize = { w: img.naturalWidth, h: img.naturalHeight };
      }
    };
    img.src = spriteUrl;
    return () => {
      live = false;
      img.onload = null;
    };
  });

  // The scrub layer paints only once both halves have landed. Painting
  // early would mean guessing the sheet's scale for a frame or two,
  // which reads as a zoom jump; the sheet has to be fetched before
  // anything is visible anyway, so there is nothing to lose by waiting
  // for the measurement it arrives with.
  const spriteCue = $derived(cues.length > 0 ? cues[spriteFrame % cues.length] : null);
  //
  // `!reducedMotion` is the #837 gate. Belt and braces with the fetch
  // guard above — that one stops the bytes, this one stops a layer
  // painting from a cue list already cached from a hover taken before
  // the preference was turned on.
  const showScrub = $derived(
    !!hovering && !!scrubAvailable && !!spriteCue && !!sheetSize && !reducedMotion,
  );
  const spriteStyle = $derived(
    spriteCue && sheetSize ? cueBackgroundStyle(spriteCue, sheetSize.w, sheetSize.h) : null,
  );
  // The cell's OWN aspect ratio, straight off the cue rect (#761). Not
  // the sheet's: those agree only while the grid is square, and the cue
  // states the cell directly, so there is nothing to infer. A landscape
  // cell is width-bound and centred vertically; a portrait one is
  // height-bound and pillarboxed. Recorded pixel dims are deliberately
  // not used — they are the coded frame size, which is wrong for a
  // rotated phone clip, the same trap the backend avoids.
  const spriteCellRatio = $derived(spriteCue ? spriteCue.w / spriteCue.h : 16 / 9);

  // Run the scrub only while the card is hovered. The effect owns the
  // interval so it is torn down on unhover / unmount.
  $effect(() => {
    if (!hovering || cues.length === 0 || reducedMotion) {
      spriteFrame = 0;
      return;
    }
    const n = cues.length;
    const iv = setInterval(() => {
      spriteFrame = (spriteFrame + 1) % n;
    }, 120);
    return () => clearInterval(iv);
  });
</script>

<!--
  RS matte — `bg-thumb-matte`, a dedicated token offset a few L points
  from the PAGE in both themes (#590 amendment). It used to be
  `bg-surface`, i.e. the page colour itself, which meant a light-artwork
  tile in light mode had nothing separating it from the page. Always on,
  so mixed-aspect art letterboxes cleanly instead of a blurred self-crop.

  The `after:` inset ring is now drawn in EVERY mode, not just `framed`.
  Two different bleeds needed covering:

    * grid — since #588 the art is object-cover with no padding, so it
      reaches the tile edge and the matte is never visible. Only a
      boundary line can delimit a white-artwork tile against a near-white
      page. #515 slice 4 dropped this ring from grid for a "clean dense
      wall"; that read fine while tiles were letterboxed, and stopped
      reading once they filled.
    * contain modes — the matte offset above does most of the work; the
      ring finishes the edge.

  The ring is TRANSLUCENT and theme-directional — black in light, white
  in dark — so it always contrasts with the page while staying invisible
  over artwork that already contrasts. A solid colour cannot do this:
  the library holds both near-white and near-black assets, so any fixed
  line disappears against one of them. (`dark:` now follows our theme
  class, not the OS — see @custom-variant in app.css.)
-->
<!--
  The inner radius is CONCENTRIC with the tile's outer one (#596): the
  grid tile is 4px rounded with a 2px inset, so the image inside wants
  4 - 2 = 2px to curve on the same centre. Square corners inside a
  rounded box read as a mistake at this size. `rounded-[2px]` and not
  `rounded-sm`, because the reference works in px and Tailwind's scale
  is rem — see AssetCard's wrapperClass. Applied with `fill`, which is
  set exactly in grid mode by both cards; the framed modes keep square
  corners inside their own rounded card.
-->
<!--
  `aspect-square` is the DEFAULT, not the rule (#640). When the caller
  asked for a variable tile and a ratio is known, an inline
  `aspect-ratio` overrides it — inline because the value is per-asset
  data, not a design token, exactly as ContentGrid sets `--tile-min`.
  The class stays in the list so the tile is square while the ratio is
  still unknown (before an undeclared image loads), which is both a
  sensible reservation and the shape every mode had before this change.

  `data-card-thumb` marks THE element whose height a masonry column
  stacks. It is the tile's identity, not a style hook, so both the unit
  tests and the layout measurements address it by this rather than by a
  Tailwind class that a refactor is free to rename.
-->
<div
  data-card-thumb
  style={matteColor ? `${frameStyle ?? ''} background-color: ${matteColor};` : frameStyle}
  class="relative overflow-hidden {matteColor ? '' : 'bg-thumb-matte'}
         {tileRatio ? '' : 'aspect-square'}
         after:pointer-events-none after:absolute after:inset-0 after:ring-1 after:ring-inset
         {fill ? 'rounded-[2px] after:rounded-[2px]' : ''}
         {framed
           ? 'after:ring-black/[0.07] dark:after:ring-white/[0.06]'
           : 'after:ring-black/[0.12] dark:after:ring-white/[0.10]'}"
>
  {#if restricted}
    <!-- #883 — the caller may not see this member. FIRST branch, before
         anything that reads a title, an extension or an asset id: the
         server sends none of those for a restricted member, and putting
         this check anywhere below would mean the branches above are
         trusted to have been handed nothing. Nothing here requests
         bytes. -->
    <CardRestricted ownerName={restrictedOwnerName} assetId={requestAssetId} />
  {:else if isDoc}
    <!-- Text/code assets get no rasterised preview variant at all, so
         the plate IS their tile rather than a fallback from one (#558). -->
    <CardFallback {title} {fileExtension} {assetType} {titleAdjacent} />
  {:else if showImage && !imgError}
    <!-- Thumbhash loading placeholder — contained (not bg-cover) so it
         sits where the real art will, blurred, and fades out on load
         rather than bleeding a cropped backdrop around the letterbox. -->
    {#if placeholder}
      <div
        class="absolute inset-0 bg-center bg-no-repeat transition-opacity duration-200"
        style="background-image: url({placeholder}); background-size: contain; filter: blur(6px); transform: scale(1.03);"
        class:opacity-0={imgLoaded}
        aria-hidden="true"
      ></div>
    {/if}
    <!--
      grid (fill): object-cover with NO padding, so the tile is filled
      edge-to-edge. The `col` variant is itself a 320×320 centre-cropped
      square (sysconfig DefaultPreviewConfig: Fit=cover, MaxDim=320 —
      verified against the stored bytes), and the tile is square, so
      "cover" here is a 1:1 display of the variant: no second crop, no
      upscale beyond what `contain` was already doing. It just removes the
      6px matte ring that `p-1.5` drew inside every tile.

      everything else (contain + p-1.5): letterbox on the matte, so a
      details view still shows the whole work (#515 slice 1).

      `loading="lazy"` is ALSO what makes `sizes: auto` work (#639). Per
      spec `auto` resolves to 100vw on an eagerly-loaded image, and the
      rest of the sizes list is not consulted — measured. Making this
      eager would turn the slot hint into "the whole viewport" on every
      card, with no other visible symptom.

      #1047 — a VARIABLE-ASPECT tile drops the `p-1.5` matte inset too.
      Not for symmetry with `fill`: the inset exists to letterbox art on
      a matte when the box and the picture are different shapes, and a
      variable-aspect tile IS the shape of its picture, so the padding
      can only shrink the art and draw a frame around it. On masonry —
      "maximum art per page" — that was 6px of chrome per side on every
      tile of the wall. The one case where the shapes still disagree is
      a tile clamped by the #652 floor or by `ratioFloor`, and there the
      art letterboxes onto the matte exactly as before, just without the
      extra ring.
    -->
    <img
      src={imgSrc}
      srcset={srcset || undefined}
      sizes={srcset ? sizesHint : undefined}
      alt={title}
      loading="lazy"
      decoding="async"
      class="absolute inset-0 h-full w-full transition-opacity duration-200 group-hover:scale-[1.02]
             {fill ? 'object-cover' : variableAspect ? 'object-contain' : 'object-contain p-1.5'}"
      class:opacity-0={!imgLoaded}
      class:opacity-100={imgLoaded}
      onload={onLoad}
      onerror={onError}
    />
    {#if showScrub && spriteStyle}
      <!--
        THE SCRUB USES THE SAME FIT AS THE STILL IT REPLACES (#834).
        That is the whole rule, and `fill` — the caller's "this tile is a
        contact sheet, bleed to the edges" flag — is the one input, so
        the two layers cannot drift apart the way they had.

        What #834 actually was: NOT a wrongly-shaped still. Measured on
        the browse grid, a 1920x818 video's still is the `col` rung
        (320x320) painted `object-cover` into a 367x367 tile — an exact
        fill, no band anywhere. The band was THIS layer. It letterboxed
        the 2.35:1 cue cell to `w-full` inside a SQUARE tile and backed it
        with `bg-black/95`, so hovering swapped a full-bleed frame for a
        160px strip with 109px of opaque black above and below it — 57%
        of the tile. The report described that black as belonging to the
        still because hovering is how you look closely at a card.

        So the two states disagreed, and the still was the one that was
        right for grid: a contact sheet fills (#561/#588).

        Fit, per mode:

          fill (grid) — COVER. The cell is bound on its SHORT axis
            against the square tile and overflows the long one, which the
            frame's `overflow-hidden` clips. Landscape binds height,
            portrait binds width — the mirror of the contain branch, not
            a special case. Centred, so it crops to the same middle
            square that `col` (itself a centred cover crop of the same
            poster) already shows: the still and the scrub then frame the
            IDENTICAL region and the hover no longer jumps.

          everything else — CONTAIN, unchanged from #761/#835: a
            landscape cell is width-bound, a portrait one height-bound,
            so a rotated phone clip is never cropped. Masonry sizes its
            tile from the same shape, so the box is usually invisible
            there; where it does show (a rotated clip, whose coded dims
            and display cell disagree) it now shows the MATTE the still
            letterboxes onto rather than black, because "agree with the
            still" applies to the backdrop too.

        `shrink-0` is load-bearing on the cover branch, not tidying: the
        cell is a FLEX ITEM that deliberately overflows its container, and
        a flex item's default `flex-shrink: 1` licenses the engine to
        squeeze it back to fit — which would silently restore the
        letterbox by squashing the frame instead of boxing it. Chromium
        happens to derive the width from the ratio and leave it alone;
        that is not something to depend on.
      -->
      <div
        class="pointer-events-none absolute inset-0 flex items-center justify-center overflow-hidden
               transition-opacity duration-150 {fill ? '' : 'bg-thumb-matte'}"
      >
        <div
          class="bg-no-repeat shrink-0 {fill
            ? spriteCellRatio >= 1
              ? 'h-full'
              : 'w-full'
            : spriteCellRatio >= 1
              ? 'w-full'
              : 'h-full'}"
          style="aspect-ratio: {spriteCellRatio}; background-image: url({spriteUrl}); background-size: {spriteStyle.size}; background-position: {spriteStyle.position};"
        ></div>
      </div>
    {/if}
    <!-- The media-type chip that used to live here is gone (#1047). It
         was two hardcoded English words — `video` and `3D` — covering
         two of thirteen ViewKinds, so a PDF, an audiobook and a sprite
         sheet were all unlabelled, and #1111 had already replaced it
         with an icon in grid. The replacement is CardKindBadge, drawn by
         the CARD in the children slot: the caller knows the density and
         therefore the corner, and CardThumb is handed a presentation
         rather than inferring one. -->
  {:else if !placeholder}
    <!-- No servable preview AND no thumbhash — nothing of the asset can
         be shown, so the plate states what it is instead (#558). This
         used to be a 48px landscape icon at 40% opacity, identical for a
         failed 3D turntable, a failed JPEG derivative and a gated asset:
         a tile that says "image missing" about a CAD model is worse than
         one that says nothing. -->
    <CardFallback {title} {fileExtension} {assetType} {titleAdjacent} />
  {:else}
    <!-- Gated / not-yet-generated / preview-less but thumbhash present:
         show the blurred thumbhash contained, no byte request (no 404). -->
    <div
      class="absolute inset-0 bg-center bg-no-repeat"
      style="background-image: url({placeholder}); background-size: contain; filter: blur(6px); transform: scale(1.03);"
      aria-hidden="true"
    ></div>
  {/if}

  {@render children?.()}
</div>
