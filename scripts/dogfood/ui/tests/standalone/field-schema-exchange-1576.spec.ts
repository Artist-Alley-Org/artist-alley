// #1576 S4: the field schema exchange and its import history, in a
// real browser against the real backend.
//
// What this file exists to catch, in the order it would hurt:
//
//  1. THE DOWNLOAD IS THE RESPONSE. The saved file must be the bytes
//     the export endpoint answered, not a re-encoding the page built.
//
//  2. NOTHING IS DECIDED FOR THE OPERATOR, AND NOTHING IS HIDDEN. The
//     preview pre-selects no decision, Apply does not exist until every
//     code has one, and a field that does not exist here shows every
//     portable member before it can be created.
//
//  3. AN EDIT DISCARDS THE PREVIEW. Typing into the document after a
//     preview must remove it, so stale decisions cannot be applied.
//
//  4. CREATE CREATES, AND A COLLISION KEPT LOCAL STAYS LOCAL. The new
//     field is stored as reviewed; the colliding local field is
//     unchanged; the receipt and the history agree, checked through the
//     API rather than through the page.
//
// Every field this file touches is created by it, under codes unique to
// the attempt, and removed by it; a cleanup failure fails the test. The
// applies record append-only receipts (there is no delete for them).

import { readFile } from 'node:fs/promises';
import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

function uniqueCode(tag: string): string {
  return `p1576_${tag}_${Date.now().toString(36)}${Math.floor(Math.random() * 1e6).toString(36)}`;
}

type FieldDef = Record<string, unknown> & { id: string; code: string };

async function createField(page: Page, body: Record<string, unknown>, owned: string[]): Promise<FieldDef> {
  const r = await page.request.post('/api/v1/fields', { data: body });
  expect(r.ok(), `create ${String(body.code)} -> ${r.status()} ${await r.text()}`).toBeTruthy();
  const f = (await r.json()) as FieldDef;
  owned.push(f.id);
  return f;
}

async function readField(page: Page, id: string): Promise<FieldDef> {
  const r = await page.request.get(`/api/v1/fields/${id}`);
  expect(r.ok(), `GET field ${id} -> ${r.status()}`).toBeTruthy();
  return (await r.json()) as FieldDef;
}

async function findActive(page: Page, code: string): Promise<FieldDef | undefined> {
  const r = await page.request.get('/api/v1/fields?status=active');
  expect(r.ok()).toBeTruthy();
  return ((await r.json()) as FieldDef[]).find((f) => f.code === code);
}

test.describe('#1576 field schema exchange', () => {
  let owned: string[] = [];

  test.beforeEach(async ({ page }) => {
    owned = [];
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ page }) => {
    const failures: string[] = [];
    for (const id of owned) {
      const r = await page.request.delete(`/api/v1/fields/${id}`);
      if (!r.ok()) failures.push(`${id}: ${r.status()} ${await r.text()}`);
    }
    owned = [];
    expect(failures, 'field cleanup failed').toEqual([]);
  });

  test('export, preview, decide, apply, and read it back in history', async ({ page }, testInfo) => {
    const code = uniqueCode('rt');
    await createField(page, { code, label: 'Exchange round trip', type: 'text', subject_kind: 'asset', display_order: 9999 }, owned);

    // The index links to the exchange.
    await page.goto('/admin/fields');
    await page.getByTestId('admin-fields-link-exchange').click();
    await expect(page).toHaveURL(/\/admin\/fields\/schema-exchange$/);

    // 1. Export exactly this field; the saved file is the response.
    await expect(page.getByTestId('schema-export-button')).toBeDisabled();
    await page.getByRole('searchbox', { name: 'Filter fields' }).fill(code);
    await page.locator(`[data-testid="schema-export-pick"][value="${code}"]`).check();
    await expect(page.getByTestId('schema-export-count')).toHaveText(/1 selected/);
    const [download] = await Promise.all([page.waitForEvent('download'), page.getByTestId('schema-export-button').click()]);
    expect(download.suggestedFilename()).toMatch(/^field-schema-\d{4}-\d{2}-\d{2}\.json$/);
    const saved = await readFile((await download.path())!, 'utf8');
    const direct = await page.request.post('/api/v1/fields/export', { data: { codes: [code] } });
    expect(direct.status()).toBe(200);
    expect(saved).toBe(await direct.text());
    const doc = JSON.parse(saved) as { format: string; fields: { code: string }[] };
    expect(doc.format).toBe('aa-field-schema/1');
    expect(doc.fields.map((f) => f.code)).toEqual([code]);

    // 2. Preview the saved text: identical locally, nothing pre-chosen.
    await page.getByTestId('schema-import-text').fill(saved);
    await page.getByTestId('schema-import-source-label').fill('UI spec <b>round trip</b>');
    await page.getByTestId('schema-import-preview-button').click();
    const field = page.locator(`[data-testid="schema-preview-field"][data-code="${code}"]`);
    await expect(field).toHaveAttribute('data-local', 'identical');
    await expect(page.getByTestId('schema-preview-source-label')).toHaveText('UI spec <b>round trip</b>');
    const radios = field.getByTestId('schema-decision');
    expect(await radios.count()).toBeGreaterThan(0);
    for (let i = 0; i < (await radios.count()); i++) await expect(radios.nth(i)).not.toBeChecked();
    await expect(page.getByTestId('schema-apply-button')).toHaveCount(0);

    // 3. An edit discards the preview.
    await page.getByTestId('schema-import-text').press('End');
    await page.getByTestId('schema-import-text').press('Space');
    await expect(page.getByTestId('schema-import-preview')).toHaveCount(0);
    await page.getByTestId('schema-import-preview-button').click();
    await expect(field).toHaveAttribute('data-local', 'identical');

    // Decide, apply, and read the receipt.
    await field.locator('[data-choice="use_existing"]').check();
    await page.getByTestId('schema-apply-button').click();
    const receipt = page.getByTestId('schema-apply-receipt');
    await expect(receipt).toBeVisible();
    await expect(receipt).toHaveAttribute('data-replayed', 'false');
    await expect(receipt.getByTestId('schema-receipt-item')).toHaveAttribute('data-outcome', 'matched');
    await page.screenshot({ path: testInfo.outputPath('schema-exchange-receipt.png'), fullPage: true });

    // The history shows the same receipt, as recorded.
    await receipt.getByRole('link', { name: 'View this receipt in the history' }).click();
    await expect(page).toHaveURL(/\/admin\/fields\/import-history\?receipt=[0-9a-f-]{36}$/);
    const detail = page.getByTestId('import-history-detail');
    await expect(detail.getByTestId('import-history-item')).toHaveAttribute('data-outcome', 'matched');
    await expect(detail.getByTestId('import-history-source-label')).toHaveText('UI spec <b>round trip</b>');
    await expect(detail.locator('b')).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath('import-history-detail.png'), fullPage: true });
  });

  test('review and create a new field, keep a colliding one local, and refuse a duplicate key', async ({ page }, testInfo) => {
    const created = uniqueCode('new');
    const clash = uniqueCode('clash');
    const local = await createField(page, { code: clash, label: 'Local clash', type: 'text', subject_kind: 'asset', display_order: 9998 }, owned);
    const localBefore = await readField(page, local.id);

    // A complete aa-field-schema/1 document, written by hand: every
    // portable member, nested options with a null label, a pattern-free
    // select, a tab, and a type collision with the local field.
    const incomingNew = {
      code: created,
      label: 'Medium (imported)',
      description: 'What the work is made with',
      type: 'select',
      subject_kind: 'asset',
      required: true,
      searchable: false,
      display_group: 'technical',
      display_order: 42,
      options: [
        { value: 'oil', label: 'Oil paint', children: [] },
        { value: 'ink', label: null, children: [] },
      ],
      regexp_filter: null,
      display_condition: null,
      show_on_card: true,
      show_in_advanced_search: false,
      show_on_upload: false,
      edit_tab: 'Technical',
      open_vocabulary: false,
    };
    const incomingClash = { ...incomingNew, code: clash, label: 'Clash (imported)', description: '', options: [{ value: 'x', label: 'X', children: [] }] };
    const fields = [incomingNew, incomingClash].sort((a, b) => (a.code < b.code ? -1 : 1));
    const text = JSON.stringify({ fields, format: 'aa-field-schema/1' }, null, 2);

    await page.goto('/admin/fields/schema-exchange');

    // A duplicate member reaches the server unmodified and is refused.
    await page.getByTestId('schema-import-text').fill('{"format":"aa-field-schema/1","fields":[],"fields":[]}');
    await page.getByTestId('schema-import-preview-button').click();
    await expect(page.getByTestId('schema-import-preview-error')).toBeVisible();
    await expect(page.getByTestId('schema-import-preview-error')).toHaveText(/document_invalid|request_invalid/);
    await expect(page.getByTestId('schema-import-preview')).toHaveCount(0);

    await page.getByTestId('schema-import-text').fill(text);
    await page.getByTestId('schema-import-preview-button').click();
    const fNew = page.locator(`[data-testid="schema-preview-field"][data-code="${created}"]`);
    const fClash = page.locator(`[data-testid="schema-preview-field"][data-code="${clash}"]`);
    await expect(fNew).toHaveAttribute('data-local', 'absent');
    await expect(fClash).toHaveAttribute('data-local', 'different');

    // The new field's whole definition is on screen before it can be created.
    const review = fNew.getByTestId('schema-preview-incoming');
    await expect(review).toBeVisible();
    for (const k of Object.keys(incomingNew)) await expect(review).toContainText(k);
    await expect(review).toContainText('Oil paint');
    await expect(review).toContainText('ink');
    await expect(review).toContainText('"Technical"');
    await expect(review).toContainText('What the work is made with');
    await expect(fNew.getByTestId('schema-decision-label-create')).toContainText('Create a new local field');
    await expect(fClash.getByTestId('schema-preview-differences')).toContainText('type');
    // An element shot: the admin column scrolls inside the page, so a page
    // shot would show the form, not the reviewed field.
    await fNew.screenshot({ path: testInfo.outputPath('schema-exchange-review.png') });

    await expect(page.getByTestId('schema-apply-button')).toHaveCount(0);
    await fNew.locator('[data-choice="create"]').check();
    await fClash.locator('[data-choice="keep_local"]').check();
    await expect(page.getByTestId('schema-apply-summary')).toContainText('stay this server');
    await page.getByTestId('schema-apply-button').click();
    const receipt = page.getByTestId('schema-apply-receipt');
    await expect(receipt).toBeVisible();
    await expect(page.getByTestId('schema-receipt-changed')).toHaveText('1');
    const outcomes = await receipt.getByTestId('schema-receipt-item').evaluateAll((rows) =>
      rows.map((r) => [r.querySelector('code')?.textContent, (r as HTMLElement).dataset.outcome]),
    );
    expect(Object.fromEntries(outcomes)).toEqual({ [created]: 'created', [clash]: 'kept_local' });
    const receiptId = (await page.getByTestId('schema-receipt-id').textContent())!.trim();
    await page.screenshot({ path: testInfo.outputPath('schema-exchange-created.png'), fullPage: true });

    // Independently: the new field is stored as reviewed...
    const stored = await findActive(page, created);
    expect(stored, 'created field is active').toBeTruthy();
    owned.push(stored!.id);
    const storedFull = await readField(page, stored!.id);
    expect(storedFull.type).toBe('select');
    expect(storedFull.label).toBe('Medium (imported)');
    expect(storedFull.required).toBe(true);
    expect(storedFull.show_on_card).toBe(true);
    expect(storedFull.edit_tab).toBe('Technical');
    expect(JSON.stringify(storedFull.options)).toContain('oil');
    const reexport = await page.request.post('/api/v1/fields/export', { data: { codes: [created] } });
    expect(reexport.status()).toBe(200);
    expect((await reexport.json()).fields[0]).toEqual(incomingNew);
    // ...the colliding local field is unchanged...
    expect(await readField(page, local.id)).toEqual(localBefore);
    // ...and the recorded receipt says exactly that.
    const rec = await page.request.get(`/api/v1/fields/import/receipts/${receiptId}`);
    expect(rec.status()).toBe(200);
    const body = (await rec.json()) as { changed: number; fields: { code: string; outcome: string; field_id: string | null }[] };
    expect(body.changed).toBe(1);
    const byCode = Object.fromEntries(body.fields.map((f) => [f.code, f]));
    expect(byCode[created].outcome).toBe('created');
    expect(byCode[created].field_id).toBe(stored!.id);
    expect(byCode[clash].outcome).toBe('kept_local');
    expect(byCode[clash].field_id).toBe(local.id);

    await receipt.getByRole('link', { name: 'View this receipt in the history' }).click();
    const detail = page.getByTestId('import-history-detail');
    await expect(detail.getByTestId('import-history-past-note')).toBeVisible();
    await expect(detail.getByTestId('import-history-item')).toHaveCount(2);
    await page.screenshot({ path: testInfo.outputPath('import-history-created.png'), fullPage: true });
  });
});
