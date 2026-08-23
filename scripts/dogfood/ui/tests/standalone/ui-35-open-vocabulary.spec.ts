// ui-35-open-vocabulary.spec.ts
//
// #831 + #843 — the open-vocabulary entry control, and what happens
// when the server refuses a value.
//
// The three things worth driving in a real browser, because none of
// them is provable from a unit test:
//
//   1. An OPEN field (`keywords`, open since #830/#846) offers to
//      CREATE a term it does not have, and a real upload through the
//      real modal ends with the term in the field's vocabulary — as a
//      term LABELLED with what was typed — and the asset holding its
//      slug. The label half is the part a unit test cannot reach: the
//      control emits raw text precisely so the server mints the label,
//      and sending a slug instead would store a keyword called
//      "macro-detail". Everything would still be green.
//   2. Case and spacing converge. Typing an existing term in capitals
//      must offer the term, not offer to create a second one.
//   3. A refused write SAYS SO on the row. #843's whole subject is a
//      422 that used to vanish while the upload reported success, so
//      the assertion has to be on what the operator can see.
//
// Each test creates its own field where it needs a specific vocabulary
// shape, with a per-attempt code (DELETE soft-archives and `code` is
// UNIQUE — see ui-18's note). `keywords` itself is shipped and shared,
// so the terms this file creates on it are named with a run-unique
// suffix and never removed: options are not hard-deletable by design.

import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';
import { trackUploadedRows } from '../../helpers/uploaded-rows';

/** Field codes this run created, cleaned up in afterEach. */
let createdFieldIds: string[] = [];

/** ⚠️ THE UPLOADS ARE ROWS TOO, and this file used to leave every one of
 *  them behind: three real assets a run, forever, on a persistent stack.
 *  It went unnoticed because the ids never reach the test — the modal
 *  POSTs them from the browser — so there was nothing obvious to delete.
 *  The tracker reads the id off the modal's own response; see
 *  helpers/uploaded-rows.ts. Named by the fixture ledger (#1247). */
const uploaded = trackUploadedRows();

async function createField(
  page: Page,
  body: Record<string, unknown>,
): Promise<{ id: string; code: string }> {
  const res = await page.request.post('/api/v1/fields', { data: body });
  expect(res.status(), await res.text()).toBe(201);
  const f = (await res.json()) as { id: string; code: string };
  createdFieldIds.push(f.id);
  return f;
}

/** Open the upload modal with one small file queued and wait for it. */
async function queueOneFile(page: Page, name: string): Promise<void> {
  await page.locator(tid('nav-upload-button')).click();
  await expect(page.getByRole('dialog')).toBeVisible();
  await page.locator(tid('upload-file-input')).setInputFiles({
    name,
    mimeType: 'text/plain',
    buffer: Buffer.from(`ui-35 ${name} ${Date.now()}`),
  });
  // The row uploads immediately — the click that opened the modal is
  // the only commit. Wait for it to reach a state the submit accepts.
  await expect(page.getByText(/Ready|Already uploaded/).first()).toBeVisible({ timeout: 30_000 });
}

/** Expand the row's metadata disclosure and wait for the field list. */
async function openMetadata(page: Page): Promise<void> {
  await page.getByText('Metadata', { exact: false }).first().click();
  await expect(page.locator(tid('vocab-input-keywords')).first()).toBeVisible({
    timeout: 20_000,
  });
}

test.describe('UI-35 open-vocabulary entry', () => {
  test.beforeEach(async ({ page }) => {
    createdFieldIds = [];
    uploaded.watch(page);
    await loginAsAdminViaUI(page);
    await page.goto('/');
  });

  test.afterEach(async ({ request }) => {
    // Uploads first: a field cannot be archived out from under a value
    // that still references it, and the asset is what holds the value.
    await uploaded.cleanup(request);
    for (const id of createdFieldIds) {
      await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
    }
    createdFieldIds = [];
  });

  test('typing a novel keyword creates the term, labelled as typed', async ({ page }, testInfo) => {
    // Uploads bytes, creates an asset, writes a field value and then
    // re-reads the vocabulary. The default 30s is the wrong budget for
    // a round trip that does all four on a cold container.
    test.setTimeout(120_000);
    // Run-unique so repeated runs do not collide with a term an
    // earlier one minted — options are never hard-deleted, by design.
    const term = `UI35 Macro ${testInfo.workerIndex}-${Date.now()}`;
    const expectedSlug = term.toLowerCase().replace(/[^a-z0-9]+/g, '-');

    await queueOneFile(page, 'ui35-create.txt');
    await openMetadata(page);

    const input = page.locator(tid('vocab-input-keywords')).first();
    await input.fill(term);

    // The create row is explicit and shows the slug the term will
    // become. Nothing is created by pressing Enter near a text box.
    const create = page.locator(tid('vocab-create-keywords')).first();
    await expect(create).toBeVisible();
    await expect(create).toContainText(expectedSlug);
    await create.click();

    // Chip appears, marked as a term that does not exist yet.
    await expect(page.locator(tid('vocab-chip-keywords')).first()).toContainText(term);

    // Complete the upload. No post — the assertion is about the field
    // value and the vocabulary, not the feed.
    await page.locator(tid('upload-compose-enabled')).uncheck();
    await page.locator(tid('upload-submit')).click();
    // A refused write would leave the modal open with an error; a
    // clean submit closes it.
    await expect(page.getByRole('dialog')).toBeHidden({ timeout: 30_000 });

    // The vocabulary now carries the term, LABELLED with the text that
    // was typed rather than with its slug. This is the assertion that
    // fails if the control ever starts emitting the slug instead of
    // the raw text.
    const fields = await page.request.get('/api/v1/fields?status=active&asset_type=1');
    expect(fields.status()).toBe(200);
    const defs = (await fields.json()) as Array<{
      code: string;
      open_vocabulary?: boolean;
      options?: { values?: Array<string | { value: string; label?: string }> };
    }>;
    const keywords = defs.find((d) => d.code === 'keywords');
    expect(keywords, 'the shipped `keywords` field').toBeTruthy();
    expect(keywords!.open_vocabulary, '`keywords` ships open (#830)').toBe(true);
    const minted = (keywords!.options?.values ?? [])
      .map((v) => (typeof v === 'string' ? { value: v, label: v } : v))
      .find((v) => v.value === expectedSlug);
    expect(minted, `a term ${expectedSlug} was created`).toBeTruthy();
    expect(minted!.label).toBe(term);
  });

  test('an existing term in capitals is offered, not re-created', async ({ page }) => {
    // `landscape` ships in the keywords vocabulary (migration 00024).
    await queueOneFile(page, 'ui35-case.txt');
    await openMetadata(page);

    const input = page.locator(tid('vocab-input-keywords')).first();
    await input.fill('LANDSCAPE');

    // The existing term is on offer …
    const option = page
      .locator(tid('vocab-option-keywords'))
      .filter({ has: page.locator('[data-value="landscape"]') })
      .or(page.locator('[data-testid="vocab-option-keywords"][data-value="landscape"]'));
    await expect(option.first()).toBeVisible();
    // … and nothing offers to create a second one.
    await expect(page.locator(tid('vocab-create-keywords'))).toHaveCount(0);

    // Picking it stores the canonical slug, not the capitals.
    await option.first().click();
    await expect(page.locator(tid('vocab-chip-keywords')).first()).toHaveAttribute(
      'data-value',
      'landscape',
    );
  });

  test('a closed multi_select never offers to create, and its refusal reaches the row', async ({
    page,
  }, testInfo) => {
    const code = `ui35_closed_${testInfo.workerIndex}_${testInfo.retry}_${Date.now()}`;
    await createField(page, {
      code,
      label: 'UI-35 Closed',
      type: 'multi_select',
      subject_kind: 'asset',
      applies_to: [1],
      options: { values: [{ value: 'alpha', label: 'Alpha' }] },
      open_vocabulary: false,
    });

    await queueOneFile(page, 'ui35-closed.txt');
    await openMetadata(page);

    const input = page.locator(tid(`vocab-input-${code}`)).first();
    await expect(input).toBeVisible();
    await input.fill('Something Nobody Defined');
    // No create affordance on a closed field, ever — and the reason is
    // said out loud rather than shown as an empty list.
    await expect(page.locator(tid(`vocab-create-${code}`))).toHaveCount(0);
    await expect(page.locator(tid(`vocab-blocked-${code}`)).first()).toBeVisible();

    // Force a value the server will refuse. The picker cannot produce
    // one — which is the point of the picker — so the refusal is
    // provoked at the store's level, exactly as a stale vocabulary or
    // a concurrently-retired term would produce it in the wild.
    await input.fill('alpha');
    await page.locator(tid(`vocab-option-${code}`)).first().click();
    await expect(page.locator(tid(`vocab-chip-${code}`)).first()).toBeVisible();

    // Retire the term out from under the pending value. Now the write
    // the row is holding is one the server refuses with
    // option_not_offerable — the race this UI has to survive.
    const list = await page.request.get('/api/v1/fields?status=active&asset_type=1');
    const defs = (await list.json()) as Array<{ id: string; code: string; updated_at: string }>;
    const def = defs.find((d) => d.code === code)!;
    const patch = await page.request.patch(`/api/v1/fields/${def.id}`, {
      data: { options: { values: [{ value: 'alpha', label: 'Alpha', status: 'archived' }] } },
    });
    expect(patch.status()).toBe(200);

    await page.locator(tid('upload-submit')).click();

    // #843: the modal STAYS OPEN, and the row names the field and the
    // reason. Before this, the 422 was discarded and the upload
    // reported success.
    await expect(page.locator(tid('upload-field-errors')).first()).toBeVisible({
      timeout: 30_000,
    });
    await expect(page.locator(tid('upload-field-errors')).first()).toContainText('UI-35 Closed');
    await expect(page.locator(tid('upload-compose-error'))).toBeVisible();
    await expect(page.getByRole('dialog')).toBeVisible();
  });

  test('a collection multi_select uses the same control', async ({ page }, testInfo) => {
    const code = `ui35_coll_${testInfo.workerIndex}_${testInfo.retry}_${Date.now()}`;
    await createField(page, {
      code,
      label: 'UI-35 Collection Terms',
      type: 'multi_select',
      subject_kind: 'collection',
      options: { values: [{ value: 'alpha', label: 'Alpha' }, { value: 'beta', label: 'Beta' }] },
      open_vocabulary: false,
    });

    const created = await page.request.post('/api/v1/collections', {
      data: { name: `UI-35 Collection ${Date.now()}` },
    });
    expect(created.status()).toBe(201);
    const collection = (await created.json()) as { id: string };

    await page.goto(`/collections/${collection.id}`);
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();
    await expect(page.getByTestId('collection-fields-section')).toBeVisible();

    const input = page.locator(tid(`vocab-input-${code}`)).first();
    await expect(input).toBeVisible();
    await input.fill('bet');
    await page.locator(tid(`vocab-option-${code}`)).first().click();
    await expect(page.locator(tid(`vocab-chip-${code}`)).first()).toHaveAttribute(
      'data-value',
      'beta',
    );

    await page.getByTestId('collection-fields-save').click();
    await expect(page.getByTestId('collection-fields-saved')).toBeVisible();

    // Persisted as the slug.
    const values = await page.request.get(`/api/v1/collections/${collection.id}/fields`);
    const rows = (await values.json()) as Array<{ field_id: string; value_options?: string[] }>;
    expect(rows.some((r) => (r.value_options ?? []).includes('beta'))).toBe(true);

    await page.request.delete(`/api/v1/collections/${collection.id}?hard=true`);
  });

  test('the admin open-vocabulary toggle round-trips', async ({ page }, testInfo) => {
    const code = `ui35_toggle_${testInfo.workerIndex}_${testInfo.retry}_${Date.now()}`;
    await createField(page, {
      code,
      label: 'UI-35 Toggle',
      type: 'multi_select',
      subject_kind: 'asset',
      applies_to: [1],
      options: { values: [{ value: 'alpha', label: 'Alpha' }] },
    });

    // The index links to the field's own page (#854); the editor is no
    // longer an expanding row.
    await page.goto('/admin/fields');
    const row = page.getByTestId(`admin-fields-row-${code}`);
    await expect(row).toBeVisible();
    await row.getByTestId(`admin-fields-open-${code}`).click();
    await expect(page).toHaveURL(new RegExp(`/admin/fields/${code}$`));

    const toggle = page.getByTestId('field-edit-open-vocabulary');
    await expect(toggle).toBeVisible();
    await expect(toggle).not.toBeChecked();
    await toggle.check();
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();

    // Reload from the server, not from component state.
    await page.reload();
    await expect(page.getByTestId('field-edit-open-vocabulary')).toBeChecked();
  });
});
