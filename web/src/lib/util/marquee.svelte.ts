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
import { cancelNativeDrag } from '$lib/util/nativeDrag';
import { scrollportOf } from '$lib/util/scrollport';

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
  /** The element holding the pointer capture, remembered from the frame
   *  the threshold was crossed. Escape has no event target to read it
   *  off, and releasing capture on the wrong element leaves the pointer
   *  retargeted for the rest of the press. */
  let capturedEl: HTMLElement | null = null;
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
   *  element. The walk itself moved to `scrollportOf` when the feed's
   *  infinite-scroll observer turned out to need the same answer
   *  (#1159); same algorithm, one copy. */
  function scrollParent(): HTMLElement | null {
    return scrollportOf(getEl());
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

  /** Hit-test every card against the band, then XOR the hits against the
   *  pre-gesture snapshot.
   *
   *  THE BAND TOGGLES; IT DOES NOT ONLY CHECK (owner refinement to
   *  #1127). It used to union the hits into the baseline, so sweeping a
   *  band over already-checked cards did nothing to them and there was
   *  no gesture that could clear a block of a selection except clicking
   *  each one. Every desktop file manager answers this with a toggling
   *  band, and the owner asked for the same.
   *
   *  XOR IS THE WHOLE RULE, and it is what makes the gesture reversible:
   *
   *      displayed(id) = snapshot(id) XOR inBand(id)
   *
   *  so a checked card inside the band previews as unchecked, an
   *  unchecked one previews as checked, and a card that the band sweeps
   *  ACROSS and off again lands back on exactly what it was — live,
   *  before release. Release commits what is on screen, because what is
   *  on screen is already the answer.
   *
   *  Recomputed from the snapshot on EVERY frame rather than applied
   *  incrementally, which is the property that buys all of the above: an
   *  incremental toggle would flip a card once per frame it spent under
   *  the band, and "the selection flickers while I hold the button" is
   *  precisely the behaviour that makes a rubber band feel broken.
   *
   *  Ids in the snapshot that no card on this page carries — a selection
   *  built on a previous page of an infinite scroll — are outside the
   *  band by construction and survive untouched, which is the same
   *  answer the union gave them.
   */
  function apply(dl: number, dt: number, dw: number, dh: number) {
    const el = getEl();
    if (!el) return;
    const st = scrollTopOf();
    const inBand = new Set<string>();
    for (const node of el.querySelectorAll<HTMLElement>(itemSelector)) {
      const id = node.dataset.selectId;
      if (!id) continue;
      const r = node.getBoundingClientRect();
      // Card rect into document space, then a plain AABB overlap.
      const top = r.top + st;
      if (r.left < dl + dw && r.right > dl && top < dt + dh && top + r.height > dt) {
        inBand.add(id);
      }
    }
    const next: string[] = [];
    for (const id of baseline) {
      if (!inBand.has(id)) next.push(id); // was on, band leaves it on
    }
    const was = new Set(baseline);
    for (const id of inBand) {
      if (!was.has(id)) next.push(id); // was off, band turns it on
    }
    selection.ids = next;
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

  /** Kill the browser's own drag-and-drop for the press this controller
   *  owns (#1047 fix-forward on #1127).
   *
   *  THIS WAS MISSING AND THE MARQUEE DID NOT WORK WITHOUT IT. The whole
   *  argument — why the native drag wins the race, and why the fix is
   *  here and not a `preventDefault` on pointerdown — lives in
   *  `cancelNativeDrag`, because railScroll hit the identical wall in
   *  #1122 and a copied comment is how one rule becomes two.
   *
   *  CONDITIONAL on this controller holding the pointer, deliberately,
   *  where the rail's arm is unconditional. `pointerId` is set only by a
   *  press that could become a band — `onPointerDown` returns early on a
   *  control press and on touch — so a genuine drag-and-drop that begins
   *  on some future draggable inside the wall is untouched, and the ONE
   *  press this gesture claimed is the one whose ghost is suppressed. An
   *  unconditional arm here would silently make the whole browse wall
   *  undraggable, which is a bigger promise than the bug asked for. */
  function onDragStart(e: DragEvent) {
    if (pointerId === null) return;
    cancelNativeDrag(e);
  }

  /** Escape while a band is live puts the selection back exactly as it
   *  was and drops the gesture (owner refinement to #1127).
   *
   *  A toggling band can now UNCHECK things, so "I started this drag by
   *  accident" needs an exit that is not "drag back out the way you came
   *  in" — which is impossible once the pointer has left the wall, and
   *  hopeless once the wall has autoscrolled. Restoring the snapshot IS
   *  the undo, and it is complete precisely because `apply` never
   *  mutated anything else.
   *
   *  The gesture is torn down rather than merely reset, so the pointer
   *  the reader is still holding cannot re-enter the band on its next
   *  move. The click that a following pointerup produces is left alone:
   *  `active` is false by then, so nothing swallows it, and the press
   *  the reader cancelled opens the card under it — which is the same
   *  thing a plain sub-threshold press would have done. */
  function onKeyDown(e: KeyboardEvent) {
    if (e.key !== 'Escape' || !active) return;
    e.preventDefault();
    e.stopPropagation();
    selection.ids = [...baseline];
    cancel();
  }

  /** Drop the gesture without committing anything. Shared by Escape and
   *  by `endDrag`'s teardown so the two cannot fall out of step. */
  function cancel() {
    window.removeEventListener('keydown', onKeyDown, true);
    if (capturedEl && pointerId !== null && capturedEl.hasPointerCapture?.(pointerId)) {
      capturedEl.releasePointerCapture(pointerId);
    }
    capturedEl = null;
    pointerId = null;
    active = false;
    stopRaf();
    rect = null;
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
      // THE PRE-GESTURE SNAPSHOT. Everything the band does is expressed
      // against this and nothing else — see `apply` for the XOR, and
      // `cancel` for the fact that restoring it is the whole undo.
      baseline = [...selection.ids];
      capturedEl = e.currentTarget as HTMLElement;
      capturedEl.setPointerCapture(e.pointerId);
      // Escape is armed only while a band is live, and torn down with
      // it: a document-level key handler that outlived the gesture would
      // eat Escape from every modal on the page.
      window.addEventListener('keydown', onKeyDown, true);
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
    const wasActive = active;
    // `cancel` clears `active`, and onClickCapture still needs to know a
    // band completed so it can swallow the click this pointerup will
    // produce — so it is restored immediately afterwards. It clears
    // itself there, or on the next pointerdown.
    cancel();
    active = wasActive;
    if (active) {
      // A marquee leaves a pivot behind so a following Shift+click has
      // somewhere to extend from — the last id in feed order that the
      // band caught, not the last one the hit-test happened to visit.
      const ordered = opts.ordered();
      const caught = ordered.filter((id) => selection.has(id));
      selection.setAnchor(caught.length > 0 ? caught[caught.length - 1] : null);
    }
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
      ondragstart: onDragStart,
      onclickcapture: onClickCapture,
    },
  };
}
