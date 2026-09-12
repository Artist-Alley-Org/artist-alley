// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119: the real post editor, in the browser.
//
// # The old behaviour this pins, and why it is not a unit test
//
// "Edit post…" in the post menu was a `stubAction()` alert saying the
// feature was coming soon. Every column the editor writes has been accepted by
// `PATCH /posts/{id}` for sprints, so a spec that PATCHed a hand-written
// body would prove the API works and NOTHING about whether an author can
// reach it. The API already worked. The product was the hole.
//
// So arm 1 drives the REAL menu item on the REAL post page and asserts
// two things at once: no `window.alert` was raised, and the editing
// surface is on screen. That pair fails on `6e01d5ae`, where the alert
// fires and no surface exists, and it cannot be satisfied by a helper.
//
// # The four things that would ship wrong, each with an arm
//
//  1. A SURFACE THAT DOES NOT PERSIST. The edit is asserted by RE-READING
//     `GET /posts/{id}` after the save, and again after a page reload.
//     A dialog that echoed its own form back would pass a DOM assertion.
//  2. A STALE WRITE THAT WINS. Arm 2 moves the row out of band AFTER the
//     dialog opened, then saves. The refusal has to arrive AND the
//     stored title has to still be the other writer's. Deleting
//     `if_unchanged_since` from the body turns this red, which is the
//     point of it: the request bodies are sniffed off the wire so the
//     guard cannot quietly stop being sent.
//  3. AN EDIT THAT PUBLISHES. Arm 3 saves metadata on a DRAFT and on a
//     PUBLISHED post and asserts `draft` did not move either way, plus
//     that the save issued no publish/unpublish call at all.
//  4. AUTHORSHIP READ AS CURATION. Arm 5 is the mixed-authority case,
//     and it is the reason this file needs a seeded NON-ADMIN author:
//     membership is collection-owned (#882), so an author must be
//     offered removal from their own shelf and refused on a stranger's.
//     ⛔ A single permissive fixture passes whether the flag asks about
//     the collection or about the post.
//
// # Cardinalities, stated rather than sampled
//
// MEMBERS: N=0 (no pictures to choose from), N=1, N>=2 (choosing the
// second one persists). MEMBERSHIPS: N=0, N=1, N>=2 with DIFFERENT
// authority per row. A flag read off the first row passes N<=1.
//
// # What is NOT here
//
// The focal pair's own semantics (the marquee, the contain rung, the
// discard-on-cover-change) stay in post-cover-focal-1210.spec.ts, which
// now drives them THROUGH this editor. That file is the regression that
// the fold changed the surface and not the behaviour; duplicating its
// drag here would be a second oracle for one rule.

import type { APIRequestContext, Browser, Page } from '@playwright/test';
import { test, expect } from '../../helpers/test';
import { LOGGED_OUT } from '../../helpers/auth';
import { requireSeededPrincipal, seededPrincipal } from '../../helpers/seeded-principal';
import { tid } from '../../helpers/testids';

const STAMP = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

/** The NON-ADMIN author of the mixed-authority fixture. See the
 *  catalogue entry's `why`: the bootstrap admin holds system.admin, which
 *  canMutateCollection admits on every collection, so an admin author
 *  makes arm 5 vacuous. */
const AUTHOR = seededPrincipal('ilse.varga');

async function body(r: { json(): Promise<unknown> }): Promise<Record<string, unknown>> {
  return (await r.json()) as Record<string, unknown>;
}

/** One asset, with bytes unique to its title.
 *
 *  ⚠️ UNIQUE BYTES ARE NOT DECORATION. `POST /assets` runs the operator's
 *  dedup pre-check and returns the EXISTING asset for a repeat hash from
 *  the same user, so a fixture that uploaded identical bytes twice gets
 *  ONE row back twice, and every cardinality assertion below would be
 *  measuring a fixture bug. Same trap asset-usage-1237 records.
 *
 *  Text assets: they occupy a member slot with no rendered variant, so
 *  nothing here races a preview worker. That also makes them the right
 *  fixture for the unframable branch (no contain rung, ADR 0088).
 */
async function makeAsset(request: APIRequestContext, title: string): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`post-editor-1119 bytes for "${title}"`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), `uploading bytes for "${title}"`).toBe(201);
  const hash = String((await body(up)).hash);

  const res = await request.post('/api/v1/assets', {
    data: {
      title,
      asset_type: 2,
      file_extension: 'txt',
      file_hash: hash,
      original_filename: 'post-editor-1119.txt',
    },
  });
  expect(res.status(), `creating asset "${title}"`).toBe(201);
  return String((await body(res)).id);
}

async function makePost(
  request: APIRequestContext,
  data: Record<string, unknown>,
): Promise<string> {
  const res = await request.post('/api/v1/posts', { data });
  expect(res.status(), `creating post ${JSON.stringify(data.title)}`).toBeLessThan(300);
  return String((await body(res)).id);
}

async function makeCollection(request: APIRequestContext, name: string): Promise<string> {
  const res = await request.post('/api/v1/collections', {
    data: { name, description: 'post-editor-1119 fixture', visibility: 'public' },
  });
  expect(res.status(), `creating collection "${name}"`).toBeLessThan(300);
  return String((await body(res)).id);
}

/** The post as the SERVER has it. Every persistence assertion in this
 *  file reads through here, never off the DOM the dialog just rendered
 *  and never off the PATCH response: a handler that echoed its own write
 *  passes a body assertion on the bug. */
async function stored(request: APIRequestContext, postId: string) {
  const res = await request.get(`/api/v1/posts/${postId}`);
  expect(res.ok(), `re-reading post ${postId}`).toBeTruthy();
  const p = (await res.json()) as {
    title: string;
    description: string;
    visibility: string;
    draft: boolean;
    tags: string[];
    cover_asset_id?: string | null;
    cover_focal_x?: number | null;
    cover_focal_y?: number | null;
    updated_at: string;
  };
  return p;
}

/** Whether a locator is visible ALL THE WAY UP.
 *
 *  `checkVisibility()` and not the element's own computed style: effective
 *  visibility is an ancestor question, and a child's `opacity: 1` inside a
 *  hidden overlay proves nothing. Playwright's `toBeVisible` agrees with
 *  this, and this exists for the places where a NUMBER is wanted as well
 *  as an assertion. */
async function reallyVisible(page: Page, testid: string): Promise<boolean> {
  return page
    .getByTestId(testid)
    .first()
    .evaluate((el: Element) =>
      (el as HTMLElement & { checkVisibility?: () => boolean }).checkVisibility?.() ?? false,
    );
}

/** The browser's own report of a failed subresource.
 *
 *  Carries NO url, which is the whole problem with matching on it. See
 *  [watchPage]. */
const SUBRESOURCE_FAILURE = /Failed to load resource/i;

/** A preview rung that may legitimately not exist yet. */
const PREVIEW_VARIANT = /\/variants\//;

/** Every `window.alert` the page raised, every uncaught JS error, and
 *  every request the PAGE made that answered 400 or worse.
 *
 *  ⛔ THE ALERT LIST IS ARM 1's OTHER HALF. Playwright auto-dismisses
 *  dialogs when nothing is listening, which means the stub's alert would
 *  vanish silently and the test would fail on some LATER assertion with a
 *  confusing message. Listening records it, so "the stub is still there"
 *  is its own sentence.
 *
 *  ⚠️ WHY THE CONSOLE LIST IS NOT SIMPLY "EVERY ERROR", and this is a
 *  measurement rather than a concession. The browser logs every failed
 *  subresource as a console error, and on THIS surface one class of them
 *  is expected: the cover picker draws `/variants/col` for each member,
 *  exactly as PostCard does, and a member whose raster pass has not
 *  produced that rung answers 404. Measured against a post of two freshly
 *  uploaded text members: two console errors, and the responses behind
 *  them were `404 /api/v1/assets/{id}/variants/col` twice, nothing else.
 *
 *  That is also why an unfiltered assertion passed on a workstation and
 *  failed on CI. It is a RACE with the preview worker, not a difference of
 *  opinion: a warm stack often has the rung by the time the dialog opens
 *  and a fresh database never does. Asserting zero console errors was
 *  therefore asserting the worker had drained, which is not what this
 *  spec is about.
 *
 *  ⛔ IT IS ATTRIBUTED, NOT DROPPED, because the console message carries no
 *  url and matching its text alone would blind the check to a real error
 *  that happened to be phrased the same way. Three lists, and every
 *  assertion site checks all three:
 *
 *    * `errors`: uncaught JS exceptions, NEVER filtered, plus any console
 *      error that is not a subresource line.
 *    * `httpErrors`: every >= 400 the page provoked EXCEPT a preview
 *      variant. So the 404s excused above still have to be that exact
 *      class, and a 500 on the post read or a 403 on the membership list
 *      is still a failure.
 *    * `alerts`: the stub.
 */
function watchPage(page: Page) {
  const alerts: string[] = [];
  const errors: string[] = [];
  const httpErrors: string[] = [];
  page.on('dialog', (d) => {
    alerts.push(d.message());
    void d.dismiss().catch(() => undefined);
  });
  page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    if (SUBRESOURCE_FAILURE.test(m.text())) return;
    errors.push(`console: ${m.text()}`);
  });
  page.on('response', (r) => {
    if (r.status() < 400) return;
    let path: string;
    try {
      path = new URL(r.url()).pathname;
    } catch {
      path = r.url();
    }
    if (PREVIEW_VARIANT.test(path)) return;
    httpErrors.push(`${r.status()} ${path}`);
  });
  return { alerts, errors, httpErrors };
}

/** Open the post page and the real editor through the real menu item.
 *
 *  ⛔ THIS IS THE INTERACTIVE PATH AND IT IS NOT OPTIONAL. The whole
 *  defect was that the menu item did not reach a surface, so the surface
 *  is reached the way an author reaches it: the post page, the "Post
 *  actions" menu, the item. No route is visited that an author would not
 *  visit, and no state is set up in the page. */
async function openEditor(page: Page, postId: string) {
  await page.goto(`/posts/${postId}`);
  await page.locator('[aria-label="Post actions"]').first().click();
  await page.getByTestId('post-edit').click();
  await expect(page.getByTestId('post-edit-body')).toBeVisible({ timeout: 15_000 });
}

/** Sign a seeded principal in through the real form, in its own context. */
async function principalPage(browser: Browser, user: { username: string; password: string }) {
  const ctx = await browser.newContext({ storageState: LOGGED_OUT });
  const page = await ctx.newPage();
  await page.goto('/login');
  await page.locator(tid('login-username')).fill(user.username);
  await page.locator(tid('login-password')).fill(user.password);
  await page.locator(tid('login-submit')).click();
  await page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 20_000 });
  return { ctx, page };
}

// =====================================================================
// The metadata half. Driven as the bootstrap admin, whose own posts these
// are: authorship is what the PATCH gate asks about and the admin is the
// author here, so nothing in this describe depends on a capability.
// =====================================================================

test.describe('#1119 the post editor persists what an author changes', () => {
  test.describe.configure({ mode: 'serial' });

  let assetA = '';
  let assetB = '';
  /** Assets this describe made that are not assetA/assetB: the one the
   *  N=0 fixture evicted. Tracked so teardown is by ID rather than by a
   *  naming rule (#1245's whole argument). */
  const strayAssets: string[] = [];
  let publishedPost = '';
  let draftPost = '';
  let emptyPost = '';
  let onePost = '';

  test.beforeAll(async ({ request }) => {
    assetA = await makeAsset(request, `post-editor-1119 alpha ${STAMP}`);
    assetB = await makeAsset(request, `post-editor-1119 bravo ${STAMP}`);
    expect(assetA, 'two fixture assets must be two rows').not.toBe(assetB);

    publishedPost = await makePost(request, {
      title: `pe1119 published ${STAMP}`,
      description: 'before',
      visibility: 'org-only',
      tags: ['pe1119seed'],
      members: [{ asset_id: assetA }, { asset_id: assetB }],
    });
    draftPost = await makePost(request, {
      title: `pe1119 draft ${STAMP}`,
      description: 'before',
      visibility: 'org-only',
      draft: true,
      members: [{ asset_id: assetA }],
    });
    // N=0 MEMBERS, AND IT HAS TO BE REACHED THE WAY A PERSON REACHES IT.
    // `POST /posts` refuses an empty `members` ("members: at least one
    // asset required", handler.go), so a memberless post is not something
    // that can be created: it is what a post BECOMES when its author takes
    // the last file out of it. That is the state the editor has to be able
    // to represent, so the fixture is built by removing the member rather
    // than by asserting the API would let it be created in that shape.
    const emptyAsset = await makeAsset(request, `pe1119 soon removed ${STAMP}`);
    emptyPost = await makePost(request, {
      title: `pe1119 memberless ${STAMP}`,
      description: 'before',
      visibility: 'org-only',
      members: [{ asset_id: emptyAsset }],
    });
    const evict = await request.delete(`/api/v1/posts/${emptyPost}/assets/${emptyAsset}`);
    expect(evict.status(), 'emptying the post of its one member').toBe(204);
    strayAssets.push(emptyAsset);
    // ASSERTED, not assumed. If removing the last member were refused the
    // post would still have one, the N=0 arm below would be testing N=1,
    // and it would pass for the wrong reason.
    const emptied = await request.get(`/api/v1/posts/${emptyPost}`);
    const emptiedMembers =
      ((await emptied.json()) as { members?: unknown[] }).members ?? [];
    expect(emptiedMembers.length, 'the N=0 fixture must really hold no members').toBe(0);
    onePost = await makePost(request, {
      title: `pe1119 single ${STAMP}`,
      description: 'before',
      visibility: 'org-only',
      members: [{ asset_id: assetA }],
    });

    // The draft has to actually BE a draft, and the published one
    // published. Asserted rather than assumed: arm 3's whole subject is
    // that a save does not move this flag, and a fixture whose two posts
    // were both published would pass it twice for the wrong reason.
    expect((await stored(request, draftPost)).draft, 'the draft fixture must be a draft').toBe(true);
    expect(
      (await stored(request, publishedPost)).draft,
      'the published fixture must not be a draft',
    ).toBe(false);
  });

  test.afterAll(async ({ request }) => {
    for (const p of [publishedPost, draftPost, emptyPost, onePost]) {
      if (p) await request.delete(`/api/v1/posts/${p}`).catch(() => undefined);
    }
    for (const a of [assetA, assetB, ...strayAssets]) {
      if (a) await request.delete(`/api/v1/assets/${a}`).catch(() => undefined);
    }
  });

  // ── ARM 1: the headline. RED on 6e01d5ae. ──────────────────────────
  test('Edit post opens a real editor, not the stub, and the save reaches the server', async ({
    page,
    request,
  }, testInfo) => {
    const watched = watchPage(page);
    const newTitle = `pe1119 renamed ${STAMP}`;

    await page.goto(`/posts/${publishedPost}`);
    const urlBefore = new URL(page.url()).pathname;
    await page.locator('[aria-label="Post actions"]').first().click();
    await page.getByTestId('post-edit').click();

    // ⛔ THE STUB, NAMED. On current dev `alerts` holds exactly one
    // message, the one `stubAction()` builds, and `post-edit-body` never
    // exists. Both halves are asserted because either one alone could be
    // bought cheaply: an alert could be removed without building
    // anything, and a surface could be built beside an alert nobody took
    // out.
    await expect(page.getByTestId('post-edit-body')).toBeVisible({ timeout: 15_000 });
    expect(
      watched.alerts,
      'Edit post must open a surface, not raise window.alert. The stub is still wired',
    ).toEqual([]);

    // NO UNRELATED NAVIGATION. An "editor" that was really a route change
    // to some edit page would satisfy the visibility assertion.
    expect(new URL(page.url()).pathname, 'the editor opens in place').toBe(urlBefore);

    await page.getByTestId('post-edit-title').fill(newTitle);
    await page.getByTestId('post-edit-description').fill('after');
    await page.getByTestId('post-edit-tag-input').fill('pe1119added');
    await page.getByTestId('post-edit-tag-input').press('Enter');
    await page.getByTestId('post-edit-save').click();
    await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });

    // THE SERVER'S ANSWER, not the dialog's.
    const after = await stored(request, publishedPost);
    expect(after.title, 'the new title must be stored').toBe(newTitle);
    expect(after.description).toBe('after');
    expect(after.tags.sort()).toEqual(['pe1119added', 'pe1119seed']);

    // AND IT SURVIVES A RELOAD, which is the half a client-side patch
    // cannot fake.
    await page.reload();
    await expect(page.locator('body')).toContainText(newTitle, { timeout: 20_000 });

    expect(watched.errors, 'no console or runtime errors').toEqual([]);
    expect(watched.httpErrors, 'no failed request other than a missing preview rung').toEqual([]);
    await page.screenshot({
      path: testInfo.outputPath('post-editor-desktop.png'),
      fullPage: true,
    });
  });

  // ── ARM 2: the stale write. ────────────────────────────────────────
  test('a stale save is refused and does not overwrite the newer edit', async ({
    page,
    request,
  }) => {
    // Sniff the PATCH bodies. ⛔ THIS IS WHAT MAKES THE ARM A PIN RATHER
    // THAN A COINCIDENCE: if `if_unchanged_since` stopped being sent, the
    // 409 would stop arriving and a "did the save fail?" assertion could
    // be made to pass again by some other refusal. The guard's PRESENCE
    // on the wire is asserted directly.
    const patches: Array<Record<string, unknown>> = [];
    page.on('request', (req) => {
      if (req.method().toUpperCase() !== 'PATCH') return;
      if (!new URL(req.url()).pathname.endsWith(`/posts/${onePost}`)) return;
      try {
        patches.push(JSON.parse(req.postData() ?? '{}') as Record<string, unknown>);
      } catch {
        patches.push({});
      }
    });

    const beforeOpen = await stored(request, onePost);
    await openEditor(page, onePost);

    // A SECOND WRITER, after the dialog took its snapshot. This is the
    // real stale-write path: the row moves, the open form does not know,
    // and the form's baseline is now behind.
    const theirTitle = `pe1119 theirs ${STAMP}`;
    const theirs = await request.patch(`/api/v1/posts/${onePost}`, {
      data: { title: theirTitle, if_unchanged_since: beforeOpen.updated_at },
    });
    expect(theirs.status(), "the other writer's own save must land").toBe(200);

    await page.getByTestId('post-edit-title').fill(`pe1119 mine ${STAMP}`);
    await page.getByTestId('post-edit-save').click();

    // The refusal is SHOWN, and the dialog stays open holding the edits.
    await expect(page.getByTestId('post-edit-conflict')).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId('post-edit-body')).toBeVisible();

    // ⛔ AND NOTHING WAS OVERWRITTEN. Asserted on the PERSISTED row: a
    // 409 shown over a write that went through anyway is the bug wearing
    // the fix.
    expect(
      (await stored(request, onePost)).title,
      "the other writer's title must survive a refused save",
    ).toBe(theirTitle);

    expect(patches.length, 'exactly one PATCH was attempted').toBe(1);
    expect(
      patches[0].if_unchanged_since,
      'the editor must send if_unchanged_since; without it this save would ' +
        'have silently clobbered the newer edit',
    ).toBe(beforeOpen.updated_at);

    // The author can then take their own edit forward deliberately. This
    // is the other half of the guard: it must be a decision, not a wall.
    await page.getByTestId('post-edit-conflict-ack').click();
    await page.getByTestId('post-edit-save').click();
    await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });
    expect((await stored(request, onePost)).title).toBe(`pe1119 mine ${STAMP}`);
    expect(patches.length, 'the retry is a second PATCH').toBe(2);
    expect(
      patches[1].if_unchanged_since,
      'the retry carries the timestamp the refusal reported, not the stale one',
    ).not.toBe(beforeOpen.updated_at);
  });

  // ── ARM 3: publication is untouched by a metadata save. ────────────
  for (const kind of ['draft', 'published'] as const) {
    test(`saving metadata on a ${kind} post does not move its publication`, async ({
      page,
      request,
    }) => {
      const postId = kind === 'draft' ? draftPost : publishedPost;
      const wasDraft = (await stored(request, postId)).draft;
      expect(wasDraft, `the ${kind} fixture`).toBe(kind === 'draft');

      // ⛔ NO PUBLICATION CALL AT ALL. The stronger claim: not merely
      // that the flag happens to be unchanged, but that the save never
      // touched the endpoints that could change it.
      const publicationCalls: string[] = [];
      page.on('request', (req) => {
        const p = new URL(req.url()).pathname;
        if (p.endsWith('/publish') || p.endsWith('/unpublish')) publicationCalls.push(p);
      });

      await openEditor(page, postId);
      await expect(page.getByTestId('post-edit-publication-state')).toBeVisible();
      await page.getByTestId('post-edit-description').fill(`edited while ${kind} ${STAMP}`);
      await page.getByTestId('post-edit-save').click();
      await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });

      const after = await stored(request, postId);
      expect(after.description).toBe(`edited while ${kind} ${STAMP}`);
      expect(
        after.draft,
        `editing a ${kind} post must not change whether it is published`,
      ).toBe(wasDraft);
      expect(publicationCalls, 'Save must not call the publication endpoints').toEqual([]);
    });
  }

  // The deliberate move, through the shipped endpoint, from inside the
  // editor. Included because arm 3 asserts an absence, and an absence
  // also holds on a control that does not work.
  test('the editor can publish a draft, and only through the publication endpoint', async ({
    page,
    request,
  }) => {
    const scratch = await makePost(request, {
      title: `pe1119 publish me ${STAMP}`,
      description: '',
      visibility: 'org-only',
      draft: true,
      members: [{ asset_id: assetA }],
    });
    try {
      const calls: string[] = [];
      page.on('request', (req) => {
        const p = new URL(req.url()).pathname;
        const m = req.method().toUpperCase();
        if (m === 'POST' && (p.endsWith('/publish') || p.endsWith('/unpublish'))) calls.push(p);
        // A PATCH carrying state_id would be the thing ADR 0091 forbids.
        if (m === 'PATCH' && p.endsWith(`/posts/${scratch}`)) {
          const b = JSON.parse(req.postData() ?? '{}') as Record<string, unknown>;
          expect(b.state_id, 'the editor must never PATCH state_id').toBeUndefined();
        }
      });

      await openEditor(page, scratch);
      await page.getByTestId('post-edit-publish-toggle').click();
      // The state line is re-read from the server, so waiting for the
      // sentence to change is waiting for the round trip.
      await expect(page.getByTestId('post-edit-publication-state')).not.toContainText('draft', {
        timeout: 15_000,
      });
      expect(calls, 'publication goes through /publish').toEqual([`/api/v1/posts/${scratch}/publish`]);
      expect((await stored(request, scratch)).draft).toBe(false);

      // ⚠️ AND THE FORM STILL SAVES. The transition writes
      // `updated_at = NOW()`, so an editor that did not advance its own
      // baseline would refuse the author's next save as somebody else's
      // edit, a conflict the author has no way to understand.
      await page.getByTestId('post-edit-title').fill(`pe1119 published then renamed ${STAMP}`);
      await page.getByTestId('post-edit-save').click();
      await expect(page.getByTestId('post-edit-conflict')).toBeHidden();
      await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });
      expect((await stored(request, scratch)).title).toBe(
        `pe1119 published then renamed ${STAMP}`,
      );
    } finally {
      await request.delete(`/api/v1/posts/${scratch}`).catch(() => undefined);
    }
  });

  // ── ARM 4: member cardinality. ─────────────────────────────────────
  test('N=0 members: the cover section says so and the rest of the form still saves', async ({
    page,
    request,
  }) => {
    const before = await stored(request, emptyPost);
    // ⚠️ THE FIXTURE STILL HAS A COVER POINTER, and that is the honest
    // shape of this state rather than an inconvenience. `POST /posts`
    // pinned the one member as the cover, and taking the member out does
    // not un-pin it, so a memberless post carries a `cover_asset_id`
    // naming an asset that is no longer in it. The editor's job here is to
    // leave it exactly alone: there is nothing to choose from, so there is
    // nothing to say.
    const coverPatches: Array<Record<string, unknown>> = [];
    page.on('request', (req) => {
      if (req.method().toUpperCase() !== 'PATCH') return;
      if (!new URL(req.url()).pathname.endsWith(`/posts/${emptyPost}`)) return;
      try {
        coverPatches.push(JSON.parse(req.postData() ?? '{}') as Record<string, unknown>);
      } catch {
        coverPatches.push({});
      }
    });

    await openEditor(page, emptyPost);
    await expect(page.getByTestId('post-cover-no-members')).toBeVisible();
    expect(await reallyVisible(page, 'post-cover-no-members')).toBe(true);
    await expect(page.getByTestId('post-cover-choice')).toHaveCount(0);

    await page.getByTestId('post-edit-title').fill(`pe1119 memberless renamed ${STAMP}`);
    await page.getByTestId('post-edit-save').click();
    await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });

    const after = await stored(request, emptyPost);
    expect(after.title).toBe(`pe1119 memberless renamed ${STAMP}`);
    // ⛔ AND NOTHING ABOUT THE COVER WAS SENT OR CHANGED. Asserted on the
    // WIRE as well as on the row: a body that always carried
    // `cover_asset_id` would write a value nobody chose, and with no
    // members to resolve one it would carry `null` and be refused.
    expect(coverPatches.length, 'exactly one PATCH').toBe(1);
    for (const key of [
      'cover_asset_id',
      'cover_focal_x',
      'cover_focal_y',
      'clear_cover_focal',
    ]) {
      expect(coverPatches[0][key], `${key} must not be in a rename's body`).toBeUndefined();
    }
    expect(after.cover_asset_id ?? null, 'the stored cover pointer is untouched').toBe(
      before.cover_asset_id ?? null,
    );
    expect(after.cover_focal_x ?? null).toBe(before.cover_focal_x ?? null);
  });

  test('N=1 member: one choice, already selected, and renaming writes no cover', async ({
    page,
    request,
  }) => {
    const before = await stored(request, onePost);
    await openEditor(page, onePost);
    await expect(page.getByTestId('post-cover-choice')).toHaveCount(1);
    await expect(page.getByTestId('post-cover-choice')).toHaveAttribute('aria-pressed', 'true');

    await page.getByTestId('post-edit-description').fill(`n1 ${STAMP}`);
    await page.getByTestId('post-edit-save').click();
    await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });

    // ⛔ THE UNTOUCHED-COVER RULE. The section is on screen for every
    // edit now, so "always send cover_asset_id" would make renaming a
    // post PIN its cover, a write the author did not ask for. The
    // stored value must be exactly what it was.
    const after = await stored(request, onePost);
    expect(after.cover_asset_id ?? null, 'a rename must not pin the cover').toBe(
      before.cover_asset_id ?? null,
    );
  });

  test('N>=2 members: choosing the other picture persists it as the cover', async ({
    page,
    request,
  }) => {
    await openEditor(page, publishedPost);
    const choices = page.getByTestId('post-cover-choice');
    await expect(choices).toHaveCount(2);

    // Address the choice by ASSET ID, never by index: "the second tile"
    // is a claim about ordering that a member reorder would quietly
    // invalidate.
    const before = await stored(request, publishedPost);
    const target = (before.cover_asset_id ?? assetA) === assetA ? assetB : assetA;
    await page.locator(`[data-testid="post-cover-choice"][data-asset-id="${target}"]`).click();

    // ⚠️ THE FRAMABLE BRANCH IS ASKED, NOT ASSUMED, and the first draft of
    // this assertion is why. It asserted the unframable note on the
    // grounds that "a .txt has no contain rung", which is a guess about
    // what the raster worker does to a text asset rather than something
    // this spec establishes. The rule under test is not "text is
    // unframable": it is that the section offers a marquee EXACTLY when
    // the chosen picture has a contain rung to draw it over, and says so
    // plainly otherwise (ADR 0088's fall-back-rather-than-blank rule, the
    // editor half of it). So the member's own `ladder_available` decides
    // which of the two must be on screen, and the other must be absent.
    const memberLadder = await request
      .get(`/api/v1/posts/${publishedPost}`)
      .then(async (r) => {
        const p = (await r.json()) as {
          members?: Array<{ asset_id: string; asset?: { ladder_available?: boolean } }>;
        };
        return (p.members ?? []).find((m) => m.asset_id === target)?.asset?.ladder_available === true;
      });
    if (memberLadder) {
      await expect(page.getByTestId('post-crop-marquee')).toBeVisible();
      await expect(page.getByTestId('post-cover-unframable')).toHaveCount(0);
    } else {
      await expect(page.getByTestId('post-cover-unframable')).toBeVisible();
      await expect(page.getByTestId('post-crop-marquee')).toHaveCount(0);
    }

    await page.getByTestId('post-edit-save').click();
    await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });

    const after = await stored(request, publishedPost);
    expect(after.cover_asset_id, 'the chosen cover must be stored').toBe(target);
    // Changing the cover DISCARDS the framing (#1333). There was none to
    // discard here and nothing was dragged, so the pair is still null on
    // both branches above rather than a half pair, which the column CHECK
    // would refuse anyway. The drag itself is
    // post-cover-focal-1210.spec.ts's subject.
    expect(after.cover_focal_x ?? null).toBeNull();
    expect(after.cover_focal_y ?? null).toBeNull();
  });

  // ── Both widths. ───────────────────────────────────────────────────
  for (const viewport of [
    { name: '1080p', width: 1920, height: 1080 },
    { name: '390px', width: 390, height: 844 },
  ]) {
    test(`every in-scope control is reachable and unclipped (${viewport.name})`, async ({
      page,
    }, testInfo) => {
      const watched = watchPage(page);
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await openEditor(page, publishedPost);

      // EFFECTIVE visibility, ancestor included, for each control the
      // slice promises. `checkVisibility()` rather than a computed style:
      // a child inside a collapsed or zero-opacity parent reports its own
      // `opacity: 1` quite happily.
      for (const id of [
        'post-edit-title',
        'post-edit-description',
        'post-edit-visibility',
        'post-edit-tags',
        'post-edit-publication',
        'post-edit-cover-section',
        'post-edit-collections',
        'post-edit-save',
        'post-edit-cancel',
      ]) {
        expect(await reallyVisible(page, id), `${id} must be visible at ${viewport.name}`).toBe(
          true,
        );
      }

      // NO HORIZONTAL OVERFLOW on the page itself.
      const overflow = await page.evaluate(() => ({
        scroll: document.documentElement.scrollWidth,
        client: document.documentElement.clientWidth,
      }));
      expect(
        overflow.scroll,
        `the page must not scroll sideways at ${viewport.name}`,
      ).toBeLessThanOrEqual(overflow.client + 1);

      // THE PRIMARY ACTION IS NOT CLIPPED. Measured against the viewport
      // rather than asserted as "visible": a Save button pushed below the
      // fold of a 844px-tall window is visible to Playwright and useless
      // to a person.
      const save = await page.getByTestId('post-edit-save').boundingBox();
      expect(save, 'Save must have a box').not.toBeNull();
      expect(save!.y + save!.height, `Save must sit inside the ${viewport.name} viewport`)
        .toBeLessThanOrEqual(viewport.height);
      expect(save!.x + save!.width).toBeLessThanOrEqual(viewport.width);

      // And it is genuinely usable: Cancel closes without writing.
      const before = await page.getByTestId('post-edit-title').inputValue();
      await page.getByTestId('post-edit-cancel').click();
      await expect(page.getByTestId('post-edit-body')).toBeHidden();
      expect(before.length, 'the title field had the stored value in it').toBeGreaterThan(0);

      expect(watched.alerts, 'no stub alert').toEqual([]);
      expect(watched.errors, `no console or runtime errors at ${viewport.name}`).toEqual([]);
      expect(
        watched.httpErrors,
        `no failed request other than a missing preview rung at ${viewport.name}`,
      ).toEqual([]);
      await page.screenshot({
        path: testInfo.outputPath(`post-editor-${viewport.name}.png`),
        fullPage: true,
      });
    });
  }
});

// =====================================================================
// ARM 5: membership, and the authority that is NOT the author's.
//
// Driven as a seeded ORDINARY account. Read the fixture note above: an
// admin author can mutate every collection on the instance, so this whole
// describe would pass on a build that granted removal from authorship.
// =====================================================================

test.describe('#1119 membership removal follows the collection, not the post', () => {
  test.describe.configure({ mode: 'serial' });

  let authorRef = 0;
  let authorAsset = '';
  /** The author's own post, public so the admin can pin it. */
  let postId = '';
  /** Owned by the AUTHOR. `can_remove` must be true. */
  let mineId = '';
  /** Owned by the ADMIN. `can_remove` must be false FOR THE AUTHOR. */
  let theirsId = '';
  let mineName = '';
  let theirsName = '';

  test.beforeAll(async ({ browser, request }) => {
    authorRef = await requireSeededPrincipal(browser, AUTHOR.username);

    const ctx = await browser.newContext({ storageState: LOGGED_OUT });
    try {
      const login = await ctx.request.post('/api/v1/auth/login', {
        data: { username: AUTHOR.username, password: AUTHOR.password },
        headers: { 'Content-Type': 'application/json' },
      });
      expect(login.ok(), 'signing the fixture author in').toBe(true);

      authorAsset = await makeAsset(ctx.request, `pe1119 author asset ${STAMP}`);
      // PUBLIC so the admin's pin passes the member gate. That gate is
      // the other half of #882 and is not what this arm is about.
      postId = await makePost(ctx.request, {
        title: `pe1119 mixed ${STAMP}`,
        description: '',
        visibility: 'public',
        members: [{ asset_id: authorAsset }],
      });
      mineName = `pe1119 my shelf ${STAMP}`;
      mineId = await makeCollection(ctx.request, mineName);
      const ownPin = await ctx.request.post(`/api/v1/collections/${mineId}/posts`, {
        data: { post_id: postId },
      });
      expect(ownPin.status(), "pinning the post into the author's own shelf").toBe(204);
    } finally {
      await ctx.close();
    }

    // The stranger's shelf, made and pinned by the ADMIN. Impossible to
    // construct from the author's own session, which is the point.
    theirsName = `pe1119 their shelf ${STAMP}`;
    theirsId = await makeCollection(request, theirsName);
    const pin = await request.post(`/api/v1/collections/${theirsId}/posts`, {
      data: { post_id: postId },
    });
    expect(pin.status(), "pinning the author's post into the admin's shelf").toBe(204);

    expect(mineId, 'two shelves must be two rows').not.toBe(theirsId);
  });

  test.afterAll(async ({ request }) => {
    for (const c of [mineId, theirsId]) {
      if (c) await request.delete(`/api/v1/collections/${c}`).catch(() => undefined);
    }
    if (postId) await request.delete(`/api/v1/posts/${postId}`).catch(() => undefined);
    if (authorAsset) await request.delete(`/api/v1/assets/${authorAsset}`).catch(() => undefined);
    // The ACCOUNT is the seed's (#1270): nothing to remove, no instance
    // config to put back.
  });

  /** Whether the admin's shelf still holds the post, asked as the ADMIN.
   *  The author cannot answer this question about somebody else's
   *  collection, and asking it from the wrong session is how "it was not
   *  disturbed" comes to mean "I could not see it either way". */
  async function theirShelfHolds(request: APIRequestContext): Promise<boolean> {
    const res = await request.get(`/api/v1/collections/${theirsId}/posts`);
    expect(res.ok(), "reading the admin's shelf as the admin").toBeTruthy();
    const list = (await res.json()) as { items?: Array<{ id: string }> };
    return (list.items ?? []).some((p) => p.id === postId);
  }

  // N=0.
  test('N=0 memberships reads as the zero state', async ({ browser }) => {
    const { ctx, page } = await principalPage(browser, AUTHOR);
    try {
      // A second post by the same author, on no shelf at all.
      // One member, because `POST /posts` requires one. This arm is about
      // membership cardinality (N=0 COLLECTIONS), not member cardinality.
      const bareAsset = await makeAsset(ctx.request, `pe1119 unshelved asset ${STAMP}`);
      const bare = await makePost(ctx.request, {
        title: `pe1119 unshelved ${STAMP}`,
        description: '',
        visibility: 'public',
        members: [{ asset_id: bareAsset }],
      });
      try {
        await openEditor(page, bare);
        await expect(page.getByTestId('post-edit-collections-none')).toBeVisible({
          timeout: 15_000,
        });
        await expect(page.getByTestId('post-edit-membership')).toHaveCount(0);
        await expect(page.getByTestId('post-edit-collections-withheld')).toHaveCount(0);
      } finally {
        await ctx.request.delete(`/api/v1/posts/${bare}`).catch(() => undefined);
        await ctx.request.delete(`/api/v1/assets/${bareAsset}`).catch(() => undefined);
      }
    } finally {
      await ctx.close();
    }
  });

  // N=1, and the removal actually persists.
  test("N=1 membership on the author's own shelf can be removed, and the row goes", async ({
    browser,
    request,
  }) => {
    const { ctx, page } = await principalPage(browser, AUTHOR);
    try {
      const soloAsset = await makeAsset(ctx.request, `pe1119 solo asset ${STAMP}`);
      const solo = await makePost(ctx.request, {
        title: `pe1119 solo shelved ${STAMP}`,
        description: '',
        visibility: 'public',
        members: [{ asset_id: soloAsset }],
      });
      const soloShelf = await makeCollection(ctx.request, `pe1119 solo shelf ${STAMP}`);
      try {
        const pin = await ctx.request.post(`/api/v1/collections/${soloShelf}/posts`, {
          data: { post_id: solo },
        });
        expect(pin.status()).toBe(204);

        await openEditor(page, solo);
        const row = page.locator(
          `[data-testid="post-edit-membership"][data-collection-id="${soloShelf}"]`,
        );
        await expect(row).toBeVisible({ timeout: 15_000 });
        await expect(row).toHaveAttribute('data-can-remove', 'yes');
        await row.getByTestId('post-edit-membership-remove').click();

        await expect(page.getByTestId('post-edit-collections-none')).toBeVisible({
          timeout: 15_000,
        });

        // ⛔ THE PERSISTED ROW, read back through the listing. A list that
        // spliced the item out of its own local array would pass the
        // assertion above.
        const after = await ctx.request.get(`/api/v1/collections/${soloShelf}/posts`);
        const items = ((await after.json()) as { items?: Array<{ id: string }> }).items ?? [];
        expect(
          items.some((p) => p.id === solo),
          'the membership row must be gone from the server, not just from the DOM',
        ).toBe(false);

        // And the POST survives: un-pinning is not deleting (#882).
        const still = await ctx.request.get(`/api/v1/posts/${solo}`);
        expect(still.ok(), 'removing from a collection must not delete the post').toBeTruthy();
      } finally {
        await ctx.request.delete(`/api/v1/collections/${soloShelf}`).catch(() => undefined);
        await ctx.request.delete(`/api/v1/posts/${solo}`).catch(() => undefined);
        await ctx.request.delete(`/api/v1/assets/${soloAsset}`).catch(() => undefined);
      }
    } finally {
      await ctx.close();
    }
    // Nothing about the shared fixture moved.
    expect(await theirShelfHolds(request)).toBe(true);
  });

  // ⛔ THE ARM. N>=2 with DIFFERENT authority per row.
  test("N>=2 MIXED authority: removal is offered for the author's shelf and refused for the stranger's", async ({
    browser,
    request,
  }, testInfo) => {
    const { ctx, page } = await principalPage(browser, AUTHOR);
    try {
      const watched = watchPage(page);
      await openEditor(page, postId);

      const rows = page.getByTestId('post-edit-membership');
      await expect(rows).toHaveCount(2, { timeout: 15_000 });

      const mine = page.locator(
        `[data-testid="post-edit-membership"][data-collection-id="${mineId}"]`,
      );
      const theirs = page.locator(
        `[data-testid="post-edit-membership"][data-collection-id="${theirsId}"]`,
      );

      // BOTH shelves are NAMED, and the author's own is among them, or
      // the pairing is not a pairing. A non-actionable membership is not
      // a hidden one: the author is entitled to know where their work is,
      // and dropping the row would answer the question with a lie.
      await expect(mine).toBeVisible();
      await expect(mine).toContainText(mineName);
      await expect(theirs).toBeVisible();
      await expect(theirs).toContainText(theirsName);

      await expect(mine).toHaveAttribute('data-can-remove', 'yes');
      await expect(theirs).toHaveAttribute('data-can-remove', 'no');

      // THE CONTROL FOLLOWS THE AUTHORITY, and the surface says which is
      // which. An offer that cannot succeed is worse than no offer.
      await expect(mine.getByTestId('post-edit-membership-remove')).toHaveCount(1);
      await expect(theirs.getByTestId('post-edit-membership-remove')).toHaveCount(0);
      await expect(theirs.getByTestId('post-edit-membership-foreign')).toBeVisible();
      expect(await reallyVisible(page, 'post-edit-membership-foreign')).toBe(true);

      // Remove from the one the author curates.
      await mine.getByTestId('post-edit-membership-remove').click();
      await expect(page.getByTestId('post-edit-membership')).toHaveCount(1, { timeout: 15_000 });

      // ⛔⛔ AND THE OTHER SHELF IS UNDISTURBED, asked as the shelf's OWN
      // OWNER. This is the property #882 needs on the way out: removing
      // from your collection touches one row and nobody else's.
      expect(
        await theirShelfHolds(request),
        "removing from the author's shelf must not disturb the admin's",
      ).toBe(true);

      // ⛔⛔ AND AUTHORSHIP IS NOT A BACK DOOR. The UI withheld the
      // control; the SERVER has to withhold the act. Called directly from
      // the author's own session, this is the request a widened
      // authorization would accept.
      const forbidden = await ctx.request.delete(
        `/api/v1/collections/${theirsId}/posts/${postId}`,
      );
      expect(
        forbidden.status(),
        "the author must not be able to un-pin their post from a stranger's collection: " +
          "membership is the curator's (#882), and a 204 here is an authorization widening",
      ).toBe(404);
      expect(
        await theirShelfHolds(request),
        'the refused call must leave the membership row in place',
      ).toBe(true);

      expect(watched.errors, 'no console or runtime errors').toEqual([]);
      expect(
        watched.httpErrors,
        'no failed request other than a missing preview rung',
      ).toEqual([]);
      await page.screenshot({
        path: testInfo.outputPath('post-editor-membership-mixed.png'),
        fullPage: true,
      });
    } finally {
      await ctx.close();
    }
  });

  // The same pairing at the mobile width: the membership rows are where
  // the layout is most likely to push a Remove button off the edge.
  test('the membership rows communicate authority at 390px', async ({ browser }, testInfo) => {
    const { ctx, page } = await principalPage(browser, AUTHOR);
    try {
      await page.setViewportSize({ width: 390, height: 844 });
      // Make sure the author's own shelf holds it again, whatever the
      // previous test left behind. Serial mode makes the order fixed; the
      // re-pin makes the fixture explicit rather than inherited.
      await ctx.request
        .post(`/api/v1/collections/${mineId}/posts`, { data: { post_id: postId } })
        .catch(() => undefined);

      await openEditor(page, postId);
      await expect(page.getByTestId('post-edit-membership')).toHaveCount(2, { timeout: 15_000 });

      const mine = page.locator(
        `[data-testid="post-edit-membership"][data-collection-id="${mineId}"]`,
      );
      const theirs = page.locator(
        `[data-testid="post-edit-membership"][data-collection-id="${theirsId}"]`,
      );
      const btn = mine.getByTestId('post-edit-membership-remove');
      await expect(btn).toBeVisible();
      const box = (await btn.boundingBox())!;
      expect(box.x + box.width, 'the Remove button must not run off a 390px screen')
        .toBeLessThanOrEqual(390);
      await expect(theirs.getByTestId('post-edit-membership-foreign')).toBeVisible();

      const overflow = await page.evaluate(() => ({
        scroll: document.documentElement.scrollWidth,
        client: document.documentElement.clientWidth,
      }));
      expect(overflow.scroll).toBeLessThanOrEqual(overflow.client + 1);

      await page.screenshot({
        path: testInfo.outputPath('post-editor-membership-390.png'),
        fullPage: true,
      });
    } finally {
      await ctx.close();
    }
  });
});
