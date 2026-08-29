// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The paging rig every accumulating list surface mounts (#1159, #1354).
//
// # Why it is a module and not a second copy
//
// It was built inline on the browse wall, and #1354 is what happens
// when the next surface needs it: `/search` had a manual "Load more"
// button while the wall beside it paged itself, on the same grid, with
// the same cards, for no decided reason. The honest fix is not a second
// observer — it is one definition that the next surface inherits
// CORRECT, which is the argument `scrollportOf` already makes one file
// over.
//
// That matters more here than the duplication usually would, because
// two of the three things this does are counter-intuitive enough that
// the browse wall got both of them wrong the first time.
//
// # ⛔ THE ROOT IS THE SCROLLPORT, AND rootMargin ALONE CANNOT FIX IT
//
// The original observer used the DEFAULT root (`null`, the document
// viewport) with `rootMargin: '600px 0px'`. This app never scrolls the
// window: the shell is `overflow-hidden` with `<main class="flex-1
// overflow-y-auto">` as the real scrollport (+layout.svelte, #1122).
// `rootMargin` inflates the ROOT's rect and nothing else, while the
// intersection is still clipped by every scrolling ancestor in between,
// so `<main>`'s own unexpanded clip rect cut the 600px straight back
// off: a lookahead of approximately ZERO.
//
// MEASURED, not deduced. With the margin raised to 2700px and the root
// left implicit, an in-page rAF sampler recorded the unread buffer
// sawtoothing 3893px → 52px → 3893px — refills firing at a buffer of
// ~50-300px, not at 2700px. Raising the number could never have worked.
//
// # ⛔ THE OBSERVER IS NOT THE WHOLE TRIGGER
//
// An IntersectionObserver notifies on threshold CROSSINGS. A lookahead
// deeper than one page is tall means the sentinel is STILL inside the
// margin after an append: the intersection state never changes, no
// callback is queued, and the list stalls one page in — strictly worse
// than the bug being fixed. So the trigger is a PREDICATE over the
// sentinel's own geometry, and both edges call it: the observer when
// the reader moves, and the tail of a successful fetch when the buffer
// moves (`pump`).
//
// That makes filling a deep buffer a bounded SEQUENTIAL chase. At most
// one request is ever in flight (`busy` is the gate), and the chase
// stops the moment the buffer is covered, so the steady-state cost is a
// fixed depth of prefetched pages rather than anything that scales with
// how far the reader goes.
//
// # The depth is measured in screenfuls, not pixels
//
// `LOOKAHEAD_VIEWPORTS × the scrollport's own height` — a head-start in
// screenfuls of reading, which scales across a 390px phone and a 4k
// display without either being written down. It has to cover RENDER,
// not the wire: the wire is 21ms p50 on this stack, while painting a
// page of fresh cards on a main thread already busy scrolling is what
// costs the hundreds of milliseconds the reader was waiting on. 2.5
// screenfuls buys ~1.6s at a 1.6k px/s wheel and ~1s at 2.7k px/s,
// which measurement says is enough and 1.5 is not.

import { untrack } from 'svelte';
import { scrollportOf } from '$lib/util/scrollport';

/** Screenfuls of unread list the loader is entitled to hold ahead of
 *  the reader.
 *
 *  ⚠️ `browse-lookahead-1159.spec.ts` pins this number (its
 *  `LOOKAHEAD_PORTS`) because the walk it drives has to know how much
 *  feed the loader consumes before the measurement begins. Changing it
 *  here without changing it there makes that spec assert against a
 *  reach the route no longer has. */
export const LOOKAHEAD_VIEWPORTS = 2.5;

export interface InfiniteScrollOptions {
  /** The zero-height marker at the list's tail. Read through a getter
   *  because it is `bind:this` state that arrives after the first run
   *  and is torn down whenever the list empties. */
  sentinel: () => HTMLElement | undefined;
  /** Is there a next page to ask for? (A non-empty cursor.) */
  more: () => boolean;
  /** Is a request already in flight? At most one ever is. */
  busy: () => boolean;
  /** Fire the next page. Called at most once per pump. */
  load: () => void;
}

export interface InfiniteScroll {
  /** Top the buffer back up. Call from the tail of a successful append,
   *  after `await tick()` so the new rows have been laid out — see the
   *  "not the whole trigger" note above. */
  pump: () => void;
  /** Is there less than a lookahead's worth of unread list below the
   *  fold? Exposed for callers that need to reason about it (and for
   *  tests); `pump` already consults it. */
  wantsMore: () => boolean;
}

/**
 * Wire a sentinel up to its scrollport and keep the list ahead of the
 * reader.
 *
 * ⚠️ CALL IT FROM COMPONENT INITIALISATION. It registers an `$effect`,
 * so it has to run while there is a component to own the teardown.
 */
export function createInfiniteScroll(opts: InfiniteScrollOptions): InfiniteScroll {
  /** The box that actually clips the sentinel. `null` before mount, and
   *  on any surface where nothing above the list scrolls — in which
   *  case the viewport IS the scrollport and the observer's default
   *  root is already correct.
   *
   *  Resolved per call rather than cached, for `scrollportOf`'s own
   *  reason: the same list can render inside a modal with its own
   *  scrollport, and a stale reference here fails SILENTLY. */
  const scrollport = () => scrollportOf(opts.sentinel());
  const portHeight = () => scrollport()?.clientHeight ?? window.innerHeight;
  const lookaheadPx = () => Math.round(portHeight() * LOOKAHEAD_VIEWPORTS);

  /** Read off the sentinel, which sits at the list's tail, so it answers
   *  for whatever the list's real height turned out to be — masonry's
   *  variable tiles included. Measured against the scrollport's bottom
   *  edge for the same reason the observer is rooted there. */
  function wantsMore(): boolean {
    const node = opts.sentinel();
    if (!node) return false;
    const port = scrollport();
    const bottom = port ? port.getBoundingClientRect().bottom : window.innerHeight;
    return node.getBoundingClientRect().top <= bottom + lookaheadPx();
  }

  function pump(): void {
    // untrack: this is called from effects and from fetch tails, and it
    // reads list state that must not become a dependency of either.
    untrack(() => {
      if (!opts.more() || opts.busy()) return;
      if (!wantsMore()) return;
      opts.load();
    });
  }

  $effect(() => {
    const node = opts.sentinel();
    if (!node) return;
    let observer: IntersectionObserver | undefined;
    let raf = 0;
    let armedFor = -1;

    const arm = () => {
      armedFor = portHeight();
      observer?.disconnect();
      observer = new IntersectionObserver(
        (entries) => {
          if (entries.some((e) => e.isIntersecting)) pump();
        },
        { root: scrollport(), rootMargin: `${lookaheadPx()}px 0px` },
      );
      observer.observe(node);
    };

    // rAF-coalesced, and only when the HEIGHT moved: a drag-resize fires
    // a resize event per frame, and rebuilding an observer per frame
    // would be the same churn MasonryColumns' width guard exists to
    // avoid. A width-only change (the column count moving) cannot alter
    // a vertical lookahead. `root` and `rootMargin` are fixed at
    // construction, so this rebuilds rather than mutates.
    const onResize = () => {
      if (raf) return;
      raf = requestAnimationFrame(() => {
        raf = 0;
        if (portHeight() !== armedFor) arm();
      });
    };

    arm();
    window.addEventListener('resize', onResize);
    return () => {
      observer?.disconnect();
      window.removeEventListener('resize', onResize);
      if (raf) cancelAnimationFrame(raf);
    };
  });

  return { pump, wantsMore };
}
