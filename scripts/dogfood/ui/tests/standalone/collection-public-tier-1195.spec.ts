// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1195 — a collection can be made PUBLIC from the edit modal, and the
// tier that gets stored is the one an anonymous visitor then reads.
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
// The ungranted direction is asserted with the same fixture, before the
// change: a fresh collection is `private`, and anonymous must get
// nothing for it. Without that, "anonymous can read it" would pass on an
// instance where anonymous can read everything.
//
// NOT covered here, deliberately: the instance-switch gating (the option
// is hidden when public mode is off). Pinning it in this suite would
// mean toggling `public_mode` on the shared dev instance mid-run, and a
// run that dies between the toggle and its restore leaves the whole
// install private for whatever runs next. The Go side pins the flag the
// gate reads — sysconfig's TestPublicAppearancePublishesPublicMode.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

const STAMP = Date.now();
const NAME = `#1195 public tier ${STAMP}`;

let collectionId: string | undefined;

test.describe('#1195 the collection edit modal can publish a collection', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    const created = await request.post('/api/v1/collections', {
      data: { name: NAME, description: 'fixture for #1195' },
    });
    expect(created.status(), 'fixture collection must be created').toBe(201);
    collectionId = ((await created.json()) as { id: string }).id;
  });

  test.afterAll(async ({ request }) => {
    if (collectionId) {
      await request.delete(`/api/v1/collections/${collectionId}`).catch(() => undefined);
    }
  });

  test('the modal offers Public, saves it, and an anonymous visitor reads it back', async ({
    page,
    browser,
  }) => {
    // The control, taken FIRST. A brand-new collection defaults to
    // `private`, so anonymous must be refused it — otherwise the 200
    // below proves nothing about the tier.
    const anonBefore = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    try {
      const r = await anonBefore.request.get(`/api/v1/collections/${collectionId}`);
      expect(r.status(), 'a private collection must not be readable anonymously').not.toBe(200);
    } finally {
      await anonBefore.close();
    }

    await loginAsAdminViaUI(page);
    await page.goto(`/collections/${collectionId}`);
    await page.getByTestId('collection-detail-more-button').first().click();
    await page.getByTestId('collection-detail-edit-menuitem').first().click();

    const dialog = page.locator('[role="dialog"]');
    await expect(dialog).toBeVisible();

    const publicRadio = dialog.locator('input[name="vis_edit"][value="public"]');
    await expect(
      publicRadio,
      'the modal offers no Public tier — the enum that hid it is back (#1195/#1176)',
    ).toHaveCount(1);

    // The input is `sr-only` inside its label, so the label is what a
    // person clicks and `force` is what stands in for that here.
    await publicRadio.click({ force: true });
    await page.getByRole('button', { name: /^save$/i }).click();
    await expect(dialog).toBeHidden();

    const anonAfter = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    try {
      const r = await anonAfter.request.get(`/api/v1/collections/${collectionId}`);
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
});
