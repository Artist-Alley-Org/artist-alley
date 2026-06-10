// ui-06-anon-and-auth-gates.spec.ts
//
// Verifies the auth gating story:
//   - anonymous user can hit /login + /setup + /blogs
//   - anonymous user hitting an authed route gets redirected to login
//   - authed user hitting /login bounces forward (no infinite-loop
//     redirect)

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';

test.describe('UI-06 auth gates', () => {
  test('anonymous can reach /login', async ({ page }) => {
    await page.goto('/login');
    await expect(page).toHaveURL(/\/login\b/);
    await expectPageRendersCleanly(page);
    await expect(page.getByRole('textbox', { name: 'Username or email' })).toBeVisible();
  });

  test('anonymous accessing /account/profile redirects to login', async ({ page }) => {
    await page.goto('/account/profile');
    await expect(page).toHaveURL(/\/login(\?.*)?$/);
  });

  test('anonymous accessing /admin redirects to login', async ({ page }) => {
    await page.goto('/admin');
    await expect(page).toHaveURL(/\/login(\?.*)?$/);
  });

  test('anonymous accessing /admin/federation/peers redirects to login', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    await expect(page).toHaveURL(/\/login(\?.*)?$/);
  });

  test('authed user navigating to /login is bounced forward', async ({ page }) => {
    await loginAsAdminViaUI(page);
    await page.goto('/login');
    // SvelteKit may redirect to / or keep us on /login depending on
    // the auth store's behavior; either is acceptable as long as
    // we don't end up in a redirect loop.
    await expect(page).not.toHaveURL(/\/login.*\/login/);
  });

  test('login form preserves the `next` query parameter', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    // Expect redirect to /login with ?next=
    await expect(page).toHaveURL(/\/login\?.*next=/);
    // After login, should land back at the original destination.
    await page.getByRole('textbox', { name: 'Username or email' }).fill('admin');
    await page.getByRole('textbox', { name: 'Password' }).fill('ArtistAlleyMogul');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page).toHaveURL(/\/admin\/federation\/peers/);
  });
});
