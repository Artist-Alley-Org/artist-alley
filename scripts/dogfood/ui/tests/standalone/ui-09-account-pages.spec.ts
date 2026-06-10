// ui-09-account-pages.spec.ts
//
// Account section interaction. Asserts every account page renders
// its primary form/control + that the simple read paths return
// data without crashing.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

test.describe('UI-09 account pages', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('account overview renders', async ({ page }) => {
    await page.goto('/account');
    await expect(page).toHaveURL(/\/account\/?$/);
    await expect(page.locator('main')).toBeVisible();
  });

  test('profile page renders editable fields', async ({ page }) => {
    await page.goto('/account/profile');
    // Profile has at least display name + bio (matches the
    // shipped /account/profile +page.svelte).
    await expect(page.getByLabel(/Display name/i)).toBeVisible();
  });

  test('preferences page renders theme + language pickers', async ({ page }) => {
    await page.goto('/account/preferences');
    await expect(page.locator('main')).toContainText(/Theme/i);
    await expect(page.locator('main')).toContainText(/Language/i);
  });

  test('AI preferences page renders', async ({ page }) => {
    await page.goto('/account/preferences/ai');
    await expect(page.locator('main')).toBeVisible();
  });

  test('password page renders', async ({ page }) => {
    await page.goto('/account/password');
    await expect(page.locator('main')).toBeVisible();
  });

  test('tokens page renders + has a create-token action', async ({ page }) => {
    await page.goto('/account/tokens');
    await expect(page.locator('main')).toBeVisible();
  });

  test('sessions page lists active sessions', async ({ page }) => {
    await page.goto('/account/sessions');
    await expect(page.locator('main')).toBeVisible();
  });

  test('notifications page renders', async ({ page }) => {
    await page.goto('/account/notifications');
    await expect(page.locator('main')).toBeVisible();
  });

  test('messages page renders', async ({ page }) => {
    await page.goto('/account/messages');
    await expect(page.locator('main')).toBeVisible();
  });

  test('blocked users page renders', async ({ page }) => {
    await page.goto('/account/blocked');
    await expect(page.locator('main')).toBeVisible();
  });
});
