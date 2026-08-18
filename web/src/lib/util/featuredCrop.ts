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

/** The COLLECTION card's aspect (#1207).
 *
 *  Mirrors CollectionCard's `aspect-[4/3]` tile — the shape a chosen
 *  cover is actually cropped to on the hub, the profile and search.
 *
 *  ⚠️ NOT A SQUARE, and the correction is worth recording. The square
 *  looks like the right answer because `col` is one: `fit: cover` at
 *  320px, a 320x320 centre-crop, and it is what every small collection
 *  thumbnail is MADE of. But `col` is a SOURCE, not a destination — the
 *  tile that paints it is 4:3, so a curator positioning against a square
 *  would be shown a region the card never displays. The rule is that a
 *  crop marquee locks to the dimensions of the thing that renders it,
 *  and for a collection cover that thing is this tile. */
export const COLLECTION_CARD_ASPECT = 4 / 3;

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
  zoom: number | null | undefined = null,
): { w: number; h: number } {
  if (!(naturalAspect > 0) || !(cardAspect > 0)) return { w: 1, h: 1 };
  const fit =
    naturalAspect > cardAspect
      ? { w: cardAspect / naturalAspect, h: 1 }
      : { w: 1, h: naturalAspect / cardAspect };
  const z = clampZoom(zoom);
  return { w: fit.w / z, h: fit.h / z };
}

/** The bounds a cover zoom is clamped to (#1212), and neither end is a
 *  matter of taste.
 *
 *  MIN is geometry. The window is the fit window DIVIDED by the zoom,
 *  so anything below 1 asks for a window bigger than the picture, and
 *  there are no pixels out there. 1 is the fit itself — what every
 *  collection rendered before zoom existed.
 *
 *  MAX COMES FROM THE PREVIEW LADDER'S REAL RUNGS. A cover carrying a
 *  crop has to be painted from a CONTAIN rung: `col` is `fit: cover` at
 *  320px, a square already cropped at the centre, so positioning
 *  applied to it crops a crop. The contain rungs this install ships are
 *  `preview` 1024, `screen` 1920 and `hires` 4096
 *  (app/internal/sysconfig/previews.go), and `preview` is the one a
 *  cover is GUARANTEED to have — it is exactly what
 *  `CollectionCover.preview_available` reports. Zooming to z feeds the
 *  card 1/z of the picture's fitted width, so it demands z times the
 *  source pixels per CSS pixel; the browser answers that by climbing
 *  the srcset, and 4096 is precisely four times 1024. At 4 the ladder
 *  still has a rung to climb to; past 4 it has none, and every further
 *  step is upscaling bytes the server never made.
 *
 *  The same two numbers are the column CHECK in migration 00056 and
 *  `MinCoverZoom`/`MaxCoverZoom` in the Go handler. Three copies on
 *  purpose: the constraint is where a broken client stops, the handler
 *  is where a caller gets a 400 it can act on, and this is where the
 *  curator is stopped before either — a slider that could request a
 *  value the server refuses is a slider that produces a save error
 *  instead of a framing.
 */
export const MIN_ZOOM = 1;
export const MAX_ZOOM = 4;

/** A stored zoom as a usable multiplier. Null, undefined and NaN all
 *  mean FIT, which is 1 — the single place that decision is made, for
 *  the reason [objectPosition] centralises "null means centre". */
export function clampZoom(zoom: number | null | undefined): number {
  if (zoom == null || !Number.isFinite(zoom)) return MIN_ZOOM;
  return zoom < MIN_ZOOM ? MIN_ZOOM : zoom > MAX_ZOOM ? MAX_ZOOM : zoom;
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

/** EVERY CONSUMER'S CSS, from one place (#1212).
 *
 *  # Why `object-fit: cover` alone could not stay
 *
 *  A covered box shows the largest rectangle of its own aspect that
 *  fits the picture, and `object-position` slides that rectangle along
 *  the one axis that overflows. There is no CSS property that makes the
 *  rectangle SMALLER — `background-size` can name a size but not "cover,
 *  times two", and neither can `object-fit`. So zoom has to come from
 *  somewhere else, and there were two candidates.
 *
 *  `transform: scale()` was rejected. The featured rail's tile already
 *  carries `group-hover:scale-[1.02]`, and a second transform on the
 *  same element does not compose with it — the later declaration wins
 *  outright, so either the zoom or the hover would silently stop
 *  working depending on class order. That is a bug that looks like a
 *  CSS ordering accident rather than a decision, which is the worst
 *  kind to leave for the next reader.
 *
 *  What is used instead is the box model: the image is made z TIMES THE
 *  BOX in both axes and offset so the chosen part lands in the box,
 *  inside a clipping parent. `object-fit: cover` still does the fitting
 *  — against a box of z times the size, which selects the same shaped
 *  rectangle and then shows 1/z of it through the real box.
 *
 *  # Why the ARITHMETIC is two independent halves
 *
 *  The image is laid out against a virtual box z times the real one, so
 *  it has two sources of travel and both are needed:
 *
 *    - `object-position` moves the picture inside the virtual box, over
 *      the FIT window's travel, 1 - fit.
 *    - `left`/`top` move the virtual box across the real one, over
 *      (z - 1)/z of the fit window.
 *
 *  Driving BOTH from the same focal fraction is what makes them add up:
 *  focal x (1 - fit) + focal x fit (z - 1)/z = focal x (1 - fit/z),
 *  which is exactly `marqueeOrigin(focal, fit/z)` — the marquee's own
 *  origin against the window `cropWindow` returns for the same zoom. The
 *  editor and the surface are therefore the same equation, not two that
 *  agree.
 *
 *  # Why null zoom is byte-identical to what shipped before
 *
 *  At z = 1 this emits `width: 100%; height: 100%; left: 0; top: 0`
 *  with the same `object-position` the surfaces already had, and
 *  `transform-origin: 50% 50%`, which is the CSS default. An absolutely
 *  positioned image with those four values occupies exactly the box its
 *  `inset-0 h-full w-full` predecessor did. That is the regression this
 *  change is most at risk of and the reason the numbers are emitted
 *  rather than branched around: there is no "zoomed" code path for an
 *  unzoomed collection to fall into.
 *
 *  # Caller contract
 *
 *  The image must be `position: absolute` inside a `position: relative;
 *  overflow: hidden` box of the destination aspect. The overflow is not
 *  optional once z > 1 — without it the enlarged picture paints over
 *  whatever sits below the tile.
 *
 *  `transform-origin` is emitted for one consumer and harmless for the
 *  rest: the rail's hover scale must grow about the part of the picture
 *  the tile is SHOWING, not about the enlarged image's own centre,
 *  which at z = 4 is somewhere off-screen. The visible centre sits at
 *  (focal x (z - 1) + 0.5) / z of the image, which collapses to 50% at
 *  z = 1.
 */
export function coverPlacement(
  x: number | null | undefined,
  y: number | null | undefined,
  zoom: number | null | undefined,
): string {
  const z = clampZoom(zoom);
  const px = clamp01(x ?? 0.5);
  const py = clamp01(y ?? 0.5);
  const size = pct(z);
  return [
    `position: absolute`,
    `width: ${size}`,
    `height: ${size}`,
    `left: ${pct(-px * (z - 1))}`,
    `top: ${pct(-py * (z - 1))}`,
    `object-position: ${pct(px)} ${pct(py)}`,
    `transform-origin: ${pct((px * (z - 1) + 0.5) / z)} ${pct((py * (z - 1) + 0.5) / z)}`,
  ].join('; ');
}

/** A fraction as a percentage string, rounded to four decimals so the
 *  value is stable enough to assert on and never renders `-0%`. */
function pct(v: number): string {
  const n = Math.round(v * 1e6) / 1e4;
  return `${n === 0 ? 0 : n}%`;
}

function clamp01(v: number): number {
  if (!Number.isFinite(v)) return 0.5;
  return v < 0 ? 0 : v > 1 ? 1 : v;
}
