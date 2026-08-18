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

import { expect, test, type Page } from '../../helpers/test';

const STAMP = Date.now();
const COLLECTION_NAME = `#1207 cover editor ${STAMP}`;

interface AssetListItem {
  id: string;
  processing_status?: string;
  file_extension?: string;
}

interface CollectionPayload {
  id: string;
  cover_asset_id?: string | null;
  featured_cover_asset_id?: string | null;
  featured_cover_focal_x?: number | null;
  featured_cover_focal_y?: number | null;
}

let collectionId: string | undefined;
let memberIds: string[] = [];

function isRasterImage(ext?: string): boolean {
  return ['jpg', 'jpeg', 'png', 'webp', 'gif', 'avif'].includes((ext ?? '').toLowerCase());
}

test.describe('#1207 the collection cover editor', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    // Sorted by id so the SAME assets are chosen on every run whatever
    // order Postgres hands them back — the #488 flake, avoided the way
    // ui-30 avoids it.
    const res = await request.get('/api/v1/assets?limit=200');
    expect(res.ok(), 'GET /assets must succeed').toBeTruthy();
    const { items } = (await res.json()) as { items: AssetListItem[] };
    const candidates = (items ?? [])
      .filter((a) => a.processing_status === 'ready' && isRasterImage(a.file_extension))
      .sort((a, b) => a.id.localeCompare(b.id));

    // TWO members, not one, and the whole spec depends on it: a single
    // picture cannot tell "the featured slot took my choice" from "the
    // featured slot inherited the collection cover".
    expect(
      candidates.length,
      'the seeded stack needs at least two ready raster assets to tell the two cover slots apart',
    ).toBeGreaterThanOrEqual(2);
    memberIds = candidates.slice(0, 2).map((a) => a.id);

    const created = await request.post('/api/v1/collections', {
      data: { name: COLLECTION_NAME, description: 'fixture for #1207' },
    });
    expect(created.status(), 'fixture collection must be created').toBe(201);
    collectionId = ((await created.json()) as { id: string }).id;

    for (const assetId of memberIds) {
      const pinned = await request.post(`/api/v1/collections/${collectionId}/resources`, {
        data: { asset_id: assetId, pinned: true },
      });
      expect(pinned.ok(), `pinning ${assetId} must succeed`).toBeTruthy();
    }
  });

  test.afterAll(async ({ request }) => {
    if (collectionId) await request.delete(`/api/v1/collections/${collectionId}`).catch(() => undefined);
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
  async function readFocal(page: Page): Promise<{ x: string; y: string }> {
    const el = page.getByTestId('cover-editor-position');
    return {
      x: (await el.getAttribute('data-focal-x')) ?? '',
      y: (await el.getAttribute('data-focal-y')) ?? '',
    };
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
    await expect(page.getByTestId('cover-editor-collection-preview')).toHaveAttribute(
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

    // ⚠️ STATED IN PIXELS ON THE STAGE, not in fractions, and the
    // reason is worth recording rather than rediscovering.
    //
    // The marquee is drawn from the 890:500 CONSTANT; the card preview
    // is laid out by CSS `aspect-ratio: 890 / 500`, which lands on a
    // sub-pixel fraction of that. So the two windows differ by exactly
    // the box's layout rounding — a few thousandths of the picture — and
    // a fraction-space tolerance would either be too loose to catch a
    // real misalignment or would fail on a browser rounding a box half
    // a pixel differently.
    //
    // A pixel on the stage is the bar that means something: it is the
    // unit the curator judges the crop in. Under it, the marquee and
    // the card are the same rectangle as far as anyone can see; over
    // it, they are visibly different rectangles and the preview lies.
    const deltas = {
      left: Math.abs(alignment.marquee.left - alignment.card.left) * alignment.stage.width,
      top: Math.abs(alignment.marquee.top - alignment.card.top) * alignment.stage.height,
      width: Math.abs(alignment.marquee.width - alignment.card.width) * alignment.stage.width,
      height: Math.abs(alignment.marquee.height - alignment.card.height) * alignment.stage.height,
    };
    // eslint-disable-next-line no-console
    console.log('#1207 marquee-vs-card alignment (px on the stage):', JSON.stringify(deltas));
    for (const axis of ['left', 'top', 'width', 'height'] as const) {
      expect(
        deltas[axis],
        `the marquee's ${axis} is more than a pixel from the card's actual crop — ` +
          `marquee ${JSON.stringify(alignment.marquee)} vs card ${JSON.stringify(alignment.card)}`,
      ).toBeLessThan(1);
    }
    // And the frame really is the strip's shape, to within a pixel of
    // width — the half of the claim the tolerance above absorbs.
    expect(
      Math.abs(alignment.cardBox.width - (alignment.cardBox.height * 890) / 500),
      'the preview frame is not 890:500; the marquee is marking the wrong shape',
    ).toBeLessThan(1);

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
});
