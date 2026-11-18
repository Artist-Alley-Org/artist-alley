// comment-edit-1572.spec.ts
//
// #1572: a comment's author edits it in place from the post thread.
//
//   - The author is offered Edit on their own comment; saving replaces
//     the text with the server's answer and shows the "(edited)" marker,
//     which survives a reload (the server stored it, nothing local).
//   - Another signed-in user sees the comment but is offered no Edit, and
//     the server refuses their PATCH (403): nobody but the author edits a
//     comment, a moderator included.
//   - A blank edit cannot be saved.

import type { APIRequestContext, Browser } from '@playwright/test';
import { test, expect } from '../../helpers/test';
import { LOGGED_OUT } from '../../helpers/auth';
import { seededPrincipal } from '../../helpers/seeded-principal';
import { tid } from '../../helpers/testids';

const STAMP = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
const OTHER = seededPrincipal('ilse.varga');

async function json(r: { json(): Promise<unknown> }): Promise<Record<string, unknown>> {
  return (await r.json()) as Record<string, unknown>;
}

/** A published org-only post with one text asset, readable by every
 *  signed-in account. */
async function makePost(request: APIRequestContext, title: string): Promise<{ post: string; asset: string }> {
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`comment-edit-1572 bytes for "${title}" ${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), `uploading bytes for "${title}"`).toBe(201);
  const a = await request.post('/api/v1/assets', {
    data: { title, asset_type: 2, file_extension: 'txt', file_hash: String((await json(up)).hash), original_filename: 'ce1572.txt' },
  });
  expect(a.status(), `creating asset "${title}"`).toBe(201);
  const asset = String((await json(a)).id);
  const p = await request.post('/api/v1/posts', {
    data: { title, description: 'ce1572', visibility: 'org-only', members: [{ asset_id: asset }] },
  });
  expect(p.status(), `creating post "${title}"`).toBe(201);
  return { post: String((await json(p)).id), asset };
}

async function signIn(browser: Browser, user: { username: string; password: string }) {
  const ctx = await browser.newContext({ storageState: LOGGED_OUT });
  const page = await ctx.newPage();
  await page.goto('/login');
  await page.locator(tid('login-username')).fill(user.username);
  await page.locator(tid('login-password')).fill(user.password);
  await page.locator(tid('login-submit')).click();
  await page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 20_000 });
  return { ctx, page };
}

const made: Array<{ post: string; asset: string }> = [];

test.describe('#1572: the author edits their own comment', () => {
  test.afterAll(async ({ request }) => {
    for (const m of made.splice(0)) {
      await request.delete(`/api/v1/posts/${m.post}`).catch(() => undefined);
      await request.delete(`/api/v1/assets/${m.asset}`).catch(() => undefined);
    }
  });

  test('Edit replaces the text in place, shows "(edited)", and only the author is offered it', async ({
    page,
    browser,
    request,
  }, testInfo) => {
    const m = await makePost(request, `ce1572 ${STAMP}`);
    made.push(m);
    const original = `original words ${STAMP}`;
    const c = await request.post(`/api/v1/posts/${m.post}/comments`, { data: { body: original } });
    expect(c.status(), 'posting the comment').toBe(201);
    const commentId = String((await json(c)).id);

    await page.goto(`/posts/${m.post}`);
    const row = page.locator(`[data-testid="comment-row"][data-comment-id="${commentId}"]`);
    await expect(row.getByTestId('comment-body')).toHaveText(original, { timeout: 15_000 });
    await expect(row.getByText('(edited)')).toHaveCount(0);

    await row.getByTestId('comment-edit').click();
    const input = row.getByTestId('comment-edit-input');
    await expect(input).toHaveValue(original);
    await input.fill('   ');
    await expect(row.getByTestId('comment-edit-save')).toBeDisabled();
    const edited = `edited words ${STAMP}`;
    await input.fill(edited);
    const saved = page.waitForResponse(
      (r) => r.request().method() === 'PATCH' && r.url().endsWith(`/api/v1/comments/${commentId}`),
    );
    await row.getByTestId('comment-edit-save').click();
    expect((await saved).status()).toBe(200);
    await expect(row.getByTestId('comment-body')).toHaveText(edited);
    await expect(row.getByText('(edited)')).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath('comment-edit-1572-author.png') });

    // Stored, not local: a reload shows the edit and the marker.
    await page.reload();
    await expect(row.getByTestId('comment-body')).toHaveText(edited, { timeout: 15_000 });
    await expect(row.getByText('(edited)')).toBeVisible();

    // Another user: sees it, is offered no Edit, and is refused by the API.
    const other = await signIn(browser, OTHER);
    try {
      await other.page.goto(`/posts/${m.post}`);
      const otherRow = other.page.locator(`[data-testid="comment-row"][data-comment-id="${commentId}"]`);
      await expect(otherRow.getByTestId('comment-body')).toHaveText(edited, { timeout: 15_000 });
      await expect(otherRow.getByTestId('comment-edit')).toHaveCount(0);
      const refused = await other.page.request.patch(`/api/v1/comments/${commentId}`, { data: { body: 'not mine' } });
      expect(refused.status()).toBe(403);
      await other.page.screenshot({ path: testInfo.outputPath('comment-edit-1572-other.png') });
    } finally {
      await other.ctx.close();
    }
    const after = await request.get(`/api/v1/posts/${m.post}/comments`);
    expect(JSON.stringify(await after.json())).toContain(edited);
  });
});
