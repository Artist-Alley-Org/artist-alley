// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1223 — the page behind an open dialog does not move.
//
// ⚠️ TWO PREMISES IN THE ISSUE ARE WRONG AND THE TESTS ARE SHAPED BY BOTH.
//
// 1. There is no document scroller. The shell is `<div class="h-dvh …
//    overflow-hidden">` with `<main class="flex-1 overflow-y-auto">`
//    inside it, so `document.scrollingElement.scrollTop` is 0 on every
//    route. Everything below reads <main>.
//
// 2. Modal.svelte is not a native `<dialog>` — it is a portalled `<div
//    role="dialog">`, so there is no native inertness to lean on and the
//    diagnosis is not "Chrome routes wheel past an inert dialog".
//
// ⛔ AND IT DOES NOT REPRODUCE AT EVERY VIEWPORT. The overlay is
// `position: fixed` and parented to <body>; Chrome resolves its scroll
// chain against the viewport and only reaches <main> when <main> has
// been promoted to the document's effective root scroller — which
// happens when it fills the viewport. Measured on the pre-fix build,
// dialog open, <main> parked at 400:
//
//   390x780    wheel over the backdrop -> 3400
//   1280x600   wheel over the backdrop -> 2831
//   1920x1080  wheel over the backdrop ->  400   (no movement)
//
// So "the wheel did not move it" at 1080p passes on the broken build and
// proves nothing. The 1080p wheel case still runs — the lock has to hold
// there too — but the assertion that DISCRIMINATES at every viewport is
// the lock itself, and it gets its own test below.
//
// ⛔⛔ NOTHING HERE IS CALIBRATED TO A CORPUS, AND THE FIRST VERSION WAS.
//
// It hardcoded `PARK = 400`, measured against the dev stack's seeded
// catalogue (2015 live assets, ~4000px of scroll range). CI's database is
// rebuilt every run and holds 150 — its census reads `150|87|7|27|32` —
// and the three scrolling tests all failed there, through every retry, on
// the assertion that the page MOVES AGAIN after the dialog closes. The
// lock was fine; the page simply had nowhere left to go above 400.
//
// Measured across three seeded collections on the dev stack, which is the
// spread a fresh database lands somewhere inside:
//
//   9 posts     1920x1080 range =    0     390x780 range =    0
//   29 posts    1920x1080 range =  563     390x780 range =  763
//   120 posts   1920x1080 range = 4012     390x780 range = 4193
//
// A nine-post collection cannot scroll AT ALL at either viewport. So the
// park point and the room above it are DERIVED from the page's own
// measured maximum, the deepest collection is chosen rather than the
// first, and a page too short to measure skips LOUDLY rather than
// passing vacuously (the failure mode filed as #1272).
//
// ⚠️ THE MAXIMUM IS MEASURED WITH THE CHROME DOWN, and that is not
// incidental. <main> carries `margin-top: chromeBottom`, so hiding the
// auto-hiding navbar makes <main> TALLER and its scroll range SMALLER —
// by the chrome's height, ~73px at 1080p. A range measured with the
// chrome up is therefore an over-estimate that a downward scroll
// invalidates, and parking near it puts the browser's clamp BELOW the
// park point. That is the other half of why a fixed 400 failed.

import type { APIRequestContext, Locator, Page } from '@playwright/test';
import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

/** Below this much scroll range there is no room to park a reader and
 *  still prove the page moves afterwards. Not a corpus assumption — a
 *  statement about what the assertion needs. */
const MIN_RANGE = 250;

const scroller = (page: Page) => page.locator('main').first();
const mainScrollTop = (page: Page) => scroller(page).evaluate((el) => el.scrollTop);
const mainOverflow = (page: Page) => scroller(page).evaluate((el) => getComputedStyle(el).overflowY);

/**
 * The collection with the deepest wall.
 *
 * The first version took `items[0]`, which on a fresh database can
 * easily be the nine-post one that cannot scroll at all. Asking each
 * collection how many posts it holds costs seven requests once.
 */
async function deepestCollection(request: APIRequestContext): Promise<string> {
  const res = await request.get('/api/v1/collections?limit=20');
  expect(res.status(), 'the seeded catalogue must be readable').toBe(200);
  const items = ((await res.json()) as { items?: Array<{ id: string }> }).items ?? [];
  expect(items.length, 'no seeded collection to scroll behind a dialog').toBeGreaterThan(0);

  let best = items[0].id;
  let bestCount = -1;
  for (const { id } of items) {
    const posts = await request.get(`/api/v1/collections/${id}/posts?limit=200`);
    if (!posts.ok()) continue;
    const n = (((await posts.json()) as { items?: unknown[] }).items ?? []).length;
    if (n > bestCount) {
      bestCount = n;
      best = id;
    }
  }
  return best;
}

/**
 * The page's TRUE scroll maximum, measured with the chrome in the state
 * a downward scroll leaves it — which is the smallest the range ever
 * gets, and therefore the only safe number to calibrate against.
 */
async function measureMax(page: Page): Promise<number> {
  const el = scroller(page);
  await el.evaluate((n) => {
    n.scrollTop = n.scrollHeight;
  });
  // Let the chrome's scroll listener run and the layout settle; the
  // range shrinks as the navbar leaves.
  await page.waitForTimeout(400);
  const max = await el.evaluate((n) => {
    n.scrollTop = n.scrollHeight;
    return n.scrollTop;
  });
  await el.evaluate((n) => {
    n.scrollTop = 0;
  });
  await page.waitForTimeout(300);
  return max;
}

async function park(page: Page, y: number) {
  await scroller(page).evaluate((el, top) => {
    el.scrollTop = top;
  }, y);
  await page.waitForTimeout(250);
}

async function landOnCollection(page: Page, request: APIRequestContext) {
  await loginAsAdminViaUI(page);
  await page.goto(`/collections/${await deepestCollection(request)}`);
  await expect(scroller(page)).toBeVisible();
  // The wall arrives in one request; wait for it to have laid out before
  // measuring anything about height.
  await page.waitForTimeout(1500);
}

async function openEditModal(page: Page) {
  await page.getByTestId('collection-detail-more-button').first().click();
  await page.getByTestId('collection-detail-edit-menuitem').first().click();
  const dialog = page.locator('[role="dialog"]');
  await expect(dialog).toBeVisible();
  // The dialog's own seed has to have finished before any of this means
  // anything — a half-built panel is a different geometry.
  await expect(dialog.locator('input[name="vis_edit"]').first()).toBeVisible();
  return dialog;
}

async function wheelAt(page: Page, x: number, y: number, ticks = 15) {
  await page.mouse.move(x, y);
  for (let i = 0; i < ticks; i += 1) await page.mouse.wheel(0, 200);
  await page.waitForTimeout(400);
}

/**
 * Establish a park point for a scrolling assertion, or skip LOUDLY.
 *
 * A silently-skipped scroll test is the vacuous green filed as #1272, so
 * the reason carries the measured numbers and is printed as well as
 * annotated — `test.skip` alone only reaches the HTML report.
 */
async function parkPointOrSkip(page: Page, label: string): Promise<number> {
  const max = await measureMax(page);
  if (max < MIN_RANGE) {
    const reason =
      `#1223 ${label}: the deepest seeded collection offers only ${max}px of scroll range ` +
      `(needs ${MIN_RANGE}px) — this corpus cannot host a scrolling assertion. The LOCK itself ` +
      `is still asserted by the two tests that need no scroll range.`;
    // eslint-disable-next-line no-console
    console.warn(`\n⚠️  SKIPPED — ${reason}\n`);
    test.info().annotations.push({ type: 'skip-reason', description: reason });
    test.skip(true, reason);
  }
  // 40% of the true maximum: deep enough that the chrome has hidden and
  // <main> is the effective root scroller, with 60% still above the
  // reader so "it moves again" has somewhere to go.
  const point = Math.max(80, Math.floor(max * 0.4));
  await park(page, point);
  return point;
}

/** The dialog's own scrolling body.
 *
 *  ⛔ ADDRESSED DIRECTLY SINCE #1264, not by reaching for the parent of
 *  the details page. The edit dialog was two pages inside a wrapper and
 *  `…details-page` + `..` was the only handle on that wrapper; it is one
 *  surface now, and the scroller carries its own testid. This fails on
 *  the pre-#1264 build, where `collection-edit-body` does not exist. */
const dialogBody = (dialog: Locator) => dialog.getByTestId('collection-edit-body');

test.describe('#1223 an open dialog freezes the page behind it', () => {
  // ── The two that need no scroll range at all ──────────────────────
  //
  // These are the discriminating assertions: they fail on the pre-fix
  // build at every viewport and on every corpus, because they read the
  // lock rather than its consequences.

  test('1080p — the lock is taken on open and given back on close', async ({ page, request }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await landOnCollection(page, request);

    expect(await mainOverflow(page), 'the page was locked before any dialog opened').toBe('auto');
    const dialog = await openEditModal(page);
    expect(await mainOverflow(page), 'the page behind an open dialog is still scrollable').toBe(
      'hidden',
    );
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    expect(await mainOverflow(page), 'the lock outlived the dialog — the page is stuck').toBe(
      'auto',
    );
  });

  test('390px — the lock never reaches the dialog’s own scroller', async ({ page, request }) => {
    await page.setViewportSize({ width: 390, height: 780 });
    await landOnCollection(page, request);

    const dialog = await openEditModal(page);
    expect(await mainOverflow(page)).toBe('hidden');

    // Corpus-independent: at 390px the one surface overflows its capped
    // body whatever the collection holds, because the overflow comes
    // from the FORM and the cover block below it (#1264), not from the
    // wall behind it.
    const body = dialogBody(dialog);
    const range = await body.evaluate((el) => el.scrollHeight - el.clientHeight);
    expect(range, 'the dialog body does not overflow, so nothing here is measured').toBeGreaterThan(
      0,
    );
    const panel = (await dialog.locator('> div').first().boundingBox())!;
    await wheelAt(page, panel.x + panel.width / 2, panel.y + panel.height / 2, 25);
    expect(
      await body.evaluate((el) => el.scrollTop),
      'the lock reached inside the dialog and froze its own scroller',
    ).toBeGreaterThan(0);

    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    expect(await mainOverflow(page)).toBe('auto');
  });

  test('two open/close cycles leak no lock', async ({ page, request }) => {
    await page.setViewportSize({ width: 390, height: 780 });
    await landOnCollection(page, request);

    for (let i = 0; i < 2; i += 1) {
      const dialog = await openEditModal(page);
      expect(
        await mainOverflow(page),
        `cycle ${i + 1}: the dialog opened without locking the page`,
      ).toBe('hidden');
      await page.keyboard.press('Escape');
      await expect(dialog).toBeHidden();
      expect(await mainOverflow(page), `cycle ${i + 1}: the lock leaked past the close`).toBe(
        'auto',
      );
    }
  });

  // ── The three that need a page tall enough to scroll ──────────────

  test('1080p — the wheel does not reach the page, which moves again once the dialog closes', async ({
    page,
    request,
  }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await landOnCollection(page, request);

    const dialog = await openEditModal(page);
    // Parked with the dialog already open. The only door into this
    // dialog is the More menu at the TOP of the page, and opening a menu
    // moves focus into it — so on a scrolled page the browser reveals
    // the off-screen trigger and puts <main> back to 0 before the dialog
    // exists, with or without Playwright's own scroll-into-view. Setting
    // the offset once the dialog is up reaches the identical state with
    // the artefact out of the way.
    const parked = await parkPointOrSkip(page, '1080p');

    await wheelAt(page, 60, 540);
    expect(await mainScrollTop(page), 'the wheel reached the page behind the dialog').toBe(parked);

    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    expect(await mainScrollTop(page), 'closing the dialog moved the page').toBe(parked);

    // ⛔ THE RELEASE, and it must stay a MOVEMENT assertion. An element
    // frozen at `parked` forever satisfies every "unchanged" check above,
    // so this is the one that tells a released lock from a permanent one.
    await wheelAt(page, 960, 540, 5);
    expect(
      await mainScrollTop(page),
      'the page did not scroll after the dialog closed — the lock never came off',
    ).toBeGreaterThan(parked);
  });

  test('390px — the wheel over the backdrop does not move the page', async ({ page, request }) => {
    await page.setViewportSize({ width: 390, height: 780 });
    await landOnCollection(page, request);

    const dialog = await openEditModal(page);
    const parked = await parkPointOrSkip(page, '390px');

    // x=5 is the backdrop's own padding gutter, beside the panel. This is
    // the viewport and the gesture that moved <main> 400 -> 3400 before
    // the lock existed.
    await wheelAt(page, 5, 400);
    expect(
      await mainScrollTop(page),
      'the wheel scrolled the page behind the open dialog — the pre-#1223 build moved it by ' +
        'thousands of pixels at this viewport',
    ).toBe(parked);

    // Scrolling the dialog to its own end must not chain out either.
    const panel = (await dialog.locator('> div').first().boundingBox())!;
    await wheelAt(page, panel.x + panel.width / 2, panel.y + panel.height / 2, 25);
    expect(
      await mainScrollTop(page),
      'scrolling the dialog to its end chained out to the page behind it',
    ).toBe(parked);

    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    expect(await mainScrollTop(page), 'the scroll position did not survive the dialog').toBe(
      parked,
    );
    await wheelAt(page, 195, 400, 5);
    expect(
      await mainScrollTop(page),
      'the page did not scroll after the dialog closed — the lock never came off',
    ).toBeGreaterThan(parked);
  });

  test('touch scrolling inside the dialog still works', async ({ browser, request }) => {
    // A separate context: `hasTouch` is a context option, and a coarse
    // pointer is what the lock must not trap. The mechanism that would
    // break this is the lock reaching the dialog's own scroller — an
    // `overflow: hidden` box cannot be scrolled by a finger any more than
    // by a wheel — so the assertion is a REAL touch drag rather than a
    // reading of the styles that permit one.
    const ctx = await browser.newContext({
      viewport: { width: 390, height: 780 },
      hasTouch: true,
      storageState: '.pw-results/admin-state.json',
    });
    const page = await ctx.newPage();
    try {
      await page.goto(`/collections/${await deepestCollection(request)}`);
      await expect(scroller(page)).toBeVisible();
      const dialog = await openEditModal(page);
      const body = dialogBody(dialog);
      await expect
        .poll(async () => body.evaluate((el) => el.scrollHeight - el.clientHeight))
        .toBeGreaterThan(0);

      const box = (await body.boundingBox())!;
      const cx = Math.round(box.x + box.width / 2);
      const from = Math.round(box.y + box.height * 0.75);
      const to = Math.round(box.y + box.height * 0.25);
      const cdp = await page.context().newCDPSession(page);
      const touch = (type: string, y: number) =>
        cdp.send('Input.dispatchTouchEvent', {
          type,
          touchPoints: type === 'touchEnd' ? [] : [{ x: cx, y }],
        });
      await touch('touchStart', from);
      for (let y = from; y >= to; y -= 20) {
        await touch('touchMove', y);
      }
      await touch('touchEnd', to);
      await page.waitForTimeout(600);

      expect(
        await body.evaluate((el) => el.scrollTop),
        'a finger cannot scroll the dialog — the lock trapped the touch',
      ).toBeGreaterThan(0);
    } finally {
      await ctx.close();
    }
  });
});
