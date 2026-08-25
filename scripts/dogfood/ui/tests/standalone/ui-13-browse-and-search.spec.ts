// ui-13-browse-and-search.spec.ts
//
// Browse + search flows. Browse shows the feed; clicking a post
// card opens the post modal/page; the modal renders the asset
// viewer + the details sidebar.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';
import { tid } from '../../helpers/testids';

// TYPING DOES NOT SEARCH (#1156).
//
// The nav box used to commit on a 250ms keystroke debounce, so every one
// of these flows drove it by `fill()` alone and waited for the URL to
// change by itself. Owner direction retired that: suggestions may appear
// while typing, but the feed and the address change only on an EXPLICIT
// commit — Enter, a picked suggestion, or the clear button.
//
// This helper is how every spec below drives the box, and it asserts
// BOTH halves so the specs are the regression guard for #1156 rather
// than merely surviving it:
//
//   - the negative: after typing and waiting out the old debounce window
//     plus a wide margin, the URL has NOT moved. On the pre-#1156
//     component this fails here, which is what makes these tests a
//     guard;
//   - the positive: Enter commits, and the caller asserts where it went.
//
// The wait is a real one, not a sleep dressed up: there is no "the
// navigation did not happen" event to await, so the only way to observe
// an absence is to outlast the window in which it would have occurred.
// 900ms is ~3.6x the retired 250ms debounce.
const RETIRED_DEBOUNCE_MARGIN_MS = 900;

async function typeWithoutSearching(page: import('@playwright/test').Page, term: string) {
  const nav = page.locator(tid('nav-search'));
  const before = page.url();
  await nav.click();
  await nav.fill(term);
  await page.waitForTimeout(RETIRED_DEBOUNCE_MARGIN_MS);
  expect(
    page.url(),
    'typing in the nav search box navigated — #1156 says only an explicit commit may',
  ).toBe(before);
  return nav;
}

/** Type, prove nothing happened, then commit with Enter. */
async function commitNavSearch(page: import('@playwright/test').Page, term: string) {
  const nav = await typeWithoutSearching(page, term);
  await nav.press('Enter');
  return nav;
}

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

  test('search box on browse searches on Enter, and only on Enter', async ({ page }) => {
    await page.goto('/');
    // typeWithoutSearching asserts the URL does not move while typing
    // (#1156); Enter then commits in place (+layout.svelte handleSearch).
    await commitNavSearch(page, 'a');
    await expect(page).toHaveURL(/\/\?.*q=/);
    await expectPageRendersCleanly(page);
  });

  // browse → advanced search. #1157 gave this control a destination of
  // its own: the form, not the result surface the nav box already
  // reaches. Located by test id so the next rename does not break it.
  test('advanced-search link navigates browse → /search/advanced', async ({ page }) => {
    await page.goto('/');
    await page.locator(tid('nav-search-page')).click();
    await expect(page).toHaveURL(/\/search\/advanced\b/);
    await expectPageRendersCleanly(page);
    // The page's own chrome: the conditional search and the metadata
    // field filters are what distinguish it from the result surface.
    await expect(page.locator(tid('advanced-search-page'))).toBeVisible();
    await expect(page.locator(tid('advanced-conditions'))).toBeVisible();
    await expect(page.locator(tid('advanced-field-filters'))).toBeVisible();
  });

  // The result surface still lands on the grid. This used to be asserted
  // as a side effect of the nav control's destination; now that the
  // control goes elsewhere, /search is reached directly and the
  // assertion is unchanged.
  //
  // It is STRONGER than a URL check on purpose: /search renders through
  // the same ContentGrid as browse, so asserting only the address would
  // let the page come back as a column of text and still pass.
  test('/search lands on the grid surface', async ({ page }) => {
    await page.goto('/search');
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

    // This test used to reach /search by typing into the nav box and
    // then clicking the search-surface link, and carried a long note
    // about racing the 250ms commit debounce: the click would start one
    // navigation and the timer would overtake it with another, stranding
    // the URL at /?q=<term> (#1024's CI failure).
    //
    // #1156 deleted that race along with the timer — typing starts no
    // navigation at all — and #1157 pointed that link at the advanced
    // FORM rather than at /search. Both reasons for the dance are gone,
    // so the test goes where it was always trying to go and asserts the
    // thing it was always about: a hit is a tile, not a text row.
    await page.goto(`/search?q=${encodeURIComponent(term)}`);
    await expect(page).toHaveURL(new RegExp(`/search\\?.*q=${term}`, 'i'));

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
    await expectPageRendersCleanly(page);
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
   *  not a passing one, and this is the spec for #1053 itself.
   *
   *  The scan covers EVERY rendered post, not a fixed first twelve, and
   *  the difference is a whole suite run. The head of the wall is
   *  whatever posted most recently, which on a long-lived dev instance
   *  is a block of fixture posts other specs left behind — and one
   *  spec's fixtures share a title template, so the first N can be a
   *  single searchable word repeated N times. Thirteen of them was
   *  enough to red seven tests in this file at once, for a reason with
   *  nothing to do with search. Every rendered card is still a bounded
   *  scan (one page of the feed) and it reaches past any one family of
   *  fixtures; the accumulation itself is #1198's. */
  /** Distinct 5+-letter words off the rendered post titles, in wall
   *  order. Stops once it has `want` of them, or runs out of wall. */
  async function wallWords(page: import('@playwright/test').Page, want: number) {
    const links = page.locator('a[href^="/posts/"]');
    await expect(links.first()).toBeVisible();
    const n = await links.count();
    const terms: string[] = [];
    for (let i = 0; i < n && terms.length < want; i++) {
      const label = (await links.nth(i).getAttribute('aria-label')) ?? '';
      for (const word of label.match(/[A-Za-z]{5,}/g) ?? []) {
        if (!terms.includes(word)) {
          terms.push(word);
          break;
        }
      }
    }
    return { terms, n };
  }

  async function seededTerms(page: import('@playwright/test').Page) {
    const { terms, n } = await wallWords(page, 2);
    expect(
      terms.length,
      `no two distinct searchable words across ${n} rendered post titles — the whole ` +
        'page is one fixture family (#1198), not a search defect',
    ).toBe(2);
    return terms as [string, string];
  }

  /** A term the ASSET index actually answers.
   *
   *  ⛔ A WORD OFF A POST TITLE IS NOT A WORD IN AN ASSET TITLE, and the
   *  one caller that scopes its search to `types=asset` was relying on
   *  the two coinciding. They coincide by luck: the wall shows posts,
   *  newest first, and whether the newest post happens to share a word
   *  with any asset is a property of whatever the install last seeded.
   *  It stopped being true the moment the catalogue grew and the CI
   *  coverage profile selected a different subset — the wall's newest
   *  post became `Cinematic cut — Big Buck Bunny`, no asset is titled
   *  "Cinematic", and the test failed all three attempts with an empty
   *  grid that had nothing to do with what it asserts.
   *
   *  So the term is CONFIRMED against the same endpoint the test then
   *  drives, rather than assumed. Candidates still come off the wall, so
   *  it is still a word this corpus really contains. */
  async function seededAssetTerm(page: import('@playwright/test').Page) {
    const { terms, n } = await wallWords(page, 24);
    for (const term of terms) {
      // ⚠️ THE KEY IS `hits`, NOT `items`. /search answers a ranked
      // envelope; the collection endpoints answer `items`. Reading the
      // wrong one returns 0 for every term and turns this probe into a
      // guard that rejects the whole corpus.
      const hits = await page.evaluate(async (q: string) => {
        const r = await fetch(
          `/api/v1/search?q=${encodeURIComponent(q)}&types=asset&limit=1`,
        );
        if (!r.ok) return 0;
        return ((await r.json()).hits ?? []).length as number;
      }, term);
      if (hits > 0) return term;
    }
    throw new Error(
      `none of the ${terms.length} candidate words off ${n} rendered post titles ` +
        `matched a single ASSET (${terms.join(', ')}). A post title and an asset ` +
        'title share words only by luck; if this install genuinely has no ' +
        'searchable asset the failure is the seed, not the search.',
    );
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

    // #1156 — typing changes nothing; Enter commits. The helper asserts
    // the URL does not move while typing, which on this surface is also
    // the #1053 property under test: a refinement must not bounce you to
    // browse, and it cannot bounce you anywhere if it does not fire.
    const nav = await commitNavSearch(page, second);

    // THE #1053 BUG: this used to become /?q=<second>. The negative
    // assertion is separate from the positive one so a failure says
    // which half broke.
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

    // Scroll survives UP TO WHAT THE NEW RESULT SET CAN HOLD.
    //
    // The refinement swaps in a different, usually SHORTER wall, and the
    // browser clamps scrollTop to that wall's maximum — losing the extra
    // is the content being shorter, not the page jumping. The bare
    // `toBe(before)` this replaced only held because the assertion used
    // to run before the new grid had rendered: typing committed after a
    // ~250ms debounce, so the read landed on the OLD wall's height. With
    // the commit now explicit (#1156) the new wall is up by the time we
    // look, and the clamp is visible.
    //
    // Asserting `min(before, max)` keeps the property #1053 is about —
    // the offset is preserved, not reset — and the `> 0` guard keeps it
    // from passing vacuously if the page did jump to the top and the new
    // wall happened to be short.
    const { top, max } = await page
      .locator('main')
      .evaluate((el) => ({ top: el.scrollTop, max: Math.max(0, el.scrollHeight - el.clientHeight) }));
    expect(top).toBe(Math.min(before, max));
    expect(top, 'the refinement reset the scroll offset to the top').toBeGreaterThan(0);
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

    await commitNavSearch(page, second);
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
  /** The number of tiles on screen, read only once it has stopped
   *  changing (#1170).
   *
   *  Any count taken while an append is in flight is a torn reading, and
   *  a torn baseline turns a correct restore into a red test. Two
   *  consecutive equal samples a poll apart is the settle condition; the
   *  count must also be non-zero, so "nothing rendered at all" cannot
   *  masquerade as stability. */
  async function settledCount(tiles: import('@playwright/test').Locator): Promise<number> {
    let previous = -1;
    let current = -1;
    await expect
      .poll(
        async () => {
          previous = current;
          current = await tiles.count();
          return current > 0 && current === previous;
        },
        {
          timeout: 15_000,
          intervals: [250, 250, 250, 250, 500],
          message: 'the result grid never stopped growing, so no baseline is measurable',
        },
      )
      .toBe(true);
    return current;
  }

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

    await commitNavSearch(page, second);
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
    const term = await seededAssetTerm(page);
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
    // #1170 — read the count only once it has stopped moving.
    //
    // This used to be a bare `tiles.count()` taken straight after the
    // "load more" button went away, on the assumption that the button
    // disappearing means the page it fetched has rendered. It does not:
    // the button is bound to `hasMore`, which the store clears when the
    // RESPONSE lands, while the tiles append on a later render. Measured
    // on the dev stack: the button hid 56ms after the click with 25
    // tiles on screen, and the grid settled at 28 about 50ms later.
    //
    // So `loaded` was 25 for a grid that ended up holding 28, and the
    // restore assertion below then compared the snapshot's honest 28
    // against a torn reading — failing on every single local run while
    // CI, whose prod-shape build renders inside the polling window,
    // stayed green. The subject of this test is what SURVIVES the round
    // trip, so the baseline it measures against has to be settled first.
    const loaded = await settledCount(tiles);
    const before = searches.length;

    // Leave on a tile that is ALREADY fully on screen. Playwright scrolls
    // a click target into view first, which would move the very offset
    // being asserted — so the offset is whatever holds a whole tile.
    //
    // "On screen" means inside the SCROLL CONTAINER, not inside the
    // viewport (#1170). This used to compare the tile's rect against
    // `0` and `window.innerHeight`; `main` starts below the navbar and
    // is 333px tall against a 400px viewport, so a tile clipped by
    // main's own top edge still satisfied that test. Playwright then
    // scrolled the grid UP by the navbar's height to click it, and the
    // page was left at 202 rather than the 240 recorded here — so the
    // app snapshotted 202, restored 202, and the assertion below failed
    // against a number that had stopped being true before the
    // navigation even happened. The restore was right; the reference
    // point was wrong.
    const target = await page.locator('main').evaluate((el) => {
      const links = [...document.querySelectorAll('main a[href^="/assets/"]')];
      for (const y of [240, 160, 100, 40]) {
        el.scrollTo(0, y);
        if (el.scrollTop === 0) continue;
        const box = el.getBoundingClientRect();
        const seen = links.find((a) => {
          const r = a.getBoundingClientRect();
          return r.top >= box.top && r.bottom <= box.bottom;
        });
        if (seen) return { y: el.scrollTop, href: seen.getAttribute('href') };
      }
      return null;
    });
    expect(target, 'no scrolled position showed a whole tile, so nothing here is measured')
      .not.toBeNull();

    // Then STOP predicting the departure offset and read it (#1170).
    //
    // Choosing an unclipped tile above removes the common case, but not
    // the race: the navbar auto-hides past 96px of scroll and animates
    // back, so `main`'s box is still moving while the tile is picked.
    // Under a full-suite load the box settled a navbar's height (38px)
    // away from where it was measured, Playwright scrolled to correct
    // for it, and the page departed from 202 while this test went on
    // asserting 240 — the same "the app is right, the reference point
    // is stale" failure as above, one layer down. So the scroll that
    // the click would have performed is performed HERE, and the offset
    // the assertion uses is the one the page actually left on.
    const link = page.locator(`main a[href="${target!.href}"]`).first();
    await link.scrollIntoViewIfNeeded();
    const departure = await page.locator('main').evaluate((el) => el.scrollTop);
    expect(
      departure,
      'the grid ended up at the top, so the offset half of this test measures nothing',
    ).toBeGreaterThan(0);
    await link.click();
    await expect(page).toHaveURL(/\/assets\//);

    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/search\\?.*q=${term}`, 'i'));
    await expect(tiles).toHaveCount(loaded);
    await expect
      .poll(async () => page.locator('main').evaluate((el) => el.scrollTop), { timeout: 10_000 })
      .toBe(departure);
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
    // #1156 — the helper's negative half matters most HERE: on a
    // non-result surface the old debounce navigated you away from the
    // page you were on, mid-keystroke. Now nothing happens until Enter.
    await commitNavSearch(page, term);

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
