<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The cover editor (#1207).
  //
  // Its own near-full-viewport dialog, raised from the collection edit
  // modal, because the owner's finding was that the cover surface "is
  // too small and I can't really see how the cover images will be
  // cropped". Cropping is a judgement about a picture; making that
  // judgement in a 200px thumbnail is the defect, and no amount of
  // re-laying a shared modal fixes it while the picture stays small.
  //
  // TWO SLOTS, because the second finding was that a collection card
  // and a featured-rail card want different pictures:
  //
  //   1. the collection cover  — every collection card, roughly square
  //   2. the featured cover    — the strip only, locked to 890:500
  //
  // Slot 2 DEFAULTS TO SLOT 1 rather than starting empty. "No separate
  // choice" is the common case and it is also what the rail's fallback
  // chain does, so the editor shows the picture the rail would actually
  // use instead of an empty box that means "look elsewhere".
  //
  // IT OWNS NO SAVE. Every value here is a bindable prop belonging to
  // EditCollectionModal, which has the collection, the concurrency
  // baseline and the single PATCH. A dialog that saved its own two
  // fields would give the curator two Save buttons with different
  // failure modes and two chances to hit a 409 — and would need its own
  // copy of the tri-state clear discipline. Closing this is "done
  // looking", not "committed".

  import { t } from '$stores/lang.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import Modal from './Modal.svelte';
  import {
    CARD_ASPECT,
    cropWindow,
    focalFromOrigin,
    hasTravel,
    marqueeOrigin,
    objectPosition,
  } from '$lib/util/featuredCrop';

  /** One choosable picture. Mirrors EditCollectionModal's CoverChoice —
   *  the same rows, handed down rather than re-fetched, so both slots
   *  and the summary behind them are looking at one list. */
  interface CoverChoice {
    asset_id: string;
    restricted?: boolean;
    preview_available?: boolean;
    ladder_available?: boolean;
  }

  interface Props {
    open: boolean;
    onclose: () => void;
    choices: CoverChoice[];
    loading: boolean;
    /** The collection cover, null for the derived mosaic. */
    coverAssetId: string | null;
    /** The featured-rail cover, null for "same as the collection cover". */
    featuredCoverAssetId: string | null;
    /** The focal pair, null for centre. Always moved together. */
    focalX: number | null;
    focalY: number | null;
  }

  let {
    open,
    onclose,
    choices,
    loading,
    coverAssetId = $bindable(),
    featuredCoverAssetId = $bindable(),
    focalX = $bindable(),
    focalY = $bindable(),
  }: Props = $props();

  // ── Which picture each slot is showing ─────────────────────────────
  //
  // The featured slot falls back to the collection cover exactly as the
  // RAIL does. That is the point: what this box shows is what the strip
  // will show, and if the fallback were spelled out differently here
  // the editor would be a second opinion about the rail's own rule.
  const featuredEffectiveId = $derived(featuredCoverAssetId ?? coverAssetId);
  const featuredIsInherited = $derived(featuredCoverAssetId === null && coverAssetId !== null);

  function choiceFor(assetId: string | null): CoverChoice | null {
    if (assetId === null) return null;
    return choices.find((c) => c.asset_id === assetId) ?? null;
  }

  function colUrl(assetId: string) {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  // ⚠️ THE EDITOR HAS TO LOAD THE SAME PICTURE THE CARD LOADS, or the
  // marquee is drawn over the wrong thing. FeaturedRail picks between
  // two sources with DIFFERENT shapes:
  //
  //   - with a ladder for this asset, a `contain` rung — the original
  //     aspect, so the card's crop is the only crop;
  //   - without one, `col` — which the server has ALREADY centre-cropped
  //     to a square, so the card crops a square, not the original.
  //
  // Previewing `col` in both cases would tell a portrait cover's curator
  // their picture crops to a wide band of a square that does not exist.
  // Carried forward from #1195's preview, and load-bearing twice over
  // now that the curator is POSITIONING against it: a focal fraction
  // chosen against the wrong source lands somewhere else on the strip.
  //
  // An asset with no row in `choices` (a cover chosen from outside this
  // collection) has an unknown ladder, which resolves to `col` — the
  // same fallback the rail takes when `ladder_available` is false, so
  // the editor degrades to the card's own worst case rather than to an
  // optimistic guess.
  function srcFor(assetId: string | null): string | null {
    if (assetId === null) return null;
    if (choiceFor(assetId)?.ladder_available !== true) return colUrl(assetId);
    const smallest = previewLadder.smallestKey();
    return smallest ? `/api/v1/assets/${assetId}/variants/${smallest}` : colUrl(assetId);
  }

  function srcsetFor(assetId: string | null): string | undefined {
    if (assetId === null || choiceFor(assetId)?.ladder_available !== true) return undefined;
    return previewLadder.srcsetFor(assetId) ?? undefined;
  }

  const featuredSrc = $derived(srcFor(featuredEffectiveId));
  const featuredSrcset = $derived(srcsetFor(featuredEffectiveId));
  const coverSrc = $derived(srcFor(coverAssetId));
  const coverSrcset = $derived(srcsetFor(coverAssetId));

  // ── The picture's own proportions, STAMPED WITH THE ASSET ──────────
  //
  // The stamp is what makes the marquee safe rather than merely usually
  // right (#1195's note, kept). A plain {w,h} cleared by an effect on
  // the selection has an ordering hazard: a picture already in the
  // browser cache fires `load` synchronously enough to beat the effect,
  // which then clears the value it just produced. Keying on the id and
  // deriving "is this measurement about the current picture?" removes
  // the question instead of timing it.
  let natural = $state<{ assetId: string; w: number; h: number } | null>(null);
  const naturalNow = $derived(
    natural && natural.assetId === featuredEffectiveId ? natural : null,
  );

  function onStageLoad(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    if (featuredEffectiveId && img.naturalWidth > 0 && img.naturalHeight > 0) {
      natural = { assetId: featuredEffectiveId, w: img.naturalWidth, h: img.naturalHeight };
    }
  }

  const win = $derived(naturalNow ? cropWindow(naturalNow.w / naturalNow.h, CARD_ASPECT) : null);
  /** Which axis the curator can actually move on. Exactly one, ever —
   *  `object-fit: cover` trims one axis and shows the other whole. */
  const canMoveX = $derived(win !== null && hasTravel(win.w));
  const canMoveY = $derived(win !== null && hasTravel(win.h));
  const canMove = $derived(canMoveX || canMoveY);

  /** The stored pair, with null read as centre. The marquee is drawn
   *  from these and the drag writes back to them. */
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

  /** What the strip will render. Called for the live preview AND
   *  written by the rail from the same helper, so the two cannot drift
   *  over what null means. */
  const previewPosition = $derived(objectPosition(focalX, focalY));

  // ── Dragging ───────────────────────────────────────────────────────
  //
  // Pointer events, not mouse events: one code path covers mouse, touch
  // and pen, which is what makes the marquee work at 390px on a coarse
  // pointer without a second handler to keep in step.
  //
  // `setPointerCapture` is what makes a drag survive the pointer leaving
  // the picture — and the curator dragging to an edge is not an edge
  // case here, it is how you frame the left of a wide photograph. Without
  // capture the marquee would stop the moment the pointer crossed the
  // border and the position would silently be short of what was asked.
  let stage = $state<HTMLDivElement | null>(null);
  let dragging = $state(false);
  /** Where in the marquee the pointer grabbed, as a fraction of the
   *  PICTURE. Kept so the marquee moves WITH the pointer rather than
   *  jumping its centre to it — a jump on mousedown is how a
   *  positioning control loses the position it was showing. */
  let grabOffset = { x: 0, y: 0 };

  function stageRect(): DOMRect | null {
    return stage?.getBoundingClientRect() ?? null;
  }

  function onPointerDown(e: PointerEvent) {
    const rect = stageRect();
    if (!rect || !win || !canMove) return;
    e.preventDefault();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    dragging = true;
    const px = (e.clientX - rect.left) / rect.width;
    const py = (e.clientY - rect.top) / rect.height;
    grabOffset = {
      x: px - marqueeOrigin(fx, win.w),
      y: py - marqueeOrigin(fy, win.h),
    };
  }

  function onPointerMove(e: PointerEvent) {
    if (!dragging) return;
    const rect = stageRect();
    if (!rect || !win) return;
    e.preventDefault();
    const px = (e.clientX - rect.left) / rect.width;
    const py = (e.clientY - rect.top) / rect.height;
    // Both axes are written on every move even though only one can
    // travel: focalFromOrigin answers 0.5 for a pinned axis, so the
    // pinned half stays neutral instead of accumulating pointer noise,
    // and the PAIR stays a pair — which is what the column CHECK and
    // the API's both-or-neither rule require.
    setFocal(
      focalFromOrigin(px - grabOffset.x, win.w),
      focalFromOrigin(py - grabOffset.y, win.h),
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
   *  the picture's proportions; Shift takes bigger steps for a long
   *  move, and Home/End go to the ends. Arrow keys are claimed
   *  (preventDefault) because the dialog body scrolls and an unclaimed
   *  ArrowDown would scroll the page out from under the control the
   *  curator is using. */
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

  /** A cover chosen from outside the member list still has to show as
   *  the current selection, or an unrelated edit would look like it had
   *  cleared the cover. */
  function isExternal(assetId: string | null) {
    return assetId !== null && !choices.some((c) => c.asset_id === assetId);
  }

  const positionLabel = $derived(
    t('collections.cover_editor_position_value', {
      x: String(Math.round(fx * 100)),
      y: String(Math.round(fy * 100)),
    }),
  );
</script>

<!-- Near-full-viewport. The whole point of this dialog is that the
     picture is big enough to judge, so the width is a viewport
     proportion rather than a Tailwind size step — `max-w-7xl` on a 4k
     display is the cramped modal again with more whitespace round it. -->
<Modal
  title={t('collections.cover_editor_title')}
  {open}
  {onclose}
  panelClass="max-w-[min(96rem,95vw)]"
>
  <div class="max-h-[80vh] space-y-6 overflow-y-auto pr-1" data-testid="collection-cover-editor">
    <!-- Slot 2 FIRST. It is the one with the work in it — the marquee,
         the live preview — and it is the reason the curator opened this
         dialog. The collection cover below it is a straightforward
         pick, and putting the simple thing first would push the
         positioning stage under the fold on a laptop. -->
    <section aria-labelledby="featured-slot-heading" data-testid="featured-cover-slot">
      <h3 id="featured-slot-heading" class="text-sm font-semibold">
        {t('collections.cover_editor_featured_heading')}
      </h3>
      <p class="mt-0.5 text-xs text-fg-muted">
        {featuredIsInherited
          ? t('collections.cover_editor_featured_inherited')
          : t('collections.cover_editor_featured_hint')}
      </p>

      {#if featuredEffectiveId === null || featuredSrc === null}
        <p class="mt-3 rounded border border-border bg-surface p-3 text-xs text-fg-muted">
          {t('collections.cover_editor_featured_none')}
        </p>
      {:else}
        <div class="mt-3 grid gap-4 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
          <!-- LEFT: the whole picture at size, with the marquee on it.

               The wrapper SHRINK-WRAPS the image (inline-block, no width
               or height of its own) so the element and the picture are
               the SAME rectangle. That is what lets the marquee be
               positioned in percentages with nothing measured from the
               DOM. Giving the wrapper a size instead — an aspect-ratio,
               a fixed height — reintroduces exactly the bug this
               surface exists to fix: the image letterboxes inside a box
               it does not fill, and the marquee marks a region of the
               BOX rather than of the picture. -->
          <figure class="min-w-0">
            <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
              {t('collections.cover_editor_stage_label')}
            </figcaption>
            <div class="flex justify-center rounded border border-border bg-surface p-2">
              <div bind:this={stage} class="relative inline-block max-w-full align-top">
                <img
                  src={featuredSrc}
                  srcset={featuredSrcset}
                  sizes="(max-width: 640px) 90vw, 55vw"
                  alt={t('collections.cover_editor_stage_alt')}
                  onload={onStageLoad}
                  data-testid="cover-editor-stage-image"
                  class="block max-h-[52vh] max-w-full select-none rounded"
                  draggable="false"
                />
                {#if marquee}
                  <!-- What gets cropped OFF is dimmed and what survives
                       is outlined: two readings of one fact, so it holds
                       up for a colour-blind reader and in a 390px
                       screenshot alike. Four bars rather than #1195's
                       two, because a marquee that has MOVED is off
                       centre and the two offcuts are no longer equal —
                       the symmetric pair was only ever correct for a
                       centred crop. `pointer-events-none` because they
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
                       obviously interactive without inventing a role for
                       a two-axis positioner that ARIA does not have one
                       for. Its accessible name carries the current
                       position, so a screen-reader user gets the same
                       feedback a sighted one gets from watching it move.

                       `touch-action: none` is what makes the drag work
                       on a phone: without it the browser claims the
                       gesture for scrolling and the marquee never sees
                       a pointermove. -->
                  <button
                    type="button"
                    data-testid="cover-editor-marquee"
                    aria-label={t('collections.cover_editor_marquee_label')}
                    aria-describedby="cover-editor-position"
                    disabled={!canMove}
                    onpointerdown={onPointerDown}
                    onpointermove={onPointerMove}
                    onpointerup={endDrag}
                    onpointercancel={endDrag}
                    onkeydown={onKeydown}
                    class="absolute border-2 border-accent shadow-[0_0_0_9999px_rgba(0,0,0,0)] focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring {canMove
                      ? dragging
                        ? 'cursor-grabbing'
                        : 'cursor-grab'
                      : 'cursor-default'}"
                    style="left: {marquee.left}%; top: {marquee.top}%; width: {marquee.width}%; height: {marquee.height}%; touch-action: none;"
                  ></button>
                {/if}
              </div>
            </div>
            <p
              id="cover-editor-position"
              class="mt-2 text-xs text-fg-muted"
              data-testid="cover-editor-position"
              data-focal-x={focalX === null ? '' : String(focalX)}
              data-focal-y={focalY === null ? '' : String(focalY)}
            >
              {#if !canMove}
                {t('collections.cover_editor_no_travel')}
              {:else}
                {t('collections.cover_editor_drag_hint')} — {positionLabel}
              {/if}
            </p>
          </figure>

          <!-- RIGHT: the card itself, drawn the way FeaturedRail draws
               it. Not a simulation of the crop: the same three CSS
               properties the strip uses (the aspect box, object-cover,
               and object-position from the same helper), on the same
               source. If the two ever disagree it is because the strip
               changed. -->
          <figure class="min-w-0">
            <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
              {t('collections.cover_editor_card_label')}
            </figcaption>
            <div
              class="overflow-hidden rounded-lg border border-border bg-surface-elevated"
              style="aspect-ratio: 890 / 500"
            >
              <img
                src={featuredSrc}
                srcset={featuredSrcset}
                sizes="(max-width: 640px) 90vw, 35vw"
                alt={t('collections.cover_editor_card_alt')}
                data-testid="cover-editor-card-preview"
                class="h-full w-full object-cover"
                style="object-position: {previewPosition}"
              />
            </div>
            <div class="mt-2 flex flex-wrap items-center gap-2">
              <button
                type="button"
                onclick={resetFocal}
                disabled={focalX === null && focalY === null}
                data-testid="cover-editor-reset-focal"
                class="rounded border border-border px-2 py-1 text-xs hover:bg-surface disabled:cursor-not-allowed disabled:opacity-40"
              >
                {t('collections.cover_editor_reset')}
              </button>
              {#if featuredCoverAssetId !== null}
                <button
                  type="button"
                  onclick={() => (featuredCoverAssetId = null)}
                  data-testid="cover-editor-clear-featured"
                  class="rounded border border-border px-2 py-1 text-xs hover:bg-surface"
                >
                  {t('collections.cover_editor_use_collection_cover')}
                </button>
              {/if}
            </div>
          </figure>
        </div>
      {/if}

      {#if loading}
        <p class="mt-3 text-xs text-fg-muted">{t('collections.cover_loading')}</p>
      {:else}
        <div class="mt-3 grid max-h-40 grid-cols-6 gap-2 overflow-y-auto rounded border border-border p-1 sm:grid-cols-10"
             data-testid="featured-cover-choices">
          <button
            type="button"
            onclick={() => (featuredCoverAssetId = null)}
            aria-pressed={featuredCoverAssetId === null}
            class="flex aspect-square flex-col items-center justify-center rounded border-2 bg-surface p-1 text-center text-[10px] leading-tight text-fg-muted hover:border-border-strong"
            class:border-accent={featuredCoverAssetId === null}
            class:border-border={featuredCoverAssetId !== null}
          >
            <span class="font-medium">{t('collections.cover_editor_same_as_cover')}</span>
          </button>
          {#if isExternal(featuredCoverAssetId) && featuredCoverAssetId}
            <button
              type="button"
              aria-pressed="true"
              title={t('collections.cover_current_external')}
              class="relative aspect-square overflow-hidden rounded border-2 border-accent"
            >
              <img src={colUrl(featuredCoverAssetId)} alt={t('collections.cover_current_external')}
                   loading="lazy" class="h-full w-full object-cover" />
            </button>
          {/if}
          {#each choices as choice (choice.asset_id)}
            <button
              type="button"
              onclick={() => (featuredCoverAssetId = choice.asset_id)}
              aria-pressed={featuredCoverAssetId === choice.asset_id}
              data-testid="featured-cover-choice"
              data-asset-id={choice.asset_id}
              class="relative aspect-square overflow-hidden rounded border-2 hover:border-border-strong"
              class:border-accent={featuredCoverAssetId === choice.asset_id}
              class:border-border={featuredCoverAssetId !== choice.asset_id}
            >
              <img src={colUrl(choice.asset_id)} alt="" loading="lazy" class="h-full w-full object-cover" />
            </button>
          {/each}
        </div>
      {/if}
    </section>

    <section aria-labelledby="cover-slot-heading" data-testid="collection-cover-slot"
             class="border-t border-border pt-5">
      <h3 id="cover-slot-heading" class="text-sm font-semibold">
        {t('collections.cover_editor_cover_heading')}
      </h3>
      <p class="mt-0.5 text-xs text-fg-muted">{t('collections.cover_hint')}</p>

      <div class="mt-3 flex items-start gap-4">
        <!-- The collection card is roughly square and takes a centre
             crop, so this slot needs no marquee — there is nothing to
             position that the curator did not already choose by picking
             the picture. Showing the square preview is the whole
             feedback the slot owes them. -->
        <figure class="w-40 shrink-0">
          <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
            {t('collections.cover_editor_cover_preview_label')}
          </figcaption>
          <div class="aspect-square overflow-hidden rounded border border-border bg-surface-elevated">
            {#if coverAssetId !== null && coverSrc !== null}
              <img src={coverSrc} srcset={coverSrcset} sizes="160px"
                   alt={t('collections.cover_editor_cover_preview_alt')}
                   data-testid="cover-editor-collection-preview"
                   class="h-full w-full object-cover" />
            {:else}
              <div class="flex h-full items-center justify-center p-2 text-center text-[10px] text-fg-muted">
                {t('collections.cover_derived')}
              </div>
            {/if}
          </div>
        </figure>

        {#if loading}
          <p class="text-xs text-fg-muted">{t('collections.cover_loading')}</p>
        {:else}
          <div class="grid max-h-40 min-w-0 flex-1 grid-cols-6 gap-2 overflow-y-auto rounded border border-border p-1 sm:grid-cols-10"
               data-testid="collection-cover-choices">
            <button
              type="button"
              onclick={() => (coverAssetId = null)}
              aria-pressed={coverAssetId === null}
              class="flex aspect-square flex-col items-center justify-center rounded border-2 bg-surface p-1 text-center text-[10px] leading-tight text-fg-muted hover:border-border-strong"
              class:border-accent={coverAssetId === null}
              class:border-border={coverAssetId !== null}
            >
              <span class="font-medium">{t('collections.cover_derived')}</span>
            </button>
            {#if isExternal(coverAssetId) && coverAssetId}
              <button
                type="button"
                aria-pressed="true"
                title={t('collections.cover_current_external')}
                class="relative aspect-square overflow-hidden rounded border-2 border-accent"
              >
                <img src={colUrl(coverAssetId)} alt={t('collections.cover_current_external')}
                     loading="lazy" class="h-full w-full object-cover" />
              </button>
            {/if}
            {#each choices as choice (choice.asset_id)}
              <button
                type="button"
                onclick={() => (coverAssetId = choice.asset_id)}
                aria-pressed={coverAssetId === choice.asset_id}
                data-testid="collection-cover-choice"
                data-asset-id={choice.asset_id}
                class="relative aspect-square overflow-hidden rounded border-2 hover:border-border-strong"
                class:border-accent={coverAssetId === choice.asset_id}
                class:border-border={coverAssetId !== choice.asset_id}
              >
                <img src={colUrl(choice.asset_id)} alt="" loading="lazy" class="h-full w-full object-cover" />
              </button>
            {/each}
          </div>
        {/if}
      </div>
    </section>
  </div>

  {#snippet footer()}
    <!-- ONE button, and it says Done rather than Save. Nothing here is
         written until the form behind this dialog is submitted, and a
         Save button that only closed a dialog would be a promise this
         surface cannot keep. -->
    <button
      type="button"
      onclick={onclose}
      data-testid="cover-editor-done"
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent"
    >
      {t('collections.cover_editor_done')}
    </button>
  {/snippet}
</Modal>
