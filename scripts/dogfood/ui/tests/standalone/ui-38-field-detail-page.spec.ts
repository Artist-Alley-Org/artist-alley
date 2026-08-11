// ui-38-field-detail-page.spec.ts
//
// #854 — /admin/fields was a nine-column table that expanded the whole
// field form inside its own cells. Editing moved to
// /admin/fields/{code}; the table went back to being an index.
//
// A relocation is not a behaviour change, so most of what is worth
// testing here is what must NOT have changed. Four things:
//
//   1. The route key. `code` is what the URL carries, and it is only
//      safe as one because it is UNIQUE (field_definition_code_key),
//      URL-safe (server-validated ^[a-z][a-z0-9_]*$) and IMMUTABLE
//      (absent from FieldDefinitionUpdate). Deep-linking is the point
//      of the route, so it is driven as a cold navigation.
//   2. The 409 conflict flow. Relocating a conflict-guarded form is
//      exactly how a guard turns into a silent overwrite — the
//      baseline gets re-read from a prop, or a remount resets it. The
//      assertion is on the PERSISTED value, because a body assertion
//      passes on the bug when the handler echoes its own write.
//   3. show_on_card round-trips from the new page, all the way to a
//      real card. The flag ships nothing on its own; the card is where
//      it means something.
//   4. mirrors_column is legible and not editable. A mirrored field is
//      a view onto an asset column (#822) — the whole reason it gets a
//      callout is that an operator otherwise has no way to know why
//      the field behaves differently.
//
// Fields are created per attempt and archived in afterEach: DELETE is
// a soft archive and `code` is UNIQUE, so a fixed code would collide
// with its own tombstone on the second run.

import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

interface FieldDef {
  id: string;
  code: string;
  label: string;
  updated_at: string;
  show_on_card?: boolean;
  mirrors_column?: string | null;
  display_group?: string;
  display_order?: number;
  applies_to?: number[];
  description?: string;
}

let createdFieldIds: string[] = [];

function fieldCode(prefix: string, testInfo: { workerIndex: number; retry: number }): string {
  return `ui38_${prefix}_${testInfo.workerIndex}_${testInfo.retry}_${Date.now()}`;
}

async function createField(page: Page, body: Record<string, unknown>): Promise<FieldDef> {
  const res = await page.request.post('/api/v1/fields', { data: body });
  expect(res.status(), await res.text()).toBe(201);
  const f = (await res.json()) as FieldDef;
  createdFieldIds.push(f.id);
  return f;
}

/** The stored row, straight from the API — never the form's own echo. */
async function readField(page: Page, id: string): Promise<FieldDef> {
  const res = await page.request.get(`/api/v1/fields/${id}`);
  expect(res.ok(), await res.text()).toBe(true);
  return (await res.json()) as FieldDef;
}

test.describe('UI-38 the per-field page', () => {
  test.beforeEach(async ({ page }) => {
    createdFieldIds = [];
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ request }) => {
    for (const id of createdFieldIds) {
      await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
    }
    createdFieldIds = [];
  });

  // ── 1. The index is an index, and the row is a link ───────────────
  test('the index links to the field page instead of expanding it', async ({ page }, testInfo) => {
    const f = await createField(page, {
      code: fieldCode('index', testInfo),
      label: 'UI-38 Index',
      type: 'text',
      subject_kind: 'asset',
    });

    await page.goto('/admin/fields');
    const row = page.getByTestId(`admin-fields-row-${f.code}`);
    await expect(row).toBeVisible();

    // Fewer than the nine the issue counted, and the form is nowhere
    // on this page any more.
    const columns = await page.locator('table thead th').count();
    expect(columns).toBeLessThan(9);
    expect(columns).toBe(5);
    await expect(page.getByTestId('field-editor')).toHaveCount(0);

    await page.screenshot({ path: testInfo.outputPath('fields-index-after.png'), fullPage: true });

    await row.getByTestId(`admin-fields-open-${f.code}`).click();
    await expect(page).toHaveURL(new RegExp(`/admin/fields/${f.code}$`));
    await expect(page.getByTestId('field-editor')).toBeVisible();
    await expect(page.getByTestId('field-detail-code')).toHaveText(f.code);

    // …and back again.
    await page.getByTestId('field-detail-back').click();
    await expect(page).toHaveURL(/\/admin\/fields$/);
  });

  // ── 2. Deep link, cold ───────────────────────────────────────────
  test('opens straight from a pasted URL', async ({ page }, testInfo) => {
    const f = await createField(page, {
      code: fieldCode('deep', testInfo),
      label: 'UI-38 Deep Link',
      type: 'select',
      subject_kind: 'asset',
      options: { values: [{ value: 'alpha', label: 'Alpha' }] },
    });

    // No index in between: this is the tab somebody pasted a link into.
    await page.goto(`/admin/fields/${f.code}`);
    await expect(page.getByTestId('field-detail-label')).toHaveText('UI-38 Deep Link');
    await expect(page.getByTestId('field-editor')).toBeVisible();
    await expect(page.getByTestId('field-option-row-alpha')).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath('field-page-1080.png'), fullPage: true });

    // A code that names nothing says so — it does not hang on a
    // spinner or render an empty form bound to nothing.
    await page.goto('/admin/fields/ui38_no_such_field_at_all');
    await expect(page.getByTestId('field-detail-not-found')).toBeVisible();
    await expect(page.getByTestId('field-editor')).toHaveCount(0);
  });

  // ── 3. THE PIN: the conflict guard survived the move ─────────────
  test('a stale save conflicts rather than silently overwriting', async ({ page }, testInfo) => {
    const f = await createField(page, {
      code: fieldCode('conflict', testInfo),
      label: 'UI-38 Conflict',
      type: 'text',
      subject_kind: 'asset',
    });

    await page.goto(`/admin/fields/${f.code}`);
    await expect(page.getByTestId('field-editor')).toBeVisible();

    // Somebody else edits the row while this page sits open. A second
    // admin, an open-vocabulary mint and a script are all the same
    // thing from here.
    const other = await page.request.patch(`/api/v1/fields/${f.id}`, {
      data: { if_unchanged_since: f.updated_at, label: 'Edited Elsewhere' },
    });
    expect(other.status(), await other.text()).toBe(200);

    await page.getByTestId('field-edit-label').fill('Mine');
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-conflict')).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath('field-page-conflict.png'), fullPage: true });

    // ASSERT THE PERSISTED VALUE. The response body would have echoed
    // whatever the handler wrote, so it cannot tell a refused save
    // from an accepted one.
    expect((await readField(page, f.id)).label).toBe('Edited Elsewhere');

    // The operator's edit is still in the form, and the deliberate
    // overwrite goes through on the re-based timestamp.
    await expect(page.getByTestId('field-edit-label')).toHaveValue('Mine');
    await page.getByTestId('field-options-conflict-overwrite').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();
    expect((await readField(page, f.id)).label).toBe('Mine');

    // And the alternative branch — adopt theirs — is still wired.
    const third = await page.request.patch(`/api/v1/fields/${f.id}`, {
      data: { label: 'Theirs Again' },
    });
    expect(third.status(), await third.text()).toBe(200);
    await page.getByTestId('field-edit-label').fill('Mine Again');
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-conflict')).toBeVisible();
    await page.getByTestId('field-options-conflict-reload').click();
    await expect(page.getByTestId('field-edit-label')).toHaveValue('Theirs Again');
    expect((await readField(page, f.id)).label).toBe('Theirs Again');
  });

  // ── 4. show_on_card round-trips, and a card shows it ─────────────
  test('show_on_card sticks and reaches a real card', async ({ page }, testInfo) => {
    const f = await createField(page, {
      code: fieldCode('card', testInfo),
      label: 'UI-38 Card Field',
      type: 'text',
      subject_kind: 'asset',
    });

    await page.goto(`/admin/fields/${f.code}`);
    const toggle = page.getByTestId('field-edit-show-on-card');
    await expect(toggle).not.toBeChecked();
    await toggle.check();
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();

    // Persisted, not echoed.
    expect((await readField(page, f.id)).show_on_card).toBe(true);

    // Still on after a hard reload of the page that set it.
    await page.reload();
    await expect(page.getByTestId('field-edit-show-on-card')).toBeChecked();

    // The index says so too, without being opened.
    await page.goto('/admin/fields');
    await expect(page.getByTestId(`admin-fields-card-badge-${f.code}`)).toBeVisible();

    // And a card renders it. The flag is a display hint and does
    // nothing on its own; the card is where it means something, so
    // this is driven all the way to a rendered tile rather than
    // stopping at the column.
    const assets = await page.request.get('/api/v1/assets?limit=24');
    expect(assets.ok(), await assets.text()).toBe(true);
    const list = (await assets.json()) as {
      items?: Array<{ id: string; owner_user_ref?: number | null; restricted?: boolean }>;
    };
    const owned = (list.items ?? []).find((a) => !a.restricted && a.owner_user_ref);
    expect(owned, 'the seeded stack has an owned, readable asset').toBeTruthy();
    const ownerRef = owned!.owner_user_ref!;

    // Take the asset from the OWNER's own page rather than the global
    // one, so the tile is guaranteed to be in the 24 the profile grid
    // renders — the global newest is not necessarily this owner's.
    const ownerFirst = await page.request.get(`/api/v1/assets?owner_ref=${ownerRef}&limit=24`);
    const ownerRows = (await ownerFirst.json()) as { items?: Array<{ id: string }> };
    const assetId = (ownerRows.items ?? [])[0]?.id;
    expect(assetId, 'the owner has at least one asset').toBeTruthy();

    const put = await page.request.put(`/api/v1/assets/${assetId}/fields/${f.id}`, {
      data: { value_text: 'on the card' },
    });
    expect(put.ok(), await put.text()).toBe(true);

    // The server side of the hint: the list endpoint resolves the flag
    // into a display string on the row itself (#552's decorateCards).
    const decorated = await page.request.get(`/api/v1/assets?owner_ref=${ownerRef}&limit=24`);
    const rows = (await decorated.json()) as {
      items?: Array<{ id: string; card_fields?: Array<{ code: string; value: string }> | null }>;
    };
    const row = (rows.items ?? []).find((a) => a.id === assetId);
    expect(row?.card_fields?.find((c) => c.code === f.code)?.value).toBe('on the card');

    // The browser side: the owner's profile grid is the shipped
    // consumer of that decoration. The at-a-glance strip lives in the
    // details footer, which AssetCard renders in `thumbnail` mode —
    // a stored browse preference, set here the same way the view
    // control sets it, so this drives the real card and not a
    // hand-built one.
    await page.addInitScript(() => {
      window.localStorage.setItem('aa_browse_mode', 'thumbnail');
    });
    await page.goto(`/users/by-ref/${ownerRef}`);
    const cardValue = page.getByTestId(`card-field-${f.code}`).first();
    await expect(cardValue).toHaveText('on the card', { timeout: 15_000 });
    await page.screenshot({ path: testInfo.outputPath('card-field.png'), fullPage: true });
  });

  // ── 5. A mirrored field explains itself and cannot be retargeted ──
  test('mirrors_column is shown and refused in both directions', async ({ page }, testInfo) => {
    // `title` ships mirrored (migration 00044). Not created here —
    // a mirror cannot be declared through the API at all, which is
    // half of what this test is about.
    await page.goto('/admin/fields/title');
    await expect(page.getByTestId('field-detail-mirror')).toBeVisible();
    await expect(page.getByTestId('field-detail-mirrors-column')).toHaveText('title');
    await expect(page.getByTestId('field-detail-mirror-badge')).toBeVisible();
    await expect(page.getByTestId('field-detail-mirror-readonly')).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath('field-page-mirrored.png'), fullPage: true });

    // THROUGH THE UI: there is no control. Not a disabled one — a
    // disabled input advertises a setting that does not exist.
    const controls = page
      .locator('input, select, textarea')
      .filter({ has: page.locator(':scope') });
    const named = await controls.evaluateAll((els) =>
      els.map((e) => `${e.getAttribute('name') ?? ''}|${e.getAttribute('data-testid') ?? ''}`),
    );
    expect(named.some((n) => n.toLowerCase().includes('mirror'))).toBe(false);

    // THROUGH THE API: the property is readOnly, so a PATCH naming it
    // changes nothing. Asserted on the STORED row.
    const before = await page.request.get('/api/v1/fields');
    const defs = (await before.json()) as FieldDef[];
    const titleDef = defs.find((d) => d.code === 'title');
    expect(titleDef, 'the shipped `title` field definition').toBeTruthy();
    const id = titleDef!.id;

    const retarget = await page.request.patch(`/api/v1/fields/${id}`, {
      data: { mirrors_column: 'description' },
    });
    // Accepted or refused, the invariant is the same: the column did
    // not move. "Accepted but inert" is the failure mode a status-code
    // assertion alone would sail past.
    expect((await readField(page, id)).mirrors_column).toBe('title');

    const clear = await page.request.patch(`/api/v1/fields/${id}`, {
      data: { mirrors_column: null },
    });
    expect((await readField(page, id)).mirrors_column).toBe('title');

    // Recorded for the handoff rather than asserted: what the server
    // answers to an unknown property is its business, the stored value
    // is the contract.
    expect([200, 400]).toContain(retarget.status());
    expect([200, 400]).toContain(clear.status());

    // A field that ships mirrored also cannot be UN-mirrored by
    // declaring a mirror on a fresh field — there is no way to say it.
    const attempt = await page.request.post('/api/v1/fields', {
      data: {
        code: fieldCode('mirror', testInfo),
        label: 'UI-38 Mirror Attempt',
        type: 'text',
        subject_kind: 'asset',
        mirrors_column: 'title',
      },
    });
    if (attempt.status() === 201) {
      const made = (await attempt.json()) as FieldDef;
      createdFieldIds.push(made.id);
      expect(made.mirrors_column ?? null).toBeNull();
    }
  });

  // ── 6. The long tail lives behind Advanced, and it round-trips ───
  test('the Advanced section saves the long tail', async ({ page }, testInfo) => {
    const f = await createField(page, {
      code: fieldCode('adv', testInfo),
      label: 'UI-38 Advanced',
      type: 'text',
      subject_kind: 'asset',
    });

    await page.goto(`/admin/fields/${f.code}`);

    // Collapsed by default — that is the progressive disclosure, and
    // it is why the everyday controls are reachable without scrolling
    // past scoping.
    await expect(page.getByTestId('field-edit-display-group')).toBeHidden();
    await page.getByTestId('field-edit-advanced-toggle').click();
    await expect(page.getByTestId('field-edit-display-group')).toBeVisible();

    await page.getByTestId('field-edit-description').fill('What belongs in this field.');
    await page.getByTestId('field-edit-display-group').fill('ui38group');
    await page.getByTestId('field-edit-display-order').fill('42');
    // Scope it to one asset type; ref 1 exists on every seeded stack.
    await page.getByTestId('field-edit-applies-to-1').check();
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();

    const stored = await readField(page, f.id);
    expect(stored.description).toBe('What belongs in this field.');
    expect(stored.display_group).toBe('ui38group');
    expect(stored.display_order).toBe(42);
    expect(stored.applies_to).toEqual([1]);

    // Capabilities are shown, never edited (the read cap is what makes
    // show_on_card unavailable, and this page does not get to change
    // an access rule).
    await expect(page.getByTestId('field-edit-read-capability')).toBeVisible();
    await expect(page.getByTestId('field-edit-capabilities').locator('input')).toHaveCount(0);

    // Un-ticking every type is "all types", not "no types" — the
    // empty array has to reach the server as an empty array, which is
    // the one place a nil/empty mix-up would silently no-op.
    await page.reload();
    await page.getByTestId('field-edit-advanced-toggle').click();
    await page.getByTestId('field-edit-applies-to-1').uncheck();
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();
    expect((await readField(page, f.id)).applies_to).toEqual([]);
  });

  // ── 7. Archive, not delete ───────────────────────────────────────
  test('archiving a field keeps the values assets already hold', async ({ page }, testInfo) => {
    const f = await createField(page, {
      code: fieldCode('archive', testInfo),
      label: 'UI-38 Archive',
      type: 'text',
      subject_kind: 'asset',
    });

    const assets = await page.request.get('/api/v1/assets?limit=1');
    const list = (await assets.json()) as { items?: Array<{ id: string }> } | Array<{ id: string }>;
    const items = Array.isArray(list) ? list : (list.items ?? []);
    const assetId = items[0].id;
    const put = await page.request.put(`/api/v1/assets/${assetId}/fields/${f.id}`, {
      data: { value_text: 'survives the archive' },
    });
    expect(put.ok(), await put.text()).toBe(true);

    const del = await page.request.delete(`/api/v1/fields/${f.id}`);
    expect(del.status()).toBe(204);

    // Soft: the row is still there, tombstoned rather than destroyed.
    const after = await readField(page, f.id);
    expect((after as unknown as { status: string }).status).toBe('archived');

    // Out of the live schema…
    const live = await page.request.get('/api/v1/fields');
    expect(((await live.json()) as FieldDef[]).some((d) => d.id === f.id)).toBe(false);

    // …and the asset's value is untouched.
    const values = await page.request.get(`/api/v1/assets/${assetId}/fields`);
    const rows = (await values.json()) as Array<{ field_id: string; value_text?: string }>;
    expect(rows.find((r) => r.field_id === f.id)?.value_text).toBe('survives the archive');

    // A bookmark to an archived field still opens and says what it is,
    // rather than pretending the field never existed.
    await page.goto(`/admin/fields/${f.code}`);
    await expect(page.getByTestId('field-detail-status')).toBeVisible();
    await expect(page.getByTestId('field-detail-status-fact')).toHaveText('archived');
  });
});

// ── 8. The same page with a thumb ──────────────────────────────────
//
// The admin surface is used at both widths. The old table needed a
// horizontal scroll to reach the editor's Save button at 390px; the
// page it moved to has no excuse for needing one.
test.describe('UI-38 the per-field page at 390px', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true });

  test.beforeEach(async ({ page }) => {
    createdFieldIds = [];
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ request }) => {
    for (const id of createdFieldIds) {
      await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
    }
    createdFieldIds = [];
  });

  test('edits a field by tapping, with no horizontal scroll', async ({ page }, testInfo) => {
    const f = await createField(page, {
      code: fieldCode('mobile', testInfo),
      label: 'UI-38 Mobile',
      type: 'text',
      subject_kind: 'asset',
    });

    await page.goto('/admin/fields');
    await page.screenshot({ path: testInfo.outputPath('fields-index-390.png'), fullPage: true });

    // The index REDUCES at this width rather than shrinking: `code`
    // folds under the label and `subject` is dropped, so nothing ends
    // up off-screen behind a sideways scroll nobody discovers.
    await expect(page.getByTestId(`admin-fields-open-${f.code}`)).toBeVisible();
    const indexOverflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(indexOverflow).toBeLessThanOrEqual(1);

    await page.getByTestId(`admin-fields-open-${f.code}`).tap();
    await expect(page.getByTestId('field-editor')).toBeVisible();

    await page.getByTestId('field-edit-label').fill('UI-38 Mobile Edited');
    await page.getByTestId('field-edit-advanced-toggle').tap();
    await page.getByTestId('field-edit-display-group').fill('thumbs');
    await page.screenshot({ path: testInfo.outputPath('field-page-390.png'), fullPage: true });

    // Tapped, not clicked, and reachable without a sideways scroll.
    const save = page.getByTestId('field-options-save');
    const box = await save.boundingBox();
    expect(box, 'the save button has a box').toBeTruthy();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
    await save.tap();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();

    const stored = await readField(page, f.id);
    expect(stored.label).toBe('UI-38 Mobile Edited');
    expect(stored.display_group).toBe('thumbs');

    // The page itself does not scroll sideways.
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(1);
  });
});
