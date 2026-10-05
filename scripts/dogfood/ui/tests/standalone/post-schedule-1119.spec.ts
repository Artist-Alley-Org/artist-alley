// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119 sprint 21e: an author schedules their OWN post for later, through
// the scheduled-action engine, from the two places an author works.
//
// # The headline, and why it is over the wire
//
// Arm 1 PUTs `/api/v1/posts/{id}/publication-schedule` as the author of
// a draft and reads the schedule BACK from the server. On the baseline
// (247f8413) that route does not exist: the request answers 404 and no
// row can be read back. That is the red-before proof for this sprint,
// and it is deliberately a request rather than a Go test: every Go test
// for this seam names the new handlers, so none of them can compile
// against the baseline, and a test that cannot compile is not a test
// that failed.
//
// # What every persistence assertion reads
//
// `GET /posts/{id}/publication-schedule` and `GET /posts/{id}`, never
// the dialog's own text and never the PUT's echo. The status endpoint
// re-reads the scheduled_actions row; a handler that answered 200 and
// wrote nothing would pass a body assertion and fail these.
//
// # The partial failure that has to be proven, not described
//
// "Publish later" on /create is two durable operations. Arm 4 makes
// the SECOND one fail on the wire (the PUT is intercepted and answered
// 503 after the POST /posts has succeeded) and then asserts the rows:
// exactly one post with the fixture's title, a draft, with no schedule.
// Then it recovers the way the page offers to, from the persisted
// draft, and asserts the count is STILL one. A test that stopped at
// "an error was shown" would pass on a store that made two drafts.
//
// # Cardinalities
//
// Schedules per post: N=0 (no schedule; ordinary publish and draft are
// the controls), N=1 (pending, cancelled, replaced). Posts per failed
// create: exactly 1. The N>=2 concurrent case is a database property
// and lives in the Go suite, where the interleaving can be held.

import type { APIRequestContext, Browser, Page } from '@playwright/test';
import { test, expect } from '../../helpers/test';
import { ADMIN_STATE_PATH, LOGGED_OUT, bootstrapAdminRef } from '../../helpers/auth';
import { seededPrincipal } from '../../helpers/seeded-principal';
import { tid } from '../../helpers/testids';

const STAMP = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

/** A stranger to every post this file makes. Used only where the
 *  assertion is that a non-author gets nothing, so any seeded ordinary
 *  account serves; this one is signed in through the real form so the
 *  refusal is measured through a real session. */
const STRANGER = seededPrincipal('ilse.varga');

interface Schedule {
  id: string;
  post_id: string;
  scheduled_for: string;
  state: string;
  created_by: number;
}

async function body(r: { json(): Promise<unknown> }): Promise<Record<string, unknown>> {
  return (await r.json()) as Record<string, unknown>;
}

/** One text asset with novel bytes (see post-editor-1119 for why the
 *  bytes must differ per call: dedup would hand one row back twice). */
async function makeAsset(request: APIRequestContext, title: string): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`post-schedule-1119 bytes for "${title}" ${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), `uploading bytes for "${title}"`).toBe(201);
  const hash = String((await body(up)).hash);
  const res = await request.post('/api/v1/assets', {
    data: { title, asset_type: 2, file_extension: 'txt', file_hash: hash, original_filename: 'ps1119.txt' },
  });
  expect(res.status(), `creating asset "${title}"`).toBe(201);
  return String((await body(res)).id);
}

async function makeDraft(request: APIRequestContext, title: string, assetId: string): Promise<string> {
  const res = await request.post('/api/v1/posts', {
    data: { title, description: 'ps1119', visibility: 'org-only', draft: true, members: [{ asset_id: assetId }] },
  });
  expect(res.status(), `creating draft "${title}"`).toBeLessThan(300);
  return String((await body(res)).id);
}

/** The schedule as the SERVER holds it, or null. */
async function storedSchedule(request: APIRequestContext, postId: string): Promise<Schedule | null> {
  const res = await request.get(`/api/v1/posts/${postId}/publication-schedule`);
  expect(res.status(), `reading the schedule of ${postId}`).toBe(200);
  return ((await res.json()) as { schedule: Schedule | null }).schedule;
}

async function storedPost(request: APIRequestContext, postId: string) {
  const res = await request.get(`/api/v1/posts/${postId}`);
  expect(res.ok(), `re-reading post ${postId}`).toBeTruthy();
  return (await res.json()) as { title: string; draft: boolean; updated_at: string };
}

/** How many of the caller's drafts carry this exact title. The
 *  partial-failure invariant is a COUNT, and the count has to come from
 *  the server. */
async function draftsTitled(request: APIRequestContext, title: string): Promise<string[]> {
  const res = await request.get('/api/v1/posts?draft=true&limit=100');
  expect(res.status()).toBe(200);
  const items = ((await res.json()) as { items?: { id: string; title: string }[] }).items ?? [];
  return items.filter((p) => p.title === title).map((p) => p.id);
}

/** A `datetime-local` value one hour ahead, in the browser's local
 *  wall-clock, which is what the control speaks. */
function localInAnHour(): string {
  const d = new Date(Date.now() + 60 * 60 * 1000);
  d.setSeconds(0, 0);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

const SUBRESOURCE_FAILURE = /Failed to load resource/i;
const PREVIEW_VARIANT = /\/variants\//;

/** Uncaught errors, non-subresource console errors, and every >= 400
 *  the page provoked other than a missing preview rung. The same three
 *  lists post-editor-1119 keeps, for the same reasons. `allow` names
 *  requests this arm EXPECTS to fail (arm 4 makes one fail on purpose). */
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

/** Everything this file makes, torn down by id. */
const madePosts: string[] = [];
const madeAssets: string[] = [];

test.describe('#1119 21e: an author schedules their own post', () => {
  test.describe.configure({ mode: 'serial' });

  test.afterAll(async ({ request }) => {
    for (const p of madePosts.splice(0)) {
      await request.delete(`/api/v1/posts/${p}/publication-schedule`).catch(() => undefined);
      await request.delete(`/api/v1/posts/${p}`).catch(() => undefined);
    }
    for (const a of madeAssets.splice(0)) {
      await request.delete(`/api/v1/assets/${a}`).catch(() => undefined);
    }
  });

  // ── ARM 1: the headline. RED on 247f8413 (the route does not exist). ──
  test('the author can schedule a draft, and the schedule is persisted as theirs', async ({
    page,
    request,
  }) => {
    const me = await bootstrapAdminRef(page);
    const asset = await makeAsset(request, `ps1119 headline ${STAMP}`);
    madeAssets.push(asset);
    const post = await makeDraft(request, `ps1119 headline ${STAMP}`, asset);
    madePosts.push(post);

    const when = new Date(Date.now() + 2 * 60 * 60 * 1000);
    when.setMilliseconds(0);
    const put = await request.put(`/api/v1/posts/${post}/publication-schedule`, {
      data: { scheduled_for: when.toISOString() },
    });
    expect(
      put.status(),
      'PUT /posts/{id}/publication-schedule must exist and accept the author. ' +
        'On the baseline this is 404: there is no author scheduling surface at all',
    ).toBe(200);

    // THE SERVER'S ROW, not the PUT's echo.
    const stored = await storedSchedule(request, post);
    expect(stored, 'the status read must return the pending schedule').not.toBeNull();
    expect(stored!.state).toBe('pending');
    expect(stored!.post_id).toBe(post);
    expect(stored!.created_by, 'created_by must be the real requesting user').toBe(me);
    expect(new Date(stored!.scheduled_for).getTime()).toBe(when.getTime());

    // Scheduling did not publish.
    expect((await storedPost(request, post)).draft, 'a scheduled draft is still a draft').toBe(true);

    // The past and the present are refused, and refuse to write.
    for (const bad of [new Date(Date.now() - 60_000), new Date()]) {
      const r = await request.put(`/api/v1/posts/${post}/publication-schedule`, {
        data: { scheduled_for: bad.toISOString() },
      });
      expect(r.status(), `PUT with ${bad.toISOString()} (not in the future)`).toBe(400);
    }
    expect((await storedSchedule(request, post))!.id, 'a refused PUT must not disturb the schedule').toBe(stored!.id);

    // Cancel prevents it, and status says so.
    const del = await request.delete(`/api/v1/posts/${post}/publication-schedule`);
    expect(del.status()).toBe(200);
    expect(await storedSchedule(request, post)).toBeNull();
    expect((await storedPost(request, post)).draft).toBe(true);
  });

  // ── ARM 2: a stranger sees and gets nothing. ──────────────────────
  test('a non-author cannot see or set the schedule of somebody else’s draft', async ({
    browser,
    request,
  }) => {
    const asset = await makeAsset(request, `ps1119 stranger ${STAMP}`);
    madeAssets.push(asset);
    const post = await makeDraft(request, `ps1119 stranger ${STAMP}`, asset);
    madePosts.push(post);

    const { ctx } = await principalPage(browser, STRANGER);
    try {
      // A draft is unreadable to a stranger, so the answer is 404 and
      // not 403: the surface is not an existence oracle.
      const get = await ctx.request.get(`/api/v1/posts/${post}/publication-schedule`);
      expect(get.status()).toBe(404);
      const put = await ctx.request.put(`/api/v1/posts/${post}/publication-schedule`, {
        data: { scheduled_for: new Date(Date.now() + 3_600_000).toISOString() },
      });
      expect(put.status()).toBe(404);
      expect(await storedSchedule(request, post), 'nothing was written').toBeNull();
    } finally {
      await ctx.close();
    }
  });

  // ── ARM 3: the editor, desktop. ───────────────────────────────────
  test('the post editor schedules, shows the persisted schedule after a reload, and cancels', async ({
    page,
    request,
  }, testInfo) => {
    const watched = watchPage(page);
    const asset = await makeAsset(request, `ps1119 editor ${STAMP}`);
    madeAssets.push(asset);
    const post = await makeDraft(request, `ps1119 editor ${STAMP}`, asset);
    madePosts.push(post);

    // Sniff every write the dialog makes, so the "Save does not
    // schedule" half below is a statement about the wire.
    const writes: string[] = [];
    page.on('request', (req) => {
      const m = req.method().toUpperCase();
      if (m === 'GET' || m === 'HEAD' || m === 'OPTIONS') return;
      writes.push(`${m} ${new URL(req.url()).pathname}`);
    });

    await openEditor(page, post);
    await expect(page.getByTestId('post-edit-schedule')).toBeVisible();
    await expect(page.getByTestId('post-edit-schedule-at')).toBeVisible();
    await expect(page.getByTestId('post-edit-schedule-submit')).toBeDisabled();

    // METADATA SAVE DOES NOT SCHEDULE, PUBLISH OR UNPUBLISH.
    await page.getByTestId('post-edit-title').fill(`ps1119 editor renamed ${STAMP}`);
    await page.getByTestId('post-edit-save').click();
    await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });
    expect(writes.filter((w) => /publication-schedule|\/publish$|\/unpublish$/.test(w))).toEqual([]);
    expect(await storedSchedule(request, post)).toBeNull();
    expect((await storedPost(request, post)).draft).toBe(true);
    expect((await storedPost(request, post)).title).toBe(`ps1119 editor renamed ${STAMP}`);

    // SCHEDULE, from the editor.
    await openEditor(page, post);
    const local = localInAnHour();
    await page.getByTestId('post-edit-schedule-at').fill(local);
    await expect(page.getByTestId('post-edit-schedule-submit')).toBeEnabled();
    const putDone = page.waitForResponse(
      (r) => r.url().includes('/publication-schedule') && r.request().method() === 'PUT',
    );
    await page.getByTestId('post-edit-schedule-submit').click();
    expect((await putDone).status()).toBe(200);
    await expect(page.getByTestId('post-edit-schedule-pending')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('post-edit-schedule-at')).toHaveCount(0);

    const stored = await storedSchedule(request, post);
    expect(stored).not.toBeNull();
    expect(stored!.state).toBe('pending');
    expect(new Date(stored!.scheduled_for).getTime()).toBe(new Date(local).getTime());
    expect((await storedPost(request, post)).draft).toBe(true);

    // A metadata save with a schedule standing leaves it standing.
    await page.getByTestId('post-edit-description').fill('after');
    await page.getByTestId('post-edit-save').click();
    await expect(page.getByTestId('post-edit-body')).toBeHidden({ timeout: 15_000 });
    expect((await storedSchedule(request, post))!.id, 'Save must not touch the schedule').toBe(stored!.id);

    // PERSISTED: a reload and a fresh open show the same row.
    await page.reload();
    await page.locator('[aria-label="Post actions"]').first().click();
    await page.getByTestId('post-edit').click();
    await expect(page.getByTestId('post-edit-schedule-pending')).toBeVisible({ timeout: 15_000 });
    expect(await reallyVisible(page, 'post-edit-schedule-cancel')).toBe(true);
    await page.getByTestId('post-edit-schedule-cancel').scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath('post-schedule-editor-desktop.png') });

    // CANCEL, from the editor.
    const delDone = page.waitForResponse(
      (r) => r.url().includes('/publication-schedule') && r.request().method() === 'DELETE',
    );
    await page.getByTestId('post-edit-schedule-cancel').click();
    expect((await delDone).status()).toBe(200);
    await expect(page.getByTestId('post-edit-schedule-at')).toBeVisible({ timeout: 10_000 });
    expect(await storedSchedule(request, post)).toBeNull();
    expect((await storedPost(request, post)).draft, 'cancelling leaves the draft a draft').toBe(true);

    // The explicit publish control still works beside it, and once
    // published the picker is gone (nothing to schedule).
    await page.getByTestId('post-edit-publish-toggle').click();
    await expect(page.getByTestId('post-edit-schedule-at')).toHaveCount(0, { timeout: 10_000 });
    expect((await storedPost(request, post)).draft).toBe(false);

    expect(watched.errors, 'no console or runtime errors').toEqual([]);
    expect(watched.httpErrors, 'no failed request other than a missing preview rung').toEqual([]);
  });

  // ── ARM 4: /create, the happy path and the partial failure. ────────
  test('/create schedules a draft, and a failed schedule leaves exactly one unscheduled draft', async ({
    page,
    request,
  }, testInfo) => {
    const me = await bootstrapAdminRef(page);

    // ── the happy path ───────────────────────────────────────────
    const watched = watchPage(page, [/PUT \/api\/v1\/posts\/[0-9a-f-]+\/publication-schedule/]);
    await page.goto('/create');
    await expect(page.locator(tid('create-page'))).toBeVisible();
    const created = page.waitForResponse(
      (r) => r.url().includes('/api/v1/assets') && r.request().method() === 'POST' && r.ok(),
      { timeout: 30_000 },
    );
    await page.locator(tid('create-file-input')).setInputFiles({
      name: 'ps1119-happy.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(`ps1119 happy ${STAMP} ${Math.random()}`),
    });
    madeAssets.push(((await (await created).json()) as { id: string }).id);
    await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });
    const happyTitle = `ps1119 create happy ${STAMP}`;
    await page.locator(tid('create-title')).fill(happyTitle);

    // The two-action buttons are untouched; scheduling is a disclosure.
    await expect(page.locator(tid('create-publish'))).toBeVisible();
    await expect(page.locator(tid('create-save-draft'))).toBeVisible();
    const details = page.locator(tid('create-schedule'));
    await details.locator('summary').click();
    await expect(details).toHaveJSProperty('open', true);
    const local = localInAnHour();
    await page.locator(tid('create-schedule-at')).fill(local);
    await expect(page.locator(tid('create-schedule-submit'))).toBeEnabled();
    await page.locator(tid('create-schedule-submit')).scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath('post-schedule-create-desktop.png') });
    await page.locator(tid('create-schedule-submit')).click();
    await page.waitForURL(/\/posts\/[0-9a-f-]{36}/, { timeout: 30_000 });
    const happyPost = page.url().split('/posts/')[1];
    madePosts.push(happyPost);

    const happy = await storedPost(request, happyPost);
    expect(happy.draft, 'publish later makes a DRAFT').toBe(true);
    const happySched = await storedSchedule(request, happyPost);
    expect(happySched).not.toBeNull();
    expect(happySched!.created_by).toBe(me);
    expect(new Date(happySched!.scheduled_for).getTime()).toBe(new Date(local).getTime());

    // ── the partial failure ──────────────────────────────────────
    // The SECOND durable operation fails on the wire, after the first
    // has succeeded on the server.
    let putSeen = 0;
    await page.route(/\/api\/v1\/posts\/[0-9a-f-]+\/publication-schedule$/, async (route) => {
      if (route.request().method() === 'PUT') {
        putSeen += 1;
        await route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'scheduler unavailable (ps1119 fault injection)' }),
        });
        return;
      }
      await route.continue();
    });

    await page.goto('/create');
    const created2 = page.waitForResponse(
      (r) => r.url().includes('/api/v1/assets') && r.request().method() === 'POST' && r.ok(),
      { timeout: 30_000 },
    );
    await page.locator(tid('create-file-input')).setInputFiles({
      name: 'ps1119-fail.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(`ps1119 fail ${STAMP} ${Math.random()}`),
    });
    madeAssets.push(((await (await created2).json()) as { id: string }).id);
    await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });
    const failTitle = `ps1119 create fail ${STAMP}`;
    await page.locator(tid('create-title')).fill(failTitle);
    await page.locator(tid('create-schedule')).locator('summary').click();
    await page.locator(tid('create-schedule-at')).fill(localInAnHour());
    const postMade = page.waitForResponse(
      (r) => new URL(r.url()).pathname.endsWith('/api/v1/posts') && r.request().method() === 'POST',
      { timeout: 30_000 },
    );
    await page.locator(tid('create-schedule-submit')).click();
    expect((await postMade).status(), 'the draft is created before the schedule is attempted').toBeLessThan(300);

    // HONEST REPORT, IN PLACE: no navigation, the draft named.
    const failed = page.getByTestId('create-schedule-failed');
    await expect(failed).toBeVisible({ timeout: 15_000 });
    await expect(failed).toContainText('ps1119 fault injection');
    expect(new URL(page.url()).pathname, 'the page stays put with the report').toBe('/create');
    expect(putSeen, 'exactly one schedule attempt was made').toBe(1);

    // THE ROWS. Exactly one draft, no schedule, not published.
    const ids = await draftsTitled(request, failTitle);
    expect(ids, 'exactly ONE draft carries the failed create’s title').toHaveLength(1);
    const failPost = ids[0];
    madePosts.push(failPost);
    expect((await storedPost(request, failPost)).draft).toBe(true);
    expect(await storedSchedule(request, failPost), 'ZERO schedule survives a failed PUT').toBeNull();
    // And the page holds nothing that could make another: the rows are gone.
    await expect(page.locator(tid('create-file-row'))).toHaveCount(0);
    await failed.scrollIntoViewIfNeeded();
    await page.screenshot({ path: testInfo.outputPath('post-schedule-create-failed.png') });

    // RECOVERY, the way the page offers it: from the persisted draft,
    // with the fault lifted. Still one post afterwards.
    await page.unroute(/\/api\/v1\/posts\/[0-9a-f-]+\/publication-schedule$/);
    await page.getByTestId('create-schedule-failed-open').click();
    await page.waitForURL(new RegExp(`/posts/${failPost}`), { timeout: 15_000 });
    await page.locator('[aria-label="Post actions"]').first().click();
    await page.getByTestId('post-edit').click();
    await expect(page.getByTestId('post-edit-body')).toBeVisible({ timeout: 15_000 });
    await page.getByTestId('post-edit-schedule-at').fill(localInAnHour());
    const recovered = page.waitForResponse(
      (r) => r.url().includes('/publication-schedule') && r.request().method() === 'PUT',
    );
    await page.getByTestId('post-edit-schedule-submit').click();
    expect((await recovered).status()).toBe(200);
    await expect(page.getByTestId('post-edit-schedule-pending')).toBeVisible({ timeout: 10_000 });
    expect(await draftsTitled(request, failTitle), 'recovery made no second post').toHaveLength(1);
    expect((await storedSchedule(request, failPost))!.created_by).toBe(me);

    expect(watched.errors, 'no console or runtime errors').toEqual([]);
    expect(
      watched.httpErrors,
      'no failed request other than a missing preview rung (the injected 503 is allowed)',
    ).toEqual([]);
  });

  // ── ARM 5: the controls. Publish now and Save as draft never schedule. ──
  test('immediate publish and ordinary draft-save write no schedule', async ({ page, request }) => {
    for (const [button, wantDraft] of [
      ['create-publish', false],
      ['create-save-draft', true],
    ] as const) {
      const puts: string[] = [];
      const onReq = (req: { method(): string; url(): string }) => {
        if (req.method() === 'PUT' && req.url().includes('/publication-schedule')) puts.push(req.url());
      };
      page.on('request', onReq);
      await page.goto('/create');
      const created = page.waitForResponse(
        (r) => r.url().includes('/api/v1/assets') && r.request().method() === 'POST' && r.ok(),
        { timeout: 30_000 },
      );
      await page.locator(tid('create-file-input')).setInputFiles({
        name: `ps1119-${button}.txt`,
        mimeType: 'text/plain',
        buffer: Buffer.from(`ps1119 ${button} ${STAMP} ${Math.random()}`),
      });
      madeAssets.push(((await (await created).json()) as { id: string }).id);
      await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });
      // Filling the picker but pressing the ORDINARY button must not
      // schedule: the instruction rides only its own button.
      await page.locator(tid('create-schedule')).locator('summary').click();
      await page.locator(tid('create-schedule-at')).fill(localInAnHour());
      await page.locator(tid(button)).click();
      await page.waitForURL(/\/posts\/[0-9a-f-]{36}/, { timeout: 30_000 });
      const postId = page.url().split('/posts/')[1];
      madePosts.push(postId);
      page.off('request', onReq);

      expect((await storedPost(request, postId)).draft, `${button} sets draft=${wantDraft}`).toBe(wantDraft);
      expect(await storedSchedule(request, postId), `${button} writes no schedule`).toBeNull();
      expect(puts, `${button} sends no schedule request`).toEqual([]);
    }
  });

  // ── ARM 6: 390px. ─────────────────────────────────────────────────
  test('the editor and /create scheduling are reachable and unclipped at 390px', async ({
    browser,
    request,
  }, testInfo) => {
    const asset = await makeAsset(request, `ps1119 narrow ${STAMP}`);
    madeAssets.push(asset);
    const post = await makeDraft(request, `ps1119 narrow ${STAMP}`, asset);
    madePosts.push(post);

    const ctx = await browser.newContext({
      storageState: ADMIN_STATE_PATH,
      viewport: { width: 390, height: 844 },
      hasTouch: true,
      isMobile: true,
    });
    const page = await ctx.newPage();
    const watched = watchPage(page);
    try {
      await openEditor(page, post);
      for (const id of ['post-edit-schedule', 'post-edit-schedule-at', 'post-edit-schedule-submit', 'post-edit-save']) {
        await page.getByTestId(id).scrollIntoViewIfNeeded();
        expect(await reallyVisible(page, id), `${id} must be visible all the way up at 390px`).toBe(true);
        const box = await page.getByTestId(id).boundingBox();
        expect(box, `${id} has a box`).not.toBeNull();
        expect(box!.x, `${id} starts on screen`).toBeGreaterThanOrEqual(0);
        expect(box!.x + box!.width, `${id} must not run off a 390px screen`).toBeLessThanOrEqual(390);
      }
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      );
      expect(overflow, 'no horizontal overflow with the editor open').toBeLessThanOrEqual(0);
      await page.getByTestId('post-edit-schedule-at').fill(localInAnHour());
      await page.getByTestId('post-edit-schedule-submit').click();
      await expect(page.getByTestId('post-edit-schedule-pending')).toBeVisible({ timeout: 10_000 });
      await page.getByTestId('post-edit-schedule-pending').scrollIntoViewIfNeeded();
      await page.screenshot({ path: testInfo.outputPath('post-schedule-editor-390.png') });
      expect((await storedSchedule(request, post))?.state).toBe('pending');

      await page.goto('/create');
      await expect(page.locator(tid('create-page'))).toBeVisible();
      await page.locator(tid('create-schedule')).scrollIntoViewIfNeeded();
      await page.locator(tid('create-schedule')).locator('summary').click();
      for (const id of ['create-schedule-at', 'create-schedule-submit', 'create-publish', 'create-save-draft']) {
        await page.locator(tid(id)).scrollIntoViewIfNeeded();
        const box = await page.locator(tid(id)).boundingBox();
        expect(box, `${id} has a box`).not.toBeNull();
        expect(box!.x + box!.width, `${id} must not run off a 390px screen`).toBeLessThanOrEqual(390);
      }
      const overflow2 = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      );
      expect(overflow2, 'no horizontal overflow on /create').toBeLessThanOrEqual(0);
      await page.locator(tid('create-schedule-submit')).scrollIntoViewIfNeeded();
      await page.screenshot({ path: testInfo.outputPath('post-schedule-create-390.png') });

      expect(watched.errors, 'no console or runtime errors at 390px').toEqual([]);
      expect(watched.httpErrors).toEqual([]);
    } finally {
      await ctx.close();
    }
  });
});
