// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #667 — an explicit share must actually grant read, in the browser.
//
// `POST /posts/{id}/acls` has always written a post_acls row and listed
// it straight back, so every surface that talks ABOUT sharing looked
// correct. Nothing on a read path consulted the table, so the person you
// shared with saw no difference at all: the link you sent them answered
// "not visible to this user". A test that only checks the grant was
// stored passes on that build — the grant WAS stored.
//
// So the observable here is the GRANTEE's browser, and it is asserted
// against a control in the same fixture: two posts by the same author at
// the SAME tier (`explicit-share`), differing only in whether a grant
// names the grantee. One must open; the other must not. Without the
// control, "the page rendered" could equally mean the tier stopped being
// enforced, which is the opposite bug and a far worse one.
//
// The grantee is a real second session driven through the login form,
// not a cookie transplanted from the admin's context — the ACL disjunct
// binds the caller's own user ref, and a borrowed session would prove
// nothing about whose ref it bound.
//
// NOT asserted here, deliberately: that the shared post shows up in the
// grantee's default browse grid. `GET /posts` pins `visibility=org-only`
// when the caller sends no filter (posts/handler.go), so the browse feed
// shows the walled-garden tier and nothing else regardless of grants.
// That is a display default, not the read rule, and changing it is a
// separate product decision.

import { test, expect, type APIRequestContext, type Browser } from '@playwright/test';
import { LOGGED_OUT } from '../../helpers/auth';
import { ensureFixtureUser, restoreSelfRegistration } from '../../helpers/fixture-user';
import { tid } from '../../helpers/testids';

// A 1x1 PNG. The post needs at least one member asset; what the pixels
// are is irrelevant to the read rule.
const PNG_1PX = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
);

const STAMP = Date.now();
// Constant account, stamped CONTENT (#1198) — see helpers/fixture-user.ts
// for why a per-run account was the thing that reddened three unrelated
// specs on every local run.
const GRANTEE_USER = 'acl667_grantee';
const GRANTEE_PASS = 'Sharing1sCaring!667';

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

test.describe('#667 an explicit share grants read to the grantee', () => {
  // Serial: every test in this file reads one shared fixture, and the
  // fixture creates a user + two posts. Building it per-test would
  // register a new account per test for no added coverage.
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ browser, request }) => {
    // The grantee has to exist. It is resolved, not created: the helper
    // signs in as the constant fixture account and only registers it —
    // toggling self-registration and putting it straight back — on an
    // instance that has never run this suite.
    const { ref: granteeRef, priorSelfRegistration } = await ensureFixtureUser(
      browser,
      request,
      { username: GRANTEE_USER, password: GRANTEE_PASS, fullName: 'ACL 667 grantee' },
    );

    // Author side: one asset, two explicit-share posts on it.
    const up = await request.post('/api/v1/storage/objects', {
      data: PNG_1PX,
      headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'image/png' },
    });
    expect(up.status(), 'uploading fixture bytes').toBe(201);
    const fileHash = String((await json(up)).hash);

    const asset = await request.post('/api/v1/assets', {
      data: {
        title: `acl667 fixture ${STAMP}`,
        asset_type: 1,
        file_extension: 'png',
        file_hash: fileHash,
        original_filename: 'acl667.png',
      },
    });
    expect(asset.status(), 'creating the fixture asset').toBeLessThan(300);
    const assetId = String((await json(asset)).id);

    const mkPost = async (title: string): Promise<string> => {
      const r = await request.post('/api/v1/posts', {
        data: {
          title,
          description: 'acl667 fixture',
          visibility: 'explicit-share',
          members: [{ asset_id: assetId }],
        },
      });
      expect(r.status(), `creating post "${title}"`).toBeLessThan(300);
      return String((await json(r)).id);
    };
    const sharedPostId = await mkPost(`acl667 shared ${STAMP}`);
    const unsharedPostId = await mkPost(`acl667 control ${STAMP}`);

    // The grant, through the real endpoint. principal_id is TEXT.
    const grant = await request.post(`/api/v1/posts/${sharedPostId}/acls`, {
      data: {
        principal_type: 'user',
        principal_id: String(granteeRef),
        permission: 'read',
      },
    });
    expect(grant.status(), 'granting read to the grantee').toBe(204);

    fx = { granteeRef, sharedPostId, unsharedPostId, assetId, priorSelfRegistration };
  });

  test.afterAll(async ({ request }) => {
    if (!fx) return;
    await request.delete(`/api/v1/posts/${fx.sharedPostId}`).catch(() => undefined);
    await request.delete(`/api/v1/posts/${fx.unsharedPostId}`).catch(() => undefined);
    await request.delete(`/api/v1/assets/${fx.assetId}`).catch(() => undefined);
    // The grantee ACCOUNT survives on purpose — the next run reuses it
    // rather than adding another (#1198).
    await restoreSelfRegistration(request, fx.priorSelfRegistration);
  });

  // Sign the grantee in through the form, in their own context.
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

  test('the grantee can open the post that was shared with them', async ({ browser }) => {
    const { ctx, page } = await granteeContext(browser);
    try {
      await page.goto(`/posts/${fx!.sharedPostId}`);
      // The title on the page is the honest signal: a shell that
      // mounted and then rendered a refusal would still satisfy
      // "banner is visible".
      await expect(page.locator('body')).toContainText(`acl667 shared ${STAMP}`, {
        timeout: 20_000,
      });
      await expect(page.locator('body')).not.toContainText('not visible to this user');
    } finally {
      await ctx.close();
    }
  });

  test('an explicit-share post with no grant stays shut for the same caller', async ({
    browser,
  }) => {
    const { ctx, page } = await granteeContext(browser);
    try {
      await page.goto(`/posts/${fx!.unsharedPostId}`);
      await expect(page.locator('body')).toContainText('not visible to this user', {
        timeout: 20_000,
      });
      await expect(page.locator('body')).not.toContainText(`acl667 control ${STAMP}`);
    } finally {
      await ctx.close();
    }
  });

  test('the list path agrees with the page for the same grantee', async ({ browser }) => {
    // Both read paths run ONE SQL fragment (posts/read_rule.go), so a
    // disagreement between them means a splice site was missed rather
    // than that the rule is wrong. Asked with the tier filter, because
    // the unfiltered feed defaults to org-only — see the header note.
    const { ctx, page } = await granteeContext(browser);
    try {
      const list = await page.request.get('/api/v1/posts?visibility=explicit-share&limit=100');
      expect(list.status()).toBe(200);
      const ids = (((await list.json()) as { items?: Array<{ id: string }> }).items ?? []).map(
        (i) => i.id,
      );
      expect(ids, 'the granted post must be listed').toContain(fx!.sharedPostId);
      expect(ids, 'the ungranted post must not be listed').not.toContain(fx!.unsharedPostId);

      const one = await page.request.get(`/api/v1/posts/${fx!.sharedPostId}`);
      expect(one.status(), 'GET /posts/{id} must agree with the list').toBe(200);
      const other = await page.request.get(`/api/v1/posts/${fx!.unsharedPostId}`);
      expect(other.status(), 'the ungranted post must be refused').toBeGreaterThanOrEqual(400);
    } finally {
      await ctx.close();
    }
  });
});
