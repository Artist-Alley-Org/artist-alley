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
    MAX_ZOOM,
    MIN_ZOOM,
    clampZoom,
    coverPlacement,
    cropWindow,
    focalFromOrigin,
    hasTravel,
    marqueeOrigin,
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
    /** The stored zoom, null for the fit (#1212). Bound both ways.
     *  Null rather than 1 so the caller can tell "never framed" from
     *  "framed, and the answer was the fit" — the same distinction the
     *  focal pair keeps, and what makes Reset a clear. */
    zoom: number | null;
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
    zoom = $bindable(),
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

  /** THE STAGE PICTURE'S WIDTH, and it is computed rather than capped.
   *
   *  #1207 wrote `width: clamp(...); height: auto; max-height: Nvh` and
   *  recorded that this combination "cannot distort: the auto axis
   *  always follows the aspect". That premise is false in Chrome for a
   *  replaced element whose width is a specified length: the max-height
   *  binds, the height is clipped to it, and the width is NOT reduced to
   *  match — so a 1:2 portrait was laid out at 608x562, an aspect of
   *  1.08. Measured, not reasoned about; a portrait is exactly the
   *  picture #1212 exists for, and framing a subject by eye against a
   *  picture that is the wrong shape is not framing it.
   *
   *  Capping the WIDTH by the height budget and the aspect removes the
   *  constraint that was doing the damage instead of adding a second one
   *  to fight it. `height: auto` then cannot exceed the budget, because
   *  the width it follows from was chosen so it would not. Both of
   *  #1207's original requirements survive: the picture still UPSCALES
   *  into a large dialog (the clamp is a width, not a max-width, which
   *  is what stopped a 320px `col` rendering at 320px), and the wrapper
   *  still shrink-wraps it so the marquee can be positioned in pure
   *  percentages with nothing measured from the DOM.
   *
   *  Before the picture has loaded there is no aspect to cap by, and the
   *  old clamp is used unchanged — no marquee is drawn at that point, so
   *  there is nothing yet to distort. */
  const stageWidthCSS = $derived(
    naturalNow
      ? `min(clamp(17rem, 40vw, 38rem), calc(${maxHeightVh}vh * ${naturalNow.w / naturalNow.h}))`
      : 'clamp(17rem, 40vw, 38rem)',
  );

  /** The zoom as a usable multiplier — 1 whenever nothing is stored. */
  const z = $derived(clampZoom(zoom));

  const win = $derived(naturalNow ? cropWindow(naturalNow.w / naturalNow.h, aspect, z) : null);
  /** Which axis the curator can actually move on.
   *
   *  AT THE FIT THIS IS EXACTLY ONE AXIS, EVER — `object-fit: cover`
   *  trims one and shows the other whole — and that is the defect #1212
   *  is about: a portrait cover in a wide window is pinned to full
   *  width, so a subject in its left half can never be brought to the
   *  middle. Zooming shrinks the window on BOTH axes, so both start
   *  travelling; nothing here special-cases that, because it falls out
   *  of `cropWindow` taking the zoom. */
  const canMoveX = $derived(win !== null && hasTravel(win.w));
  const canMoveY = $derived(win !== null && hasTravel(win.h));
  const canMove = $derived(canMoveX || canMoveY);
  /** Is there any tightening left? The marquee stays operable while
   *  there is, because its `+`/`-` keys are the a11y twin of the
   *  slider — and a picture with NO travel is precisely the one a
   *  curator reaches for zoom on. */
  const canZoomIn = $derived(win !== null && z < MAX_ZOOM);

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

  /** The destination's own CSS, from the helper every consumer uses —
   *  so this preview cannot drift from the card it predicts. */
  const previewPlacement = $derived(coverPlacement(focalX, focalY, zoom));

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

  /** Every pointer currently down ON THE MARQUEE, by id.
   *
   *  Kept because a pinch is two pointers and `setPointerCapture` gives
   *  each of them its own stream: without a record of the other one
   *  there is no distance to compare, and the gesture cannot be
   *  recognised at all. One pointer is a drag, two are a pinch, and the
   *  transition between them is why `dragging` is cleared rather than
   *  left running — a second finger landing mid-drag must stop moving
   *  the crop, not fight the scale. */
  const pointers = new Map<number, { x: number; y: number }>();
  /** Distance between the two pinch pointers when the gesture began,
   *  and the zoom it began at. Ratios are taken against these rather
   *  than accumulated per move: accumulating multiplies rounding error
   *  by the number of events, and a pinch fires a lot of them. */
  let pinchStart: { dist: number; zoom: number } | null = null;

  /** The one place a zoom is written. Clamped to the ladder-derived
   *  bounds, and stored as a NUMBER — including exactly 1, which is a
   *  deliberate "back to the fit" and is not the same as the null a
   *  Reset writes. */
  function setZoom(v: number) {
    zoom = clampZoom(v);
  }

  function pinchDistance(): number | null {
    if (pointers.size < 2) return null;
    const [a, b] = [...pointers.values()];
    return Math.hypot(a.x - b.x, a.y - b.y);
  }

  function onPointerDown(e: PointerEvent) {
    const rect = stage?.getBoundingClientRect();
    if (!rect || !win) return;
    e.preventDefault();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    const d = pinchDistance();
    if (d !== null) {
      // Second finger: the gesture is a pinch from here.
      dragging = false;
      pinchStart = { dist: d, zoom: z };
      return;
    }
    if (!canMove) return;
    dragging = true;
    grabOffset = {
      x: (e.clientX - rect.left) / rect.width - marqueeOrigin(fx, win.w),
      y: (e.clientY - rect.top) / rect.height - marqueeOrigin(fy, win.h),
    };
  }

  function onPointerMove(e: PointerEvent) {
    if (pointers.has(e.pointerId)) pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (pinchStart) {
      const d = pinchDistance();
      if (d !== null && pinchStart.dist > 0) {
        e.preventDefault();
        setZoom((pinchStart.zoom * d) / pinchStart.dist);
      }
      return;
    }
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
    pointers.delete(e.pointerId);
    if (pointers.size < 2) pinchStart = null;
    if (dragging) dragging = false;
    (e.currentTarget as HTMLElement).releasePointerCapture?.(e.pointerId);
  }

  /** Wheel over the stage zooms (#1212).
   *
   *  MULTIPLICATIVE, not additive: a fixed step feels glacial at 1x and
   *  violent at 4x, because what the eye reads is the RATIO of window
   *  sizes. 1.0015 per deltaY unit gives roughly the same perceived
   *  speed everywhere on the range.
   *
   *  `deltaMode` is honoured because a mouse that reports lines rather
   *  than pixels sends deltas around 3 instead of 100, and treating
   *  them alike makes the wheel do nothing at all on that mouse.
   *
   *  preventDefault stops the dialog scrolling out from under the
   *  picture being framed, which is the same reason the arrow keys are
   *  claimed below. */
  function onWheel(e: WheelEvent) {
    if (!win) return;
    e.preventDefault();
    const perUnit = e.deltaMode === 1 ? 0.05 : e.deltaMode === 2 ? 0.5 : 0.0015;
    setZoom(z * Math.exp(-e.deltaY * perUnit));
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
    if (!win) return;
    // Zoom first, and NOT gated on `canMove`: a picture that is already
    // card-shaped has no travel to nudge and is exactly the picture a
    // curator most wants to tighten, so the keys that answer that have
    // to work when the nudge keys do not.
    //
    // `=` rides with `+` because on a US layout `+` needs Shift, and a
    // control that demands a modifier to zoom in but not to zoom out is
    // a control people press twice and give up on.
    if (e.key === '+' || e.key === '=') {
      e.preventDefault();
      setZoom(z * ZOOM_KEY_STEP);
      return;
    }
    if (e.key === '-' || e.key === '_') {
      e.preventDefault();
      setZoom(z / ZOOM_KEY_STEP);
      return;
    }
    if (!canMove) return;
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

  /** One press per keyboard step. 1.15 rather than a round 1.25 so the
   *  full 1..4 range takes about ten presses — the same "roughly ten
   *  presses end to end" the 2%-of-travel nudge step gives the arrows,
   *  so the two halves of the control feel like one. */
  const ZOOM_KEY_STEP = 1.15;

  /** Back to the fit, centred — a CLEAR of all three values, not a
   *  re-set to 0.5/0.5/1. Null and the neutral numbers render
   *  identically and are stored differently on purpose (see migrations
   *  00055 and 00056): this is what makes "the curator never framed
   *  this" recoverable.
   *
   *  Zoom is cleared TOGETHER with the position even though the two are
   *  independent settings on the wire. They are independent because
   *  each can be changed without the other; Reset is not a change to
   *  one of them, it is the single "put this back how it was" the
   *  curator reaches for, and leaving a 3x tightening behind after it
   *  would be the control not doing what it says. */
  function resetFocal() {
    focalX = null;
    focalY = null;
    zoom = null;
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
           shape that is not on screen.

           ⚠️ #1207 CONCLUDED that a definite width with `height: auto`
           and a `max-height` cannot distort, "because the auto axis
           always follows the aspect". IT DOES NOT. Chrome clips the
           height to the max and leaves the specified width alone, so a
           1:2 portrait came out 608x562 — the same squash, one
           combination further along. #1212 measured it and removed the
           max-height entirely; see `stageWidthCSS`, which caps the WIDTH
           by the height budget and the aspect instead, so the auto axis
           genuinely has nothing to fight.

           The wrapper stays SHRINK-WRAPPED around the image, which is
           what lets the marquee be positioned in percentages with
           nothing measured from the DOM. Giving the wrapper a size of
           its own puts the overlay over the BOX instead of over the
           picture — the exact bug this surface exists to reveal. -->
      <div class="flex justify-center rounded border border-border bg-surface p-2">
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          bind:this={stage}
          onwheel={onWheel}
          class="relative inline-block max-w-full align-top"
          style="touch-action: none;"
        >
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
            style="width: {stageWidthCSS}; height: auto;"
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
              disabled={!canMove && !canZoomIn}
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
        data-zoom={zoom === null ? '' : String(zoom)}
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

      <!-- THE ZOOM CONTROL (#1212).
           A native `range` rather than a pair of buttons or a bespoke
           slider, and the reason is a11y rather than effort: it is
           focusable, arrow-key operable, announced with its value and
           its bounds, and respects the platform's own step behaviour —
           all of which a div with pointer handlers would have to
           reimplement and would get subtly wrong. Wheel-over-the-stage
           and pinch are conveniences layered on top of it; this is the
           control, and it is the one the keyboard path uses.

           It lives UNDER THE STAGE rather than beside the preview
           because it changes what the stage's marquee shows, and a
           control placed away from the thing it moves is the surface
           complaint #1207 already fixed once. -->
      {#if win}
        <div class="mt-3 flex items-center gap-3">
          <label
            class="text-[10px] uppercase tracking-wide text-fg-muted"
            for="{testidPrefix}-zoom"
          >
            {t('collections.cover_editor_zoom_label')}
          </label>
          <input
            id="{testidPrefix}-zoom"
            type="range"
            data-testid="{testidPrefix}-zoom"
            min={MIN_ZOOM}
            max={MAX_ZOOM}
            step="0.01"
            value={z}
            oninput={(e) => setZoom((e.currentTarget as HTMLInputElement).valueAsNumber)}
            class="h-1 min-w-0 flex-1 accent-accent"
          />
          <span
            class="w-12 shrink-0 text-right text-xs tabular-nums text-fg-muted"
            data-testid="{testidPrefix}-zoom-value"
          >
            {t('collections.cover_editor_zoom_value', { z: z.toFixed(2) })}
          </span>
        </div>
      {/if}
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
        class="relative overflow-hidden rounded-lg border border-border bg-surface-elevated"
        style="aspect-ratio: {aspect}; max-width: calc({maxHeightVh}vh * {aspect});"
      >
        <img
          {src}
          {srcset}
          {sizes}
          alt={cardAlt}
          data-testid="{testidPrefix}-card-preview"
          class="object-cover"
          style={previewPlacement}
        />
      </div>
      <div class="mt-2 flex flex-wrap items-center gap-2">
        <button
          type="button"
          onclick={resetFocal}
          disabled={focalX === null && focalY === null && zoom === null}
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
