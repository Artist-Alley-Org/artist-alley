import { Page, expect, APIRequestContext } from '@playwright/test';
import { tid } from './testids';

const ADMIN_USER = process.env.AA_DOGFOOD_ADMIN_USER ?? 'admin';
const ADMIN_PASS = process.env.AA_DOGFOOD_ADMIN_PASS ?? 'ArtistAlleyMogul';

// Where the shared admin session is written by the `setup` project and
// read back via the standalone project's `use.storageState` (#481). One
// login for the whole suite instead of ~130 — the per-test login churn
// was the auth-race + mid-suite-ECONNREFUSED source.
export const ADMIN_STATE_PATH = '.pw-results/admin-state.json';

// Opt-out state for specs whose SUBJECT is the logged-out experience
// (login form, sign-out, anonymous auth-gates). An EXPLICITLY EMPTY jar,
// not `storageState: undefined` — Playwright treats undefined as
// "inherit the project value", which would keep the shared admin session
// and defeat the point. Use via `test.use({ storageState: LOGGED_OUT })`.
export const LOGGED_OUT = { cookies: [], origins: [] };

/**
 * Log in as the bootstrap admin via the UI form.
 *
 * Tests the actual login flow — username field, password field,
 * sign-in button, post-login redirect. Uses `data-testid` selectors
 * so the helper survives i18n / copy / role-name drift.
 */
export async function loginAsAdminViaUI(page: Page) {
  await page.goto('/login');
  // Idempotent under the shared session (#481): when the context is
  // already authenticated (storageState from the `setup` project), the
  // layout's load runs BEFORE the /login page renders and redirects to
  // /, so the form never appears — there is nothing to drive. Decide by
  // RACING the two settled outcomes rather than reading page.url()
  // eagerly (which resolves before the client-side redirect and was the
  // #481 flake: it saw /login, drove a form that never rendered, and
  // timed out). The login FORM is still exercised by ui-16, which opts
  // out of the shared state (storageState: undefined).
  const userField = page.locator(tid('login-username'));
  const outcome = await Promise.race([
    userField
      .waitFor({ state: 'visible', timeout: 10_000 })
      .then(() => 'form' as const)
      .catch(() => 'none' as const),
    page
      .waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 10_000 })
      .then(() => 'authed' as const)
      .catch(() => 'none' as const),
  ]);
  if (outcome !== 'form') {
    // Already authenticated (redirected off /login) — live session, no
    // form to drive.
    return;
  }
  await userField.fill(ADMIN_USER);
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
  // Idempotent (#481): a context carrying the shared session is already
  // authenticated — skip the extra login round-trip. The `setup`
  // project's own request context is fresh, so /auth/me 401s there and
  // the real login below runs.
  const me = await request.get('/api/v1/auth/me');
  if (me.ok()) {
    return;
  }
  const r = await request.post('/api/v1/auth/login', {
    data: { username: ADMIN_USER, password: ADMIN_PASS },
    headers: { 'Content-Type': 'application/json' },
  });
  if (r.status() !== 200) {
    throw new Error(`admin login failed: HTTP ${r.status()} — ${await r.text()}`);
  }
}
