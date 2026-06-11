import { Page, expect, APIRequestContext } from '@playwright/test';
import { tid } from './testids';

const ADMIN_USER = process.env.AA_DOGFOOD_ADMIN_USER ?? 'admin';
const ADMIN_PASS = process.env.AA_DOGFOOD_ADMIN_PASS ?? 'ArtistAlleyMogul';

/**
 * Log in as the bootstrap admin via the UI form.
 *
 * Tests the actual login flow — username field, password field,
 * sign-in button, post-login redirect. Uses `data-testid` selectors
 * so the helper survives i18n / copy / role-name drift.
 */
export async function loginAsAdminViaUI(page: Page) {
  await page.goto('/login');
  await page.locator(tid('login-username')).fill(ADMIN_USER);
  await page.locator(tid('login-password')).fill(ADMIN_PASS);
  await page.locator(tid('login-submit')).click();
  // toHaveURL matches against the FULL url; we just want to land
  // on the post-login root (which may carry query params from a
  // ?next= redirect).
  await expect(page).toHaveURL(/\/(?:\?|$)/);
}

/**
 * Log in via the JSON API and reuse the session cookie. Faster
 * setup than the UI form for tests where login itself isn't the
 * subject — e.g. "open /admin/federation and verify the tile grid".
 */
export async function loginAsAdminViaAPI(request: APIRequestContext) {
  const r = await request.post('/api/v1/auth/login', {
    data: { username: ADMIN_USER, password: ADMIN_PASS },
    headers: { 'Content-Type': 'application/json' },
  });
  if (r.status() !== 200) {
    throw new Error(`admin login failed: HTTP ${r.status()} — ${await r.text()}`);
  }
}
