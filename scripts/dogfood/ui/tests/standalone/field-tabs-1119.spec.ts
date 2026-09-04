// `edit_tab` GETS ITS FIRST CONSUMER, and the operator can reach
// `display_condition` (#1173 part 4, #1119, ADR 0099 §9).
//
// # What was true before this file
//
// `edit_tab` shipped in sprint 18b: stored, validated, written by
// FieldEditor.svelte, and consumed by NOTHING. Zero references across
// web/src/**/*.svelte outside the admin editor itself. An operator could
// fill the box in and no surface anywhere composed differently.
//
// So every assertion here about a field MOVING between sections, about a
// strip existing, or about the `/create` page nesting sections inside an
// asset type, fails on that build by locating a control that is not
// there.
//
// # The discriminators
//
//  1. ⛔ A TAB MUST CHANGE WHAT IS RENDERED, NOT WHAT IS STYLED. Every
//     tab test below asserts that switching REMOVES one field from the
//     DOM and ADDS another. A strip that merely highlighted a segment
//     would pass a visibility check and fail these.
//
//  2. ⛔ SAME-NAMED TABS FROM DIFFERENT ASSET TYPES MUST NEVER MERGE.
//     Asset type is the OUTER axis on /create. Two types both naming a
//     tab "Print" are two operator decisions about two different kinds of
//     thing.
//
//  3. ⛔ POLICY B. A tab emptied by a condition KEEPS its chrome and its
//     selection. Deriving the strip from visible controls would make a
//     tab vanish from under the person as a side effect of typing
//     somewhere else.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page } from '@playwright/test';
import { loginAsAdminViaAPI, loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

const RUN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
const code = (name: string) => `ft_${name}_${RUN}`;

interface Probe {
  id: string;
  code: string;
}

// ⚠️ CLEANED AFTER EVERY TEST, not at the end of the file.
//
// These specs run against a SHARED INSTALL whose field corpus is global:
// a tab this test configures is a tab the NEXT test's strip counts. The
// first version cleaned up in afterAll and the three-bucket test saw four
// tabs, because the previous test's "Print" was still configured. A count
// assertion is only meaningful if the corpus is known, so each test
// leaves the corpus exactly as it found it.
const createdFields: string[] = [];
const createdAssets: string[] = [];

/**
 * Create a field, then CONFIGURE it.
 *
 * ⚠️ THE SECOND REQUEST IS NOT A CONVENIENCE. `edit_tab` is UPDATE-ONLY:
 * `FieldDefinitionCreate` carries none of the participation properties
 * (18b's decision, and `display_condition` follows it in ADR 0099 §1), so
 * an `edit_tab` in a create body is accepted and SILENTLY IGNORED. A
 * fixture that sent it there would build a field with no tab and the test
 * would fail looking for a strip that was never configured — which is
 * exactly how this helper was written the first time.
 */
async function makeField(
  request: APIRequestContext,
  name: string,
  body: Record<string, unknown> = {},
): Promise<Probe> {
  const c = code(name);
  const { edit_tab: editTab, ...creatable } = body;
  const r = await request.post('/api/v1/fields', {
    data: {
      code: c,
      label: `FT ${name}`,
      type: 'text',
      subject_kind: 'asset',
      display_order: 9400,
      ...creatable,
    },
  });
  expect(r.status(), `create field ${c} → ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  createdFields.push(id);
  if (editTab !== undefined) {
    await patchField(request, id, { edit_tab: editTab });
  }
  return { id, code: c };
}

async function patchField(request: APIRequestContext, id: string, data: Record<string, unknown>) {
  const r = await request.patch(`/api/v1/fields/${id}`, { data });
  expect(r.ok(), `patch field → ${r.status()} ${await r.text()}`).toBeTruthy();
}

async function makeAsset(request: APIRequestContext, assetType = 2, ext = 'txt'): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`ft fixture ${Date.now()}-${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status()).toBe(201);
  const { hash } = (await up.json()) as { hash: string };
  const r = await request.post('/api/v1/assets', {
    data: {
      title: `FT probe ${Date.now()}-${Math.random()}`,
      asset_type: assetType,
      file_hash: hash,
      file_extension: ext,
    },
  });
  expect(r.status(), `create asset → ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  createdAssets.push(id);
  return id;
}

async function openAssetEdit(page: Page, assetId: string) {
  await page.goto(`/assets/${assetId}/edit`);
  await expect(page.locator(tid('asset-edit-form'))).toBeVisible();
  await expect(page.locator(tid('asset-fields-section'))).toBeVisible();
}

const strip = tid('asset-fields-tabs');
const tab = (id: string) => tid(`asset-fields-tabs-tab-${id}`);

test.describe.configure({ mode: 'serial' });

test.beforeEach(async ({ page }) => {
  await loginAsAdminViaUI(page);
});

// The per-test sweep. Fields FIRST, because a definition that still has
// values on a deleted asset is the leftover that breaks the next run.
test.afterEach(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  while (createdAssets.length > 0) {
    const id = createdAssets.pop()!;
    await request.delete(`/api/v1/assets/${id}?hard=true`).catch(() => undefined);
  }
  while (createdFields.length > 0) {
    const id = createdFields.pop()!;
    await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
  }
});

// Backstop for anything a crashed test left behind.
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
// A-4 / A-5 — a tab CHANGES COMPOSITION, and named + default is a strip
// ---------------------------------------------------------------------------

test('A-4/A-5: one named tab plus unassigned fields yields a STRIP, default FIRST, and switching MOVES what is rendered', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const loose = await makeField(request, 'a4_loose', { display_order: 9401 });
  const printed = await makeField(request, 'a4_print', {
    display_order: 9402,
    edit_tab: 'Print',
  });
  const assetId = await makeAsset(request);

  await openAssetEdit(page, assetId);

  // TWO BUCKETS, so a strip.
  await expect(page.locator(strip)).toBeVisible();
  await expect(page.locator(`${strip} [role="tab"]`)).toHaveCount(2);

  // DEFAULT FIRST, and selected.
  const first = page.locator(`${strip} [role="tab"]`).first();
  await expect(first).toHaveAttribute('aria-selected', 'true');
  await expect(page.locator(tab('default'))).toHaveAttribute('aria-selected', 'true');

  // ⛔ COMPOSITION, not styling: the unassigned field is IN THE DOM and
  // the tabbed one is NOT.
  await expect(page.locator(tid(`field-input-${loose.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-input-${printed.code}`))).toHaveCount(0);

  // Switching MOVES the fields.
  await page.locator(tab('Print')).click();
  await expect(page.locator(tid(`field-input-${printed.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-input-${loose.code}`))).toHaveCount(0);
  await expect(page.locator(tab('Print'))).toHaveAttribute('aria-selected', 'true');

  // And back, so the move is proved in both directions.
  await page.locator(tab('default')).click();
  await expect(page.locator(tid(`field-input-${loose.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-input-${printed.code}`))).toHaveCount(0);
});

test('two named tabs PLUS unassigned yields THREE buckets, in a deterministic order across reloads', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  // display_order decides the strip order, so "Zebra" is deliberately
  // given the LOWEST order: an alphabetical implementation would put it
  // last, and the operator's own ordering is what has to win.
  const loose = await makeField(request, 'a5_loose', { display_order: 9450 });
  const zebra = await makeField(request, 'a5_zebra', {
    display_order: 9410,
    edit_tab: 'Zebra',
  });
  const alpha = await makeField(request, 'a5_alpha', {
    display_order: 9420,
    edit_tab: 'Alpha',
  });
  const assetId = await makeAsset(request);

  await openAssetEdit(page, assetId);
  await expect(page.locator(`${strip} [role="tab"]`)).toHaveCount(3);

  const order = async () =>
    page.locator(`${strip} [role="tab"]`).evaluateAll((els) =>
      els.map((e) => e.getAttribute('data-testid')),
    );
  const expected = [
    'asset-fields-tabs-tab-default',
    'asset-fields-tabs-tab-Zebra',
    'asset-fields-tabs-tab-Alpha',
  ];
  expect(await order()).toEqual(expected);

  // DETERMINISTIC across reloads: the same strip, in the same order.
  await page.reload();
  await expect(page.locator(tid('asset-fields-section'))).toBeVisible();
  expect(await order()).toEqual(expected);

  // Each bucket holds its own member and nothing else.
  await expect(page.locator(tid(`field-input-${loose.code}`))).toBeVisible();
  await page.locator(tab('Zebra')).click();
  await expect(page.locator(tid(`field-input-${zebra.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-input-${alpha.code}`))).toHaveCount(0);
  await page.locator(tab('Alpha')).click();
  await expect(page.locator(tid(`field-input-${alpha.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-input-${zebra.code}`))).toHaveCount(0);
});

test('CLASS B — the no-tab floor: with every field unassigned there is NO strip and every control is reachable', async ({
  page,
  request,
}) => {
  // Passes on the sprint baseline, and that is the point: bucketing must
  // not add chrome to an install that never configured a tab.
  await loginAsAdminViaAPI(request);
  const a = await makeField(request, 'b2_a', { display_order: 9460 });
  const b = await makeField(request, 'b2_b', { display_order: 9461 });
  const assetId = await makeAsset(request);
  await openAssetEdit(page, assetId);
  await expect(page.locator(strip)).toHaveCount(0);
  await expect(page.locator(tid(`field-input-${a.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-input-${b.code}`))).toBeVisible();
});

test('naming ONE tab on a real install yields exactly TWO buckets, because the baseline fields are unassigned', async ({
  page,
  request,
}) => {
  // ⚠️ THE "ONE NAMED TAB, ONE BUCKET, NO STRIP" FLOOR IS NOT
  // REPRODUCIBLE HERE, and pretending otherwise is how a test starts
  // lying. A shipped install seeds ~28 field definitions and every one of
  // them is UNASSIGNED, so the default bucket always exists and naming
  // one tab necessarily makes two. That floor is a property of the
  // bucketing rule and is pinned where it can be stated honestly, in
  // web/src/lib/fieldTabs.test.ts.
  //
  // What IS worth asserting on a real install is the consequence: the
  // named field moves OUT of the default bucket, so nothing is lost and
  // nothing is duplicated.
  await loginAsAdminViaAPI(request);
  const only = await makeField(request, 'one_named', {
    display_order: 9470,
    edit_tab: 'Only',
  });
  const assetId = await makeAsset(request);
  await openAssetEdit(page, assetId);

  await expect(page.locator(`${strip} [role="tab"]`)).toHaveCount(2);
  await expect(page.locator(tab('default'))).toHaveAttribute('aria-selected', 'true');
  // It is NOT in the default bucket...
  await expect(page.locator(tid(`field-input-${only.code}`))).toHaveCount(0);
  // ...and it is reachable in its own, exactly once.
  await page.locator(tab('Only')).click();
  await expect(page.locator(tid(`field-input-${only.code}`))).toHaveCount(1);
});

// ---------------------------------------------------------------------------
// A-15 — POLICY B
// ---------------------------------------------------------------------------

test('A-15: a tab emptied by a condition KEEPS its chrome and its selection, shows an empty-state line, and its draft survives', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const ctrl = await makeField(request, 'b_ctrl', { display_order: 9481 });
  const only = await makeField(request, 'b_only', {
    display_order: 9482,
    edit_tab: 'Extras',
  });
  await patchField(request, only.id, { display_condition: [`${ctrl.code}=Commission`] });
  const assetId = await makeAsset(request);

  await openAssetEdit(page, assetId);
  await expect(page.locator(`${strip} [role="tab"]`)).toHaveCount(2);

  // Make the condition TRUE, switch into the tab, and type a draft.
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Commission');
  await page.locator(tab('Extras')).click();
  const onlyInput = page.locator(tid(`field-input-${only.code}`));
  await expect(onlyInput).toBeVisible();
  const DRAFT = 'typed inside the tab';
  await onlyInput.fill(DRAFT);

  // Now empty the tab out from the OTHER tab, which is the interaction
  // Policy B exists for.
  await page.locator(tab('default')).click();
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Personal');
  await page.locator(tab('Extras')).click();

  // ⛔ THE TAB IS STILL THERE, still selected, and says so.
  await expect(page.locator(`${strip} [role="tab"]`)).toHaveCount(2);
  await expect(page.locator(tab('Extras'))).toHaveAttribute('aria-selected', 'true');
  await expect(page.locator(tid('asset-fields-tab-empty'))).toBeVisible();
  await expect(onlyInput).toHaveCount(0);

  // Nothing hidden is submitted.
  const writes: string[] = [];
  page.on('request', (req) => {
    if (req.url().includes(`/fields/${only.id}`) && req.method() !== 'GET') {
      writes.push(req.method());
    }
  });
  await page.locator(tab('default')).click();
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  expect(writes, 'nothing hidden may be submitted').toEqual([]);

  // The DRAFT survives: bring the condition back and switch in.
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Commission');
  await page.locator(tab('Extras')).click();
  await expect(page.locator(tid(`field-input-${only.code}`))).toHaveValue(DRAFT);
});

// ---------------------------------------------------------------------------
// A-16, tab half — drafts survive A -> B -> A
// ---------------------------------------------------------------------------

test('A-16: a pending value survives tab switching A -> B -> A', async ({ page, request }) => {
  await loginAsAdminViaAPI(request);
  const loose = await makeField(request, 'sw_loose', { display_order: 9491 });
  const tabbed = await makeField(request, 'sw_tabbed', {
    display_order: 9492,
    edit_tab: 'Second',
  });
  const assetId = await makeAsset(request);

  await openAssetEdit(page, assetId);
  await page.locator(tid(`field-input-${loose.code}`)).fill('draft in A');
  await page.locator(tab('Second')).click();
  await expect(page.locator(tid(`field-input-${loose.code}`))).toHaveCount(0);
  await page.locator(tid(`field-input-${tabbed.code}`)).fill('draft in B');
  await page.locator(tab('default')).click();
  // A -> B -> A: the draft in A is still there, unsaved.
  await expect(page.locator(tid(`field-input-${loose.code}`))).toHaveValue('draft in A');
  await page.locator(tab('Second')).click();
  await expect(page.locator(tid(`field-input-${tabbed.code}`))).toHaveValue('draft in B');

  // And a save from either tab writes BOTH, because the model is one form
  // and a tab is a view of it.
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  const r = await request.get(`/api/v1/assets/${assetId}/fields`);
  const rows = (await r.json()) as Array<Record<string, unknown>>;
  expect(rows.find((v) => v.field_id === loose.id)?.value_text).toBe('draft in A');
  expect(rows.find((v) => v.field_id === tabbed.id)?.value_text).toBe('draft in B');
});

// ---------------------------------------------------------------------------
// The strip at a NARROW viewport
// ---------------------------------------------------------------------------

test('at 390px the strip stays a TABLIST and the page body does not scroll sideways', async ({
  page,
  request,
}) => {
  // Deliberately NOT the footer control's behaviour, which collapses to a
  // single pill and a menu below `sm`. On a form the tabs ARE the
  // structure, so they stay visible and scroll themselves.
  await loginAsAdminViaAPI(request);
  // The TAB name is capitalised and the field CODE is not: field codes
  // match ^[a-z][a-z0-9_]*$, so a code carrying the tab's own spelling is
  // a 400 rather than a fixture.
  for (const [i, name] of ['One', 'Two', 'Three', 'Four', 'Five'].entries()) {
    await makeField(request, `narrow_${name.toLowerCase()}`, {
      display_order: 9500 + i,
      edit_tab: name,
    });
  }
  await makeField(request, 'narrow_loose', { display_order: 9520 });
  const assetId = await makeAsset(request);

  await page.setViewportSize({ width: 390, height: 844 });
  await openAssetEdit(page, assetId);

  const tabs = page.locator(`${strip} [role="tab"]`);
  await expect(tabs).toHaveCount(6);
  // The tablist itself is present, not replaced by a menu.
  await expect(page.locator(`${strip}[role="tablist"]`)).toBeVisible();

  // The PAGE does not scroll horizontally; the strip does.
  const bodyOverflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(bodyOverflow, 'the page body must not scroll sideways because of the tab strip').toBeLessThanOrEqual(1);
  const stripScrolls = await page
    .locator(strip)
    .evaluate((el) => el.scrollWidth > el.clientWidth + 1);
  expect(stripScrolls, 'the strip itself is what scrolls at 390px').toBe(true);
});

// ---------------------------------------------------------------------------
// A-6 — /create NESTING, with N >= 2 asset types and a SHARED TAB NAME
// ---------------------------------------------------------------------------

test('A-6: /create nests tabs INSIDE the asset type, and two types naming the same tab do NOT merge', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  // Two asset types, and BOTH have a tab called "Print". Merging them
  // would file one type's field under the other type's section.
  const imgPrint = await makeField(request, 'c_img_print', {
    display_order: 9530,
    applies_to: [1],
    edit_tab: 'Print',
    display_group: 'core',
  });
  const imgLoose = await makeField(request, 'c_img_loose', {
    display_order: 9531,
    applies_to: [1],
  });
  const docPrint = await makeField(request, 'c_doc_print', {
    display_order: 9532,
    applies_to: [2],
    edit_tab: 'Print',
  });
  const docLoose = await makeField(request, 'c_doc_loose', {
    display_order: 9533,
    applies_to: [2],
  });

  await page.goto('/create');
  await expect(page.locator(tid('create-page'))).toBeVisible();

  // The drop CREATES ASSETS, so their ids are captured from the create
  // responses and swept with everything else. A spec that leaves rows on
  // a shared install is a spec that changes what the next one sees.
  page.on('response', (r) => {
    if (r.url().endsWith('/api/v1/assets') && r.request().method() === 'POST' && r.ok()) {
      void r
        .json()
        .then((b: { id?: string }) => {
          if (b.id) createdAssets.push(b.id);
        })
        .catch(() => undefined);
    }
  });

  // Drop one file of each kind, so the page really has TWO active asset
  // types (ref 1 = Image, ref 2 = Document). The server assigns the type;
  // the page ASKS rather than mirroring the extension table, which is
  // #1119's own rule, so both rows have to finish uploading before the
  // sections exist.
  await page.locator(tid('create-file-input')).setInputFiles([
    {
      name: `ft-${RUN}.png`,
      mimeType: 'image/png',
      // A 1x1 PNG, so the server assigns the image type for real.
      buffer: Buffer.from(
        'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
        'base64',
      ),
    },
    {
      name: `ft-${RUN}.txt`,
      mimeType: 'text/plain',
      buffer: Buffer.from(`ft create fixture ${Date.now()}-${Math.random()}`),
    },
  ]);

  // Both rows uploaded and typed before anything is asserted.
  await expect(page.locator(tid('create-file-row'))).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });

  // Open the fields disclosure.
  const details = page.getByTestId('create-fields');
  await expect(details).toBeVisible({ timeout: 30_000 });
  await details.locator('summary').click();

  // BOTH type sections exist, each with its OWN strip.
  const imgSection = page.getByTestId('create-fields-type-1');
  const docSection = page.getByTestId('create-fields-type-2');
  await expect(imgSection).toBeVisible({ timeout: 30_000 });
  await expect(docSection).toBeVisible();

  const imgTabs = page.getByTestId('create-fields-tabs-1');
  const docTabs = page.getByTestId('create-fields-tabs-2');
  await expect(imgTabs).toBeVisible();
  await expect(docTabs).toBeVisible();

  // ⛔ THE TABS DO NOT MERGE. Each type has its own "Print" tab, and
  // selecting one type's Print must not reveal the other type's field.
  await page.getByTestId('create-fields-tabs-1-tab-Print').click();
  await expect(page.getByTestId(`create-field-${imgPrint.code}`)).toBeVisible();
  await expect(page.getByTestId(`create-field-${docPrint.code}`)).toHaveCount(0);
  await expect(page.getByTestId(`create-field-${imgLoose.code}`)).toHaveCount(0);
  // The DOCUMENT section is still on its own default tab, untouched.
  await expect(page.getByTestId(`create-field-${docLoose.code}`)).toBeVisible();

  // Now the other way round.
  await page.getByTestId('create-fields-tabs-2-tab-Print').click();
  await expect(page.getByTestId(`create-field-${docPrint.code}`)).toBeVisible();
  await expect(page.getByTestId(`create-field-${docLoose.code}`)).toHaveCount(0);
  // And the IMAGE section did not move.
  await expect(page.getByTestId(`create-field-${imgPrint.code}`)).toBeVisible();

  // The image field lives INSIDE the image section in the DOM, which is
  // what "asset type is the outer axis" means structurally.
  await expect(imgSection.getByTestId(`create-field-${imgPrint.code}`)).toHaveCount(1);
  await expect(docSection.getByTestId(`create-field-${imgPrint.code}`)).toHaveCount(0);
});

// ---------------------------------------------------------------------------
// A-14 — THE ADMIN FIELD EDITOR ACTUALLY CONSUMES THE PROPERTY
// ---------------------------------------------------------------------------

// Row helpers for the condition editor. ONE CONTROL PER STORED ARRAY
// ELEMENT: there is no delimiter, so the test addresses rows by index the
// same way the editor does.
const condRow = (i: number) => tid(`field-edit-display-condition-term-${i}`);
const condRows = '[data-testid^="field-edit-display-condition-term-"]';
const condAdd = tid('field-edit-display-condition-add');

async function setCondition(page: Page, terms: string[], expectExisting: number) {
  // ⛔ WAIT FOR THE EDITOR FIRST. `count()` does NOT auto-wait, so calling
  // this straight after a reload read zero rows off a page that had not
  // rendered yet, removed nothing, and left the stored condition in place
  // while the caller believed it had been cleared. The caller states how
  // many rows it expects to find, which turns that race into a failed
  // assertion instead of a silent no-op.
  await expect(page.locator(tid('field-edit-display-condition'))).toBeVisible();
  await expect(page.locator(condRows)).toHaveCount(expectExisting);

  // Remove every existing row, then add exactly as many as we want. Rows
  // are removed from the END so the surviving indices do not shift under
  // the loop.
  for (let n = (await page.locator(condRows).count()) - 1; n >= 0; n--) {
    await page.locator(tid(`field-edit-display-condition-remove-${n}`)).click();
  }
  await expect(page.locator(condRows)).toHaveCount(0);
  for (let i = 0; i < terms.length; i++) {
    await page.locator(condAdd).click();
    await page.locator(condRow(i)).fill(terms[i]);
  }
}

test('A-14: the admin field editor loads, saves, replaces, clears and SURFACES SERVER ERRORS for display_condition', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const ctrlA = await makeField(request, 'ed_a', { display_order: 9550 });
  const ctrlB = await makeField(request, 'ed_b', { display_order: 9551 });
  const dep = await makeField(request, 'ed_dep', { display_order: 9552 });

  // 2. AN EXISTING CONDITION LOADS INTO THE EDITOR. Seeded through the
  //    API first, so "loads" is a real read and not an echo of something
  //    the editor just wrote.
  await patchField(request, dep.id, { display_condition: [`${ctrlA.code}=Commission`] });

  // 1. Open an ordinary, non-mirrored definition.
  await page.goto(`/admin/fields/${dep.code}`);
  await expect(page.locator(tid('field-edit-display-condition'))).toBeVisible();
  // ONE stored term is ONE row, holding it exactly.
  await expect(page.locator(condRows)).toHaveCount(1);
  await expect(page.locator(condRow(0))).toHaveValue(`${ctrlA.code}=Commission`);

  // 3 + 4. Set an N>=2 condition and save.
  const twoTerms = [`${ctrlA.code}=Commission`, `${ctrlB.code}~urgent`];
  await setCondition(page, twoTerms, 1);
  await page.locator(tid('field-options-save')).click();
  await expect(page.locator(tid('field-options-saved'))).toBeVisible();

  // 5 + 6. Reload; the EXACT condition survives, in canonical form, as
  //        TWO rows.
  await page.reload();
  await expect(page.locator(condRows)).toHaveCount(2);
  await expect(page.locator(condRow(0))).toHaveValue(twoTerms[0]);
  await expect(page.locator(condRow(1))).toHaveValue(twoTerms[1]);
  const after = await request.get(`/api/v1/fields/${dep.id}`);
  expect(((await after.json()) as { display_condition: string[] }).display_condition).toEqual(
    twoTerms,
  );

  // 7. REPLACE it, and prove WHOLE-ARRAY REPLACEMENT rather than a merge.
  //    The replacement is SHORTER, which is what makes the two
  //    distinguishable.
  await setCondition(page, [`${ctrlB.code}~urgent`], 2);
  await page.locator(tid('field-options-save')).click();
  await expect(page.locator(tid('field-options-saved'))).toBeVisible();
  const replaced = await request.get(`/api/v1/fields/${dep.id}`);
  expect(
    ((await replaced.json()) as { display_condition: string[] }).display_condition,
    'saving a shorter condition must REPLACE the array, not merge into it',
  ).toEqual([`${ctrlB.code}~urgent`]);

  // 8 + 9. CLEAR it through the editor, reload, and prove the canonical
  //    unset: the member is ABSENT, not an empty array.
  await page.reload();
  await setCondition(page, [], 1);
  await page.locator(tid('field-options-save')).click();
  await expect(page.locator(tid('field-options-saved'))).toBeVisible();
  const cleared = await request.get(`/api/v1/fields/${dep.id}`);
  const clearedBody = (await cleared.json()) as { display_condition?: string[] };
  expect(clearedBody.display_condition).toBeUndefined();
  await page.reload();
  await expect(page.locator(condRows)).toHaveCount(0);
  await expect(page.locator(tid('field-edit-display-condition-none'))).toBeVisible();

  // 10. SERVER VALIDATION ERRORS ARE SURFACED, not silently treated as
  //     success. A self-reference is refused with a sentence.
  await setCondition(page, [`${dep.code}=loop`], 0);
  await page.locator(tid('field-options-save')).click();
  const err = page.locator(tid('field-options-error'));
  await expect(err).toBeVisible();
  await expect(err).toContainText('itself');
  await expect(page.locator(tid('field-options-saved'))).toHaveCount(0);
  // ...and nothing was stored.
  const stillClear = await request.get(`/api/v1/fields/${dep.id}`);
  expect(
    ((await stillClear.json()) as { display_condition?: string[] }).display_condition,
  ).toBeUndefined();
});

test('⛔ A-14 DISCRIMINATOR: a term whose VALUE contains a newline is ONE row, and round-trips unchanged', async ({
  page,
  request,
}) => {
  // THE CASE A LINE-DELIMITED EDITOR GETS WRONG, and the reason this
  // control maps one UI item to one array element.
  //
  // `facet.SplitFieldTerm` ends with
  //     value = strings.TrimSpace(rest[len(candidate):])
  // and `TrimSpace` strips only the ENDS. `validFieldCode` constrains
  // only the code, and 00065's CHECK asks only for a non-empty string. So
  // `notes~line one\nline two` is a VALID SINGLE TERM whose parsed value
  // contains a newline.
  //
  // A textarea holding one term per LINE loads that stored term as two
  // visual lines and saves it back as `["notes~line one", "line two"]`,
  // whose second element has no operator at all. That is a lossy round
  // trip on a legal configuration.
  await loginAsAdminViaAPI(request);
  const ctrl = await makeField(request, 'nl_ctrl', { display_order: 9570, type: 'longtext' });
  const dep = await makeField(request, 'nl_dep', { display_order: 9571 });

  const MULTILINE = `${ctrl.code}~line one
line two`;
  const canonical = [MULTILINE];

  // The server accepts it, which is the premise this whole test rests on.
  await patchField(request, dep.id, { display_condition: canonical });
  const stored = await request.get(`/api/v1/fields/${dep.id}`);
  expect(
    ((await stored.json()) as { display_condition: string[] }).display_condition,
    'the server stores an embedded newline verbatim; if this fails the premise is wrong, not the editor',
  ).toEqual(canonical);

  await page.goto(`/admin/fields/${dep.code}`);
  await expect(page.locator(tid('field-edit-display-condition'))).toBeVisible();

  // ⛔ ONE ROW, NOT TWO. This is the assertion the previous design fails.
  await expect(
    page.locator(condRows),
    'a term whose value contains a newline is ONE array element and must render as ONE control',
  ).toHaveCount(1);
  // ...holding the WHOLE term, newline included.
  await expect(page.locator(condRow(0))).toHaveValue(MULTILINE);

  // SAVE WITHOUT SEMANTIC CHANGE: touch an unrelated property, so the
  // condition rides along untouched through the same submit path.
  await page.locator(tid('field-edit-label')).fill('NL round trip');
  await page.locator(tid('field-options-save')).click();
  await expect(page.locator(tid('field-options-saved'))).toBeVisible();

  // RELOAD, and the stored array is ELEMENT-EQUIVALENT to the original.
  const after = await request.get(`/api/v1/fields/${dep.id}`);
  const afterCond = ((await after.json()) as { display_condition: string[] }).display_condition;
  expect(afterCond, 'the stored condition must survive an editor round trip unchanged').toEqual(
    canonical,
  );
  expect(afterCond).toHaveLength(1);
  expect(afterCond[0]).toContain('\n');

  await page.reload();
  await expect(page.locator(condRows)).toHaveCount(1);
  await expect(page.locator(condRow(0))).toHaveValue(MULTILINE);

  // And the editor can EDIT one: adding a second ordinary term beside it
  // leaves the multiline one intact rather than splitting it.
  await page.locator(condAdd).click();
  await page.locator(condRow(1)).fill(`${ctrl.code}~second`);
  await page.locator(tid('field-options-save')).click();
  await expect(page.locator(tid('field-options-saved'))).toBeVisible();
  const two = await request.get(`/api/v1/fields/${dep.id}`);
  expect(((await two.json()) as { display_condition: string[] }).display_condition).toEqual([
    MULTILINE,
    `${ctrl.code}~second`,
  ]);
});

test('A-14 boundary: a MIRRORED definition is offered no condition control at all', async ({
  page,
}) => {
  // `title` mirrors a column of the asset, which has a second human write
  // plane, so it cannot carry a condition. The server refuses one; the
  // editor renders no control, so an operator is not refused after typing.
  await page.goto('/admin/fields/title');
  await expect(page.locator(tid('field-options-save'))).toBeVisible();
  await expect(page.locator(tid('field-edit-display-condition'))).toHaveCount(0);
});

// ---------------------------------------------------------------------------
// THE FULL /create HIERARCHY: asset type -> tab bucket -> display_group
// ---------------------------------------------------------------------------

test('/create nests display_group fieldsets INSIDE the selected tab, inside the asset type', async ({
  page,
  request,
}) => {
  // ⛔ THIS IS THE LAYER /create WAS MISSING, and the assertion is written
  // to say so. The page rendered each bucket's fields as ONE FLAT LIST:
  // `display_group` came back from the server in the right ORDER and
  // structured nothing, so an operator who split a type's fields between
  // "core" and "rights" saw one undifferentiated run of inputs while the
  // asset edit page drew two labelled fieldsets from the same
  // definitions.
  //
  // Checking the server's ORDERING would not distinguish the two builds,
  // because the ordering was already correct. What distinguishes them is
  // the FIELDSET: `create-fields-group-{type}-{group}` does not exist on
  // the previous head, under any name, so every group assertion below
  // fails there by locating an element that is not in the DOM.
  await loginAsAdminViaAPI(request);

  // ⚠️ RUN-UNIQUE GROUP NAMES. The seeded corpus already uses `core`,
  // `general`, `rights` and `technical`, and every seeded definition is
  // UNASSIGNED, so it all lands in the default bucket. Naming a fixture
  // group `rights` would make "the rights fieldset is absent from the
  // default bucket" false for reasons that have nothing to do with this
  // page. These names collide with nothing.
  const GA = `ga${RUN}`;
  const GB = `gb${RUN}`;

  // Type 1 (Image): one tab, TWO display_groups inside it.
  const imgCore = await makeField(request, 'g_img_core', {
    display_order: 9560,
    applies_to: [1],
    edit_tab: 'Print',
    display_group: GA,
  });
  const imgRights = await makeField(request, 'g_img_rights', {
    display_order: 9561,
    applies_to: [1],
    edit_tab: 'Print',
    display_group: GB,
  });
  const imgCore2 = await makeField(request, 'g_img_core2', {
    display_order: 9562,
    applies_to: [1],
    edit_tab: 'Print',
    display_group: GA,
  });
  // ...and an unassigned field, so type 1 has TWO buckets and tab
  // navigation is a real navigation rather than one segment.
  const imgLoose = await makeField(request, 'g_img_loose', {
    display_order: 9563,
    applies_to: [1],
    display_group: GA,
  });
  // Type 2 (Document): its OWN "Print" tab and its own "core" group, both
  // sharing a NAME with type 1's. Neither may merge.
  const docPrint = await makeField(request, 'g_doc_print', {
    display_order: 9564,
    applies_to: [2],
    edit_tab: 'Print',
    display_group: GA,
  });
  const docLoose = await makeField(request, 'g_doc_loose', {
    display_order: 9565,
    applies_to: [2],
    display_group: GA,
  });

  await page.goto('/create');
  await expect(page.locator(tid('create-page'))).toBeVisible();
  page.on('response', (r) => {
    if (r.url().endsWith('/api/v1/assets') && r.request().method() === 'POST' && r.ok()) {
      void r
        .json()
        .then((b: { id?: string }) => {
          if (b.id) createdAssets.push(b.id);
        })
        .catch(() => undefined);
    }
  });
  await page.locator(tid('create-file-input')).setInputFiles([
    {
      name: `ftg-${RUN}.png`,
      mimeType: 'image/png',
      buffer: Buffer.from(
        'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
        'base64',
      ),
    },
    {
      name: `ftg-${RUN}.txt`,
      mimeType: 'text/plain',
      buffer: Buffer.from(`ftg create fixture ${Date.now()}-${Math.random()}`),
    },
  ]);
  await expect(page.locator(tid('create-file-row'))).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });
  await page.getByTestId('create-fields').locator('summary').click();

  const imgSection = page.getByTestId('create-fields-type-1');
  const docSection = page.getByTestId('create-fields-type-2');
  await expect(imgSection).toBeVisible({ timeout: 30_000 });
  await expect(docSection).toBeVisible();

  const imgCoreGroup = page.getByTestId(`create-fields-group-1-${GA}`);
  const imgRightsGroup = page.getByTestId(`create-fields-group-1-${GB}`);

  // The DEFAULT bucket is selected, so only the unassigned field is
  // drawn, in its own `core` fieldset. `rights` lives only in the Print
  // bucket and must not be rendered yet.
  await expect(imgCoreGroup).toBeVisible();
  await expect(imgCoreGroup.getByTestId(`create-field-${imgLoose.code}`)).toHaveCount(1);
  await expect(imgRightsGroup).toHaveCount(0);

  // TAB NAVIGATION CHANGES THE RENDERED BUCKET.
  await page.getByTestId('create-fields-tabs-1-tab-Print').click();
  await expect(imgSection.getByTestId(`create-field-${imgLoose.code}`)).toHaveCount(0);

  // ⛔ AND THE BUCKET HOLDS TWO DISTINCT display_group FIELDSETS.
  await expect(imgCoreGroup).toBeVisible();
  await expect(imgRightsGroup).toBeVisible();
  await expect(imgCoreGroup.locator('legend')).toHaveText(GA);
  await expect(imgRightsGroup.locator('legend')).toHaveText(GB);

  // Each field is in the RIGHT fieldset, and in exactly one.
  await expect(imgCoreGroup.getByTestId(`create-field-${imgCore.code}`)).toHaveCount(1);
  await expect(imgCoreGroup.getByTestId(`create-field-${imgCore2.code}`)).toHaveCount(1);
  await expect(imgCoreGroup.getByTestId(`create-field-${imgRights.code}`)).toHaveCount(0);
  await expect(imgRightsGroup.getByTestId(`create-field-${imgRights.code}`)).toHaveCount(1);
  await expect(imgRightsGroup.getByTestId(`create-field-${imgCore.code}`)).toHaveCount(0);

  // ORDERING IS PINNED WITHIN THE GROUP: display_order decides, so the
  // two `core` members appear in 9560, 9562 order and not in DOM-accident
  // order.
  const coreOrder = await imgCoreGroup
    .locator('[data-testid^="create-field-"]')
    .evaluateAll((els) => els.map((e) => e.getAttribute('data-testid')));
  expect(coreOrder).toEqual([`create-field-${imgCore.code}`, `create-field-${imgCore2.code}`]);

  // AND THE GROUPS THEMSELVES are in first-appearance order, which is the
  // server's `display_group, display_order` ordering.
  const groupOrder = (
    await imgSection
      .locator('[data-testid^="create-fields-group-"]')
      .evaluateAll((els) => els.map((e) => e.getAttribute('data-testid') ?? ''))
  ).filter((id) => id.endsWith(GA) || id.endsWith(GB));
  expect(groupOrder).toEqual([
    `create-fields-group-1-${GA}`,
    `create-fields-group-1-${GB}`,
  ]);

  // ⛔ NOTHING CROSSED THE ASSET TYPE. Type 2 has a "Print" tab and a
  // "core" group of its own; selecting type 1's Print must not have
  // touched either, and type 2's `core` fieldset must hold only type 2's
  // field.
  const docCoreGroup = page.getByTestId(`create-fields-group-2-${GA}`);
  await expect(docCoreGroup).toBeVisible();
  await expect(docCoreGroup.getByTestId(`create-field-${docLoose.code}`)).toHaveCount(1);
  await expect(docCoreGroup.getByTestId(`create-field-${imgCore.code}`)).toHaveCount(0);
  await expect(docSection.getByTestId(`create-field-${docPrint.code}`)).toHaveCount(0);
  await expect(docSection.getByTestId(`create-field-${imgCore.code}`)).toHaveCount(0);

  // Type 2's own tab still navigates independently.
  await page.getByTestId('create-fields-tabs-2-tab-Print').click();
  await expect(docCoreGroup.getByTestId(`create-field-${docPrint.code}`)).toHaveCount(1);
  await expect(docCoreGroup.getByTestId(`create-field-${docLoose.code}`)).toHaveCount(0);
  // ...and type 1 did not move when type 2's tab did.
  await expect(imgCoreGroup.getByTestId(`create-field-${imgCore.code}`)).toHaveCount(1);
  await expect(imgRightsGroup).toBeVisible();

  // AT 390px the same structure survives: the groups are still fieldsets
  // inside the selected bucket, and the page does not scroll sideways.
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(imgCoreGroup).toBeVisible();
  await expect(imgRightsGroup).toBeVisible();
  await expect(imgCoreGroup.getByTestId(`create-field-${imgCore.code}`)).toHaveCount(1);
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow, 'the create page must not scroll sideways at 390px').toBeLessThanOrEqual(1);
});
