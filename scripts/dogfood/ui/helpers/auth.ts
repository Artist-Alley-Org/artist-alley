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

/**
 * The bootstrap admin's own user ref, read from the live session (#1198).
 *
 * The three specs that open the admin's /admin/users row used to find it
 * with `getByRole('link', {name: /admin/i}).first()` against an unfiltered
 * list. Both halves of that are assumptions rather than facts: `/admin/i`
 * matches any fixture account whose name happens to contain "admin", and
 * `.first()` is whatever the sort put on top — the list is newest-first,
 * so the OLDEST account on the instance is the one guaranteed NOT to be
 * there. Ask the session who it is instead.
 */
export async function bootstrapAdminRef(page: Page): Promise<number> {
  const me = await page.request.get('/api/v1/auth/me');
  expect(me.status(), 'the suite must be signed in to resolve the admin ref').toBe(200);
  const ref = Number(((await me.json()) as { ref: number }).ref);
  expect(Number.isFinite(ref) && ref > 0, 'the session reported no usable user ref').toBe(true);
  return ref;
}

/**
 * Open /admin/users, narrow it to the bootstrap admin with the list's own
 * search box, and hand back the row's link — located by the href that
 * carries the ref, so neither the page it lands on nor its position in
 * the sort can make this pass or fail (#1198).
 *
 * Returns the ref for callers that want to assert on the URL.
 */
export async function openAdminUsersFilteredToAdmin(
  page: Page,
): Promise<{ ref: number; row: ReturnType<Page['locator']> }> {
  const ref = await bootstrapAdminRef(page);
  await page.goto('/admin/users');
  // The list pages at 50, newest first. On any instance that has run the
  // suite more than a couple of dozen times the bootstrap admin — the
  // oldest row there is — is several pages down, so it has to be
  // searched for rather than looked for.
  await page.locator(tid('admin-users-search')).fill(ADMIN_USER);
  const row = page.locator(`a[href="/admin/users/${ref}"]`);
  await expect(
    row,
    `the users list has no row for the bootstrap admin (ref ${ref}) even when searched for`,
  ).toBeVisible({ timeout: 15_000 });
  return { ref, row };
}
