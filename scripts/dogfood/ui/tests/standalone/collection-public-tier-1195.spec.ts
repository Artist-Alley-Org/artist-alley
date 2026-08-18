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
// ⚠️ THIS SPEC OWNS THE PUBLIC-MODE SWITCH FOR ITS DURATION, and has to.
// The option is gated on the instance flag, so a spec that merely
// assumed a state would pass on the dev box (public mode on) and fail in
// CI (a fresh install, off) — which is exactly what the first version of
// this file did. It reads the prior value in beforeAll and puts it back
// in afterAll, the same shape share-discovery-875-876 uses for
// self-registration. Serial mode, because the tests hand the switch to
// each other.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

const STAMP = Date.now();
const PUBLISHED = `#1195 published ${STAMP}`;
const PRIVATE = `#1195 stays private ${STAMP}`;

let publishedId: string | undefined;
let privateId: string | undefined;
let priorPublicMode: boolean | undefined;

test.describe('#1195 the collection edit modal can publish a collection', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    const mode = await request.get('/api/v1/admin/system/public-mode');
    expect(mode.status(), 'public-mode state must be readable as admin').toBe(200);
    priorPublicMode = ((await mode.json()) as { enabled: boolean }).enabled;

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
    if (priorPublicMode !== undefined) {
      await request
        .patch('/api/v1/admin/system/public-mode', { data: { enabled: priorPublicMode } })
        .catch(() => undefined);
    }
    for (const id of [publishedId, privateId]) {
      if (id) await request.delete(`/api/v1/collections/${id}`).catch(() => undefined);
    }
  });

  async function setPublicMode(request: import('@playwright/test').APIRequestContext, on: boolean) {
    const r = await request.patch('/api/v1/admin/system/public-mode', { data: { enabled: on } });
    expect(r.status(), `public mode must be settable to ${on}`).toBe(200);
    expect(((await r.json()) as { enabled: boolean }).enabled).toBe(on);
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
