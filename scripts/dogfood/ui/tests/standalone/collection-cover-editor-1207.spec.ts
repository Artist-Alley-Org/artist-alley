// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1207 — the real cover editor: two slots, a draggable crop marquee,
// and three values that have to survive a round trip.
//
// WHAT THIS SPEC IS FOR, and why every assertion is on a PERSISTED
// value rather than on the form it was typed into. The defect class
// #1195 and #1176 both came from is "the server has always accepted
// this and no client could say it": a test that PATCHes a hand-written
// body proves nothing about whether the curator can reach the setting.
// So this drives the REAL modal — open the collection page, open the
// editor, click the pickers, DRAG the marquee — and then reads the
// values back out of the API. A build whose handler echoed its own
// request body would pass a body assertion and fail this one (#946).
//
// The drag is a real pointer sequence rather than a synthetic event or
// a keyboard nudge. `page.mouse` produces trusted events; a dispatched
// PointerEvent does not, and a marquee that only moves for untrusted
// events is a marquee no curator can move. The keyboard path is
// exercised too, because it is the a11y half of the same control and
// nothing else would notice if it stopped answering.

import zlib from 'node:zlib';

import { expect, test, type Page } from '../../helpers/test';

const STAMP = Date.now();
const COLLECTION_NAME = `#1207 cover editor ${STAMP}`;

interface CollectionPayload {
  id: string;
  cover_asset_id?: string | null;
  featured_cover_asset_id?: string | null;
  featured_cover_focal_x?: number | null;
  featured_cover_focal_y?: number | null;
  cover_focal_x?: number | null;
  cover_focal_y?: number | null;
  featured_cover_zoom?: number | null;
  cover_zoom?: number | null;
}

let collectionId: string | undefined;
let memberIds: string[] = [];
/** Deliberately NOT a member — the #1074 proof needs a picture the
 *  collection does not contain. */
let outsiderId = '';
/** A TALL picture, and the only one here. #1212's defect and #1207's
 *  stage distortion are both properties of a portrait in a wide window,
 *  and every other fixture is wide — a suite that never provisions one
 *  cannot see either. */
let portraitId = '';

/** A real PNG, BUILT rather than pasted, and UNIQUE PER RUN.
 *
 *  Built rather than inlined as base64 or read from a fixture file for a
 *  reason the first version of this spec found the hard way — a
 *  hand-written base64 blob had a bad CRC, the upload succeeded, and the
 *  raster worker failed with `png: invalid format: invalid checksum`.
 *  The asset then sat at `processing_status = 'failed'` and the
 *  anonymous read 404'd, which looks exactly like the tier bug this
 *  spec is for. A generated PNG has correct chunks by construction.
 *
 *  ⚠️ THE `seed` IS THE CONTENT-ADDRESS DEDUPE GUARD, and it is not
 *  decoration. Storage is content-addressed: two runs uploading
 *  identical bytes resolve to the SAME object, `deduped: true`, and on a
 *  persistent stack that object already carries whatever renditions and
 *  history a previous run left it. The status rule under test would then
 *  be riding a pre-existing object rather than the one this run made,
 *  and a green result would say nothing about the rule. So every run's
 *  bytes are distinct.
 *
 *  The noise is in the LOW THREE BITS of each channel, driven by a
 *  seeded xorshift. That makes the bytes unique and the deflate stream
 *  different, while leaving the four quadrants visually intact — they
 *  are what makes a screenshot show unambiguously WHICH part of the
 *  picture the crop kept. Noise in the high bits would have bought the
 *  same uniqueness and thrown the readable fixture away.
 *
 *  Default 2:1: the featured card is 890:500, so a wide source has real
 *  horizontal travel and the marquee is draggable rather than pinned to
 *  a picture that is already the right shape.
 */
function makePng(seed: string, width = 400, height = 200): Buffer {
  // FNV-1a over the seed, then xorshift32. A stdlib-free PRNG because
  // the requirement is "different every run", not cryptographic.
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

  const raw = Buffer.alloc(height * (1 + width * 3));
  let at = 0;
  for (let y = 0; y < height; y++) {
    raw[at++] = 0; // filter: none
    for (let x = 0; x < width; x++) {
      const n = next();
      raw[at++] = (x < width / 2 ? 220 : 40) ^ (n & 7);
      raw[at++] = (y < height / 2 ? 200 : 60) ^ ((n >>> 3) & 7);
      raw[at++] = 120 ^ ((n >>> 6) & 7);
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
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // colour type: truecolour
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

/** Every fixture asset this run creates, newest last. Soft-deleted in
 *  afterAll — including the one the upload test makes, which the test
 *  registers itself. */
const provisioned: string[] = [];

/** The token every fixture title carries, so a search can find exactly
 *  this run's assets and a cleanup can never touch another run's. */
const TOKEN = `cvrfix${STAMP}`;

/** ⚠️ THIS SPEC OWNS THE PUBLIC-MODE SWITCH FOR ITS DURATION.
 *
 *  THE INSTANCE CONFIG IS A FIXTURE TOO, and forgetting that cost two CI
 *  rounds. Half the assertions here are the ANONYMOUS story — a visitor
 *  reading back an uploaded cover, the anonymous rail rendering the
 *  chosen one — and on an install with anonymous browsing off every one
 *  of those reads 401s before any asset state is consulted. The dev
 *  stack has it on and CI's dogfood stack does not, so a spec that
 *  assumed it passed locally and failed there for a reason that had
 *  nothing to do with the assets it was blaming.
 *
 *  ENABLED, not skipped. A skip-when-off would delete exactly the
 *  coverage this feature needs: "the curator's cover reaches a visitor"
 *  is the claim, and there is no visitor with the switch off.
 *
 *  Read in beforeAll and PUT BACK in afterAll — never assumed, never
 *  left flipped. Same shape collection-public-tier-1195 uses, which is
 *  the spec that established it and passes on CI.
 *
 *  No cache dance is needed: the admin write path calls
 *  InvalidatePublicMode BEFORE it returns (sysconfig/handler.go:799),
 *  so a 200 from the PATCH means the flag is already live for the next
 *  request on every node. Restart-free by construction. */
let priorPublicMode: boolean | undefined;

test.describe('#1207 the collection cover editor', () => {
  test.describe.configure({ mode: 'serial' });

  /** Upload bytes and create the asset row, the way the app does it:
   *  POST /storage/objects with a raw octet-stream and an X-Content-Type,
   *  then POST /assets pointing at the returned hash.
   *
   *  `status: 'active'` is not a convenience — it is THE RULE THIS SPEC
   *  IS ABOUT, applied to its own fixtures. The anonymous asset
   *  predicate wants `status = 'active'` AND `sensitivity = 'public'`
   *  (the column default, and no API writes it) AND
   *  `processing_status = 'ready'`. A fixture created as `draft`, which
   *  is what the upload queue does for an ordinary file, is invisible to
   *  the visitor whose view half these tests assert.
   *
   *  `asset_type: 1` is Photo, the value every real upload sends
   *  (upload.svelte.ts's DEFAULT_ASSET_TYPE). There is no asset-types
   *  route to look it up from; if it were wrong here it would be wrong
   *  for every upload on the instance, and the expect below would say so
   *  rather than the test skipping.
   */
  async function provisionAsset(
    request: import('@playwright/test').APIRequestContext,
    label: string,
    width: number,
    height: number,
  ): Promise<string> {
    const bytes = makePng(`${TOKEN}-${label}`, width, height);
    const up = await request.post('/api/v1/storage/objects', {
      headers: {
        'Content-Type': 'application/octet-stream',
        'X-Content-Type': 'image/png',
      },
      data: bytes,
    });
    expect(
      up.ok(),
      `could not upload fixture bytes for ${label}: ${up.status()} ${await up.text().catch(() => '')}`,
    ).toBeTruthy();
    const { hash } = (await up.json()) as { hash: string; deduped?: boolean };

    const created = await request.post('/api/v1/assets', {
      data: {
        title: `${TOKEN} ${label}`,
        asset_type: 1,
        status: 'active',
        file_hash: hash,
        file_extension: 'png',
        mature: false,
      },
    });
    expect(
      created.ok(),
      `could not create fixture asset ${label}: ${created.status()} ${await created.text().catch(() => '')}`,
    ).toBeTruthy();
    const id = ((await created.json()) as { id: string }).id;
    provisioned.push(id);
    return id;
  }

  async function setPublicMode(
    request: import('@playwright/test').APIRequestContext,
    on: boolean,
  ) {
    const r = await request.patch('/api/v1/admin/system/public-mode', { data: { enabled: on } });
    expect(r.status(), `public mode must be settable to ${on}`).toBe(200);
    // The response is the stored value, so this also confirms the write
    // landed rather than merely being accepted.
    expect(((await r.json()) as { enabled: boolean }).enabled).toBe(on);
  }

  test.beforeAll(async ({ request, browser }) => {
    // THE CONFIG FIXTURE, FIRST — before any asset exists, because the
    // provisioning check below reads anonymously and would otherwise be
    // measuring the switch rather than the assets.
    const mode = await request.get('/api/v1/admin/system/public-mode');
    expect(mode.status(), 'public-mode state must be readable as admin').toBe(200);
    priorPublicMode = ((await mode.json()) as { enabled: boolean }).enabled;
    if (!priorPublicMode) await setPublicMode(request, true);

    // ⚠️ THE SPEC BUILDS ITS OWN FIXTURES. It used to pick two assets
    // out of the seeded library and check they were anonymously
    // picturable; that passed on the dev stack and failed on CI with
    // "Expected: 2, Received: 0", because the CI seed profile contains
    // no anonymously-picturable raster asset at all. A test whose
    // precondition is a property of somebody else's seed data is a test
    // that reports the seed rather than the code.
    //
    // THREE assets, and each one is load-bearing:
    //   member 0  — the collection cover
    //   member 1  — the featured cover, a DIFFERENT picture, because one
    //               picture cannot tell "the featured slot took my
    //               choice" from "the featured slot inherited the
    //               collection cover"
    //   outsider  — never added to the collection, so the #1074 test can
    //               prove a cover need not be a member. Picking an
    //               arbitrary search result for that would silently
    //               choose a MEMBER on a stack with few assets, and the
    //               "it did not join the collection" assertion would
    //               fail for a reason that has nothing to do with the
    //               feature.
    const memberA = await provisionAsset(request, 'member-a', 400, 200);
    const memberB = await provisionAsset(request, 'member-b', 440, 220);
    outsiderId = await provisionAsset(request, 'outsider', 360, 240);
    portraitId = await provisionAsset(request, 'portrait', 200, 420);
    memberIds = [memberA, memberB];

    // ANONYMOUSLY PICTURABLE, VERIFIED — not assumed from the fact that
    // we asked for `status: 'active'`.
    //
    // Two of the three gates are decided at create time above; the third
    // (`processing_status = 'ready'`, and a servable `col`) is decided by
    // the raster worker a moment later. So this polls the one surface
    // that answers the whole question at once, from a context with no
    // session: the `col` variant a visitor's tile would request.
    const anon = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    try {
      for (const id of [...memberIds, outsiderId, portraitId]) {
        await expect
          .poll(async () => (await anon.request.get(`/api/v1/assets/${id}/variants/col`)).status(), {
            timeout: 120000,
            message:
              `fixture asset ${id} never became anonymously picturable. THREE CANDIDATES, in ` +
              `the order they have actually bitten: (1) INSTANCE CONFIG — anonymous browsing ` +
              `off, which 401s every anonymous read before asset state is even consulted; this ` +
              `spec enables it in beforeAll, so a 401 here means that write did not take. ` +
              `(2) THE WORKER — the raster job failed on the generated PNG, leaving ` +
              `processing_status at 'failed' rather than 'ready' (check the app log for ` +
              `preview.raster). (3) THE RULE — the asset was created status=active with the ` +
              `default public sensitivity, so a 404 with a healthy worker means the tier rule ` +
              `is not holding. NOT a reason to skip: every visitor-facing assertion below ` +
              `depends on this.`,
          })
          .toBe(200);
      }
    } finally {
      await anon.close();
    }

    // The anti-vacuity backstop stays, now guarding provisioning rather
    // than seed shape. If anything above silently produced nothing, this
    // fails by name instead of letting the suite run on an empty fixture.
    expect(
      memberIds.length,
      'fixture provisioning did not produce two members — the cover slots cannot be told apart',
    ).toBe(2);
    expect(outsiderId, 'fixture provisioning did not produce a non-member asset').toBeTruthy();

    const created = await request.post('/api/v1/collections', {
      data: { name: COLLECTION_NAME, description: 'fixture for #1207' },
    });
    expect(created.status(), 'fixture collection must be created').toBe(201);
    collectionId = ((await created.json()) as { id: string }).id;

    for (const assetId of [...memberIds, portraitId]) {
      const pinned = await request.post(`/api/v1/collections/${collectionId}/resources`, {
        data: { asset_id: assetId, pinned: true },
      });
      expect(pinned.ok(), `pinning ${assetId} must succeed`).toBeTruthy();
    }
  });

  test.afterAll(async ({ request }) => {
    // The switch goes back to whatever this install had, whatever else
    // happened. Restored even on failure, and only when we know what it
    // was — an undefined prior means beforeAll never got far enough to
    // read it, and guessing would be how a private install gets left
    // public by a test run.
    if (priorPublicMode !== undefined) {
      await request
        .patch('/api/v1/admin/system/public-mode', { data: { enabled: priorPublicMode } })
        .catch(() => undefined);
    }
    // Collection first, then the pictures it pointed at — the reverse
    // order would leave the collection briefly pointing at soft-deleted
    // assets. Both are soft-deletes, so neither is destructive, and
    // every id here was created by THIS run (they carry its stamp), so
    // a concurrent run's fixtures are untouchable from here.
    if (collectionId) {
      await request.delete(`/api/v1/collections/${collectionId}`).catch(() => undefined);
    }
    for (const id of provisioned) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
  });

  async function openCoverEditor(page: Page) {
    await page.goto(`/collections/${collectionId}`);
    await page.getByTestId('collection-detail-more-button').first().click();
    await page.getByTestId('collection-detail-edit-menuitem').first().click();

    // WAIT FOR THE FORM TO HAVE SEEDED ITSELF before touching anything.
    // The modal reads the collection in an effect that runs on open; a
    // click that lands first is overwritten by that seed, and the save
    // then stores what the collection already had. Nobody clicks that
    // fast — Playwright does. The summary chip is the observable that
    // says the seed has run (#1195 learned this the expensive way with
    // the visibility radios).
    await expect(page.getByTestId('collection-cover-section')).toBeVisible();
    await page.getByTestId('collection-cover-edit-button').click();
    const editor = page.getByTestId('collection-cover-editor');
    await expect(editor).toBeVisible();
    return editor;
  }

  /** The editor's live readout of the stored pair. Empty string is
   *  null — "never positioned" — which is a different answer from
   *  "positioned at 0.5" and the reason the attribute is not defaulted. */
  async function readFocal(
    page: Page,
    prefix: 'cover-editor' | 'collection-crop' = 'cover-editor',
  ): Promise<{ x: string; y: string }> {
    const el = page.getByTestId(`${prefix}-position`);
    return {
      x: (await el.getAttribute('data-focal-x')) ?? '',
      y: (await el.getAttribute('data-focal-y')) ?? '',
    };
  }

  /** The editor's live readout of the stored ZOOM (#1212). Empty string
   *  is null — "never tightened" — which renders identically to a
   *  stored 1 and is a different value, which is the whole reason the
   *  attribute is not defaulted to '1'. */
  async function readZoom(
    page: Page,
    prefix: 'cover-editor' | 'collection-crop' = 'cover-editor',
  ): Promise<string> {
    return (await page.getByTestId(`${prefix}-position`).getAttribute('data-zoom')) ?? '';
  }

  /** Click Save and WAIT FOR THE WRITE, not for the dialog.
   *
   *  Reading the collection back the instant after the click is a race
   *  the test loses often enough to be a flake and rarely enough to be
   *  blamed on the feature — and when the PATCH is rejected the read
   *  comes back with the OLD values, which reads as "the save silently
   *  did nothing" rather than as the 400 it was. Awaiting the response
   *  and asserting its status turns that into the actual message. */
  async function saveAndAwaitPatch(page: Page) {
    const [response] = await Promise.all([
      page.waitForResponse(
        (r) => r.url().includes(`/collections/${collectionId}`) && r.request().method() === 'PATCH',
      ),
      page.getByRole('button', { name: /^save$/i }).click(),
    ]);
    expect(
      response.status(),
      `the collection PATCH was rejected: ${await response.text().catch(() => '')}`,
    ).toBe(200);
  }

  async function fetchCollection(page: Page): Promise<CollectionPayload> {
    const r = await page.request.get(`/api/v1/collections/${collectionId}`);
    expect(r.status(), 'the collection must be readable back').toBe(200);
    return (await r.json()) as CollectionPayload;
  }

  test('picks two different covers, drags the crop, and all three values persist', async ({
    page,
  }) => {
    const editor = await openCoverEditor(page);

    // Slot 1: the collection cover. Slot 2 starts inheriting it, which
    // is the state the summary and the stage should both be showing.
    await editor
      .getByTestId('collection-cover-choice')
      .filter({ has: page.locator(`[src*="${memberIds[0]}"]`) })
      .first()
      .click();
    await expect(page.getByTestId('collection-crop-card-preview')).toHaveAttribute(
      'src',
      new RegExp(memberIds[0]),
    );

    // Slot 2: a DIFFERENT picture. This is the finding — "the featured
    // collection rail cover and regular collection cover should be
    // different. Not force the same image for both."
    await editor
      .getByTestId('featured-cover-choice')
      .filter({ has: page.locator(`[src*="${memberIds[1]}"]`) })
      .first()
      .click();
    const stage = page.getByTestId('cover-editor-stage-image');
    await expect(stage).toHaveAttribute('src', new RegExp(memberIds[1]));

    // A brand-new collection has no focal point at all — null, not 0.5.
    expect(await readFocal(page), 'a fresh collection must start unpositioned').toEqual({
      x: '',
      y: '',
    });

    const marquee = page.getByTestId('cover-editor-marquee');
    await expect(marquee).toBeVisible();

    const canMove = await marquee.isEnabled();
    if (canMove) {
      // A REAL pointer drag: trusted events, captured pointer, and a
      // move BEFORE the button goes down so the browser has a position
      // to start from. Two intermediate moves rather than one jump —
      // a single move can be coalesced, and coalescing is exactly what
      // hid the bug the last drag control shipped with.
      const box = (await marquee.boundingBox())!;
      const stageBox = (await stage.boundingBox())!;
      const startX = box.x + box.width / 2;
      const startY = box.y + box.height / 2;
      await page.mouse.move(startX, startY);
      await page.mouse.down();
      await page.mouse.move(startX - stageBox.width * 0.15, startY - stageBox.height * 0.15, {
        steps: 8,
      });
      await page.mouse.move(startX - stageBox.width * 0.3, startY - stageBox.height * 0.3, {
        steps: 8,
      });
      await page.mouse.up();

      const dragged = await readFocal(page);
      expect(
        dragged.x === '' && dragged.y === '',
        'the drag produced no focal point — a marquee that does not move is the whole bug',
      ).toBe(false);
      // Dragged up and to the left, so whichever axis has travel must
      // have moved BELOW centre. Asserting the direction rather than a
      // number keeps this true for any seeded picture's proportions.
      const movedX = dragged.x !== '' && Number(dragged.x) < 0.5;
      const movedY = dragged.y !== '' && Number(dragged.y) < 0.5;
      expect(movedX || movedY, `focal did not move up-and-left: ${JSON.stringify(dragged)}`).toBe(
        true,
      );
    }

    // ── The alignment rig (#1195, re-verified with a NON-CENTRE focal)
    //
    // The marquee is not an illustration of the crop, it is the same
    // rectangle. So the region the marquee marks and the region the
    // card preview actually shows must agree — computed from the card's
    // OWN geometry (its box, the picture's natural size, and the
    // resolved object-position), not from the same code the marquee
    // used. Sub-pixel, because "roughly right" is how a crop preview
    // lies about a face at the edge of frame.
    const alignment = await page.evaluate(() => {
      const card = document.querySelector<HTMLImageElement>(
        '[data-testid="cover-editor-card-preview"]',
      )!;
      const stageImg = document.querySelector<HTMLImageElement>(
        '[data-testid="cover-editor-stage-image"]',
      )!;
      const marqueeEl = document.querySelector<HTMLElement>('[data-testid="cover-editor-marquee"]')!;

      const box = card.getBoundingClientRect();
      const nw = card.naturalWidth;
      const nh = card.naturalHeight;
      // What `object-fit: cover` does, worked out independently.
      const scale = Math.max(box.width / nw, box.height / nh);
      const rw = nw * scale;
      const rh = nh * scale;
      const pos = getComputedStyle(card).objectPosition.split(' ');
      const px = parseFloat(pos[0]) / 100;
      const py = parseFloat(pos[1]) / 100;
      const offsetX = (box.width - rw) * px;
      const offsetY = (box.height - rh) * py;

      const stageRect = stageImg.getBoundingClientRect();
      const marqueeRect = marqueeEl.getBoundingClientRect();

      return {
        // The window the CARD shows, as fractions of the picture.
        card: {
          left: -offsetX / rw,
          top: -offsetY / rh,
          width: box.width / rw,
          height: box.height / rh,
        },
        // The window the MARQUEE marks, as fractions of the picture.
        marquee: {
          left: (marqueeRect.left - stageRect.left) / stageRect.width,
          top: (marqueeRect.top - stageRect.top) / stageRect.height,
          width: marqueeRect.width / stageRect.width,
          height: marqueeRect.height / stageRect.height,
        },
        // The scale the comparison is stated in: a disagreement only
        // matters at the size the curator is looking at it.
        stage: { width: stageRect.width, height: stageRect.height },
        // The FRAME's aspect — the element that carries
        // `aspect-ratio: 890 / 500`, measured on the frame rather than
        // on the picture inside it. The frame is border-box and has a
        // 1px border, exactly as FeaturedRail's card does, so the
        // picture's own box is a couple of pixels narrower and shorter
        // and is NOT 890:500. That is not a defect to correct here: the
        // preview reproduces the strip's card structure, border
        // included, which is the whole reason it can be trusted.
        cardBox: {
          width: card.parentElement!.getBoundingClientRect().width,
          height: card.parentElement!.getBoundingClientRect().height,
        },
      };
    });

    // ⚠️ STATED AS A FRACTION OF THE PICTURE, and the unit is the whole
    // point of this block.
    //
    // The claim being tested is "the marquee marks the region the card
    // shows" — a claim about WHICH PART OF THE PICTURE, so its natural
    // unit is a fraction of the picture. An absolute pixel bar was the
    // first attempt and it is not scale-invariant: the stage is ~512px
    // wide here and would be ~900px on a larger display, so the same
    // fractional disagreement reads as 1.6px or 2.8px depending on
    // nothing that matters.
    //
    // AND THE DISAGREEMENT IS NOT ZERO, for a reason worth writing down
    // rather than tuning around. The marquee is computed from the
    // 890:500 CONSTANT. The card preview mirrors FeaturedRail's own
    // structure — `aspect-ratio` on a border-box frame with a 1px
    // border, the picture inside it — so the picture's box is
    // (W-2)/(H-2), which is not 890:500. At this frame size that is
    // ~0.3% of the picture. The real rail card has the identical
    // structure and therefore the identical offset, so "fix" it by
    // moving the aspect onto the image and the preview stops matching
    // the thing it is previewing. A sub-percent bar is the honest one:
    // a genuine misalignment is off by the size of the crop — tens of
    // percent — not by a border.
    const deltas = {
      left: Math.abs(alignment.marquee.left - alignment.card.left),
      top: Math.abs(alignment.marquee.top - alignment.card.top),
      width: Math.abs(alignment.marquee.width - alignment.card.width),
      height: Math.abs(alignment.marquee.height - alignment.card.height),
    };
    // eslint-disable-next-line no-console
    console.log(
      '#1207 marquee-vs-card alignment — fraction of the picture:',
      JSON.stringify(deltas),
      '| px on this stage:',
      JSON.stringify({
        left: deltas.left * alignment.stage.width,
        top: deltas.top * alignment.stage.height,
        width: deltas.width * alignment.stage.width,
        height: deltas.height * alignment.stage.height,
      }),
    );
    for (const axis of ['left', 'top', 'width', 'height'] as const) {
      expect(
        deltas[axis],
        `the marquee's ${axis} disagrees with the card's actual crop by more than the 1px ` +
          `border can account for — marquee ${JSON.stringify(alignment.marquee)} vs card ` +
          JSON.stringify(alignment.card),
      ).toBeLessThan(0.005);
    }
    // And the frame really is the strip's shape, to within a pixel of
    // width — the half of the claim the tolerance above absorbs.
    expect(
      Math.abs(alignment.cardBox.width - (alignment.cardBox.height * 890) / 500),
      'the preview frame is not 890:500; the marquee is marking the wrong shape',
    ).toBeLessThan(1);

    // ── The REGULAR slot's square marquee (#1207 addition) ──────────
    //
    // A SECOND pair on a SECOND destination shape, and the test asserts
    // they are independent. A single stored pair shared by both slots
    // would pass every "did it persist" check and still be wrong: the
    // point that centres a subject in a 890:500 band is not the point
    // that centres it in a square, so the two have to be able to hold
    // different values at the same time.
    const collMarquee = page.getByTestId('collection-crop-marquee');
    await expect(
      collMarquee,
      'the collection-cover slot has no crop marquee — the 4:3 lock did not render',
    ).toBeVisible();
    expect(await readFocal(page, 'collection-crop'), 'the collection crop must start unpositioned')
      .toEqual({ x: '', y: '' });

    const collCanMove = await collMarquee.isEnabled();
    if (collCanMove) {
      // SCROLL IT INTO VIEW FIRST. The dialog body scrolls and this slot
      // sits below the featured one, so its bounding box is real but
      // off-screen — and `page.mouse` works in VIEWPORT coordinates, so
      // the drag would be dispatched at a point the marquee is not
      // currently occupying. It moved nothing and read as "the square
      // marquee is not draggable".
      await collMarquee.scrollIntoViewIfNeeded();
      const sBox = (await collMarquee.boundingBox())!;
      const sStage = (await page.getByTestId('collection-crop-stage-image').boundingBox())!;
      await page.mouse.move(sBox.x + sBox.width / 2, sBox.y + sBox.height / 2);
      await page.mouse.down();
      await page.mouse.move(
        sBox.x + sBox.width / 2 + sStage.width * 0.3,
        sBox.y + sBox.height / 2 + sStage.height * 0.3,
        { steps: 8 },
      );
      await page.mouse.up();
      const sq = await readFocal(page, 'collection-crop');
      expect(sq.x === '' && sq.y === '', 'the collection-cover marquee did not move').toBe(false);
    }

    // The collection preview is 4:3 — CollectionCard's actual tile.
    //
    // ⚠️ NOT 1:1, and the distinction is the point. `col` is a square
    // (fit: cover at 320px), which makes "lock it to a square" a very
    // reasonable read — but `col` is the SOURCE and the tile that paints
    // it is `aspect-[4/3]`. A marquee locked to the source rather than
    // the destination shows the curator a region the card never
    // displays. If this ever reads 890:500 the two slots have been wired
    // to one stage.
    const collRatio = await page.evaluate(() => {
      const img = document.querySelector<HTMLImageElement>(
        '[data-testid="collection-crop-card-preview"]',
      )!;
      const box = img.parentElement!.getBoundingClientRect();
      return box.width / box.height;
    });
    expect(collRatio, 'the collection-cover preview is not 4:3').toBeCloseTo(4 / 3, 2);

    await page.getByTestId('cover-editor-done').click();
    await expect(page.getByTestId('collection-cover-editor')).toBeHidden();
    await saveAndAwaitPatch(page);

    // THE ASSERTION. Read back from the server, not from the form.
    const saved = await fetchCollection(page);
    expect(saved.cover_asset_id, 'the collection cover did not persist').toBe(memberIds[0]);
    expect(
      saved.featured_cover_asset_id,
      'the featured cover did not persist as a SEPARATE choice',
    ).toBe(memberIds[1]);
    if (canMove) {
      expect(saved.featured_cover_focal_x, 'focal x did not persist').not.toBeNull();
      expect(saved.featured_cover_focal_y, 'focal y did not persist').not.toBeNull();
      expect(saved.featured_cover_focal_x).toBeGreaterThanOrEqual(0);
      expect(saved.featured_cover_focal_x).toBeLessThanOrEqual(1);
    }
    if (collCanMove) {
      expect(saved.cover_focal_x, 'the collection cover focal x did not persist').not.toBeNull();
      expect(saved.cover_focal_y, 'the collection cover focal y did not persist').not.toBeNull();
      // INDEPENDENT, not a mirror. The two marquees were dragged in
      // opposite directions on purpose, so a build that stored one pair
      // for both slots fails here rather than passing every other check.
      if (canMove) {
        expect(
          saved.cover_focal_x === saved.featured_cover_focal_x &&
            saved.cover_focal_y === saved.featured_cover_focal_y,
          'both slots stored the same pair — the square crop and the 890:500 crop are ' +
            'sharing one focal point, which cannot be right for both shapes',
        ).toBe(false);
      }
    }
  });

  test('the keyboard moves the same marquee', async ({ page }) => {
    const editor = await openCoverEditor(page);
    const marquee = editor.getByTestId('cover-editor-marquee');
    await expect(marquee).toBeVisible();
    test.skip(!(await marquee.isEnabled()), 'this picture is already card-shaped: no travel');

    const before = await readFocal(page);
    await marquee.focus();
    for (let i = 0; i < 5; i++) await page.keyboard.press('ArrowRight');
    for (let i = 0; i < 5; i++) await page.keyboard.press('ArrowDown');
    const after = await readFocal(page);
    expect(
      after,
      'arrow keys did not move the crop — the pointer is not the only way in',
    ).not.toEqual(before);
  });

  // ⚠️ THE #1081 TRAP, checked on all three columns. A nullable field
  // whose Go type is a pointer with `omitempty` cannot express "clear"
  // by sending null: absent and explicit-null collapse before the query
  // sees them, and the COALESCE reads the collapse as "keep". #1073
  // shipped that defect for three releases on expires_at. So this
  // asserts NULL AFTER CLEAR rather than "the value changed".
  test('clearing actually clears — all three columns come back null', async ({ page }) => {
    const before = await fetchCollection(page);
    expect(
      before.featured_cover_asset_id ?? null,
      'the previous test must have left a value to clear',
    ).not.toBeNull();

    const editor = await openCoverEditor(page);

    await editor.getByTestId('cover-editor-clear-featured').click();
    const reset = editor.getByTestId('cover-editor-reset-focal');
    if (await reset.isEnabled()) await reset.click();
    expect(await readFocal(page), 'reset must clear to null, not re-set to 0.5').toEqual({
      x: '',
      y: '',
    });

    // The collection crop has its own Reset, and its own clear flag
    // behind it — same tri-state, second pair.
    const collReset = editor.getByTestId('collection-crop-reset-focal');
    if (await collReset.isEnabled()) await collReset.click();
    expect(
      await readFocal(page, 'collection-crop'),
      'the collection-cover reset must clear to null too',
    ).toEqual({ x: '', y: '' });

    // And the collection cover back to the mosaic ("Use mosaic" is the
    // first control in that grid), so the clear is exercised on the
    // column #1027 introduced too.
    await editor.getByTestId('collection-cover-choices').locator('button').first().click();

    await page.getByTestId('cover-editor-done').click();
    await saveAndAwaitPatch(page);

    // `?? null` because a null column is OMITTED from the payload
    // rather than serialised as JSON null — the convention
    // `cover_asset_id` has followed since #1027 (a nullable property
    // with `omitempty` on the Go side). Absent and null are the same
    // answer here; what they are NOT is a value.
    //
    // ⚠️ It has to be `?? null` and not `toBeFalsy()`. A focal
    // coordinate of 0 is a perfectly good positioning — the left or top
    // edge, which is exactly where a drag to the corner lands — and
    // `toBeFalsy` would call that a successful clear. That is the shape
    // of assertion #1081 warns about: one that passes on the bug.
    const after = await fetchCollection(page);
    expect(after.featured_cover_asset_id ?? null, 'clear_featured_cover did not clear').toBeNull();
    expect(
      after.featured_cover_focal_x ?? null,
      'clear_featured_cover_focal did not clear x',
    ).toBeNull();
    expect(
      after.featured_cover_focal_y ?? null,
      'clear_featured_cover_focal did not clear y',
    ).toBeNull();
    expect(after.cover_asset_id ?? null, 'clear_cover did not clear').toBeNull();
    expect(after.cover_focal_x ?? null, 'clear_cover_focal did not clear x').toBeNull();
    expect(after.cover_focal_y ?? null, 'clear_cover_focal did not clear y').toBeNull();
  });


  // Modal grew a shared open-order stack for this dialog, and that is a
  // change to a component five other surfaces use. The property it buys
  // is the one below: Escape steps back exactly ONE level. Before it,
  // the document-level handler had every open instance answering every
  // press, so one Escape over the editor would also have dismissed the
  // form behind it — taking the curator's unsaved edits with it.
  test('Escape closes the cover editor and leaves the form open', async ({ page }) => {
    await openCoverEditor(page);
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('collection-cover-editor')).toBeHidden();
    await expect(
      page.getByTestId('collection-cover-section'),
      'Escape dismissed the edit form as well as the dialog on top of it',
    ).toBeVisible();
    // And a second press closes the form, so nothing has been trapped.
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('collection-cover-section')).toBeHidden();
  });

  // ── #1074, folded into #1207: a cover that is NOT a member ─────────
  //
  // The write gate has always been CallerMayPictureAsset, never a
  // membership check — verified in the handler, not inferred from the
  // issue. So the capability existed and only the picker withheld it.
  // These two tests are the proof that a curator can now reach it.

  test('a picture from MY FILES can be the cover without joining the collection', async ({
    page,
    request,
  }) => {
    const editor = await openCoverEditor(page);
    await editor.getByTestId('collection-source-mine').click();

    // SEARCH FOR THIS RUN'S OWN OUTSIDER, and select it by id.
    //
    // Taking `results.first()` was wrong in a way that only shows on a
    // sparse library: the unfiltered listing is the curator's whole
    // collection of assets, MEMBERS INCLUDED, so on a stack whose only
    // assets are this spec's three the arbitrary first result is a
    // member — and the "it did not join the collection" assertion below
    // then fails for a reason that has nothing to do with the feature.
    //
    // Searching also exercises the arm this test is named after, rather
    // than only the listing behind it.
    await editor.getByTestId('collection-search-input').fill(TOKEN);
    await editor.getByTestId('collection-search-input').press('Enter');

    const outsider = editor.locator(
      `[data-testid="collection-mine-choice"][data-asset-id="${outsiderId}"]`,
    );
    await expect(
      outsider,
      `the my-files search did not return this run's non-member fixture (${outsiderId}). ` +
        'Either the search arm is broken or the fixture never became searchable — both are ' +
        'failures, and neither is a reason to skip.',
    ).toBeVisible({ timeout: 20000 });
    await outsider.click();

    await page.getByTestId('cover-editor-done').click();
    await saveAndAwaitPatch(page);

    const saved = await fetchCollection(page);
    expect(saved.cover_asset_id, 'a non-member cover did not persist').toBe(outsiderId);

    // AND IT IS NOT A MEMBER. The whole point of the free pointer is
    // that choosing a picture does not add it to the collection — a
    // cover that quietly joined would change what the collection IS.
    const members = await request.get(`/api/v1/collections/${collectionId}/resources?limit=200`);
    const { items } = (await members.json()) as { items: Array<{ asset_id: string }> };
    expect(
      items.some((m) => m.asset_id === outsiderId),
      'choosing a cover added it to the collection — the pointer is supposed to be free',
    ).toBe(false);
  });

  // A FRESH UPLOAD, on a PUBLIC collection, read back by an ANONYMOUS
  // visitor. That last hop is the whole acceptance: an uploaded cover
  // the curator can see and a visitor cannot is the "reads as broken"
  // failure the tier rule exists to prevent — and the axis that decides
  // it is `status`, because the anonymous asset predicate requires
  // `status = 'active'` while assets default to `draft` from the upload
  // queue. (`sensitivity` already defaults to the widest tier and has
  // no write API at all.)
  test('a freshly uploaded cover reaches an anonymous visitor on a public collection', async ({
    page,
    browser,
    request,
  }) => {
    await request.patch(`/api/v1/collections/${collectionId}`, { data: { visibility: 'public' } });

    const editor = await openCoverEditor(page);
    await editor.getByTestId('featured-source-upload').click();
    await expect(editor.getByTestId('featured-upload-pane')).toBeVisible();

    // A real 2:1 PNG, so the marquee has travel on the x axis and the
    // picture is unmistakably the uploaded one rather than a member.
    await editor.getByTestId('featured-upload-input').setInputFiles({
      name: `cover-1207-${STAMP}.png`,
      mimeType: 'image/png',
      buffer: makePng(`${TOKEN}-upload`, 1200, 500),
    });
    await expect(editor.getByTestId('featured-uploading')).toBeHidden({ timeout: 30000 });

    await page.getByTestId('cover-editor-done').click();
    await saveAndAwaitPatch(page);

    const saved = await fetchCollection(page);
    const uploaded = saved.featured_cover_asset_id;
    expect(uploaded, 'the uploaded picture did not become the featured cover').toBeTruthy();
    // Registered so afterAll cleans it up like every other fixture.
    if (uploaded) provisioned.push(uploaded);
    expect(uploaded, 'the uploaded picture is one of the members — it should be new').not.toBe(
      memberIds[0],
    );
    expect(uploaded).not.toBe(memberIds[1]);

    // THE ANONYMOUS READ. Not "the admin can see it" — the admin can
    // see everything, which is exactly why re-reading as the writer
    // proves nothing (#946).
    //
    // POLLED, because the last gate is asynchronous and that is a
    // property of the product rather than of the test. The anonymous
    // asset predicate wants `status = 'active'` AND
    // `sensitivity = 'public'` AND `processing_status = 'ready'`; the
    // first two are decided at create time by the rule this test is
    // about, and the third is decided by the raster worker a second or
    // so later. CallerMayPictureAsset documents the same asymmetry from
    // the other side — it deliberately does NOT require a rendition,
    // because refusing a just-uploaded cover would be an error the
    // curator cannot act on. So the cover is chosen immediately and
    // becomes visible shortly after, and a snapshot assertion here
    // would report that ordinary sequence as the tier bug.
    const anon = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    try {
      await expect
        .poll(
          async () => (await anon.request.get(`/api/v1/assets/${uploaded}`)).status(),
          {
            timeout: 60000,
            message:
              'a visitor never got to read the uploaded cover. If it never reaches 200 the ' +
              'upload went up as a draft on a PUBLIC collection, which is the silent-fallback ' +
              'failure the status rule exists to prevent.',
          },
        )
        .toBe(200);

      // AND ON THE RAIL, which is where the curator will look for it.
      await request.patch(`/api/v1/collections/${collectionId}`, {
        data: { visibility: 'public' },
      });
      const placed = await request.post('/api/v1/admin/featured', {
        data: { subject_kind: 'collection', subject_id: collectionId, scope: 'public' },
      });
      expect(placed.ok(), 'featuring the fixture collection must succeed').toBeTruthy();
      const placementId = ((await placed.json()) as { id: string }).id;
      try {
        await expect
          .poll(
            async () => {
              const rail = await anon.request.get('/api/v1/featured?limit=50');
              const { items } = (await rail.json()) as {
                items: Array<{ subject_id: string; cover_asset_id?: string | null }>;
              };
              return items.find((i) => i.subject_id === collectionId)?.cover_asset_id ?? null;
            },
            {
              timeout: 60000,
              message:
                'the anonymous rail never showed the uploaded cover — it is falling back to ' +
                'something a visitor can see instead, which is correct behaviour for a cover ' +
                'they cannot, and therefore the failure this test is for',
            },
          )
          .toBe(uploaded);
      } finally {
        await request.delete(`/api/v1/admin/featured/${placementId}`).catch(() => undefined);
      }
    } finally {
      await anon.close();
    }
  });


  // ── The hub tile CONSUMES the focal point (#1207 item 3) ───────────
  //
  // Everything above proves the value is stored. This proves it is
  // SPENT — that a collection card actually shows a different part of
  // the picture because the curator moved a box.
  //
  // ⚠️ THE SOURCE HAS TO CHANGE WITH IT, and that is the assertion this
  // test exists for. `col` is `fit: cover` at 320px — a 320x320
  // CENTRE-CROP — so applying object-position to it takes a second crop
  // of the server's crop and lands somewhere nobody chose. A card that
  // merely set object-position on `col` would look like it worked and
  // be wrong, so the src is checked as well as the position, and the
  // rendered pixels are compared to catch "the CSS is right and nothing
  // moved".
  test('a hub tile visibly shifts when the cover has an off-centre focal point', async ({
    page,
    request,
  }) => {
    // A clean starting point: the chosen cover, no focal.
    const reset = await request.patch(`/api/v1/collections/${collectionId}`, {
      data: { cover_asset_id: memberIds[0], clear_cover_focal: true },
    });
    expect(reset.status(), 'fixture PATCH must be accepted').toBe(200);

    const hubTile = () =>
      page
        .locator('a[href$="/collections/' + collectionId + '"]')
        .first()
        .locator('[data-testid="collection-card-cover"]');

    async function loadHub() {
      await page.setViewportSize({ width: 1440, height: 900 });
      await page.goto(`/collections?q=${encodeURIComponent(COLLECTION_NAME)}`);
      const tile = hubTile();
      await expect(tile, 'the fixture collection has no card on the hub').toBeVisible({
        timeout: 20000,
      });
      await tile.scrollIntoViewIfNeeded();
      // Wait for the picture itself, not just the element — comparing
      // screenshots of a tile that has not decoded yet compares two
      // blank boxes and passes for the wrong reason.
      await expect
        .poll(async () => tile.evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0), {
          timeout: 20000,
        })
        .toBe(true);
      return tile;
    }

    const before = await loadHub();
    expect(
      await before.getAttribute('data-focal'),
      'the card claims a focal crop before one was set',
    ).toBe('off');
    expect(await before.getAttribute('src'), 'with no focal the card should paint col').toContain(
      '/variants/col',
    );
    expect(
      await before.evaluate((el) => getComputedStyle(el).objectPosition),
      'with no focal the card must centre, exactly as it always did',
    ).toBe('50% 50%');
    const beforeShot = await before.screenshot();

    // Hard to one corner, so the shift is unmistakable rather than
    // subtle enough to be argued with.
    const patched = await request.patch(`/api/v1/collections/${collectionId}`, {
      data: { cover_focal_x: 0, cover_focal_y: 0 },
    });
    expect(patched.status(), 'the focal PATCH must be accepted').toBe(200);

    // The payload boolean the card branches on — the read path this
    // whole item depends on, asserted directly rather than inferred
    // from the card having worked.
    const read = await request.get(`/api/v1/collections/${collectionId}`);
    const body = (await read.json()) as {
      covers?: Array<{ asset_id: string; preview_available?: boolean }>;
    };
    expect(body.covers?.length, 'the chosen cover should compose to exactly one entry').toBe(1);
    expect(
      body.covers?.[0].preview_available,
      'CollectionCover carries no preview_available — the card cannot tell whether a contain ' +
        'rung exists and would have to guess, which is how it ends up cropping a crop',
    ).toBe(true);

    const after = await loadHub();
    expect(
      await after.getAttribute('data-focal'),
      'the card did not switch to focal mode',
    ).toBe('on');
    expect(
      await after.getAttribute('src'),
      'the card is still painting the pre-cropped col — object-position on that crops a crop',
    ).toContain('/variants/preview');
    expect(
      await after.evaluate((el) => getComputedStyle(el).objectPosition),
      'the card did not take the focal point',
    ).toBe('0% 0%');

    // AND THE PIXELS MOVED. The three assertions above are about the
    // DOM; this one is about what a person sees, and it is the reason
    // the source-swap defect could not hide here.
    const afterShot = await after.screenshot();
    expect(
      Buffer.compare(beforeShot, afterShot) === 0,
      'the tile renders identically before and after an extreme focal point — the value is ' +
        'stored and read and changes nothing on screen',
    ).toBe(false);
  });

  // The rail is the surface all of this exists to feed (#1200). It read
  // NEITHER chosen column before #1207, so a curator's choice reached
  // every collection surface EXCEPT the one they chose it for.
  test('the featured strip renders the chosen featured cover, not a derived one', async ({
    page,
    request,
  }) => {
    // Re-set the two covers through the API here rather than the modal:
    // the modal path is proven above, and what this test is about is the
    // RAIL's read.
    const patched = await request.patch(`/api/v1/collections/${collectionId}`, {
      data: {
        cover_asset_id: memberIds[0],
        featured_cover_asset_id: memberIds[1],
        featured_cover_focal_x: 0.2,
        featured_cover_focal_y: 0.8,
      },
    });
    expect(patched.status(), 'the PATCH must accept all three fields').toBe(200);
    await request.patch(`/api/v1/collections/${collectionId}`, { data: { visibility: 'public' } });

    const placed = await request.post('/api/v1/admin/featured', {
      data: { subject_kind: 'collection', subject_id: collectionId, scope: 'public' },
    });
    expect(placed.ok(), 'featuring the fixture collection must succeed').toBeTruthy();
    const placementId = ((await placed.json()) as { id: string }).id;

    try {
      const rail = await request.get('/api/v1/featured?limit=50');
      expect(rail.status()).toBe(200);
      const { items } = (await rail.json()) as {
        items: Array<{
          subject_id: string;
          cover_asset_id?: string | null;
          cover_focal_x?: number | null;
          cover_focal_y?: number | null;
        }>;
      };
      const tile = items.find((i) => i.subject_id === collectionId);
      expect(tile, 'the featured collection is missing from the rail').toBeTruthy();
      expect(
        tile!.cover_asset_id,
        'the rail is still deriving its cover instead of reading the curator’s choice (#1200)',
      ).toBe(memberIds[1]);
      expect(tile!.cover_focal_x, 'the rail dropped the focal point').toBeCloseTo(0.2, 6);
      expect(tile!.cover_focal_y).toBeCloseTo(0.8, 6);

      // And the STRIP renders it there — object-position on the real
      // card, from the real payload, so the last hop is covered too.
      await page.goto('/');
      const tileEl = page
        .getByTestId('featured-rail-item')
        .filter({ hasText: COLLECTION_NAME })
        .first();
      await expect(tileEl).toBeVisible();
      const img = tileEl.locator('img').first();
      await expect(img).toHaveAttribute('style', /object-position:\s*20% 80%/);
    } finally {
      await request.delete(`/api/v1/admin/featured/${placementId}`).catch(() => undefined);
    }
  });

  // ── #1212: the crop can be TIGHTENED ────────────────────────────────
  //
  // The defect #1207 left behind, in one sentence: the marquee is the
  // LARGEST rectangle of the destination's aspect that fits the
  // picture, so one axis is always the whole picture and only the other
  // can travel. The owner hit it with a portrait cover in the rail's
  // wide window; these fixtures are 2:1, so it is the VERTICAL axis
  // that is pinned instead. Same geometry, same bug, and using the
  // fixtures that already exist keeps this spec's provisioning honest.

  test('at the fit one axis cannot move, and zooming is what frees it', async ({ page }) => {
    const editor = await openCoverEditor(page);
    await editor
      .getByTestId('featured-cover-choice')
      .filter({ has: page.locator(`[src*="${memberIds[0]}"]`) })
      .first()
      .click();
    const stage = page.getByTestId('cover-editor-stage-image');
    await expect(stage).toHaveAttribute('src', new RegExp(memberIds[0]));

    const marquee = page.getByTestId('cover-editor-marquee');
    await expect(marquee).toBeVisible();
    expect(await readZoom(page), 'a fresh collection must start untightened').toBe('');

    // THE BUG, REPRODUCED FIRST. A 2:1 picture in the 890:500 window
    // keeps its full height, so a drag straight down has nowhere to go
    // — and the control says so by writing the neutral 0.5 rather than
    // accumulating pointer noise on an axis that cannot render it.
    const fitBox = (await marquee.boundingBox())!;
    const stageBox = (await stage.boundingBox())!;
    expect(
      Math.abs(fitBox.height - stageBox.height) < 2,
      'the fixture is not pinned on the vertical axis, so this test is not testing anything',
    ).toBe(true);

    await page.mouse.move(fitBox.x + fitBox.width / 2, fitBox.y + fitBox.height / 2);
    await page.mouse.down();
    await page.mouse.move(fitBox.x + fitBox.width / 2, fitBox.y + fitBox.height / 2 - stageBox.height * 0.4, { steps: 8 });
    await page.mouse.up();
    expect(
      (await readFocal(page)).y,
      'the pinned axis moved at the fit — the marquee is not honest about what it can do',
    ).toBe('0.5');

    // THE FIX. The slider is clicked at its midpoint — a trusted
    // pointer event on a native range, which is the control the
    // keyboard path uses too.
    const slider = page.getByTestId('cover-editor-zoom');
    await expect(slider).toBeVisible();
    const track = (await slider.boundingBox())!;
    await page.mouse.click(track.x + track.width / 2, track.y + track.height / 2);

    const zoomed = Number(await readZoom(page));
    expect(zoomed, 'the slider wrote nothing').toBeGreaterThan(1.5);
    expect(zoomed).toBeLessThanOrEqual(4);

    // THE MARQUEE RESIZED LIVE. Not a style assertion: the rectangle
    // drawn on the picture is measurably smaller on BOTH axes, which is
    // what having two axes of travel means.
    const tightBox = (await marquee.boundingBox())!;
    expect(tightBox.width).toBeLessThan(fitBox.width - 1);
    expect(tightBox.height).toBeLessThan(fitBox.height - 1);
    expect(tightBox.height / fitBox.height).toBeCloseTo(1 / zoomed, 1);

    // AND NOW THE PINNED AXIS MOVES. This is the acceptance criterion
    // in one assertion.
    await page.mouse.move(tightBox.x + tightBox.width / 2, tightBox.y + tightBox.height / 2);
    await page.mouse.down();
    await page.mouse.move(
      tightBox.x + tightBox.width / 2,
      tightBox.y + tightBox.height / 2 - stageBox.height * 0.3,
      { steps: 8 },
    );
    await page.mouse.up();
    const freed = await readFocal(page);
    expect(
      Number(freed.y),
      'the vertical axis still will not move once the window is smaller than the picture',
    ).toBeLessThan(0.45);

    // ── THE PREVIEW STILL PREDICTS THE CARD, ZOOMED ─────────────────
    //
    // The #1207 alignment check above runs at the fit, where the card's
    // image IS its frame. Zoomed it is not: the picture is laid out at
    // z times the frame and the frame clips it, so the window the card
    // shows has to be worked back through TWO transforms. This is the
    // assertion that would catch the editor and the surfaces drifting
    // apart — the failure mode where every value round-trips and the
    // curator still frames the wrong thing.
    const aligned = await page.evaluate(() => {
      const card = document.querySelector<HTMLImageElement>(
        '[data-testid="cover-editor-card-preview"]',
      )!;
      const stageImg = document.querySelector<HTMLImageElement>(
        '[data-testid="cover-editor-stage-image"]',
      )!;
      const marqueeEl = document.querySelector<HTMLElement>('[data-testid="cover-editor-marquee"]')!;

      // The FRAME is what the viewer sees; the img is the enlarged
      // picture-holder inside it.
      const frame = card.parentElement!.getBoundingClientRect();
      const holder = card.getBoundingClientRect();
      const nw = card.naturalWidth;
      const nh = card.naturalHeight;
      // `object-fit: cover` against the HOLDER, worked out
      // independently of the helper that produced the CSS.
      const scale = Math.max(holder.width / nw, holder.height / nh);
      const rw = nw * scale;
      const rh = nh * scale;
      const pos = getComputedStyle(card).objectPosition.split(' ');
      const picLeft = holder.left + (holder.width - rw) * (parseFloat(pos[0]) / 100);
      const picTop = holder.top + (holder.height - rh) * (parseFloat(pos[1]) / 100);

      const stageRect = stageImg.getBoundingClientRect();
      const marqueeRect = marqueeEl.getBoundingClientRect();
      return {
        card: {
          left: (frame.left - picLeft) / rw,
          top: (frame.top - picTop) / rh,
          width: frame.width / rw,
          height: frame.height / rh,
        },
        marquee: {
          left: (marqueeRect.left - stageRect.left) / stageRect.width,
          top: (marqueeRect.top - stageRect.top) / stageRect.height,
          width: marqueeRect.width / stageRect.width,
          height: marqueeRect.height / stageRect.height,
        },
      };
    });
    for (const axis of ['left', 'top', 'width', 'height'] as const) {
      expect(
        Math.abs(aligned.marquee[axis] - aligned.card[axis]),
        `zoomed, the marquee's ${axis} disagrees with the card's actual crop — marquee ` +
          `${JSON.stringify(aligned.marquee)} vs card ${JSON.stringify(aligned.card)}`,
      ).toBeLessThan(0.005);
    }

    // PERSISTED, and read back from the API rather than from the form
    // that produced it (#946).
    await page.getByTestId('cover-editor-done').click();
    await saveAndAwaitPatch(page);
    const saved = await fetchCollection(page);
    expect(saved.featured_cover_zoom, 'the zoom did not survive the round trip').toBeCloseTo(
      zoomed,
      4,
    );
    expect(saved.featured_cover_focal_y).toBeCloseTo(Number(freed.y), 4);
  });

  test('+ and − zoom from the keyboard, and Reset clears the zoom to null', async ({ page }) => {
    await openCoverEditor(page);
    const marquee = page.getByTestId('cover-editor-marquee');
    await expect(marquee).toBeVisible();

    // The a11y twin of the slider. Focused with a real Tab-free focus
    // call and driven with real key events, because a control that only
    // answers a synthesised event is a control no keyboard user has.
    await marquee.focus();
    const start = Number(await readZoom(page)) || 1;
    await page.keyboard.press('+');
    await page.keyboard.press('+');
    const up = Number(await readZoom(page));
    expect(up, 'the + key did not tighten the crop').toBeGreaterThan(start);
    await page.keyboard.press('-');
    expect(Number(await readZoom(page)), 'the − key did not loosen it').toBeLessThan(up);

    // RESET IS A CLEAR OF ALL THREE VALUES, not a re-set to the neutral
    // numbers. Null and 1 render the same picture and are stored
    // differently on purpose — this is the assertion that keeps them
    // distinguishable.
    await page.getByTestId('cover-editor-reset-focal').click();
    expect(await readZoom(page), 'Reset left a zoom behind').toBe('');
    expect(await readFocal(page)).toEqual({ x: '', y: '' });

    await page.getByTestId('cover-editor-done').click();
    await saveAndAwaitPatch(page);
    const saved = await fetchCollection(page);
    expect(
      saved.featured_cover_zoom ?? null,
      'the clear was accepted but the column still holds a value — #1073 all over again',
    ).toBeNull();
  });

  test('the hub tile paints the zoom, and an unzoomed tile is unchanged', async ({
    page,
    request,
  }) => {
    // A focal point WITHOUT a zoom first: that is every collection that
    // exists today, and its pixels are the baseline the whole change is
    // measured against.
    const base = await request.patch(`/api/v1/collections/${collectionId}`, {
      data: {
        cover_asset_id: memberIds[0],
        cover_focal_x: 0.25,
        cover_focal_y: 0.75,
        clear_cover_zoom: true,
      },
    });
    expect(base.status(), 'the baseline PATCH must be accepted').toBe(200);

    async function loadTile() {
      await page.goto('/collections');
      const card = page
        .locator('a', { hasText: COLLECTION_NAME })
        .locator('[data-testid="collection-card-cover"]')
        .first();
      await expect(card).toBeVisible();
      await page.waitForTimeout(300);
      return card;
    }

    const before = await loadTile();
    expect(await before.getAttribute('data-zoom'), 'an unzoomed tile must claim no zoom').toBe('');
    // THE BYTE-IDENTICAL CLAIM, asserted on the box the browser actually
    // laid out: at the fit the image is exactly the tile, which is what
    // `inset-0 h-full w-full` gave it before this change.
    const fitGeom = await before.evaluate((el) => {
      const img = el as HTMLImageElement;
      const box = img.parentElement!.getBoundingClientRect();
      const r = img.getBoundingClientRect();
      return { dw: r.width - box.width, dh: r.height - box.height, dx: r.x - box.x, dy: r.y - box.y };
    });
    expect(Math.abs(fitGeom.dw)).toBeLessThan(0.5);
    expect(Math.abs(fitGeom.dh)).toBeLessThan(0.5);
    expect(Math.abs(fitGeom.dx)).toBeLessThan(0.5);
    expect(Math.abs(fitGeom.dy)).toBeLessThan(0.5);
    const beforeShot = await before.screenshot();

    const zoomed = await request.patch(`/api/v1/collections/${collectionId}`, {
      data: { cover_zoom: 3 },
    });
    expect(zoomed.status(), 'the zoom PATCH must be accepted').toBe(200);

    const after = await loadTile();
    expect(await after.getAttribute('data-zoom')).toBe('3');
    const zoomGeom = await after.evaluate((el) => {
      const img = el as HTMLImageElement;
      const box = img.parentElement!.getBoundingClientRect();
      const r = img.getBoundingClientRect();
      return { w: r.width / box.width, h: r.height / box.height };
    });
    expect(zoomGeom.w, 'the tile is not laying the picture out three times its own width').toBeCloseTo(3, 1);
    expect(zoomGeom.h).toBeCloseTo(3, 1);

    // AND THE PIXELS MOVED. The DOM assertions above say the geometry
    // is right; this one says a person can see it.
    const afterShot = await after.screenshot();
    expect(
      Buffer.compare(beforeShot, afterShot) === 0,
      'the tile renders identically zoomed and unzoomed — the value is stored, read, and paints ' +
        'nothing',
    ).toBe(false);

    // BACK TO NULL, BACK TO THE OLD PICTURE, byte for byte. This is the
    // regression that matters: every collection that exists has a null
    // zoom, and none of them may change.
    const cleared = await request.patch(`/api/v1/collections/${collectionId}`, {
      data: { clear_cover_zoom: true },
    });
    expect(cleared.status()).toBe(200);
    const restored = await loadTile();
    expect(await restored.getAttribute('data-zoom')).toBe('');
    expect(
      Buffer.compare(beforeShot, await restored.screenshot()) === 0,
      'clearing the zoom did not restore the exact picture the tile painted before it existed',
    ).toBe(true);
  });

  test('the featured strip paints the zoom on the real card', async ({ page, request }) => {
    await request.patch(`/api/v1/collections/${collectionId}`, {
      data: {
        cover_asset_id: memberIds[0],
        featured_cover_asset_id: memberIds[1],
        featured_cover_focal_x: 0.2,
        featured_cover_focal_y: 0.8,
        clear_featured_cover_zoom: true,
        visibility: 'public',
      },
    });
    const placed = await request.post('/api/v1/admin/featured', {
      data: { subject_kind: 'collection', subject_id: collectionId, scope: 'public' },
    });
    expect(placed.ok(), 'featuring the fixture collection must succeed').toBeTruthy();
    const placementId = ((await placed.json()) as { id: string }).id;

    try {
      async function loadTile() {
        await page.goto('/');
        const el = page
          .getByTestId('featured-rail-item')
          .filter({ hasText: COLLECTION_NAME })
          .first();
        await expect(el).toBeVisible();
        await page.waitForTimeout(300);
        return el.locator('img').first();
      }

      const before = await loadTile();
      expect(await before.getAttribute('data-zoom'), 'an unzoomed tile must claim no zoom').toBe('');
      const beforeShot = await before.screenshot();

      const patched = await request.patch(`/api/v1/collections/${collectionId}`, {
        data: { featured_cover_zoom: 2.5 },
      });
      expect(patched.status(), 'the featured zoom PATCH must be accepted').toBe(200);

      // THE WIRE CARRIES IT — the rail's own payload, not the
      // collection's, because the rail resolves its cover through a
      // preference ladder and only the chosen rungs may carry a framing.
      const rail = await request.get('/api/v1/featured?limit=50');
      const { items } = (await rail.json()) as {
        items: Array<{ subject_id: string; cover_zoom?: number | null }>;
      };
      const row = items.find((i) => i.subject_id === collectionId);
      expect(row?.cover_zoom, 'the rail dropped the zoom').toBeCloseTo(2.5, 6);

      const after = await loadTile();
      expect(await after.getAttribute('data-zoom')).toBe('2.5');
      expect(
        Buffer.compare(beforeShot, await after.screenshot()) === 0,
        'the rail card renders identically zoomed and unzoomed',
      ).toBe(false);
    } finally {
      await request.delete(`/api/v1/admin/featured/${placementId}`).catch(() => undefined);
    }
  });

  test("a portrait cover is shown at its TRUE shape, and zoom frees its horizontal axis", async ({
    page,
    request,
  }) => {
    // THE FIXTURE IS SHARED AND THE FILE IS SERIAL, so the framing an
    // earlier test left behind is this one's starting state. Reset it
    // explicitly rather than depending on execution order — the
    // assertion below is "at the FIT there is no horizontal travel",
    // and it is meaningless if the slot arrives already tightened.
    const reset = await request.patch(`/api/v1/collections/${collectionId}`, {
      data: { clear_featured_cover_zoom: true, clear_featured_cover_focal: true },
    });
    expect(reset.status(), 'the pre-test reset must be accepted').toBe(200);

    const editor = await openCoverEditor(page);
    await editor
      .getByTestId('featured-cover-choice')
      .filter({ has: page.locator(`[src*="${portraitId}"]`) })
      .first()
      .click();
    const stage = page.getByTestId('cover-editor-stage-image');
    await expect(stage).toHaveAttribute('src', new RegExp(portraitId));
    await expect(page.getByTestId('cover-editor-marquee')).toBeVisible();

    // THE STAGE MUST NOT LIE ABOUT THE PICTURE'S SHAPE.
    //
    // This has now been got wrong twice on this element, both times by
    // reasoning about CSS instead of measuring it. #1195 shipped a
    // definite HEIGHT with a `max-width` and squashed wide pictures;
    // #1207 replaced it with a definite WIDTH plus a `max-height` and
    // recorded that the combination "cannot distort", which is false —
    // Chrome clips the height and leaves the specified width alone, so
    // a 200x420 fixture was laid out at 608x562. A curator framing a
    // subject by eye against a picture of the wrong shape is not
    // framing it, and a portrait is precisely the picture #1212 is
    // about. So the invariant is asserted rather than argued.
    const shape = await stage.evaluate((el) => {
      const img = el as HTMLImageElement;
      const r = img.getBoundingClientRect();
      return { natural: img.naturalWidth / img.naturalHeight, laid: r.width / r.height };
    });
    expect(
      Math.abs(shape.laid - shape.natural) / shape.natural,
      `the stage is distorting the picture: natural aspect ${shape.natural}, laid out at ` +
        `${shape.laid}`,
    ).toBeLessThan(0.01);

    // THE OWNER'S COMPLAINT, VERBATIM: "I can't center items that are
    // left aligned." A portrait in the 890:500 window keeps its whole
    // width, so there is no horizontal travel at all until it is
    // tightened.
    const marquee = page.getByTestId('cover-editor-marquee');
    const fitBox = (await marquee.boundingBox())!;
    const stageBox = (await stage.boundingBox())!;
    expect(
      Math.abs(fitBox.width - stageBox.width) < 2,
      'the portrait fixture is not pinned horizontally, so this test proves nothing',
    ).toBe(true);

    await page.mouse.move(fitBox.x + fitBox.width / 2, fitBox.y + fitBox.height / 2);
    await page.mouse.down();
    await page.mouse.move(fitBox.x + fitBox.width * 0.1, fitBox.y + fitBox.height / 2, { steps: 8 });
    await page.mouse.up();
    expect(
      (await readFocal(page)).x,
      'the horizontal axis moved at the fit — the marquee is not honest about what it can do',
    ).toBe('0.5');

    await marquee.focus();
    for (let i = 0; i < 6; i++) await page.keyboard.press('+');
    expect(Number(await readZoom(page)), 'the keyboard zoom did nothing').toBeGreaterThan(1.5);

    const tight = (await marquee.boundingBox())!;
    expect(tight.width).toBeLessThan(fitBox.width - 1);
    await page.mouse.move(tight.x + tight.width / 2, tight.y + tight.height / 2);
    await page.mouse.down();
    await page.mouse.move(tight.x + tight.width * 0.1 - stageBox.width * 0.2, tight.y + tight.height / 2, {
      steps: 8,
    });
    await page.mouse.up();
    expect(
      Number((await readFocal(page)).x),
      'a tightened portrait still cannot be moved sideways — the whole issue',
    ).toBeLessThan(0.45);
  });
});
