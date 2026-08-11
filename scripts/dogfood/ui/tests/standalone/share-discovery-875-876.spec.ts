// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #875 + #876 — a share becomes discoverable without becoming a leak.
//
// #667 made a share WORK (see post-acl-share-667.spec.ts): the grantee
// can open the post. It could still only be FOUND if the sharer sent a
// link out of band — nothing notified the recipient, and the browse feed
// pins `visibility=org-only` when no filter is sent, so the post never
// entered their grid. And gating the ACL list on "can read the post"
// meant the grantee could enumerate everyone else the post was shared
// with.
//
// Everything below is asserted in the GRANTEE's own browser session,
// signed in through the login form, because every claim here is about
// what one specific caller's ref can see. A borrowed admin cookie would
// prove nothing about whose ref the ACL predicate bound.
//
// Two controls carry most of the weight:
//   - an ungranted post at the SAME tier, so "the surface showed
//     something" can never be satisfied by the tier filter having
//     stopped being enforced;
//   - the 403 on the ACL list is asserted in the same test as a 200 on
//     the post itself, so #876 cannot pass by the share having broken.

import { test, expect, type APIRequestContext, type Browser } from '@playwright/test';
import { LOGGED_OUT } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

const PNG_1PX = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
);

const STAMP = Date.now();
const GRANTEE_USER = `share875_${STAMP}`;
const GRANTEE_PASS = 'Sharing1sCaring!875';
const SHARED_TITLE = `share875 shared ${STAMP}`;
const CONTROL_TITLE = `share875 control ${STAMP}`;

interface Fixture {
  granteeRef: number;
  sharedPostId: string;
  unsharedPostId: string;
  assetId: string;
  priorSelfRegistration: unknown;
}

let fx: Fixture | undefined;

async function json(r: { json(): Promise<unknown> }): Promise<Record<string, unknown>> {
  return (await r.json()) as Record<string, unknown>;
}

test.describe('#875/#876 a share is announced, findable, and not a guest list', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ browser, request }: { browser: Browser; request: APIRequestContext }) => {
    const authCfg = await request.get('/api/v1/admin/system/auth');
    expect(authCfg.status(), 'admin auth config must be readable').toBe(200);
    const priorSelfRegistration = (await json(authCfg)).self_registration;

    const enable = await request.patch('/api/v1/admin/system/auth', {
      data: {
        self_registration: {
          enabled: true,
          require_email_verification: false,
          default_role: 'Base',
        },
      },
    });
    expect(enable.status(), 'enabling self-registration').toBeLessThan(400);

    const anon = await browser.newContext({ storageState: LOGGED_OUT });
    const reg = await anon.request.post('/api/v1/auth/register', {
      data: {
        username: GRANTEE_USER,
        email: `${GRANTEE_USER}@example.test`,
        password: GRANTEE_PASS,
        full_name: 'share875 grantee',
      },
    });
    expect(reg.status(), 'registering the grantee').toBeLessThan(400);
    const granteeRef = Number((await json(reg)).ref);
    expect(Number.isFinite(granteeRef) && granteeRef > 0).toBe(true);
    await anon.close();

    const up = await request.post('/api/v1/storage/objects', {
      data: PNG_1PX,
      headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'image/png' },
    });
    expect(up.status(), 'uploading fixture bytes').toBe(201);
    const fileHash = String((await json(up)).hash);

    const asset = await request.post('/api/v1/assets', {
      data: {
        title: `share875 fixture ${STAMP}`,
        asset_type: 1,
        file_extension: 'png',
        file_hash: fileHash,
        original_filename: 'share875.png',
      },
    });
    expect(asset.status(), 'creating the fixture asset').toBeLessThan(300);
    const assetId = String((await json(asset)).id);

    const mkPost = async (title: string): Promise<string> => {
      const r = await request.post('/api/v1/posts', {
        data: {
          title,
          description: 'share875 fixture',
          visibility: 'explicit-share',
          members: [{ asset_id: assetId }],
        },
      });
      expect(r.status(), `creating post "${title}"`).toBeLessThan(300);
      return String((await json(r)).id);
    };
    const sharedPostId = await mkPost(SHARED_TITLE);
    const unsharedPostId = await mkPost(CONTROL_TITLE);

    // A SECOND grant on the shared post, to a principal that is not the
    // grantee. This is the row #876 must hide: without it the ACL list
    // would only ever contain the grantee's own grant and "the list
    // leaked" would have nothing to leak.
    const grant = await request.post(`/api/v1/posts/${sharedPostId}/acls`, {
      data: { principal_type: 'user', principal_id: String(granteeRef), permission: 'read' },
    });
    expect(grant.status(), 'granting read to the grantee').toBe(204);

    const otherGrant = await request.post(`/api/v1/posts/${sharedPostId}/acls`, {
      data: { principal_type: 'user', principal_id: '999000875', permission: 'read' },
    });
    expect(otherGrant.status(), 'granting read to a second principal').toBe(204);

    fx = { granteeRef, sharedPostId, unsharedPostId, assetId, priorSelfRegistration };
  });

  test.afterAll(async ({ request }: { request: APIRequestContext }) => {
    if (!fx) return;
    await request.delete(`/api/v1/posts/${fx.sharedPostId}`).catch(() => undefined);
    await request.delete(`/api/v1/posts/${fx.unsharedPostId}`).catch(() => undefined);
    await request.delete(`/api/v1/assets/${fx.assetId}`).catch(() => undefined);
    if (fx.priorSelfRegistration !== undefined) {
      await request
        .patch('/api/v1/admin/system/auth', {
          data: { self_registration: fx.priorSelfRegistration },
        })
        .catch(() => undefined);
    }
  });

  async function granteeContext(browser: Browser) {
    const ctx = await browser.newContext({ storageState: LOGGED_OUT });
    const page = await ctx.newPage();
    await page.goto('/login');
    await page.locator(tid('login-username')).fill(GRANTEE_USER);
    await page.locator(tid('login-password')).fill(GRANTEE_PASS);
    await page.locator(tid('login-submit')).click();
    await page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 20_000 });
    return { ctx, page };
  }

  // ---- #875(a): the share announces itself --------------------------

  test('the grantee is notified, and the notification points at the post', async ({ browser }) => {
    const { ctx, page } = await granteeContext(browser);
    try {
      await page.goto('/account/notifications');
      await expect(page.locator('body')).toContainText('A post was shared with you', {
        timeout: 20_000,
      });
      // The title rides in the payload so the card names the post
      // rather than saying "something was shared".
      await expect(page.locator('body')).toContainText(SHARED_TITLE);

      // Following it must land on the post, not on a dead end.
      await page.getByText('A post was shared with you').first().click();
      await page.waitForURL((u) => u.pathname.startsWith('/posts/'), { timeout: 20_000 });
      expect(page.url()).toContain(fx!.sharedPostId);
      await expect(page.locator('body')).toContainText(SHARED_TITLE, { timeout: 20_000 });
    } finally {
      await ctx.close();
    }
  });

  // ---- #875(b): the share accumulates somewhere ---------------------

  test('the shared post is on "Shared with me", and the ungranted one is not', async ({
    browser,
  }) => {
    const { ctx, page } = await granteeContext(browser);
    try {
      await page.goto('/account/shared');
      await expect(page.locator('body')).toContainText(SHARED_TITLE, { timeout: 20_000 });
      // The control is the test. Both posts are `explicit-share` by the
      // same author; only one carries a grant.
      await expect(page.locator('body')).not.toContainText(CONTROL_TITLE);
    } finally {
      await ctx.close();
    }
  });

  test('the default browse feed still does not carry the share', async ({ browser }) => {
    // #875 was deliberately NOT fixed by widening the feed: `/posts`
    // with no filter still means the org-only tier. If a post_acls
    // EXISTS ever leaks into that query this fails, which is the point.
    const { ctx, page } = await granteeContext(browser);
    try {
      const list = await page.request.get('/api/v1/posts?limit=200');
      expect(list.status()).toBe(200);
      const ids = (((await list.json()) as { items?: Array<{ id: string }> }).items ?? []).map(
        (i) => i.id,
      );
      expect(ids, 'the shared post must NOT be in the default feed').not.toContain(
        fx!.sharedPostId,
      );

      // And the surface that IS supposed to carry it does.
      const shared = await page.request.get('/api/v1/account/shared-posts?limit=200');
      expect(shared.status()).toBe(200);
      const sharedIds = (
        ((await shared.json()) as { items?: Array<{ id: string }> }).items ?? []
      ).map((i) => i.id);
      expect(sharedIds, 'the shared post must be on /account/shared-posts').toContain(
        fx!.sharedPostId,
      );
      expect(sharedIds, 'the ungranted post must not be').not.toContain(fx!.unsharedPostId);
    } finally {
      await ctx.close();
    }
  });

  // ---- #876: the guest list ----------------------------------------

  test('the grantee is refused the guest list while still reading the post', async ({
    browser,
  }) => {
    // Both directions in ONE test on purpose. A 403 on the ACL list
    // passes just as happily on a build where the grantee can read
    // nothing at all — i.e. where #876 was "fixed" by breaking #667.
    const { ctx, page } = await granteeContext(browser);
    try {
      const acls = await page.request.get(`/api/v1/posts/${fx!.sharedPostId}/acls`);
      expect(acls.status(), 'the grantee must not be able to list the grants').toBe(403);

      const post = await page.request.get(`/api/v1/posts/${fx!.sharedPostId}`);
      expect(post.status(), 'and the share itself must still work').toBe(200);
    } finally {
      await ctx.close();
    }
  });

  test('the author still gets the guest list', async ({ request }) => {
    // Narrowing an endpoint is only correct if the people who need it
    // kept it. The admin here is the post's author.
    const acls = await request.get(`/api/v1/posts/${fx!.sharedPostId}/acls`);
    expect(acls.status()).toBe(200);
    const rows = (await acls.json()) as Array<{ principal_id: string }>;
    expect(rows.map((r) => r.principal_id)).toContain(String(fx!.granteeRef));
    expect(rows.map((r) => r.principal_id)).toContain('999000875');
  });
});
