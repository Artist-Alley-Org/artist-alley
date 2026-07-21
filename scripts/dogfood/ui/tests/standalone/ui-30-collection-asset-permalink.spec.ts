// ui-30-collection-asset-permalink.spec.ts
//
// #475 / ADR 0068 layer 3 — the golden-path walk the suite was missing:
// browse → collection → click an asset tile → the asset viewer opens on
// a real, reloadable /assets/{id} URL.
//
// ui-18 only exercises the collection fields editor; ui-29 only asserts
// that BOGUS routes 404 — neither ever confirmed that a REAL asset link
// resolves. This is that assertion, and it is exactly what #475 slipped
// past: every AssetCard linked to /assets/{id}, a route that did not
// exist, so clicking any asset inside a collection dead-ended on 404.
//
// Structural, not seed-id-bound: it PROVISIONS its own fixture — a
// collection with one pinned asset, built from whatever asset the API
// hands back — then drives the UI through it and cleans up. That keeps
// it deterministic across any seeded stack (the demo dataset does not
// always pin assets into collections).

import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

const TEST_COLLECTION_NAME = 'UI-30 permalink smoke';

// API failures that are benign background noise and must not fail the
// golden-path assertion. Kept tight so a REAL navigation error (a 404
// for the asset we opened) still fails the test.
function isBenignApiFailure(url: string): boolean {
  return /\/api\/v1\/(account\/messages\/unread-count|notifications)/.test(url);
}

// provisionCollectionWithAsset creates a collection and pins one real
// asset into it, returning both ids. Uses page.request so it runs with
// the logged-in admin's cookies.
async function provisionCollectionWithAsset(
  page: Page,
): Promise<{ collectionId: string; assetId: string }> {
  const assetsRes = await page.request.get('/api/v1/assets?limit=1');
  expect(assetsRes.ok(), 'GET /assets should succeed').toBeTruthy();
  const assets = (await assetsRes.json()) as { items: Array<{ id: string }> };
  const assetId = assets.items?.[0]?.id;
  expect(assetId, 'the seeded stack should have at least one asset').toBeTruthy();

  const createRes = await page.request.post('/api/v1/collections', {
    data: { name: TEST_COLLECTION_NAME },
  });
  expect(createRes.ok(), 'POST /collections should succeed').toBeTruthy();
  const collection = (await createRes.json()) as { id: string };

  const pinRes = await page.request.post(`/api/v1/collections/${collection.id}/resources`, {
    data: { asset_id: assetId, pinned: true },
  });
  expect(pinRes.ok(), 'pinning the asset should succeed').toBeTruthy();

  return { collectionId: collection.id, assetId };
}

test.describe('UI-30 collection → asset permalink', () => {
  let collectionId: string | undefined;

  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ page }) => {
    if (collectionId) {
      await page.request.delete(`/api/v1/collections/${collectionId}`).catch(() => undefined);
      collectionId = undefined;
    }
  });

  test('clicking an asset tile in a collection opens the viewer on a real /assets/{id} URL', async ({
    page,
  }) => {
    const provisioned = await provisionCollectionWithAsset(page);
    collectionId = provisioned.collectionId;

    // Collect real API failures during the navigation — the #475 class
    // is exactly "the click 4xx'd / dead-ended", so this is the guard.
    const apiFailures: string[] = [];
    page.on('response', (resp) => {
      const url = resp.url();
      if (url.includes('/api/') && resp.status() >= 400 && !isBenignApiFailure(url)) {
        apiFailures.push(`${resp.status()} ${url}`);
      }
    });

    await page.goto(`/collections/${collectionId}`);

    // The first asset tile — AssetCard renders <a href="/assets/{id}">.
    // Selecting by href shape keeps this independent of any testid and
    // is exactly the link #475 was about.
    const tile = page.locator('a[href^="/assets/"]').first();
    await expect(tile, 'the collection should render its pinned asset tile').toBeVisible();
    await tile.click();

    // A real, shareable /assets/{uuid} URL.
    await expect(page).toHaveURL(/\/assets\/[0-9a-f-]{36}$/);

    // The asset viewer actually opened — not the 404 page.
    await expect(page.getByTestId('asset-playlist')).toBeVisible();

    // No 404 / error-boundary shell fired. The default SvelteKit error
    // page surfaces the status text; assert it's absent.
    await expect(page.getByText(/not found/i)).toHaveCount(0);

    // Reload-safe: a shared/bookmarked asset URL must survive a hard
    // reload — a client-only pushState hack would fail this.
    await page.reload();
    await expect(page).toHaveURL(/\/assets\/[0-9a-f-]{36}$/);
    await expect(page.getByTestId('asset-playlist')).toBeVisible();

    expect(
      apiFailures,
      `unexpected API failures during the asset-open flow:\n${apiFailures.join('\n')}`,
    ).toEqual([]);
  });
});
