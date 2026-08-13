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

    // Typing into the nav search box arms a 250ms debounce inside
    // SearchBar; when it settles it calls the layout's handleSearch,
    // which NAVIGATES. Clicking the search-surface link inside that
    // window starts a second navigation that the debounce then
    // overtakes: the click lands on /search?q=…, ~130ms later the timer
    // fires, handleSearch sees a pathname that is no longer `/` and
    // goto()s the browse page instead — leaving the URL at /?q=<term>,
    // which is precisely the string #1024's CI failure reported.
    //
    // So wait for the commit to have HAPPENED before clicking. On
    // browse it is directly observable: handleSearch rewrites this
    // page's own URL to /?q=<term>. Once that lands, SearchBar's
    // lastCommitted equals what is in the box and no timer is left
    // armed to yank the page out from under the click. This is a wait
    // on an observed state change, not a sleep.
    await page.locator(tid('nav-search')).fill(term);
    await expect(page).toHaveURL(new RegExp(`/\\?.*q=${term}`, 'i'));
    // The link's href is computed reactively from what was typed
    // (+layout.svelte). Assert it is carrying the query before clicking
    // it — the ui-07 pattern; `toHaveAttribute` retries, a click does
    // not.
    await expect(page.locator(tid('nav-search-page'))).toHaveAttribute(
      'href',
      `/search?q=${encodeURIComponent(term)}`,
    );
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

  // ─────────────────────────────────────────────────────────────────
  // #1053 — /search is a RESULT SURFACE, so it is refined in place
  // ─────────────────────────────────────────────────────────────────
  //
  // The nav search box used to bounce you off /search: handleSearch
  // asked `pathname === '/'`, /search is not '/', so ~250ms after the
  // last keystroke it goto()'d the browse page — taking the focus and
  // the scroll position with it. The one surface built for refining a
  // search was the one surface you could not refine a search on.
  //
  // Nothing here clicks anything after typing (#1024): every step waits
  // on an observable commit — the URL, the page's own input, the tiles.

  /** Two DIFFERENT words, each taken from a real post's title, so
   *  neither search can land on the empty state and make the assertions
   *  below vacuous — the same reasoning as the tiles test above, which
   *  is why neither of these queries is a hardcoded string.
   *
   *  Scans the feed until it has two distinct words rather than taking
   *  posts 0 and 1 and skipping when they collide: a skipped test is
   *  not a passing one, and this is the spec for #1053 itself. */
  async function seededTerms(page: import('@playwright/test').Page) {
    const links = page.locator('a[href^="/posts/"]');
    await expect(links.first()).toBeVisible();
    const n = Math.min(await links.count(), 12);
    const terms: string[] = [];
    for (let i = 0; i < n && terms.length < 2; i++) {
      const label = (await links.nth(i).getAttribute('aria-label')) ?? '';
      const word = (label.match(/[A-Za-z]{5,}/g) ?? [])[0];
      if (word && !terms.includes(word)) terms.push(word);
    }
    expect(terms.length, `no two distinct searchable words in the first ${n} post titles`).toBe(2);
    return terms as [string, string];
  }

  test('nav search refines /search IN PLACE and never bounces to browse', async ({ page }) => {
    // Short viewport so the result grid overflows and the scroll
    // assertion below has something to measure. Even a single-hit page
    // is taller than 400px, so this does not depend on how much the
    // install happens to have seeded.
    await page.setViewportSize({ width: 1280, height: 400 });
    await page.goto('/');
    const [first, second] = await seededTerms(page);

    await page.goto(`/search?q=${encodeURIComponent(first)}`);
    const tiles = page.locator(
      'main a[href^="/assets/"], main a[href^="/posts/"], main a[href^="/collections/"]',
    );
    await expect(tiles.first()).toBeVisible({ timeout: 15_000 });

    // Scroll the results, so "the fix keeps your place" is measured and
    // not assumed. <main> is the scroller (the chrome-hide store owns
    // its listener), not the window.
    //
    // Down THEN up, and not because a user would: past 96px of downward
    // scroll the navbar auto-hides (chromeScroll), so a single jump
    // leaves the search box translated off-screen and unclickable. An
    // upward scroll brings the chrome back while leaving the page
    // scrolled — which is also exactly the state someone is in when
    // they reach for the search box after reading a page of results.
    await page.locator('main').evaluate((el) => el.scrollTo(0, 260));
    await page.locator('main').evaluate((el) => el.scrollTo(0, 120));
    const before = await page.locator('main').evaluate((el) => el.scrollTop);
    expect(before, 'the results did not scroll, so this test would prove nothing').toBeGreaterThan(
      0,
    );

    const nav = page.locator(tid('nav-search'));
    await nav.click();
    await nav.fill(second);

    // THE BUG: this used to become /?q=<second> about 250-450ms after
    // the keystroke. The negative assertion is separate from the
    // positive one so a failure says which half broke.
    await expect(page).toHaveURL(new RegExp(`/search\\?.*q=${second}`, 'i'));
    expect(new URL(page.url()).pathname).toBe('/search');

    // The page ADOPTED the new query rather than just letting the URL
    // change underneath a stale result set — a same-route navigation
    // does not remount the component, so this is the half that a
    // layout-only fix would have left broken.
    await expect(page.locator(tid('search-input'))).toHaveValue(second);
    await expect(tiles.first()).toBeVisible({ timeout: 15_000 });

    // Focus and scroll survive, as they do on browse.
    await expect(nav).toBeFocused();
    expect(await page.locator('main').evaluate((el) => el.scrollTop)).toBe(before);
  });

  test('refining the query keeps the kind chips and the facet filter', async ({ page }) => {
    await page.goto('/');
    const [first, second] = await seededTerms(page);

    await page.goto(`/search?q=${encodeURIComponent(first)}`);
    await expect(page.locator(tid('kind-chip-all'))).toBeVisible();

    // A kind chip: `?types=`. Wait for the URL it writes before
    // touching anything else — the chip re-queries as it navigates.
    await page.locator(tid('kind-chip-post')).click();
    await expect(page).toHaveURL(/types=post/);

    // A facet bucket: `?filter=<dimension>:<value>` (#907). Read the
    // token out of the URL rather than predicting it, so this does not
    // depend on which dimension the seed happens to produce.
    await page.locator(tid('open-facets')).click();
    await expect(page.locator(tid('search-slideover'))).toBeVisible();
    const bucket = page.locator('[data-testid^="facet-option-"]').first();
    await expect(bucket).toBeVisible({ timeout: 15_000 });
    await bucket.click();
    await expect(page).toHaveURL(/filter=/);
    const token = new URL(page.url()).searchParams.get('filter')!;
    await page.locator(tid('search-slideover-close')).click();

    const nav = page.locator(tid('nav-search'));
    await nav.click();
    await nav.fill(second);
    await expect(page.locator(tid('search-input'))).toHaveValue(second);

    // Both survive. A refinement that silently dropped either would
    // hand back a wider result set than the one on screen.
    const after = new URL(page.url());
    expect(after.pathname).toBe('/search');
    expect(after.searchParams.get('types')).toBe('post');
    expect(after.searchParams.getAll('filter')).toContain(token);
  });

  // The cost of adopting the URL in an effect is that EVERY URL change
  // now passes through it, so this counts the requests it causes.
  //
  // Two failure modes, one test, because they are opposite mistakes:
  //   - opening a post over the grid changes the URL (`?post=`) and must
  //     re-query NOTHING — the results underneath the overlay are the
  //     page you came back to;
  //   - a kind chip must re-query exactly ONCE. It applies its change
  //     and then navigates, so an adopter that treated the page's own
  //     write as an external one would fetch the same results twice.
  test('the URL watcher does not re-query for a modal, and only once for a chip', async ({
    page,
  }) => {
    const searches: string[] = [];
    page.on('request', (r) => {
      if (r.url().includes('/api/v1/search?')) searches.push(r.url());
    });

    await page.goto('/');
    const [term] = await seededTerms(page);
    await page.goto(`/search?q=${encodeURIComponent(term)}`);
    const postTiles = page.locator('main a[href^="/posts/"]');
    await expect(postTiles.first()).toBeVisible({ timeout: 15_000 });
    const afterLoad = searches.length;
    expect(afterLoad, 'the initial adoption should have run exactly one search').toBe(1);

    await postTiles.first().click();
    await expect(page).toHaveURL(/post=/);
    await expect(page.locator(tid('search-input'))).toHaveValue(term);
    expect(searches.length, 'opening a post re-ran the search underneath it').toBe(afterLoad);

    await page.goBack();
    await expect(page).not.toHaveURL(/post=/);
    expect(searches.length, 'closing the post re-ran the search').toBe(afterLoad);

    await page.locator(tid('kind-chip-post')).click();
    await expect(page).toHaveURL(/types=post/);
    await expect(page.locator(tid('search-input'))).toHaveValue(term);
    expect(
      searches.length - afterLoad,
      `a kind chip fired ${searches.length - afterLoad} searches, want exactly 1`,
    ).toBe(1);
  });

  // The other half of the same predicate, asserted so this fix cannot
  // quietly delete it: a page that does NOT consume `q` still sends you
  // to browse with the query, which is the whole reason the search box
  // is in the navbar on /account and /admin at all.
  test('nav search from a non-result surface still lands on browse', async ({ page }) => {
    await page.goto('/');
    const [term] = await seededTerms(page);

    await page.goto('/account');
    const nav = page.locator(tid('nav-search'));
    await nav.click();
    await nav.fill(term);

    await expect(page).toHaveURL(new RegExp(`^[^?]*/\\?.*q=${term}`, 'i'));
    expect(new URL(page.url()).pathname).toBe('/');
    await expectPageRendersCleanly(page);
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
