// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Rubber-band drag-select over a wall of cards (#1127).
//
// # The threshold is the whole design, again
//
// This is the same shape `railScroll` landed in sprint 23, for the same
// reason and with the same ordering, because the hazard is identical:
// every card under this surface is clickable, so a drag implementation
// that treats pointerdown-then-pointerup as a drag breaks opening a
// post, and one that never suppresses its click makes every sweep
// navigate. So nothing is "a marquee" until the pointer has travelled
// DRAG_THRESHOLD px; below that the gesture was a click and is left
// entirely alone.
//
// Pointer capture is taken LAZILY, at the threshold rather than at
// pointerdown, for railScroll's reason: capturing on down retargets the
// pointer immediately and the `click` a plain press produces never
// reaches the card.
//
// The click a completed marquee produces is swallowed in the CAPTURE
// phase, before the card's own handler — by the bubble phase the card
// link has already navigated.
//
// # Where a marquee may START
//
// Anywhere in the container that is not an interactive control. The
// owner's recommendation was "marquee starts anywhere the press isn't a
// plain click (threshold crossed), and once active, suppresses the
// click", which includes starting ON a card — and that is what this
// does. The one exclusion is a press that begins on a button, link or
// input: those own their own drag semantics (the ⋯ menu, the checkbox,
// the author link), and starting a rubber band from inside one would
// make them un-pressable rather than merely draggable.
//
// # Mouse only
//
// `pointerType === 'mouse'`. A marquee on a touch screen fights the
// scroll it is drawn over and loses — the same call railScroll made,
// and #1127 states it outright. Pen behaves like touch here for the same
// reason: it is used on a surface that also scrolls.

import { selection } from '$stores/selection.svelte';

/** Movement before a press becomes a marquee. Same number railScroll
 *  uses; the two gestures should feel identical up to the point they
 *  diverge. */
const DRAG_THRESHOLD = 5;

/** How close to the scrollport edge the pointer must get before the
 *  wall starts scrolling itself, and how fast it goes at the very edge.
 *  48px is roughly a thumb's width of runway — enough to enter
 *  deliberately, not so much that a drag near the bottom of the visible
 *  area scrolls when the reader did not ask. */
const AUTOSCROLL_ZONE_PX = 48;
const AUTOSCROLL_MAX_PX_PER_FRAME = 18;

export interface MarqueeRect {
  left: number;
  top: number;
  width: number;
  height: number;
}

export interface MarqueeOptions {
  /** Selector for one selectable card. Each match must carry
   *  `data-select-id`. */
  itemSelector?: string;
  /** The feed-order id list, for the anchor bookkeeping a marquee
   *  leaves behind. */
  ordered: () => string[];
}

/**
 * Marquee controller for one container.
 *
 * `getEl` is a thunk so the caller can `bind:this` into a `$state` and
 * hand it over before the node exists.
 */
export function createMarquee(getEl: () => HTMLElement | null, opts: MarqueeOptions) {
  const itemSelector = opts.itemSelector ?? '[data-select-id]';

  /** Viewport-space rect of the band, or null when not dragging. Drawn
   *  by the caller. */
  let rect = $state<MarqueeRect | null>(null);
  let active = $state(false);

  let pointerId: number | null = null;
  let startX = 0;
  let startY = 0;
  /** Where the drag began in DOCUMENT space. Kept alongside the viewport
   *  coordinates because the wall can scroll under an active marquee
   *  (edge autoscroll does exactly that), and a band anchored to a
   *  viewport point would shrink and grow as the content moved beneath
   *  it. Document space is the only frame in which "the rectangle the
   *  reader drew" stays the same rectangle. */
  let startDocX = 0;
  let startDocY = 0;
  let lastX = 0;
  let lastY = 0;
  /** Ids selected by the marquee before it started, so the band is
   *  ADDITIVE — dragging over nothing must not clear a selection the
   *  reader built by clicking. */
  let baseline: string[] = [];
  let raf: number | null = null;

  /** The scrolling ancestor. The browse wall lives inside <main>'s own
   *  `overflow-y-auto` scrollport (#1122), NOT the window, so both the
   *  autoscroll and the document-space maths have to address that
   *  element. Resolved per drag rather than cached: the same component
   *  renders inside a modal on other surfaces. */
  function scrollParent(): HTMLElement | null {
    let n: HTMLElement | null = getEl();
    while (n) {
      const s = getComputedStyle(n);
      if (/(auto|scroll|overlay)/.test(s.overflowY) && n.scrollHeight > n.clientHeight) return n;
      n = n.parentElement;
    }
    return null;
  }

  function scrollTopOf(): number {
    return scrollParent()?.scrollTop ?? 0;
  }

  /** Rebuild the band from the anchor and the live pointer, both in
   *  document space, then project back to the viewport for painting. */
  function recompute() {
    const docX = lastX;
    const docY = lastY + scrollTopOf();
    const left = Math.min(startDocX, docX);
    const top = Math.min(startDocY, docY);
    const width = Math.abs(docX - startDocX);
    const height = Math.abs(docY - startDocY);
    rect = { left, top: top - scrollTopOf(), width, height };
    apply(left, top, width, height);
  }

  /** Hit-test every card against the band and union the intersections
   *  into the baseline.
   *
   *  Recomputed from the baseline on EVERY frame rather than
   *  incrementally added: a reader who overshoots and drags back must
   *  see the cards they left behind become unselected again. An
   *  incremental union cannot do that, and "the selection only ever
   *  grows while I am still holding the button" is precisely the
   *  behaviour that makes a rubber band feel broken.
   */
  function apply(dl: number, dt: number, dw: number, dh: number) {
    const el = getEl();
    if (!el) return;
    const st = scrollTopOf();
    const hit: string[] = [];
    for (const node of el.querySelectorAll<HTMLElement>(itemSelector)) {
      const id = node.dataset.selectId;
      if (!id) continue;
      const r = node.getBoundingClientRect();
      // Card rect into document space, then a plain AABB overlap.
      const top = r.top + st;
      if (r.left < dl + dw && r.right > dl && top < dt + dh && top + r.height > dt) {
        hit.push(id);
      }
    }
    selection.ids = [...new Set([...baseline, ...hit])];
  }

  /** Scroll the wall when the pointer sits near the scrollport's top or
   *  bottom edge, and keep the band growing while it does. Runs on rAF
   *  so the speed is frame-rate shaped rather than event-rate shaped —
   *  a stationary pointer in the hot zone still scrolls. */
  function tick() {
    raf = null;
    if (!active) return;
    const sp = scrollParent();
    if (sp) {
      const r = sp.getBoundingClientRect();
      const fromTop = lastY - r.top;
      const fromBottom = r.bottom - lastY;
      let dy = 0;
      if (fromTop < AUTOSCROLL_ZONE_PX) {
        dy = -ramp(AUTOSCROLL_ZONE_PX - fromTop);
      } else if (fromBottom < AUTOSCROLL_ZONE_PX) {
        dy = ramp(AUTOSCROLL_ZONE_PX - fromBottom);
      }
      if (dy !== 0) sp.scrollTop += dy;
    }
    recompute();
    raf = requestAnimationFrame(tick);
  }

  /** Linear ramp to the cap: the deeper into the zone, the faster. A
   *  constant speed makes the near edge feel unresponsive and the far
   *  edge feel out of control. */
  function ramp(depth: number): number {
    const t = Math.min(1, Math.max(0, depth / AUTOSCROLL_ZONE_PX));
    return Math.max(1, Math.round(t * AUTOSCROLL_MAX_PX_PER_FRAME));
  }

  function stopRaf() {
    if (raf !== null) cancelAnimationFrame(raf);
    raf = null;
  }

  /**
   * True when the press began on something that owns its own clicks.
   *
   * ⚠️ THE CARD'S OWN STRETCHED LINK IS NOT ONE OF THEM, and this is
   * the whole reason the check is a function rather than one selector.
   * A card is a `<div>` covered edge to edge by an `absolute inset-0`
   * anchor (the stretched-link pattern, #515), so EVERY press on the
   * artwork lands on an `<a>` — a naive "did this start on a link"
   * test refuses to start a marquee anywhere on the wall except the
   * gaps between tiles. Measured: with the plain selector, a drag begun
   * on a card produced no band at all.
   *
   * That link is safe to drag from precisely because of the threshold:
   * a press that never travels 5px stays a click and still opens the
   * post, and one that does is swallowed in the capture phase. So the
   * stretched link opts out by marking itself, and the controls that
   * genuinely own their gesture — the ⋯ menu, the checkbox, the author
   * link, a resize handle — do not.
   */
  function onControl(target: EventTarget | null): boolean {
    const el = target as HTMLElement | null;
    const hit = el?.closest?.(
      'a,button,input,select,textarea,[role="checkbox"],[role="separator"]',
    ) as HTMLElement | null;
    if (!hit) return false;
    return hit.dataset.marqueePassthrough === undefined;
  }

  function onPointerDown(e: PointerEvent) {
    if (e.pointerType !== 'mouse' || e.button !== 0) return;
    if (onControl(e.target)) return;
    pointerId = e.pointerId;
    startX = lastX = e.clientX;
    startY = lastY = e.clientY;
    startDocX = e.clientX;
    startDocY = e.clientY + scrollTopOf();
    // NOT a marquee yet, and no capture — see the threshold note.
    active = false;
  }

  function onPointerMove(e: PointerEvent) {
    if (pointerId === null || e.pointerId !== pointerId) return;
    lastX = e.clientX;
    lastY = e.clientY;
    if (!active) {
      if (Math.abs(e.clientX - startX) < DRAG_THRESHOLD && Math.abs(e.clientY - startY) < DRAG_THRESHOLD) {
        return;
      }
      active = true;
      // Additive: whatever was already selected survives the drag.
      baseline = [...selection.ids];
      (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
      stopRaf();
      raf = requestAnimationFrame(tick);
    }
    recompute();
    // Stops the browser starting its own image/text drag off a card's
    // artwork and painting a ghost under the cursor.
    e.preventDefault();
  }

  function endDrag(e: PointerEvent) {
    if (pointerId === null || e.pointerId !== pointerId) return;
    const el = e.currentTarget as HTMLElement;
    if (el.hasPointerCapture?.(e.pointerId)) el.releasePointerCapture(e.pointerId);
    pointerId = null;
    stopRaf();
    rect = null;
    if (active) {
      // A marquee leaves a pivot behind so a following Shift+click has
      // somewhere to extend from — the last id in feed order that the
      // band caught, not the last one the hit-test happened to visit.
      const ordered = opts.ordered();
      const caught = ordered.filter((id) => selection.has(id));
      selection.setAnchor(caught.length > 0 ? caught[caught.length - 1] : null);
    }
    // `active` is NOT cleared here: the click this pointerup produces
    // has not fired yet and onClickCapture needs to know to swallow it.
    // It clears itself there, or on the next pointerdown.
  }

  function onClickCapture(e: MouseEvent) {
    if (!active) return;
    active = false;
    e.preventDefault();
    e.stopPropagation();
  }

  return {
    get rect() {
      return rect;
    },
    get active() {
      return active;
    },
    handlers: {
      onpointerdown: onPointerDown,
      onpointermove: onPointerMove,
      onpointerup: endDrag,
      onpointercancel: endDrag,
      onclickcapture: onClickCapture,
    },
  };
}
