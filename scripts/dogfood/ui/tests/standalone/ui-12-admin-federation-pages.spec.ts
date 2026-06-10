// ui-12-admin-federation-pages.spec.ts (standalone)
//
// Federation-section admin surfaces (the five live surfaces
// shipped in 1.22.D-c). Asserts each page renders cleanly with
// EMPTY tables — no peer / share rows required. Cross-instance
// assertions (peer row visible, outbox sent, inbox processed)
// live in tests/federation/ui-21-federation-peer-flow.spec.ts.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';

test.describe('UI-12 admin federation pages (standalone)', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('peers page renders', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    await expectPageRendersCleanly(page);
    // No row assertion — federation/ui-21 handles cross-instance
    // visibility. What this catches: a render crash from a missing
    // i18n key, a schema field the page consumes that the API
    // dropped, etc.
  });

  test('directories page renders', async ({ page }) => {
    await page.goto('/admin/federation/directories');
    await expectPageRendersCleanly(page);
  });

  test('shares page renders', async ({ page }) => {
    await page.goto('/admin/federation/shares');
    await expectPageRendersCleanly(page);
  });

  test('outbox page renders + has the queue table heading', async ({ page }) => {
    await page.goto('/admin/federation/outbox');
    await expectPageRendersCleanly(page);
    // Status filter UI (queued/sent/failed) is the structural marker.
    await expect(page.locator('main')).toContainText(/sent|queued|outbox/i);
  });

  test('inbox page renders + has the inbox state heading', async ({ page }) => {
    await page.goto('/admin/federation/inbox');
    await expectPageRendersCleanly(page);
    await expect(page.locator('main')).toContainText(/inbox|processed|received/i);
  });
});
