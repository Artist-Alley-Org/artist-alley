// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119 sprint 21d: a post decides whether it takes comments.
//
// # The headline, and why it is red on the baseline
//
// Arm 1 opens the REAL post editor and looks for the comments control.
// On 3f442292 there is no such control, no `comments_enabled` on the
// post, and `POST /posts/{id}/comments` has no 409: the first
// `toBeVisible` on the toggle fails. Everything after it is what the
// capability has to do once it exists, driven through the product path
// and asserted against the server's rows.
//
// # What every persistence assertion reads
//
// `GET /posts/{id}` (the setting and `comment_count`) and
// `GET /posts/{id}/comments` (the rows), never the dialog's own state
// and never a PATCH's echo. A save that answered 200 and stored nothing
// would pass a checkbox assertion and fail these.
//
// # Anti-vacuity
//
// A gate that refused every comment passes every 409 assertion here.
// So the second user's FIRST comment goes through the real composer
// while the post is enabled, the isolation arm proves post B accepts
// from the same caller before and after post A refuses, and re-enabling
// is followed by a comment that lands.
//
// # Cardinalities
//
// Comments on the disabled post: N=0 (the whiteboard arm's post),
// N=1 (the headline: one comment kept, root and reply refused), N>=2
// (the headline again after re-enable: two rows, and the Go suite holds
// the retained-rows case at depth). Posts per /create submit: exactly 1.
// The disable-versus-comment race is a database property and lives in
// the Go suite, where the interleaving can be held.

import type { APIRequestContext, Browser, Page } from '@playwright/test';
import { test, expect } from '../../helpers/test';
import { ADMIN_STATE_PATH, LOGGED_OUT } from '../../helpers/auth';
import { seededPrincipal } from '../../helpers/seeded-principal';
import { tid } from '../../helpers/testids';

const STAMP = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

/** The OTHER user: an ordinary seeded account that holds `posts.comment`
 *  through the Base role and nothing that could explain a refusal or an
 *  acceptance by privilege. Signed in through the real form. */
const COMMENTER = seededPrincipal('ilse.varga');

async function body(r: { json(): Promise<unknown> }): Promise<Record<string, unknown>> {
  return (await r.json()) as Record<string, unknown>;
}

/** One text asset with novel bytes (dedup would hand one row back
 *  twice otherwise; see post-editor-1119). */
async function makeAsset(request: APIRequestContext, title: string): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`post-comments-1119 bytes for "${title}" ${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), `uploading bytes for "${title}"`).toBe(201);
  const hash = String((await body(up)).hash);
  const res = await request.post('/api/v1/assets', {
    data: { title, asset_type: 2, file_extension: 'txt', file_hash: hash, original_filename: 'pc1119.txt' },
  });
  expect(res.status(), `creating asset "${title}"`).toBe(201);
  return String((await body(res)).id);
}

/** A PUBLISHED org-only post, readable by every signed-in account. The
 *  create body names `comments_enabled` only when asked to, so the
 *  omitted / explicit cases are both reachable from here. */
async function makePost(
  request: APIRequestContext,
  title: string,
  assetId: string,
  commentsEnabled?: boolean,
): Promise<string> {
  const data: Record<string, unknown> = {
    title,
    description: 'pc1119',
    visibility: 'org-only',
    members: [{ asset_id: assetId }],
  };
  if (commentsEnabled !== undefined) data.comments_enabled = commentsEnabled;
  const res = await request.post('/api/v1/posts', { data });
  expect(res.status(), `creating post "${title}"`).toBe(201);
  return String((await body(res)).id);
}

interface StoredPost {
  id: string;
  title: string;
  draft: boolean;
  comments_enabled: boolean;
  comment_count: number;
  updated_at: string;
}

/** The post as the SERVER holds it. */
async function storedPost(request: APIRequestContext, postId: string): Promise<StoredPost> {
  const res = await request.get(`/api/v1/posts/${postId}`);
  expect(res.ok(), `re-reading post ${postId}`).toBeTruthy();
  return (await res.json()) as StoredPost;
}

/** The thread's rows as the SERVER holds them. */
async function storedComments(request: APIRequestContext, postId: string): Promise<{ id: string; body: string }[]> {
  const res = await request.get(`/api/v1/posts/${postId}/comments`);
  expect(res.status(), `reading the thread of ${postId}`).toBe(200);
  return ((await res.json()) as { items?: { id: string; body: string }[] }).items ?? [];
}

async function comment(
  request: APIRequestContext,
  postId: string,
  text: string,
  parentId?: string,
): Promise<{ status: number; error?: string; id?: string }> {
  const res = await request.post(`/api/v1/posts/${postId}/comments`, {
    data: { body: text, parent_id: parentId ?? null },
  });
  const json = (await res.json().catch(() => ({}))) as { error?: string; id?: string };
  return { status: res.status(), error: json.error, id: json.id };
}

/** Every post the caller can list that carries this exact title. The
 *  "exactly one post" invariant is a COUNT from the server. */
async function postsTitled(request: APIRequestContext, title: string): Promise<StoredPost[]> {
  const res = await request.get('/api/v1/posts?limit=100');
  expect(res.status()).toBe(200);
  const items = ((await res.json()) as { items?: StoredPost[] }).items ?? [];
  return items.filter((p) => p.title === title);
}

const SUBRESOURCE_FAILURE = /Failed to load resource/i;
const PREVIEW_VARIANT = /\/variants\//;

/** Uncaught errors, non-subresource console errors, and every >= 400
 *  the page provoked other than a missing preview rung. `allow` names
 *  requests an arm EXPECTS to fail (a deliberate 409). */
function watchPage(page: Page, allow: RegExp[] = []) {
  const errors: string[] = [];
  const httpErrors: string[] = [];
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
    if (allow.some((re) => re.test(`${r.request().method()} ${path}`))) return;
    httpErrors.push(`${r.status()} ${path}`);
  });
  return { errors, httpErrors };
}

async function openEditor(page: Page, postId: string) {
  await page.goto(`/posts/${postId}`);
  await page.locator('[aria-label="Post actions"]').first().click();
  await page.getByTestId('post-edit').click();
  await expect(page.getByTestId('post-edit-body')).toBeVisible({ timeout: 15_000 });
}

/** Ancestor-aware visibility: the element's own styles say nothing
 *  about an `opacity: 0` parent. */
async function reallyVisible(page: Page, testid: string): Promise<boolean> {
  return page
    .getByTestId(testid)
    .first()
    .evaluate((el: Element) => (el as HTMLElement & { checkVisibility?: () => boolean }).checkVisibility?.() ?? false);
}

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

/** The thread as the READER sees it: rows, the composer, the reply
 *  affordance and the note, each measured with checkVisibility. */
async function threadState(page: Page) {
  const rows = page.getByTestId('comment-row');
  const n = await rows.count();
  let visibleRows = 0;
  for (let i = 0; i < n; i++) {
    if (await rows.nth(i).evaluate((el: Element) => (el as HTMLElement).checkVisibility())) visibleRows += 1;
  }
  return {
    rows: n,
    visibleRows,
    composer: await page.getByTestId('comments-composer').count(),
    replyButtons: await page.getByTestId('comment-reply').count(),
    replyComposers: await page.getByTestId('comment-reply-composer').count(),
    note: (await page.getByTestId('comments-disabled-note').count()) > 0 && (await reallyVisible(page, 'comments-disabled-note')),
  };
}

/** Everything this file makes, torn down by id. */
const madePosts: string[] = [];
const madeAssets: string[] = [];

test.describe('#1119 21d: a post decides whether it takes comments', () => {
  test.describe.configure({ mode: 'serial' });

  test.afterAll(async ({ request }) => {
    for (const p of madePosts.splice(0)) {
      await request.delete(`/api/v1/posts/${p}`).catch(() => undefined);
    }
    for (const a of madeAssets.splice(0)) {
      await request.delete(`/api/v1/assets/${a}`).catch(() => undefined);
    }
  });

  // ── ARM 1: the headline. RED on 3f442292 (no control, no setting, no 409). ──
  test('the author turns comments off in the editor; the thread keeps its rows, refuses new ones, and comes back when turned on', async ({
    page,
    browser,
    request,
  }, testInfo) => {
    const watched = watchPage(page);
    const asset = await makeAsset(request, `pc1119 headline ${STAMP}`);
    madeAssets.push(asset);
    const post = await makePost(request, `pc1119 headline ${STAMP}`, asset);
    madePosts.push(post);
    expect((await storedPost(request, post)).comments_enabled, 'a post created without the field is enabled').toBe(true);

    // The other user, on the post while it is enabled: the first comment
    // goes through the REAL composer, which is the proof that this
    // account is otherwise permitted.
    const { ctx: readerCtx, page: reader } = await principalPage(browser, COMMENTER);
    const readerWatched = watchPage(reader, [/POST \/api\/v1\/posts\/[0-9a-f-]+\/comments/]);
    try {
      await reader.goto(`/posts/${post}`);
      await expect(reader.getByTestId('comments-composer')).toBeVisible({ timeout: 15_000 });
      await reader.getByTestId('comments-composer-body').fill(`first while enabled ${STAMP}`);
      const firstPost = reader.waitForResponse(
        (r) => r.url().includes('/comments') && r.request().method() === 'POST',
      );
      await reader.getByTestId('comments-composer-submit').click();
      expect((await firstPost).status(), 'the enabled post accepts through the real composer').toBe(201);
      await expect(reader.getByTestId('comment-row')).toHaveCount(1);
      const stored1 = await storedComments(request, post);
      expect(stored1.map((c) => c.body)).toEqual([`first while enabled ${STAMP}`]);
      const existing = stored1[0].id;
      expect((await storedPost(request, post)).comment_count).toBe(1);

      // ── THE AUTHOR TURNS COMMENTS OFF, in the editor. ──
      // Sniff every write, so "Save does not publish, unpublish or
      // schedule" is a statement about the wire, and the PATCH body is
      // what it says.
      const writes: { method: string; path: string; body: unknown }[] = [];
      page.on('request', (req) => {
        const m = req.method().toUpperCase();
        if (m === 'GET' || m === 'HEAD' || m === 'OPTIONS') return;
        writes.push({ method: m, path: new URL(req.url()).pathname, body: req.postDataJSON() });
      });

      await openEditor(page, post);
      const toggle = page.getByTestId('post-edit-comments-enabled');
      await expect(
        toggle,
        'the editor must offer a comments control. On the baseline there is none: this is the red-before assertion',
      ).toBeVisible({ timeout: 10_000 });
      await expect(toggle).toBeChecked();
      expect(await reallyVisible(page, 'post-edit-comments')).toBe(true);
      await page.getByTestId('post-edit-comments').scrollIntoViewIfNeeded();
      await page.screenshot({ path: testInfo.outputPath('post-comments-editor-desktop.png') });

      await toggle.uncheck();
      await expect(toggle).not.toBeChecked();
      const saved = page.waitForResponse((r) => r.url().includes(`/posts/${post}`) && r.request().method() === 'PATCH');
      await page.getByTestId('post-edit-save').click();
      expect((await saved).status()).toBe(200);
      await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });

      const patch = writes.find((w) => w.method === 'PATCH' && w.path.endsWith(`/posts/${post}`));
      expect(patch, 'the save is one PATCH').toBeTruthy();
      expect((patch!.body as { comments_enabled?: unknown }).comments_enabled, 'the PATCH carries an explicit false').toBe(false);
      expect((patch!.body as { if_unchanged_since?: unknown }).if_unchanged_since, 'and rides the stale-write guard').toBeTruthy();
      expect(writes.filter((w) => /publication-schedule|\/publish$|\/unpublish$/.test(w.path))).toEqual([]);

      // PERSISTED, and nothing else moved.
      const afterDisable = await storedPost(request, post);
      expect(afterDisable.comments_enabled, 'the setting is stored').toBe(false);
      expect(afterDisable.draft, 'disabling comments did not unpublish').toBe(false);
      expect(afterDisable.comment_count, 'the existing comment is still counted').toBe(1);
      expect((await storedComments(request, post)).map((c) => c.id), 'the existing comment is still there').toEqual([existing]);

      // The author's own page re-read the post: the thread flipped to
      // the read-only state WITHOUT navigating.
      expect(new URL(page.url()).pathname).toBe(`/posts/${post}`);
      await expect(page.getByTestId('comments-disabled-note')).toBeVisible({ timeout: 10_000 });
      expect((await threadState(page)).composer).toBe(0);

      // A RELOAD AND A FRESH OPEN show the persisted value.
      await page.reload();
      await page.locator('[aria-label="Post actions"]').first().click();
      await page.getByTestId('post-edit').click();
      await expect(page.getByTestId('post-edit-comments-enabled')).toBeVisible({ timeout: 15_000 });
      await expect(page.getByTestId('post-edit-comments-enabled')).not.toBeChecked();

      // A metadata save with comments off leaves them off, and sends
      // nothing about them.
      writes.length = 0;
      await page.getByTestId('post-edit-title').fill(`pc1119 headline renamed ${STAMP}`);
      await page.getByTestId('post-edit-save').click();
      await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });
      const renamePatch = writes.find((w) => w.method === 'PATCH');
      expect(renamePatch).toBeTruthy();
      expect('comments_enabled' in (renamePatch!.body as object), 'an untouched setting is not re-sent').toBe(false);
      expect((await storedPost(request, post)).comments_enabled).toBe(false);
      expect((await storedPost(request, post)).title).toBe(`pc1119 headline renamed ${STAMP}`);

      // ── THE STALE BROWSER: the reader's page still shows the composer. ──
      // The server says no; the thread says so in words, withdraws the
      // composer, and appends nothing.
      expect((await threadState(reader)).composer, 'precondition: the reader still holds the enabled representation').toBe(1);
      await reader.getByTestId('comments-composer-body').fill(`from a stale page ${STAMP}`);
      const staleRes = reader.waitForResponse((r) => r.url().includes('/comments') && r.request().method() === 'POST');
      await reader.getByTestId('comments-composer-submit').click();
      expect((await staleRes).status()).toBe(409);
      await expect(reader.getByTestId('comments-disabled-note')).toBeVisible({ timeout: 10_000 });
      await expect(reader.getByRole('alert').filter({ hasText: /turned off/i })).toBeVisible();
      const stale = await threadState(reader);
      expect(stale.composer, 'the composer is withdrawn on the server’s answer').toBe(0);
      expect(stale.rows, 'nothing was appended optimistically').toBe(1);
      expect((await storedComments(request, post)).length).toBe(1);
      expect((await storedPost(request, post)).comment_count).toBe(1);

      // ── THE READER, FRESH: existing comment visible, nothing to compose with. ──
      await reader.reload();
      await expect(reader.getByTestId('comments-disabled-note')).toBeVisible({ timeout: 15_000 });
      await expect(reader.getByTestId('comment-row')).toHaveCount(1);
      const fresh = await threadState(reader);
      expect(fresh.visibleRows, 'the existing comment is readable').toBe(1);
      expect(fresh.composer, 'no top-level composer').toBe(0);
      expect(fresh.replyButtons, 'no Reply affordance').toBe(0);
      expect(fresh.replyComposers).toBe(0);
      expect(fresh.note, 'the note says why').toBe(true);
      await reader.getByTestId('comments-disabled-note').scrollIntoViewIfNeeded();
      await reader.screenshot({ path: testInfo.outputPath('post-comments-thread-disabled-desktop.png') });

      // The real API, as the reader: root and reply both refused with
      // the stable value, and the rows do not move.
      const root = await comment(readerCtx.request, post, `root while off ${STAMP}`);
      expect(root.status, 'a top-level comment is refused').toBe(409);
      expect(root.error).toBe('comments_disabled');
      const reply = await comment(readerCtx.request, post, `reply while off ${STAMP}`, existing);
      expect(reply.status, 'a reply is refused').toBe(409);
      expect(reply.error).toBe('comments_disabled');
      expect((await storedComments(request, post)).map((c) => c.id)).toEqual([existing]);
      expect((await storedPost(request, post)).comment_count).toBe(1);

      // ── THE AUTHOR TURNS COMMENTS BACK ON. ──
      await openEditor(page, post);
      await page.getByTestId('post-edit-comments-enabled').check();
      const reSaved = page.waitForResponse((r) => r.url().includes(`/posts/${post}`) && r.request().method() === 'PATCH');
      await page.getByTestId('post-edit-save').click();
      expect((await reSaved).status()).toBe(200);
      await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });
      expect((await storedPost(request, post)).comments_enabled, 'disabled -> enabled persisted').toBe(true);
      // The author's page: the composer returned, no navigation.
      expect(new URL(page.url()).pathname).toBe(`/posts/${post}`);
      await expect(page.getByTestId('comments-composer')).toBeVisible({ timeout: 10_000 });

      // The reader: the composer is back, and a comment LANDS (N>=2).
      await reader.reload();
      await expect(reader.getByTestId('comments-composer')).toBeVisible({ timeout: 15_000 });
      expect((await threadState(reader)).replyButtons, 'Reply is offered again').toBeGreaterThan(0);
      await reader.getByTestId('comments-composer-body').fill(`after re-enable ${STAMP}`);
      const landed = reader.waitForResponse((r) => r.url().includes('/comments') && r.request().method() === 'POST');
      await reader.getByTestId('comments-composer-submit').click();
      expect((await landed).status()).toBe(201);
      await expect(reader.getByTestId('comment-row')).toHaveCount(2);
      expect((await storedComments(request, post)).map((c) => c.body).sort()).toEqual(
        [`after re-enable ${STAMP}`, `first while enabled ${STAMP}`].sort(),
      );
      expect((await storedPost(request, post)).comment_count).toBe(2);

      expect(watched.errors, 'no console or runtime errors on the author’s page').toEqual([]);
      expect(watched.httpErrors, 'no failed request on the author’s page').toEqual([]);
      expect(readerWatched.errors, 'no console or runtime errors on the reader’s page').toEqual([]);
      expect(readerWatched.httpErrors, 'only the deliberate 409 on the reader’s page').toEqual([]);
    } finally {
      await readerCtx.close();
    }
  });

  // ── ARM 2: two posts, one caller: the setting is per post. ────────
  test('a disabled post refuses while an enabled one accepts, from the same commenter', async ({
    browser,
    request,
  }) => {
    const asset = await makeAsset(request, `pc1119 isolation ${STAMP}`);
    madeAssets.push(asset);
    const postA = await makePost(request, `pc1119 isolation A ${STAMP}`, asset);
    const postB = await makePost(request, `pc1119 isolation B ${STAMP}`, asset);
    madePosts.push(postA, postB);

    // Disable A through the same PATCH the editor sends.
    const patched = await request.patch(`/api/v1/posts/${postA}`, { data: { comments_enabled: false } });
    expect(patched.status()).toBe(200);
    expect((await storedPost(request, postA)).comments_enabled).toBe(false);
    expect((await storedPost(request, postB)).comments_enabled, 'disabling A did not touch B').toBe(true);

    const { ctx } = await principalPage(browser, COMMENTER);
    try {
      // Anti-vacuity FIRST: B accepts from this caller.
      const b1 = await comment(ctx.request, postB, `B accepts before ${STAMP}`);
      expect(b1.status, 'the enabled post must genuinely accept before it is used as the comparison').toBe(201);
      expect((await storedComments(request, postB)).map((c) => c.body)).toEqual([`B accepts before ${STAMP}`]);

      const a = await comment(ctx.request, postA, `A refuses ${STAMP}`);
      expect(a.status).toBe(409);
      expect(a.error).toBe('comments_disabled');

      const b2 = await comment(ctx.request, postB, `B accepts after ${STAMP}`);
      expect(b2.status, 'B still accepts after A refused').toBe(201);

      expect((await storedComments(request, postA)).length).toBe(0);
      expect((await storedPost(request, postA)).comment_count).toBe(0);
      expect((await storedComments(request, postB)).length).toBe(2);
      expect((await storedPost(request, postB)).comment_count).toBe(2);
    } finally {
      await ctx.close();
    }
  });

  // ── ARM 3: whiteboards are a separate path and stay open. ─────────
  test('a post created with comments off still takes a whiteboard, and still refuses an ordinary comment', async ({
    request,
  }) => {
    const asset = await makeAsset(request, `pc1119 whiteboard ${STAMP}`);
    madeAssets.push(asset);
    // The explicit-false CREATE wire, and its explicit-true twin.
    const off = await makePost(request, `pc1119 whiteboard off ${STAMP}`, asset, false);
    const on = await makePost(request, `pc1119 whiteboard on ${STAMP}`, asset, true);
    madePosts.push(off, on);
    expect((await storedPost(request, off)).comments_enabled, 'explicit false on create is stored').toBe(false);
    expect((await storedPost(request, on)).comments_enabled, 'explicit true on create is stored').toBe(true);

    // N=0 on the disabled post: nothing there, nothing lands.
    const refused = await comment(request, off, `ordinary on the sketch post ${STAMP}`);
    expect(refused.status).toBe(409);
    expect(refused.error).toBe('comments_disabled');

    const wb = await request.post(`/api/v1/posts/${off}/whiteboards`, {
      data: { title: `sketch ${STAMP}`, content: { source_w: 800, source_h: 600, layers: [] } },
    });
    expect(wb.status(), 'the whiteboard path is not gated by the comments setting').toBe(201);
    const wbId = String((await body(wb)).id);
    const list = await request.get(`/api/v1/posts/${off}/whiteboards`);
    expect(list.status()).toBe(200);
    expect(((await list.json()) as { id: string }[]).map((w) => w.id)).toEqual([wbId]);

    // And ordinary comments are STILL refused beside it, replies under it included.
    const under = await comment(request, off, `reply under the sketch ${STAMP}`, wbId);
    expect(under.status).toBe(409);
    expect(under.error).toBe('comments_disabled');
    expect((await storedPost(request, off)).comments_enabled).toBe(false);
  });

  // ── ARM 4: /create, the default and the explicit off. ─────────────
  test('/create leaves comments on by default and turns them off from one disclosure', async ({
    page,
    request,
  }, testInfo) => {
    const watched = watchPage(page);

    // ── DEFAULT: the artist does nothing about comments. ──
    await page.goto('/create');
    await expect(page.locator(tid('create-page'))).toBeVisible();
    const created1 = page.waitForResponse(
      (r) => r.url().includes('/api/v1/assets') && r.request().method() === 'POST' && r.ok(),
      { timeout: 30_000 },
    );
    await page.locator(tid('create-file-input')).setInputFiles({
      name: 'pc1119-default.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(`pc1119 default ${STAMP} ${Math.random()}`),
    });
    madeAssets.push(((await (await created1).json()) as { id: string }).id);
    await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });
    const defaultTitle = `pc1119 create default ${STAMP}`;
    await page.locator(tid('create-title')).fill(defaultTitle);

    // The two actions are untouched and the disclosure is closed.
    await expect(page.locator(tid('create-publish'))).toBeVisible();
    await expect(page.locator(tid('create-save-draft'))).toBeVisible();
    const details = page.locator(tid('create-comments'));
    await expect(details).toBeVisible();
    await expect(details).toHaveJSProperty('open', false);
    await page.locator(tid('create-publish')).click();
    await page.waitForURL(/\/posts\/[0-9a-f-]{36}/, { timeout: 30_000 });
    const defaultPost = page.url().split('/posts/')[1];
    madePosts.push(defaultPost);
    const stored = await storedPost(request, defaultPost);
    expect(stored.comments_enabled, 'doing nothing makes a post that takes comments').toBe(true);
    expect(stored.draft, 'Publish still publishes').toBe(false);
    await expect(page.getByTestId('comments-composer')).toBeVisible({ timeout: 15_000 });

    // ── EXPLICIT OFF: open the disclosure, untick, publish. ──
    await page.goto('/create');
    await expect(page.locator(tid('create-page'))).toBeVisible();
    const created2 = page.waitForResponse(
      (r) => r.url().includes('/api/v1/assets') && r.request().method() === 'POST' && r.ok(),
      { timeout: 30_000 },
    );
    await page.locator(tid('create-file-input')).setInputFiles({
      name: 'pc1119-off.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(`pc1119 off ${STAMP} ${Math.random()}`),
    });
    madeAssets.push(((await (await created2).json()) as { id: string }).id);
    await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });
    const offTitle = `pc1119 create off ${STAMP}`;
    await page.locator(tid('create-title')).fill(offTitle);
    await page.locator(tid('create-comments')).locator('summary').click();
    await expect(page.locator(tid('create-comments'))).toHaveJSProperty('open', true);
    const box = page.locator(tid('create-comments-enabled'));
    await expect(box, 'the default is on, and a fresh composition starts there').toBeChecked();
    await box.uncheck();
    await expect(box).not.toBeChecked();
    await page.locator(tid('create-comments')).scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath('post-comments-create-desktop.png') });
    const postCreate = page.waitForResponse((r) => r.url().endsWith('/api/v1/posts') && r.request().method() === 'POST');
    await page.locator(tid('create-publish')).click();
    const createRes = await postCreate;
    expect(createRes.status()).toBe(201);
    expect((createRes.request().postDataJSON() as { comments_enabled?: unknown }).comments_enabled, 'the create carries an explicit false').toBe(false);
    await page.waitForURL(/\/posts\/[0-9a-f-]{36}/, { timeout: 30_000 });
    const offPost = page.url().split('/posts/')[1];
    madePosts.push(offPost);

    // EXACTLY ONE post with this title, and it is off.
    const titled = await postsTitled(request, offTitle);
    expect(titled.length, 'exactly one post was made').toBe(1);
    expect(titled[0].id).toBe(offPost);
    expect(titled[0].comments_enabled).toBe(false);
    expect((await storedPost(request, offPost)).draft).toBe(false);
    await expect(page.getByTestId('comments-disabled-note')).toBeVisible({ timeout: 15_000 });
    expect((await threadState(page)).composer).toBe(0);

    // The next composition starts at the default again.
    await page.goto('/create');
    await page.locator(tid('create-comments')).locator('summary').click();
    await expect(page.locator(tid('create-comments-enabled'))).toBeChecked();

    expect(watched.errors, 'no console or runtime errors').toEqual([]);
    expect(watched.httpErrors, 'no failed request other than a missing preview rung').toEqual([]);
  });

  // ── ARM 5: 390px. ─────────────────────────────────────────────────
  test('the editor toggle, the disabled thread and the /create disclosure are reachable and unclipped at 390px', async ({
    browser,
    request,
  }, testInfo) => {
    const asset = await makeAsset(request, `pc1119 narrow ${STAMP}`);
    madeAssets.push(asset);
    const post = await makePost(request, `pc1119 narrow ${STAMP}`, asset);
    madePosts.push(post);
    // One comment to keep, so "existing comments readable" is measured.
    const kept = await comment(request, post, `kept at 390 ${STAMP}`);
    expect(kept.status).toBe(201);

    const ctx = await browser.newContext({
      storageState: ADMIN_STATE_PATH,
      viewport: { width: 390, height: 844 },
      hasTouch: true,
      isMobile: true,
    });
    const page = await ctx.newPage();
    const watched = watchPage(page);
    try {
      const noOverflow = async (what: string) => {
        const overflow = await page.evaluate(
          () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
        );
        expect(overflow, `no horizontal overflow: ${what}`).toBeLessThanOrEqual(0);
      };
      const onScreen = async (id: string) => {
        await page.getByTestId(id).first().scrollIntoViewIfNeeded();
        expect(await reallyVisible(page, id), `${id} must be visible all the way up at 390px`).toBe(true);
        const box = await page.getByTestId(id).first().boundingBox();
        expect(box, `${id} has a box`).not.toBeNull();
        expect(box!.x, `${id} starts on screen`).toBeGreaterThanOrEqual(0);
        expect(box!.x + box!.width, `${id} must not run off a 390px screen`).toBeLessThanOrEqual(390);
      };

      // The editor: toggle beside the 21c and 21e controls.
      await openEditor(page, post);
      for (const id of ['post-edit-comments', 'post-edit-comments-enabled', 'post-edit-title', 'post-edit-publication', 'post-edit-publish-toggle', 'post-edit-save']) {
        await onScreen(id);
      }
      await noOverflow('editor open');
      await page.getByTestId('post-edit-comments').scrollIntoViewIfNeeded();
      await page.screenshot({ path: testInfo.outputPath('post-comments-editor-390.png') });
      await page.getByTestId('post-edit-comments-enabled').uncheck();
      await page.getByTestId('post-edit-save').click();
      await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });
      expect((await storedPost(request, post)).comments_enabled).toBe(false);

      // The thread, disabled: the kept comment is readable, no composer,
      // no Reply, the note is there.
      await page.reload();
      await expect(page.getByTestId('comments-disabled-note')).toBeVisible({ timeout: 15_000 });
      // The note is prop-driven and renders before the thread has
      // loaded; the rows are a fetch. Wait for the row, then measure.
      await expect(page.getByTestId('comment-row')).toHaveCount(1, { timeout: 15_000 });
      const state = await threadState(page);
      expect(state.visibleRows).toBe(1);
      expect(state.composer).toBe(0);
      expect(state.replyButtons).toBe(0);
      await onScreen('comments-disabled-note');
      await onScreen('comment-row');
      await noOverflow('disabled thread');
      await page.screenshot({ path: testInfo.outputPath('post-comments-thread-390.png') });

      // Re-enable from the editor at this width, and the composer returns.
      await page.locator('[aria-label="Post actions"]').first().click();
      await page.getByTestId('post-edit').click();
      await expect(page.getByTestId('post-edit-comments-enabled')).toBeVisible({ timeout: 15_000 });
      await expect(page.getByTestId('post-edit-comments-enabled')).not.toBeChecked();
      await page.getByTestId('post-edit-comments-enabled').check();
      await page.getByTestId('post-edit-save').click();
      await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });
      expect((await storedPost(request, post)).comments_enabled).toBe(true);
      await expect(page.getByTestId('comments-composer')).toBeVisible({ timeout: 10_000 });
      await onScreen('comments-composer-body');

      // /create: the disclosure and its box.
      await page.goto('/create');
      await expect(page.locator(tid('create-page'))).toBeVisible();
      await page.locator(tid('create-comments')).scrollIntoViewIfNeeded();
      await page.locator(tid('create-comments')).locator('summary').click();
      for (const id of ['create-comments', 'create-comments-enabled', 'create-publish', 'create-save-draft', 'create-schedule']) {
        await onScreen(id);
      }
      await expect(page.locator(tid('create-comments-enabled'))).toBeChecked();
      await noOverflow('/create');
      await page.locator(tid('create-comments')).scrollIntoViewIfNeeded();
      await page.screenshot({ path: testInfo.outputPath('post-comments-create-390.png') });

      expect(watched.errors, 'no console or runtime errors at 390px').toEqual([]);
      expect(watched.httpErrors).toEqual([]);
    } finally {
      await ctx.close();
    }
  });
});
