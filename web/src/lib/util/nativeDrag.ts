// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * Cancelling the browser's OWN drag-and-drop while a pointer gesture
 * owns the sequence — the one rule two gestures need and neither may
 * restate (#1047, extracted from railScroll's #1122 finding).
 *
 * # The failure it prevents, which is invisible from the code
 *
 * Both gestures built on this codebase's press-threshold pattern —
 * `railScroll`'s drag-pan and `marquee`'s rubber band — sit over content
 * full of links and images, and both are natively draggable. The first
 * pointermove of a press is BELOW the threshold, so the handler returns
 * early — correctly — WITHOUT calling preventDefault, and in that same
 * instant Chromium starts a native image/link drag. `dragstart` then
 * CANCELS THE POINTER SEQUENCE: no further pointermove is delivered, no
 * pointerup arrives, and the gesture dies one frame in while a ghost
 * image follows the cursor.
 *
 * Measured on the featured strip (#1122): a 260px drag panned 20px.
 * Measured again on the browse wall (#1047, owner report): a press-drag
 * from a card's artwork produced a ghost and either no band at all or a
 * band that stopped tracking after one frame — with `pointerup` never
 * observed by a document-level listener, which is the same signature.
 *
 * # Why cancelling `dragstart` and not `preventDefault` on pointerdown
 *
 * Preventing the default on pointerdown ALSO suppresses focus and the
 * click that follows it, which would break every item on the surface to
 * buy the same thing. The threshold pattern's whole point is that a
 * press under the threshold is left entirely alone.
 *
 * # Why it lives here
 *
 * Two callers is where a copied comment becomes a copied rule. The code
 * is one line; the reason is the part that must not be duplicated, and
 * the reason is why a third gesture built on the same threshold must
 * reach for this rather than rediscover the symptom.
 */
export function cancelNativeDrag(e: DragEvent): void {
  e.preventDefault();
}
