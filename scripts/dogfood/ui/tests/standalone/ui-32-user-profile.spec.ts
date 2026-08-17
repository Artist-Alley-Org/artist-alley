// ui-32-user-profile.spec.ts
//
// #478 slice-1 — public user-profile pages (ADR 0070). Verifies the two
// permalinks resolve to the same profile and render the display header +
// owner-scoped content, and that the previously-dead author-name link
// (parked in KNOWN_GAPS until #478) now opens a real page.
//
// Anonymous-visibility + opt-out + real-name stripping are covered by the
// Go integration test (app/internal/users/profile_public_test.go); this
// spec exercises the rendered surface with the shared admin session.

import { test, expect } from '../../helpers/test';

test.describe('UI-32 public user profile', () => {
  test('profile resolves by username and renders a header', async ({ page }) => {
    await page.goto('/users/by-username/seed.bot');
    // The display-name header (h1) is the anchor of the profile.
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    await expect(page).toHaveTitle(/Artist Alley/);
    // The @handle is shown for a local user.
    await expect(page.getByText('@seed.bot')).toBeVisible();
  });

  test('profile resolves by ref to the same person', async ({ page }) => {
    // Resolve the ref from the by-username API so the test is not pinned
    // to a hard-coded seed id.
    const res = await page.request.get('/api/v1/users/by-username/seed.bot');
    expect(res.ok()).toBeTruthy();
    const { ref, display_name } = await res.json();
    expect(typeof ref).toBe('number');

    await page.goto(`/users/by-ref/${ref}`);
    await expect(page.getByRole('heading', { level: 1, name: display_name })).toBeVisible();
    await expect(page.getByText('@seed.bot')).toBeVisible();
  });

  test('mounts the shared view controls and honors the mode switch (#511)', async ({ page }) => {
    await page.goto('/users/by-username/seed.bot');
    // The same floating control bar as browse (mode switcher + sort).
    await expect(page.getByTestId('view-controls')).toBeVisible();

    // ...but NOT browse's asset-type filter (#1166). It is injected
    // through ViewControls' `trailing` seam by BrowseFooter, the same
    // way the latest/following pills arrive through `middle`, because
    // it filters a FEED and this page does not have one. Pinned as an
    // absence so the seam cannot quietly become part of the shared bar
    // and start offering a control that would do nothing here.
    await expect(page.getByTestId('kind-filter-toggle')).toHaveCount(0);

    // Switching to list mode re-renders the posts section as a table —
    // the browseView mode is honored here exactly as on browse. The mode
    // is the global browseView preference (localStorage); set it + reload.
    await page.evaluate(() => localStorage.setItem('aa_browse_mode', 'list'));
    await page.reload();
    await expect(page.getByRole('columnheader', { name: 'Title' })).toBeVisible();

    // Reset so the shared preference doesn't leak into other specs.
    await page.evaluate(() => localStorage.setItem('aa_browse_mode', 'grid'));
  });

  test('a previously-dead profile link now opens a real page', async ({ page }) => {
    // The user menu (top-right) links to the signed-in user's own profile
    // via /users/by-username/{username} — one of the links parked in
    // KNOWN_GAPS until this slice. Open the menu, follow it, land on a
    // rendered profile.
    await page.goto('/');
    await page.getByRole('button', { name: /Bootstrap Admin/i }).first().click();
    const profileLink = page.locator('a[href^="/users/by-username/"]').first();
    await expect(profileLink).toBeVisible();
    const href = await profileLink.getAttribute('href');
    expect(href).toMatch(/^\/users\/by-username\//);

    await profileLink.click();
    await expect(page).toHaveURL(/\/users\/by-username\//);
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    // Not the SPA 404 fallback.
    await expect(page).not.toHaveTitle(/Not Found|404/i);
  });
});
