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
  //   aria-hidden           — deliberate, not an oversight, and STILL
  //     correct now that it is also shown on focus (#1126). The card's
  //     stretched <a> carries the FULL title as its accessible name
  //     already, truncation being purely visual (`text-overflow`
  //     clips pixels, not the accessibility tree). So a screen reader
  //     has this string before the tooltip exists; wiring
  //     aria-describedby would announce it a second time. What the
  //     focus arm adds is for a SIGHTED keyboard user, who could see
  //     the clipped title and had no way to read the rest.
  //
  //   never focused         — no tabindex, no focus handlers ON THE
  //     TOOLTIP. It is shown in response to focus elsewhere, which is
  //     what WCAG 1.4.13 asks for; it stays dismissable (Escape and
  //     blur both hide it), non-obscuring (placed outside the anchor)
  //     and, being pointer-events:none, cannot be hovered itself so
  //     there is nothing to persist.
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
  /** Vertical offset under 'follow'. Larger than CURSOR_DX because the
   *  pointer glyph is TALLER than it is wide and hangs BELOW its
   *  hotspot: at 14px the arrow's tail overlapped the box's top edge.
   *  The box must never sit under the cursor — #1126 says so, and a box
   *  the pointer is inside would also be a box the card underneath
   *  stops receiving mousemove from. */
  const CURSOR_DY = 20;

  let w = $state(0);
  let h = $state(0);
  let vw = $state(0);
  let vh = $state(0);

  const clampX = (x: number) => Math.max(GAP, Math.min(x, Math.max(GAP, vw - w - GAP)));

  const left = $derived(
    cardTooltip.placement === 'element'
      ? clampX(cardTooltip.anchorLeft)
      : clampX(cardTooltip.x + CURSOR_DX),
  );

  const top = $derived.by(() => {
    // 'follow' — the box rides the cursor in both axes (#1126). It
    // prefers BELOW-RIGHT of the pointer and flips above only at the
    // viewport's bottom edge, so a title read near the fold does not
    // hang off screen. Clamping rather than flipping horizontally: a
    // box that jumped to the cursor's left mid-sweep would read as a
    // second tooltip appearing.
    if (cardTooltip.placement === 'follow') {
      const below = cardTooltip.y + CURSOR_DY;
      if (below + h <= vh - GAP) return below;
      const above = cardTooltip.y - h - CURSOR_DY;
      return above >= GAP ? above : Math.max(GAP, vh - h - GAP);
    }
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
  //
  // Escape hides it too (#1126). WCAG 1.4.13's "dismissable" clause
  // wants a way out that does not require moving the pointer or losing
  // focus, and the focus-shown arm has no mouseleave to rely on.
  $effect(() => {
    if (!cardTooltip.visible) return;
    const off = () => cardTooltip.hide();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') cardTooltip.hide();
    };
    document.addEventListener('scroll', off, true);
    window.addEventListener('blur', off);
    window.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('scroll', off, true);
      window.removeEventListener('blur', off);
      window.removeEventListener('keydown', onKey);
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
    <!-- `line-clamp-2` for masonry, where the tooltip is a SCAN aid
         beside a dense wall and a five-line box would cover the tiles
         either side of the one it describes.

         NOT clamped for the two #1126 placements, whose whole purpose is
         that the title was already clipped once — clamping the tooltip
         would clip it a second time and answer the question with the
         same ellipsis. `break-words` because an unbroken 200-character
         filename is a real asset name and would otherwise push the box
         past its max-width. -->
    <p
      class="text-sm font-medium leading-snug text-fg
             {cardTooltip.placement === 'anchored' ? 'line-clamp-2' : 'break-words'}"
    >{cardTooltip.title}</p>
    {#if cardTooltip.meta.length > 0}
      <p class="mt-0.5 truncate text-xs text-fg-muted">{cardTooltip.meta.join(' · ')}</p>
    {/if}
  </div>
{/if}
