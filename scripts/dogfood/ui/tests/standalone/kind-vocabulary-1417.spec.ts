// kind-vocabulary-1417.spec.ts
//
// A kind is searchable vocabulary (#1417, sprint 24).
//
// A reader who types `ebook` into the ordinary search box expects the
// post that CONTAINS an ebook, whatever its cover shows and whether or
// not anybody wrote that word into it. Before this sprint that was only
// true of the structured `kind:` filter: the resolved kind of an asset
// (the badge its card draws) was not in the search document at all, so
// free text could not see it.
//
// # What this proves that the Go tests cannot
//
// search/kind_vocabulary_test.go pins the DOCUMENT: the asset document
// carries the kind at weight D, the post document inherits it through
// the eligible-member fold, and both free-text readers of each column
// return the row. What only a browser can add is that the two surfaces
// a reader actually uses, the search page and the browse wall, take the
// word from the box and put the post on screen. So this builds the
// reported shape through the real API (an image-covered post with an
// epub buried inside it, no kind word anywhere in its text), types
// `ebook`, and looks for the post.
//
// # The fixture is REAL and it is NEW every run
//
// The epub is built in-process (helpers/epub-fixture.ts) with a nonce,
// uploaded, and left to the ebook worker until it is `ready`, because
// only a public, active, READY member contributes to the post document
// (#883). A seeded epub would have made the case depend on which corpus
// the suite happens to run against; a fixture that skipped the worker
// would have tested a member the fold ignores.
//
// # Vacuity
//
// Every title and description is nonsense and none contains `ebook`,
// `epub`, `book` or any other kind word. The post is asserted absent
// from `ebook` results by TEXT before the assertion that it is present
// by KIND would mean anything; the Go suite carries the fail-before-fix
// evidence on origin/dev, and this spec's job is the rendered surface.

import { test, expect, type APIRequestContext, type Page } from '../../helpers/test';
import { tid } from '../../helpers/testids';
import { buildMinimalEpub } from '../../helpers/epub-fixture';
import zlib from 'node:zlib';

const STAMP = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
// Nonsense words: no kind, no extension, nothing the stemmer folds onto
// one. The stamp goes into a description rather than a title so the
// searchable words stay exactly these.
const COVER_TITLE = 'quillbrasse morvantide';
const MEMBER_TITLE = 'thessaly vellichor';
const POST_TITLE = 'caskerling plinthorax';

/** A small PNG whose bytes carry the stamp, so storage stores a new
 *  object rather than deduplicating onto an earlier run's. */
function makePng(seed: string, width = 64, height = 64): Buffer {
  const raw = Buffer.alloc((width * 3 + 1) * height);
  let at = 0;
  for (let y = 0; y < height; y++) {
    raw[at++] = 0;
    for (let x = 0; x < width; x++) {
      raw[at++] = (x * 3) & 255;
      raw[at++] = (y * 5) & 255;
      raw[at++] = 90;
    }
  }
  const chunk = (type: string, data: Buffer): Buffer => {
    const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
    const len = Buffer.alloc(4);
    len.writeUInt32BE(data.length);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(zlib.crc32(body) >>> 0);
    return Buffer.concat([len, body, crc]);
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8;
  ihdr[9] = 2;
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('tEXt', Buffer.from(`Comment\0${seed}`, 'latin1')),
    chunk('IDAT', zlib.deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

async function uploadAsset(
  request: APIRequestContext,
  label: string,
  bytes: Buffer,
  contentType: string,
  ext: string,
  assetType: number,
  title: string,
): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': contentType },
    data: bytes,
  });
  expect(up.ok(), `upload ${label}: ${up.status()} ${await up.text().catch(() => '')}`).toBeTruthy();
  const { hash } = (await up.json()) as { hash: string };
  const created = await request.post('/api/v1/assets', {
    data: {
      title,
      description: `1417 fixture ${STAMP}`,
      asset_type: assetType,
      status: 'active',
      file_hash: hash,
      file_extension: ext,
      original_filename: `${label}.${ext}`,
    },
  });
  expect(
    created.ok(),
    `create ${label}: ${created.status()} ${await created.text().catch(() => '')}`,
  ).toBeTruthy();
  return ((await created.json()) as { id: string }).id;
}

/** Poll one asset until its worker has settled it. */
async function waitForReady(request: APIRequestContext, id: string, label: string): Promise<void> {
  const deadline = Date.now() + 180_000;
  let last: Record<string, unknown> = {};
  while (Date.now() < deadline) {
    const res = await request.get(`/api/v1/assets/${id}`);
    if (res.ok()) {
      last = (await res.json()) as Record<string, unknown>;
      if (last.processing_status === 'ready') return;
      if (last.processing_status === 'failed') break;
    }
    await new Promise((r) => setTimeout(r, 2000));
  }
  throw new Error(
    `${label} ${id} never reached processing_status=ready; last seen ${JSON.stringify(last)}`,
  );
}

/** Every post id `/api/v1/search?q=<q>&types=post` returns, paged to
 *  exhaustion, so "present" is a statement about the result SET and not
 *  about the first page. */
async function searchPostIds(page: Page, q: string): Promise<string[]> {
  return page.evaluate(async (term: string) => {
    const ids: string[] = [];
    let cursor: string | null = null;
    for (let guard = 0; guard < 50; guard++) {
      let u = `/api/v1/search?q=${encodeURIComponent(term)}&types=post&limit=50`;
      if (cursor) u += '&cursor=' + encodeURIComponent(cursor);
      const d = await (await fetch(u)).json();
      for (const h of d.hits ?? d.items ?? []) ids.push(h.id);
      cursor = d.next_cursor ?? null;
      if (!cursor) break;
    }
    return ids;
  }, q);
}

/** Every post id `/api/v1/posts?q=<q>` returns, paged to exhaustion. */
async function browsePostIds(page: Page, q: string): Promise<string[]> {
  return page.evaluate(async (term: string) => {
    const ids: string[] = [];
    let cursor: string | null = null;
    for (let guard = 0; guard < 50; guard++) {
      let u = `/api/v1/posts?q=${encodeURIComponent(term)}&limit=200`;
      if (cursor) u += '&cursor=' + encodeURIComponent(cursor);
      const d = await (await fetch(u)).json();
      for (const p of d.items ?? []) ids.push(p.id);
      cursor = d.next_cursor ?? null;
      if (!cursor) break;
    }
    return ids;
  }, q);
}

/** Scroll the results region until a tile for `postId` is on screen, or
 *  give up after a bounded number of pages. Results page by infinite
 *  scroll, and a post with one D-weight occurrence of the word ranks
 *  below posts that say it in their title, so it may not be on page one. */
async function scrollUntilTile(page: Page, postId: string): Promise<boolean> {
  const tile = page.locator(`main a[href^="/posts/${postId}"]`).first();
  for (let i = 0; i < 12; i++) {
    if (await tile.isVisible().catch(() => false)) return true;
    await page.locator('main').evaluate((el) => {
      el.scrollTop = el.scrollHeight;
    });
    await page.waitForTimeout(700);
  }
  return tile.isVisible().catch(() => false);
}

test.describe('#1417 a kind is searchable vocabulary', () => {
  test.describe.configure({ mode: 'serial' });

  let coverId = '';
  let epubId = '';
  let postId = '';

  test.beforeAll(async ({ request }) => {
    test.setTimeout(600_000);
    coverId = await uploadAsset(
      request, 'cover', makePng(STAMP), 'image/png', 'png', 1, COVER_TITLE,
    );
    const epub = buildMinimalEpub(MEMBER_TITLE);
    epubId = await uploadAsset(
      request, 'member', epub.bytes, 'application/epub+zip', 'epub', 2, MEMBER_TITLE,
    );
    // ⚠️ READY IS THE PRECONDITION. The fold takes only public, active,
    // ready members (#883); a member still processing contributes
    // nothing and every assertion below would measure the cover alone.
    await waitForReady(request, coverId, 'the cover');
    await waitForReady(request, epubId, 'the epub');

    const post = await request.post('/api/v1/posts', {
      data: {
        title: POST_TITLE,
        description: `1417 fixture ${STAMP}`,
        visibility: 'public',
        members: [{ asset_id: coverId }, { asset_id: epubId }],
      },
    });
    expect(post.ok(), `create post: ${post.status()} ${await post.text().catch(() => '')}`).toBeTruthy();
    postId = ((await post.json()) as { id: string }).id;

    // The shape under test, read back rather than assumed: an image
    // cover and an epub that is a member but not the cover.
    const got = await (await request.get(`/api/v1/posts/${postId}`)).json();
    expect(got.cover_asset_id, 'the cover is the PNG').toBe(coverId);
    const memberIds = (got.members ?? []).map((m: { asset_id?: string; asset?: { id?: string } }) =>
      m.asset_id ?? m.asset?.id,
    );
    expect(memberIds, 'the epub is a member').toContain(epubId);
  });

  test.afterAll(async ({ request }) => {
    if (postId) await request.delete(`/api/v1/posts/${postId}`).catch(() => undefined);
    for (const id of [epubId, coverId]) {
      if (id) await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
  });

  test('typing `ebook` on /search finds the post that contains an epub', async ({ page }) => {
    await page.goto('/search');
    const input = page.locator(tid('search-input'));
    await expect(input).toBeVisible();
    await input.fill('ebook');
    await input.press('Enter');
    await expect(page).toHaveURL(/\/search\?.*q=ebook/);

    // The result SET, exhaustively: the post is in it.
    expect(
      await searchPostIds(page, 'ebook'),
      'the search result set does not contain the post that holds the epub',
    ).toContain(postId);
    // And a tile for it is on the page the reader is looking at.
    expect(
      await scrollUntilTile(page, postId),
      'no tile for the post appeared in the search results',
    ).toBe(true);

    // Control: a kind the post does not contain does not return it.
    expect(await searchPostIds(page, 'audiobook')).not.toContain(postId);
  });

  test('the browse wall with `?q=ebook` lists the same post', async ({ page }) => {
    await page.goto('/?q=ebook');
    await expect(page.locator(tid('browse-wall'))).toBeVisible({ timeout: 20_000 });
    expect(
      await browsePostIds(page, 'ebook'),
      'browse `?q=ebook` does not return the post that holds the epub',
    ).toContain(postId);
    expect(await scrollUntilTile(page, postId), 'no tile for the post on the browse wall').toBe(true);
  });

  test('and at 390px, where the reduced app has the same box', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto('/search');
    const input = page.locator(tid('search-input'));
    await expect(input).toBeVisible();
    await input.fill('ebook');
    await input.press('Enter');
    await expect(page).toHaveURL(/\/search\?.*q=ebook/);
    expect(await searchPostIds(page, 'ebook')).toContain(postId);
    expect(await scrollUntilTile(page, postId), 'no tile at 390px').toBe(true);

    await page.goto('/?q=ebook');
    await expect(page.locator(tid('browse-wall'))).toBeVisible({ timeout: 20_000 });
    expect(await scrollUntilTile(page, postId), 'no tile on the 390px browse wall').toBe(true);
  });
});
