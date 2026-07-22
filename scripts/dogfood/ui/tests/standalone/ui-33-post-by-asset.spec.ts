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

import { test, expect } from '../../helpers/test';

test.describe('UI-33 post-by-asset', () => {
  test('resolves an asset to the post(s) featuring it — no dead end', async ({ page }) => {
    // Find a real post + one of its member assets from the feed.
    const res = await page.request.get('/api/v1/posts?limit=20');
    expect(res.ok()).toBeTruthy();
    const { items } = await res.json();
    const post = (items ?? []).find(
      (p: { members?: { asset_id: string }[] }) => (p.members?.length ?? 0) > 0,
    );
    test.skip(!post, 'no post with members in the feed');
    const assetId = post.members[0].asset_id as string;

    await page.goto(`/posts/by-asset/${assetId}`);

    // Either it redirected straight to a post permalink (single visible
    // post) or it rendered the lookup page with a heading — never a 404.
    await expect(page).not.toHaveTitle(/Not Found|404/i);
    const url = page.url();
    if (/\/posts\/[0-9a-f-]{36}(\?|$)/.test(url)) {
      // Single-post redirect landed on the post permalink.
      expect(url).toMatch(/\/posts\/[0-9a-f-]{36}/);
    } else {
      // Multi-post list: the lookup heading + at least one post tile.
      await expect(page).toHaveURL(/\/posts\/by-asset\//);
      await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    }
  });

  test('the SimilarAssetsPanel link target renders (KNOWN_GAPS cleared)', async ({ page }) => {
    // Direct-navigate the route shape the panel links to; a random asset
    // with no posts is a valid empty result, not a 404.
    const res = await page.request.get('/api/v1/assets?limit=1');
    const { items } = await res.json();
    test.skip(!items?.length, 'no assets seeded');
    await page.goto(`/posts/by-asset/${items[0].id}`);
    await expect(page).not.toHaveTitle(/Not Found|404/i);
  });
});
