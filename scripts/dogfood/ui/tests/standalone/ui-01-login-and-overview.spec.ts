// ui-01-login-and-overview.spec.ts
//
// Wire-health check at the UI layer. Mirrors scenario 01's Phase A
// but exercises the actual browser surface — login form, post-login
// redirect, navbar render, /admin gate.
//
// Catches: stale schema (frontend route depends on a field the
// backend dropped), bootstrap-admin login flow drift, navbar
// breakage that 401s every API call silently.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI, LOGGED_OUT } from '../../helpers/auth';

// Login/overview subject — starts LOGGED OUT (opts out of the shared
// admin session, #481) so its tests drive the real login form.
test.use({ storageState: LOGGED_OUT });

test.describe('UI-01 login + overview', () => {
  test('login form accepts admin / ArtistAlleyMogul and lands on Browse', async ({ page }) => {
    await loginAsAdminViaUI(page);
    await expect(page).toHaveTitle(/Browse — Artist Alley/);
    // Navbar branding is present (logo.svg or text fallback).
    await expect(page.getByRole('link', { name: 'Artist Alley' })).toBeVisible();
  });

  test('admin menu is reachable after login', async ({ page }) => {
    await loginAsAdminViaUI(page);
    await page.goto('/admin');
    await expect(page).toHaveTitle(/Administration|Admin — Artist Alley/);
  });

  test('federation section landing renders', async ({ page }) => {
    await loginAsAdminViaUI(page);
    await page.goto('/admin/federation');
    await expect(page).toHaveTitle(/Federation/);
    // The section header.
    await expect(page.getByRole('heading', { name: 'Federation', exact: true })).toBeVisible();
  });
});
