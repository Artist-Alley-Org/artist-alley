// menu-trigger-focus-1109.spec.ts
//
// #1109 — the shared `Menu` primitive wrapped its trigger snippet in a
// `button class="contents"`. An element with `display: contents`
// generates NO BOX: the button reported `getClientRects().length === 0`,
// refused `.focus()`, and was skipped by Tab. Every menu in the app was
// pointer-only, which meant a keyboard user could not open the user menu
// and therefore could not sign out.
//
// The repair is per-call-site (`triggerClass` carries a real box) plus a
// box-generating default on the component. Nothing about that repair is
// visible in a rendered screenshot when the menu is closed, and nothing
// in `npm run check` or vitest can see it either — a happy-dom unit test
// has no layout, so `getClientRects()` is empty for EVERY element there
// and an assertion written against it would pass on the bug. It takes a
// real engine, so it lives here.
//
// What this pins, in the order the defect actually bit:
//   1. every rendered menu trigger generates a box and accepts focus;
//   2. Tab reaches the nav triggers, and the focus ring renders;
//   3. a keyboard-only user can sign out end to end;
//   4. no trigger nests another focusable inside itself — three call
//      sites used to put a real `<button>` inside `Menu`'s button, which
//      is invalid markup and put two stops on one control. Passing that
//      test is what makes test 1's count meaningful: it proves the
//      focusable thing IS the trigger, not a control smuggled inside it.

import { test, expect } from '../../helpers/test';
import { tid } from '../../helpers/testids';
import { LOGGED_OUT } from '../../helpers/auth';
import type { Page } from '@playwright/test';

const ADMIN_USER = process.env.AA_DOGFOOD_ADMIN_USER ?? 'admin';
const ADMIN_PASS = process.env.AA_DOGFOOD_ADMIN_PASS ?? 'ArtistAlleyMogul';

const TRIGGER = 'button[aria-haspopup="menu"]';

/** Measure every menu trigger on the current page.
 *
 *  `rendered` filters out triggers behind a `display: none` ancestor —
 *  the mobile-only feed filter is one, and demanding a box from a
 *  control the viewport is not showing would be a false failure. The
 *  filter is an explicit ancestor walk and NOT `checkVisibility()`,
 *  because `checkVisibility()` reports a `display: contents` element as
 *  visible: it would have called the #1109 bug healthy.
 */
async function measureTriggers(page: Page) {
  return page.evaluate((sel) => {
    return Array.from(document.querySelectorAll<HTMLElement>(sel)).map((b, i) => {
      const hiddenAncestor = (() => {
        for (let a = b.parentElement; a; a = a.parentElement) {
          if (getComputedStyle(a).display === 'none') return true;
        }
        return false;
      })();
      const previous = document.activeElement;
      b.focus();
      const takesFocus = document.activeElement === b;
      if (!takesFocus && previous instanceof HTMLElement) previous.focus();
      return {
        index: i,
        name: (b.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 40),
        testId: b.getAttribute('data-testid'),
        display: getComputedStyle(b).display,
        rendered: !hiddenAncestor,
        rects: b.getClientRects().length,
        takesFocus,
        // Anything inside the trigger that is itself a tab stop.
        nestedFocusables: b.querySelectorAll('a[href], button, input, select, textarea, [tabindex]')
          .length,
      };
    });
  }, TRIGGER);
}

/** Tab forward from the top of the document until `predicate` matches
 *  the focused element, and report how many presses it took. Returns -1
 *  when the walk never lands on it — that is the #1109 symptom, and it
 *  is what a Tab-order regression looks like from the outside. */
async function tabUntil(page: Page, predicate: string, maxPresses = 200): Promise<number> {
  await page.evaluate(() => {
    document.body.setAttribute('tabindex', '-1');
    document.body.focus();
  });
  for (let i = 1; i <= maxPresses; i++) {
    await page.keyboard.press('Tab');
    const hit = await page.evaluate(
      (sel) => !!document.activeElement && document.activeElement.matches(sel),
      predicate,
    );
    if (hit) return i;
  }
  return -1;
}

test.describe('#1109 shared-Menu triggers are keyboard-reachable', () => {
  test('every rendered menu trigger generates a box and accepts focus', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator(tid('nav-user-menu-trigger'))).toBeVisible();

    const all = await measureTriggers(page);
    const rendered = all.filter((t) => t.rendered);

    // Guard the guard: an empty set would satisfy every assertion below.
    // The navbar alone carries Explore / notifications / messages / user
    // / admin, so anything under five means the page did not finish or
    // the selector stopped matching, and this spec is measuring nothing.
    expect(rendered.length).toBeGreaterThanOrEqual(5);

    const zeroBox = rendered.filter((t) => t.rects === 0);
    const unfocusable = rendered.filter((t) => !t.takesFocus);
    expect(zeroBox, `triggers with no client rects: ${JSON.stringify(zeroBox)}`).toEqual([]);
    expect(unfocusable, `triggers refusing focus: ${JSON.stringify(unfocusable)}`).toEqual([]);
    expect(rendered.filter((t) => t.display === 'contents')).toEqual([]);
  });

  test('no menu trigger nests another focusable inside it', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator(tid('nav-user-menu-trigger'))).toBeVisible();

    const nested = (await measureTriggers(page)).filter((t) => t.nestedFocusables > 0);
    expect(nested, `triggers wrapping a second tab stop: ${JSON.stringify(nested)}`).toEqual([]);
  });

  test('Tab reaches the user + admin menu triggers, and the ring renders', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator(tid('nav-user-menu-trigger'))).toBeVisible();

    const userStop = await tabUntil(page, '[data-testid="nav-user-menu-trigger"]');
    expect(userStop, 'Tab never landed on the user-menu trigger').toBeGreaterThan(0);

    // Keyboard focus must be SEEN, not just held: the ring is the only
    // thing telling the reader where they are.
    expect(
      await page.evaluate(() => document.activeElement?.matches(':focus-visible') ?? false),
    ).toBe(true);

    // …and the trigger opens from the keyboard, with focus moving into
    // the panel. Enter, not click — a control that only answers the
    // mouse is the bug this spec exists for.
    await page.keyboard.press('Enter');
    await expect(page.locator(tid('user-menu-panel'))).toBeVisible();
    expect(
      await page.evaluate(() => {
        const panel = document.querySelector('[data-testid="user-menu-panel"]');
        return !!panel && !!document.activeElement && panel.contains(document.activeElement);
      }),
    ).toBe(true);

    await page.keyboard.press('Escape');
    // Escape returns focus to the trigger it came from.
    await expect(page.locator(tid('nav-user-menu-trigger'))).toBeFocused();

    const adminStop = await tabUntil(page, '[data-testid="nav-admin-menu-trigger"]');
    expect(adminStop, 'Tab never landed on the admin-menu trigger').toBeGreaterThan(0);
  });
});

test.describe('#1109 keyboard-only sign-out', () => {
  // Signs the session out, so it must not share the suite's session
  // (#481) — sign-out revokes the cookie SERVER-side and every later
  // spec would 401. Same opt-out ui-16 uses for the same reason.
  test.use({ storageState: LOGGED_OUT });

  test('a keyboard user can reach the user menu and sign out', async ({ page }) => {
    await page.goto('/login');
    await page.locator(tid('login-username')).fill(ADMIN_USER);
    await page.locator(tid('login-password')).fill(ADMIN_PASS);
    await page.locator(tid('login-submit')).click();
    await expect(page).toHaveURL(/\/(?:\?|$)/);
    await expect(page.locator(tid('nav-user-menu-trigger'))).toBeVisible();

    // No .click() anywhere below this line — that is the whole point.
    const stop = await tabUntil(page, '[data-testid="nav-user-menu-trigger"]');
    expect(stop, 'Tab never landed on the user-menu trigger').toBeGreaterThan(0);

    await page.keyboard.press('Enter');
    await expect(page.locator(tid('user-menu-panel'))).toBeVisible();

    // Walk the panel with ArrowDown until Sign out has focus, then
    // activate it. Menu owns Arrow navigation between its items.
    const signOut = page.locator(tid('user-menu-sign-out'));
    for (let i = 0; i < 20; i++) {
      if (await signOut.evaluate((el) => el === document.activeElement)) break;
      await page.keyboard.press('ArrowDown');
    }
    await expect(signOut).toBeFocused();
    await page.keyboard.press('Enter');

    await expect(page).toHaveURL(/\/login\b/);
    // Revoked server-side, not just dropped from the browser.
    expect((await page.request.get('/api/v1/admin/users?limit=1')).status()).toBe(401);
  });
});
