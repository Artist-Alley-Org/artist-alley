// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/** The geometry behind #1207's draggable crop marquee.
 *
 *  Pure functions in their own module rather than `$derived` blocks
 *  inside the editor, for one reason: this is the arithmetic that has
 *  to agree with what the browser's own `object-fit: cover` does, to
 *  the pixel, on a surface where being close is indistinguishable from
 *  being right. A `$derived` inside a component can only be checked by
 *  driving a browser; these can be checked by a unit test AND driven in
 *  a browser, and #1195's preview was verified by exactly that pairing.
 */

/** The featured card's aspect, as one number.
 *
 *  Mirrors FeaturedRail's CARD_ASPECT ('890 / 500'). The strip is
 *  locked to it (#1110/#1098), so it is a constant rather than a
 *  parameter of the component — but it IS a parameter of the functions
 *  below, so a future second crop shape does not need them rewritten.
 */
export const CARD_ASPECT = 890 / 500;

/** How much of the picture the card actually shows, per axis, as a
 *  fraction in (0, 1].
 *
 *  `object-fit: cover` scales the picture to COVER the box, so exactly
 *  one axis overflows and gets trimmed; the other is shown whole and
 *  its fraction is 1. Which one depends on whether the picture is
 *  wider or narrower than the card.
 *
 *  This is the marquee's SIZE. The marquee is not an approximation of
 *  the crop drawn beside it — it is the same rectangle, expressed as a
 *  fraction of the same picture.
 */
export function cropWindow(
  naturalAspect: number,
  cardAspect: number = CARD_ASPECT,
): { w: number; h: number } {
  if (!(naturalAspect > 0) || !(cardAspect > 0)) return { w: 1, h: 1 };
  return naturalAspect > cardAspect
    ? { w: cardAspect / naturalAspect, h: 1 }
    : { w: 1, h: naturalAspect / cardAspect };
}

/** Where the marquee's top-left corner sits, as a fraction of the
 *  picture, for a stored focal value.
 *
 *  ⚠️ THE STORED FRACTION IS THE `object-position` FRACTION, NOT THE
 *  MARQUEE'S CENTRE IN PICTURE COORDINATES. The two are different
 *  numbers and it is worth being explicit about which one #1207 chose,
 *  because both are defensible and the code reads the same either way.
 *
 *  CSS defines `object-position: X%` as "align the X% point of the
 *  picture with the X% point of the box", which for a covered box
 *  works out to `origin = X * (1 - window)`: 0 pins the marquee to the
 *  left edge, 1 to the right, 0.5 centres it. So a stored 0.25 means
 *  "a quarter of the way through the available travel", not "a quarter
 *  of the way across the picture".
 *
 *  The alternative — store the marquee's centre in picture coordinates
 *  and convert — describes a POINT OF INTEREST, which survives the card
 *  aspect changing. It was not chosen because it is partial where this
 *  is total: its reachable range is [window/2, 1 - window/2], which
 *  collapses to a single value for a picture that is already
 *  card-shaped, and the inverse conversion divides by (1 - window),
 *  which is zero for exactly that picture. Every value this function
 *  accepts is reachable and meaningful, and the degenerate picture
 *  simply has no travel — which the editor says out loud instead of
 *  guarding against a division.
 *
 *  The issue's own wording ("maps directly to object-position") settles
 *  it the same way.
 */
export function marqueeOrigin(focal: number, windowFraction: number): number {
  return clamp01(focal) * (1 - windowFraction);
}

/** The inverse: the focal value that puts the marquee's top-left corner
 *  at `origin`. Used while dragging, where the pointer produces a
 *  position and the model wants a focal.
 *
 *  An axis with no travel (`windowFraction` 1 — the picture is already
 *  card-shaped on that axis) has no answer, and returns the centre
 *  rather than dividing by zero. Returning 0.5 rather than throwing is
 *  right because that axis is genuinely unpositionable: every value
 *  renders identically, so the honest one is the neutral one.
 */
export function focalFromOrigin(origin: number, windowFraction: number): number {
  const travel = 1 - windowFraction;
  if (travel <= 0) return 0.5;
  return clamp01(origin / travel);
}

/** Is there anything to drag on this axis? False when the picture is
 *  already card-shaped there, to within a fraction of a pixel on a
 *  1000px-wide stage. */
export function hasTravel(windowFraction: number): boolean {
  return 1 - windowFraction > 0.0005;
}

/** The `object-position` value for a stored focal pair, with null
 *  meaning centre — the CSS default, and what every tile rendered
 *  before #1207.
 *
 *  Returned as a complete CSS value rather than two numbers so there is
 *  ONE place that decides what null means. The rail, the editor's live
 *  preview and the static crop preview all call this; a component that
 *  spelled out `${(x ?? 0.5) * 100}%` locally would be a second copy of
 *  the default, and the second copy is the one that gets it wrong when
 *  the default changes.
 */
export function objectPosition(x: number | null | undefined, y: number | null | undefined): string {
  return `${clamp01(x ?? 0.5) * 100}% ${clamp01(y ?? 0.5) * 100}%`;
}

function clamp01(v: number): number {
  if (!Number.isFinite(v)) return 0.5;
  return v < 0 ? 0 : v > 1 ? 1 : v;
}
