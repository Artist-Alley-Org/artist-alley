// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1407: a successful publish reaches the page the artist was standing on.
//
// # The bug
//
// The upload modal is mounted ONCE, globally, in `routes/+layout.svelte`,
// so it opens over whatever route the artist is on. `handleSubmit` was
// `await upload.submit()` and nothing else, and `submit()` ends in
// `reset()`: every row dropped, the dialog closed, no navigation, no
// refetch, no signal out. The posts existed; the page did not know. The
// sharpest shape of it is the collection empty state: press "upload your
// first", complete the upload INTO that collection, and the empty state
// is still on screen telling you the collection is empty.
//
// # ⚠️ WHY THE OBVIOUS ASSERTION IS VACUOUS
//
// "the create call returned 201" passes on broken `dev`. It always did,
// that is exactly why the bug was invisible to the existing suites, and
// why every case here asserts VISIBLE CONTENT instead.
//
// # ⚠️ AND WHY "visible" is not enough on its own
//
// A page that reloaded itself would satisfy a bare visibility assertion
// while being the fallback the issue rules out. So every case stamps
// `window.__aa1407` before submitting and reads it back after: a reload
// wipes it, and a navigation changes `page.url()`. Both are asserted, so
// "it appeared" cannot be bought with either.
//
// # What each case would catch
//
//  1. THE HEADLINE. Collection empty state, one file, one post. Fails on
//     `274cc337`, where the empty state never goes away.
//  2. N >= 2. Three files, one post per file, and the assertion is that
//     each created id is on the wall EXACTLY ONCE. A half-arrived batch
//     and a doubled card both fail here; one file only would catch
//     neither.
//  3. 390px. The same headline at the mobile width, because the modal
//     and the wall both reflow there.
//  4. A REFUSED PUBLISH REFRESHES NOTHING. `POST /posts` is failed at
//     the wire. The modal must stay open with its error, and the
//     collection must still say it is empty. A surface that refreshed
//     anyway would be telling the artist their work is somewhere.
//  5. NO DOUBLING ACROSS TWO PUBLISHES. The second refresh must not
//     re-add what the first one already put on the wall.
//  6. ASSET ONLY. No post at all, so the only surface it can land on is
//     the author's own uploads grid. Covers the third create flow and
//     the profile consumer in one case.
//  7. THE FEED. The other end of the caller census: the modal opened
//     from `/` with nothing collection-shaped about it.
//  8. /create IS NOT A REFRESH CONSUMER. It navigates to what it made,
//     and that is preserved rather than replaced.

import type { APIRequestContext, Page, Response } from '@playwright/test';
import { test, expect } from '../../helpers/test';
import { tid } from '../../helpers/testids';
import { trackUploadedRows } from '../../helpers/uploaded-rows';

const STAMP = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

/** Rows the BROWSER made. Deleted in afterEach; see uploaded-rows.ts. */
const uploaded = trackUploadedRows();

/**
 * Post ids the page created, in the order the server answered.
 *
 * The POST is made by the app, so its id never reaches the test except
 * by watching the traffic. Identity recorded AT CREATION, which is what
 * lets case 2 say "this exact post, exactly once" rather than counting
 * tiles and hoping.
 */
function watchCreatedPostIds(page: Page): string[] {
  const ids: string[] = [];
  page.on('response', (res: Response) => {
    if (res.request().method().toUpperCase() !== 'POST') return;
    if (res.status() !== 201) return;
    let path: string;
    try {
      path = new URL(res.url()).pathname;
    } catch {
      return;
    }
    if (path !== '/api/v1/posts') return;
    void res
      .json()
      .then((b) => {
        const id = (b as { id?: string }).id;
        if (id) ids.push(id);
      })
      .catch(() => undefined);
  });
  return ids;
}

/** The same, for assets: the asset-only flow makes no post. */
function watchCreatedAssetIds(page: Page): string[] {
  const ids: string[] = [];
  page.on('response', (res: Response) => {
    if (res.request().method().toUpperCase() !== 'POST') return;
    if (res.status() !== 201) return;
    let path: string;
    try {
      path = new URL(res.url()).pathname;
    } catch {
      return;
    }
    if (path !== '/api/v1/assets') return;
    void res
      .json()
      .then((b) => {
        const id = (b as { id?: string }).id;
        if (id) ids.push(id);
      })
      .catch(() => undefined);
  });
  return ids;
}

function fixtureFiles(n: number, label: string) {
  return Array.from({ length: n }, (_, i) => ({
    name: `aa1407-${label}-${STAMP}-${i}.txt`,
    mimeType: 'text/plain',
    buffer: Buffer.from(`#1407 ${label} ${STAMP} ${i}`),
  }));
}

/**
 * Mark this exact document. A reload replaces it and the mark is gone;
 * a client-side navigation keeps it but moves `page.url()`. Read both
 * back and neither escape is available.
 */
async function markDocument(page: Page): Promise<{ url: string }> {
  await page.evaluate(() => {
    (window as unknown as Record<string, unknown>).__aa1407 = 'same-document';
  });
  return { url: page.url() };
}

/**
 * The marker half on its own, for the one case whose address is SUPPOSED
 * to change: the reader submits a new query, so `page.url()` moving is
 * the behaviour rather than the failure. The document surviving is still
 * the thing that proves nothing reloaded.
 */
async function expectNoReload(page: Page, why: string) {
  expect(
    await page.evaluate(
      () => (window as unknown as Record<string, unknown>).__aa1407 ?? null,
    ),
    `${why}: the marker is gone, so the page RELOADED`,
  ).toBe('same-document');
}

async function expectSameDocument(page: Page, before: { url: string }, why: string) {
  expect(
    await page.evaluate(
      () => (window as unknown as Record<string, unknown>).__aa1407 ?? null,
    ),
    `${why}: the marker is gone, so the page RELOADED`,
  ).toBe('same-document');
  expect(page.url(), `${why}: the page navigated away`).toBe(before.url);
}

/** Drive the modal from wherever it is already open, and publish. */
async function pickAndPublish(
  page: Page,
  files: ReturnType<typeof fixtureFiles>,
  opts: { mode?: 'one-post' | 'one-per-file'; post?: boolean } = {},
) {
  await expect(page.getByRole('dialog')).toBeVisible();
  await page.locator(tid('upload-file-input')).setInputFiles(files);
  // Every row must actually be ready. Publishing while a row is still
  // in flight is refused by the store, and a case that raced that would
  // fail for a reason it is not about.
  await expect(page.getByText(/Ready|Already uploaded/)).toHaveCount(files.length, {
    timeout: 60_000,
  });
  if (opts.post === false) {
    await page.locator(tid('upload-compose-enabled')).uncheck();
  } else if (opts.mode) {
    await page.locator(tid('upload-post-mode')).selectOption(opts.mode);
  }
  await page.locator(tid('upload-submit')).click();
}

test.describe('#1407 a publish reaches the page it was made from', () => {
  let collectionId = '';

  test.beforeEach(async ({ page, request }) => {
    uploaded.watch(page);
    const created = await request.post('/api/v1/collections', {
      data: {
        name: `#1407 refresh ${STAMP}-${Math.random().toString(36).slice(2, 7)}`,
        description: 'fixture for #1407',
      },
    });
    expect(created.status(), 'fixture collection must be created').toBe(201);
    collectionId = ((await created.json()) as { id: string }).id;
  });

  test.afterEach(async ({ request }) => {
    await uploaded.cleanup(request);
    if (collectionId) {
      await request.delete(`/api/v1/collections/${collectionId}`).catch(() => undefined);
    }
  });

  // ── 1. the headline ────────────────────────────────────────────────
  test('the collection empty state fills in without a reload', async ({ page }) => {
    const postIds = watchCreatedPostIds(page);
    await page.goto(`/collections/${collectionId}`);

    // ⚠️ THE FIXTURE FIRST. A collection that already held a post would
    // make every assertion below trivially true.
    const cta = page.locator(tid('collection-empty-upload'));
    await expect(cta, 'the fixture collection must start empty').toBeVisible();
    await expect(page.locator(tid('collection-posts'))).toHaveCount(0);

    const before = await markDocument(page);
    await cta.click();
    await pickAndPublish(page, fixtureFiles(1, 'headline'));

    // THE assertion. The wall exists, the empty state is gone, and the
    // post the server just made is on it.
    await expect(
      page.locator(tid('collection-posts')),
      'the post was created and the page never showed it (#1407)',
    ).toBeVisible({ timeout: 30_000 });
    await expect(cta).toHaveCount(0);
    expect(postIds, 'exactly one post should have been created').toHaveLength(1);
    await expect(
      page.locator(`a[href="/posts/${postIds[0]}"]`).first(),
    ).toBeAttached();

    await expectSameDocument(page, before, 'the collection wall appeared');
    await expect(page.getByRole('dialog')).toBeHidden();
  });

  // ── 2. N >= 2 ──────────────────────────────────────────────────────
  test('three files publish three posts, each on the wall exactly once', async ({ page }) => {
    const postIds = watchCreatedPostIds(page);
    await page.goto(`/collections/${collectionId}`);
    const cta = page.locator(tid('collection-empty-upload'));
    await expect(cta).toBeVisible();

    const before = await markDocument(page);
    await cta.click();
    await pickAndPublish(page, fixtureFiles(3, 'batch'), { mode: 'one-per-file' });

    await expect(page.locator(tid('collection-posts'))).toBeVisible({ timeout: 45_000 });
    // The batch is whole: not two of three, and not the first one only.
    await expect
      .poll(() => postIds.length, { timeout: 30_000 })
      .toBe(3);

    const wall = page.locator(tid('collection-posts'));
    for (const id of postIds) {
      // The card's stretched navigation link is one per tile, so this
      // count IS the tile count for that post. Two would be the
      // duplicate a client-side insert plus a refetch produces.
      await expect(
        wall.locator(`a[data-marquee-passthrough][href="/posts/${id}"]`),
        `post ${id} must appear exactly once`,
      ).toHaveCount(1);
    }
    expect(new Set(postIds).size, 'the ids must be three distinct posts').toBe(3);

    await expectSameDocument(page, before, 'the batch appeared');
  });

  // ── 3. 390px ───────────────────────────────────────────────────────
  test('390px: the collection empty state fills in without a reload', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const postIds = watchCreatedPostIds(page);
    await page.goto(`/collections/${collectionId}`);
    const cta = page.locator(tid('collection-empty-upload'));
    await expect(cta).toBeVisible();

    const before = await markDocument(page);
    await cta.click();
    await pickAndPublish(page, fixtureFiles(1, 'mobile'));

    await expect(page.locator(tid('collection-posts'))).toBeVisible({ timeout: 30_000 });
    expect(postIds).toHaveLength(1);
    await expect(page.locator(`a[href="/posts/${postIds[0]}"]`).first()).toBeAttached();
    await expectSameDocument(page, before, 'the collection wall appeared at 390px');
  });

  // ── 4. a refusal is not a success ──────────────────────────────────
  test('a refused publish leaves the surface saying exactly what it said', async ({ page }) => {
    await page.goto(`/collections/${collectionId}`);
    const cta = page.locator(tid('collection-empty-upload'));
    await expect(cta).toBeVisible();

    // Fail the post CREATE only. The collection's own refetch is
    // `GET /collections/{id}/posts` and is deliberately left alone, so
    // a surface that refreshed regardless would still be caught.
    await page.route('**/api/v1/posts', async (route) => {
      if (route.request().method().toUpperCase() === 'POST') {
        await route.fulfill({
          status: 500,
          contentType: 'application/json',
          body: JSON.stringify({ error: '#1407 fixture refusal' }),
        });
        return;
      }
      await route.continue();
    });

    const before = await markDocument(page);
    await cta.click();
    await pickAndPublish(page, fixtureFiles(1, 'refused'));

    // The modal stays, with the refusal on it.
    await expect(
      page.locator(tid('upload-compose-error')),
      'a refused publish must report itself',
    ).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole('dialog')).toBeVisible();

    // And the page behind it is untouched: still empty, still no wall.
    await expect(
      cta,
      'a refusal must not refresh the surface into looking like a success',
    ).toBeVisible();
    await expect(page.locator(tid('collection-posts'))).toHaveCount(0);
    await expectSameDocument(page, before, 'the refusal changed nothing');
  });

  // ── 5. two publishes, no doubling ──────────────────────────────────
  test('a second publish does not double the first one', async ({ page }) => {
    const postIds = watchCreatedPostIds(page);
    await page.goto(`/collections/${collectionId}`);
    const cta = page.locator(tid('collection-empty-upload'));
    await expect(cta).toBeVisible();

    const before = await markDocument(page);
    await cta.click();
    await pickAndPublish(page, fixtureFiles(1, 'first'));
    await expect(page.locator(tid('collection-posts'))).toBeVisible({ timeout: 30_000 });
    await expect.poll(() => postIds.length, { timeout: 30_000 }).toBe(1);

    // Second publish, from the header control this time. The empty
    // state is gone, so the CTA that opened the first one no longer
    // exists. Same store, same seam.
    await page.locator(tid('nav-upload-button')).click();
    await pickAndPublish(page, fixtureFiles(1, 'second'));
    await expect.poll(() => postIds.length, { timeout: 45_000 }).toBe(2);

    const wall = page.locator(tid('collection-posts'));
    for (const id of postIds) {
      await expect(
        wall.locator(`a[data-marquee-passthrough][href="/posts/${id}"]`),
        `post ${id} must still appear exactly once after the second refresh`,
      ).toHaveCount(1);
    }
    await expectSameDocument(page, before, 'two publishes landed');
  });

  // ── 6. asset only ──────────────────────────────────────────────────
  test('an asset with no post reaches the uploads grid without a reload', async ({
    page,
    request,
  }) => {
    const me = await request.get('/api/v1/auth/me');
    expect(me.status(), 'the suite signs in as the bootstrap admin').toBe(200);
    const username = ((await me.json()) as { username: string }).username;

    const assetIds = watchCreatedAssetIds(page);
    const postIds = watchCreatedPostIds(page);
    await page.goto(`/users/by-username/${username}`);
    await expect(page.locator(tid('profile-wall'))).toBeVisible({ timeout: 30_000 });

    const before = await markDocument(page);
    await page.locator(tid('nav-upload-button')).click();
    await pickAndPublish(page, fixtureFiles(1, 'assetonly'), { post: false });

    await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });
    await expect.poll(() => assetIds.length, { timeout: 30_000 }).toBe(1);
    expect(postIds, 'the asset-only flow must not compose a post').toHaveLength(0);

    await expect(
      page.locator(tid('profile-uploads')).locator(`a[href="/assets/${assetIds[0]}"]`).first(),
      'the upload was made and the profile never showed it (#1407)',
    ).toBeAttached({ timeout: 30_000 });
    await expectSameDocument(page, before, 'the uploads grid refreshed');
  });

  // ── 7. the feed ────────────────────────────────────────────────────
  test('the browse feed shows a post published from it without a reload', async ({ page }) => {
    const postIds = watchCreatedPostIds(page);
    await page.goto('/');
    // The wall has to have finished its first page, or "the post is
    // there" would be indistinguishable from the initial load arriving.
    await expect(page.locator('a[data-marquee-passthrough]').first()).toBeAttached({
      timeout: 30_000,
    });

    const before = await markDocument(page);
    await page.locator(tid('nav-upload-button')).click();
    await pickAndPublish(page, fixtureFiles(1, 'feed'));

    await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });
    await expect.poll(() => postIds.length, { timeout: 30_000 }).toBe(1);
    await expect(
      page.locator(`a[data-marquee-passthrough][href="/posts/${postIds[0]}"]`),
      'the feed never showed the post published from it (#1407)',
    ).toHaveCount(1, { timeout: 30_000 });
    await expectSameDocument(page, before, 'the feed refreshed its head');
  });

  // ── 8. /create is a different flow, and stays one ──────────────────
  test('/create still navigates to the post it made', async ({ page }) => {
    const postIds = watchCreatedPostIds(page);
    await page.goto('/create');
    await expect(page.locator(tid('create-page'))).toBeVisible();

    await page.locator(tid('create-file-input')).setInputFiles(fixtureFiles(1, 'create'));
    await expect(page.locator(tid('create-file-row'))).toHaveCount(1);
    await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 60_000 });
    await page.locator(tid('create-publish')).click();

    // The full-page flow LEAVES, and that is the behaviour #1407 must
    // not turn into a refresh: a create surface that dropped you back
    // on an empty form threw away the only thing you wanted from it.
    await expect.poll(() => postIds.length, { timeout: 45_000 }).toBe(1);
    await expect(page).toHaveURL(new RegExp(`/posts/${postIds[0]}$`), { timeout: 30_000 });
  });
});

// ---------------------------------------------------------------------------
// #1407 on `/search`
// ---------------------------------------------------------------------------
//
// The result page is reachable through the same door as every other
// surface: the modal is mounted once in the layout and `NavUploadButton`
// is in the navbar on `/search` too. So the bug reproduces there, and it
// reproduces in its plainest form, because a result page states an
// answer and then goes on stating it after the answer has changed.
//
// ⚠️ WHY THIS ONE IS NOT JUST ANOTHER LIST
//
// `/search` cannot use its ordinary refresh. A non-append `runSearch`
// carries ADR 0056 3c's refine reset, replaces `hits`, and rewrites
// `cursor` from a page-one response. Firing it from a background signal
// would send a reader who is halfway down a result list back to its
// first row, which is the destructive half of this bug rather than a
// fix for it. So the refresh is a THIRD mode, and these two cases pin
// both halves: the answer becomes current, and the reader keeps their
// place while it happens.
//
// ⛔ "another search request was sent" is not asserted anywhere here,
// because a request proves nothing about what the reader can see.

/** One asset, straight through the API, titled so a query can find it. */
async function makeSearchableAsset(
  request: APIRequestContext,
  title: string,
): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    // Novel bytes: storage is content addressed, so a fixed body would
    // dedupe and hand back an EXISTING asset some other spec owns.
    data: Buffer.from(`#1407 search fixture ${Date.now()}-${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), 'fixture upload').toBe(201);
  const { hash } = (await up.json()) as { hash: string };
  const created = await request.post('/api/v1/assets', {
    data: { title, asset_type: 2, file_hash: hash, file_extension: 'txt' },
  });
  expect(created.status(), `fixture asset -> ${created.status()}`).toBe(201);
  return ((await created.json()) as { id: string }).id;
}

/** `Showing {n} of {total} results`, as the two numbers. */
async function readCounter(page: Page): Promise<{ shown: number; total: number }> {
  const text = (await page.locator(tid('search-total-count')).innerText()).replace(/,/g, '');
  const m = text.match(/(\d+)\D+(\d+)/);
  if (!m) throw new Error(`could not read the result counter from ${JSON.stringify(text)}`);
  return { shown: Number(m[1]), total: Number(m[2]) };
}

/** Every asset card on the result wall, as ids, in render order.
 *
 *  The card's stretched navigation link is one per tile, so this is the
 *  tile list rather than a link list. */
async function resultAssetIds(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll('a[data-marquee-passthrough][href^="/assets/"]')]
      .map((a) => (a.getAttribute('href') ?? '').slice('/assets/'.length))
      .filter(Boolean),
  );
}

/** The scrollport the results live in. This app never scrolls the
 *  window (`scrollport.ts`), so `window.scrollY` would read 0 forever
 *  and the whole preservation case would be vacuous. */
async function resultsScrollTop(page: Page): Promise<number> {
  return page.evaluate(() => document.querySelector('main')?.scrollTop ?? -1);
}

/**
 * Put the reader somewhere that is not the top AND leave the navbar
 * reachable.
 *
 * ⚠️ THE TWO ARE IN TENSION, AND THAT IS THE WHOLE REASON THIS EXISTS.
 * The header auto-hides on the way down and comes back on the way up
 * (`chromeScroll.svelte.ts`: past `HIDE_AFTER`, direction decided with
 * a `DIRECTION_EPSILON` of 6). A case that scrolls the results and then
 * reaches for `nav-upload-button` is asking for a control the app has
 * deliberately slid off screen, and it fails as "element is outside of
 * the viewport" only once the transition has actually landed, which is
 * why it survives a quiet run and rots under a loaded one.
 *
 * So this scrolls the way a reader does: down with real wheel events,
 * then a short way back up, which is the gesture that brings the
 * chrome back. Then it WAITS for the button to be in the viewport
 * rather than assuming the transition finished, and asserts the offset
 * it leaves behind is genuinely non-zero.
 */
async function scrollResultsAndKeepNavbar(page: Page): Promise<number> {
  await page.mouse.move(200, 400);
  await page.mouse.wheel(0, 900);
  await page.waitForTimeout(300);
  await revealNavbar(page);
  return resultsScrollTop(page);
}

/**
 * Bring the auto-hiding header back and WAIT for it, without returning
 * the reader to the top.
 *
 * Anything that scrolls the page hides it, and Playwright's own
 * `scrollIntoViewIfNeeded` counts: clicking a control at the bottom of
 * a list scrolls down to reach it, and the navbar goes with it. A short
 * upward wheel is the gesture that reveals it (`chromeScroll`: past
 * `DIRECTION_EPSILON`, which is 6), and the poll is what makes this
 * deterministic instead of a bet on the transition being finished.
 */
async function revealNavbar(page: Page): Promise<void> {
  await page.mouse.move(200, 400);
  await page.mouse.wheel(0, -160);
  await expect
    .poll(
      async () =>
        page.evaluate(() => {
          const b = document.querySelector('[data-testid="nav-upload-button"]');
          if (!b) return -1;
          return Math.round(b.getBoundingClientRect().top);
        }),
      {
        message: 'the auto-hiding header has to be back before the modal can be opened',
        timeout: 15_000,
      },
    )
    .toBeGreaterThanOrEqual(0);
}

test.describe('#1407 a publish reaches the result page it was made from', () => {
  /** Assets this file made through the API. Deleted in afterEach. */
  let apiAssets: string[] = [];

  test.beforeEach(async ({ page }) => {
    uploaded.watch(page);
    apiAssets = [];
  });

  test.afterEach(async ({ request }) => {
    await uploaded.cleanup(request);
    for (const id of apiAssets) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
    apiAssets = [];
  });

  // ── 9. the headline search case ────────────────────────────────────
  test('a matching publish becomes visible on the results without a reload', async ({
    page,
  }) => {
    // A token this instance cannot already hold. Corpus coincidence is
    // the way a search assertion passes for the wrong reason, and the
    // dev corpus is deep where CI's is fresh, so the query has to name
    // something only this run creates.
    const token = `aa1407srch${STAMP.replace(/-/g, '')}`;
    const assetIds = watchCreatedAssetIds(page);

    await page.goto(`/search?q=${token}`);

    // ⚠️ THE PRECONDITION, POSITIVELY. Not "the selector is absent",
    // which a query that never ran would also satisfy: the no-matches
    // line is rendered only once a search has actually returned nothing.
    await expect(
      page.locator(tid('search-no-matches')),
      'the token must name nothing before the upload, or this case proves nothing',
    ).toBeVisible({ timeout: 30_000 });

    const before = await markDocument(page);
    await page.locator(tid('nav-upload-button')).click();
    // The filename IS the searchable representation: the store derives
    // the asset title from it, and `search_text` is rebuilt from the
    // title by a trigger on the write, so the row is findable by the
    // time the create response comes back.
    await pickAndPublish(page, [
      {
        name: `${token}.txt`,
        mimeType: 'text/plain',
        buffer: Buffer.from(`#1407 search headline ${STAMP}`),
      },
    ]);

    await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });
    await expect.poll(() => assetIds.length, { timeout: 30_000 }).toBe(1);

    // THE assertion: the answer on screen changed, and exactly once.
    await expect(
      page.locator(`a[data-marquee-passthrough][href="/assets/${assetIds[0]}"]`),
      'the upload matched the query and the result page never showed it (#1407)',
    ).toHaveCount(1, { timeout: 30_000 });
    await expect(page.locator(tid('search-no-matches'))).toHaveCount(0);

    await expectSameDocument(page, before, 'the results caught up');
  });

  // ── 10. the reader keeps their place ───────────────────────────────
  test('the refresh does not send a reader back to the first result', async ({
    page,
    request,
  }) => {
    const token = `aa1407scroll${STAMP.replace(/-/g, '')}`;
    const assetIds = watchCreatedAssetIds(page);

    // ⚠️ MANUFACTURED, NOT BORROWED. A result list shorter than the
    // viewport has a scroll range of exactly 0, and the corpus this
    // runs against is not a constant: CI's is fresh and shallow where
    // the dev one is deep. So the rows this case needs are made here.
    // 30 is one page (25) plus a tail, so there is genuinely something
    // below the fold AND something the merge has to preserve.
    const SEEDED = 30;
    for (let i = 0; i < SEEDED; i++) {
      apiAssets.push(
        await makeSearchableAsset(request, `${token} ${String(i).padStart(2, '0')}`),
      );
    }

    // Narrow and short, so the wall is many rows tall whatever the
    // reader's tile size is. The width is the mobile control's, and the
    // behaviour under test is not width dependent.
    await page.setViewportSize({ width: 390, height: 700 });
    await page.goto(`/search?q=${token}`);
    await expect(page.locator(tid('search-total-count'))).toBeVisible({ timeout: 30_000 });

    const counterBefore = await readCounter(page);
    expect(
      counterBefore.total,
      'the seeded rows must be the whole population this query names',
    ).toBe(SEEDED);
    const idsBefore = await resultAssetIds(page);
    expect(idsBefore.length, 'page one should be the full first page').toBeGreaterThan(0);

    // Put the reader somewhere that is not the top, and PROVE they are
    // there. Without this the "it did not reset" assertion is satisfied
    // by an offset that was already 0.
    const offsetBefore = await scrollResultsAndKeepNavbar(page);
    expect(
      offsetBefore,
      'the results region must genuinely scroll, or this case cannot observe a reset',
    ).toBeGreaterThan(0);

    const before = await markDocument(page);
    await page.locator(tid('nav-upload-button')).click();
    await pickAndPublish(page, [
      {
        name: `${token}-31.txt`,
        mimeType: 'text/plain',
        buffer: Buffer.from(`#1407 search scroll ${STAMP}`),
      },
    ]);
    await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });
    await expect.poll(() => assetIds.length, { timeout: 30_000 }).toBe(1);

    // The answer is server authoritative again. Asserted on the COUNT
    // rather than on the new row's presence, because where a hit ranks
    // is the engine's business and this case is not about ranking: the
    // population the query names grew by one, and the page says so
    // without being reloaded.
    await expect
      .poll(async () => (await readCounter(page)).total, { timeout: 30_000 })
      .toBe(SEEDED + 1);

    // ⛔ AND THE READER DID NOT MOVE. A refine writes `scrollTop = 0`
    // exactly, so 0 is the falsifying observation.
    const offsetAfter = await resultsScrollTop(page);
    expect(
      offsetAfter,
      `the refresh reset the results to the top: ${offsetBefore} -> ${offsetAfter}`,
    ).toBeGreaterThan(0);

    // Continuity: every hit the reader already had is still there, and
    // still once. A refresh that replaced `hits` with page one would
    // drop the tail; one that concatenated would double the head.
    const idsAfter = await resultAssetIds(page);
    const seen = new Map<string, number>();
    for (const id of idsAfter) seen.set(id, (seen.get(id) ?? 0) + 1);
    expect(
      [...seen.entries()].filter(([, n]) => n > 1),
      'no result may appear twice',
    ).toEqual([]);
    for (const id of idsBefore) {
      expect(seen.get(id), `result ${id} was on screen and must still be, exactly once`).toBe(1);
    }

    await expectSameDocument(page, before, 'the results caught up in place');
  });
});

// ---------------------------------------------------------------------------
// #1407: the refresh must not race a page that is already in flight
// ---------------------------------------------------------------------------
//
// # The race
//
// Every list here owns its own fetch, and any of them can have a
// request outstanding when a publish lands. The first version of this
// work fired the refresh immediately, and the three surfaces resolved
// the collision three different and equally wrong ways:
//
//   - `/search` and `/` supersede by generation. The refresh bumps past
//     the append, the append's response is dropped by its own guard,
//     and the page the reader had already scrolled for never arrives.
//   - `/teams/{id}` has no generation at all. Its append composes from
//     whatever the list holds when the response LANDS, so a refresh
//     arriving in between leaves it re-adding rows the merge kept.
//
// # ⛔ AND THE OBVIOUS FIX IS THE OTHER HALF OF THE BUG
//
// "Skip the refresh while something is in flight" drops the publish,
// and the excuse for it is false: the running request may have read the
// database BEFORE the publish committed, so its response is not
// guaranteed to carry the new content. The artist would be told their
// work is not there, which is #1407 itself.
//
// # ⚠️ WHY THESE HOLD THE RESPONSE
//
// A case that hoped the two requests would overlap would pass or fail
// by network luck, and on a quiet workstation it would simply never
// reproduce. So the page response is INTERCEPTED AND HELD open, the
// publish is completed while it is held, and only then is it released.
// The overlap is arranged, not awaited.
//
// # ⚠️ AND WHY THE FIXTURE IS ASSERTED FIRST
//
// Each case proves the held request was genuinely outstanding before it
// publishes, and proves the rows it is about to check for were not
// there already. A hold that never engaged, or a page that had already
// arrived, would make every assertion below true for the wrong reason.

/**
 * Let the next matching request REACH THE SERVER, then hold its response
 * until `release()` is called.
 *
 * ⚠️ THE ORDER MATTERS AND IT IS THE WHOLE INSTRUMENT. Holding before
 * `continue()` would keep the request in the browser, so the server
 * would not run the query until after the publish committed and every
 * held response would come back already containing the new content.
 * That is the one situation these cases must NOT arrange, because it is
 * the false premise the broken fix rests on: "the request already in
 * flight will carry it". `route.fetch()` performs the request now and
 * `route.fulfill()` delivers it later, so the response held here was
 * genuinely computed BEFORE the publish and genuinely cannot contain it.
 *
 * Returns the number of requests it has caught, so a case can assert
 * the request it is racing genuinely went out.
 */
function holdNextResponse(
  page: Page,
  match: (url: URL, method: string) => boolean,
  opts: { blockFurther?: boolean } = {},
): {
  held: () => number;
  /** The rows the held response actually carried, as `type:id`. */
  heldRows: () => string[];
  release: () => Promise<void>;
  stop: () => Promise<void>;
} {
  let caught = 0;
  let heldRows: string[] = [];
  let releaseNow: (() => void) | null = null;
  const gate = new Promise<void>((resolve) => {
    releaseNow = resolve;
  });
  let armed = true;

  void page.route('**/api/v1/**', async (route) => {
    let url: URL;
    try {
      url = new URL(route.request().url());
    } catch {
      await route.continue();
      return;
    }
    if (!match(url, route.request().method().toUpperCase())) {
      await route.continue();
      return;
    }
    if (!armed) {
      // ⛔ NO SECOND CHANCE, and this is what makes the defect
      // OBSERVABLE rather than merely likely. Discarding the reader's
      // page is SELF HEALING on the broken code: the cursor was never
      // advanced, so the paging pump can quietly ask for the same page
      // again and the rows turn up after all, depending on where the
      // sentinel happens to land. That made the red a coin flip. With
      // the retry blocked, the only way those rows can be on screen is
      // if the response this case held was actually used.
      //
      // It costs the correct implementation nothing: it needs no retry,
      // so there is nothing here for it to trip over.
      if (opts.blockFurther) {
        caught += 1;
        await route.abort().catch(() => undefined);
        return;
      }
      await route.continue();
      return;
    }
    // One request only. Everything after it, including the refresh this
    // case is about, must be allowed straight through.
    armed = false;
    caught += 1;
    let response;
    let body: Buffer;
    try {
      response = await route.fetch();
      body = await response.body();
    } catch {
      await route.continue().catch(() => undefined);
      return;
    }
    // ⭐ WHAT THIS PAGE ACTUALLY CARRIES, read before it is delivered.
    // A case can then name the exact rows that must survive, instead of
    // asserting the list got longer: a head refresh alone lengthens it
    // by one, so a length check passes while the reader's page is still
    // in the bin.
    try {
      const parsed = JSON.parse(body.toString('utf8')) as {
        hits?: Array<{ type: string; id: string }>;
        items?: Array<{ id: string }>;
      };
      if (parsed.hits) heldRows = parsed.hits.map((h) => `${h.type}:${h.id}`);
      // The feed's `/posts` payload has no `type`, because everything in
      // it is a post. Keyed the same way regardless.
      else if (parsed.items) heldRows = parsed.items.map((i) => `post:${i.id}`);
    } catch {
      heldRows = [];
    }
    await gate;
    await route.fulfill({ response, body }).catch(() => undefined);
  });

  return {
    held: () => caught,
    heldRows: () => [...heldRows],
    release: async () => {
      releaseNow?.();
      // Let the released response land and the gate drain.
      await page.waitForTimeout(1500);
    },
    stop: async () => {
      releaseNow?.();
      await page.unroute('**/api/v1/**').catch(() => undefined);
    },
  };
}

/**
 * Count every matching request the page makes.
 *
 * ⭐ Not "a request was sent", which proves nothing about what the
 * reader sees. This counts whether the SAME page had to be asked for
 * TWICE, which is what happens when the first one is thrown away: the
 * requirement is that an in-flight operation is preserved rather than
 * cancelled, and a redundant re-fetch is the signature of cancelling it.
 */
function countRequests(page: Page, match: (url: URL) => boolean): () => number {
  let n = 0;
  page.on('request', (req) => {
    if (req.method().toUpperCase() !== 'GET') return;
    try {
      if (match(new URL(req.url()))) n += 1;
    } catch {
      // not a url we care about
    }
  });
  return () => n;
}

/** Ids of every result card on `/search`, keyed the way the app keys them. */
async function resultIdentities(page: Page): Promise<string[]> {
  // ⚠️ THE ENGINE'S OWN KEY, `type:id`, and singular because that is
  // what the search API calls a hit's type. The permalinks are plural
  // (`/assets/`, `/posts/`), so the trailing `s` is dropped here rather
  // than left to a caller to remember: the first version of this
  // compared `assets:x` against the API's `asset:x` and could never
  // match, which made a case go red against a correct implementation.
  return page.evaluate(() =>
    [...document.querySelectorAll('a[data-marquee-passthrough]')]
      .map((a) => a.getAttribute('href') ?? '')
      .filter((h) => h.startsWith('/assets/') || h.startsWith('/posts/'))
      .map((h) => h.replace(/^\/(asset|post)s\//, (_m, t: string) => `${t}:`)),
  );
}

function duplicatesIn(ids: string[]): string[] {
  const seen = new Map<string, number>();
  for (const id of ids) seen.set(id, (seen.get(id) ?? 0) + 1);
  return [...seen.entries()].filter(([, n]) => n > 1).map(([id]) => id);
}

test.describe('#1407 a publish does not race a page that is already loading', () => {
  let apiAssets: string[] = [];

  test.beforeEach(async ({ page }) => {
    uploaded.watch(page);
    apiAssets = [];
  });

  test.afterEach(async ({ request }) => {
    await uploaded.cleanup(request);
    for (const id of apiAssets) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
    apiAssets = [];
  });

  // ── 11. search: a held page two, and a publish on top of it ────────
  test('search keeps the page it was already fetching', async ({ page, request }) => {
    const token = `aa1407hold${STAMP.replace(/-/g, '')}`;
    const assetIds = watchCreatedAssetIds(page);

    // Two pages worth. The result limit is 25, so 40 rows guarantees a
    // second page exists and that its rows are distinguishable from the
    // first page's.
    const SEEDED = 40;
    for (let i = 0; i < SEEDED; i++) {
      apiAssets.push(
        await makeSearchableAsset(request, `${token} ${String(i).padStart(2, '0')}`),
      );
    }

    // ⚠️ Arm the hold BEFORE the page loads, and let page one through:
    // the request this case races is the CURSORED one.
    const cursoredRequests = countRequests(
      page,
      (url) => url.pathname === '/api/v1/search' && url.searchParams.has('cursor'),
    );
    const hold = holdNextResponse(
      page,
      (url, method) =>
        method === 'GET' && url.pathname === '/api/v1/search' && url.searchParams.has('cursor'),
      { blockFurther: true },
    );

    await page.setViewportSize({ width: 390, height: 700 });
    await page.goto(`/search?q=${token}`);
    await expect(page.locator(tid('search-total-count'))).toBeVisible({ timeout: 30_000 });

    const counterBefore = await readCounter(page);
    expect(counterBefore.total, 'the seeded rows are the whole population').toBe(SEEDED);
    expect(
      counterBefore.shown,
      'page one must not already be the whole answer, or there is no second page to hold',
    ).toBeLessThan(SEEDED);
    const idsBefore = await resultIdentities(page);

    // Scroll into the pump's reach, keeping the navbar available. This
    // is what fires the cursored request the hold is waiting for.
    const offsetBefore = await scrollResultsAndKeepNavbar(page);
    expect(offsetBefore, 'the reader must genuinely be off the top').toBeGreaterThan(0);

    // ⚠️ THE PRECONDITION THAT MAKES THIS A CONCURRENCY CASE AT ALL.
    // If no cursored request went out, nothing is being raced and every
    // assertion below would hold on any implementation.
    await expect
      .poll(() => hold.held(), {
        message: 'page two must be genuinely in flight before the publish',
        timeout: 20_000,
      })
      .toBe(1);
    await expect(page.locator(tid('search-loading-more'))).toBeVisible();

    // Publish WHILE page two is held open.
    const before = await markDocument(page);
    await page.locator(tid('nav-upload-button')).click();
    await pickAndPublish(page, [
      {
        name: `${token}-held.txt`,
        mimeType: 'text/plain',
        buffer: Buffer.from(`#1407 held page ${STAMP}`),
      },
    ]);
    await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });
    await expect.poll(() => assetIds.length, { timeout: 30_000 }).toBe(1);

    await hold.release();

    // 1. The publish was NOT dropped: the answer is current.
    await expect
      .poll(async () => (await readCounter(page)).total, {
        message: 'the publish must not be lost merely because a page was loading',
        timeout: 30_000,
      })
      .toBe(SEEDED + 1);

    // 2. The held page was NOT discarded, and nothing doubled.
    //
    // ⚠️ NAMED ROWS, NOT A LENGTH. The head refresh on its own makes
    // the list one longer, because the new row joins page one and
    // pushes its last row into the tail. A length check is satisfied by
    // that while the reader's page is still in the bin, which is how
    // this case passed on the defect before it was written this way.
    const heldRows = hold.heldRows();
    expect(
      heldRows.length,
      'the held response must genuinely have carried rows, or this proves nothing',
    ).toBeGreaterThan(0);
    for (const row of heldRows) {
      await expect
        .poll(async () => (await resultIdentities(page)).filter((x) => x === row).length, {
          message: `${row} was on the page the reader had already asked for and must arrive, once`,
          timeout: 30_000,
        })
        .toBe(1);
    }
    const idsAfter = await resultIdentities(page);
    expect(idsAfter.length, 'and the list grew by that page').toBeGreaterThan(idsBefore.length);
    expect(duplicatesIn(idsAfter), 'no (type, id) may appear twice').toEqual([]);
    for (const id of idsBefore) {
      expect(
        idsAfter.filter((x) => x === id).length,
        `${id} was on screen and must still be, exactly once`,
      ).toBe(1);
    }

    // ⭐ AND THE READER'S PAGE WAS PRESERVED, NOT RE-FETCHED. This and
    // the assertion above close both ways the defect can show: either
    // the discarded page never arrives (that one fails) or the paging
    // pump quietly asks for it a SECOND time (this one fails). A fix
    // that cancels the in-flight request cannot satisfy both.
    expect(
      cursoredRequests(),
      'the page the reader had already asked for must be USED, not thrown away and re-requested',
    ).toBe(1);

    // 3. Address, document and scroll are all where they were.
    expect(await resultsScrollTop(page), 'the reader must not be sent to the top').toBeGreaterThan(0);
    await expectSameDocument(page, before, 'the held page and the refresh both landed');

    // 4. Paging is not corrupted.
    //
    // ⚠️ NOT "another page must arrive". Page two was fetched while the
    // population was still 40 and returned its last 15, so it was
    // TERMINAL and the engine handed back no cursor. There is nothing
    // further to ask for, and demanding it would be asserting against
    // the fixture rather than against the code. What a corrupted cursor
    // would do instead is re-deliver rows, so that is what is asserted:
    // pump again and nothing may double or disappear.
    await hold.stop();
    const beforeMore = (await resultIdentities(page)).length;
    await page.evaluate(() => {
      const port = document.querySelector('main');
      if (port) port.scrollTop = port.scrollHeight;
    });
    await page.waitForTimeout(2500);
    const idsFinal = await resultIdentities(page);
    expect(duplicatesIn(idsFinal), 'a further pump must not duplicate anything').toEqual([]);
    expect(idsFinal.length, 'the list may only grow').toBeGreaterThanOrEqual(beforeMore);
  });

  // ── 12. search: a FRESH query in flight, which must not drop it ─────
  test('search queues the publish behind a query that changes the address', async ({
    page,
    request,
  }) => {
    // Two distinct populations. The reader is looking at A, submits B,
    // and the publish lands into B while B's request is held.
    const tokenA = `aa1407qa${STAMP.replace(/-/g, '')}`;
    const tokenB = `aa1407qb${STAMP.replace(/-/g, '')}`;
    const assetIds = watchCreatedAssetIds(page);
    for (let i = 0; i < 3; i++) {
      apiAssets.push(await makeSearchableAsset(request, `${tokenA} ${i}`));
      apiAssets.push(await makeSearchableAsset(request, `${tokenB} ${i}`));
    }

    await page.goto(`/search?q=${tokenA}`);
    await expect(page.locator(tid('search-total-count'))).toBeVisible({ timeout: 30_000 });
    expect((await readCounter(page)).total).toBe(3);

    // Hold the request for B, the NON-append one.
    const hold = holdNextResponse(
      page,
      (url, method) =>
        method === 'GET' &&
        url.pathname === '/api/v1/search' &&
        (url.searchParams.get('q') ?? '') === tokenB,
    );

    const before = await markDocument(page);
    await page.locator(tid('search-input')).fill(tokenB);
    await page.locator(tid('search-input')).press('Enter');
    await expect
      .poll(() => hold.held(), {
        message: 'the new query must be genuinely in flight',
        timeout: 20_000,
      })
      .toBe(1);

    // Publish a row that belongs to B, while B is still on the wire.
    await page.locator(tid('nav-upload-button')).click();
    await pickAndPublish(page, [
      {
        name: `${tokenB}-during.txt`,
        mimeType: 'text/plain',
        buffer: Buffer.from(`#1407 query in flight ${STAMP}`),
      },
    ]);
    await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });
    await expect.poll(() => assetIds.length, { timeout: 30_000 }).toBe(1);

    await hold.release();

    // ⭐ THE POINT. The publish was queued, not dropped, AND the
    // refresh it produced applied to B, the set actually on screen,
    // rather than to A, which is what a gate holding parameters from
    // queue time would have refreshed.
    await expect
      .poll(async () => (await readCounter(page)).total, {
        message: 'the queued publish must reach the query that is now on screen',
        timeout: 30_000,
      })
      .toBe(4);
    await expect(
      page.locator(`a[data-marquee-passthrough][href="/assets/${assetIds[0]}"]`),
    ).toHaveCount(1, { timeout: 30_000 });
    expect(duplicatesIn(await resultIdentities(page))).toEqual([]);
    expect(page.url(), 'the address is B').toContain(tokenB);
    // ⚠️ The ADDRESS moved on purpose here, so only the reload half of
    // the usual check applies. `before` is kept for the marker it
    // stamped.
    void before;
    await expectNoReload(page, 'the queued refresh applied to the new address');
    await hold.stop();
  });

  // ── 13. the browse feed, with page two held ────────────────────────
  test('the feed keeps the page it was already fetching', async ({ page }) => {
    const postIds = watchCreatedPostIds(page);

    const hold = holdNextResponse(
      page,
      (url, method) =>
        method === 'GET' && url.pathname === '/api/v1/posts' && url.searchParams.has('cursor'),
      { blockFurther: true },
    );

    await page.setViewportSize({ width: 390, height: 700 });
    await page.goto('/');
    await expect(page.locator('a[data-marquee-passthrough]').first()).toBeAttached({
      timeout: 30_000,
    });
    const idsBefore = await resultIdentities(page);
    expect(idsBefore.length, 'the wall must have a first page').toBeGreaterThan(0);

    const offsetBefore = await scrollResultsAndKeepNavbar(page);
    expect(offsetBefore).toBeGreaterThan(0);

    await expect
      .poll(() => hold.held(), {
        message: 'the next feed page must be genuinely in flight before the publish',
        timeout: 20_000,
      })
      .toBe(1);

    const before = await markDocument(page);
    await page.locator(tid('nav-upload-button')).click();
    await pickAndPublish(page, fixtureFiles(1, 'feedhold'));
    await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });
    await expect.poll(() => postIds.length, { timeout: 30_000 }).toBe(1);

    await hold.release();

    // The publish arrived, in the SERVER's position rather than one the
    // client chose: it is on page one of a newest-first wall, so it is
    // at the head of the merged list.
    await expect(
      page.locator(`a[data-marquee-passthrough][href="/posts/${postIds[0]}"]`),
      'the publish must not be lost merely because a page was loading',
    ).toHaveCount(1, { timeout: 30_000 });
    const idsAfter = await resultIdentities(page);
    expect(idsAfter[0], 'newest first, so the new post leads the wall').toBe(
      `post:${postIds[0]}`,
    );

    // The held page was not discarded, and nothing doubled. Named rows
    // rather than a length, for the reason the search case gives.
    const heldRows = hold.heldRows();
    expect(
      heldRows.length,
      'the held response must genuinely have carried rows',
    ).toBeGreaterThan(0);
    for (const row of heldRows) {
      expect(
        idsAfter.filter((x) => x === row).length,
        `${row} was on the page the reader had already asked for and must arrive, once`,
      ).toBe(1);
    }
    expect(
      idsAfter.length,
      'the page the reader had already asked for must arrive',
    ).toBeGreaterThan(idsBefore.length);
    expect(duplicatesIn(idsAfter), 'no post may appear twice').toEqual([]);
    for (const id of idsBefore) {
      expect(idsAfter.filter((x) => x === id).length, `${id} exactly once`).toBe(1);
    }

    expect(await resultsScrollTop(page), 'the wall must not reset').toBeGreaterThan(0);
    await expectSameDocument(page, before, 'the held page and the refresh both landed');

    // The cursor still works.
    await hold.stop();
    const beforeMore = idsAfter.length;
    await page.evaluate(() => {
      const port = document.querySelector('main');
      if (port) port.scrollTop = port.scrollHeight;
    });
    await expect
      .poll(async () => (await resultIdentities(page)).length, {
        message: 'the cursor must still be usable after the refresh',
        timeout: 30_000,
      })
      .toBeGreaterThan(beforeMore);
    expect(duplicatesIn(await resultIdentities(page))).toEqual([]);
  });

  // ── 14. the studio page, whose coordination is its own ─────────────
  //
  // `/teams/{id}` does not use the generation machinery the feed and the
  // results page share, so it gets its own case rather than being
  // assumed covered. Its defect is a different shape: ONE busy flag for
  // TWO loaders. With both lists live, a publish starts a posts head and
  // an assets head together, and whichever finished first wrote "idle"
  // while the other was still running, on a page where that flag is what
  // disables load-more and what any refresh has to consult.
  //
  // ⭐ WHAT THIS MEASURES IS WHEN, NOT WHETHER. Holding the assets load
  // open and counting the studio's POSTS requests says exactly one
  // thing: did the refresh start on top of a request that was already in
  // flight? On the defect the posts head goes out immediately, before
  // the release. That is not "a request was sent" (which proves nothing
  // about the reader); it is a request landing on the wrong side of a
  // response this case is holding.
  //
  // ⚠️ AND THE FIXTURE IS DELIBERATELY TINY. An earlier version made 41
  // posts so the studio would page, and they sat at the head of the
  // newest-first wall for the length of the test: three unrelated specs
  // went red reading a feed this one had taken over (#1198's lesson, and
  // create-page-1119 writes it down). Nothing here needs a second page,
  // so nothing here makes one.
  //
  // ⚠️ ONE CONTRIVANCE, STATED. Nothing on this page opens the modal
  // with the studio's id (`NavUploadButton` scopes to a collection and
  // nothing else), so a modal publish cannot land IN the team today. The
  // row the refreshed head has to surface is therefore written directly,
  // before the publish. What is under test is that the head re-asks the
  // server AFTER the in-flight load settles, and that row proves it did.
  test('the studio page does not refresh on top of a load in flight', async ({
    page,
    request,
  }) => {
    const stamp = `${STAMP}-${Math.random().toString(36).slice(2, 6)}`;
    const created = await request.post('/api/v1/teams', {
      data: {
        name: `#1407 studio ${stamp}`,
        slug: `aa1407-studio-${stamp}`.toLowerCase().replace(/[^a-z0-9-]/g, ''),
        description: 'fixture for #1407',
      },
    });
    expect(created.status(), 'fixture team').toBe(201);
    const teamId = ((await created.json()) as { id: string }).id;

    const backing = await makeSearchableAsset(request, `#1407 studio backing ${stamp}`);
    apiAssets.push(backing);
    const teamPosts: string[] = [];
    const makeTeamPost = async (label: string): Promise<string> => {
      const r = await request.post('/api/v1/posts', {
        data: {
          title: `#1407 studio ${stamp} ${label}`,
          members: [{ asset_id: backing, sort_order: 0 }],
          team_id: teamId,
        },
      });
      expect(r.status(), `fixture post ${label}`).toBe(201);
      const id = ((await r.json()) as { id: string }).id;
      teamPosts.push(id);
      return id;
    };
    for (let i = 0; i < 3; i++) await makeTeamPost(String(i));

    try {
      // Count the studio's own posts requests. One is made on mount; the
      // refresh would be a second.
      const postsRequests = countRequests(
        page,
        (url) => url.pathname === '/api/v1/posts' && url.searchParams.has('team_id'),
      );

      // ⚠️ PIN THE VIEW MODE. `resultIdentities` reads the card's
      // stretched link, which LIST mode does not render at all (it draws
      // a table instead), and the mode is a stored preference rather
      // than a constant. This case is about concurrency, so the mode is
      // a variable it should not be carrying.
      await page.addInitScript(() => {
        try {
          localStorage.setItem('aa_browse_mode', 'grid');
        } catch {
          // storage disabled; the default mode renders cards anyway
        }
      });
      await page.goto(`/teams/${teamId}`);
      await expect(page.locator(tid('team-page'))).toBeVisible({ timeout: 30_000 });
      // ⚠️ AND WAIT FOR THE POSTS. `team-page` is the outer container and
      // is visible before `loadPosts` resolves, so reading the cards
      // straight after it counts an empty grid on any runner slower than
      // the one this was written on. CI found that; a fast workstation
      // never would.
      await expect
        .poll(async () => (await resultIdentities(page)).length, {
          message: 'the studio must show its posts first',
          timeout: 30_000,
        })
        .toBe(3);
      const idsBefore = await resultIdentities(page);
      await expect
        .poll(() => postsRequests(), { timeout: 20_000 })
        .toBe(1);

      // Hold the assets tab's load open. This is an ORDINARY page load,
      // not a refresh, so what follows is a refresh meeting a request
      // that was already in flight.
      const hold = holdNextResponse(
        page,
        (url, method) => method === 'GET' && url.pathname === '/api/v1/assets',
      );
      await page.locator(tid('team-tab-assets')).click();
      await expect
        .poll(() => hold.held(), {
          message: 'the assets load must be genuinely in flight',
          timeout: 20_000,
        })
        .toBe(1);

      // The row the refreshed head has to bring back.
      const lateId = await makeTeamPost('late');

      const before = await markDocument(page);
      await revealNavbar(page);
      await page.locator(tid('nav-upload-button')).click();
      await pickAndPublish(page, fixtureFiles(1, 'teamhold'));
      await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });

      // ⭐ THE ASSERTION. An immediate refresh starts both loaders here,
      // so the posts head would already have gone out. The wait is what
      // lets that happen before the check, so a pass cannot be the check
      // simply arriving first.
      await page.waitForTimeout(3000);
      expect(hold.held(), 'the assets load is still held, so nothing may say otherwise').toBe(1);
      expect(
        postsRequests(),
        'a load is still in flight, so the refresh must not have started on top of it',
      ).toBe(1);

      await hold.release();

      // Now it runs, and it re-asks the SERVER: the row written after
      // the page loaded is on screen, once, and nothing is doubled.
      await page.locator(tid('team-tab-posts')).click();
      await expect(
        page.locator(`a[data-marquee-passthrough][href="/posts/${lateId}"]`),
        'the head must re-ask the server once the in-flight load settles',
      ).toHaveCount(1, { timeout: 30_000 });
      const idsAfter = await resultIdentities(page);
      expect(duplicatesIn(idsAfter), 'no post may appear twice').toEqual([]);
      for (const id of idsBefore) {
        expect(idsAfter.filter((x) => x === id).length, `${id} exactly once`).toBe(1);
      }
      expect(idsAfter.length, 'the three it had, plus the one the head found').toBe(4);
      await expectSameDocument(page, before, 'the held load and the refresh both landed');
      await hold.stop();
    } finally {
      for (const id of teamPosts) {
        await request.delete(`/api/v1/posts/${id}`).catch(() => undefined);
      }
      await request.delete(`/api/v1/teams/${teamId}`).catch(() => undefined);
    }
  });
});
