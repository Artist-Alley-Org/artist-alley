// ui-07-top-nav.spec.ts
//
// Top navbar: logo, brand text, primary nav links, search box,
// upload button, notifications, messages, user menu, admin menu.
//
// Every button is asserted clickable. Every link's destination is
// asserted. The user + admin menus open and contain the documented
// menu items.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

test.describe('UI-07 top navbar', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('logo + brand link to home', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    const brand = page.getByRole('link', { name: 'Artist Alley' }).first();
    await expect(brand).toBeVisible();
    await brand.click();
    await expect(page).toHaveURL(/\/$/);
  });

  test('primary nav: Explore, Collections, Review', async ({ page }) => {
    // Scope to the banner so feed-grid post titles named "Review …"
    // don't collide with the nav link.
    const nav = page.getByRole('banner');
    await expect(nav.getByRole('button', { name: 'Explore' })).toBeVisible();
    await expect(nav.getByRole('link', { name: 'Collections', exact: true })).toHaveAttribute('href', '/collections');
    await expect(nav.getByRole('link', { name: 'Review', exact: true })).toHaveAttribute('href', '/review');
  });

  test('search box accepts input and lands on browse with ?q=', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    const searchbox = page.locator(tid('nav-search'));
    await expect(searchbox).toBeVisible();
    await searchbox.fill('test query');
    await searchbox.press('Enter');
    // Per +layout.svelte's handleSearch: from non-browse pages,
    // navigate TO browse (/) with the query.
    await expect(page).toHaveURL(/\/\?.*q=/);
  });

  // #1157 gave this control a destination of its own again.
  //
  // #850 had renamed it "Advanced search" -> "Search" and pointed it at
  // /search, because at that time the builder had moved into a panel and
  // `/search/advanced` was an empty shell. There is now a page's worth
  // of form on that route — the conditional search, a filter per
  // searchable metadata field, the search-by-image arm — so the label
  // and the destination went back together. Beside a search input that
  // already goes to /search, a control also labelled "Search" named
  // nothing the caller could not already do.
  //
  // Located by test id AND asserted for its accessible name: the id is
  // what stops the next rename from breaking the spec, and the name
  // assertion is what keeps the control an advertised affordance rather
  // than a bare icon.
  test('advanced-search link is named and goes to /search/advanced', async ({ page }) => {
    const link = page.locator(tid('nav-search-page'));
    await expect(link).toBeVisible();
    await expect(link).toHaveAccessibleName('Advanced search');
    await expect(link).toHaveAttribute('href', '/search/advanced');
  });

  // The other half. A control beside a search input that navigates away
  // and drops what you had typed would be a trap, so the link carries
  // the box's contents (#850, kept by #1157).
  test('advanced-search link carries what is typed in the nav search box', async ({ page }) => {
    await page.goto('/');
    await page.locator(tid('nav-search')).fill('quenlibrium');
    await expect(page.locator(tid('nav-search-page'))).toHaveAttribute(
      'href',
      '/search/advanced?q=quenlibrium',
    );
  });

  test('upload button is visible', async ({ page }) => {
    await expect(page.locator(tid('nav-upload-button'))).toBeVisible();
  });

  test('notifications button is visible', async ({ page }) => {
    await expect(page.getByRole('button', { name: 'Notifications' }).first()).toBeVisible();
  });

  test('messages button is visible', async ({ page }) => {
    await expect(page.getByRole('button', { name: 'Messages' }).first()).toBeVisible();
  });

  test('user menu opens and contains Sign out', async ({ page }) => {
    await page.locator(tid('nav-user-menu-trigger')).click();
    await expect(page.locator(tid('user-menu-sign-out'))).toBeVisible();
  });

  test('admin menu opens and contains every section', async ({ page }) => {
    await page.locator(tid('nav-admin-menu-trigger')).click();
    await expect(page.locator(tid('admin-menu-panel'))).toBeVisible();
    const expectedSections = [
      'Identity & access',
      'Content & metadata',
      'Federation',
      'System',
    ];
    for (const label of expectedSections) {
      await expect(page.getByRole('menuitem', { name: label })).toBeVisible();
    }
  });
});
