// ui-30-collection-asset-permalink.spec.ts
//
// TWO assertions over one fixture — a collection with a single pinned
// asset — because #1185 turned the first one inside out.
//
// 1. #1185 — A COLLECTION SHOWS POSTS ONLY. The owner's ruling: "non-post
//    assets only belong to their uploader; collections and browse contain
//    posts only." So a collection holding nothing but a pinned asset is,
//    to every reader, EMPTY. This spec pins the absence: no
//    `collection-assets` section, no asset tile, and the empty state on
//    the page. Pinning the absence rather than deleting the old assertion
//    is deliberate — `POST /collections/{id}/resources` still succeeds
//    (dropping it is #1161), so nothing else in the suite would notice
//    the section coming back.
//
// 2. #475 / ADR 0068 layer 3 — THE /assets/{id} PERMALINK STILL RESOLVES.
//    That route did not exist once, so every AssetCard linked into a 404.
//    The walk this spec used to make (collection → click a tile → viewer)
//    is no longer a thing the product does, but the route it proved is
//    still real and still reachable from browse, search and the profile
//    uploads grid. ui-13 covers the card→link click on those surfaces;
//    what is asserted here is the destination: the URL opens the viewer,
//    survives a hard reload, and 404s nothing.
//
// Structural, not seed-id-bound: it PROVISIONS its own fixture, then
// cleans up. That keeps it independent of any seeded collection.
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

// provisionEmptyCollection creates a collection and picks one
// deterministic, preview-friendly asset — WITHOUT pinning the asset
// into it, because #1161 retired the endpoint that could.
//
// That retirement is why this helper changed shape. It used to POST to
// `/collections/{id}/resources`, and the first test below then checked
// that the resulting bare membership rendered as nothing (#1185's
// finding). ADR 0091 closed the write path entirely, so the state that
// test described can no longer be created through the API at all —
// which is a STRONGER guarantee than the one it was asserting, and the
// first test below now asserts that instead.
async function provisionEmptyCollection(
  page: Page,
): Promise<{ collectionId: string; assetId: string }> {
  const assetId = await pickDeterministicImageAsset(page);

  const createRes = await page.request.post('/api/v1/collections', {
    data: { name: TEST_COLLECTION_NAME },
  });
  expect(createRes.ok(), 'POST /collections should succeed').toBeTruthy();
  const collection = (await createRes.json()) as { id: string };

  return { collectionId: collection.id, assetId };
}

test.describe('UI-30 collections are posts-only, and /assets/{id} still resolves', () => {
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

  test('a bare asset can no longer be pinned at all, and the wall shows nothing (#1185, #1161)', async ({
    page,
  }) => {
    const provisioned = await provisionEmptyCollection(page);
    collectionId = provisioned.collectionId;

    // #1161 / ADR 0091: the write endpoints are gone, so the state
    // #1185 hid from the page cannot be reached through the API at
    // all. 404 or 405 both mean "not routed"; a handler answering
    // anything else would mean the endpoint is merely unused.
    const pinRes = await page.request.post(`/api/v1/collections/${collectionId}/resources`, {
      data: { asset_id: provisioned.assetId, pinned: true },
    });
    expect(
      [404, 405],
      `POST /collections/{id}/resources answered ${pinRes.status()} — the retired write endpoint is still routed`,
    ).toContain(pinRes.status());

    const rmRes = await page.request.delete(
      `/api/v1/collections/${collectionId}/resources/${provisioned.assetId}`,
    );
    expect(
      [404, 405],
      `DELETE /collections/{id}/resources/{asset_id} answered ${rmRes.status()} — still routed`,
    ).toContain(rmRes.status());

    await page.goto(`/collections/${collectionId}`);

    // The collection loaded as ITSELF, not as the 404 plate — otherwise
    // every absence below would pass for the wrong reason.
    await expect(page.getByRole('heading', { name: TEST_COLLECTION_NAME })).toBeVisible();
    await expect(page.getByTestId('collection-unavailable')).toHaveCount(0);

    // The section, the tile and the heading are all gone. `toHaveCount(0)`
    // and not `toBeHidden`: the markup must not be rendered at all.
    await expect(
      page.getByTestId('collection-assets'),
      'the collection page still renders the non-post assets section',
    ).toHaveCount(0);
    await expect(
      page.locator('main a[href^="/assets/"]'),
      'an asset tile is still on the collection wall',
    ).toHaveCount(0);
    await expect(page.getByRole('heading', { name: 'Assets', exact: true })).toHaveCount(0);

    // And what a reader sees instead is the honest answer: this
    // collection has nothing on its wall, because a pinned asset is not
    // content a collection shows.
    await expect(page.getByText('Nothing in this collection yet.')).toBeVisible();
    await expect(page.getByTestId('collection-posts')).toHaveCount(0);
  });

  test('the /assets/{id} permalink opens the viewer and survives a reload (#475)', async ({
    page,
  }) => {
    const provisioned = await provisionEmptyCollection(page);
    collectionId = provisioned.collectionId;

    // Collect real API failures during the navigation — the #475 class
    // is exactly "the link 4xx'd / dead-ended", so this is the guard.
    const apiFailures: string[] = [];
    page.on('response', (resp) => {
      const url = resp.url();
      if (url.includes('/api/') && resp.status() >= 400 && !isBenignApiFailure(url)) {
        apiFailures.push(`${resp.status()} ${url}`);
      }
    });

    // The address every AssetCard on browse / search / the profile
    // uploads grid points at.
    await page.goto(`/assets/${provisioned.assetId}`);
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
