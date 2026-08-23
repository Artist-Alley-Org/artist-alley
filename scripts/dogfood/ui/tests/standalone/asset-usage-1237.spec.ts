// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1237 — "where has my file ended up", in the browser.
//
// `GET /assets/{id}/posts` shipped in #1232 with no caller anywhere in
// `web/src`, so the epic's fourth owner ruling was unreachable. This
// spec drives the surface that reaches it.
//
// # Why the ASSET OWNER here is not the admin
//
// The bootstrap admin holds `system.admin` and can therefore read every
// post on the instance, so `withheld_count` is permanently 0 for them:
// a suite driven as admin would assert the interesting half against a
// number that cannot move. So the asset's owner is an ORDINARY account
// and the admin is the OTHER author — the one whose private post the
// owner may not read. That is also the real shape of the ruling: an
// asset is personal storage, and the question is what happened to it in
// somebody else's hands.
//
// # The four states, and why each is here
//
//   used      one readable post + one withheld → both the card and the
//             sentence are on screen. THE STATE THAT MUST NOT REDIRECT:
//             /posts/by-asset/{id} goes straight to the post when
//             exactly one is visible (ADR 0070 §4), and doing that here
//             would skip past the only thing this page exists to say.
//   withheld  zero readable, one withheld → the sentence renders ALONE.
//             The state most likely to look broken, and the one a
//             "renders some cards" assertion would never reach.
//   unused    in no post at all → the zero state, not an error.
//   gate      the menu entry is absent on a card the caller does not
//             own, because the endpoint answers 404 there. Asserted
//             beside a PRESENT case in the same grid, or "absent" would
//             pass just as well on a menu that failed to open.
//
// # The assertion that keeps the shape honest
//
// `withheld_count` carries no ids, titles, authors or timestamps, on
// purpose: any of them would let the integer be walked back into the
// posts behind it (#902's count-leak shape). So this spec searches the
// WHOLE DOM of the owner's page for the withheld post's id and title and
// requires both to be absent. A DOM that names them has re-created the
// enumeration the API refused, whether or not anything renders visibly.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Browser, Page } from '@playwright/test';
import { LOGGED_OUT } from '../../helpers/auth';
import { ensureFixtureUser, restoreSelfRegistration } from '../../helpers/fixture-user';
import { tid } from '../../helpers/testids';

const STAMP = Date.now();
// Constant account, stamped CONTENT (#1198) — a per-run account
// accumulates forever and eventually reddens unrelated specs.
const OWNER_USER = 'usage1237_owner';
const OWNER_PASS = 'WhereDidItGo!1237';

const SECRET_TITLE = `usage1237 SECRET private ${STAMP}`;

interface Fixture {
  ownerRef: number;
  /** Owned by OWNER. In one readable post and one withheld one. */
  usedAssetId: string;
  /** Owned by OWNER. In a withheld post only. */
  withheldOnlyAssetId: string;
  /** Owned by OWNER. In no post at all. */
  unusedAssetId: string;
  /** Owned by ADMIN, public. The owner may READ it and may not ASK about it. */
  strangerAssetId: string;
  readablePostId: string;
  secretPostId: string;
  secretPostId2: string;
  priorSelfRegistration: unknown;
}

let fx: Fixture | undefined;

async function json(r: { json(): Promise<unknown> }): Promise<Record<string, unknown>> {
  return (await r.json()) as Record<string, unknown>;
}

/**
 * Upload one asset's bytes and create it through the real API.
 *
 * ⚠️ THE BYTES MUST BE UNIQUE PER ASSET. `POST /assets` runs the
 * operator's dedup pre-check (`upload.dedup_scope` /
 * `dedup_behavior`) and, for a repeat hash from the SAME user, returns
 * the EXISTING asset instead of making a new one. A fixture that
 * uploaded the same 1x1 PNG three times therefore got one asset back
 * three times, all three "different" assets were the same row, and
 * this spec's counts came out wrong in a way that read like a bug in
 * the code under test. Stamping the content is what makes three
 * assets three assets.
 *
 * Text assets, following the #1177 fixture: they occupy a tile with no
 * rendered variant, so nothing here races a preview worker.
 */
async function makeAsset(request: APIRequestContext, title: string): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`usage1237 fixture bytes for "${title}"`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), 'uploading fixture bytes').toBe(201);
  const fileHash = String((await json(up)).hash);

  const asset = await request.post('/api/v1/assets', {
    data: {
      title,
      asset_type: 2,
      file_extension: 'txt',
      file_hash: fileHash,
      original_filename: 'usage1237.txt',
    },
  });
  expect(asset.status(), `creating asset "${title}"`).toBe(201);
  const id = String((await json(asset)).id);
  expect(id, `asset "${title}" must be a NEW row, not a dedup hit`).toBeTruthy();
  return id;
}

test.describe('#1237 an owner can find out where their file is used', () => {
  // Serial: one fixture, one account, several assets and posts. Building
  // it per test would register nothing new and cost four uploads.
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ browser, request }) => {
    const { ref: ownerRef, priorSelfRegistration } = await ensureFixtureUser(browser, request, {
      username: OWNER_USER,
      password: OWNER_PASS,
      fullName: 'Usage 1237 owner',
    });

    // The OWNER's own assets, created in the owner's own context — the
    // endpoint gates on `assets.owner_user_ref`, so who uploads is the
    // whole fixture.
    const ownerCtx = await browser.newContext({ storageState: LOGGED_OUT });
    let usedAssetId: string;
    let withheldOnlyAssetId: string;
    let unusedAssetId: string;
    let readablePostId: string;
    try {
      const login = await ownerCtx.request.post('/api/v1/auth/login', {
        data: { username: OWNER_USER, password: OWNER_PASS },
        headers: { 'Content-Type': 'application/json' },
      });
      expect(login.ok(), 'signing the fixture owner in').toBe(true);

      usedAssetId = await makeAsset(ownerCtx.request, `usage1237 used ${STAMP}`);
      withheldOnlyAssetId = await makeAsset(ownerCtx.request, `usage1237 hidden ${STAMP}`);
      unusedAssetId = await makeAsset(ownerCtx.request, `usage1237 unused ${STAMP}`);

      // The readable half: a post the OWNER wrote themselves. Public, so
      // there is no doubt about why it is readable.
      const own = await ownerCtx.request.post('/api/v1/posts', {
        data: {
          title: `usage1237 my own post ${STAMP}`,
          description: 'usage1237 fixture',
          visibility: 'public',
          members: [{ asset_id: usedAssetId }],
        },
      });
      expect(own.status(), "creating the owner's own post").toBeLessThan(300);
      readablePostId = String((await json(own)).id);
    } finally {
      await ownerCtx.close();
    }

    // The withheld half, written by the ADMIN over the owner's files.
    // `private` is author-only, so the owner cannot read either of these
    // — which is exactly the situation the owner ruling is about, and
    // impossible to construct from the owner's own session.
    const mkSecret = async (title: string, asset: string): Promise<string> => {
      const r = await request.post('/api/v1/posts', {
        data: {
          title,
          description: 'usage1237 fixture',
          visibility: 'private',
          members: [{ asset_id: asset }],
        },
      });
      expect(r.status(), `creating hidden post "${title}"`).toBeLessThan(300);
      return String((await json(r)).id);
    };
    const secretPostId = await mkSecret(SECRET_TITLE, usedAssetId);
    const secretPostId2 = await mkSecret(`${SECRET_TITLE} two`, withheldOnlyAssetId);

    // A public asset the OWNER does not own. The gate case.
    const strangerAssetId = await makeAsset(request, `usage1237 stranger ${STAMP}`);

    // Four DISTINCT rows. Stated rather than assumed: the upload dedup
    // pre-check returns an existing asset for a repeat hash, and a
    // fixture that quietly collapsed to one row would make every count
    // below wrong while looking like a defect in the endpoint.
    const ids = [usedAssetId, withheldOnlyAssetId, unusedAssetId, strangerAssetId];
    expect(new Set(ids).size, 'the four fixture assets must be four rows').toBe(ids.length);

    fx = {
      ownerRef,
      usedAssetId,
      withheldOnlyAssetId,
      unusedAssetId,
      strangerAssetId,
      readablePostId,
      secretPostId,
      secretPostId2,
      priorSelfRegistration,
    };
  });

  test.afterAll(async ({ request }) => {
    if (!fx) return;
    for (const p of [fx.readablePostId, fx.secretPostId, fx.secretPostId2]) {
      await request.delete(`/api/v1/posts/${p}`).catch(() => undefined);
    }
    for (const a of [
      fx.usedAssetId,
      fx.withheldOnlyAssetId,
      fx.unusedAssetId,
      fx.strangerAssetId,
    ]) {
      await request.delete(`/api/v1/assets/${a}`).catch(() => undefined);
    }
    // The ACCOUNT survives on purpose — the next run reuses it (#1198).
    await restoreSelfRegistration(request, fx.priorSelfRegistration);
  });

  /** Sign the fixture owner in through the real form, in their own context. */
  async function ownerContext(browser: Browser) {
    const ctx = await browser.newContext({ storageState: LOGGED_OUT });
    const page = await ctx.newPage();
    await page.goto('/login');
    await page.locator(tid('login-username')).fill(OWNER_USER);
    await page.locator(tid('login-password')).fill(OWNER_PASS);
    await page.locator(tid('login-submit')).click();
    await page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 20_000 });
    return { ctx, page };
  }

  /** Open one AssetCard's ⋯ menu in the current grid and return the
   *  portaled panel. The card is addressed by `data-select-id`, which
   *  carries the ASSET id, so "which card" is never a guess about
   *  ordering. */
  async function openCardMenu(page: Page, assetId: string) {
    const card = page.locator(`[data-select-id="${assetId}"]`).first();
    await expect(card, `a card for asset ${assetId} must be on the page`).toBeVisible({
      timeout: 20_000,
    });
    await card.hover();
    await card.locator(tid('card-menu-trigger')).first().click();
    const panel = page.locator(tid('card-menu-panel'));
    await expect(panel, 'the ⋯ menu should have opened').toBeVisible({ timeout: 10_000 });
    return panel;
  }

  for (const viewport of [
    { name: '1080p', width: 1920, height: 1080 },
    { name: '390px', width: 390, height: 844 },
  ]) {
    test(`the used file shows its readable post AND the withheld count (${viewport.name})`, async ({
      browser,
    }, testInfo) => {
      const { ctx, page } = await ownerContext(browser);
      try {
        await page.setViewportSize({ width: viewport.width, height: viewport.height });
        await page.goto(`/assets/${fx!.usedAssetId}/usage`);

        await expect(page.locator(tid('asset-usage-heading'))).toBeVisible({ timeout: 20_000 });

        // The readable post is a real card, whole.
        await expect(page.locator('body')).toContainText(`usage1237 my own post ${STAMP}`);

        // NO REDIRECT. One visible item is exactly when the sentence
        // matters, so the page must still be the page.
        expect(new URL(page.url()).pathname).toBe(`/assets/${fx!.usedAssetId}/usage`);

        // The remainder, as prose.
        const withheld = page.locator(tid('asset-usage-withheld'));
        await expect(withheld).toBeVisible();
        await expect(withheld).toContainText('1 post');

        // ⚠️ The count is the WHOLE surface. Nothing in the DOM may name
        // the post it counted — not its id, not its title — or the
        // integer is walkable and the endpoint's whole shape is undone.
        const html = await page.content();
        expect(html, 'the withheld post’s TITLE leaked into the DOM').not.toContain(SECRET_TITLE);
        expect(html, 'the withheld post’s ID leaked into the DOM').not.toContain(fx!.secretPostId);

        // And it is not a control: no per-item element, nothing to click.
        await expect(page.locator(`${tid('asset-usage-withheld')} a`)).toHaveCount(0);
        await expect(page.locator(`${tid('asset-usage-withheld')} button`)).toHaveCount(0);

        await page.screenshot({
          path: testInfo.outputPath(`usage-used-${viewport.name}.png`),
          fullPage: true,
        });
      } finally {
        await ctx.close();
      }
    });

    test(`a file whose every post is withheld reads as a count, not as empty (${viewport.name})`, async ({
      browser,
    }, testInfo) => {
      // The state most likely to render wrong: zero cards and a positive
      // count. A page that printed only "also in 1 post" over an empty
      // grid — or nothing at all — reads as broken.
      const { ctx, page } = await ownerContext(browser);
      try {
        await page.setViewportSize({ width: viewport.width, height: viewport.height });
        await page.goto(`/assets/${fx!.withheldOnlyAssetId}/usage`);

        await expect(page.locator(tid('asset-usage-heading'))).toBeVisible({ timeout: 20_000 });
        const withheld = page.locator(tid('asset-usage-withheld'));
        await expect(withheld).toBeVisible();
        await expect(withheld).toContainText('1 post');

        // Not the zero state: "this file is in no post" would be a lie
        // here, and the two must never be confusable.
        await expect(page.locator(tid('asset-usage-none'))).toHaveCount(0);

        const html = await page.content();
        expect(html).not.toContain(SECRET_TITLE);
        expect(html).not.toContain(fx!.secretPostId2);

        await page.screenshot({
          path: testInfo.outputPath(`usage-all-withheld-${viewport.name}.png`),
          fullPage: true,
        });
      } finally {
        await ctx.close();
      }
    });
  }

  test('a file in no post gets the zero state, not an error', async ({ browser }, testInfo) => {
    const { ctx, page } = await ownerContext(browser);
    try {
      await page.goto(`/assets/${fx!.unusedAssetId}/usage`);
      await expect(page.locator(tid('asset-usage-heading'))).toBeVisible({ timeout: 20_000 });
      await expect(page.locator(tid('asset-usage-none'))).toBeVisible();
      // No count line at all — 0 withheld is not "0 posts you cannot
      // see", it is nothing to say.
      await expect(page.locator(tid('asset-usage-withheld'))).toHaveCount(0);
      await page.screenshot({
        path: testInfo.outputPath('usage-zero-state.png'),
        fullPage: true,
      });
    } finally {
      await ctx.close();
    }
  });

  test("the menu entry is on the owner's own card and not on a stranger's", async ({
    browser,
  }, testInfo) => {
    // Both cards in ONE grid, from one search, so "absent" cannot mean
    // "the menu never opened" or "the page did not load".
    const { ctx, page } = await ownerContext(browser);
    try {
      await page.setViewportSize({ width: 1920, height: 1080 });
      await page.goto(`/search?q=${encodeURIComponent(`usage1237`)}&types=asset`);

      const own = await openCardMenu(page, fx!.usedAssetId);
      await expect(
        own.locator(tid('card-usage')),
        'the owner must be offered "where is this used" on their own file',
      ).toBeVisible();
      await expect(own.locator(tid('card-usage'))).toHaveAttribute(
        'href',
        `/assets/${fx!.usedAssetId}/usage`,
      );
      await page.screenshot({
        path: testInfo.outputPath('usage-menu-owner.png'),
        fullPage: true,
      });
      await page.keyboard.press('Escape');

      const theirs = await openCardMenu(page, fx!.strangerAssetId);
      await expect(
        theirs.locator(tid('card-usage')),
        'a file the caller does not own must not offer it — the endpoint 404s there',
      ).toHaveCount(0);
      // The control: the menu DID open and does carry its ordinary
      // items, so the absence above is about the gate.
      await expect(theirs.locator('[role="menuitem"]').first()).toBeVisible();
      await page.screenshot({
        path: testInfo.outputPath('usage-menu-stranger.png'),
        fullPage: true,
      });
    } finally {
      await ctx.close();
    }
  });

  test("the route itself refuses a caller who is not the file's owner", async ({ browser }) => {
    // The menu item is a hint; this is the gate. And it must answer 404
    // rather than 403, so that it cannot be used to discover which
    // assets exist or whose files are in use.
    const { ctx, page } = await ownerContext(browser);
    try {
      const res = await page.request.get(`/api/v1/assets/${fx!.strangerAssetId}/posts`);
      expect(
        res.status(),
        'asking about somebody else’s file must 404, not 403 — a 403 confirms the asset exists',
      ).toBe(404);
    } finally {
      await ctx.close();
    }
  });
});
