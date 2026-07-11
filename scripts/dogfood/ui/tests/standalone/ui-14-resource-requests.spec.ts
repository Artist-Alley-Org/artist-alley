// ui-14-resource-requests.spec.ts
//
// Phase 1.17.E — resource request workflow.
//
//   /account/requests — requester's own list (empty-state happy path)
//   /admin/requests   — approver's pending list with decision dialog
//                       (empty-state happy path; the full grant flow
//                       requires a seeded request which the backend
//                       integration tests already cover)

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';

test.describe('UI-14 resource requests (Phase 1.17.E)', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('account requests page renders + empty state visible', async ({ page }) => {
    await page.goto('/account/requests');
    await expectPageRendersCleanly(page);
    // Heading from the new page (i18n: account.requests.title).
    await expect(page.getByRole('heading', { name: /access requests/i })).toBeVisible();
    // Bootstrap admin has no submitted requests on a fresh stack —
    // empty-state copy must show.
    await expect(page.locator('[data-testid="requests-empty"]')).toBeVisible();
  });

  test('admin requests page renders + empty state visible (admin gate passes)', async ({ page }) => {
    await page.goto('/admin/requests');
    await expectPageRendersCleanly(page);
    // Heading from the new page (i18n: admin.requests.title).
    await expect(page.getByRole('heading', { name: /resource requests/i })).toBeVisible();
    // Bootstrap admin has system.admin → coarse approver gate
    // passes → the page rendered (no 403). With no pending rows,
    // the empty-state copy shows.
    await expect(page.locator('[data-testid="requests-empty"]')).toBeVisible();
  });
});
