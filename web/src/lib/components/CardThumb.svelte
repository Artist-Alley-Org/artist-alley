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
  // AssetCard) — so the RS-style presentation lands everywhere at once.
  //
  // RS thumbnail pattern (pages/search_views/thumbs.php): the artwork is
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
  //   3. Fallbacks: typed-doc card (no raster preview), sprite-scrub
  //      hover preview for video/3D, icon placeholder otherwise.
  //
  // preview_available gating (#471) is preserved: the <img> renders only
  // when the server confirms a servable `col` for THIS caller, so gated /
  // not-yet-generated / preview-less assets fire NO byte request that
  // would 404 (keeps the zero-console-404 tiles shipped in v0.6.0).

  import type { Snippet } from 'svelte';
  import { onMount } from 'svelte';
  import { decodeThumbhash } from '$lib/util/thumbhash';
  import { isVideoExt, is3DExt, isDocExt } from './viewers/controller';

  interface Props {
    /** Asset whose variants back the thumbnail (the cover asset for a
     *  Post). Null → placeholder only. */
    assetId: string | null;
    /** Alt text + used by the caller's overlay. */
    title: string;
    thumbhash?: string | null;
    fileExtension?: string | null;
    hasFileHash?: boolean;
    previewAvailable?: boolean;
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
    hasFileHash = false,
    previewAvailable = false,
    hovering = false,
    framed = true,
    fill = false,
    children,
  }: Props = $props();

  const colUrl = $derived(assetId ? `/api/v1/assets/${assetId}/variants/col` : '');

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
  const isVideo = $derived(isVideoExt(fileExtension));
  const is3D = $derived(is3DExt(fileExtension));

  // Render the <img> only when the server confirms a servable col for
  // this caller (preview_available, #471) and the asset actually has
  // bytes. Doc assets get a typed card instead of a raster preview.
  const showImage = $derived(!isDoc && !!assetId && !!hasFileHash && !!previewAvailable);

  let imgLoaded = $state(false);
  let imgError = $state(false);
  $effect(() => {
    // Reset the fade-in whenever the target asset changes.
    void assetId;
    imgLoaded = false;
    imgError = false;
  });
  function onLoad() {
    imgLoaded = true;
  }
  // Defensive only: preview_available guarantees a servable col, so this
  // fires only on undecodable bytes — degrade to the icon placeholder.
  function onError() {
    imgError = true;
  }

  // Sprite-sheet hover preview. Video covers walk the preview.video 10×10
  // timeline sheet; 3D covers walk the preview.model 6×6 turntable sheet.
  // Both serve from the same sprites.jpg variant.
  const hasSpriteScrub = $derived(isVideo || is3D);
  const spriteUrl = $derived(assetId ? `/api/v1/assets/${assetId}/variants/sprites.jpg` : '');
  const spriteCols = $derived(is3D ? 6 : 10);
  const spriteRows = $derived(is3D ? 6 : 10);
  const spriteCells = $derived(spriteCols * spriteRows);
  let spriteFrame = $state(0);
  // Run the sprite turntable only while the card is hovered. The effect
  // owns the interval so it's torn down on unhover / unmount.
  $effect(() => {
    if (!hovering || !hasSpriteScrub) {
      spriteFrame = 0;
      return;
    }
    const iv = setInterval(() => {
      spriteFrame = (spriteFrame + 1) % spriteCells;
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
<div
  class="relative aspect-square overflow-hidden bg-thumb-matte
         after:pointer-events-none after:absolute after:inset-0 after:ring-1 after:ring-inset
         {fill ? 'rounded-[2px] after:rounded-[2px]' : ''}
         {framed
           ? 'after:ring-black/[0.07] dark:after:ring-white/[0.06]'
           : 'after:ring-black/[0.12] dark:after:ring-white/[0.10]'}"
>
  {#if isDoc}
    <!-- Typed doc card — text/code don't get a rasterised preview
         variant, so render a file-shape with the extension. -->
    <div class="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-gradient-to-br from-surface-elevated to-surface text-fg-muted/80">
      <svg xmlns="http://www.w3.org/2000/svg" width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round">
        <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
        <polyline points="14 2 14 8 20 8" />
        <line x1="8" y1="13" x2="16" y2="13" />
        <line x1="8" y1="17" x2="16" y2="17" />
        <line x1="8" y1="9" x2="12" y2="9" />
      </svg>
      {#if fileExtension}
        <span class="rounded bg-black/40 px-2 py-0.5 font-mono text-[10px] uppercase tracking-wider text-fg">
          {fileExtension.replace(/^\./, '')}
        </span>
      {/if}
    </div>
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
    -->
    <img
      src={colUrl}
      alt={title}
      loading="lazy"
      decoding="async"
      class="absolute inset-0 h-full w-full transition-opacity duration-200 group-hover:scale-[1.02]
             {fill ? 'object-cover' : 'object-contain p-1.5'}"
      class:opacity-0={!imgLoaded}
      class:opacity-100={imgLoaded}
      onload={onLoad}
      onerror={onError}
    />
    {#if hasSpriteScrub && hovering}
      {#if isVideo}
        <!-- Video scrub: 16:9 sprite cells letterboxed in the 1:1 slot on
             a black backdrop, so the cell renders at native ratio. -->
        <div class="pointer-events-none absolute inset-0 bg-black/95 transition-opacity duration-150">
          <div
            class="absolute left-0 right-0 top-1/2 aspect-video -translate-y-1/2 bg-no-repeat"
            style="background-image: url({spriteUrl}); background-size: {spriteCols * 100}% {spriteRows * 100}%; background-position: {(spriteFrame % spriteCols) * (100 / (spriteCols - 1))}% {Math.floor(spriteFrame / spriteCols) * (100 / (spriteRows - 1))}%;"
          ></div>
        </div>
      {:else}
        <!-- 3D turntable: 1:1 cells in the 1:1 slot — no letterbox. -->
        <div
          class="pointer-events-none absolute inset-0 bg-cover bg-no-repeat transition-opacity duration-150"
          style="background-image: url({spriteUrl}); background-size: {spriteCols * 100}% {spriteRows * 100}%; background-position: {(spriteFrame % spriteCols) * (100 / (spriteCols - 1))}% {Math.floor(spriteFrame / spriteCols) * (100 / (spriteRows - 1))}%;"
        ></div>
      {/if}
    {/if}
    {#if isVideo}
      <div class="pointer-events-none absolute left-2 top-2 inline-flex items-center gap-1 rounded-full bg-black/65 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm">
        <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="currentColor"><polygon points="6 4 20 12 6 20 6 4" /></svg>
        video
      </div>
    {:else if is3D}
      <div class="pointer-events-none absolute left-2 top-2 inline-flex items-center gap-1 rounded-full bg-black/65 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm">
        <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" /><polyline points="3.27 6.96 12 12.01 20.73 6.96" /><line x1="12" y1="22.08" x2="12" y2="12" /></svg>
        3D
      </div>
    {/if}
  {:else if !placeholder}
    <!-- No thumbhash either — fall back to the icon. -->
    <div class="absolute inset-0 flex items-center justify-center text-fg-muted/40">
      <svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
        <rect x="3" y="3" width="18" height="18" rx="2" />
        <circle cx="9" cy="9" r="2" />
        <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
      </svg>
    </div>
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
