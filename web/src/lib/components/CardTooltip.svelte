<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The single hover tooltip for masonry tiles (#652). Mounted ONCE in
  // the root layout; every card feeds the shared `cardTooltip` store.
  // The store owns the timing (cold vs warm delay, leave grace) and the
  // whole argument for why this exists — read it first.
  //
  // This component owns placement and the a11y posture:
  //
  //   pointer-events: none  — it must never intercept a click meant for
  //     the tile underneath. On a 60px tile the tooltip sits within a
  //     few px of the artwork; anything hit-testable there would eat
  //     the navigation click and, worse, its own appearance would fire
  //     a mouseleave on the card and hide it — a flicker loop.
  //
  //   aria-hidden           — deliberate, not an oversight. This is
  //     mouse-only affordance duplicating information a screen reader
  //     already has: the card's stretched <a> carries the title as its
  //     accessible name. Wiring aria-describedby to a hover-only node
  //     announces the title twice and gives keyboard users a reference
  //     to something they can never see. Keyboard users reach the same
  //     facts through the ⋮ menu's "View details".
  //
  //   never focused         — no tabindex, no focus handlers. WCAG
  //     1.4.13 governs content shown on hover OR focus; this is shown
  //     on hover only and is dismissable (leave), non-obscuring (it is
  //     placed OUTSIDE the tile) and, being pointer-events:none, cannot
  //     be hovered itself so there is nothing to persist.
  //
  // Placement: x tracks the cursor, y is pinned outside the tile's own
  // rect — below it when there is room, above it otherwise. The tooltip
  // therefore never covers the artwork it is describing, which is the
  // failure the overlay it replaces had.

  import { cardTooltip } from '$stores/cardTooltip.svelte';

  /** Gap between the tooltip and the tile edge / viewport edge. */
  const GAP = 8;
  /** Horizontal offset from the cursor — enough that the pointer glyph
   *  doesn't sit on top of the first character. */
  const CURSOR_DX = 14;

  let w = $state(0);
  let h = $state(0);
  let vw = $state(0);
  let vh = $state(0);

  const left = $derived(
    Math.max(GAP, Math.min(cardTooltip.x + CURSOR_DX, Math.max(GAP, vw - w - GAP))),
  );

  const top = $derived.by(() => {
    const below = cardTooltip.anchorBottom + GAP;
    // Prefer below the tile. Flip above only when the tooltip would
    // leave the viewport — a flip is a jump, so it should be rare.
    if (below + h <= vh - GAP) return below;
    const above = cardTooltip.anchorTop - h - GAP;
    if (above >= GAP) return above;
    return Math.max(GAP, Math.min(below, vh - h - GAP));
  });

  // Hidden until measured, so the first frame doesn't flash at 0,0.
  const measured = $derived(w > 0 && h > 0);

  // Any scroll strands the tooltip: the anchor rect it was placed
  // against has moved. CAPTURE on the document, not `window`, because
  // the browse wall scrolls inside <main> (`overflow-y-auto`) and a
  // window-level listener never hears it — that is the difference
  // between "hides on scroll" and "sticks to the screen while the wall
  // slides underneath". Only bound while something is showing.
  $effect(() => {
    if (!cardTooltip.visible) return;
    const off = () => cardTooltip.hide();
    document.addEventListener('scroll', off, true);
    window.addEventListener('blur', off);
    return () => {
      document.removeEventListener('scroll', off, true);
      window.removeEventListener('blur', off);
    };
  });
</script>

<svelte:window bind:innerWidth={vw} bind:innerHeight={vh} />

{#if cardTooltip.visible}
  <div
    bind:clientWidth={w}
    bind:clientHeight={h}
    aria-hidden="true"
    data-testid="card-tooltip"
    class="pointer-events-none fixed z-40 max-w-[18rem] rounded-md border border-border bg-surface
           px-2.5 py-1.5 shadow-lg transition-opacity duration-100"
    class:opacity-0={!measured}
    style="left:{left}px; top:{top}px;"
  >
    <p class="line-clamp-2 text-sm font-medium leading-snug text-fg">{cardTooltip.title}</p>
    {#if cardTooltip.meta.length > 0}
      <p class="mt-0.5 truncate text-xs text-fg-muted">{cardTooltip.meta.join(' · ')}</p>
    {/if}
  </div>
{/if}
