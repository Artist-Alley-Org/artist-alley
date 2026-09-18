// The asset edit page can edit an asset's FIELD VALUES (#1119, #1389,
// #1173 part 4).
//
// # What was true before this file
//
// NOTHING drove /assets/{id}/edit. The page wrapped PATCH /assets/{id}
// and submitted six members — title, description, tags, status, mature,
// ai_provenance — and its own comment deferred the rest to "the
// per-field editor (#552)". #552 closed in v0.9.0 having shipped a
// card-display flag and never built one.
//
// So an operator could mark a field `required`, `read_only`, or give it
// a `regexp_filter`, and on an ASSET none of it was reachable after
// upload, because the page had never rendered a field value at all. The
// only frontend writer of asset field values was the upload flush,
// which runs once after creation and only ever SETS.
//
// Every test below fails on that build, and most of them fail by
// timing out on a control that does not exist. That is the point.
//
// # The two discriminators worth reading before changing anything here
//
//  1. DP-a. A DEPRECATED definition must render on the EDIT page and
//     must NOT be offered by the CREATE composer. Both halves are in
//     one test, on both real surfaces, so widening the shared
//     `GET /fields?asset_type=` query to fix the editor FAILS here
//     rather than passing quietly.
//
//  2. CL-c. `required` refusing a removal is only meaningful beside an
//     OPTIONAL removal that succeeds. Without both, "required refuses
//     Clear" is indistinguishable from "Clear is broken everywhere",
//     which is what it actually was.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page } from '@playwright/test';
import { loginAsAdminViaAPI, loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';
import { waitForAssetReady } from '../../helpers/asset-ready';

// Per-run codes. DELETE /fields SOFT-ARCHIVES and `code` is UNIQUE, so a
// fixed code collides on the retry that a flake produces (#527).
const RUN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
const code = (name: string) => `afe_${name}_${RUN}`;

interface Probe {
  id: string;
  code: string;
}

const createdFields: string[] = [];
const createdAssets: string[] = [];

async function makeField(
  request: APIRequestContext,
  name: string,
  body: Record<string, unknown>,
): Promise<Probe> {
  const c = code(name);
  const r = await request.post('/api/v1/fields', {
    data: {
      code: c,
      label: `AFE ${name}`,
      type: 'text',
      subject_kind: 'asset',
      display_order: 9200,
      ...body,
    },
  });
  expect(r.status(), `create field ${c} → ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  createdFields.push(id);
  return { id, code: c };
}

async function patchField(request: APIRequestContext, id: string, data: Record<string, unknown>) {
  const r = await request.patch(`/api/v1/fields/${id}`, { data });
  expect(r.ok(), `patch field → ${r.status()} ${await r.text()}`).toBeTruthy();
}

async function makeAsset(request: APIRequestContext): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    // Novel bytes: storage is content-addressed, so a fixed body would
    // dedupe and hand back an EXISTING asset that other specs own.
    data: Buffer.from(`afe fixture ${Date.now()}-${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status()).toBe(201);
  const { hash } = (await up.json()) as { hash: string };
  const r = await request.post('/api/v1/assets', {
    data: {
      title: `AFE probe ${Date.now()}-${Math.random()}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: 'txt',
    },
  });
  expect(r.status(), `create asset → ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  // The 201 is not a settled row (#1401). CreateAsset commits, enqueues
  // the preview job, then answers; the text pipeline that follows writes
  // `updated_at` again on MarkAssetProcessing, MergeAssetMetadata and
  // MarkAssetReady. An editor opened off the bare 201 loads a baseline
  // the worker is still moving, and the save 409s exactly as the guard
  // (#549) is meant to. So the fixture returns only once the row is
  // `ready`, which is the last of those writes; a `failed` preview
  // throws here instead, because its write history is a different one
  // and not the subject of any test in this file.
  await waitForAssetReady(request, id, { label: 'afe fixture asset' });
  createdAssets.push(id);
  return id;
}

/** Write a value straight through the API — the "somebody else" writer. */
async function putValue(
  request: APIRequestContext,
  assetId: string,
  fieldId: string,
  body: Record<string, unknown>,
): Promise<{ set_at: string }> {
  const r = await request.put(`/api/v1/assets/${assetId}/fields/${fieldId}`, { data: body });
  expect(r.ok(), `put value → ${r.status()} ${await r.text()}`).toBeTruthy();
  return (await r.json()) as { set_at: string };
}

/**
 * What the SERVER holds for one field, read back rather than scraped
 * off the page. A control showing a value proves the control rendered;
 * only a read proves the value was written.
 */
async function storedValue(
  request: APIRequestContext,
  assetId: string,
  fieldId: string,
): Promise<Record<string, unknown> | undefined> {
  const r = await request.get(`/api/v1/assets/${assetId}/fields`);
  expect(r.ok()).toBeTruthy();
  const rows = (await r.json()) as Array<Record<string, unknown>>;
  return rows.find((v) => v.field_id === fieldId);
}

async function openEdit(page: Page, assetId: string) {
  await page.goto(`/assets/${assetId}/edit`);
  await expect(page.locator(tid('asset-edit-form'))).toBeVisible();
}

test.describe.configure({ mode: 'serial' });

test.beforeEach(async ({ page }) => {
  await loginAsAdminViaUI(page);
});

test.afterAll(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  for (const id of createdAssets) {
    await request.delete(`/api/v1/assets/${id}?hard=true`).catch(() => undefined);
  }
  for (const id of createdFields) {
    await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
  }
});

// ---------------------------------------------------------------------------
// U-a / U-b / U-c / U-d / U-g — the section exists, saves, and does not
// duplicate the controls the page already owns
// ---------------------------------------------------------------------------

test('an ordinary field renders, edits and PERSISTS across a reload', async ({ page, request }) => {
  await loginAsAdminViaAPI(request);
  const f = await makeField(request, 'plain', {
    description: 'What the client needs to know.',
  });
  const assetId = await makeAsset(request);

  await openEdit(page, assetId);

  // U-a: this control does not exist on the shipped build. Before
  // #1119 the page rendered title, description, tags, status, mature
  // and ai_provenance, and nothing else.
  const input = page.locator(tid(`field-input-${f.code}`));
  await expect(input).toBeVisible();

  // U-g: the operator's own note about the field, rendered where the
  // person filling it in can read it.
  await expect(page.locator(tid(`field-help-${f.code}`))).toHaveText(
    'What the client needs to know.',
  );

  await input.fill('written from the edit page');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();

  // U-b: PERSISTED. Asserted against the API, then against the
  // re-rendered page, because a component that keeps its own state
  // across a save shows the right string either way.
  const stored = await storedValue(request, assetId, f.id);
  expect(stored?.value_text).toBe('written from the edit page');
  await page.reload();
  await expect(page.locator(tid(`field-input-${f.code}`))).toHaveValue(
    'written from the edit page',
  );
});

test('EXACTLY ONE control each for title, description and the ordinary field', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const f = await makeField(request, 'once', {});
  const assetId = await makeAsset(request);
  await openEdit(page, assetId);
  await expect(page.locator(tid(`field-input-${f.code}`))).toBeVisible();

  // U-c, BY COUNT. A section that rendered every returned definition
  // would put a second title box under the first one, because
  // GET /assets/{id}/fields DOES return the mirrored columns.
  await expect(page.locator(tid('asset-edit-title'))).toHaveCount(1);
  await expect(page.locator(tid('asset-edit-description'))).toHaveCount(1);
  await expect(page.locator(tid(`field-input-${f.code}`))).toHaveCount(1);

  // U-d: and the mirrored definitions are not in the generic section at
  // all, under any id.
  await expect(page.locator(tid('field-input-title'))).toHaveCount(0);
  await expect(page.locator(tid('field-input-description'))).toHaveCount(0);
});

// U-h. The direct title control keeps writing through PATCH
// /assets/{id}: the two planes stay separate, and the mirrored
// exclusion above did not cost the page its own save.
test('the direct title control still saves through PATCH /assets/{id}', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const assetId = await makeAsset(request);
  await openEdit(page, assetId);

  const patched = page.waitForResponse(
    (r) => r.url().includes(`/api/v1/assets/${assetId}`) && r.request().method() === 'PATCH',
  );
  await page.locator(tid('asset-edit-title')).fill('retitled by the direct control');
  await page.locator(tid('asset-edit-save')).click();
  const resp = await patched;
  expect(resp.ok(), `PATCH → ${resp.status()} ${await resp.text()}`).toBeTruthy();

  const r = await request.get(`/api/v1/assets/${assetId}`);
  expect(((await r.json()) as { title: string }).title).toBe('retitled by the direct control');
});

// ---------------------------------------------------------------------------
// U-e / U-f — read_only and regexp_filter reach the asset surface
// ---------------------------------------------------------------------------

test('a read_only field shows its VALUE, disabled, with a reason', async ({ page, request }) => {
  await loginAsAdminViaAPI(request);
  const f = await makeField(request, 'ro', {});
  const assetId = await makeAsset(request);
  // Seeded BEFORE the flag goes on: read_only refuses human writes, and
  // the point of the assertion is that the value stays readable.
  await putValue(request, assetId, f.id, { value_text: 'written by the system' });
  await patchField(request, f.id, { read_only: true });

  await openEdit(page, assetId);
  const input = page.locator(tid(`field-input-${f.code}`));
  await expect(input).toBeVisible();
  await expect(input).toHaveValue('written by the system');
  await expect(input).toBeDisabled();
  await expect(page.locator(tid(`field-readonly-${f.code}`))).toBeVisible();
});

test('regexp_filter: the rule is shown, the client refuses, and the SERVER refusal renders when the client check never ran', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  // `regexp_filter` and `read_only` are PATCH-only on the field API —
  // FieldDefinitionCreate carries neither, deliberately, so configuring
  // them is always an explicit second act.
  const f = await makeField(request, 'pat', {});
  await patchField(request, f.id, { regexp_filter: '[A-Z]{3}_[0-9]{4}' });
  const assetId = await makeAsset(request);

  await openEdit(page, assetId);
  await expect(page.locator(tid(`field-pattern-${f.code}`))).toContainText('[A-Z]{3}_[0-9]{4}');

  // The CLIENT half: a violating value is marked and the save is
  // refused before any round trip.
  await page.locator(tid(`field-input-${f.code}`)).fill('nope');
  await expect(page.locator(tid(`field-pattern-error-${f.code}`))).toBeVisible();
  await expect(page.locator(tid('asset-fields-save'))).toBeDisabled();

  await page.locator(tid(`field-input-${f.code}`)).fill('AAA_0010');
  await expect(page.locator(tid(`field-pattern-error-${f.code}`))).toHaveCount(0);

  // The SERVER half, reached by a real bypass rather than a hack: the
  // operator TIGHTENS the pattern after this form loaded, so the copy
  // of the definition in the browser has no idea. This is the case that
  // matters — a client-side check is a convenience, and the rule has to
  // hold when it is not the check that ran.
  await patchField(request, f.id, { regexp_filter: 'ZZZ_[0-9]{4}' });
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid(`field-error-${f.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-error-${f.code}`))).toContainText('ZZZ_[0-9]{4}');

  // And nothing was stored.
  expect(await storedValue(request, assetId, f.id)).toBeUndefined();
});

// ---------------------------------------------------------------------------
// U-i / U-j / U-k / DP-a — which DEFINITIONS the page is allowed to offer
// ---------------------------------------------------------------------------

test('a collection-scoped definition never renders on the asset edit page', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const c = `afe_coll_${RUN}`;
  const r = await request.post('/api/v1/fields', {
    data: { code: c, label: 'AFE collection-only', type: 'text', subject_kind: 'collection' },
  });
  expect(r.status()).toBe(201);
  createdFields.push(((await r.json()) as { id: string }).id);

  const assetId = await makeAsset(request);
  await openEdit(page, assetId);
  await expect(page.locator(tid('asset-edit-title'))).toBeVisible();
  await expect(page.locator(tid(`field-input-${c}`))).toHaveCount(0);
});

test('an ARCHIVED definition is not offered, and its stored value stays BYTE-IDENTICAL', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const f = await makeField(request, 'arch', {});
  const assetId = await makeAsset(request);
  await putValue(request, assetId, f.id, { value_text: 'value on a tombstone' });
  // DELETE soft-archives a field definition; the values it holds are
  // deliberately left standing.
  const del = await request.delete(`/api/v1/fields/${f.id}`);
  expect(del.ok(), `archive field → ${del.status()}`).toBeTruthy();

  await openEdit(page, assetId);
  await expect(page.locator(tid('asset-edit-title'))).toBeVisible();
  await expect(page.locator(tid(`field-input-${f.code}`))).toHaveCount(0);

  // U-k, the half that would be a data loss rather than a display bug:
  // NOT RENDERING a value must never be what clears it. Saving the page
  // afterwards must leave it exactly where it was.
  await page.locator(tid('asset-edit-title')).fill('touched after archiving');
  await page.locator(tid('asset-edit-save')).click();
  await page.waitForURL(new RegExp(`/assets/${assetId}$`));
  const stored = await storedValue(request, assetId, f.id);
  expect(stored?.value_text).toBe('value on a tombstone');
});

test('DP-a: a DEPRECATED definition is EDITABLE on the edit page and NOT OFFERED by the create composer', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const f = await makeField(request, 'dep', {});
  const assetId = await makeAsset(request);
  await putValue(request, assetId, f.id, { value_text: 'held on an existing row' });
  await patchField(request, f.id, { status: 'deprecated' });

  // ── HALF ONE: the EDITOR renders it and it is editable. A deprecated
  // definition is one an operator stopped wanting NEW values in; the
  // rows already carrying one are still live data, and an editor that
  // hid them would hide part of what the record contains.
  await openEdit(page, assetId);
  const input = page.locator(tid(`field-input-${f.code}`));
  await expect(input).toBeVisible();
  await expect(input).toHaveValue('held on an existing row');
  await input.fill('edited while deprecated');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  expect((await storedValue(request, assetId, f.id))?.value_text).toBe('edited while deprecated');

  // ── HALF TWO: the COMPOSER does not offer it. Same test, so widening
  // the query that fixes half one breaks here instead of shipping.
  //
  // Driven on the real /create page: a file is dropped, which is what
  // makes the page fetch the field list for the row's asset type.
  const created = page.waitForResponse(
    (r) => r.url().includes('/api/v1/assets') && r.request().method() === 'POST',
  );
  await page.goto('/create');
  await expect(page.locator(tid('create-page'))).toBeVisible();
  await page.locator(tid('create-file-input')).setInputFiles({
    name: `afe-dp-${RUN}.txt`,
    mimeType: 'text/plain',
    buffer: Buffer.from(`afe dp fixture ${Date.now()}-${Math.random()}`),
  });
  await expect(page.locator(tid('create-file-row'))).toHaveCount(1);
  await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });
  createdAssets.push(((await (await created).json()) as { id: string }).id);

  // The section is open on the page; the deprecated field is not in it.
  await expect(page.locator(tid(`create-field-${f.code}`))).toHaveCount(0);

  // And the composer's own request is asserted directly, because the
  // absence above would also be produced by a page that failed to load
  // any fields at all.
  const composer = await request.get('/api/v1/fields?status=active&asset_type=2');
  expect(composer.ok()).toBeTruthy();
  const composerCodes = ((await composer.json()) as Array<{ code: string }>).map((x) => x.code);
  expect(composerCodes).not.toContain(f.code);

  const editor = await request.get('/api/v1/fields?asset_type=2');
  expect(editor.ok()).toBeTruthy();
  const editorCodes = ((await editor.json()) as Array<{ code: string }>).map((x) => x.code);
  expect(editorCodes).toContain(f.code);
});

// ---------------------------------------------------------------------------
// CL — removing a value through the ordinary control
// ---------------------------------------------------------------------------

test('CL-a / CL-e: emptying the control REMOVES the value, for text, multi_select and boolean', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const txt = await makeField(request, 'cltext', {});
  const ms = await makeField(request, 'clms', {
    type: 'multi_select',
    options: { values: [{ value: 'alpha', label: 'Alpha' }, { value: 'beta', label: 'Beta' }] },
  });
  const bool = await makeField(request, 'clbool', { type: 'boolean' });
  const neighbour = await makeField(request, 'clkeep', {});
  const assetId = await makeAsset(request);

  await putValue(request, assetId, txt.id, { value_text: 'remove me' });
  await putValue(request, assetId, ms.id, { value_options: ['alpha', 'beta'] });
  await putValue(request, assetId, bool.id, { value_num: 1 });
  await putValue(request, assetId, neighbour.id, { value_text: 'untouched' });

  await openEdit(page, assetId);
  await expect(page.locator(tid(`field-input-${txt.code}`))).toHaveValue('remove me');

  // The interaction is EMPTYING THE CONTROL. No Clear button was
  // invented — including for boolean, which became a three-state select
  // precisely so that emptying it is the same gesture as emptying a
  // select rather than a second kind of affordance.
  await page.locator(tid(`field-input-${txt.code}`)).fill('');
  // Every chip removed is what "emptied" means for a multi_select, and
  // the combobox's remove buttons are not per-slug ids — the list
  // reindexes as each one goes, so take the first repeatedly.
  const chipRemove = page.locator(`[data-testid="vocab-chip-remove-${ms.code}"]`);
  while ((await chipRemove.count()) > 0) {
    await chipRemove.first().click();
  }
  // Removing the last chip leaves the combobox's option list open, and
  // it overlaps the save button in the two-column layout.
  await page.keyboard.press('Escape');
  await page.locator(tid(`field-input-${bool.code}`)).selectOption('');

  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();

  // ABSENT — the row is gone, not blanked.
  expect(await storedValue(request, assetId, txt.id)).toBeUndefined();
  expect(await storedValue(request, assetId, ms.id)).toBeUndefined();
  expect(await storedValue(request, assetId, bool.id)).toBeUndefined();
  // The neighbour is BYTE-IDENTICAL.
  expect((await storedValue(request, assetId, neighbour.id))?.value_text).toBe('untouched');
});

test('CL-c: the same removal against a REQUIRED field is refused and the value survives', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const f = await makeField(request, 'clreq', { required: true });
  const assetId = await makeAsset(request);
  await putValue(request, assetId, f.id, { value_text: 'cannot be removed' });

  await openEdit(page, assetId);
  await page.locator(tid(`field-input-${f.code}`)).fill('');
  await page.locator(tid('asset-fields-save')).click();

  // The SERVER is what refuses, and its sentence is what renders.
  await expect(page.locator(tid(`field-error-${f.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-error-${f.code}`))).toContainText('required');
  expect((await storedValue(request, assetId, f.id))?.value_text).toBe('cannot be removed');
});

test('CL-f: a definition that is already absent and left empty sends NO request', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const empty = await makeField(request, 'clphantom', {});
  const other = await makeField(request, 'clother', {});
  const assetId = await makeAsset(request);

  await openEdit(page, assetId);

  const touched: string[] = [];
  page.on('request', (r) => {
    if (r.url().includes(`/fields/${empty.id}`)) touched.push(`${r.method()} ${r.url()}`);
  });

  // Type into the OTHER field, so the save genuinely runs.
  await page.locator(tid(`field-input-${other.code}`)).fill('something');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();

  // An untouched blank input is not a deletion. A save model that
  // mapped "empty" to DELETE without asking whether a value ever
  // existed would have fired one here.
  expect(touched, 'an absent field left empty must produce no request').toEqual([]);
});

test('CL-g: boolean FALSE is stored as a value and does NOT clear', async ({ page, request }) => {
  await loginAsAdminViaAPI(request);
  const optional = await makeField(request, 'boolopt', { type: 'boolean' });
  const required = await makeField(request, 'boolreq', { type: 'boolean', required: true });
  const assetId = await makeAsset(request);
  await putValue(request, assetId, required.id, { value_num: 1 });

  await openEdit(page, assetId);

  // Choosing "no" writes 0. A checkbox could not express this: it
  // rendered an absent value and a stored false identically, so the
  // control could not even DISPLAY the difference it is now asserting.
  await page.locator(tid(`field-input-${optional.code}`)).selectOption('false');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  const stored = await storedValue(request, assetId, optional.id);
  expect(stored, 'FALSE is a real value; the row must EXIST').toBeDefined();
  expect(stored?.value_num).toBe(0);

  // Reload: the control must show "no", not blank.
  await page.reload();
  await expect(page.locator(tid(`field-input-${optional.code}`))).toHaveValue('false');

  // Now remove it -> the row goes.
  await page.locator(tid(`field-input-${optional.code}`)).selectOption('');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  expect(await storedValue(request, assetId, optional.id)).toBeUndefined();

  // A REQUIRED boolean refuses removal, and its value is untouched.
  await page.locator(tid(`field-input-${required.code}`)).selectOption('');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid(`field-error-${required.code}`))).toBeVisible();
  expect((await storedValue(request, assetId, required.id))?.value_num).toBe(1);
});

// ---------------------------------------------------------------------------
// UC — the editor CONSUMES the per-field concurrency contract
// ---------------------------------------------------------------------------

test('UC-a / UC-d: a stale save is refused, the newer value survives, and the input is kept', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const f = await makeField(request, 'ucconf', {});
  const assetId = await makeAsset(request);
  await putValue(request, assetId, f.id, { value_text: 'original' });

  await openEdit(page, assetId);
  await expect(page.locator(tid(`field-input-${f.code}`))).toHaveValue('original');

  // Somebody else, after this form loaded.
  await putValue(request, assetId, f.id, { value_text: 'written by somebody else' });

  const puts: string[] = [];
  page.on('request', (r) => {
    if (r.url().includes(`/fields/${f.id}`)) puts.push(`${r.method()} ${r.url()}`);
  });

  await page.locator(tid(`field-input-${f.code}`)).fill('my edit');
  await page.locator(tid('asset-fields-save')).click();

  await expect(page.locator(tid(`field-conflict-${f.code}`))).toBeVisible();
  // The user's input is still there. A save that was REFUSED is not a
  // save that was undone.
  await expect(page.locator(tid(`field-input-${f.code}`))).toHaveValue('my edit');
  expect((await storedValue(request, assetId, f.id))?.value_text).toBe('written by somebody else');

  // UC-d: no silent retry, and no fall-back to an unguarded write. One
  // attempt, and it stopped.
  await page.waitForTimeout(500);
  expect(puts.length, `expected exactly one attempt, got ${puts.join(' | ')}`).toBe(1);

  // Re-baselined on the server's newer token: pressing save again is a
  // DELIBERATE overwrite, not another 409.
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  expect((await storedValue(request, assetId, f.id))?.value_text).toBe('my edit');
});

test('UC-b / UC-c: the first Set guards on ABSENCE, and the second uses the token the first RETURNED', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const f = await makeField(request, 'uctoken', {});
  const assetId = await makeAsset(request);

  await openEdit(page, assetId);

  const bodies: Array<Record<string, unknown>> = [];
  page.on('request', (r) => {
    if (r.url().includes(`/fields/${f.id}`) && r.method() === 'PUT') {
      bodies.push(JSON.parse(r.postData() ?? '{}') as Record<string, unknown>);
    }
  });

  // UC-c: no value was loaded, so the guard is ABSENCE. Never an
  // invented timestamp for a row nobody wrote.
  await page.locator(tid(`field-input-${f.code}`)).fill('first');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  expect(bodies).toHaveLength(1);
  expect(bodies[0].if_absent).toBe(true);
  expect(bodies[0].if_unchanged_since).toBeUndefined();

  const afterFirst = (await storedValue(request, assetId, f.id))?.set_at as string;
  expect(afterFirst).toBeTruthy();

  // UC-b: the SECOND save from the SAME still-open form. The token on
  // the wire must be the one the first response returned — keeping the
  // pre-save baseline here is how a form conflicts with its own
  // previous save.
  await page.locator(tid(`field-input-${f.code}`)).fill('second');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  expect(bodies).toHaveLength(2);
  expect(bodies[1].if_absent).toBeUndefined();
  expect(
    bodies[1].if_unchanged_since,
    'the second save must guard on the token the FIRST response returned',
  ).toBe(afterFirst);
  expect((await storedValue(request, assetId, f.id))?.value_text).toBe('second');
});

// ---------------------------------------------------------------------------
// MX-a — N>=2 dirty fields, MIXED outcomes, on the real surface
// ---------------------------------------------------------------------------

test('MX-a: X saves while Y conflicts, independently, and Z is untouched', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const x = await makeField(request, 'mxx', {});
  const y = await makeField(request, 'mxy', {});
  const z = await makeField(request, 'mxz', {});
  const assetId = await makeAsset(request);
  await putValue(request, assetId, x.id, { value_text: 'x original' });
  await putValue(request, assetId, y.id, { value_text: 'y original' });
  await putValue(request, assetId, z.id, { value_text: 'z untouched' });

  // 1 + 2: load both, edit both.
  await openEdit(page, assetId);
  await expect(page.locator(tid(`field-input-${x.code}`))).toHaveValue('x original');
  await page.locator(tid(`field-input-${x.code}`)).fill('x mine');
  await page.locator(tid(`field-input-${y.code}`)).fill('y mine');

  // 3: another actor changes Y ONLY.
  await putValue(request, assetId, y.id, { value_text: 'y from somebody else' });

  // 4: save the form.
  await page.locator(tid('asset-fields-save')).click();

  // 5 + 6 + 7: X SUCCEEDS, Y 409s and the newer Y survives, and the UI
  // says which one. These are independent per-field operations, not a
  // batch transaction — a fix that rolled the form back on any failure
  // would lose the X edit here.
  await expect(page.locator(tid(`field-conflict-${y.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-conflict-${x.code}`))).toHaveCount(0);
  expect((await storedValue(request, assetId, x.id))?.value_text).toBe('x mine');
  expect((await storedValue(request, assetId, y.id))?.value_text).toBe('y from somebody else');

  // 8: reconciliation does not rewrite X from its old baseline, and X
  // must not conflict with its own successful prior save. Pressing save
  // again resolves Y and leaves X exactly as it was.
  const xRequests: string[] = [];
  page.on('request', (r) => {
    if (r.url().includes(`/fields/${x.id}`)) xRequests.push(r.method());
  });
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  expect(xRequests, 'X already saved; the retry must not resend it').toEqual([]);
  expect((await storedValue(request, assetId, y.id))?.value_text).toBe('y mine');
  expect((await storedValue(request, assetId, x.id))?.value_text).toBe('x mine');

  // 9: the neighbour nobody touched.
  expect((await storedValue(request, assetId, z.id))?.value_text).toBe('z untouched');
});

// The boundary matrix's N=0 cell is asserted in
// web/src/lib/components/FieldValuesSection.test.ts instead: the dev and
// CI corpora both carry ~25 asset field definitions with an empty
// `applies_to`, so no asset on either instance HAS zero applicable
// fields, and archiving them mid-suite to manufacture the state would
// change the page under every other spec.

// ---------------------------------------------------------------------------
// #1401: the fixture waits for its pipeline
//
// The standalone suite went red on this file on three of four landing
// pushes, always as a 409 from `PATCH /assets/{id}`. The product was
// right each time: `makeAsset` returned on the bare 201, `openEdit`
// loaded a baseline `updated_at`, and the text preview pipeline wrote
// the row again (MarkAssetProcessing, MergeAssetMetadata, MarkAssetReady,
// each `updated_at = now()`) between that load and the save. The repair
// is in the fixture, not the guard: `makeAsset` now returns only once
// the row reports `processing_status = ready`, the last of those writes.
//
// Three classes below. A1 drives the file-local fixture through a
// scripted request double and is the deterministic fail-before-fix
// case: on the pre-fix `makeAsset` it records ZERO readiness reads.
// B1-B4 pin the helper's boundaries with injected timing. C1 and C2 go
// through the real API and the real editor, and C2 is the load-bearing
// proof that the guard itself is untouched.
// ---------------------------------------------------------------------------

/** A scripted response, the four members the fixture and the helper read. */
function scripted(status: number, body: unknown) {
  return {
    ok: () => status >= 200 && status < 300,
    status: () => status,
    json: async () => body,
    text: async () => JSON.stringify(body),
  };
}

/**
 * A request double that answers `POST` twice (the upload, then the
 * create) and `GET /api/v1/assets/{id}` from a queue of readiness
 * states, holding the last state once the queue runs dry. Every GET is
 * recorded so a test can say how many times readiness was asked for
 * and what the last answer was.
 */
function readinessDouble(mintedId: string, states: Array<string | number>) {
  const posts: string[] = [];
  const gets: Array<{ url: string; state: string | number }> = [];
  const queue = [...states];
  return {
    posts,
    gets,
    mintedId,
    post: async (url: string) => {
      posts.push(url);
      return posts.length === 1
        ? scripted(201, { hash: `afe-1401-hash-${mintedId}` })
        : scripted(201, { id: mintedId });
    },
    get: async (url: string) => {
      const next = queue.length > 1 ? (queue.shift() as string | number) : queue[0];
      gets.push({ url, state: next });
      // A number in the queue is a non-OK HTTP status with no body.
      if (typeof next === 'number') return scripted(next, { error: `scripted ${next}` });
      return scripted(200, { id: mintedId, processing_status: next, updated_at: '2026-01-01T00:00:00Z' });
    },
  };
}

/** Microseconds since the epoch, the precision the guard compares at. */
function micros(iso: unknown): number {
  const m = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/.exec(String(iso));
  if (!m) throw new Error(`not an RFC 3339 timestamp: ${String(iso)}`);
  const ms = Date.parse(`${m[1]}${m[3]}`);
  const frac = (m[2] ?? '').padEnd(6, '0').slice(0, 6);
  return ms * 1000 + Number(frac);
}

/** A real txt asset through the real API, with its creation-time `updated_at`. */
async function createTxtAsset(request: APIRequestContext): Promise<{ id: string; updated_at: string }> {
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`afe 1401 ${Date.now()}-${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status()).toBe(201);
  const { hash } = (await up.json()) as { hash: string };
  const r = await request.post('/api/v1/assets', {
    data: {
      title: `AFE 1401 ${Date.now()}-${Math.random()}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: 'txt',
    },
  });
  expect(r.status(), `create asset → ${r.status()} ${await r.text()}`).toBe(201);
  const created = (await r.json()) as { id: string; updated_at: string };
  createdAssets.push(created.id);
  return created;
}

test('A1: makeAsset returns only after the asset has reported ready (#1401)', async () => {
  const double = readinessDouble(crypto.randomUUID(), ['processing', 'processing', 'ready']);

  const id = await makeAsset(double as unknown as APIRequestContext);

  expect(double.posts, 'upload then create, nothing else').toHaveLength(2);
  expect(
    double.gets.length,
    'the fixture must read readiness before returning; a 201 is not a settled row',
  ).toBeGreaterThan(0);
  for (const g of double.gets) expect(g.url).toBe(`/api/v1/assets/${double.mintedId}`);
  expect(double.gets[double.gets.length - 1].state, 'the last readiness answer').toBe('ready');
  expect(id).toBe(double.mintedId);
});

test('B1: the helper resolves with the ready representation after exactly two reads', async () => {
  const double = readinessDouble(crypto.randomUUID(), ['processing', 'ready']);
  const got = await waitForAssetReady(double, double.mintedId, { pollMs: 10, timeoutMs: 5_000 });
  expect(got.processing_status).toBe('ready');
  expect(got.id).toBe(double.mintedId);
  expect(double.gets.map((g) => g.state)).toEqual(['processing', 'ready']);
});

test('B2: the helper refuses a FAILED preview at once, naming the state it saw', async () => {
  const double = readinessDouble(crypto.randomUUID(), ['processing', 'failed']);
  const started = Date.now();
  await expect(
    waitForAssetReady(double, double.mintedId, { pollMs: 10, timeoutMs: 30_000 }),
  ).rejects.toThrow(/processing_status: failed/);
  expect(Date.now() - started, 'failed is terminal; no waiting out the deadline').toBeLessThan(5_000);
  expect(double.gets.map((g) => g.state)).toEqual(['processing', 'failed']);
});

test('B3: a row that never settles throws at the deadline with the last state, and the reads are bounded', async () => {
  const double = readinessDouble(crypto.randomUUID(), ['processing']);
  const pollMs = 50;
  const timeoutMs = 250;
  const started = Date.now();
  await expect(
    waitForAssetReady(double, double.mintedId, { pollMs, timeoutMs }),
  ).rejects.toThrow(/never reached processing_status=ready[\s\S]*processing_status: processing/);
  expect(Date.now() - started).toBeLessThan(timeoutMs + 1_000);
  expect(double.gets.length).toBeGreaterThanOrEqual(2);
  expect(double.gets.length).toBeLessThanOrEqual(Math.ceil(timeoutMs / pollMs) + 1);
  for (const g of double.gets) expect(g.state).toBe('processing');
});

test('B4: a non-OK answer inside the deadline is tolerated and polling continues', async () => {
  const double = readinessDouble(crypto.randomUUID(), [503, 'processing', 'ready']);
  const got = await waitForAssetReady(double, double.mintedId, { pollMs: 10, timeoutMs: 5_000 });
  expect(got.processing_status).toBe('ready');
  expect(double.gets.map((g) => g.state)).toEqual([503, 'processing', 'ready']);
});

test('C1: a real txt asset moves updated_at after creation, and the editor opened after ready saves on the settled baseline', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const created = await createTxtAsset(request);

  const ready = await waitForAssetReady(request, created.id, { label: 'C1 asset' });
  expect(ready.processing_status).toBe('ready');
  expect(
    micros(ready.updated_at),
    'the pipeline writes the row after the 201; the creation-time updated_at is not the settled one',
  ).toBeGreaterThan(micros(created.updated_at));

  await openEdit(page, created.id);

  let sent: Record<string, unknown> | undefined;
  page.on('request', (r) => {
    if (r.url().includes(`/api/v1/assets/${created.id}`) && r.method() === 'PATCH') {
      sent = JSON.parse(r.postData() ?? '{}') as Record<string, unknown>;
    }
  });
  await page.locator(tid('asset-edit-title')).fill('C1 retitled on a settled baseline');

  // Read the API's current updated_at right before the save. The body
  // the page sends must carry exactly this value, at the precision the
  // guard compares at: nothing moved the row between load and save.
  const before = await request.get(`/api/v1/assets/${created.id}`);
  const current = ((await before.json()) as { updated_at: string }).updated_at;

  const patched = page.waitForResponse(
    (r) => r.url().includes(`/api/v1/assets/${created.id}`) && r.request().method() === 'PATCH',
  );
  await page.locator(tid('asset-edit-save')).click();
  const resp = await patched;
  expect(resp.status(), `PATCH → ${resp.status()} ${await resp.text()}`).toBe(200);
  expect(sent?.if_unchanged_since, 'the page sends its baseline as the guard token').toBeTruthy();
  expect(micros(sent?.if_unchanged_since)).toBe(micros(current));

  const after = await request.get(`/api/v1/assets/${created.id}`);
  expect(((await after.json()) as { title: string }).title).toBe('C1 retitled on a settled baseline');
});

test('C2: after ready, a save guarded on the creation-time updated_at is still refused with 409', async ({
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const created = await createTxtAsset(request);
  const ready = await waitForAssetReady(request, created.id, { label: 'C2 asset' });
  expect(ready.processing_status).toBe('ready');

  const stale = await request.patch(`/api/v1/assets/${created.id}`, {
    data: { title: 'C2 must never land', if_unchanged_since: created.updated_at },
  });
  expect(stale.status(), `stale PATCH → ${stale.status()} ${await stale.text()}`).toBe(409);
  const body = (await stale.json()) as { error: string; updated_at: string };
  expect(body.error).toContain('edited by someone else');

  const now = await request.get(`/api/v1/assets/${created.id}`);
  const row = (await now.json()) as { title: string; updated_at: string };
  expect(micros(body.updated_at), 'the 409 carries the current updated_at').toBe(micros(row.updated_at));
  expect(row.title, 'the refused save wrote nothing').not.toBe('C2 must never land');
});
