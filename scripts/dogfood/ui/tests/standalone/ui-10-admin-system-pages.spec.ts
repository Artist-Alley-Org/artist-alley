// ui-10-admin-system-pages.spec.ts
//
// Walks every page under /admin/system/* and verifies it renders
// its primary form/list. Catches drift in the sysconfig endpoints
// (a missing field, a renamed key, a 500 on GET).

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';

test.describe('UI-10 admin system pages', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  const systemPages = [
    { path: '/admin/system/site',       expectText: /Site/ },
    { path: '/admin/system/smtp',       expectText: /SMTP/ },
    { path: '/admin/system/auth',       expectText: /Authentication|Password/ },
    { path: '/admin/system/ai',         expectText: /AI/ },
    { path: '/admin/system/activities', expectText: /activit/i },
    { path: '/admin/system/themes',     expectText: /Theme/i },
    { path: '/admin/system/log',        expectText: /log|audit/i },
    { path: '/admin/system/license',    expectText: /Licens/i },
  ];

  for (const p of systemPages) {
    test(`${p.path} renders`, async ({ page }) => {
      await page.goto(p.path);
      await expectPageRendersCleanly(page);
      await expect(page.locator('main')).toContainText(p.expectText);
    });
  }

  test('site form has Name + Base URL fields', async ({ page }) => {
    await page.goto('/admin/system/site');
    // These come from sysconfig.Site (name, base_url). Either an
    // input or a labeled control matches.
    const main = page.locator('main');
    await expect(main).toContainText(/Name/i);
    await expect(main).toContainText(/Base URL|Site URL/i);
  });
});
