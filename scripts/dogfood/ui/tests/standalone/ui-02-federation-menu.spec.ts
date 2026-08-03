// ui-02-federation-menu.spec.ts
//
// Verifies the /admin/federation tile grid renders the shipped
// surfaces as ENABLED links + the future surfaces as disabled
// tiles. Directly catches the bug class I caught manually this
// week — where the federation page showed 7 stale "Phase 1.22"
// disabled tiles instead of the 5 shipped + 2 future ones.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

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
  // should render as DISABLED tiles, not enabled links. #801 removed
  // the internal "Phase X" pill they used to carry: the dimmed,
  // aria-disabled styling alone conveys "not built yet", and no internal
  // release identifier is shown to operators.
  const futureTiles = ['Block list', 'Public fediverse compatibility'];

  // Internal roadmap identifiers that must never render (e.g. "Phase
  // 1.22.G", "1.22.K"). Kept in step with phase-badge-801.spec.ts.
  const INTERNAL_ID = /\b(?:phase\s+)?\d+\.\d+(?:\.[A-Za-z0-9-]+)?\b/i;

  for (const name of futureTiles) {
    test(`${name} tile is disabled with no phase pill`, async ({ page }) => {
      const card = page.locator('[aria-disabled="true"]').filter({ hasText: name });
      await expect(card).toBeVisible();
      // Still a disabled tile, never an enabled link.
      await expect(card.getByRole('link')).toHaveCount(0);
      // ...and it carries no internal release identifier.
      expect(await card.innerText()).not.toMatch(INTERNAL_ID);
    });
  }
});
