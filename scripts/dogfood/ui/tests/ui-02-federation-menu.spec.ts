// ui-02-federation-menu.spec.ts
//
// Verifies the /admin/federation tile grid renders the shipped
// surfaces as ENABLED links + the future surfaces as disabled
// tiles. Directly catches the bug class I caught manually this
// week — where the federation page showed 7 stale "Phase 1.22"
// disabled tiles instead of the 5 shipped + 2 future ones.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaUI } from '../helpers/auth';

test.describe('UI-02 federation tile grid', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
    await page.goto('/admin/federation');
  });

  // The five surfaces that shipped in 1.22.D and must be reachable.
  const liveTiles: Array<{ name: string; href: string }> = [
    { name: 'Peers',        href: '/admin/federation/peers' },
    { name: 'Directories',  href: '/admin/federation/directories' },
    { name: 'Shares',       href: '/admin/federation/shares' },
    { name: 'Outbox',       href: '/admin/federation/outbox' },
    { name: 'Inbox',        href: '/admin/federation/inbox' },
  ];

  for (const tile of liveTiles) {
    test(`${tile.name} tile is a live link to ${tile.href}`, async ({ page }) => {
      const link = page.getByRole('link').filter({ hasText: new RegExp(`^${tile.name}\\b`) });
      await expect(link).toBeVisible();
      await expect(link).toHaveAttribute('href', tile.href);
      // Sanity: clicking it actually lands on the right page (not
      // a 404 placeholder).
      await link.click();
      await expect(page).toHaveURL(tile.href);
      // The page renders SOMETHING — a heading or table or filter
      // bar. We don't assert specific content here (each tile's
      // own UI test does that); we just confirm the page wasn't
      // an error fallback.
      await expect(page.locator('main')).toBeVisible();
    });
  }

  // The two surfaces that legitimately aren't shipped yet — they
  // should render as DISABLED tiles, not enabled links.
  const futureTiles = [
    { name: 'Block list',                       phase: '1.22.G' },
    { name: 'Public fediverse compatibility',    phase: '1.22.K' },
  ];

  for (const tile of futureTiles) {
    test(`${tile.name} tile is disabled with phase pill`, async ({ page }) => {
      const card = page.locator('[aria-disabled="true"]').filter({ hasText: tile.name });
      await expect(card).toBeVisible();
      await expect(card).toContainText(`Phase ${tile.phase}`);
    });
  }
});
