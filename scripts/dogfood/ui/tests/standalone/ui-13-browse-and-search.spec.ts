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

  // browse → search navigation. Renamed with the control in #850 (it
  // read "Advanced search" and pointed at a page that is now a panel);
  // located by test id so the next rename does not break it.
  //
  // The landing assertion got STRONGER rather than weaker, because
  // /search stopped being a text list: it now renders through the same
  // ContentGrid as browse, so the result of this navigation is a wall of
  // the same tiles. Asserting only the URL would have let the page come
  // back as a column of text and still pass.
  test('search-surface link navigates browse → /search and lands on the grid', async ({ page }) => {
    await page.goto('/');
    await page.locator(tid('nav-search-page')).click();
    await expect(page).toHaveURL(/\/search\b/);
    await expectPageRendersCleanly(page);
    // The kind chips are the search surface's own chrome — present at
    // every width, so this is not a viewport-dependent assertion.
    await expect(page.locator(tid('kind-chip-all'))).toBeVisible();
  });

  // The search surface renders the SAME cards as browse (#850). Before
  // it, a hit was a `<li data-testid="search-hit">` carrying a title, a
  // one-line summary and `score 1.000`; there is no such element now,
  // and an asset hit is an /assets/{id} tile like every other grid.
  test('search results render as tiles, not text rows', async ({ page }) => {
    await page.goto('/');
    // Search for a word taken from a post that IS on the feed, so the
    // result set is non-empty by construction. A hardcoded query would
    // make this test pass vacuously through the empty state on any
    // install whose seed does not happen to contain it — the
    // "accepted-but-empty" shape that makes a green assertion worthless.
    const firstPost = page.locator('a[href^="/posts/"]').first();
    await expect(firstPost).toBeVisible();
    const title = (await firstPost.getAttribute('aria-label')) ?? '';
    const term = (title.match(/[A-Za-z]{5,}/g) ?? [])[0] ?? '';
    expect(term, `no searchable word in the first post's title: "${title}"`).toBeTruthy();

    await page.locator(tid('nav-search')).fill(term);
    await page.locator(tid('nav-search-page')).click();
    await expect(page).toHaveURL(new RegExp(`/search\\?.*q=${term}`, 'i'));
    await expectPageRendersCleanly(page);

    // A hit is a TILE — a link into the entity, inside the shared grid.
    // Before #850 it was a `<li data-testid="search-hit">` carrying a
    // title, a one-line summary and `score 1.000`.
    const tiles = page.locator(
      'main a[href^="/assets/"], main a[href^="/posts/"], main a[href^="/collections/"]',
    );
    await expect(tiles.first()).toBeVisible({ timeout: 15_000 });
    await expect(page.locator('[data-testid="search-hit"]')).toHaveCount(0);
    // And the raw relevance score is not printed on results any more.
    await expect(page.locator('main')).not.toContainText(/score \d\.\d{3}/);
  });

  test('feed filter tabs are reachable', async ({ page }) => {
    await page.goto('/');
    // Two tabs, not four. `Team` and `Trending` were removed in #691/#705:
    // they were never wired to the API — the page sent them as an undeclared
    // `filter=` param the server ignored, so both silently returned `latest`.
    // The server's `feed` enum has only ever been [latest, following].
    // `Team` returns with the teams browse surface in #684.
    const tabs = ['Latest', 'Following'];
    for (const t of tabs) {
      await expect(page.getByRole('tab', { name: t })).toBeVisible();
    }
    // Guard the removal too — a tab reappearing here means the UI is offering
    // a filter the API cannot serve, which is the bug #691 fixed.
    for (const gone of ['Team', 'Trending']) {
      await expect(page.getByRole('tab', { name: gone })).toHaveCount(0);
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
