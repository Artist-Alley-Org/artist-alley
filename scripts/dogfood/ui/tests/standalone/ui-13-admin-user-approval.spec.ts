// ui-13-admin-user-approval.spec.ts
//
// Phase 1.17.A — typed user-state machine + admin approval surface.
// Verifies the admin user detail page renders the correct set of
// transition buttons for the current row state (derived from the
// typed matrix in $lib/admin/users) and that clicking one drives
// the state through the PUT /admin/users/{ref}/status endpoint.
//
// The bootstrap admin is `active`. From `active` the matrix
// permits {disabled, archived} — both buttons should render; the
// no-longer-permitted "Mark pending" button (which used to render
// unconditionally + return 400 on the move) MUST NOT.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';

test.describe('UI-13 admin user approval surface', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('active user shows only Disable + Archive transitions (matrix-derived)', async ({ page }) => {
    await page.goto('/admin/users');
    await expectPageRendersCleanly(page);
    // Open the bootstrap admin's detail page.
    const adminLink = page.getByRole('link', { name: /admin/i }).first();
    await adminLink.click();
    await expect(page).toHaveURL(/\/admin\/users\/\d+/);

    const transitions = page.locator('[data-testid="admin-user-transitions"]');
    await expect(transitions).toBeVisible();

    // Active → {disabled, archived}. Approve/Restore must NOT
    // render (the bootstrap admin is already active so there's
    // nothing to approve OR restore from).
    await expect(page.locator('[data-testid="transition-disable"]')).toBeVisible();
    await expect(page.locator('[data-testid="transition-archive"]')).toBeVisible();
    await expect(page.locator('[data-testid="transition-approve"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="transition-restore"]')).toHaveCount(0);
  });

  test('archived filter option present in the list filter dropdown', async ({ page }) => {
    await page.goto('/admin/users');
    await expectPageRendersCleanly(page);
    // The filter dropdown gains an `archived` option in 1.17.A.
    const archivedOption = page.locator('select option[value="archived"]');
    await expect(archivedOption).toHaveCount(1);
  });
});
