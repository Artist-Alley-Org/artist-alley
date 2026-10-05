// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1195 — a collection can be made PUBLIC from the edit modal, the tier
// that gets stored is the one an anonymous visitor then reads, and the
// option is offered only on an install where it means anything.
//
// The tier had been in the column's CHECK since migration 00008 and in
// the collection read rule ever since; the OpenAPI enum, and therefore
// every generated client, was the only thing still saying it did not
// exist. That shape of defect — a value the server serves and no client
// can name — is invisible to any test that talks to the API directly
// with a hand-written body, so this one drives the REAL modal: open it
// from the collection page, click the radio, click Save.
//
// The assertion is the PERSISTED value, read back through a context with
// no session at all. Re-reading it as the admin who just wrote it would
// pass on a build where the handler echoed its own request body back
// without storing anything (#946), and would say nothing about the one
// thing `public` is FOR.
//
// ⚠️ CONTENDED INSTANCE STATE: `system.public_mode`.
//
// THIS SPEC OWNS THE PUBLIC-MODE SWITCH FOR ITS DURATION, and has to.
// The option is gated on the instance flag, so a spec that merely
// assumed a state would pass on the dev box (public mode on) and fail in
// CI (a fresh install, off) — which is exactly what the first version of
// this file did. It reads the prior value in beforeAll and puts it back
// in afterAll, the same shape share-discovery-875-876 uses for
// self-registration. Serial mode, because the tests hand the switch to
// each other.
//
// It is NOT the only writer: collection-cover-editor-1207,
// ai-toggle-1251 and advanced-operators-1165-1173-1197 write the same
// setting, and serial mode says nothing about two FILES in two workers.
// So the switch is taken through the cross-file lock in
// helpers/public-mode.ts, which reads the prior value INSIDE the
// critical section; see that file for the lost-update shape.
//
// ⚠️ THAT IS NOT WHY THIS FILE GOES RED, and the guess that it was cost
// a round: the rotating "the published collection must be readable
// anonymously — expected 200, received 404" survives the lock. The
// instance is public when this asserts; the COLLECTION is not, because
// the save stored `private`. The stored rows agree — 2 of 27
// `#1195 published` collections carry `visibility = 'private'`, and both
// timestamps land on an observed failure.
//
// ⛔ AND THE MODAL IS WHAT SENT IT. The request recorder below caught the
// failing run's PATCH:
//
//   {"name":"#1195 published …","description":"fixture for #1195",
//    "visibility":"private","if_unchanged_since":"…"}
//
// `visibility: private` — sent immediately after this test asserted the
// Public radio WAS checked. So the component's state was put back
// between the click and the submit, and nothing below the browser is
// involved. EditCollectionModal.svelte:201 is where to look: that
// `$effect` re-seeds `name`, `description`, `visibility`, `coverAssetId`
// and `framing` from the `collection` prop whenever the prop changes
// identity — not only when the dialog opens — and the file's own comment
// records this already happening mid-edit under the full parallel suite.
// The fix applied then covered `page` only, not the edited fields.
//
// A product defect, filed rather than fixed here.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { publicModeHold } from '../../helpers/public-mode';

const STAMP = Date.now();
const PUBLISHED = `#1195 published ${STAMP}`;
const PRIVATE = `#1195 stays private ${STAMP}`;

let publishedId: string | undefined;
let privateId: string | undefined;
const publicMode = publicModeHold('collection-public-tier-1195');

test.describe('#1195 the collection edit modal can publish a collection', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    // Waiting for another writer to give the switch back is normal and
    // can outlast the default 30s hook budget — 1207 holds it for its
    // whole file. A hook that dies waiting would look like a product
    // failure, so the wait gets room and the lock's own timeout (which
    // names the holder) is what reports a genuine jam.
    test.setTimeout(360_000);
    await publicMode.acquire(request);

    for (const [name, set] of [
      [PUBLISHED, (id: string) => (publishedId = id)],
      [PRIVATE, (id: string) => (privateId = id)],
    ] as const) {
      const created = await request.post('/api/v1/collections', {
        data: { name, description: 'fixture for #1195' },
      });
      expect(created.status(), `fixture collection ${name} must be created`).toBe(201);
      set(((await created.json()) as { id: string }).id);
    }
  });

  test.afterAll(async ({ request }) => {
    // The switch goes back and the lock is handed on FIRST, so a
    // fixture-delete failure below cannot leave the next spec waiting.
    await publicMode.release(request);
    for (const id of [publishedId, privateId]) {
      if (id) await request.delete(`/api/v1/collections/${id}`).catch(() => undefined);
    }
  });

  async function setPublicMode(request: import('@playwright/test').APIRequestContext, on: boolean) {
    await publicMode.set(request, on);
  }

  async function openEditModal(page: import('@playwright/test').Page, id: string) {
    await page.goto(`/collections/${id}`);
    await page.getByTestId('collection-detail-more-button').first().click();
    await page.getByTestId('collection-detail-edit-menuitem').first().click();
    const dialog = page.locator('[role="dialog"]');
    await expect(dialog).toBeVisible();
    return dialog;
  }

  test('the modal offers Public, saves it, and an anonymous visitor reads it back', async ({
    page,
    browser,
    request,
  }) => {
    await setPublicMode(request, true);

    // ⚠️ WHAT THE MODAL ACTUALLY SENT, kept for the failure message.
    //
    // This test used to report its failure as a bare `Expected 200,
    // received 404` from the anonymous read, and 404 has TWO causes here
    // that need opposite responses: the instance refused an anonymous
    // caller, or the collection is not public because the SAVE stored the
    // wrong tier. They are indistinguishable from outside, and a whole
    // round went into the first before the stored rows ruled it out.
    // Recording the PATCH costs nothing on the passing path and turns the
    // next failure into a diagnosis rather than an investigation.
    const patches: string[] = [];
    page.on('request', (r) => {
      if (r.method() === 'PATCH' && r.url().includes('/api/v1/collections/')) {
        patches.push(r.postData() ?? '(no body)');
      }
    });

    // The control, taken FIRST and with anonymous browsing already ON, so
    // the refusal is about the TIER and not about the instance switch. A
    // brand-new collection is `private`.
    const anonBefore = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    try {
      const r = await anonBefore.request.get(`/api/v1/collections/${publishedId}`);
      expect(r.status(), 'a private collection must not be readable anonymously').not.toBe(200);
    } finally {
      await anonBefore.close();
    }

    await loginAsAdminViaUI(page);
    const dialog = await openEditModal(page, publishedId!);

    const publicRadio = dialog.locator('input[name="vis_edit"][value="public"]');
    await expect(
      publicRadio,
      'the modal offers no Public tier — the enum that hid it is back (#1195/#1176)',
    ).toHaveCount(1);

    // WAIT FOR THE MODAL TO HAVE READ THE COLLECTION BEFORE TOUCHING IT.
    // The form seeds itself from the collection in an effect that runs on
    // open; a click that lands first is overwritten by that seed and the
    // save then stores the tier the collection already had. Nobody can
    // click that fast — Playwright can, and did: the first version of
    // this test saved `private` and reported it as a persistence bug.
    // Asserting the CURRENT tier is selected is how "the seed has run"
    // is observed rather than waited out.
    await expect(
      dialog.locator('input[name="vis_edit"][value="private"]'),
      'the modal has not seeded itself from the collection yet',
    ).toBeChecked();

    // The input is `sr-only` inside its label, so the label is what a
    // person clicks and `force` is what stands in for that here.
    await publicRadio.click({ force: true });
    await expect(publicRadio, 'the Public radio did not take the click').toBeChecked();
    await page.getByRole('button', { name: /^save$/i }).click();
    await expect(dialog).toBeHidden();

    // ── which of the two 404s is it? ─────────────────────────────────
    //
    // Asked BEFORE the anonymous read and as the admin, purely to name
    // the failure. It is NOT the assertion this file exists for — an
    // admin read-back would pass on a build that echoed its own request
    // body (#946), which is why the claim below is still made with no
    // session at all. This only decides which story the failure tells.
    const asOwner = await request.get(`/api/v1/collections/${publishedId}`);
    const storedTier =
      asOwner.status() === 200
        ? ((await asOwner.json()) as { visibility?: string }).visibility
        : `unreadable (${asOwner.status()})`;
    expect(
      storedTier,
      `the modal reported a successful save and the STORED tier is ${storedTier}. ` +
        `The curator chose Public; something between the click and the PATCH put the old ` +
        `value back. What the modal sent: ${JSON.stringify(patches)}. This is a SAVE ` +
        `defect in the collection edit modal, not an anonymous-read one — the anonymous ` +
        `404 below is the API correctly refusing a collection that is still private. ` +
        `Observed twice on 2026-08-23 under the full two-worker suite and never once in ` +
        `isolation (0 failures in 6 solo runs, 8 instrumented repeats at two workers, and ` +
        `3 runs each at 4x and 8x CPU throttle).`,
    ).toBe('public');

    const anonAfter = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    try {
      const r = await anonAfter.request.get(`/api/v1/collections/${publishedId}`);
      expect(r.status(), 'the published collection must be readable anonymously').toBe(200);
      const body = (await r.json()) as { visibility?: string };
      expect(
        body.visibility,
        'the STORED tier is what matters — an echoed request body would pass otherwise',
      ).toBe('public');
    } finally {
      await anonAfter.close();
    }
  });

  test('with anonymous browsing off the tier is not offered — unless it is already set', async ({
    page,
    request,
  }) => {
    await setPublicMode(request, false);
    await loginAsAdminViaUI(page);

    // A collection that is NOT public: the option promises a reach this
    // install cannot deliver, so it is absent.
    const priv = await openEditModal(page, privateId!);
    await expect(priv.locator('input[name="vis_edit"]')).not.toHaveCount(0);
    await expect(
      priv.locator('input[name="vis_edit"][value="public"]'),
      'Public is offered on an install with anonymous browsing off',
    ).toHaveCount(0);

    // The collection published by the test above keeps the option, with
    // the caveat printed. Hiding it would leave the radio group with
    // nothing selected and present the collection as something it is not.
    const pub = await openEditModal(page, publishedId!);
    const stillOffered = pub.locator('input[name="vis_edit"][value="public"]');
    await expect(
      stillOffered,
      'an already-public collection lost the tier it is actually set to',
    ).toHaveCount(1);
    // Selected, not merely present — the point of the exception is that
    // the picker keeps showing the collection's real state.
    await expect(stillOffered).toBeChecked();
    await expect(pub.getByTestId('collection-public-inert-note')).toBeVisible();
  });
});
