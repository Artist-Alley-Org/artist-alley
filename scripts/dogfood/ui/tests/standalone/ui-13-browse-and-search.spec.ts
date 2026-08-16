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

  /** Scroll the result list down past the navbar's auto-hide threshold
   *  and then back up until the navbar is on screen again. Returns the
   *  offset the page ends at, which is what the caller asserts against.
   *
   *  Down THEN up, and not because a user would: past 96px of downward
   *  scroll the navbar auto-hides (`chromeScroll`), which translates the
   *  search box off the top of the viewport. Playwright then reports
   *  `element is outside of the viewport` and retries the click until the
   *  test times out — the #1061 failure, which cost three full 30s
   *  attempts per run and mailed the owner every time it landed on dev.
   *
   *  Why the whole dance runs inside ONE `evaluate`, and why each jump
   *  waits for its own `scroll` event:
   *
   *    - Scroll events are COALESCED per frame. Two `scrollTo` calls
   *      issued from two CDP round-trips normally straddle a frame
   *      boundary and arrive as two events (down, then up — the chrome
   *      hides, then returns). When the renderer is busy — a grid still
   *      decoding a page of tiles, or a loaded CI runner — both land in
   *      one frame and arrive as ONE event carrying only the final
   *      offset. `120` with no `260` before it reads as a single
   *      downward scroll, so the chrome hides and never comes back.
   *      That is load-dependent, which is exactly why it presented as a
   *      flake. Measured here: 2 failures in 5 cold runs, unthrottled.
   *
   *    - The up-jump is RELATIVE to where the page actually landed, so a
   *      result set too short to reach 260px does not turn the second
   *      jump into a no-op (no movement, no event, chrome stays hidden).
   *
   *    - The "is it back?" test reads the chrome layer's own hidden
   *      class — `chromeScroll.hidden`, rendered — and not the box's
   *      rectangle. The rectangle is the wrong signal twice over: it is
   *      unchanged until the pending scroll event is dispatched, and it
   *      is mid-flight for the 200ms of the transition. Both read as
   *      "already fine" and both were tried before this.
   *
   *  Waits on observed events and rendered state throughout; no sleeps,
   *  and no `force: true` — a forced click would land somewhere a user
   *  could not reach, which is the class of bug this suite exists for. */
  async function scrollResultsAndKeepChrome(page: import('@playwright/test').Page) {
    return page.locator('main').evaluate(async (el) => {
      const raf = () => new Promise((r) => requestAnimationFrame(r));
      const layer = document.querySelector('[data-testid="chrome-layer"]')!;
      const hidden = () => layer.classList.contains('chrome-hidden-top');
      // Resolves once the app's own scroll listener has seen this jump.
      // Listeners fire in registration order and the store attached
      // first, so its handler has already run by the time this resolves;
      // the extra frame lets the class it sets reach the DOM. A jump the
      // scroller cannot make fires no event at all, so the reachable
      // target is computed rather than waited for.
      const jump = (to: number) =>
        new Promise<void>((resolve) => {
          const max = Math.max(0, el.scrollHeight - el.clientHeight);
          const target = Math.min(Math.max(0, to), max);
          if (el.scrollTop === target) return resolve();
          el.addEventListener('scroll', () => resolve(), { once: true });
          el.scrollTo(0, target);
        });

      await jump(260);
      await raf();
      // Nudge back up until the chrome is back. One nudge is enough
      // whenever the down-jump was seen on its own; the loop is what
      // makes a coalesced pair recoverable instead of terminal.
      for (let i = 0; i < 30 && hidden(); i++) {
        await jump(el.scrollTop - 20);
        await raf();
      }
      return el.scrollTop;
    });
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
    // not assumed, and leave the page in the state a real reader is in
    // when they reach for the search box: scrolled down, chrome back.
    const before = await scrollResultsAndKeepChrome(page);
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
  //   - a kind chip must re-query exactly ONCE. It writes the address
  //     and the adoption fetches; a chip that also fetched for itself
  //     would fetch the same results twice.
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

  // ─────────────────────────────────────────────────────────────────
  // #1060 — after Back, the address and the results must agree
  // ─────────────────────────────────────────────────────────────────
  //
  // They did not. The address went back, the page re-queried it
  // correctly, and then a snapshot captured mid-navigation put the NEWER
  // results back on screen underneath the older address — because
  // SvelteKit captures the entry you are leaving part-way through the
  // navigation away from it, and this page had already replaced its
  // results by then.

  /** The hrefs the grid is showing, in order — an identity for the
   *  result set on screen.
   *
   *  Neither "there are tiles" nor the count line can stand in for this.
   *  Two different queries routinely return the same NUMBER of hits (on
   *  this seed the two terms below return three each), and every result
   *  set fills the same grid, so both weaker checks pass while the wrong
   *  results are displayed — which is the bug. */
  async function resultFingerprint(page: import('@playwright/test').Page) {
    return page.evaluate(() =>
      [
        ...document.querySelectorAll(
          'main a[href^="/assets/"], main a[href^="/posts/"], main a[href^="/collections/"]',
        ),
      ]
        .map((a) => a.getAttribute('href'))
        .join(' '),
    );
  }

  test('Back and Forward render the address, not the newer results', async ({ page }) => {
    const searches: string[] = [];
    page.on('request', (r) => {
      if (r.url().includes('/api/v1/search?')) searches.push(r.url());
    });

    await page.goto('/');
    const [first, second] = await seededTerms(page);
    const input = page.locator(tid('search-input'));
    const tiles = page.locator(
      'main a[href^="/assets/"], main a[href^="/posts/"], main a[href^="/collections/"]',
    );

    await page.goto(`/search?q=${encodeURIComponent(first)}`);
    await expect(tiles.first()).toBeVisible({ timeout: 15_000 });
    const firstResults = await resultFingerprint(page);

    const nav = page.locator(tid('nav-search'));
    await nav.fill(second);
    await expect(page).toHaveURL(new RegExp(`/search\\?.*q=${second}`, 'i'));
    await expect(input).toHaveValue(second);
    await expect.poll(() => resultFingerprint(page)).not.toBe(firstResults);
    const secondResults = await resultFingerprint(page);
    const afterRefine = searches.length;

    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/search\\?.*q=${first}`, 'i'));
    // The results and the box both describe the address.
    await expect.poll(() => resultFingerprint(page)).toBe(firstResults);
    await expect(input).toHaveValue(first);
    // …and it is the snapshot that put them there. A fix that simply
    // re-queried everything would satisfy the three assertions above and
    // silently undo #584, which is why this counts requests instead of
    // trusting the screen.
    expect(searches.length, 'going Back re-queried a result set it already had').toBe(afterRefine);

    await page.goForward();
    await expect(page).toHaveURL(new RegExp(`/search\\?.*q=${second}`, 'i'));
    await expect.poll(() => resultFingerprint(page)).toBe(secondResults);
    await expect(input).toHaveValue(second);
    expect(searches.length, 'going Forward re-queried a result set it already had').toBe(
      afterRefine,
    );
  });

  // The same failure through a chip rather than through the query text,
  // because the two arrive by different routes: the chip is this page's
  // own control, so it is the one that used to leave its new result set
  // in the OLD entry's snapshot.
  test('Back out of a kind chip restores the unscoped result set', async ({ page }) => {
    await page.goto('/');
    const [term] = await seededTerms(page);
    const tiles = page.locator(
      'main a[href^="/assets/"], main a[href^="/posts/"], main a[href^="/collections/"]',
    );

    await page.goto(`/search?q=${encodeURIComponent(term)}`);
    await expect(tiles.first()).toBeVisible({ timeout: 15_000 });
    const unscoped = await resultFingerprint(page);

    const chip = page.locator(tid('kind-chip-post'));
    await chip.click();
    await expect(page).toHaveURL(/types=post/);
    await expect(chip).toHaveAttribute('aria-pressed', 'true');
    // If the scoped set is identical to the unscoped one there is
    // nothing here to get wrong, and asserting it would pass either way.
    await expect
      .poll(() => resultFingerprint(page), {
        message: 'the kind chip changed nothing, so this test would prove nothing',
      })
      .not.toBe(unscoped);

    await page.goBack();
    await expect(page).not.toHaveURL(/types=post/);
    await expect.poll(() => resultFingerprint(page)).toBe(unscoped);
    await expect(chip).toHaveAttribute('aria-pressed', 'false');
  });

  // #584, asserted on requests rather than on appearance. This is the
  // navigation that unmounts the page — /assets/{id} is its own route —
  // so coming back rebuilds the component, and the accumulated "load
  // more" pages exist only in the snapshot. Re-querying would hand back
  // a first page under a scroll offset measured against four.
  test('Back from an asset restores the loaded pages without re-querying', async ({ page }) => {
    const searches: string[] = [];
    page.on('request', (r) => {
      if (r.url().includes('/api/v1/search?')) searches.push(r.url());
    });

    // Short viewport, for the same reason the refine test uses one: it
    // makes the result page overflow whatever the seed happens to hold,
    // so the offset half of this is measured rather than skipped.
    await page.setViewportSize({ width: 1280, height: 400 });
    await page.goto('/');
    const [term] = await seededTerms(page);
    // Scoped to assets, because /assets/{id} is a route that UNMOUNTS
    // this page — the case #584 exists for, and the only one where the
    // loaded pages live nowhere but the snapshot. A post card opens
    // `?post=` OVER the grid and tears nothing down (covered above).
    await page.goto(`/search?q=${encodeURIComponent(term)}&types=asset`);
    const tiles = page.locator('main a[href^="/assets/"]');
    await expect(tiles.first(), `the seeded term "${term}" matched no asset`).toBeVisible({
      timeout: 15_000,
    });

    // A second page if the seed has one; the test is about restoring
    // whatever was loaded, so a single-page result set still exercises it.
    const more = page.getByRole('button', { name: /load more/i });
    if (await more.count()) {
      await more.click();
      await expect(more).toBeHidden({ timeout: 15_000 }).catch(() => {});
    }
    const loaded = await tiles.count();
    const before = searches.length;

    // Leave on a tile that is ALREADY fully on screen. Playwright scrolls
    // a click target into view first, which would move the very offset
    // being asserted — so the offset is whatever holds a whole tile.
    const target = await page.locator('main').evaluate((el) => {
      const links = [...document.querySelectorAll('main a[href^="/assets/"]')];
      for (const y of [240, 160, 100, 40]) {
        el.scrollTo(0, y);
        if (el.scrollTop === 0) continue;
        const seen = links.find((a) => {
          const r = a.getBoundingClientRect();
          return r.top >= 0 && r.bottom <= window.innerHeight;
        });
        if (seen) return { y: el.scrollTop, href: seen.getAttribute('href') };
      }
      return null;
    });
    expect(target, 'no scrolled position showed a whole tile, so nothing here is measured')
      .not.toBeNull();
    await page.locator(`main a[href="${target!.href}"]`).first().click();
    await expect(page).toHaveURL(/\/assets\//);

    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/search\\?.*q=${term}`, 'i'));
    await expect(tiles).toHaveCount(loaded);
    await expect
      .poll(async () => page.locator('main').evaluate((el) => el.scrollTop), { timeout: 10_000 })
      .toBe(target!.y);
    expect(searches.length, 'the restored page re-queried what the snapshot already held').toBe(
      before,
    );
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
