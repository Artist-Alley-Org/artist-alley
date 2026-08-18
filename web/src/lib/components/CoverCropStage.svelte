<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // One crop marquee, parameterised by DESTINATION SHAPE (#1207).
  //
  // #1207 shipped this for the featured rail's 890:500 card. The owner
  // then asked for the same control on the regular collection cover,
  // locked to a square. The two differ in exactly one number, so this is
  // one component rendered twice rather than a second marquee to keep in
  // step — and "in step" is not a tidiness argument here: the marquee,
  // the dimming, the drag arithmetic and the live preview all have to
  // agree with what the browser's own `object-fit: cover` does, and a
  // divergence between two copies would be invisible until someone
  // compared the editor against the surface it claims to predict.
  //
  // WHY THE SHAPE IS LOCKED AND THERE IS NO FREE-CROP TOGGLE. A crop is
  // only meaningful against a destination that renders it, and both
  // destinations are fixed by something other than preference: the rail
  // card is locked to 890:500 (#1110/#1098), and the collection cover's
  // square is the `col` rendition itself — `fit: cover` at 320px, a
  // 320x320 centre-crop (sysconfig/previews.go), which every small
  // collection thumbnail is made of. An arbitrary rectangle has no
  // surface that would honour it, so a toggle would offer a choice the
  // product cannot keep.

  import { t } from '$stores/lang.svelte';
  import {
    cropWindow,
    focalFromOrigin,
    hasTravel,
    marqueeOrigin,
    objectPosition,
  } from '$lib/util/featuredCrop';

  interface Props {
    /** The picture to crop. Null renders nothing — the caller decides
     *  what an empty slot says. */
    src: string | null;
    srcset?: string;
    sizes?: string;
    /** The DESTINATION aspect: 890/500 for the rail card, 1 for the
     *  collection cover's square. */
    aspect: number;
    /** The stored focal pair, null for centre. Bound both ways. */
    focalX: number | null;
    focalY: number | null;
    /** Prefixes every data-testid, so two instances on one screen are
     *  addressable apart. */
    testidPrefix: string;
    stageAlt: string;
    cardAlt: string;
    cardLabel: string;
    /** How tall the stage may get, in vh. Defaults to the wide card's
     *  52. A SQUARE destination wants less: the stage is as tall as it
     *  is wide, so the same cap makes the collection slot twice the
     *  height of the featured one and pushes its own picker off the
     *  bottom of the dialog — the cramped-surface complaint, reappearing
     *  one slot down. */
    maxHeightVh?: number;
    /** Extra controls beside Reset — the featured slot puts its "go back
     *  to the collection cover" button here. */
    extraActions?: import('svelte').Snippet;
  }

  let {
    src,
    srcset,
    sizes,
    aspect,
    focalX = $bindable(),
    focalY = $bindable(),
    testidPrefix,
    stageAlt,
    cardAlt,
    cardLabel,
    maxHeightVh = 52,
    extraActions,
  }: Props = $props();

  // ── The picture's own proportions, STAMPED WITH ITS SOURCE ─────────
  //
  // The stamp is what makes the marquee safe rather than merely usually
  // right (#1195's note, kept). A plain {w,h} cleared by an effect on
  // the selection has an ordering hazard: a picture already in the
  // browser cache fires `load` synchronously enough to beat the effect,
  // which then clears the value it just produced. Keying on the source
  // and deriving "is this measurement about the current picture?"
  // removes the question instead of timing it.
  let natural = $state<{ src: string; w: number; h: number } | null>(null);
  const naturalNow = $derived(natural && natural.src === src ? natural : null);

  /** The picture did not load, stamped for the same reason.
   *
   *  Found by driving this rather than reasoning about it: a picture
   *  chosen seconds after it was uploaded has no rendition yet, because
   *  renditions are produced asynchronously. "Still being made" is a
   *  wait, and it is not the same message as "nothing to move". */
  let failedSrc = $state<string | null>(null);
  const isPending = $derived(failedSrc !== null && failedSrc === src);

  function onLoad(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    if (src && img.naturalWidth > 0 && img.naturalHeight > 0) {
      natural = { src, w: img.naturalWidth, h: img.naturalHeight };
      failedSrc = null;
    }
  }
  function onError() {
    if (src) failedSrc = src;
  }

  const win = $derived(naturalNow ? cropWindow(naturalNow.w / naturalNow.h, aspect) : null);
  /** Which axis the curator can actually move on. Exactly one, ever —
   *  `object-fit: cover` trims one axis and shows the other whole. */
  const canMoveX = $derived(win !== null && hasTravel(win.w));
  const canMoveY = $derived(win !== null && hasTravel(win.h));
  const canMove = $derived(canMoveX || canMoveY);

  const fx = $derived(focalX ?? 0.5);
  const fy = $derived(focalY ?? 0.5);

  const marquee = $derived(
    win === null
      ? null
      : {
          left: marqueeOrigin(fx, win.w) * 100,
          top: marqueeOrigin(fy, win.h) * 100,
          width: win.w * 100,
          height: win.h * 100,
        },
  );

  const previewPosition = $derived(objectPosition(focalX, focalY));

  // ── Dragging ───────────────────────────────────────────────────────
  //
  // Pointer events, not mouse events: one code path covers mouse, touch
  // and pen, which is what makes the marquee work at 390px on a coarse
  // pointer without a second handler to keep in step.
  //
  // `setPointerCapture` is what makes a drag survive the pointer leaving
  // the picture — and the curator dragging to an edge is not an edge
  // case here, it is how you frame the left of a wide photograph.
  let stage = $state<HTMLDivElement | null>(null);
  let dragging = $state(false);
  /** Where in the marquee the pointer grabbed, as a fraction of the
   *  PICTURE. Kept so the marquee moves WITH the pointer rather than
   *  jumping its centre to it — a jump on pointerdown is how a
   *  positioning control loses the position it was showing. */
  let grabOffset = { x: 0, y: 0 };

  function onPointerDown(e: PointerEvent) {
    const rect = stage?.getBoundingClientRect();
    if (!rect || !win || !canMove) return;
    e.preventDefault();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    dragging = true;
    grabOffset = {
      x: (e.clientX - rect.left) / rect.width - marqueeOrigin(fx, win.w),
      y: (e.clientY - rect.top) / rect.height - marqueeOrigin(fy, win.h),
    };
  }

  function onPointerMove(e: PointerEvent) {
    if (!dragging) return;
    const rect = stage?.getBoundingClientRect();
    if (!rect || !win) return;
    e.preventDefault();
    // Both axes are written on every move even though only one can
    // travel: focalFromOrigin answers 0.5 for a pinned axis, so the
    // pinned half stays neutral instead of accumulating pointer noise,
    // and the PAIR stays a pair — which is what the column CHECK and the
    // API's both-or-neither rule require.
    setFocal(
      focalFromOrigin((e.clientX - rect.left) / rect.width - grabOffset.x, win.w),
      focalFromOrigin((e.clientY - rect.top) / rect.height - grabOffset.y, win.h),
    );
  }

  function endDrag(e: PointerEvent) {
    if (!dragging) return;
    dragging = false;
    (e.currentTarget as HTMLElement).releasePointerCapture?.(e.pointerId);
  }

  function setFocal(x: number, y: number) {
    focalX = x;
    focalY = y;
  }

  /** Keyboard nudge — the same control, reachable without a pointer.
   *
   *  A step of 2% of the TRAVEL rather than of the picture, so the
   *  number of presses it takes to cross the range is the same whatever
   *  the picture's proportions. Arrow keys are claimed because the
   *  dialog body scrolls, and an unclaimed ArrowDown would scroll the
   *  page out from under the control being used. */
  function onKeydown(e: KeyboardEvent) {
    if (!win || !canMove) return;
    const step = e.shiftKey ? 0.1 : 0.02;
    let x = fx;
    let y = fy;
    switch (e.key) {
      case 'ArrowLeft':
        x -= step;
        break;
      case 'ArrowRight':
        x += step;
        break;
      case 'ArrowUp':
        y -= step;
        break;
      case 'ArrowDown':
        y += step;
        break;
      case 'Home':
        x = canMoveX ? 0 : x;
        y = canMoveY ? 0 : y;
        break;
      case 'End':
        x = canMoveX ? 1 : x;
        y = canMoveY ? 1 : y;
        break;
      default:
        return;
    }
    e.preventDefault();
    setFocal(canMoveX ? clamp01(x) : 0.5, canMoveY ? clamp01(y) : 0.5);
  }

  function clamp01(v: number) {
    return v < 0 ? 0 : v > 1 ? 1 : v;
  }

  /** Back to centre — a CLEAR, not a re-set to 0.5. Null and 0.5 render
   *  identically and are stored differently on purpose (see the
   *  migration): this is what makes "the curator never positioned this"
   *  recoverable. */
  function resetFocal() {
    focalX = null;
    focalY = null;
  }

  const positionLabel = $derived(
    t('collections.cover_editor_position_value', {
      x: String(Math.round(fx * 100)),
      y: String(Math.round(fy * 100)),
    }),
  );
</script>

{#if src}
  <div class="grid gap-4 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
    <!-- LEFT: the whole picture at size, with the marquee on it. -->
    <figure class="min-w-0">
      <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
        {t('collections.cover_editor_stage_label')}
      </figcaption>
      <!-- The image carries a DEFINITE WIDTH with `height: auto`, and
           both halves are load-bearing — arrived at by driving it and
           getting the other two wrong first.

           `max-height`/`max-width` alone never UPSCALES, so a cover with
           no preview ladder rendered at the 320px `col` rendition inside
           a 1500px dialog: the original complaint, in the case #1074
           made ordinary. A definite HEIGHT with `max-width` is worse —
           when the max binds the browser honours both and SQUASHES the
           picture, and the marquee is computed from
           naturalWidth/naturalHeight, so it then marks a region of a
           shape that is not on screen. A definite width with
           `height: auto` and a `max-height` cannot distort: the auto
           axis always follows the aspect.

           The wrapper stays SHRINK-WRAPPED around the image, which is
           what lets the marquee be positioned in percentages with
           nothing measured from the DOM. Giving the wrapper a size of
           its own puts the overlay over the BOX instead of over the
           picture — the exact bug this surface exists to reveal. -->
      <div class="flex justify-center rounded border border-border bg-surface p-2">
        <div bind:this={stage} class="relative inline-block max-w-full align-top">
          <img
            {src}
            {srcset}
            {sizes}
            alt={stageAlt}
            onload={onLoad}
            onerror={onError}
            data-testid="{testidPrefix}-stage-image"
            class="block select-none rounded"
            draggable="false"
            style="width: clamp(17rem, 40vw, 38rem); height: auto; max-height: {maxHeightVh}vh;"
          />
          {#if marquee}
            <!-- What gets cropped OFF is dimmed and what survives is
                 outlined: two readings of one fact, so it holds up for a
                 colour-blind reader and in a 390px screenshot alike.
                 FOUR bars rather than #1195's two, because a marquee
                 that has MOVED is off centre and the two offcuts are no
                 longer equal — the symmetric pair was only ever correct
                 for a centred crop. `pointer-events-none` because they
                 are annotation; the drag belongs to the marquee. -->
            <div class="pointer-events-none absolute inset-x-0 top-0 bg-black/60"
                 style="height: {marquee.top}%"></div>
            <div class="pointer-events-none absolute inset-x-0 bottom-0 bg-black/60"
                 style="height: {100 - marquee.top - marquee.height}%"></div>
            <div class="pointer-events-none absolute left-0 bg-black/60"
                 style="top: {marquee.top}%; height: {marquee.height}%; width: {marquee.left}%"></div>
            <div class="pointer-events-none absolute right-0 bg-black/60"
                 style="top: {marquee.top}%; height: {marquee.height}%; width: {100 - marquee.left - marquee.width}%"></div>

            <!-- The marquee is a BUTTON: focusable, announced, and
                 obviously interactive without inventing a role for a
                 two-axis positioner that ARIA does not have one for. Its
                 accessible name carries the current position, so a
                 screen-reader user gets the feedback a sighted one gets
                 from watching it move.

                 `touch-action: none` is what makes the drag work on a
                 phone: without it the browser claims the gesture for
                 scrolling and the marquee never sees a pointermove. -->
            <button
              type="button"
              data-testid="{testidPrefix}-marquee"
              aria-label={t('collections.cover_editor_marquee_label')}
              aria-describedby="{testidPrefix}-position"
              disabled={!canMove}
              onpointerdown={onPointerDown}
              onpointermove={onPointerMove}
              onpointerup={endDrag}
              onpointercancel={endDrag}
              onkeydown={onKeydown}
              class="absolute border-2 border-accent focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring {canMove
                ? dragging
                  ? 'cursor-grabbing'
                  : 'cursor-grab'
                : 'cursor-default'}"
              style="left: {marquee.left}%; top: {marquee.top}%; width: {marquee.width}%; height: {marquee.height}%; touch-action: none;"
            ></button>
          {/if}
        </div>
      </div>
      <!-- THREE STATES, and the middle one is the point of this block.
           "Nothing to move" is a statement about a picture's
           PROPORTIONS, so it must not be printed before the proportions
           are known — which is what happened the first time this was
           driven in a browser with a freshly uploaded cover whose
           rendition did not exist yet: the editor confidently announced
           that a 2.4:1 picture was already card-shaped, about an image
           it had failed to load. -->
      <p
        id="{testidPrefix}-position"
        class="mt-2 text-xs text-fg-muted"
        data-testid="{testidPrefix}-position"
        data-focal-x={focalX === null ? '' : String(focalX)}
        data-focal-y={focalY === null ? '' : String(focalY)}
      >
        {#if isPending}
          {t('collections.cover_editor_stage_pending')}
        {:else if win === null}
          {t('collections.crop_no_dimensions')}
        {:else if !canMove}
          {t('collections.cover_editor_no_travel')}
        {:else}
          {t('collections.cover_editor_drag_hint')} — {positionLabel}
        {/if}
      </p>
    </figure>

    <!-- RIGHT: the destination itself, drawn the way the destination
         draws it. Not a simulation of the crop: the same three CSS
         properties — the aspect box, `object-cover`, and
         `object-position` from the same helper the consumers use — on
         the same source. If the two ever disagree it is because the
         destination changed. -->
    <figure class="min-w-0">
      <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
        {cardLabel}
      </figcaption>
      <!-- Bounded by the same HEIGHT budget as the stage, expressed as
           a max-WIDTH because width is the axis a block-level box takes
           from its column. Without it a SQUARE destination fills the
           grid column and becomes as tall as it is wide — 590px on a
           1080p screen — which pushes its own picker off the bottom of
           the dialog, i.e. the cramped-surface complaint reappearing one
           slot down. The wide card is unaffected: 52vh x 1.78 is wider
           than any column it sits in. -->
      <div
        class="overflow-hidden rounded-lg border border-border bg-surface-elevated"
        style="aspect-ratio: {aspect}; max-width: calc({maxHeightVh}vh * {aspect});"
      >
        <img
          {src}
          {srcset}
          {sizes}
          alt={cardAlt}
          data-testid="{testidPrefix}-card-preview"
          class="h-full w-full object-cover"
          style="object-position: {previewPosition}"
        />
      </div>
      <div class="mt-2 flex flex-wrap items-center gap-2">
        <button
          type="button"
          onclick={resetFocal}
          disabled={focalX === null && focalY === null}
          data-testid="{testidPrefix}-reset-focal"
          class="rounded border border-border px-2 py-1 text-xs hover:bg-surface disabled:cursor-not-allowed disabled:opacity-40"
        >
          {t('collections.cover_editor_reset')}
        </button>
        {@render extraActions?.()}
      </div>
    </figure>
  </div>
{/if}
