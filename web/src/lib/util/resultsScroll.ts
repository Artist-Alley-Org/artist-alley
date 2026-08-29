// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// "Refining is a new address": put the reader back at the first row of
// the results when the result set changes identity (#1298, ADR 0056
// §3c's 2026-08-28 amendment).
//
// # What was measured, and why the obvious model is wrong
//
// On `/search`, six accumulated pages at `main.scrollTop` 4511 of a
// 6088px grid, refined to a query with 25 hits:
//
//     before   top 4511   scrollHeight 6088   clientHeight 1080
//     after    top  330   scrollHeight 1337   clientHeight 1007
//
// `330` is exactly `scrollHeight - clientHeight`. The reader lands at
// the BOTTOM of the new result set, looking at its last, part-filled
// row and the end of the list, with the first 25 hits — the whole
// answer to the question they just asked — above the fold and unseen.
//
// It is not always a clamp. #1298 recorded the other outcome on a
// taller refined wall: Chrome's scroll anchoring re-resolves the offset
// against content that reflowed above the viewport, and it landed on 0
// on one machine and on 279 (39px FURTHER DOWN than the 240 it started
// at) on the CI runner. Both are legitimate anchoring outcomes and
// neither is expressible as `min(before, max)`. Which is the point: the
// destination was never DECIDED, so it varied by machine.
//
// # ⛔ ADDRESS THE SCROLLPORT, NOT THE WINDOW
//
// `window.scrollTo` is the wrong instrument here and will silently do
// nothing: this app never scrolls the window (`scrollport.ts`, #1122).
// The scrollport is resolved through `scrollportOf` for the reason that
// file gives — three call sites learned this separately and two wrote
// their own answer.
//
// # Why the whole scrollport, and not just the grid's own top
//
// ADR 0056's amendment says the results region resets to its first row
// and the page CHROME does not move. On both surfaces the chrome the
// reader's hands are on — the nav search box, the filter menu, the view
// controls — is in the fixed shell or in the floating footer, neither
// of which is inside the scrollport at all. So scrolling the scrollport
// to its top moves exactly the results and nothing the reader is
// holding, and it is also the answer that leaves the paging sentinel
// furthest off screen (#1354).
//
// # It is deliberately not a smooth scroll
//
// The reader asked a new question and this is the answer arriving, not
// a journey through the old one. An animated 4511px climb also fights
// the swap that is happening underneath it, and `prefers-reduced-motion`
// would have to turn it off anyway.

import { scrollportOf } from '$lib/util/scrollport';

/**
 * Put the results region back to its first row.
 *
 * `anchor` is any element inside the results region — the grid's own
 * wrapper is the natural one. It is only used to find the scrollport,
 * so it does not have to be the first row itself.
 *
 * A no-op when nothing above the anchor scrolls (`scrollportOf` returns
 * null for a list that does not overflow), which is the correct answer:
 * a result set shorter than one screen has no offset to reset.
 */
export function resetResultsScroll(anchor: Element | null | undefined): void {
  const port = scrollportOf(anchor);
  if (!port) return;
  // Written unconditionally rather than guarded on `scrollTop !== 0`:
  // the guard would save nothing and the assignment is idempotent.
  port.scrollTop = 0;
}
