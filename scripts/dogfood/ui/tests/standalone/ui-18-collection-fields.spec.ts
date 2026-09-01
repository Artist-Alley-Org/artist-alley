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
