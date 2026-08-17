// rail-drag-1138.spec.ts
//
// The per-consumer drag guard `nativeDrag.ts` never had (#1138 item 4).
//
// # Why this file exists
//
// `cancelNativeDrag` has three consumers — the featured strip and the
// teams rail (both through `railScroll`), and the browse wall's marquee.
// Sprint 24 extracted the drag into `railScroll`, sprint 26 extracted
// the dragstart cancel into `nativeDrag`, and after the second
// extraction NOBODY re-drove the featured strip. Both refactors passed
// every unit test in the repo, because no test in the repo could see
// this failure: it is a race between the browser's own drag-and-drop and
// a sub-threshold pointermove, and it has no representation in a DOM
// stub. `npm run check` cannot see it either.
//
// So the guard has to be a real browser holding a real mouse, and it has
// to exist ONCE PER CONSUMER — a shared-helper test would have passed
// throughout the exact window in which a consumer was broken.
//
// # What the failure actually looks like, and what is asserted
//
// Measured (#1138 grounding, Chromium, with the cancel removed): a 320px
// drag beginning on a card's artwork pans **0px**, the document sees a
// **pointercancel**, and the pointermove count collapses from ~161 to 3.
// The same drag beginning on the GAP between two cards pans the full
// 320px. That asymmetry is the signature — the native image drag can
// only win when the gesture BEGINS on an `<img>`.
//
// Each test therefore asserts all three of:
//
//   1. the strip actually panned (the user-visible symptom),
//   2. no `pointercancel` reached the document (the mechanism — this is
//      what separates "the cancel is bound" from "the drag happened to
//      work because nothing was draggable under the cursor"),
//   3. the gesture STARTS ON AN IMAGE. Pressing anywhere else tests
//      nothing: the gap-origin arm passes on the broken code.
//
// # Why the movement is delivered in 2px samples
//
// The threshold pattern's first sample must be BELOW `DRAG_THRESHOLD`
// (5px), because that is the sample that returns early without calling
// `preventDefault` and hands the browser its window to start a native
// drag. A test that jumps 20px per sample crosses the threshold on move
// one and can pass on code that would fail under a real hand. This was
// verified both ways during grounding.
//
// # Chromium is the engine that matters
//
// Firefox's default image-drag behaviour differs and the bug does not
// bite there (#1138 owner matrix). The standalone project runs Chromium,
// so the acceptance case already targets the right engine — but the
// cancel is written to be engine-agnostic rather than relying on that.

import type { Page } from '@playwright/test';

import { test, expect } from '../../helpers/test';
import { tid } from '../../helpers/testids';

/** Movement per pointer sample. See the header: 2px keeps the first
 *  sample under the 5px threshold, which is the whole point. */
const STEP = 2;
const TRAVEL = 320;

/** Arm the document-level probe. `pointercancel` is the native drag's
 *  fingerprint; `moves` catches a gesture that dies without one. */
async function armProbe(page: Page) {
  await page.evaluate(() => {
    const w = window as unknown as { __dragProbe?: { cancels: number; moves: number } };
    w.__dragProbe = { cancels: 0, moves: 0 };
    document.addEventListener('pointercancel', () => w.__dragProbe!.cancels++, true);
    document.addEventListener('pointermove', () => w.__dragProbe!.moves++, true);
  });
}

async function readProbe(page: Page) {
  return page.evaluate(() => {
    const w = window as unknown as { __dragProbe?: { cancels: number; moves: number } };
    return w.__dragProbe ?? { cancels: 0, moves: 0 };
  });
}

/**
 * Press at (x, y), travel `TRAVEL` px left in `STEP` px samples, release.
 *
 * Not `page.mouse.move(x, y, { steps: n })` in one call: that delivers
 * evenly-spaced samples but the FIRST one is `TRAVEL / n` px, and the
 * whole point is a first sample under the threshold. The loop makes the
 * sample size explicit and independent of the distance.
 */
async function dragLeftFrom(page: Page, x: number, y: number) {
  await page.mouse.move(x, y);
  await page.mouse.down();
  for (let travelled = STEP; travelled <= TRAVEL; travelled += STEP) {
    await page.mouse.move(x - travelled, y, { steps: 1 });
  }
  await page.mouse.up();
}

/**
 * Where to press inside a rail item.
 *
 * Prefers an `<img>` — that is the one press position that can
 * reproduce this bug, because only a natively-draggable element under
 * the pointer gives the browser a drag to start. Falls back to the first
 * item itself when NO item in the rail renders one, and reports which it
 * got so a caller can insist.
 *
 * The fallback is not a loophole. A consumer with no draggable content
 * cannot exhibit the #1138 failure at all today, but it is still a
 * consumer of the same gesture and still needs to be DRIVEN — that is
 * the guard the issue asks for. What must never happen is a test
 * silently taking the fallback on the surface where the bug was
 * actually reported, which is why the featured case asserts `onImage`.
 *
 * # Why it scans the rail instead of pinning to item 0 (#1180)
 *
 * It used to take `.first()` and nothing else, which quietly coupled
 * this test to WHICH tile the seed happens to put in slot 0 and to
 * WHETHER that tile's picture had finished rendering. Both move.
 *
 * FeaturedRail has two legitimate arms: an `<img>` card, and a
 * title-only plate for a cover with no servable `col` variant — an
 * empty collection, a cover above the public tier (#559), or a variant
 * that is not there yet (#1110). The plate is correct behaviour, not a
 * failure.
 *
 * On the CI seed profile the first featured collection resolves its
 * cover to a `.glb`, whose `col` comes from `preview.3d` — the one
 * render kind ui-pr.yml deliberately does NOT wait for (it is the slow
 * tail, and gating on it blew the job budget; see the drain step). So
 * slot 0 is a plate for as long as that queue is draining, and this
 * test was a coin flip on how far it had got. Verified: the `.glb`
 * cover is what BOTH dev and #1180 select — the coverage profile picks
 * a byte-identical set on each — so the flake was latent, not
 * introduced.
 *
 * What #1138 needs is A natively-draggable element in this rail, not
 * specifically the first one. Scanning for it removes the timing
 * coupling without weakening anything: if no item anywhere in the rail
 * has a visible image, `onImage` is still false and the featured case
 * still fails loudly.
 *
 * Only items actually inside the viewport are considered — the rail
 * overflows by design, and `page.mouse` cannot press a tile that is
 * scrolled off to the right.
 */
async function firstItemPressPoint(page: Page, itemTestId: string) {
  const items = page.locator(tid(itemTestId));
  await items.first().waitFor({ state: 'visible', timeout: 15_000 });

  const viewport = page.viewportSize();
  const maxX = viewport?.width ?? Number.MAX_SAFE_INTEGER;
  const count = await items.count();

  for (let i = 0; i < count; i++) {
    const img = items.nth(i).locator('img').first();
    if ((await img.count()) === 0 || !(await img.isVisible())) continue;
    const box = await img.boundingBox();
    if (!box || box.width === 0 || box.height === 0) continue;
    if (box.x < 0 || box.x + box.width > maxX) continue;
    return { x: box.x + box.width / 2, y: box.y + box.height / 2, onImage: true, index: i };
  }

  const box = await items.first().boundingBox();
  expect(box, `${itemTestId} has no box to press`).not.toBeNull();
  return { x: box!.x + box!.width / 2, y: box!.y + box!.height / 2, onImage: false, index: 0 };
}

/** Drive one railScroll consumer end to end and assert the three
 *  properties in the header. Shared by the two rails because they are
 *  the same gesture — but CALLED ONCE PER CONSUMER, which is the part
 *  that matters. */
async function assertRailPans(
  page: Page,
  scrollerTestId: string,
  itemTestId: string,
  { requireImage }: { requireImage: boolean },
) {
  const scroller = page.locator(tid(scrollerTestId));
  await expect(scroller).toBeVisible();

  // A rail with nothing to scroll refuses the gesture by design
  // (`onPointerDown` returns early when `scrollWidth <= clientWidth`),
  // and a test that pressed it anyway would assert 0px panned and read
  // as the bug. Fail loudly instead of passing vacuously.
  const overflows = await scroller.evaluate((el) => el.scrollWidth > el.clientWidth + 1);
  expect(
    overflows,
    `${scrollerTestId} does not overflow at this viewport, so there is no pan to test — ` +
      `widen the content or narrow the viewport rather than letting this pass silently`,
  ).toBe(true);

  await scroller.evaluate((el) => (el.scrollLeft = 0));
  await armProbe(page);

  const pt = await firstItemPressPoint(page, itemTestId);
  if (requireImage) {
    expect(
      pt.onImage,
      `no ${itemTestId} in the viewport rendered a visible <img>, so this drag would start ` +
        `somewhere that CANNOT reproduce #1138 — the regression only bites when the gesture ` +
        `begins on a natively draggable element. Every tile in the rail is a title-only plate: ` +
        `either the covers are gated / missing, or the preview queue has not produced a single ` +
        `col variant. Fix the fixture rather than relaxing this.`,
    ).toBe(true);
  }
  await dragLeftFrom(page, pt.x, pt.y);
  await page.waitForTimeout(200);

  const panned = await scroller.evaluate((el) => el.scrollLeft);
  const probe = await readProbe(page);

  expect(
    probe.cancels,
    'the document saw a pointercancel — the browser started its own image drag and killed the ' +
      'gesture, which is exactly what cancelNativeDrag exists to prevent (#1138)',
  ).toBe(0);
  // Not the full 320: a rail may hit its own end first. A pan that
  // travelled less than one card is the failure — the broken code
  // managed 0-20px.
  expect(
    panned,
    `${scrollerTestId} panned ${panned}px from a press on a card image; a working pan moves ` +
      `hundreds of px and the regression moved 0-20 (${probe.moves} pointermoves observed)`,
  ).toBeGreaterThan(100);
}

test.describe('#1138 — every nativeDrag consumer pans from a press on artwork', () => {
  test('consumer 1: the featured strip (railScroll)', async ({ page }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await page.goto('/');
    // requireImage — this IS the surface #1138 was reported on, and a
    // gap-origin gesture passes on the broken code.
    await assertRailPans(page, 'featured-rail-scroller', 'featured-rail-item', {
      requireImage: true,
    });
  });

  test('consumer 2: the teams rail (railScroll)', async ({ page }) => {
    // Narrower than the featured case ON PURPOSE. The teams rail holds
    // short chips and does not overflow a 1920 viewport on the seeded
    // instance, and a rail that fits has no pan to exercise — the
    // overflow assertion above would (correctly) fail the test rather
    // than let it pass having driven nothing.
    await page.setViewportSize({ width: 900, height: 1080 });
    await page.goto('/');
    // requireImage: false, deliberately and with a reason. A teams chip
    // draws `TeamAvatar`, which is an `<img>` only for a team that has
    // uploaded one and initials text otherwise — and the seeded studios
    // have none, so there is no draggable element to press. This
    // consumer therefore cannot exhibit #1138's failure today; it can
    // still lose its pan to the next shared-helper refactor, and being
    // driven is what this test is for. The day a seeded team gets an
    // avatar, `firstItemPressPoint` starts pressing it with no edit here.
    await assertRailPans(page, 'teams-rail-scroller', 'teams-rail-chip', { requireImage: false });
  });

  test('consumer 3: the browse wall marquee', async ({ page }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await page.goto('/');

    const wall = page.locator(tid('browse-wall'));
    await expect(wall).toBeVisible();
    const img = wall.locator('img').first();
    await img.waitFor({ state: 'visible', timeout: 15_000 });
    const box = await img.boundingBox();
    expect(box).not.toBeNull();

    await armProbe(page);

    // The band is a DRAG, so it is driven the same way — press on the
    // artwork, small samples, and hold the pointer down while the
    // assertion runs. Released too early and there is no band left to
    // see; the marquee tears itself down on pointerup.
    const x = box!.x + box!.width / 2;
    const y = box!.y + box!.height / 2;
    await page.mouse.move(x, y);
    await page.mouse.down();
    for (let travelled = STEP; travelled <= 240; travelled += STEP) {
      await page.mouse.move(x + travelled, y + travelled, { steps: 1 });
    }

    const band = page.locator(tid('marquee-band'));
    await expect(
      band,
      'no rubber band formed from a press on a card image — the marquee is the third ' +
        'nativeDrag consumer and this is the failure #1127 reported (#1138)',
    ).toBeVisible();

    await page.mouse.up();
    const probe = await readProbe(page);
    expect(
      probe.cancels,
      'the document saw a pointercancel during the band gesture — the native image drag won',
    ).toBe(0);
  });
});
