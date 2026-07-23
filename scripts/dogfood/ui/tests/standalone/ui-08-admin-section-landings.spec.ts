// ui-08-admin-section-landings.spec.ts
//
// Verifies each /admin/<section> landing renders the right tile
// grid: every tile labelled, future tiles disabled, live tiles
// reachable. Catches the bug class we caught manually this week
// where /admin/federation showed 7 stale "Phase 1.22" tiles
// instead of the real shipped surfaces.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

const SECTION_LANDINGS = [
  {
    slug: 'identity',
    expectedTiles: ['Users', 'Roles & capabilities', 'Groups & teams'],
  },
  {
    slug: 'content',
    // Content section — exercise a couple known tiles. We don't
    // pin the full list here; that would be brittle.
    expectedTiles: ['Asset types'],
  },
  {
    slug: 'automation',
    expectedTiles: ['Workflow states'],
  },
  {
    slug: 'federation',
    expectedTiles: ['Peers', 'Directories', 'Shares', 'Outbox', 'Inbox'],
  },
  {
    slug: 'system',
    expectedTiles: ['Site', 'SMTP', 'Authentication', 'AI'],
  },
];

test.describe('UI-08 admin section landings', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  for (const section of SECTION_LANDINGS) {
    test(`${section.slug} landing shows expected tiles`, async ({ page }) => {
      await page.goto(`/admin/${section.slug}`);
      await expect(page.locator('main')).toBeVisible();
      for (const tile of section.expectedTiles) {
        // exact:true — substring match silently flips to a strict-mode
        // failure when another tile title contains the chip text as a
        // substring (e.g. "AI" matches "Maintenance" via the "ai" infix).
        await expect(
          page.getByRole('heading', { name: tile, level: 3, exact: true }),
        ).toBeVisible();
      }
    });
  }

  // #525 — the "Anonymous browse" tile (moderation) shipped drift: its
  // backend (public_mode) landed in v0.5.0 but the tile stayed future.
  // It's now a live front door to the toggle on the site-settings page.
  // Guard both facts so it can't silently regress to a disabled badge.
  test('anonymous-browse tile is live and links to the public-mode toggle (#525)', async ({ page }) => {
    await page.goto('/admin/moderation');
    const tile = page.getByRole('link', { name: /Anonymous browse/i });
    await expect(tile).toBeVisible();
    await expect(tile).toHaveAttribute('href', '/admin/system/site');
    await tile.click();
    await expect(page).toHaveURL(/\/admin\/system\/site$/);
    // The public-access section (housing the toggle) is the reason the
    // tile exists.
    await expect(page.getByRole('heading', { name: /Public access/i })).toBeVisible();
  });

  // #525 — reindex / checksum_verify / find_orphans were placeholder
  // duplicates of pages that shipped elsewhere; they were removed from
  // the Maintenance-tools section. Assert they're gone (the canonical
  // live routes are exercised by their own storage/search specs).
  test('duplicate maintenance-tools tiles are removed (#525)', async ({ page }) => {
    await page.goto('/admin/tools');
    await expect(page.locator('main')).toBeVisible();
    for (const gone of ['Reindex search', 'Verify checksums', 'Find orphan bytes']) {
      await expect(page.getByRole('heading', { name: gone, level: 3, exact: true })).toHaveCount(0);
    }
  });

  // Sanity: catch the federation menu bug specifically — if a real
  // tile gets demoted back to disabled (status=future), the test
  // fails because aria-disabled would be true. The ui-02 spec
  // already covers this for federation; we add identity here too.
  test('identity tiles are not disabled', async ({ page }) => {
    await page.goto('/admin/identity');
    const disabledTiles = await page.locator('[aria-disabled="true"]').allTextContents();
    // No live tile (Users, Roles, Groups) should appear in the
    // disabled list.
    for (const liveTile of ['Users', 'Roles & capabilities', 'Groups & teams']) {
      expect(disabledTiles.some((t) => t.includes(liveTile))).toBe(false);
    }
  });
});
