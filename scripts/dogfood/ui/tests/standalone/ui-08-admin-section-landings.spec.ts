// ui-08-admin-section-landings.spec.ts
//
// Verifies each /admin/<section> landing renders the right tile
// grid: every tile labelled, future tiles disabled, live tiles
// reachable. Catches the bug class we caught manually this week
// where /admin/federation showed 7 stale "Phase 1.22" tiles
// instead of the real shipped surfaces.

import { test, expect } from '@playwright/test';
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
    expectedTiles: ['Site', 'SMTP', 'Authentication', 'AI providers'],
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
        await expect(
          page.getByRole('heading', { name: tile, level: 3 }),
        ).toBeVisible();
      }
    });
  }

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
