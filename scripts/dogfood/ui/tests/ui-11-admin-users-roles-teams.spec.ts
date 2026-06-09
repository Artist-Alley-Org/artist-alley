// ui-11-admin-users-roles-teams.spec.ts
//
// Identity-section management pages: users list, roles list,
// teams list. Verifies the list endpoint feeds the UI + filter/
// search controls render.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../helpers/auth';
import { expectPageRendersCleanly } from '../helpers/assertions';

test.describe('UI-11 admin users / roles / teams', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('users list renders the admin user', async ({ page }) => {
    await page.goto('/admin/users');
    await expectPageRendersCleanly(page);
    // Bootstrap admin row must be present — it always exists.
    await expect(page.locator('main')).toContainText(/admin\b/i);
  });

  test('clicking the admin user opens their detail page', async ({ page }) => {
    await page.goto('/admin/users');
    // First link in the table that contains "admin"
    const adminLink = page
      .getByRole('link', { name: /admin/i })
      .first();
    await expect(adminLink).toBeVisible();
    await adminLink.click();
    await expect(page).toHaveURL(/\/admin\/users\/\d+/);
    await expectPageRendersCleanly(page);
  });

  test('roles list renders', async ({ page }) => {
    await page.goto('/admin/roles');
    await expectPageRendersCleanly(page);
    // Seeded roles include "Admin" + others.
    await expect(page.locator('main')).toContainText(/Admin/i);
  });

  test('teams list renders', async ({ page }) => {
    await page.goto('/admin/teams');
    await expectPageRendersCleanly(page);
  });
});
