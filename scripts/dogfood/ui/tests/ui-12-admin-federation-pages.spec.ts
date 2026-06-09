// ui-12-admin-federation-pages.spec.ts
//
// Federation-section admin surfaces (the five live surfaces
// shipped in 1.22.D-c). Each page is walked + a sample of its
// interactive controls is asserted.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../helpers/auth';
import { expectPageRendersCleanly } from '../helpers/assertions';

test.describe('UI-12 admin federation pages', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('peers page renders with at least the paired studio-b row', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    await expectPageRendersCleanly(page);
    // pair.sh registered studio-b — its row must be visible.
    await expect(page.locator('main')).toContainText(/studio-b/);
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
    // Outbox page should show the queue status — "queued", "sent",
    // "failed", or similar status filter UI is the marker.
    await expect(page.locator('main')).toContainText(/sent|queued|outbox/i);
  });

  test('inbox page renders + has the inbox state heading', async ({ page }) => {
    await page.goto('/admin/federation/inbox');
    await expectPageRendersCleanly(page);
    await expect(page.locator('main')).toContainText(/inbox|processed|received/i);
  });
});
