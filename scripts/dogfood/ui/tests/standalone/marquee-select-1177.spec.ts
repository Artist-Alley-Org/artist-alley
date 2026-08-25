// marquee-select-1177.spec.ts
//
// #1177 — the marquee band selects ASSET cards, not just post cards.
//
// # What was wrong
//
// The band's hit-test enumerates `[data-select-id]` under its surface
// (marquee.svelte.ts `apply`). PostCard's root has carried that
// attribute since #1127. AssetCard's never did, so a marquee drawn
// across a grid of asset cards matched ZERO nodes and selected nothing
// — silently, because the band still drew and still committed, it just
// committed an empty set.
//
// Checkbox clicks and Shift+range were unaffected and looked fine: both
// go through CardCheckbox into the selection store without ever
// consulting the DOM attribute. Only the band was blind, which is why
// the gap survived the surface being built.
//
// The profile also had no marquee mounted at all — the controller was
// wired to the browse wall alone — so the fix is the attribute plus the
// wiring, and this spec drives both.
//
// # Why this spec is a real browser, and why it is not a unit test
//
// The failure is a hit-test between two live rectangles: the band's
// document-space AABB and each card's `getBoundingClientRect()`. A DOM
// stub has no layout, so every rect is zero and the test would pass on
// the broken code — the exact class of green that let this ship. It
// needs a real grid, at a real size, with a real pointer dragged across
// it.
//
// # The load-bearing assertion
//
// `idsIntersecting` computes which asset cards the band actually
// crossed, geometrically, and the test then asserts EVERY one of them
// came back checked. Asserting "the selection is non-empty" would pass
// on a mixed sweep purely from the post cards — which is precisely the
// bug, since posts were never broken. The mixed test therefore counts
// each kind separately.
//
// Proving it is red without the fix: delete `data-select-id` from
// AssetCard's root and re-run. Verified — the uploads sweep fails
// waiting for cards carrying the attribute, and the mixed sweep times
// out waiting for an asset card to come back selected.

// The extended `test`, for its post-hydration `goto` — the profile is a
// client-rendered route and every locator here is a laid-out box.
// The admin session comes from the standalone project's storageState.
import { test, expect } from '../../helpers/test';
import type { Page, Locator } from '@playwright/test';

/** The signed-in admin's own profile — the uploads grid is `isSelf`
 *  only (#1106), so a visitor's page has no asset cards to sweep. */
const PROFILE = '/users/by-username/admin';

/** Movement is delivered in small samples rather than one jump: the
 *  marquee arms on a threshold crossing and rebuilds the band per
 *  pointermove, and a single large move can cross the whole grid in one
 *  event before the controller has armed. */
const STEP = 24;

/** Ids of the cards whose boxes intersect the given band rectangle,
 *  computed the same way the controller does (plain AABB overlap in
 *  viewport space, which is what the page is in while nothing scrolls). */
async function idsIntersecting(
  cards: Locator,
  band: { x1: number; y1: number; x2: number; y2: number },
): Promise<string[]> {
  const left = Math.min(band.x1, band.x2);
  const right = Math.max(band.x1, band.x2);
  const top = Math.min(band.y1, band.y2);
  const bottom = Math.max(band.y1, band.y2);

  const out: string[] = [];
  const n = await cards.count();
  for (let i = 0; i < n; i++) {
    const card = cards.nth(i);
    const box = await card.boundingBox();
    if (!box) continue;
    if (box.x < right && box.x + box.width > left && box.y < bottom && box.y + box.height > top) {
      const id = await card.getAttribute('data-select-id');
      if (id) out.push(id);
    }
  }
  return out;
}

/** The selection store's live contents, read from the checked cards
 *  themselves rather than from the store — the user-visible answer. */
async function checkedIds(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll('[data-select-id]'))
      .filter((el) => el.querySelector('[role="checkbox"][aria-checked="true"]'))
      .map((el) => (el as HTMLElement).dataset.selectId ?? '')
      .filter(Boolean),
  );
}

/** Drag a band from (x1,y1) to (x2,y2) and release. */
async function sweep(page: Page, x1: number, y1: number, x2: number, y2: number) {
  await page.mouse.move(x1, y1);
  await page.mouse.down();
  const dx = x2 - x1;
  const dy = y2 - y1;
  const steps = Math.max(1, Math.ceil(Math.max(Math.abs(dx), Math.abs(dy)) / STEP));
  for (let i = 1; i <= steps; i++) {
    await page.mouse.move(x1 + (dx * i) / steps, y1 + (dy * i) / steps, { steps: 1 });
  }
  // The band must still exist while it is holding the selection —
  // released too early and there is nothing to have swept.
  await expect(page.getByTestId('marquee-band')).toBeVisible();
  await page.mouse.up();
}

/** Clear any selection carried over from a previous test in the file.
 *  The store is a global singleton that deliberately survives
 *  navigation (selection.svelte.ts), so it does not reset itself. */
async function clearSelection(page: Page) {
  await page.evaluate(() => {
    document
      .querySelectorAll<HTMLElement>('[role="checkbox"][aria-checked="true"]')
      .forEach((el) => el.click());
  });
  await expect.poll(() => checkedIds(page).then((ids) => ids.length)).toBe(0);
}

/** How many asset cards the uploads grid needs for a sweep to be a
 *  sweep rather than a click on one tile. */
const MIN_UPLOADS = 4;

/**
 * The admin OWNS assets and posts — and this spec does not make them.
 *
 * The uploads grid is `isSelf` only (#1106), so the sweep needs the
 * signed-in admin to be the uploader, and every seeded asset belongs to
 * one of the fictional artists. This used to top the admin up to
 * MIN_UPLOADS in `beforeEach` — four text assets and four posts, created
 * on a fresh database and DELETED BY NOTHING: `DELETE` is a soft delete
 * on both tables, so no teardown could have returned the corpus to where
 * it started. On the long-lived coding stack the cost was paid once and
 * invisible; on a fresh database it is paid every run, which is what
 * kept the suite-level corpus census out of CI (#1245, #1263).
 *
 * `aa seed --fixtures` writes the plates now (app/internal/seed/
 * fixtures.go). This asserts they are there.
 *
 * ⛔ IT MUST NOT FALL BACK TO CREATING THEM. A top-up branch would make
 * this pass on an unseeded instance and put the leak straight back —
 * silently, and only on the machines where nobody is looking.
 */
async function requireAdminFixture(page: Page) {
  const assets = await page.request.get('/api/v1/assets?owner_ref=1&limit=50');
  expect(assets.ok(), 'reading the admin\'s own assets').toBe(true);
  const owned = ((await assets.json()).items ?? []).length;
  expect(
    owned,
    `the bootstrap admin owns ${owned} asset(s); this spec needs at least ` +
      `${MIN_UPLOADS} for a sweep to be a sweep rather than a click on one tile. ` +
      `This instance was seeded without the test substrate. Re-seed with:\n\n` +
      `    aa seed --site <site> --catalogue seed/profiles --fixtures\n`,
  ).toBeGreaterThanOrEqual(MIN_UPLOADS);
}

test.describe('#1177 marquee selects asset cards', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await page.goto(PROFILE);
    await requireAdminFixture(page);
    await page.reload();
    await expect(page.getByTestId('profile-wall')).toBeVisible();
  });

  test('a sweep across the uploads grid checks the asset cards it crosses', async ({ page }) => {
    const uploads = page.getByTestId('profile-uploads');
    await expect(uploads).toBeVisible();
    await uploads.scrollIntoViewIfNeeded();

    const cards = uploads.locator('[data-select-id]');
    await expect
      .poll(() => cards.count(), {
        message:
          'the uploads grid has no cards carrying data-select-id — this is #1177 itself: ' +
          'AssetCard\'s root had no attribute for the hit-test to enumerate',
        timeout: 15_000,
      })
      .toBeGreaterThanOrEqual(MIN_UPLOADS);
    await clearSelection(page);

    // Anchor ON the first card, low enough to clear the checkbox in its
    // top-left corner, and drag right/down across the row.
    //
    // Starting on the card rather than in the gutter is deliberate and
    // is itself an assertion: the whole tile is covered by a stretched
    // <a>, and the controller refuses to arm on a press that begins on
    // a control unless the element opts out with
    // `data-marquee-passthrough`. AssetCard's anchor did not, so before
    // #1177 no band could START on an asset card at all — a gutter-only
    // press would have hidden half the bug.
    const first = await cards.first().boundingBox();
    expect(first).not.toBeNull();
    const x1 = first!.x + first!.width * 0.5;
    const y1 = first!.y + first!.height * 0.7;
    const x2 = first!.x + first!.width * 2.5;
    const y2 = first!.y + first!.height * 0.95;

    const expected = await idsIntersecting(cards, { x1, y1, x2, y2 });
    expect(
      expected.length,
      'the band rectangle crossed no asset cards — the sweep geometry is wrong, not the app',
    ).toBeGreaterThan(0);

    await sweep(page, x1, y1, x2, y2);

    const checked = await checkedIds(page);
    for (const id of expected) {
      expect(
        checked,
        `asset card ${id} was inside the band but did not get selected`,
      ).toContain(id);
    }
  });

  test('a mixed sweep down the page selects posts AND assets', async ({ page }) => {
    const posts = page.getByTestId('profile-posts');
    const uploads = page.getByTestId('profile-uploads');
    await expect(posts).toBeVisible();
    await expect(uploads).toBeVisible();
    await clearSelection(page);

    // ONE band, from a post card down through the uploads grid, using
    // the controller's own edge AUTOSCROLL to get there.
    //
    // The two grids are separated by the collections section, so they
    // do not share a viewport at any tile size — which is exactly the
    // gesture autoscroll exists for: press, drag to the bottom edge,
    // and hold while the wall moves underneath the band. Faking it by
    // scrolling first and drawing a short band would test less than the
    // real thing does.
    const postCards = page.getByTestId('profile-posts').locator('[data-select-id]');
    const assetCards = page.getByTestId('profile-uploads').locator('[data-select-id]');
    await expect.poll(() => postCards.count(), { timeout: 15_000 }).toBeGreaterThan(0);
    await expect.poll(() => assetCards.count(), { timeout: 15_000 }).toBeGreaterThan(0);

    const assetIds = new Set(
      (await assetCards.evaluateAll((els) =>
        els.map((el) => (el as HTMLElement).dataset.selectId ?? ''),
      )).filter(Boolean),
    );
    const postIds = new Set(
      (await postCards.evaluateAll((els) =>
        els.map((el) => (el as HTMLElement).dataset.selectId ?? ''),
      )).filter(Boolean),
    );
    expect(assetIds.size).toBeGreaterThan(0);
    expect(postIds.size).toBeGreaterThan(0);

    const anchor = await postCards.first().boundingBox();
    expect(anchor).not.toBeNull();
    const x1 = anchor!.x + anchor!.width * 0.5;
    const y1 = anchor!.y + anchor!.height * 0.7;

    await page.mouse.move(x1, y1);
    await page.mouse.down();
    // Into the bottom autoscroll zone (48px deep) and stay there.
    for (let y = y1; y < 1060; y += 60) {
      await page.mouse.move(x1 + 200, y, { steps: 1 });
    }
    await page.mouse.move(x1 + 200, 1060, { steps: 1 });
    await expect(page.getByTestId('marquee-band')).toBeVisible();

    // Hold while the wall scrolls past the collections section and the
    // uploads grid enters the band.
    await expect
      .poll(
        async () => (await checkedIds(page)).filter((id) => assetIds.has(id)).length,
        {
          message:
            'the band autoscrolled through the uploads grid and selected no asset card — ' +
            'the hit-test is blind to AssetCard (#1177)',
          timeout: 30_000,
        },
      )
      .toBeGreaterThan(0);

    const checked = await checkedIds(page);
    await page.mouse.up();

    expect(
      checked.filter((id) => postIds.has(id)).length,
      'no post card came back selected from the mixed sweep',
    ).toBeGreaterThan(0);
    expect(
      checked.filter((id) => assetIds.has(id)).length,
      'no asset card came back selected from the mixed sweep',
    ).toBeGreaterThan(0);
  });
});
