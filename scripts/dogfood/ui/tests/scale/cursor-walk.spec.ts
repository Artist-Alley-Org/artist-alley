// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// cursor-walk.spec.ts: every cursor-paged list walked to its end on a
// whole public dataset edition (#1514, ui-scale.yml).
//
// # What is being proved
//
// For /api/v1/assets and /api/v1/posts, at the UI's page size (36) and at
// the API's cap (200), as the bootstrap admin and as ordinary seeded users:
//
//   - the walk terminates, by the server withholding next_cursor, within
//     ceil(rows / limit) + 2 pages;
//   - no page exceeds the limit, and no id is handed out twice;
//   - the id SEQUENCE at limit 36 equals the sequence at limit 200, so no
//     page boundary drops, repeats or reorders a row;
//   - the admin's sequence equals the database's own live set in the
//     API's keyset order, recorded by scripts/ci/scale_expected.py.
//
// # Why the two endpoints are held to different page rules
//
// /assets keeps an unreadable row as a withheld placeholder that still
// carries its id (app/internal/assets/withhold.go), so every non-final
// page is FULL. /posts runs its SQL and then drops posts whose members are
// all restricted for the caller (posts/feed_filters.go, applyHideRestricted)
// while next_cursor follows the SQL window (posts/handler.go). Its
// non-final pages may therefore be short or even EMPTY, and a walker must
// keep going. That is exactly the path an ordinary site_a user exercises.
//
// # Why an ordinary user's POSTS are not compared with a database set
//
// Which posts the hide step drops depends on the per-caller "restricted"
// mark that enrichPreview computes for each member. That is its own rule,
// not the asset listing's, so deriving it here would re-implement the code
// under test. The posts checks for an ordinary user are instead: a subset
// of the admin set, in the same relative order, containing every post the
// user authored, plus the page-size invariance above.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import {
  checkRunInputs,
  scaleSite,
  scalePrincipalNames,
  signInAs,
  walk,
  checkWalkShape,
  sequenceDiff,
  SAME_SEQUENCE,
  pageBound,
} from '../../helpers/scale';

const LIMITS = [36, 200] as const;
/** Three pages of the UI's 36: the floor below which "endless scroll" is not exercised. */
const THIRD_PAGE_FLOOR = 73;

test.describe.configure({ mode: 'serial' });

test.describe(`full-corpus cursor walks (${scaleSite()})`, () => {
  test('admin: /assets and /posts walk to the end and equal the database', async ({ request }) => {
    test.setTimeout(10 * 60_000);
    const E = checkRunInputs();
    await loginAsAdminViaAPI(request);

    const assetSeq: string[][] = [];
    const postSeq: string[][] = [];
    for (const limit of LIMITS) {
      const a = await walk(request, '/api/v1/assets', {}, limit, pageBound(E.admin.assets.length, limit));
      checkWalkShape(a, `admin /assets limit ${limit}`, limit, { fullNonFinalPages: true });
      expect(
        a.pages.reduce((n, p) => n + p.restricted, 0),
        `admin /assets limit ${limit}: system.admin reads every asset, so nothing is withheld`,
      ).toBe(0);
      expect(sequenceDiff(a.ids, E.admin.assets), `admin /assets limit ${limit} vs the database`).toEqual(
        SAME_SEQUENCE(E.admin.assets.length),
      );
      assetSeq.push(a.ids);

      const p = await walk(request, '/api/v1/posts', {}, limit, pageBound(E.admin.posts.length, limit));
      checkWalkShape(p, `admin /posts limit ${limit}`, limit, { fullNonFinalPages: false });
      expect(sequenceDiff(p.ids, E.admin.posts), `admin /posts limit ${limit} vs the database`).toEqual(
        SAME_SEQUENCE(E.admin.posts.length),
      );
      postSeq.push(p.ids);
    }
    expect(sequenceDiff(assetSeq[1], assetSeq[0]), 'admin /assets: limit 200 sequence equals limit 36').toEqual(
      SAME_SEQUENCE(assetSeq[0].length),
    );
    expect(sequenceDiff(postSeq[1], postSeq[0]), 'admin /posts: limit 200 sequence equals limit 36').toEqual(
      SAME_SEQUENCE(postSeq[0].length),
    );
    expect(E.admin.assets.length, 'the admin asset set is the whole edition').toBe(E.manifest.assets);
    expect(E.admin.posts.length, 'the admin post set holds every posts.json row').toBeGreaterThanOrEqual(
      E.manifest.posts,
    );
  });

  for (const name of scalePrincipalNames()) {
    test(`${name}: /assets and /posts walk to the end within that user's visibility`, async ({
      request,
    }, testInfo) => {
      test.setTimeout(10 * 60_000);
      const E = checkRunInputs();
      await loginAsAdminViaAPI(request);
      const P = E.principals[name];
      const baseURL = String(testInfo.project.use.baseURL);
      const ctx = await signInAs(request, baseURL, name, P.ref);
      try {
        const assetSeq: string[][] = [];
        const postSeq: string[][] = [];
        const restricted: number[] = [];
        const shortPosts: number[] = [];
        for (const limit of LIMITS) {
          const a = await walk(ctx, '/api/v1/assets', {}, limit, pageBound(E.admin.assets.length, limit));
          checkWalkShape(a, `${name} /assets limit ${limit}`, limit, { fullNonFinalPages: true });
          // Every live asset except the mature rows this user does not own,
          // placeholders included (they keep their id).
          expect(sequenceDiff(a.ids, P.assets), `${name} /assets limit ${limit} vs the database`).toEqual(
            SAME_SEQUENCE(P.assets.length),
          );
          assetSeq.push(a.ids);
          restricted.push(a.pages.reduce((n, p) => n + p.restricted, 0));

          const p = await walk(ctx, '/api/v1/posts', {}, limit, pageBound(E.admin.posts.length, limit));
          const shape = checkWalkShape(p, `${name} /posts limit ${limit}`, limit, { fullNonFinalPages: false });
          shortPosts.push(shape.shortNonFinal);
          postSeq.push(p.ids);
        }
        expect(sequenceDiff(assetSeq[1], assetSeq[0]), `${name} /assets: limit 200 equals limit 36`).toEqual(
          SAME_SEQUENCE(assetSeq[0].length),
        );
        expect(sequenceDiff(postSeq[1], postSeq[0]), `${name} /posts: limit 200 equals limit 36`).toEqual(
          SAME_SEQUENCE(postSeq[0].length),
        );

        const posts = postSeq[0];
        const seen = new Set(posts);
        // A subset of the admin set, in the admin's relative order.
        expect(
          sequenceDiff(posts, E.admin.posts.filter((id) => seen.has(id))),
          `${name} /posts is an order-preserving subset of the admin set`,
        ).toEqual(SAME_SEQUENCE(posts.length));
        expect(
          posts.filter((id) => !E.admin.posts.includes(id)).length,
          `${name} /posts holds nothing the admin set lacks`,
        ).toBe(0);
        const missingOwn = P.authored_posts.filter((id) => !seen.has(id));
        expect(missingOwn.length, `${name} sees every post they authored (missing ${missingOwn.slice(0, 3)})`).toBe(0);

        testInfo.annotations.push({
          type: 'scale',
          description:
            `${E.site} ${name}: ${P.assets.length} assets (${restricted[0]} withheld at limit 36), ` +
            `${posts.length} of ${E.admin.posts.length} posts, short non-final post pages ` +
            `${shortPosts[0]} at 36 / ${shortPosts[1]} at 200`,
        });

        if (E.site === 'site_a') {
          // site_a mixes team, public and restricted tiers across 31
          // owners, so an ordinary user meets withheld rows, and enough of
          // them to drop whole posts from some pages.
          expect(restricted[0], `${name}: site_a withholds some assets from an ordinary user`).toBeGreaterThan(0);
          expect(posts.length, `${name}: at least three UI pages of posts`).toBeGreaterThanOrEqual(THIRD_PAGE_FLOOR);
          expect(
            shortPosts[0],
            `${name}: at least one short non-final /posts page at limit 36, so the walk proved it keeps ` +
              `going past one (pinned edition; re-check if the edition changes)`,
          ).toBeGreaterThan(0);
        } else {
          // site_b is all public with no mature row, so nothing is withheld
          // and nothing is hidden: the user's sets ARE the admin's.
          expect(E.db.assets_mature + E.db.posts_mature, 'site_b holds no mature row').toBe(0);
          expect(restricted[0], `${name}: nothing on site_b is withheld`).toBe(0);
          expect(sequenceDiff(posts, E.admin.posts), `${name} /posts equals the admin set on site_b`).toEqual(
            SAME_SEQUENCE(E.admin.posts.length),
          );
          expect(shortPosts[0] + shortPosts[1], `${name}: no short non-final page on site_b`).toBe(0);
        }
      } finally {
        await ctx.dispose();
      }
    });
  }
});
