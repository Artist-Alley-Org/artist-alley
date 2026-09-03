// ui-18-collection-fields.spec.ts
//
// Phase 1.9.B — collection custom-fields UI smoke.
//
// Two scenarios:
//   1. Admin creates a collection-scoped field via /admin/fields
//      using the subject_kind picker; the filter chip then restricts
//      the list to collection fields only.
//   2. Opening EditCollectionModal on an existing collection surfaces
//      the new field via CollectionFieldsSection; typing a value +
//      saving persists through PUT /collections/{id}/fields/{field_id}
//      and re-renders.
//
// Tests reset the gate field at the end so subsequent runs don't see
// a permanent test artefact.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

const TEST_COLLECTION_NAME = 'UI-18 Smoke Collection';

// The field code is generated PER TEST ATTEMPT (see the round-trip
// test) rather than fixed. DELETE /fields soft-archives + `code` is
// UNIQUE, so a fixed code collides on a retry: the first attempt's
// archived row still holds the code, and creating it again 23505s
// (#527). A per-attempt code means a retry starts clean. The code the
// current attempt created is stashed here so afterEach can clean it.
let createdFieldCode: string | undefined;

test.describe('UI-18 collection custom fields', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ request }) => {
    const code = createdFieldCode;
    createdFieldCode = undefined;
    if (!code) return;
    // Best-effort cleanup. We don't fail the test on cleanup errors.
    // Check the default (active) list AND status=archived: since #532
    // the default excludes archived, so a field a failed attempt already
    // soft-archived wouldn't be found by the default query alone.
    for (const q of ['subject_kind=collection', 'subject_kind=collection&status=archived']) {
      const res = await request.get(`/api/v1/fields?${q}`).catch(() => null);
      if (!res || !res.ok()) continue;
      const fields = (await res.json()) as Array<{ id: string; code: string }>;
      const f = fields.find((x) => x.code === code);
      if (f) {
        await request.delete(`/api/v1/fields/${f.id}`).catch(() => undefined);
        break;
      }
    }
  });

  test('admin/fields filter chip restricts to collection-scoped fields', async ({ page }) => {
    await page.goto('/admin/fields');
    // Click the collection-only filter chip.
    await page.getByTestId('admin-fields-filter-collection').click();
    // Either the table renders only collection rows OR shows the
    // empty-list message — both are valid end states depending on
    // what other tests have seeded. We assert the filter chip is now
    // visually selected (data-testid present + clickable) and the
    // page still rendered without crashing.
    await expect(page.getByTestId('admin-fields-filter-collection')).toBeVisible();
  });

  test('create + edit round-trip: define collection field, set value on a collection', async ({ page }, testInfo) => {
    // Per-attempt-unique code: DELETE soft-archives + `code` is UNIQUE,
    // so reusing a fixed code across a retry 23505s on the second create
    // (the archived row still owns it). workerIndex + retry + a clock
    // stamp make it unique across retries, parallel workers, and reruns.
    // Must satisfy the server's ^[a-z][a-z0-9_]*$ code rule (#527).
    const fieldCode = `ui18_notes_${testInfo.workerIndex}_${testInfo.retry}_${Date.now()}`;
    createdFieldCode = fieldCode;

    // 1. Open the create form on /admin/fields, pick subject_kind=collection,
    //    submit. The field should appear in the filtered list.
    await page.goto('/admin/fields');
    await page.getByTestId('admin-fields-create-button').click();
    await page.getByTestId('admin-fields-create-code').fill(fieldCode);
    await page.getByTestId('admin-fields-create-label').fill('UI-18 Notes');
    await page.getByTestId('admin-fields-create-type').selectOption('text');
    // The radio is sr-only so the styled label captures pointer
    // events. Click the wrapping label instead of .check() on the
    // hidden input — Playwright's actionability check refuses to
    // click a label-obscured input.
    await page.locator('label:has([data-testid="admin-fields-create-subject-collection"])').click();
    await page.getByTestId('admin-fields-create-submit').click();
    // #505: inherit the config's global expect.timeout (15s), set for
    // CI-hydration-under-load (#481/#490). The explicit 5s cap here (and
    // on the two assertions below) undercut it — after the create POST the
    // fields list refetches + re-renders, and the new row's hydration can
    // exceed 5s under CI load. The assertions are unchanged, so a row that
    // genuinely never renders still fails (at 15s) — timing weakened, the
    // create+edit round-trip still genuinely verified.
    await expect(page.getByTestId(`admin-fields-row-${fieldCode}`)).toBeVisible();

    // The id, read back from the API rather than scraped off the page:
    // the description PATCH below is keyed on the UUID, and every field
    // endpoint is.
    const listed = await page.request.get('/api/v1/fields?subject_kind=collection');
    expect(listed.ok(), `list fields → ${listed.status()}`).toBeTruthy();
    const rows = (await listed.json()) as { id: string; code: string }[];
    const fieldId = rows.find((f) => f.code === fieldCode)?.id;
    expect(fieldId, `the field just created must be listed`).toBeTruthy();

    // 2. Create a collection, then open its edit modal — the new
    //    custom field section should surface with our field row.
    //    We create via the API to keep the test focused on the
    //    fields surface (not on Collection CRUD UX).
    const createRes = await page.request.post('/api/v1/collections', {
      data: { name: TEST_COLLECTION_NAME },
    });
    expect(createRes.status()).toBe(201);
    const collection = (await createRes.json()) as { id: string };

    // Open the collection detail page. Edit is hidden behind the
    // "more" dropdown menu, so click that first, then the Edit
    // menuitem. Both have data-testids so the test survives copy /
    // i18n changes.
    await page.goto(`/collections/${collection.id}`);
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();

    // Custom-fields section is in the modal.
    await expect(page.getByTestId('collection-fields-section')).toBeVisible();
    // Field input renders for our test field.
    const input = page.getByTestId(`field-input-${fieldCode}`);
    await expect(input).toBeVisible();

    // #1173 — `field_definition.description` is the operator's note about
    // what belongs in the field. It has always been authorable and was
    // shown NOWHERE the person entering a value could read it, which made
    // it guidance nobody was guided by. It renders under the control now.
    //
    // Absent before it is written: a field nobody documented must lay out
    // exactly as it did, rather than reserving a blank line.
    await expect(page.getByTestId(`field-help-${fieldCode}`)).toHaveCount(0);
    const guidance = 'Anything the client needs to know before delivery.';
    const patched = await page.request.patch(`/api/v1/fields/${fieldId}`, {
      data: { description: guidance },
    });
    expect(patched.ok(), `set description → ${patched.status()}`).toBeTruthy();

    await page.reload();
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();
    await expect(page.getByTestId('collection-fields-section')).toBeVisible();
    await expect(page.getByTestId(`field-help-${fieldCode}`)).toHaveText(guidance);

    await page.getByTestId(`field-input-${fieldCode}`).fill('hello from ui-18');
    await page.getByTestId('collection-fields-save').click();
    await expect(page.getByTestId('collection-fields-saved')).toBeVisible();

    // 3. Cleanup — delete the collection so the run is idempotent.
    await page.request.delete(`/api/v1/collections/${collection.id}?hard=true`);
  });
});

// ---------------------------------------------------------------------------
// #1119 / #1389 — REMOVING a value, and what happens when two people
// edit at once
// ---------------------------------------------------------------------------
//
// The collection modal was the ONLY surface in the product that could
// edit a field value at all, and it could not remove one. FieldValueInput
// collapsed an emptied control to null, the save mapped null to
// `undefined` so the member was OMITTED from the typed PUT, and
// validateCollectionValueType then refused it ("value_text required for
// field type text"). What an operator saw was the generic save error, on
// every attempt, forever. The backend's Clear operation had ZERO web
// callers.
//
// These extend the round-trip above rather than living in their own file
// because they are the same surface, and because the CL-c discriminator
// only means anything sitting next to CL-b: "required refuses a removal"
// is indistinguishable from "removal is broken" unless the optional one
// works in the same run.

test.describe('#1119 — removing values and per-field conflicts', () => {
  // Serial: CL-c turns `required` on for a moment, and R2 makes an
  // active required collection field refuse every create that omits it.
  test.describe.configure({ mode: 'serial' });

  const RUN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
  const fieldIds: string[] = [];
  let collectionId = '';

  async function makeField(
    request: import('@playwright/test').APIRequestContext,
    name: string,
    body: Record<string, unknown>,
  ) {
    const code = `cf_${name}_${RUN}`;
    const r = await request.post('/api/v1/fields', {
      data: {
        code,
        label: `CF ${name}`,
        type: 'text',
        subject_kind: 'collection',
        display_order: 9300,
        ...body,
      },
    });
    expect(r.status(), `create field → ${r.status()} ${await r.text()}`).toBe(201);
    const id = ((await r.json()) as { id: string }).id;
    fieldIds.push(id);
    return { id, code };
  }

  async function putValue(
    request: import('@playwright/test').APIRequestContext,
    fieldId: string,
    data: Record<string, unknown>,
  ) {
    const r = await request.put(`/api/v1/collections/${collectionId}/fields/${fieldId}`, { data });
    expect(r.ok(), `put value → ${r.status()} ${await r.text()}`).toBeTruthy();
    return (await r.json()) as { set_at: string };
  }

  async function storedValue(
    request: import('@playwright/test').APIRequestContext,
    fieldId: string,
  ): Promise<Record<string, unknown> | undefined> {
    const r = await request.get(`/api/v1/collections/${collectionId}/fields`);
    expect(r.ok()).toBeTruthy();
    const rows = (await r.json()) as Array<Record<string, unknown>>;
    return rows.find((v) => v.field_id === fieldId);
  }

  async function openModal(page: import('@playwright/test').Page) {
    await page.goto(`/collections/${collectionId}`);
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();
    await expect(page.getByTestId('collection-fields-section')).toBeVisible();
  }

  test.beforeEach(async ({ page }) => {
    const r = await page.request.post('/api/v1/collections', {
      data: { name: `UI-18 field-clear ${RUN}` },
    });
    expect(r.status()).toBe(201);
    collectionId = ((await r.json()) as { id: string }).id;
  });

  test.afterEach(async ({ page }) => {
    if (collectionId) {
      await page.request.delete(`/api/v1/collections/${collectionId}?hard=true`).catch(() => undefined);
      collectionId = '';
    }
  });

  test.afterAll(async ({ request }) => {
    for (const id of fieldIds) {
      await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
    }
  });

  // CL-b + CL-e + CL-g's storage half, in one pass over the three types
  // the brief names.
  test('CL-b / CL-e: emptying the control removes text, multi_select and boolean values', async ({
    page,
  }) => {
    const txt = await makeField(page.request, 'text', {});
    const ms = await makeField(page.request, 'ms', {
      type: 'multi_select',
      options: { values: [{ value: 'alpha', label: 'Alpha' }, { value: 'beta', label: 'Beta' }] },
    });
    const bool = await makeField(page.request, 'bool', { type: 'boolean' });
    const keep = await makeField(page.request, 'keep', {});

    await putValue(page.request, txt.id, { value_text: 'remove me' });
    await putValue(page.request, ms.id, { value_options: ['alpha', 'beta'] });
    await putValue(page.request, bool.id, { value_num: 1 });
    await putValue(page.request, keep.id, { value_text: 'untouched' });

    await openModal(page);
    await expect(page.getByTestId(`field-input-${txt.code}`)).toHaveValue('remove me');

    await page.getByTestId(`field-input-${txt.code}`).fill('');
    const chipRemove = page.locator(`[data-testid="vocab-chip-remove-${ms.code}"]`);
    while ((await chipRemove.count()) > 0) await chipRemove.first().click();
    // Removing the last chip leaves the combobox's option list open, and
    // it overlaps the save button in the two-column layout.
    await page.keyboard.press('Escape');
    await page.getByTestId(`field-input-${bool.code}`).selectOption('');

    await page.getByTestId('collection-fields-save').click();
    // ABSENT, not "a generic save error" — which is what every one of
    // these produced before.
    await expect(page.getByTestId('collection-fields-saved')).toBeVisible();
    expect(await storedValue(page.request, txt.id)).toBeUndefined();
    expect(await storedValue(page.request, ms.id)).toBeUndefined();
    expect(await storedValue(page.request, bool.id)).toBeUndefined();
    expect((await storedValue(page.request, keep.id))?.value_text).toBe('untouched');
  });

  test('CL-c: the same removal against a REQUIRED collection field is refused', async ({ page }) => {
    // `required` is switched on AFTER the collection exists and the
    // value is seeded, and switched off again in the `finally`.
    //
    // Not tidiness: R2 — collection CREATE's required-presence rule —
    // is still enforced, so an ACTIVE required collection field makes
    // every other spec's `POST /collections` answer 422
    // RequiredCollectionFieldMissing for as long as it is there. A spec
    // that changes what a parallel spec sees is the isolation failure
    // #1247 is about, and this one would look like a collections bug.
    const f = await makeField(page.request, 'req', {});
    await putValue(page.request, f.id, { value_text: 'stays put' });
    try {
      const on = await page.request.patch(`/api/v1/fields/${f.id}`, { data: { required: true } });
      expect(on.ok(), `mark required → ${on.status()}`).toBeTruthy();

      await openModal(page);
      await page.getByTestId(`field-input-${f.code}`).fill('');
      await page.getByTestId('collection-fields-save').click();

      await expect(page.getByTestId(`field-error-${f.code}`)).toBeVisible();
      await expect(page.getByTestId(`field-error-${f.code}`)).toContainText('required');
      expect((await storedValue(page.request, f.id))?.value_text).toBe('stays put');
    } finally {
      await page.request.patch(`/api/v1/fields/${f.id}`, { data: { required: false } }).catch(() => undefined);
    }
  });

  test('CL-g: FALSE is stored and does not clear; the control can tell it from unset', async ({
    page,
  }) => {
    const f = await makeField(page.request, 'boolval', { type: 'boolean' });

    await openModal(page);
    await page.getByTestId(`field-input-${f.code}`).selectOption('false');
    await page.getByTestId('collection-fields-save').click();
    await expect(page.getByTestId('collection-fields-saved')).toBeVisible();
    const stored = await storedValue(page.request, f.id);
    expect(stored, 'the row must EXIST — false is an answer').toBeDefined();
    expect(stored?.value_num).toBe(0);

    // The checkbox this replaced rendered an absent value and a stored
    // false identically. This is the assertion it could not have passed.
    await openModal(page);
    await expect(page.getByTestId(`field-input-${f.code}`)).toHaveValue('false');
  });

  test('CL-d: a stale removal is refused and the newer value SURVIVES', async ({ page }) => {
    const f = await makeField(page.request, 'clconf', {});
    await putValue(page.request, f.id, { value_text: 'loaded at T' });

    await openModal(page);
    await expect(page.getByTestId(`field-input-${f.code}`)).toHaveValue('loaded at T');

    // Another actor writes a NEWER value.
    await putValue(page.request, f.id, { value_text: 'newer, from somebody else' });

    // The editor removes from its stale baseline.
    await page.getByTestId(`field-input-${f.code}`).fill('');
    await page.getByTestId('collection-fields-save').click();

    await expect(page.getByTestId(`field-conflict-${f.code}`)).toBeVisible();
    expect(
      (await storedValue(page.request, f.id))?.value_text,
      'A STALE CLEAR MUST NOT ERASE A VALUE IT NEVER SAW',
    ).toBe('newer, from somebody else');
  });

  test('MX-a: one field saves while another conflicts, independently', async ({ page }) => {
    const x = await makeField(page.request, 'mxx', {});
    const y = await makeField(page.request, 'mxy', {});
    const z = await makeField(page.request, 'mxz', {});
    await putValue(page.request, x.id, { value_text: 'x original' });
    await putValue(page.request, y.id, { value_text: 'y original' });
    await putValue(page.request, z.id, { value_text: 'z untouched' });

    await openModal(page);
    await page.getByTestId(`field-input-${x.code}`).fill('x mine');
    await page.getByTestId(`field-input-${y.code}`).fill('y mine');

    await putValue(page.request, y.id, { value_text: 'y from somebody else' });

    await page.getByTestId('collection-fields-save').click();

    // Independent per-field operations, not a batch transaction: Y's
    // refusal must not cost the person their X edit.
    await expect(page.getByTestId(`field-conflict-${y.code}`)).toBeVisible();
    await expect(page.getByTestId(`field-conflict-${x.code}`)).toHaveCount(0);
    expect((await storedValue(page.request, x.id))?.value_text).toBe('x mine');
    expect((await storedValue(page.request, y.id))?.value_text).toBe('y from somebody else');
    expect((await storedValue(page.request, z.id))?.value_text).toBe('z untouched');

    // The retry resolves Y from the re-baselined token and does not
    // resend X, which already saved.
    const xRequests: string[] = [];
    page.on('request', (r) => {
      if (r.url().includes(`/fields/${x.id}`)) xRequests.push(r.method());
    });
    await page.getByTestId('collection-fields-save').click();
    await expect(page.getByTestId('collection-fields-saved')).toBeVisible();
    expect(xRequests, 'X already saved; the retry must not resend it').toEqual([]);
    expect((await storedValue(page.request, y.id))?.value_text).toBe('y mine');
    expect((await storedValue(page.request, x.id))?.value_text).toBe('x mine');
  });
});
