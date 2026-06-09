// ui-04-comments-render.spec.ts
//
// Catches null-scan / column-nullability regressions like the
// 500 on /posts/{id}/comments shipped this week (the LEFT JOIN
// against federation_remote_actors returned NULL into a non-
// nullable *string field). Walks every post in the feed, asks
// for its comments, and asserts none of them 500.
//
// This is "dumb scan coverage" — we don't care about the
// content, only that the endpoint doesn't crash on any
// seeded post.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaAPI } from '../helpers/auth';

test.describe('UI-04 comments endpoint', () => {
  test('GET /posts/{id}/comments returns 2xx for every visible post', async ({ request }) => {
    await loginAsAdminViaAPI(request);
    // Pull a decent sample — 50 posts covers every visibility +
    // every post kind from the seeded dataset.
    const listResp = await request.get('/api/v1/posts?limit=50');
    expect(listResp.status()).toBe(200);
    const list = await listResp.json();
    const posts = list.items ?? [];
    expect(posts.length).toBeGreaterThan(0);

    const failures: Array<{ id: string; status: number; body: string }> = [];
    for (const p of posts) {
      const r = await request.get(`/api/v1/posts/${p.id}/comments`);
      if (r.status() >= 400) {
        failures.push({
          id: p.id,
          status: r.status(),
          body: (await r.text()).slice(0, 200),
        });
      }
    }
    if (failures.length > 0) {
      throw new Error(
        `comments endpoint 500ed on ${failures.length} post(s):\n` +
          failures.map((f) => `  ${f.id}: HTTP ${f.status} — ${f.body}`).join('\n'),
      );
    }
  });
});
