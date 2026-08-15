// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * The behaviour of a horizontal rail with NO SCROLLBAR: edge-arrow
 * state, one-item stepping, and click-and-drag panning.
 *
 * # Why this is shared code and not a second copy
 *
 * #1110 shipped all of this inside FeaturedRail, with a note saying the
 * teams rail would get the same treatment when #1113 landed. It has, so
 * the behaviour moves here rather than being pasted — and the reason is
 * specific rather than tidiness. The drag implementation carries a
 * finding that is invisible from the code and was expensive to get
 * (see `ondragstart` below); a copy is a copy of the code, not of the
 * finding, and the second copy is where it gets "cleaned up" out.
 *
 * # Hiding a scrollbar removes an affordance
 *
 * Three replace it, and a caller that uses this helper is signing up
 * for all three:
 *   - the edge chevrons (`atStart` / `atEnd` / `step`), for a mouse
 *     with no horizontal wheel,
 *   - the drag pan (the handlers), for a mouse that would have grabbed
 *     the bar,
 *   - the wheel and touch swipe, which are the browser's own and are
 *     deliberately untouched — nothing here drives the scroll position
 *     except the drag and the arrows.
 *
 * The keyboard path is the chevrons, unchanged: the pan adds no
 * keyboard requirement because it adds no keyboard-only action.
 *
 * # Usage
 *
 *   const rail = createRailScroll(() => scrollerEl, { itemSelector: '…' });
 *   <div bind:this={scrollerEl} {...rail.handlers}
 *        class="rail-scroller {rail.dragging ? 'cursor-grabbing' : 'cursor-grab'}">
 *
 * `rail.attach()` belongs in an `$effect` — it measures once and keeps
 * measuring through a ResizeObserver, returning the teardown.
 *
 * The `.rail-scroller` class itself is global (see app.css): it is the
 * CSS half of the same decision and the two are useless apart.
 */

interface RailScrollOptions {
  /** Selector for one rail item, used to size an arrow step. Matched
   *  INSIDE the scroller. */
  itemSelector: string;
  /** The rail's flex gap, in px. Must match the markup's `gap-*`: the
   *  step is one item plus one gap. */
  gap: number;
  /** Width to step by when no item matches `itemSelector` yet (an empty
   *  or not-yet-loaded rail). */
  fallbackWidth: number;
}

/** Pixels of horizontal movement before a press becomes a pan. 5px is
 *  above the jitter a hand resting on a mouse produces and well below
 *  what anyone would call a deliberate drag. */
const DRAG_THRESHOLD = 5;

export function createRailScroll(
  getEl: () => HTMLElement | null | undefined,
  { itemSelector, gap, fallbackWidth }: RailScrollOptions,
) {
  let atStart = $state(true);
  let atEnd = $state(false);
  /** True from the moment a drag passes the movement threshold until
   *  the click that ends it has been swallowed. Drives the cursor and
   *  the click guard. */
  let dragging = $state(false);

  let dragStartX = 0;
  let dragStartScroll = 0;
  let dragPointerId: number | null = null;

  /** Recompute which ends the scroller is parked at.
   *
   *  The 1px tolerance is not superstition: `scrollLeft` is fractional
   *  on a zoomed or fractionally-scaled display, so `scrollLeft + w ===
   *  scrollWidth` is false at the true end often enough to leave "next"
   *  live with nothing left to scroll to. */
  function measure() {
    const el = getEl();
    if (!el) return;
    atStart = el.scrollLeft <= 1;
    atEnd = el.scrollLeft + el.clientWidth >= el.scrollWidth - 1;
  }

  /** Scroll by one item + its gap.
   *
   *  An item at a time rather than a viewport at a time: a
   *  viewport-sized jump on a wide display moves several items and
   *  lands mid-item, and the reader loses the one they were reading.
   *  The step is computed from the RENDERED item, not from a constant,
   *  so it stays right at the widths where an item is narrower than its
   *  nominal size.
   *
   *  `behavior: smooth` is deliberate on a control that can be held
   *  down; the browser coalesces repeats rather than queueing them. */
  function step(direction: -1 | 1) {
    const el = getEl();
    if (!el) return;
    const item = el.querySelector<HTMLElement>(itemSelector);
    const by = (item?.getBoundingClientRect().width ?? fallbackWidth) + gap;
    el.scrollBy({ left: direction * by, behavior: 'smooth' });
  }

  /** The guard both arrows need. `aria-disabled` is advisory — it stops
   *  nothing on its own — so a click that arrives anyway (a
   *  screen-reader activation, a stale pointer) must be a no-op rather
   *  than a scroll to a place that does not exist. */
  function prev() {
    if (atStart) return;
    step(-1);
  }
  function next() {
    if (atEnd) return;
    step(1);
  }

  // ── Click-and-drag panning ────────────────────────────────────────
  //
  // Pointer events, not mouse events, and MOUSE ONLY. Touch already
  // pans natively through `overflow-x-auto`; running our own drag over
  // the top of that fights the browser's momentum scrolling and loses,
  // so `pointerType === 'mouse'` is the gate and a phone never enters
  // this code at all. A pen behaves like touch here for the same reason.
  //
  // THE THRESHOLD IS THE WHOLE DESIGN. Every item is clickable — a link
  // in the featured strip, a filter toggle in the teams rail — so a
  // drag implementation that treats pointerdown-then-pointerup as a drag
  // breaks clicking, and one that never suppresses the click makes every
  // pan activate something. So movement is measured first and nothing is
  // "a drag" until it exceeds DRAG_THRESHOLD; below that the gesture was
  // a click and is left entirely alone.
  //
  // Capture is taken LAZILY, at the threshold rather than at
  // pointerdown. Taking it on down would retarget the pointer to the
  // scroller immediately, and the `click` a plain press produces would
  // then never reach the item — which is the failure this ordering
  // exists to avoid.

  /** Kill the browser's OWN drag-and-drop inside the strip.
   *
   *  Without this the pan does not work at all, and the reason is worth
   *  keeping because it is invisible from the code: rail items contain
   *  links and images, both natively draggable. The first pointermove of
   *  a press is below DRAG_THRESHOLD, so it returns early — correctly —
   *  WITHOUT calling preventDefault, and in that same instant Chromium
   *  starts a native image/link drag. `dragstart` cancels the pointer
   *  sequence, so no further pointermove is ever delivered and the strip
   *  moves by exactly one frame's worth of travel and then stops.
   *  Measured on the featured strip: a 260px drag panned 20px.
   *
   *  Cancelling dragstart is the fix rather than moving preventDefault
   *  up into pointerdown: preventing the default on pointerdown also
   *  suppresses focus and the click that follows it, which would break
   *  every item to buy the same thing. */
  function onDragStart(e: DragEvent) {
    e.preventDefault();
  }

  function onPointerDown(e: PointerEvent) {
    if (e.pointerType !== 'mouse' || e.button !== 0) return;
    const el = getEl();
    if (!el || el.scrollWidth <= el.clientWidth) return;
    dragPointerId = e.pointerId;
    dragStartX = e.clientX;
    dragStartScroll = el.scrollLeft;
    // NOT dragging yet, and no capture taken — see the threshold note.
    dragging = false;
  }

  function onPointerMove(e: PointerEvent) {
    if (dragPointerId === null || e.pointerId !== dragPointerId) return;
    const el = getEl();
    if (!el) return;
    const dx = e.clientX - dragStartX;
    if (!dragging) {
      if (Math.abs(dx) < DRAG_THRESHOLD) return;
      dragging = true;
      // Now that this IS a pan, keep receiving moves even when the
      // pointer leaves the strip — a pan that stops at the element's
      // edge is a pan that stops halfway.
      el.setPointerCapture(e.pointerId);
    }
    // Native `behavior: smooth` from the chevrons would fight a direct
    // assignment; setting scrollLeft cancels any in-flight smooth
    // scroll, which is the behaviour a reader grabbing the strip
    // mid-animation expects.
    el.scrollLeft = dragStartScroll - dx;
    // The browser's own text/image drag would otherwise start on the
    // item's artwork and paint a ghost image under the cursor.
    e.preventDefault();
  }

  function endDrag(e: PointerEvent) {
    if (dragPointerId === null || e.pointerId !== dragPointerId) return;
    const el = getEl();
    if (el?.hasPointerCapture(e.pointerId)) el.releasePointerCapture(e.pointerId);
    dragPointerId = null;
    // `dragging` is NOT cleared here. The click that follows this
    // pointerup has not fired yet, and onClickCapture below needs to
    // know a pan just happened so it can swallow it. It clears itself
    // there, or on the next pointerdown if no click arrives.
  }

  /** Swallow the click a pan produces, and only that one.
   *
   *  Registered in the CAPTURE phase so it runs before the item's own
   *  handler — by the bubble phase a link has already navigated and a
   *  toggle has already fired. A press that never crossed the threshold
   *  left `dragging` false and passes through untouched, which is what
   *  keeps the items clickable. */
  function onClickCapture(e: MouseEvent) {
    if (!dragging) return;
    dragging = false;
    e.preventDefault();
    e.stopPropagation();
  }

  return {
    get atStart() {
      return atStart;
    },
    get atEnd() {
      return atEnd;
    },
    get dragging() {
      return dragging;
    },
    measure,
    step,
    prev,
    next,
    /** Spread onto the scroller element. `onscroll` only MEASURES — it
     *  does not drive the scroll, so wheel / trackpad / touch swipe are
     *  untouched and it exists purely to keep the arrows' disabled state
     *  honest as the reader scrolls by any other means. */
    handlers: {
      onscroll: measure,
      onpointerdown: onPointerDown,
      onpointermove: onPointerMove,
      onpointerup: endDrag,
      onpointercancel: endDrag,
      ondragstart: onDragStart,
      onclickcapture: onClickCapture,
    },
    /** Measure now and keep measuring. Returns the teardown, so the
     *  call site is `$effect(() => rail.attach())`. Read whatever makes
     *  the rail's contents change before calling it, so the effect
     *  re-runs once the items land. */
    attach(): () => void {
      const el = getEl();
      if (!el) return () => {};
      measure();
      const ro = new ResizeObserver(measure);
      ro.observe(el);
      return () => ro.disconnect();
    },
  };
}

/** The chevrons' classes, as plain strings rather than Tailwind's
 *  `aria-disabled:` variant.
 *
 *  The variant works, but the disabled arm needs three declarations
 *  including one that is itself breakpoint-scoped
 *  (`aria-disabled:md:opacity-0`), and a stacked variant that silently
 *  fails to compile leaves a live-looking control that does nothing — a
 *  failure invisible to `npm run check` and to any test that does not
 *  read computed styles. Two named strings switched in the markup
 *  cannot half-apply. */
export const RAIL_ARROW_CLASS =
  'absolute top-1/2 z-[3] grid h-10 w-10 -translate-y-1/2 place-items-center rounded-full ' +
  'border border-border bg-surface/90 text-fg shadow-md backdrop-blur-sm transition ' +
  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:opacity-100';

/** Live: revealed on pointer hover over the strip on a pointer-ish
 *  viewport, always visible below `md` where there is no hover to speak
 *  of and the arrows are the only non-drag way across. Requires
 *  `group/rail` on the positioned ancestor. */
export const RAIL_ARROW_LIVE_CLASS = 'hover:bg-surface md:opacity-0 md:group-hover/rail:opacity-100';

/** Disabled: same reveal rule as live, dimmed, and click-through-proof.
 *
 *  Revealed on hover rather than hidden outright so the pair reads as a
 *  pair — hovering the strip shows both ends, one of them plainly spent.
 *  An arrow that VANISHES at the end of its travel makes the other one
 *  jump position in the reader's peripheral vision, and gives no clue
 *  that scrolling back is the thing that brings it back.
 *
 *  `pointer-events-none` is the belt to `aria-disabled`'s braces — the
 *  handler already refuses, and this stops the cursor changing over a
 *  control that will not respond. */
export const RAIL_ARROW_DISABLED_CLASS =
  'pointer-events-none opacity-30 md:opacity-0 md:group-hover/rail:opacity-30';
