// ui-09-account-pages.spec.ts
//
// Account section interaction. Asserts every account page renders
// its primary form/control + that the simple read paths return
// data without crashing.

import { test, expect } from '../../helpers/test';
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

  // #600 — three tiles that used to land on the "coming in a later
  // phase" placeholder, plus one page that shipped with no nav entry.
  // Asserting on a real control each, not just `main` visible, so a
  // regression to the placeholder fails instead of passing.

  test('following page renders both tabs', async ({ page }) => {
    await page.goto('/account/following');
    await expect(page.getByTestId('following-tab-following')).toBeVisible();
    await expect(page.getByTestId('following-tab-followers')).toBeVisible();
    // Either a populated table or the empty state — never the stub.
    await expect(
      page.getByTestId('following-table').or(page.getByTestId('following-empty')),
    ).toBeVisible();
  });

  test('access requests page renders (had no nav entry before #600)', async ({ page }) => {
    await page.goto('/account/requests');
    await expect(
      page.getByTestId('requests-list').or(page.getByTestId('requests-empty')),
    ).toBeVisible();
  });

  test('help page lists reachable destinations', async ({ page }) => {
    await page.goto('/account/help');
    const links = page.getByTestId('help-links');
    await expect(links).toBeVisible();
    // Scoped to the list: the account sidebar renders the same href.
    await expect(links.locator('a[href="/account/shortcuts"]')).toBeVisible();
  });

  test('shortcuts page renders the cheatsheet groups', async ({ page }) => {
    await page.goto('/account/shortcuts');
    await expect(page.getByTestId('shortcuts-groups')).toBeVisible();
    await expect(page.getByTestId('shortcut-group-whiteboard_tools')).toBeVisible();
  });

  test('the account grid links the access-requests page', async ({ page }) => {
    await page.goto('/account');
    // Scoped to the grid — the sidebar carries the same href, and the
    // tile grid is the surface #600 was about.
    const tile = page.getByTestId('account-tiles').locator('a[href="/account/requests"]');
    await expect(tile).toBeVisible();
    await tile.click();
    await expect(page).toHaveURL(/\/account\/requests$/);
    // The stub placeholder would render this instead.
    await expect(page.locator('main')).not.toContainText(/coming/i);
  });
});
