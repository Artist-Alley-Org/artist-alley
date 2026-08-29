// search-paging-1354.spec.ts
//
// /search pages itself (#1354): the reader never clicks for the next
// page, and the loader stays ahead of them.
//
// # ⛔ WHY THIS FILE SEEDS ITS OWN CORPUS
//
// The first version of this guard lived in ui-13 and picked its query
// from the browse wall, the way its neighbours do. It passed on the
// development stack (2,008 assets) and SKIPPED ON CI, whose corpus is
// 162 assets: the term it happened to choose returned ten hits, one
// page, and the guard stood itself down with "paging is unreachable".
//
// ⛔ THAT SKIP WAS NOT DECLARABLE, and the denominator manifest says
// why in its own words: a precondition that CI cannot meet is category
// 1, "the honest fix is to set the precondition ... or delete the
// test". Declaring it would have recorded agreement that this sprint's
// headline feature ships untested on the only machine that gates it.
//
// So the precondition is MANUFACTURED rather than hoped for. This file
// creates exactly one page's worth of rows plus one, all carrying a
// token nobody else's corpus holds, which makes "there is a second
// page" a property of the FIXTURE instead of a property of whichever
// catalogue subset the run was seeded from. It is then true on a fresh
// 162-asset CI database and on a 2,008-asset workstation alike.
//
// Measured before it was written, on the branch stack: 26 rows seed in
// 785ms, and `/api/v1/search?q=<token>&limit=25` answers 25 hits with a
// cursor, then 1 hit without one. Search indexing is SYNCHRONOUS on
// this path — an asset was searchable 10ms after its POST, on the first
// poll — so there is no job to wait for and no polling loop here.
//
// # ⚠️ THE ASSERTION IS "MORE THAN ONE PAGE ARRIVES", NEVER "PAGING
// # REACHES total_count"
//
// #1356: `/api/v1/search` stops handing out cursors long before the
// total it reports, on large result sets. Measured: `q=icon&types=asset`
// yields 75 of a reported 524 and then returns an empty cursor. It
// predates this work and is not caused by it, but it decides how this
// file may be written — a guard that walked to `total_count` would go
// red for a reason that has nothing to do with the paging rig.
//
// The fixture is deliberately small enough to sit far below that
// boundary (26 against the ~70 where it starts to bite), and this file
// asserts only that the SECOND page arrives.
//
// # ⭐ WHAT ACTUALLY DISCRIMINATES, AND IT IS NOT THE BUFFER SIZE
//
// The tempting assertion is "the buffer below the fold is at least one
// scrollport". It does not discriminate: measured at 1920x1080 the
// settled buffer is 0.27 screenfuls and at 1280x400 it is 1.73, and
// page one ALONE would satisfy the second of those. A number that a
// broken loader also produces is not a guard.
//
// What discriminates is the pair: the tail of page one sits BELOW THE
// FOLD (measured: 244px below at 1920x1080), and page two arrives
// anyway WITH THE READER AT scrollTop 0. A loader with no reach cannot
// do that — it is only notified once the sentinel genuinely enters the
// visible box, which requires scrolling that never happens here. That
// is exactly the #1159 defect this rig exists to prevent: an observer
// rooted on the document viewport keeps its `rootMargin` and loses the
// whole lookahead to `<main>`'s clip rect.

import { test, expect, type Page } from '../../helpers/test';
import type { APIRequestContext } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { includeMatureOnThisDevice } from '../../helpers/mature-content';

/** The `limit` /search asks for, from `runSearch` in
 *  web/src/routes/search/+page.svelte.
 *
 *  ⛔ IT IS ASSERTED AGAINST THE REAL REQUEST BELOW, not trusted. If the
 *  route's page size ever changes, a fixture sized against a stale
 *  constant would supply ONE page and this whole file would go quietly
 *  vacuous — the same silent-denominator failure #1348 is about. The
 *  assertion turns that into a loud failure that names the fix. */
const SEARCH_PAGE = 25;

/** One page, plus the row that forces a second one. Deliberately the
 *  minimum: every row here is soft-deleted at teardown and lands in the
 *  sweep backlog, so the fixture is sized by what the assertion needs
 *  and not by what would feel comfortable. */
const FIXTURE_ROWS = SEARCH_PAGE + 1;

/** In every fixture title and nowhere in any catalogue, so
 *  `/search?q=<TOKEN>` is exactly this file's rows on any corpus. */
const TOKEN = `pagingfixture${Date.now()}`;

const assetIds: string[] = [];

/** Create one searchable asset carrying the token. */
async function makeAsset(request: APIRequestContext, i: number): Promise<string> {
  // Unique bytes per asset: byte-identical uploads by one owner are
  // COLLAPSED by the content-address unique index, and a collapsed
  // asset would leave the fixture one row short of a second page —
  // which is the one property this file cannot afford to lose.
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`${TOKEN} ${i} ${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), `upload ${i}`).toBe(201);
  const { hash } = (await up.json()) as { hash: string };

  const r = await request.post('/api/v1/assets', {
    data: {
      title: `${TOKEN} plate ${i}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: 'txt',
    },
  });
  expect(r.status(), `create asset ${i} -> ${r.status()} ${await r.text()}`).toBe(201);
  return ((await r.json()) as { id: string }).id;
}

/** The hit ids the GRID is rendering. Reading the DOM rather than
 *  re-fetching is the point: the bug this catches is a page that was
 *  fetched and never reached the grid. */
function tiles(page: Page) {
  return page.locator(
    'main a[href^="/assets/"], main a[href^="/posts/"], main a[href^="/collections/"]',
  );
}

/** The rendered tile count, read only once it has STOPPED MOVING.
 *  #1170: the response landing and the tiles appearing are different
 *  frames, and this list grows in whole pages. */
async function settledTiles(page: Page): Promise<number> {
  let prev = -1;
  for (let i = 0; i < 40; i++) {
    const n = await tiles(page).count();
    if (n === prev && n > 0) return n;
    prev = n;
    await page.waitForTimeout(250);
  }
  return prev;
}

test.describe('#1354 /search pages itself', () => {
  test.describe.configure({ mode: 'serial' });
  test.setTimeout(180_000);

  // The mature axis is made inert for this file (#1345): the bootstrap
  // admin holds the ADR 0090 §2 exemption and has never opted in, so
  // their resting wall is mature-filtered by default. Nothing here is
  // about that axis, and the fixture rows are not mature, but stating
  // it keeps the grid this file measures free of an unrelated filter.
  test.beforeEach(async ({ page }) => {
    await includeMatureOnThisDevice(page);
  });

  test.beforeAll(async ({ request }) => {
    await loginAsAdminViaAPI(request);
    for (let i = 0; i < FIXTURE_ROWS; i++) {
      assetIds.push(await makeAsset(request, i));
    }

    // ⭐ THE FIXTURE PROVES ITS OWN PREMISE, ON THE WIRE, BEFORE ANY UI
    // IS DRIVEN. If the rows are not searchable, or not numerous enough
    // to force a cursor, every case below would be asserting about a
    // corpus that cannot answer the question — which is precisely the
    // failure that sent the first version of this guard to a skip.
    const r = await request.get(
      `/api/v1/search?limit=${SEARCH_PAGE}&q=${encodeURIComponent(TOKEN)}`,
    );
    expect(r.status()).toBe(200);
    const d = (await r.json()) as {
      hits: { id: string }[];
      total_count: number;
      next_cursor?: string;
    };
    expect(d.total_count, 'the fixture must be the whole of this token').toBe(FIXTURE_ROWS);
    expect(d.hits.length, 'the first page must be full').toBe(SEARCH_PAGE);
    expect(
      d.next_cursor,
      'the fixture did not produce a second page, so nothing below could be a paging test',
    ).toBeTruthy();
  });

  test.afterAll(async ({ request }) => {
    for (const id of assetIds) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
  });

  /** The shared body of the two viewport cases.
   *
   *  ⛔ NOTHING IN HERE CLICKS ANYTHING, and nothing scrolls. That is
   *  the assertion, not an economy. */
  async function pagesWithoutAClick(page: Page, label: string) {
    const searches: string[] = [];
    page.on('request', (r) => {
      if (r.url().includes('/api/v1/search?')) searches.push(r.url());
    });

    await page.goto(`/search?q=${encodeURIComponent(TOKEN)}`);
    await expect(tiles(page).first()).toBeVisible({ timeout: 20_000 });

    // ⭐ THE MANUAL PAGER IS GONE, asserted rather than assumed. A suite
    // that still found a button would be passing against the surface
    // #1354 replaces.
    await expect(
      page.getByRole('button', { name: /load more/i }),
      `${label}: /search still offers a manual pager; the browse wall it now matches has none`,
    ).toHaveCount(0);

    const settled = await settledTiles(page);

    // ⛔ THE PAGE SIZE IS VERIFIED, NOT ASSUMED. See SEARCH_PAGE.
    const firstFetch = searches.find((u) => !u.includes('cursor='));
    expect(firstFetch, `${label}: no first-page request was observed`).toBeTruthy();
    expect(
      new URL(firstFetch!).searchParams.get('limit'),
      `${label}: /search no longer asks for ${SEARCH_PAGE} per page, so FIXTURE_ROWS no ` +
        'longer straddles a page boundary and this file would be testing one page. ' +
        'Re-size the fixture against the new limit.',
    ).toBe(String(SEARCH_PAGE));

    // ⭐ MORE THAN ONE PAGE ARRIVED, WITH NO CLICK.
    //
    // ⚠️ Deliberately NOT "paging reached total_count" — see #1356 in
    // the header. The claim is that a second page arrives on its own.
    expect(
      searches.filter((u) => u.includes('cursor=')).length,
      `${label}: no cursor request was ever made, so the reader is still stranded ` +
        'without a pager',
    ).toBeGreaterThan(0);
    expect(
      settled,
      `${label}: a second page was fetched but never reached the grid`,
    ).toBeGreaterThan(SEARCH_PAGE);
    expect(settled, `${label}: the whole fixture should be on screen`).toBe(FIXTURE_ROWS);

    // ⭐ AND THE READER NEVER MOVED. This is the half that makes the
    // above a statement about the LOOKAHEAD rather than about an
    // observer that happened to see a sentinel in the viewport.
    const geometry = await page.locator('main').evaluate((el, pageSize) => {
      const box = el.getBoundingClientRect();
      const links = [
        ...document.querySelectorAll(
          'main a[href^="/assets/"], main a[href^="/posts/"], main a[href^="/collections/"]',
        ),
      ];
      return {
        scrollTop: el.scrollTop,
        clientHeight: el.clientHeight,
        scrollHeight: el.scrollHeight,
        // How far below the fold the tail of page ONE sits. The index is
        // derived from the page size rather than written as a literal,
        // so it cannot drift away from the constant asserted above.
        pageOneTailBelowFold: Math.round(
          (links[pageSize - 1]?.getBoundingClientRect().bottom ?? 0) - box.bottom,
        ),
      };
    }, SEARCH_PAGE);

    expect(
      geometry.scrollTop,
      `${label}: the reader was scrolled, so this proves nothing about reaching ahead of one`,
    ).toBe(0);

    // ⛔ THE DISCRIMINATOR. The tail of page one is below the fold, and
    // page two arrived anyway. A loader rooted on the document viewport
    // loses its whole margin to `<main>`'s clip rect (#1159) and is
    // notified only when the sentinel truly enters the visible box,
    // which never happens to a reader who has not scrolled.
    expect(
      geometry.pageOneTailBelowFold,
      `${label}: page one ended ON screen, so the loader never had to reach past the ` +
        'fold and this case cannot tell a working lookahead from a broken one',
    ).toBeGreaterThan(0);

    page.removeAllListeners('request');
  }

  test('⭐ it pages with NO CLICK, reaching past the fold, at 1080p', async ({ page }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await pagesWithoutAClick(page, '1080p');
  });

  test('⭐ and at 390px, where the reduced app has the same rig', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await pagesWithoutAClick(page, '390px');
  });
});
