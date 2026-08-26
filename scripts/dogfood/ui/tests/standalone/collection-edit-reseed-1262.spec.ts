// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1262 — the collection edit dialog seeds ONCE PER OPEN.
//
// The seeding `$effect` in EditCollectionModal re-ran whenever anything
// it read changed, not only when the dialog opened, and it read three
// kinds of thing: the `collection` prop, the preview-ladder store, and
// its own writes. Each re-run replaced the curator's unsaved values with
// the stored ones, advanced `baselineUpdatedAt` — the optimistic-
// concurrency token — to whatever the prop then held, and cleared any
// conflict message on screen.
//
// ⚠️ THE TRIGGER THAT ACTUALLY FIRES TODAY IS NOT THE PROP, and the two
// tests below are split on exactly that.
//
//   `previewLadder.init()` is called from inside the seed, and its first
//   statement reads `this.loaded` — a `$state`. Svelte collects
//   dependencies through the whole call frame, so the seed subscribed to
//   it. `GET /previews` resolves a moment after the page loads, `loaded`
//   flips false → true, and the seed runs a second time. That needs no
//   refetch, no second curator and no race: it happens on every open
//   that beats the ladder fetch, which under a loaded parallel suite is
//   how `collection-public-tier-1195.spec.ts:88` came to PATCH
//   `visibility: private` immediately after asserting Public was
//   checked. Test A drives it, by holding `/previews` open.
//
//   The `collection` PROP changing identity is the case #1262 is written
//   around, and it is the one that also moves the concurrency baseline.
//   Today the collection page has exactly one reachable caller of
//   `load()` after mount — the admin restore button — so test B goes
//   through it. See the route note in that test for the one field it has
//   to inject to get there.
//
// Both directions are covered because the re-seed is symmetric and only
// one of the two is a disclosure: test A NARROWS a public collection and
// test B WIDENS a private one. The narrowing is the one that matters —
// a curator restricts a collection, the dialog reports success, and the
// collection stays public with nothing on screen saying so.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

const STAMP = Date.now();

interface CollectionRow {
  id: string;
  name: string;
  description: string;
  visibility: string;
  updated_at: string;
}

/** Open the edit dialog from the collection page's More menu.
 *
 *  ⛔ THE MENU HAS ONE EDITING ENTRY (#1264). "Set cover" opened this
 *  same dialog deep-linked to its cover page and is gone; so is the
 *  disabled "Manage members" stub beside it. Asserted here rather than
 *  in a test of its own because every test in this file walks past the
 *  menu, so a second entry reappearing cannot go unnoticed — and on the
 *  pre-#1264 build this fails immediately.
 *
 *  ONE SURFACE (#1264): the identity fields and both cover slots are on
 *  screen together, so the visibility ladder this spec drives and the
 *  cover editor share one dialog and one Save. */
async function openEditModal(page: import('@playwright/test').Page, id: string) {
  await page.goto(`/collections/${id}`);
  await page.getByTestId('collection-detail-more-button').first().click();
  await expect(page.getByTestId('collection-detail-set-cover-menuitem')).toHaveCount(0);
  await page.getByTestId('collection-detail-edit-menuitem').first().click();
  const dialog = page.locator('[role="dialog"]');
  await expect(dialog).toBeVisible();
  // The cover editor is part of THIS surface, not a page behind a
  // button — the seed below has to hold with it mounted and its own
  // fetches in flight.
  await expect(dialog.getByTestId('collection-cover-editor')).toBeVisible();
  return dialog;
}

/** The tier radio for `v`, inside the dialog. The input is `sr-only`
 *  inside its label, so `force` stands in for clicking the label. */
function tier(dialog: import('@playwright/test').Locator, v: string) {
  return dialog.locator(`input[name="vis_edit"][value="${v}"]`);
}

// NOT `mode: 'serial'`, deliberately. The two tests share no instance
// state and make their own fixtures, and serial mode SKIPS the rest of
// the file after the first failure — which on the pre-#1262 build hid
// the refetch case behind the ladder case and reported it as a pass.
test.describe('#1262 the collection edit dialog seeds once per open', () => {
  const created: string[] = [];

  test.afterAll(async ({ request }) => {
    for (const id of created) {
      await request.delete(`/api/v1/collections/${id}`).catch(() => undefined);
    }
  });

  async function makeCollection(
    request: import('@playwright/test').APIRequestContext,
    name: string,
    visibility?: string,
  ): Promise<CollectionRow> {
    const created_ = await request.post('/api/v1/collections', {
      data: { name, description: 'fixture for #1262' },
    });
    expect(created_.status(), `fixture collection ${name} must be created`).toBe(201);
    const row = (await created_.json()) as CollectionRow;
    created.push(row.id);
    if (visibility) {
      const patched = await request.patch(`/api/v1/collections/${row.id}`, {
        data: { visibility },
      });
      expect(patched.status(), `fixture collection ${name} must reach ${visibility}`).toBe(200);
      return (await patched.json()) as CollectionRow;
    }
    return row;
  }

  test('an unsaved RESTRICTION survives the preview ladder settling', async ({ page, request }) => {
    const row = await makeCollection(request, `#1262 restrict ${STAMP}`, 'public');

    // ── hold `GET /previews` open ─────────────────────────────────────
    //
    // The store's `loaded` flag is what the seed accidentally subscribed
    // to, so the window this test needs is "the dialog is open and the
    // ladder has not answered yet". Holding the response is how that
    // window is MADE rather than waited for; the alternative is opening
    // the dialog fast enough to beat a local fetch, which is the racing
    // this replaces.
    let releaseLadder: (() => void) | undefined;
    const ladderHeld = new Promise<void>((resolve) => (releaseLadder = resolve));
    await page.route('**/api/v1/previews', async (route) => {
      await ladderHeld;
      await route.continue();
    });

    await loginAsAdminViaUI(page);
    const dialog = await openEditModal(page, row.id);

    // The seed has run when the stored tier is the checked one. Asserted
    // rather than waited out — same reason collection-public-tier-1195
    // does it: a click that lands before the seed is overwritten by it,
    // and the save then stores the tier the collection already had.
    await expect(
      tier(dialog, 'public'),
      'the dialog has not seeded itself from the collection yet',
    ).toBeChecked();

    const nameField = dialog.locator('input[type="text"]').first();
    const editedName = `${row.name} EDITED`;
    await nameField.fill(editedName);
    await tier(dialog, 'private').click({ force: true });
    await expect(tier(dialog, 'private'), 'the Private radio did not take the click').toBeChecked();

    // Let the ladder answer. In the pre-#1262 build this re-runs the
    // whole seed and puts `public` back.
    releaseLadder?.();
    await page.waitForResponse((r) => r.url().includes('/api/v1/previews'));
    // The re-seed was synchronous inside the effect that the response
    // triggers, so one settled frame is enough for it to have happened.
    await page.waitForTimeout(500);

    await expect(
      tier(dialog, 'private'),
      'the preview ladder settling put the stored tier back — the seed ran twice (#1262). ' +
        'This is the DISCLOSURE direction: the curator restricted a public collection and ' +
        'the dialog is about to save `public`.',
    ).toBeChecked();
    await expect(nameField, 'the re-seed also reverted the unsaved name').toHaveValue(editedName);

    await page.getByRole('button', { name: /^save$/i }).click();
    await expect(dialog).toBeHidden();

    // THE PERSISTED VALUE, not the one the dialog echoed back (#946).
    const stored = (await (await request.get(`/api/v1/collections/${row.id}`)).json()) as CollectionRow;
    expect(
      stored.visibility,
      'the dialog reported a successful save and the STORED tier is still public',
    ).toBe('private');
    expect(stored.name, 'the unsaved name did not survive to the save').toBe(editedName);
  });

  test('a mid-edit REFETCH moves neither the form nor the concurrency baseline', async ({
    page,
    request,
  }) => {
    const row = await makeCollection(request, `#1262 refetch ${STAMP}`);

    // Soft-delete it, so the collection page renders the admin restore
    // banner — the page's only reachable caller of `load()` after mount,
    // and therefore the only way to reassign the `collection` prop while
    // the dialog is open. The restore is REAL: it writes the row and
    // advances `updated_at`, which is what makes the baseline assertion
    // below a genuine concurrent-edit test rather than a simulated one.
    const deleted = await request.delete(`/api/v1/collections/${row.id}`);
    expect(deleted.status(), 'the fixture must soft-delete').toBe(204);

    // ⚠️ THE ONE INJECTED FIELD, and it is injected to match the database
    // rather than to fake it. `GET /collections/{id}` never returns
    // `deleted_at` — the schema says it is populated "only on rows
    // surfaced by the admin include_deleted listing" — so the page's
    // restore banner, which is gated on `collection.deleted_at`, cannot
    // render for a row that really is soft-deleted. Reported as a
    // separate finding; here the field is put back so the existing
    // control can be reached. Everything downstream of the click is the
    // real server.
    await page.route(`**/api/v1/collections/${row.id}`, async (route) => {
      if (route.request().method() !== 'GET') return route.continue();
      const res = await route.fetch();
      const body = (await res.json()) as Record<string, unknown>;
      if (body && body.deleted_at == null) body.deleted_at = new Date().toISOString();
      await route.fulfill({ response: res, json: body });
    });

    // What the dialog will capture on open, read AFTER the soft delete —
    // a soft delete is an UPDATE and moves `updated_at` with it, so the
    // row's creation timestamp is not the baseline the dialog sees.
    const baseline = (
      (await (await request.get(`/api/v1/collections/${row.id}`)).json()) as CollectionRow
    ).updated_at;

    await loginAsAdminViaUI(page);
    const dialog = await openEditModal(page, row.id);
    await expect(
      tier(dialog, 'private'),
      'the dialog has not seeded itself from the collection yet',
    ).toBeChecked();

    const nameField = dialog.locator('input[type="text"]').first();
    const editedName = `${row.name} EDITED`;
    await nameField.fill(editedName);
    await tier(dialog, 'org-only').click({ force: true });
    await expect(tier(dialog, 'org-only'), 'the Org-only radio did not take the click').toBeChecked();

    // What the dialog sent, kept for the failure message — the same
    // recorder collection-public-tier-1195 added, for the same reason:
    // "the save went through" and "the save carried the wrong baseline"
    // are indistinguishable from the outcome alone.
    const patches: string[] = [];
    page.on('request', (r) => {
      if (r.method() === 'PATCH' && r.url().includes('/api/v1/collections/')) {
        patches.push(r.postData() ?? '(no body)');
      }
    });

    // THE REFETCH. `dispatchEvent` rather than `click` because the
    // dialog's backdrop covers the button — this is producing an
    // upstream refetch, not testing the restore control.
    const restored = page.waitForResponse(
      (r) => r.url().includes(`/collections/${row.id}/restore`) && r.request().method() === 'POST',
    );
    const refetched = page.waitForResponse(
      (r) =>
        r.request().method() === 'GET' &&
        new URL(r.url()).pathname === `/api/v1/collections/${row.id}`,
    );
    await page.getByTestId('collection-detail-restore-button').dispatchEvent('click');
    expect((await restored).status(), 'the restore must succeed for this test to mean anything').toBe(204);
    await refetched;
    await page.waitForTimeout(500);

    const afterRestore = (await (
      await request.get(`/api/v1/collections/${row.id}`)
    ).json()) as CollectionRow;
    expect(
      afterRestore.updated_at,
      'the restore did not advance updated_at, so there is no concurrent edit to detect',
    ).not.toBe(baseline);

    // ── the form half ────────────────────────────────────────────────
    await expect(
      tier(dialog, 'org-only'),
      'the refetch put the stored tier back — the seed re-ran on a prop change (#1262)',
    ).toBeChecked();
    await expect(nameField, 'the refetch also reverted the unsaved name').toHaveValue(editedName);

    // ── the concurrency half ─────────────────────────────────────────
    //
    // The row moved under the dialog, so this save MUST 409. It does that
    // only if `if_unchanged_since` is still the timestamp captured on
    // open; a seed that re-ran would have replaced it with the value the
    // refetch carried — the very write it is supposed to detect — and the
    // save would have gone through and silently won.
    await page.getByRole('button', { name: /^save$/i }).click();
    await expect(
      page.getByTestId('collection-edit-conflict'),
      'the save did not conflict, so the dialog adopted the concurrent write as its own ' +
        `baseline (#1262). What it sent: ${JSON.stringify(patches)}`,
    ).toBeVisible();
    expect(
      patches.map((p) => (JSON.parse(p) as { if_unchanged_since?: string }).if_unchanged_since),
      'the PATCH carried a baseline the curator never saw',
    ).toEqual([baseline]);
    await expect(dialog, 'a conflicting save must not close the dialog').toBeVisible();

    // ── the retry, which a naive fix breaks ──────────────────────────
    //
    // Acknowledging the conflict re-seeds the baseline from the server's
    // answer — deliberately, and this is the ONE re-seed that has to
    // survive #1262: "you have seen the conflict, here is the new
    // baseline, try again."
    await page.getByTestId('collection-edit-conflict-ack').click();
    await expect(
      tier(dialog, 'org-only'),
      'acknowledging the conflict discarded the edits it exists to preserve',
    ).toBeChecked();
    await page.getByRole('button', { name: /^save$/i }).click();
    await expect(dialog, 'the retry after acknowledging the conflict did not save').toBeHidden();

    const stored = (await (await request.get(`/api/v1/collections/${row.id}`)).json()) as CollectionRow;
    expect(stored.visibility, 'the retried save did not store the widened tier').toBe('org-only');
    expect(stored.name, 'the retried save did not store the edited name').toBe(editedName);
  });
});
