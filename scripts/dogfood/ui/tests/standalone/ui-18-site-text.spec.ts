// ui-18-site-text.spec.ts
//
// Admin → Content → Site text (#794, ADR 0081 §1).
//
// Drives the real surface end to end: override a string the navbar
// renders, see it change on the public app INCLUDING logged out, revert
// it, and confirm the fail-loud 422 reaches the operator's screen.
//
// The specs clean up after themselves via the API so a re-run starts
// from the shipped strings — an override left behind would make the
// next run's "shipped value" assertions lie.

import { test, expect, Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { LOGGED_OUT } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

/** A string the navbar renders on every authenticated page. */
const NAV_KEY = 'nav.collections';
const NAV_SHIPPED = 'Collections';
const NAV_OVERRIDE = 'Libraries';

/** A string the LOGIN page renders, so the logged-out check has a
 *  target on an install that has not opened public mode. */
const LOGIN_KEY = 'login.title';
const LOGIN_OVERRIDE = 'Come on in';

async function clearOverride(page: Page, key: string, language = 'en') {
  await page.request.delete(`/api/v1/site-text/${key}?language=${language}`);
}

async function openSiteText(page: Page) {
  await page.goto('/admin/site-text');
  await expect(page.locator(tid('site-text-page'))).toBeVisible();
}

/** Search for one key and return its row's controls. */
async function rowFor(page: Page, key: string) {
  await page.locator(tid('site-text-search')).fill(key);
  const input = page.locator(tid(`site-text-input-${key}`));
  await expect(input).toBeVisible();
  return {
    input,
    save: page.locator(tid(`site-text-save-${key}`)),
    revert: page.locator(tid(`site-text-revert-${key}`)),
  };
}

test.describe('UI-18 site text overrides', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('lists the shipped catalogue and filters it', async ({ page }) => {
    await openSiteText(page);

    // The unfiltered list must be a page of a much larger set — if the
    // count ever equals the rows on screen, the catalogue did not load.
    const count = page.locator(tid('site-text-count'));
    await expect(count).toContainText(/of \d{4,} strings/);

    await page.locator(tid('site-text-search')).fill(NAV_KEY);
    await expect(page.locator(tid('site-text-row'))).toHaveCount(1);
    await expect(page.locator(tid('site-text-page'))).toContainText(NAV_SHIPPED);
  });

  test('override renders on the public surface, revert brings it back', async ({ page }) => {
    await clearOverride(page, NAV_KEY);
    await openSiteText(page);

    const row = await rowFor(page, NAV_KEY);
    await row.input.fill(NAV_OVERRIDE);
    await row.save.click();
    await expect(page.locator(tid('site-text-toast'))).toBeVisible();

    // Full reload: the override has to survive the boot fetch, not just
    // the in-memory store the admin page just wrote to.
    await page.goto('/');
    const nav = page.getByRole('banner');
    await expect(nav.getByRole('link', { name: NAV_OVERRIDE, exact: true })).toBeVisible();
    await expect(nav.getByRole('link', { name: NAV_SHIPPED, exact: true })).toHaveCount(0);

    // Revert.
    await openSiteText(page);
    const again = await rowFor(page, NAV_KEY);
    await again.revert.click();
    await expect(page.locator(tid('site-text-toast'))).toBeVisible();

    await page.goto('/');
    await expect(page.getByRole('banner').getByRole('link', { name: NAV_SHIPPED, exact: true })).toBeVisible();
  });

  test('an unknown key is refused visibly, naming the key', async ({ page }) => {
    await openSiteText(page);
    const row = await rowFor(page, NAV_KEY);

    // The list only offers real keys, so the only way to reach the
    // server's fail-loud path from a browser is to send a bad one.
    // Rewriting the request in flight exercises the REAL 422 and the
    // real error rendering — nothing is stubbed but the URL.
    await page.route('**/api/v1/site-text/nav.collections', async (route) => {
      const req = route.request();
      if (req.method() !== 'PUT') return route.fallback();
      await route.continue({ url: req.url().replace('nav.collections', 'nav.collectionz') });
    });

    await row.input.fill('anything');
    await row.save.click();

    const toast = page.locator(tid('site-text-toast'));
    await expect(toast).toBeVisible();
    // The operator must be able to see WHICH key was refused.
    await expect(toast).toContainText('nav.collectionz');
    await page.unroute('**/api/v1/site-text/nav.collections');
  });

  test('works at 390px with a touch pointer', async ({ browser }) => {
    const ctx = await browser.newContext({
      viewport: { width: 390, height: 844 },
      hasTouch: true,
      isMobile: true,
      locale: 'en-US',
      storageState: '.pw-results/admin-state.json',
      ignoreHTTPSErrors: true,
    });
    const page = await ctx.newPage();
    try {
      await clearOverride(page, NAV_KEY);
      await openSiteText(page);

      // Search by tapping the field, not by focusing it programmatically.
      await page.locator(tid('site-text-search')).tap();
      await page.locator(tid('site-text-search')).fill(NAV_KEY);

      const input = page.locator(tid(`site-text-input-${NAV_KEY}`));
      await expect(input).toBeVisible();
      await input.tap();
      await input.fill(NAV_OVERRIDE);
      await page.locator(tid(`site-text-save-${NAV_KEY}`)).tap();
      await expect(page.locator(tid('site-text-toast'))).toBeVisible();

      // The page must not scroll sideways at this width.
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      );
      expect(overflow).toBeLessThanOrEqual(1);

      // Revert by tap.
      await page.locator(tid(`site-text-revert-${NAV_KEY}`)).tap();
      await expect(page.locator(tid('site-text-toast'))).toBeVisible();
    } finally {
      await clearOverride(page, NAV_KEY);
      await ctx.close();
    }
  });
});

// A logged-out visitor must see the operator's wording. The read is
// anonymous precisely so this works; if it were gated, the copy would
// only appear after sign-in.
test.describe('UI-18 site text is visible logged out', () => {
  test('an overridden login string renders with no session', async ({ browser }) => {
    const admin = await browser.newContext({
      storageState: '.pw-results/admin-state.json',
      ignoreHTTPSErrors: true,
      locale: 'en-US',
    });
    const adminPage = await admin.newPage();
    await adminPage.goto('/admin/site-text');
    await adminPage.request.put(`/api/v1/site-text/${LOGIN_KEY}`, {
      data: { language: 'en', value: LOGIN_OVERRIDE },
    });

    // storageState: LOGGED_OUT, not a spread of it. A context created
    // in a test inherits the project's `use` — including the shared
    // admin session — so spreading `{cookies:[],origins:[]}` as
    // top-level options changes nothing and the "guest" arrives signed
    // in, gets redirected off /login, and the assertion fails for a
    // reason that has nothing to do with the feature.
    const guest = await browser.newContext({
      storageState: LOGGED_OUT,
      ignoreHTTPSErrors: true,
      locale: 'en-US',
    });
    const guestPage = await guest.newPage();
    try {
      await guestPage.goto('/login');
      await expect(guestPage.locator(tid('login-username'))).toBeVisible();
      await expect(guestPage.getByRole('heading', { name: LOGIN_OVERRIDE })).toBeVisible();
    } finally {
      await adminPage.request.delete(`/api/v1/site-text/${LOGIN_KEY}?language=en`);
      await guest.close();
      await admin.close();
    }
  });
});
