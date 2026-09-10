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

import type { Page, Response } from '@playwright/test';
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
