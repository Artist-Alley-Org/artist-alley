// ui-05-smoke-authenticated.spec.ts
//
// Tier-1 smoke: every route in the manifest renders cleanly when
// logged in as admin. One Playwright test per route — adding a
// page to the manifest automatically gains coverage.
//
// What this catches:
//   - SvelteKit hydration failures (missing layout dependency,
//     bad +page.ts load result)
//   - API contract drift that crashes a page (a field the route
//     consumes was dropped from the backend response)
//   - i18n key drift that breaks render
//   - Capability gating regressions on the admin pages

import { test } from '@playwright/test';
import { loginAsAdminViaUI } from '../helpers/auth';
import { expectPageRendersCleanly } from '../helpers/assertions';
import {
  ANONYMOUS_ROUTES,
  AUTHENTICATED_USER_ROUTES,
  ADMIN_ROUTES,
  ADMIN_CATCHALL_SECTIONS,
} from '../helpers/routes';

test.describe('UI-05 smoke (authenticated as admin)', () => {
  // Login once per test. Tests run in parallel workers so the
  // amortised cost is small (~1s for login + ~1s for the route).
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  const allUserVisibleRoutes = [
    ...AUTHENTICATED_USER_ROUTES,
    ...ADMIN_ROUTES,
    ...ADMIN_CATCHALL_SECTIONS,
    // Anonymous routes also work when authed — they just redirect
    // or render their own anon-shape. Include them so we don't lose
    // smoke when a route's scope changes.
    ...ANONYMOUS_ROUTES.filter((r) => r.path !== '/setup' && r.path !== '/login'),
  ];

  for (const route of allUserVisibleRoutes) {
    if (route.scope === 'skip') continue;
    test(`${route.label} (${route.path})`, async ({ page }) => {
      const resp = await page.goto(route.path);
      // SvelteKit may return 200 + render a clean redirect, or it
      // may genuinely 4xx. We don't want a hard fail on 4xx since
      // some routes legitimately gate on capability; the renders-
      // cleanly check below catches actual breakage.
      if (resp && resp.status() >= 500) {
        throw new Error(`${route.path} returned HTTP ${resp.status()}`);
      }
      await expectPageRendersCleanly(page);
    });
  }
});
