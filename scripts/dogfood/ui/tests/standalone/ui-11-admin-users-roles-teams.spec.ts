// ui-11-admin-users-roles-teams.spec.ts
//
// Identity-section management pages: users list, roles list,
// teams list. Verifies the list endpoint feeds the UI + filter/
// search controls render.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI, openAdminUsersFilteredToAdmin } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';

test.describe('UI-11 admin users / roles / teams', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('users list renders the admin user', async ({ page }) => {
    // By IDENTITY, not by shape (#1198). The old assertion was that
    // `main` matched /admin\b/i somewhere — which a fixture account
    // called "admin_probe" satisfies just as well, and which the real
    // bootstrap admin stops satisfying the moment it pages off the list.
    // The helper searches for it and matches the href carrying its ref.
    const { ref } = await openAdminUsersFilteredToAdmin(page);
    await expectPageRendersCleanly(page);
    expect(ref, 'the bootstrap admin is ref 1 on every install').toBeGreaterThan(0);
  });

  test('clicking the admin user opens their detail page', async ({ page }) => {
    const { ref, row } = await openAdminUsersFilteredToAdmin(page);
    await row.click();
    await expect(page).toHaveURL(new RegExp(`/admin/users/${ref}$`));
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
