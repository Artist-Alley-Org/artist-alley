// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/** The scrolling ancestor of `el`, or `null` if nothing above it scrolls.
 *
 *  This app never scrolls the window. The shell is a `h-dvh …
 *  overflow-hidden` box with `<main class="flex-1 overflow-y-auto">` as
 *  the real scrollport (+layout.svelte, #1122), so anything that reasons
 *  about scroll position, scroll distance or scroll-relative geometry
 *  has to address that element rather than `window` / `document`.
 *
 *  Three call sites learned this separately and two of them wrote their
 *  own answer — the marquee's autoscroll walked up for it, the scroll
 *  snapshot reached for `document.querySelector('main')`, and the feed's
 *  infinite-scroll observer never learned it at all and silently lost
 *  its entire lookahead to `<main>`'s clip rect (#1159). One definition,
 *  so the next surface that needs it inherits the correct one.
 *
 *  Resolved per call rather than cached: the same component can render
 *  inside a modal, which has its own scrollport, and a stale reference
 *  here fails SILENTLY — the worst failure mode for geometry nobody
 *  re-measures.
 *
 *  `scrollHeight > clientHeight` is part of the test on purpose: an
 *  `overflow-y-auto` box with nothing to scroll is not the scrollport
 *  the caller means, and treating it as one would stop the walk one
 *  level too early.
 */
export function scrollportOf(el: Element | null | undefined): HTMLElement | null {
  let n: HTMLElement | null = (el as HTMLElement | null) ?? null;
  while (n) {
    const s = getComputedStyle(n);
    if (/(auto|scroll|overlay)/.test(s.overflowY) && n.scrollHeight > n.clientHeight) return n;
    n = n.parentElement;
  }
  return null;
}
