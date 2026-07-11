// ui-13-browse-and-search.spec.ts
//
// Browse + search flows. Browse shows the feed; clicking a post
// card opens the post modal/page; the modal renders the asset
// viewer + the details sidebar.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';
import { tid } from '../../helpers/testids';

test.describe('UI-13 browse + search', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('browse home renders the post grid', async ({ page }) => {
    await page.goto('/');
    await expectPageRendersCleanly(page);
    // The grid should contain at least one post link.
    const postLinks = page.locator('a[href^="/posts/"]');
    await expect(postLinks.first()).toBeVisible();
  });

  test('clicking a post card opens the post URL', async ({ page }) => {
    await page.goto('/');
    const firstPost = page.locator('a[href^="/posts/"]').first();
    const href = await firstPost.getAttribute('href');
    await firstPost.click();
    // URL may include the post id as query (?post=…) for the
    // modal route or as path (/posts/{id}) for the full-page
    // variant — both forms are acceptable.
    await expect(page).toHaveURL(new RegExp(`(${href?.replace(/\//g, '\\/')}|post=)`));
  });

  test('search box on browse updates the URL with ?q=', async ({ page }) => {
    await page.goto('/');
    const searchbox = page.locator(tid('nav-search'));
    await searchbox.fill('a');
    await searchbox.press('Enter');
    // From the browse page, handleSearch keeps the user in place
    // and updates the query string (see +layout.svelte).
    await expect(page).toHaveURL(/\/\?.*q=/);
    await expectPageRendersCleanly(page);
  });

  test('Advanced search link goes to /search', async ({ page }) => {
    await page.goto('/');
    await page.getByRole('link', { name: 'Advanced search' }).click();
    await expect(page).toHaveURL(/\/search\b/);
    await expectPageRendersCleanly(page);
  });

  test('feed filter tabs are reachable', async ({ page }) => {
    await page.goto('/');
    const tabs = ['Team', 'Trending', 'Latest', 'Following'];
    for (const t of tabs) {
      await expect(page.getByRole('tab', { name: t })).toBeVisible();
    }
  });

  test('Latest tab is the default selection', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('tab', { name: 'Latest' })).toHaveAttribute(
      'aria-selected',
      'true',
    );
  });
});
