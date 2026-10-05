// ui-33-post-by-asset.spec.ts
//
// #478 slice-2 — post-by-asset lookup (ADR 0070). The SimilarAssetsPanel
// "featured in" link (/posts/by-asset/[id]) was the last KNOWN_GAPS
// entry; this verifies it now opens a real page: it resolves to the
// posts featuring the asset, redirecting when exactly one is visible and
// listing (through the shared TileGrid) when several.
//
// Visibility filtering (anon sees only public) is covered by the Go
// integration test (app/internal/posts/post_by_asset_test.go); this spec
// drives the rendered surface with the shared admin session.
//
// ── Why this file provisions its own posts (#1227) ───────────────────
//
// The route has three outcomes and this file asserts all three. It used
// to find each one by scanning the head of the shared feed:
//
//   list      — `/posts?limit=40`, keep an asset seen in >1 post,
//               `test.skip` if the window held none
//   redirect  — `/posts?limit=20`, first post with any member; whether
//               that member was in exactly ONE post was never checked,
//               so the branch it landed on was whatever the corpus
//               happened to be
//   empty     — `/assets?limit=1`, described in a comment as "a random
//               asset with no posts". Every seeded asset belongs to a
//               post, and the newest asset is the likeliest of all to,
//               so the case the comment names was the one case this
//               never exercised.
//
// That is #1227's shape twice over: the first is one row of drift away
// from skipping silently, and the third asserted the wrong branch while
// reading as though it asserted the right one. Both are decided by the
// ORDER of a shared corpus, which no spec here controls — the feed is
// newest-first and a couple of transient posts from a concurrently
// running spec are enough to move the window.
//
// So the three shapes are now GUARANTEED, provisioned here and removed
// in afterAll, and each test asserts the ONE outcome its fixture
// determines instead of accepting either. The corpus is not read at all
// any more, which is what makes the branch assertions unconditional.

import { test, expect, type APIRequestContext } from '../../helpers/test';

// Three 1x1 PNGs that differ only in the pixel's colour. Storage is
// content-addressed: identical bytes return the SAME asset id, so three
// fixtures that must stay distinct need three distinct payloads.
const PNG_1PX = [
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC',
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGNg+M8AAAICAQB7CYF4AAAAAElFTkSuQmCC',
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGNgYPgPAAEDAQAIicLsAAAAAElFTkSuQmCC',
].map((b64) => Buffer.from(b64, 'base64'));

const STAMP = Date.now();

interface Fixture {
  /** In exactly one visible post — the REDIRECT branch. */
  soloAssetId: string;
  soloPostId: string;
  /** In two visible posts — the LIST branch. */
  sharedAssetId: string;
  sharedPostIds: [string, string];
  /** In no post at all — the EMPTY branch. */
  orphanAssetId: string;
}

let fx: Fixture | undefined;

test.describe('UI-33 post-by-asset', () => {
  // Serial: all three cases read one provisioned set.
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }: { request: APIRequestContext }) => {
    const json = async (r: { json(): Promise<unknown> }) =>
      (await r.json()) as Record<string, unknown>;

    const mkAsset = async (n: number, label: string): Promise<string> => {
      const up = await request.post('/api/v1/storage/objects', {
        data: PNG_1PX[n],
        headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'image/png' },
      });
      expect(up.status(), `uploading fixture bytes ${n}`).toBe(201);
      const fileHash = String((await json(up)).hash);
      const r = await request.post('/api/v1/assets', {
        data: {
          title: `478 ${label} ${STAMP}`,
          asset_type: 1,
          file_extension: 'png',
          file_hash: fileHash,
          original_filename: `478-${label}-${STAMP}.png`,
        },
      });
      expect(r.status(), `creating the ${label} fixture asset`).toBeLessThan(300);
      return String((await json(r)).id);
    };

    const mkPost = async (label: string, assetId: string): Promise<string> => {
      const r = await request.post('/api/v1/posts', {
        data: {
          title: `478 ${label} ${STAMP}`,
          description: '478 fixture',
          members: [{ asset_id: assetId }],
        },
      });
      expect(r.status(), `creating post "${label}"`).toBeLessThan(300);
      return String((await json(r)).id);
    };

    const soloAssetId = await mkAsset(0, 'solo');
    const sharedAssetId = await mkAsset(1, 'shared');
    const orphanAssetId = await mkAsset(2, 'orphan');

    fx = {
      soloAssetId,
      soloPostId: await mkPost('solo', soloAssetId),
      sharedAssetId,
      sharedPostIds: [await mkPost('shared A', sharedAssetId), await mkPost('shared B', sharedAssetId)],
      orphanAssetId,
    };
  });

  test.afterAll(async ({ request }: { request: APIRequestContext }) => {
    if (!fx) return;
    for (const id of [fx.soloPostId, ...fx.sharedPostIds]) {
      await request.delete(`/api/v1/posts/${id}`).catch(() => undefined);
    }
    for (const id of [fx.soloAssetId, fx.sharedAssetId, fx.orphanAssetId]) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
  });

  test('an asset in exactly one visible post redirects to that permalink', async ({ page }) => {
    await page.goto(`/posts/by-asset/${fx!.soloAssetId}`);

    await expect(page).not.toHaveTitle(/Not Found|404/i);
    // Not "either branch is fine": the fixture puts this asset in one
    // post and one post only, so the redirect is the answer and a
    // listing here is the regression.
    await expect(page).toHaveURL(new RegExp(`/posts/${fx!.soloPostId}(\\?|#|$)`));
  });

  test('an asset in several visible posts lists them', async ({ page }) => {
    await page.goto(`/posts/by-asset/${fx!.sharedAssetId}`);

    await expect(page).not.toHaveTitle(/Not Found|404/i);
    await expect(page).toHaveURL(/\/posts\/by-asset\//);
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    // Both posts, by their own permalinks — a heading alone would pass
    // on a page that listed nothing.
    for (const id of fx!.sharedPostIds) {
      await expect(page.locator(`a[href^="/posts/${id}"]`).first()).toBeVisible();
    }
  });

  test('mounts the shared view controls (#511)', async ({ page }) => {
    // The listing branch is the one that HAS controls, so this drives
    // the asset the fixture guarantees lists.
    await page.goto(`/posts/by-asset/${fx!.sharedAssetId}`);
    // Same shared control bar as browse + profile.
    await expect(page.getByTestId('view-controls')).toBeVisible();
    // And, as on the profile, WITHOUT browse's asset-type filter
    // (#1166) — see ui-32 for why that absence is asserted.
    await expect(page.getByTestId('kind-filter-toggle')).toHaveCount(0);
  });

  test('the SimilarAssetsPanel link target renders (KNOWN_GAPS cleared)', async ({ page }) => {
    // Direct-navigate the route shape the panel links to, for an asset
    // that is in NO post: a valid empty result, not a 404. Provisioned,
    // because every seeded asset belongs to a post and this case cannot
    // be found by reading the catalogue.
    await page.goto(`/posts/by-asset/${fx!.orphanAssetId}`);
    await expect(page).not.toHaveTitle(/Not Found|404/i);
    await expect(page).toHaveURL(/\/posts\/by-asset\//);
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
  });
});
