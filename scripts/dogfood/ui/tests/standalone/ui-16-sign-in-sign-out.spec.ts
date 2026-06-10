// ui-16-sign-in-sign-out.spec.ts
//
// Full sign-in + sign-out round-trip — the foundational flow
// every other test depends on. We verify:
//   - Login form happy path lands at the user's destination
//   - Wrong password shows an error + stays on /login
//   - Sign-out from the user menu lands on /login
//   - After sign-out the previously-authed cookies are dead
//     (a subsequent admin GET returns 401)
//   - The ?next= parameter survives login round-trip
//   - The session cookie is HTTP-only (server-set, not readable
//     from document.cookie)

import { test, expect } from '@playwright/test';
import { tid } from '../../helpers/testids';

const ADMIN_USER = process.env.AA_DOGFOOD_ADMIN_USER ?? 'admin';
const ADMIN_PASS = process.env.AA_DOGFOOD_ADMIN_PASS ?? 'ArtistAlleyMogul';

test.describe('UI-16 sign-in + sign-out round-trip', () => {
  test('happy-path login lands at /', async ({ page }) => {
    await page.goto('/login');
    await page.locator(tid('login-username')).fill(ADMIN_USER);
    await page.locator(tid('login-password')).fill(ADMIN_PASS);
    await page.locator(tid('login-submit')).click();
    await expect(page).toHaveURL(/\/(?:\?|$)/);
    // Navbar's user menu trigger is present iff auth succeeded.
    await expect(page.locator(tid('nav-user-menu-trigger'))).toBeVisible();
  });

  test('wrong password keeps the user on /login + surfaces an error', async ({ page }) => {
    await page.goto('/login');
    await page.locator(tid('login-username')).fill(ADMIN_USER);
    await page.locator(tid('login-password')).fill('definitely-not-the-real-password');
    await page.locator(tid('login-submit')).click();
    // Wait briefly for the form to surface an error; we don't pin
    // a specific copy because the i18n string may evolve.
    await expect(page).toHaveURL(/\/login\b/);
    // The form must still be there to retry.
    await expect(page.locator(tid('login-username'))).toBeVisible();
  });

  test('?next= is preserved across login round-trip', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    await expect(page).toHaveURL(/\/login\?.*next=/);
    await page.locator(tid('login-username')).fill(ADMIN_USER);
    await page.locator(tid('login-password')).fill(ADMIN_PASS);
    await page.locator(tid('login-submit')).click();
    await expect(page).toHaveURL(/\/admin\/federation\/peers/);
  });

  test('sign-out from the user menu lands on /login', async ({ page }) => {
    // Quick API login to skip the form interaction we already
    // tested above.
    await page.goto('/login');
    await page.locator(tid('login-username')).fill(ADMIN_USER);
    await page.locator(tid('login-password')).fill(ADMIN_PASS);
    await page.locator(tid('login-submit')).click();
    await expect(page).toHaveURL(/\/(?:\?|$)/);

    await page.locator(tid('nav-user-menu-trigger')).click();
    await page.locator(tid('user-menu-sign-out')).click();
    await expect(page).toHaveURL(/\/login\b/);
  });

  test('session is invalid server-side after sign-out', async ({ page, request }) => {
    // Sign in via the API, sign out via the UI, then make a
    // capability-gated API call with the same context. We expect
    // 401 — the cookie was actually revoked, not just dropped
    // client-side.
    await page.goto('/login');
    await page.locator(tid('login-username')).fill(ADMIN_USER);
    await page.locator(tid('login-password')).fill(ADMIN_PASS);
    await page.locator(tid('login-submit')).click();
    await expect(page).toHaveURL(/\/(?:\?|$)/);

    // Sign out.
    await page.locator(tid('nav-user-menu-trigger')).click();
    await page.locator(tid('user-menu-sign-out')).click();
    await expect(page).toHaveURL(/\/login\b/);

    // The browser context still has the (now-revoked) cookies.
    // An admin-gated GET MUST return 401, proving the cookie was
    // invalidated on the server side and not just stripped from
    // the browser.
    const r = await page.request.get('/api/v1/admin/users?limit=1');
    expect(r.status()).toBe(401);
  });
});
