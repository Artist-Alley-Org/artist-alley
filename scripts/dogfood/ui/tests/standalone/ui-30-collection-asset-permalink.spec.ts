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
// collection with one pinned asset — then drives the UI through it and
// cleans up. That keeps it independent of any seeded collection (the
// demo dataset does not always pin assets into collections).
//
// Determinism (#488): the pinned asset is not "whatever ?limit=1
// returns" (arbitrary Postgres order — often a 3D/failed/non-preview
// asset, which flaked this spec). It is a stable, preview-friendly
// pick: ready + raster image, sorted by id. And byte sub-resource 404s
// (/variants/*, /file) are treated as content-availability facts, not
// navigation errors — see isBenignApiFailure.

import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

const TEST_COLLECTION_NAME = 'UI-30 permalink smoke';

// Raster-image extensions whose assets get the full preview/variant
// ladder. We deliberately steer away from 3D (.glb/.obj/.fbx), audio,
// documents, and anything still processing — those either lack a
// standard preview variant or 404 their byte sub-resources, which is
// exactly the noise this test was drowning in (#488).
const RASTER_IMAGE_EXTENSIONS = new Set([
  'jpg',
  'jpeg',
  'png',
  'gif',
  'webp',
  'bmp',
  'tif',
  'tiff',
  'avif',
]);

interface AssetListItem {
  id: string;
  file_extension?: string | null;
  processing_status?: string | null;
}

function isRasterImage(ext: string | null | undefined): boolean {
  return RASTER_IMAGE_EXTENSIONS.has((ext ?? '').toLowerCase().replace(/^\./, ''));
}

// API failures that are benign background noise and must not fail the
// golden-path assertion. Kept tight so a REAL navigation error still
// fails the test.
//
// Two classes are exempt:
//   1. background polls (unread-count / notifications) fired by the shell
//      on every page, unrelated to the asset we opened.
//   2. asset BYTE sub-resources — /variants/* and /file. A 404 here means
//      "this asset has no such derivative / no bytes to serve", which is
//      a content-availability fact, NOT a navigation failure. On a
//      demo-shaped seed most assets legitimately lack some variant, and
//      treating those 404s as errors is what made this spec flap (#488).
//
// A 404 on the asset RECORD itself — /api/v1/assets/{uuid} with no
// sub-path — is NOT exempt: that is the #475 regression this test
// guards, so it must still fail the run.
function isBenignApiFailure(url: string): boolean {
  if (/\/api\/v1\/(account\/messages\/unread-count|notifications)/.test(url)) {
    return true;
  }
  // Byte/variant sub-resources of an asset: /assets/{id}/variants/... or
  // /assets/{id}/file. The trailing sub-path is what distinguishes these
  // from the bare asset-record fetch, which stays a hard failure.
  return /\/api\/v1\/assets\/[^/]+\/(variants\/|file\b)/.test(url);
}

// pickDeterministicImageAsset returns a stable, preview-friendly asset:
// public/ready is not filterable server-side (the list DTO omits
// sensitivity, and `status` is the workflow lifecycle, not
// processing_status), so we page a chunk and filter client-side to
// processing_status==='ready' + a raster-image extension, then sort by
// id so the SAME asset is chosen on every run regardless of Postgres's
// arbitrary default order — the root of the flake (#488). The test pins
// it as admin, who reads content at any sensitivity, so sensitivity does
// not affect fixture stability here.
async function pickDeterministicImageAsset(page: Page): Promise<string> {
  const res = await page.request.get('/api/v1/assets?limit=200');
  expect(res.ok(), 'GET /assets should succeed').toBeTruthy();
  const { items } = (await res.json()) as { items: AssetListItem[] };

  const candidates = (items ?? [])
    .filter((a) => a.processing_status === 'ready' && isRasterImage(a.file_extension))
    .sort((a, b) => a.id.localeCompare(b.id));

  expect(
    candidates.length,
    'the seeded stack must have at least one ready raster-image asset for the permalink smoke',
  ).toBeGreaterThan(0);

  return candidates[0].id;
}

// provisionCollectionWithAsset creates a collection and pins one
// deterministic, preview-friendly asset into it, returning both ids.
// Uses page.request so it runs with the logged-in admin's cookies.
async function provisionCollectionWithAsset(
  page: Page,
): Promise<{ collectionId: string; assetId: string }> {
  const assetId = await pickDeterministicImageAsset(page);

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
