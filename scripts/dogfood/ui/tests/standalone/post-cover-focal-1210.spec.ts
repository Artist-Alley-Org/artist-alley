// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1210 + #1209: a post cover's focal point, and the picker warning
// that stopped guessing.
//
// WHY EVERY ASSERTION HERE IS ON A PERSISTED VALUE OR A COMPUTED STYLE,
// and never on the form the value was typed into. #1210's whole
// substance is that a fraction chosen in a dialog changes which part of
// a picture a browse tile paints. A test that PATCHed a hand-written
// body and read it back would prove the column exists and nothing about
// whether an author can reach the setting or whether the tile obeys it.
// So this drives the REAL dialog from the REAL post menu, DRAGS the
// marquee with trusted pointer events, and then reads the browse grid's
// own `getComputedStyle`.
//
// THE THREE THINGS THAT WOULD SHIP WRONG, each with a test:
//
//   1. The framing applied to the wrong SOURCE. `col` is a 320x320
//      square the server already cropped at the picture's centre, so
//      `object-position` over it moves a crop of a crop. A framed tile
//      has to draw from a CONTAIN rung, and its srcset must not offer
//      the browser the square as an alternative for a narrow viewport.
//   2. The framing leaking into a mode that does not crop. Only grid
//      does: `fill` is `object-fit: cover` on a square frame, and
//      masonry / feed / thumbnail letterbox the whole picture. A focal
//      point there is meaningless, and the test asserts masonry is
//      untouched rather than merely "looks fine".
//   3. #1209's warning firing with the WRONG SENTENCE. The old copy
//      said "this picture is still a draft", which was the whole truth
//      while `status` was all the client could read. It is now one of
//      four conjuncts, and saying it about a picture whose raster pass
//      FAILED would send the curator looking for a Publish button that
//      is already pressed.
//
// ⚠️ THE BEFORE STATE IS ASSERTED, not assumed. Each rendering test
// reads the tile with no focal point first, so the spec pins the
// TRANSITION: a build that ignored the stored fractions entirely would
// pass an end-state assertion written loosely and fails these.

import zlib from 'node:zlib';

import { expect, test, type Page, type APIRequestContext } from '../../helpers/test';

const STAMP = Date.now();
const TOKEN = `pstfocal${STAMP}`;

let postId = '';
let coverId = '';
/** Bytes that are a valid PNG header and undecodable content, so the
 *  raster worker fails them. See provisionBrokenAsset. */
let brokenId = '';
let collectionId = '';
/** Every asset this run creates, newest last. The two describes delete
 *  their own by id; this stays as the record of what was made, so a
 *  future arm cannot add a fixture without a line here. */
const provisioned: string[] = [];

/** A real PNG with the subject in a KNOWN corner, unique per run.
 *
 *  Unique because storage is content-addressed: two runs uploading
 *  identical bytes resolve to the same object, and the second would be
 *  riding whatever renditions and state the first left behind. The noise
 *  sits in the low three bits so the picture stays readable, which is
 *  what makes a failure diagnosable from the screenshot Playwright keeps.
 *
 *  2.4:1 AND the disc in the far LEFT THIRD, both deliberate. A square
 *  destination crops a 2.4:1 source on its width, so there is real
 *  horizontal travel to drag; and a subject outside the centre square is
 *  the only fixture where "the crop moved" and "the crop happened to
 *  look fine" are distinguishable.
 */
function makePng(seed: string, width = 480, height = 200): Buffer {
  let s = 2166136261;
  for (let i = 0; i < seed.length; i++) {
    s ^= seed.charCodeAt(i);
    s = Math.imul(s, 16777619) >>> 0;
  }
  if (s === 0) s = 0x9e3779b9;
  const next = () => {
    s ^= s << 13;
    s >>>= 0;
    s ^= s >>> 17;
    s ^= s << 5;
    s >>>= 0;
    return s;
  };
  const cx = Math.round(width * 0.14);
  const cy = Math.round(height * 0.72);
  const r = Math.round(height * 0.23);
  const raw = Buffer.alloc(height * (1 + width * 3));
  let at = 0;
  for (let y = 0; y < height; y++) {
    raw[at++] = 0;
    for (let x = 0; x < width; x++) {
      const inDisc = (x - cx) * (x - cx) + (y - cy) * (y - cy) < r * r;
      const n = next();
      raw[at++] = (inDisc ? 250 : 20) ^ (n & 7);
      raw[at++] = (inDisc ? 140 : 26) ^ ((n >>> 3) & 7);
      raw[at++] = (inDisc ? 30 : 50) ^ ((n >>> 6) & 7);
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
    chunk('IDAT', zlib.deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

async function uploadAsset(
  request: APIRequestContext,
  label: string,
  bytes: Buffer,
): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    headers: {
      'Content-Type': 'application/octet-stream',
      'X-Content-Type': 'image/png',
    },
    data: bytes,
  });
  expect(
    up.ok(),
    `upload ${label}: ${up.status()} ${await up.text().catch(() => '')}`,
  ).toBeTruthy();
  const { hash } = (await up.json()) as { hash: string };
  const created = await request.post('/api/v1/assets', {
    data: {
      title: `${TOKEN} ${label}`,
      asset_type: 1,
      status: 'active',
      file_hash: hash,
      file_extension: 'png',
    },
  });
  expect(
    created.ok(),
    `create ${label}: ${created.status()} ${await created.text().catch(() => '')}`,
  ).toBeTruthy();
  const id = ((await created.json()) as { id: string }).id;
  provisioned.push(id);
  return id;
}

/** Poll one asset until the raster worker has settled it. */
async function waitForProcessing(
  request: APIRequestContext,
  id: string,
  want: 'ready' | 'failed',
): Promise<Record<string, unknown>> {
  const deadline = Date.now() + 180_000;
  let last: Record<string, unknown> = {};
  while (Date.now() < deadline) {
    const res = await request.get(`/api/v1/assets/${id}`);
    if (res.ok()) {
      last = (await res.json()) as Record<string, unknown>;
      if (last.processing_status === want) return last;
    }
    await new Promise((r) => setTimeout(r, 2000));
  }
  throw new Error(
    `asset ${id} never reached processing_status=${want}; last seen ${JSON.stringify(last)}`,
  );
}

test.describe('#1210 a post cover gets a focal point', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    test.setTimeout(600_000);

    coverId = await uploadAsset(request, 'cover', makePng(`${TOKEN}-cover`));

    // ⚠️ THE LADDER IS THE PRECONDITION, NOT AN ASSUMPTION. A focal
    // point is honoured only from a CONTAIN rung, so a fixture whose
    // raster pass has not drained would make every rendering assertion
    // below measure the fallback and report it as the feature.
    const ready = await waitForProcessing(request, coverId, 'ready');
    expect(ready.ladder_available, 'the fixture needs its contain rungs').toBe(true);
    expect(
      ready.anonymously_visible,
      '#1209: an active, public, ready asset is what a stranger sees',
    ).toBe(true);

    const post = await request.post('/api/v1/posts', {
      data: {
        title: `${TOKEN} focal probe`,
        visibility: 'public',
        members: [{ asset_id: coverId, sort_order: 0 }],
      },
    });
    expect(
      post.ok(),
      `create post: ${post.status()} ${await post.text().catch(() => '')}`,
    ).toBeTruthy();
    postId = ((await post.json()) as { id: string }).id;
  });

  // EACH DESCRIBE DELETES WHAT IT MADE, and the split is not tidiness.
  // One module-level cleanup here runs BEFORE the #1209 describe below
  // has provisioned anything, so that describe's collection and its
  // broken asset would survive every run and pile up on a persistent
  // stack. That is the fixture backlog the suite's own ledger exists to
  // catch, and it is easier not to create than to sweep.
  test.afterAll(async ({ request }) => {
    if (postId) await request.delete(`/api/v1/posts/${postId}`).catch(() => undefined);
    if (coverId) await request.delete(`/api/v1/assets/${coverId}`).catch(() => undefined);
  });

  /** The post's grid tile on the browse wall, as the browser resolved
   *  it. The mode lives in localStorage rather than the URL, so it is
   *  set the way the view switcher sets it and the page is reloaded. */
  async function tile(page: Page, mode: 'grid' | 'masonry') {
    await page.goto('/');
    await page.evaluate((m) => localStorage.setItem('aa_browse_mode', m), mode);
    await page.goto(`/?q=${encodeURIComponent(TOKEN)}`);
    const img = page.locator(`img[data-focal]`).first();
    await img.waitFor({ state: 'visible', timeout: 30_000 });
    return img.evaluate((el: HTMLImageElement) => ({
      focal: el.dataset.focal,
      src: el.getAttribute('src') ?? '',
      srcset: el.getAttribute('srcset') ?? '',
      objectPosition: getComputedStyle(el).objectPosition,
      objectFit: getComputedStyle(el).objectFit,
    }));
  }

  async function openCoverDialog(page: Page) {
    await page.goto(`/posts/${postId}`);
    await page.locator('[aria-label="Post actions"]').first().click();
    await page.getByTestId('post-edit-cover').click();
    const dialog = page.getByTestId('post-cover-editor');
    await expect(dialog).toBeVisible();
    return dialog;
  }

  async function storedFocal(request: APIRequestContext) {
    const res = await request.get(`/api/v1/posts/${postId}`);
    expect(res.ok()).toBeTruthy();
    const body = (await res.json()) as {
      cover_focal_x?: number | null;
      cover_focal_y?: number | null;
    };
    return { x: body.cover_focal_x ?? null, y: body.cover_focal_y ?? null };
  }

  test('an unframed post tile is centred, from the pre-cropped square', async ({
    page,
    request,
  }) => {
    expect(await storedFocal(request)).toEqual({ x: null, y: null });
    const before = await tile(page, 'grid');
    expect(before.focal, 'nothing has been framed yet').toBe('off');
    expect(before.objectFit, 'grid is the mode that crops').toBe('cover');
    expect(before.objectPosition).toBe('50% 50%');
    // #1169's arrangement, and the thing the framed case must change:
    // an unframed cropping tile is free to take the cheap square.
    expect(before.srcset).toContain('/variants/col');
  });

  test('dragging the marquee persists a focal pair', async ({ page, request }) => {
    await openCoverDialog(page);

    const marquee = page.getByTestId('post-crop-marquee');
    await expect(marquee).toBeVisible();
    const stage = page.getByTestId('post-crop-stage-box');
    const sbox = (await stage.boundingBox())!;
    const mbox = (await marquee.boundingBox())!;

    // ⚠️ THE STAGE MUST BE THE PICTURE'S OWN SHAPE. If the dialog fell
    // back to `col` the stage would be square, the marquee would fill
    // it, there would be no travel, and the drag below would silently
    // do nothing while every later assertion blamed the renderer.
    expect(
      sbox.width / sbox.height,
      'the crop stage has to show the original 2.4:1 picture, not the col square',
    ).toBeGreaterThan(1.5);
    expect(mbox.width).toBeLessThan(sbox.width * 0.75);

    // A real pointer sequence: `page.mouse` produces trusted events, and
    // a marquee that only moves for dispatched ones is a marquee no
    // author can move.
    await page.mouse.move(mbox.x + mbox.width / 2, mbox.y + mbox.height / 2);
    await page.mouse.down();
    await page.mouse.move(sbox.x + 4, sbox.y + sbox.height - 4, { steps: 25 });
    await page.mouse.up();

    await page.getByTestId('post-cover-save').click();
    await expect(page.getByTestId('post-cover-editor')).toBeHidden();

    const stored = await storedFocal(request);
    expect(stored.x, 'the drag went to the left edge').toBeLessThan(0.2);
    expect(stored.y, 'a square crop of a 2.4:1 picture has no vertical travel').toBeCloseTo(0.5, 3);
  });

  test('the grid tile is painted from a CONTAIN rung with the stored position', async ({
    page,
  }) => {
    const after = await tile(page, 'grid');
    expect(after.focal).toBe('on');
    expect(after.objectFit).toBe('cover');
    expect(after.objectPosition, 'the position the author dragged to').toMatch(
      /^0%|^[0-9.]+% 50%$/,
    );
    expect(after.objectPosition).not.toBe('50% 50%');

    // ⛔ THE SOURCE IS THE POINT. `col` is a centred square crop, so
    // positioning inside it lands somewhere nobody chose, and leaving
    // it in the srcset would make the framing correct at some tile
    // widths and wrong at others, decided by the viewport.
    expect(after.src).not.toContain('/variants/col');
    expect(after.srcset, 'a framed tile must not be offered the square').not.toContain(
      '/variants/col',
    );
    expect(after.srcset).toContain('/variants/');
  });

  test('masonry is untouched: nothing there crops', async ({ page }) => {
    const m = await tile(page, 'masonry');
    expect(m.focal, 'a focal point says nothing about a picture shown whole').toBe('off');
    expect(m.objectFit).toBe('contain');
    expect(m.objectPosition).toBe('50% 50%');
  });

  test('Reset clears the pair rather than re-setting it to the centre', async ({
    page,
    request,
  }) => {
    await openCoverDialog(page);
    const reset = page.getByTestId('post-crop-reset-focal');
    await expect(reset).toBeEnabled();
    await reset.click();
    await page.getByTestId('post-cover-save').click();
    await expect(page.getByTestId('post-cover-editor')).toBeHidden();

    // NULL, not 0.5. The two render identically and are stored
    // differently on purpose: null is "the author never framed this",
    // and losing that distinction is what makes a Reset unrecoverable.
    expect(await storedFocal(request)).toEqual({ x: null, y: null });

    const back = await tile(page, 'grid');
    expect(back.focal).toBe('off');
    expect(back.objectPosition).toBe('50% 50%');
  });

  test('a post cover offers no zoom, because a post stores none', async ({ page }) => {
    await openCoverDialog(page);
    // Collections carry `cover_zoom`; posts do not. A slider whose value
    // is discarded on save is a control the product cannot keep.
    await expect(page.getByTestId('post-crop-zoom')).toHaveCount(0);
    await expect(page.getByTestId('post-crop-marquee')).toBeVisible();
  });
});

// ---------------------------------------------------------------------
// #1209: the picker's warning stops guessing
// ---------------------------------------------------------------------

test.describe('#1209 the cover picker warns about what a stranger sees', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    test.setTimeout(600_000);

    // ⭐ THE CASE THE OLD CHECK COULD NOT SEE. The client could read
    // `status`, so it caught a draft. It could not read the other three
    // conjuncts, and this fixture is `status: 'active'` with a raster
    // pass that FAILS: a valid PNG header over an undecodable IDAT, so
    // the worker rejects it and the row settles at
    // `processing_status = 'failed'`. An anonymous visitor is not shown
    // it, and nothing on the old payload said so.
    const header = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
    const chunk = (type: string, data: Buffer): Buffer => {
      const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
      const len = Buffer.alloc(4);
      len.writeUInt32BE(data.length);
      const crc = Buffer.alloc(4);
      crc.writeUInt32BE(zlib.crc32(body) >>> 0);
      return Buffer.concat([len, body, crc]);
    };
    const ihdr = Buffer.alloc(13);
    ihdr.writeUInt32BE(64, 0);
    ihdr.writeUInt32BE(64, 4);
    ihdr[8] = 8;
    ihdr[9] = 2;
    const junk = Buffer.from(`${TOKEN} not a deflate stream at all`, 'ascii');
    const broken = Buffer.concat([
      header,
      chunk('IHDR', ihdr),
      chunk('IDAT', junk),
      chunk('IEND', Buffer.alloc(0)),
    ]);

    brokenId = await uploadAsset(request, 'broken', broken);
    const settled = await waitForProcessing(request, brokenId, 'failed');
    expect(settled.status, 'the fixture is ACTIVE, which is what makes it the #1209 case').toBe(
      'active',
    );
    expect(settled.anonymously_visible, 'active but not ready: a stranger is not shown it').toBe(
      false,
    );

    const col = await request.post('/api/v1/collections', {
      data: { name: `${TOKEN} warning probe`, visibility: 'public' },
    });
    expect(col.ok()).toBeTruthy();
    collectionId = ((await col.json()) as { id: string }).id;
  });

  test.afterAll(async ({ request }) => {
    if (collectionId)
      await request.delete(`/api/v1/collections/${collectionId}`).catch(() => undefined);
    if (brokenId) await request.delete(`/api/v1/assets/${brokenId}`).catch(() => undefined);
  });

  test('the warning fires for an ACTIVE asset a stranger cannot see', async ({ page }) => {
    await page.goto(`/collections/${collectionId}`);
    await page.getByTestId('collection-detail-more-button').first().click();
    await page.getByTestId('collection-detail-edit-menuitem').first().click();
    await expect(page.getByTestId('collection-cover-section')).toBeVisible();
    await page.getByTestId('collection-cover-edit-button').click();
    await expect(page.getByTestId('collection-cover-editor')).toBeVisible();

    await page.getByTestId('collection-source-mine').click();
    await page.getByTestId('collection-search-input').fill(`${TOKEN} broken`);
    await page.keyboard.press('Enter');
    const choice = page.getByTestId('collection-mine-choice').first();
    await choice.waitFor({ state: 'visible', timeout: 30_000 });
    await choice.click();

    const warn = page.getByTestId('collection-narrow-warning');
    await expect(
      warn,
      'this is precisely the pick the status-only check let through',
    ).toBeVisible();

    // ⛔ AND WITH THE RIGHT SENTENCE. The draft copy offers "Publish
    // it", which is advice this author has already taken.
    await expect(warn).toHaveAttribute('data-warning-kind', 'hidden');
    await expect(warn).not.toContainText('draft');
  });
});
