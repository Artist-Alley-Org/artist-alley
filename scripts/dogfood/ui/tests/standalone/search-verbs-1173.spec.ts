// #1173 sprint 25a: the advanced page's free-text box speaks two verbs.
//
// `!nopreviews` finds files whose preview never rendered and
// `!list<uuid>,<uuid>` pulls an explicit set. Both are typed into the
// existing "Words to look for" input; there is deliberately no new chip
// or control. On the build before this sprint each verb was free text
// and the results page answered an empty 200, which is exactly what the
// first two tests below turn red on.
//
// # The fixture is REAL pipeline output, not seeded rows
//
// The failed asset is a PNG whose bytes are not a PNG: the raster
// handler cannot decode it, marks the row `failed`, and writes no `col`.
// The ready asset is a novel text file the text handler renders a `col`
// for. Both carry a per-run word in the title so a results page can be
// scoped to them, and both are uploaded with NOVEL bytes every run:
// storage is content-addressed, so a fixed body would dedupe onto an
// existing row and prove nothing about preview generation. The ready
// asset's `preview_available` is read back from the API before any test
// runs, so "not missing" is a measured fact about this run's row.
//
// Teardown ownership is registered the moment a row exists and BEFORE
// any readiness wait, so a pipeline that never settles cannot leak the
// row (#1401).
//
// Desktop and 390px both drive the same three flows.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';
import { waitForAssetReady } from '../../helpers/asset-ready';

const RUN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
/** A word no seeded title contains, unique to this run. */
const WORD = `verbfix${RUN}`;
const FREETEXT_LABEL = 'Words to look for (optional)';

const createdAssets: string[] = [];
let failedId = '';
let readyId = '';

async function upload(request: APIRequestContext, body: Buffer, contentType: string): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    data: body,
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': contentType },
  });
  expect(up.status(), `upload → ${up.status()} ${await up.text()}`).toBe(201);
  return ((await up.json()) as { hash: string }).hash;
}

async function createAsset(
  request: APIRequestContext,
  title: string,
  hash: string,
  ext: string,
  assetType: number,
): Promise<string> {
  const r = await request.post('/api/v1/assets', {
    data: { title, asset_type: assetType, file_hash: hash, file_extension: ext },
  });
  expect(r.status(), `create asset → ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  // Ownership BEFORE any wait (#1401).
  createdAssets.push(id);
  return id;
}

/** Poll until the row is `failed`; a `ready` outcome is the wrong fixture. */
async function waitForAssetFailed(request: APIRequestContext, id: string): Promise<void> {
  const deadline = Date.now() + 180_000;
  let last = '';
  for (;;) {
    const r = await request.get(`/api/v1/assets/${id}`);
    if (r.ok()) {
      const row = (await r.json()) as { processing_status: string };
      last = row.processing_status;
      if (last === 'failed') return;
      if (last === 'ready') throw new Error(`fixture ${id} rendered a preview; the broken PNG was decodable`);
    }
    if (Date.now() > deadline) throw new Error(`fixture ${id} never failed (last ${last})`);
    await new Promise((res) => setTimeout(res, 2_000));
  }
}

test.beforeAll(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  const types = (await (await request.get('/api/v1/asset_types')).json()) as { ref: number; name?: string }[];
  const imageRef = types.find((t) => t.name === 'Image')?.ref ?? 1;
  const docRef = types.find((t) => t.name === 'Document')?.ref ?? 2;

  // Novel, undecodable bytes with a raster extension.
  const brokenPng = Buffer.from(`not a png ${WORD} ${Math.random()}`);
  const brokenHash = await upload(request, brokenPng, 'image/png');
  failedId = await createAsset(request, `${WORD} broken preview`, brokenHash, 'png', imageRef);

  const text = Buffer.from(`${WORD} readable text fixture ${Math.random()}\n`);
  const textHash = await upload(request, text, 'text/plain');
  readyId = await createAsset(request, `${WORD} rendered preview`, textHash, 'txt', docRef);

  await waitForAssetFailed(request, failedId);
  const ready = await waitForAssetReady(request, readyId, { label: 's25a ready fixture' });
  expect(
    ready.preview_available,
    'the ready fixture has no servable preview; the text pipeline did not write a `col` for novel bytes',
  ).toBe(true);
});

test.afterAll(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  for (const id of createdAssets) {
    await request.delete(`/api/v1/assets/${id}?hard=true`).catch(() => undefined);
  }
});

async function openAdvanced(page: Page) {
  await page.goto('/search/advanced');
  await expect(page.locator(tid('advanced-search-page'))).toBeVisible();
}

/** Type into the free-text box, submit, and return the `dsl=` the results page received. */
async function submitFreeText(page: Page, text: string): Promise<string> {
  await openAdvanced(page);
  const box = page.getByLabel(FREETEXT_LABEL);
  await expect(box).toBeVisible();
  await box.fill(text);
  await page.locator(tid('advanced-submit')).click();
  await page.waitForURL(/\/search\?/, { timeout: 20_000 });
  return new URL(page.url()).searchParams.get('dsl') ?? '';
}

/** Distinct asset ids linked from the results grid. */
async function renderedAssetIds(page: Page): Promise<string[]> {
  const hrefs = await page.locator('a[href^="/assets/"]').evaluateAll((els) =>
    els.map((e) => (e as HTMLAnchorElement).getAttribute('href') ?? ''),
  );
  return [...new Set(hrefs.map((h) => h.replace(/^\/assets\//, '').split(/[?#]/)[0]))].sort();
}

for (const width of ['desktop', 390] as const) {
  test.describe(`search verbs at ${width}`, () => {
    test.beforeEach(async ({ page }) => {
      if (width === 390) await page.setViewportSize({ width: 390, height: 844 });
    });

    test('!nopreviews alone reaches /search as dsl=!nopreviews and excludes a rendered asset', async ({
      page,
      request,
    }) => {
      const dsl = await submitFreeText(page, '!nopreviews');
      expect(dsl, 'the results page did not receive the verb as the DSL').toBe('!nopreviews');
      // The page answered rather than erred, and the ready fixture is not
      // among what it rendered.
      await expect(page.locator(tid('search-total-count'))).toBeVisible({ timeout: 20_000 });
      await expect(page.locator('[role="alert"]')).toHaveCount(0);
      expect(await renderedAssetIds(page)).not.toContain(readyId);
      // Membership without paging: the verb intersected with an explicit
      // pair names exactly the failed fixture.
      const r = await request.get(
        `/api/v1/search?types=asset&dsl=${encodeURIComponent(`!nopreviews AND !list${failedId},${readyId}`)}`,
      );
      expect(r.status(), await r.text()).toBe(200);
      const body = (await r.json()) as { total_count: number; hits: { id: string }[] };
      expect(body.total_count, 'the failed fixture is not the one and only missing-preview row of the pair').toBe(1);
      expect(body.hits.map((h) => h.id)).toEqual([failedId]);
    });

    test('!nopreviews beside a word renders the failed asset and not the ready one', async ({ page }) => {
      const dsl = await submitFreeText(page, `${WORD} !nopreviews`);
      expect(dsl).toBe(`${WORD} !nopreviews`);
      await expect(page.locator(`a[href="/assets/${failedId}"]`).first(), 'the failed asset did not render').toBeVisible({
        timeout: 20_000,
      });
      await expect(page.locator(`a[href="/assets/${readyId}"]`), 'the ready asset rendered under !nopreviews').toHaveCount(0);
      const ids = await renderedAssetIds(page);
      expect(ids).toEqual([failedId]);
    });

    test('!list<a>,<b> renders exactly those two', async ({ page }) => {
      const dsl = await submitFreeText(page, `!list${failedId},${readyId}`);
      expect(dsl).toBe(`!list${failedId},${readyId}`);
      await expect(page.locator(`a[href="/assets/${readyId}"]`).first()).toBeVisible({ timeout: 20_000 });
      await expect(page.locator(`a[href="/assets/${failedId}"]`).first()).toBeVisible();
      expect(await renderedAssetIds(page)).toEqual([failedId, readyId].sort());
    });

    test('NOT !nopreviews surfaces the DSL error', async ({ page, request }) => {
      const dsl = await submitFreeText(page, 'NOT !nopreviews');
      expect(dsl).toBe('NOT !nopreviews');
      await expect(page.locator('[role="alert"]'), 'no error surfaced for a refused placement').toBeVisible({
        timeout: 20_000,
      });
      await expect(page.locator(tid('search-total-count'))).toHaveCount(0);
      // And the reason is placement, not a generic failure.
      const r = await request.get(`/api/v1/search?dsl=${encodeURIComponent('NOT !nopreviews')}`);
      expect(r.status()).toBe(400);
      expect(((await r.json()) as { message?: string }).message ?? '').toContain('top-level');
    });
  });
}
