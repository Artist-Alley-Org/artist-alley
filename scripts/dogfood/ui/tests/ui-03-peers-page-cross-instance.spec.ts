// ui-03-peers-page.spec.ts
//
// Loads /admin/federation/peers and verifies studio-b appears in
// the peers list. Catches:
//   - frontend regressions where the peers list silently doesn't
//     render despite the API returning rows;
//   - admin gating that suddenly hides the page from real admins;
//   - i18n key drift that crashes the page render.
//
// Requires that ./scripts/dogfood/pair.sh has run; otherwise the
// peer row isn't there and the test legitimately fails.
//
// Studio-b's view of studio-a is verified at the API/SQL layer in
// scripts/dogfood/scenarios/01-like-cross-instance.sh; studio-b
// has no UI of its own.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../helpers/auth';

test.describe('UI-03 peers page', () => {
  test('studio-a sees studio-b in its peers list', async ({ page }) => {
    await loginAsAdminViaUI(page);
    await page.goto('/admin/federation/peers');
    await expect(page.locator('main')).toContainText('studio-b.local');
  });
});
