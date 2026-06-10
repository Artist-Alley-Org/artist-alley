// ui-29-not-found-and-errors.spec.ts
//
// 404 + error boundary coverage.
//   - A clearly-bogus URL surfaces a friendly 404 page (not raw
//     SvelteKit error JSON, not a blank screen).
//   - The 404 has a "go home" / "back to browse" affordance so
//     the user isn't stranded.
//   - A bogus admin sub-route hits the same path (no privilege
//     leak in the 404 shape).
//   - An unauthenticated 404 redirects via the standard login
//     flow OR shows the public 404 — both are acceptable; what
//     we don't want is a raw 500.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

test.describe('UI-29 404 + error pages', () => {
  test('authed: bogus top-level route returns a friendly 404', async ({ page }) => {
    await loginAsAdminViaUI(page);
    const resp = await page.goto('/this-route-definitely-does-not-exist');
    // SvelteKit returns 404 from +error.svelte boundaries; the
    // page itself renders normally.
    expect(resp?.status()).toBe(404);
    // Scope to main (body matches the entire page incl. nav, which
    // sometimes contains "Account" → matches /account/ on /404
    // and triggers Playwright's strict-mode multi-match).
    await expect(page.locator('main').first()).toContainText(/(404|not found|page.*missing)/i);
  });

  test('authed: bogus admin sub-route returns 404 (not 500)', async ({ page }) => {
    await loginAsAdminViaUI(page);
    const resp = await page.goto('/admin/bogus-subsection');
    // /admin/[section] catch-all may render an empty section, or
    // 404 — either is fine; what we reject is 5xx.
    expect(resp?.status()).toBeLessThan(500);
    // App shell stays intact even on bogus admin paths.
    await expect(page.locator('main')).toBeVisible();
  });

  test('authed: bogus deep route does not crash the SvelteKit shell', async ({ page }) => {
    await loginAsAdminViaUI(page);
    await page.goto('/admin/federation/this-page-does-not-exist');
    // The app shell (navbar + main) must render even on a missing
    // leaf — otherwise the user can't recover via the navbar.
    await expect(page.getByRole('banner')).toBeVisible();
    await expect(page.locator('main')).toBeVisible();
  });

  test('anonymous: bogus public route does not 500', async ({ page }) => {
    const resp = await page.goto('/bogus-public-route-xyz');
    // Either 404 + render, or redirect to /login (both acceptable
    // for anonymous access). NOT 5xx.
    expect(resp?.status()).toBeLessThan(500);
  });

  test('404 page provides a way back home', async ({ page }) => {
    await loginAsAdminViaUI(page);
    await page.goto('/another-bogus-route');
    // The navbar's home link is the universal escape hatch — if
    // the page doesn't include its own "go home" affordance, the
    // navbar must still be visible.
    const banner = page.getByRole('banner');
    await expect(banner).toBeVisible();
    const home = banner.getByRole('link', { name: 'artist-alley' }).first();
    await home.click();
    await expect(page).toHaveURL(/\/$/);
  });
});
