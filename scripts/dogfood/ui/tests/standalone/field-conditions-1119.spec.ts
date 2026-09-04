// CONDITIONAL FIELD VISIBILITY, on the real edit surfaces (#1173 part 4,
// #1119, ADR 0099).
//
// # What was true before this file
//
// `display_condition` did not exist. `grep -rani display_condition app
// web scripts` returned ZERO on the sprint's baseline: no column, no API
// member, no symbol, no admin control, no ADR. So an operator could
// describe when a field should appear only by not creating the field.
//
// EVERY TEST IN THIS FILE FAILS ON THAT BUILD, and most of them fail
// while creating the fixture: `PATCH /fields/{id}` with a
// `display_condition` member is a body the server does not understand and
// a column that is not there. That is the point, and it is why none of
// these are preservation tests however preservation-flavoured they read.
//
// # The three things worth reading before changing anything here
//
//  1. ⛔ EVERY TEST PROVES THE DEPENDENT WAS RENDERED BEFORE IT WAS
//     HIDDEN. A test that only asserts absence passes against a form that
//     never drew the control at all, which is the failure mode a
//     conditional-visibility suite is most likely to ship with.
//
//  2. ⛔ THE CONTROLLER ACTUALLY CHANGES DURING THE TEST. Configuring a
//     condition that is false from the start and observing a missing
//     control proves nothing about the TRANSITION, and the transition is
//     the whole feature.
//
//  3. The WHOLE-CONDITION FAIL-OPEN cases are the ones a naive
//     implementation gets wrong. With term A FALSE and term B
//     unevaluable, "unknown counts as true inside the AND" evaluates
//     `false AND true` and still HIDES. See the two-term tests below.
//
// The UNREADABLE-controller arm of fail-open is proved at the API
// boundary in app/internal/metadata/field_composition_e2e_test.go, with
// real team-scoped grants. It is not reproducible here: the dogfood
// session is a system.admin, and `Identity.Can` short-circuits on that
// wildcard before any per-field gate is consulted, so no field this
// session can create is unreadable to it.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page } from '@playwright/test';
import { loginAsAdminViaAPI, loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

const RUN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;
const code = (name: string) => `fc_${name}_${RUN}`;

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
const createdCollections: string[] = [];

async function makeField(
  request: APIRequestContext,
  name: string,
  body: Record<string, unknown> = {},
): Promise<Probe> {
  const c = code(name);
  const r = await request.post('/api/v1/fields', {
    data: {
      code: c,
      label: `FC ${name}`,
      type: 'text',
      subject_kind: 'asset',
      display_order: 9300,
      ...body,
    },
  });
  expect(r.status(), `create field ${c} → ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  createdFields.push(id);
  // ⛔ KEEP THE FIXTURE OFF THE ADVANCED SEARCH FORM.
  //
  // A field definition is GLOBAL, and `show_in_advanced_search` defaults
  // TRUE, so every probe this file creates appears as a filter control on
  // /search for as long as it exists. field-participation-1173 snapshots
  // that page's whole control list, changes one flag, and asserts the
  // rest of the list is IDENTICAL — so a fixture of ours created between
  // its two snapshots reds a spec that has nothing to do with this one.
  // Measured, not theorised: `fc_a4_a_*` and `fc_a4_dep_*` turned up in
  // its diff under two workers.
  //
  // It is a PATCH because `show_in_advanced_search` is update-only, like
  // `edit_tab`: FieldDefinitionCreate carries none of the participation
  // properties.
  await patchField(request, id, { show_in_advanced_search: false });
  return { id, code: c };
}

async function patchField(request: APIRequestContext, id: string, data: Record<string, unknown>) {
  const r = await request.patch(`/api/v1/fields/${id}`, { data });
  expect(r.ok(), `patch field → ${r.status()} ${await r.text()}`).toBeTruthy();
}

async function makeAsset(request: APIRequestContext): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    // Novel bytes: storage is content-addressed, so fixed content would
    // dedupe onto an EXISTING asset another spec owns.
    data: Buffer.from(`fc fixture ${Date.now()}-${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status()).toBe(201);
  const { hash } = (await up.json()) as { hash: string };
  const r = await request.post('/api/v1/assets', {
    data: {
      title: `FC probe ${Date.now()}-${Math.random()}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: 'txt',
    },
  });
  expect(r.status(), `create asset → ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  createdAssets.push(id);
  return id;
}

async function makeCollection(request: APIRequestContext): Promise<string> {
  const r = await request.post('/api/v1/collections', {
    data: { name: `FC collection ${RUN}-${Math.random().toString(36).slice(2, 8)}` },
  });
  expect(r.status(), `create collection → ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  createdCollections.push(id);
  return id;
}

/** What the SERVER holds, read back rather than scraped off the page. */
async function storedValue(
  request: APIRequestContext,
  subject: 'assets' | 'collections',
  subjectId: string,
  fieldId: string,
): Promise<Record<string, unknown> | undefined> {
  const r = await request.get(`/api/v1/${subject}/${subjectId}/fields`);
  expect(r.ok()).toBeTruthy();
  const rows = (await r.json()) as Array<Record<string, unknown>>;
  return rows.find((v) => v.field_id === fieldId);
}

async function openAssetEdit(page: Page, assetId: string) {
  await page.goto(`/assets/${assetId}/edit`);
  await expect(page.locator(tid('asset-edit-form'))).toBeVisible();
  await expect(page.locator(tid('asset-fields-section'))).toBeVisible();
}

async function openCollectionEdit(page: Page, collectionId: string) {
  await page.goto(`/collections/${collectionId}`);
  await page.getByTestId('collection-detail-more-button').click();
  await page.getByTestId('collection-detail-edit-menuitem').click();
  await expect(page.getByTestId('collection-fields-section')).toBeVisible();
}

test.describe.configure({ mode: 'serial' });

test.beforeEach(async ({ page }) => {
  await loginAsAdminViaUI(page);
});

// The per-test sweep. Fields FIRST, because a definition that still has
// values on a deleted asset is the leftover that breaks the next run.
test.afterEach(async ({ request }) => {
  await loginAsAdminViaAPI(request);
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
  for (const id of createdCollections) {
    await request.delete(`/api/v1/collections/${id}?hard=true`).catch(() => undefined);
  }
  for (const id of createdFields) {
    await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
  }
});

// ---------------------------------------------------------------------------
// A-2 — N=1, BOTH ARMS, on both edit surfaces, holding across a reload
// ---------------------------------------------------------------------------

test('ASSET EDIT: a dependent hides when its controller stops matching, reveals when it matches again, and the verdict survives a reload', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const ctrl = await makeField(request, 'a2_ctrl');
  const dep = await makeField(request, 'a2_dep');
  await patchField(request, dep.id, { display_condition: [`${ctrl.code}=Commission`] });
  const assetId = await makeAsset(request);

  await openAssetEdit(page, assetId);

  const ctrlInput = page.locator(tid(`field-input-${ctrl.code}`));
  const depInput = page.locator(tid(`field-input-${dep.code}`));

  // ⛔ ANTI-VACUITY, part 1. The controller is empty, so the condition is
  // FALSE and the dependent must be ABSENT. Asserting only this would
  // pass on a form that never drew the control, which is why part 2
  // follows immediately.
  await expect(ctrlInput).toBeVisible();
  await expect(depInput).toHaveCount(0);

  // TRUE ARM: the controller CHANGES and the dependent APPEARS. This is
  // the transition, and it is what makes the absence above meaningful.
  await ctrlInput.fill('Commission');
  await expect(depInput).toBeVisible();

  // FALSE ARM, from a state where the control was demonstrably rendered.
  await ctrlInput.fill('Personal');
  await expect(depInput).toHaveCount(0);

  // ...and back, so the transition is proved in both directions within
  // one form.
  await ctrlInput.fill('Commission');
  await expect(depInput).toBeVisible();

  // The verdict SURVIVES A RELOAD, which is the difference between a
  // rendering rule and a piece of component state: save the controller,
  // reload, and the dependent is still drawn from the PERSISTED value.
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  await page.reload();
  await expect(page.locator(tid(`field-input-${dep.code}`))).toBeVisible();

  // And the other way: persist a non-matching controller and reload.
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Personal');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  await page.reload();
  await expect(page.locator(tid('asset-fields-section'))).toBeVisible();
  await expect(page.locator(tid(`field-input-${dep.code}`))).toHaveCount(0);
});

test('COLLECTION EDIT: the same both-arm transition, on the other subject kind', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const ctrl = await makeField(request, 'a2c_ctrl', { subject_kind: 'collection' });
  const dep = await makeField(request, 'a2c_dep', { subject_kind: 'collection' });
  await patchField(request, dep.id, { display_condition: [`${ctrl.code}=Commission`] });
  const collectionId = await makeCollection(request);

  await openCollectionEdit(page, collectionId);

  const ctrlInput = page.getByTestId(`field-input-${ctrl.code}`);
  const depInput = page.getByTestId(`field-input-${dep.code}`);

  await expect(ctrlInput).toBeVisible();
  await expect(depInput).toHaveCount(0);
  await ctrlInput.fill('Commission');
  await expect(depInput).toBeVisible();
  await ctrlInput.fill('Personal');
  await expect(depInput).toHaveCount(0);
});

// ---------------------------------------------------------------------------
// A-3 — the N>=2 cardinality discriminators
// ---------------------------------------------------------------------------

test('N>=2 conjunction: both true SHOWS, either false HIDES', async ({ page, request }) => {
  await loginAsAdminViaAPI(request);
  const a = await makeField(request, 'a3_a');
  const b = await makeField(request, 'a3_b');
  const dep = await makeField(request, 'a3_dep');
  await patchField(request, dep.id, {
    display_condition: [`${a.code}=yes`, `${b.code}=yes`],
  });
  const assetId = await makeAsset(request);
  await openAssetEdit(page, assetId);

  const ai = page.locator(tid(`field-input-${a.code}`));
  const bi = page.locator(tid(`field-input-${b.code}`));
  const di = page.locator(tid(`field-input-${dep.code}`));

  // case 1 — A true, B true -> SHOWN
  await ai.fill('yes');
  await bi.fill('yes');
  await expect(di).toBeVisible();

  // case 2 — A true, B false -> HIDDEN
  await bi.fill('no');
  await expect(di).toHaveCount(0);

  // case 3 — A false, B true -> HIDDEN. The conjunction is
  // order-independent, so both asymmetric cases are asserted.
  await ai.fill('no');
  await bi.fill('yes');
  await expect(di).toHaveCount(0);

  // case 6 — A READABLE BUT UNSET, B true -> HIDDEN. The mirror trap:
  // absence is a real FALSE, not "unknown". An evaluator that treated
  // every empty controller as unknown would SHOW the dependent here and
  // would never hide anything.
  await ai.fill('');
  await bi.fill('yes');
  await expect(di).toHaveCount(0);
});

test('⛔ WHOLE-CONDITION FAIL-OPEN: one term FALSE and one term UNRESOLVABLE SHOWS the field', async ({
  page,
  request,
}) => {
  // Cardinality case 4, and the case a naive implementation gets wrong.
  // "Unknown counts as true inside the AND" would evaluate `false AND
  // true` here and HIDE the dependent. The rule is that once any term is
  // unevaluable the condition has NO VERDICT and the rest is not
  // consulted.
  await loginAsAdminViaAPI(request);
  const a = await makeField(request, 'a4_a');
  const gone = await makeField(request, 'a4_gone');
  const dep = await makeField(request, 'a4_dep');
  await patchField(request, dep.id, {
    display_condition: [`${a.code}=yes`, `${gone.code}=yes`],
  });
  const assetId = await makeAsset(request);

  // ⛔ ANTI-VACUITY: prove the condition EVALUATES NORMALLY first, with
  // BOTH terms resolvable. Without this the fail-open below could be
  // caused by the condition never having worked at all.
  await openAssetEdit(page, assetId);
  await page.locator(tid(`field-input-${a.code}`)).fill('yes');
  await page.locator(tid(`field-input-${gone.code}`)).fill('yes');
  await expect(page.locator(tid(`field-input-${dep.code}`))).toBeVisible();
  await page.locator(tid(`field-input-${a.code}`)).fill('no');
  await expect(page.locator(tid(`field-input-${dep.code}`))).toHaveCount(0);

  // Now make ONE term unresolvable by archiving its controller. The
  // stored condition is NOT rewritten by that (ADR 0099 §7), which the Go
  // suite asserts against the column directly.
  await patchField(request, gone.id, { status: 'archived' });

  await page.reload();
  await expect(page.locator(tid('asset-fields-section'))).toBeVisible();
  const a2 = page.locator(tid(`field-input-${a.code}`));
  await expect(a2).toBeVisible();

  // A is FALSE (empty) and the other term is unevaluable. The dependent
  // must be SHOWN.
  await a2.fill('no');
  await expect(page.locator(tid(`field-input-${dep.code}`))).toBeVisible();

  // And it stays shown when A is true, so this is fail-open and not a
  // coincidence of A's value.
  await a2.fill('yes');
  await expect(page.locator(tid(`field-input-${dep.code}`))).toBeVisible();
});

// ---------------------------------------------------------------------------
// The operator/type spread, on a real form
// ---------------------------------------------------------------------------

test('MIXED controller types drive one dependent: text =, longtext ~, select slug =, multi_select membership', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const txt = await makeField(request, 'mx_txt');
  const long = await makeField(request, 'mx_long', { type: 'longtext' });
  const sel = await makeField(request, 'mx_sel', {
    type: 'select',
    options: { values: [{ value: 'fan_art', label: 'Fan Art' }, { value: 'original' }] },
  });
  const multi = await makeField(request, 'mx_multi', {
    type: 'multi_select',
    options: { values: [{ value: 'writer' }, { value: 'illustrator' }] },
  });
  const dep = await makeField(request, 'mx_dep');
  await patchField(request, dep.id, {
    display_condition: [
      `${txt.code}=Commission`,
      `${long.code}~urgent`,
      `${sel.code}=fan_art`,
      `${multi.code}=illustrator`,
    ],
  });
  const assetId = await makeAsset(request);

  // The multi_select's MEMBERSHIP term is satisfied from the API rather
  // than by driving the combobox. What this test is about is the
  // EVALUATION of four different controller types against one dependent;
  // the combobox's own interaction is already covered by
  // asset-field-edit-1119.spec.ts, and reproducing its chip handling here
  // would make a condition test fail for reasons that are not about
  // conditions.
  const seeded = await request.put(`/api/v1/assets/${assetId}/fields/${multi.id}`, {
    data: { value_options: ['illustrator'], if_absent: true },
  });
  expect(seeded.ok(), `seed multi → ${seeded.status()} ${await seeded.text()}`).toBeTruthy();

  await openAssetEdit(page, assetId);

  const di = page.locator(tid(`field-input-${dep.code}`));
  await expect(di).toHaveCount(0);

  await page.locator(tid(`field-input-${txt.code}`)).fill('Commission');
  await expect(di).toHaveCount(0);
  // `~` is a CASE-INSENSITIVE SUBSTRING, so this matches `urgent`.
  await page.locator(tid(`field-input-${long.code}`)).fill('Please treat as URGENT');
  await expect(di).toHaveCount(0);
  await page.locator(tid(`field-input-${sel.code}`)).selectOption('fan_art');

  // All four terms are now true, including the membership one.
  await expect(di).toBeVisible();

  // And the SELECT term is a real conjunct: taking it away hides again.
  await page.locator(tid(`field-input-${sel.code}`)).selectOption('original');
  await expect(di).toHaveCount(0);
  await page.locator(tid(`field-input-${sel.code}`)).selectOption('fan_art');
  await expect(di).toBeVisible();

  // `=` on TEXT is CASE-SENSITIVE. Lowercasing the controller must hide
  // the dependent again, which is what separates `=` from `~` on a real
  // form rather than only in a unit test.
  await page.locator(tid(`field-input-${txt.code}`)).fill('commission');
  await expect(di).toHaveCount(0);
});

test('a controller HIDDEN BY ITS OWN CONDITION still contributes its value', async ({
  page,
  request,
}) => {
  // Visibility is a rendering decision and does not remove a value from
  // the model. An implementation that skipped hidden controllers when
  // resolving terms would collapse every chain at its first hidden link.
  await loginAsAdminViaAPI(request);
  const root = await makeField(request, 'ch_root');
  const mid = await makeField(request, 'ch_mid');
  const leaf = await makeField(request, 'ch_leaf');
  await patchField(request, mid.id, { display_condition: [`${root.code}=show`] });
  await patchField(request, leaf.id, { display_condition: [`${mid.code}=go`] });
  const assetId = await makeAsset(request);

  // Seed MID's value through the API, so it is genuinely stored while the
  // control for it may or may not be drawn.
  const r = await request.put(`/api/v1/assets/${assetId}/fields/${mid.id}`, {
    data: { value_text: 'go', if_absent: true },
  });
  expect(r.ok(), `seed mid → ${r.status()} ${await r.text()}`).toBeTruthy();

  await openAssetEdit(page, assetId);

  // ROOT is empty, so MID is HIDDEN. LEAF must still be SHOWN, because
  // MID's stored value satisfies LEAF's condition.
  await expect(page.locator(tid(`field-input-${mid.code}`))).toHaveCount(0);
  await expect(page.locator(tid(`field-input-${leaf.code}`))).toBeVisible();

  // And when MID is revealed, nothing changes for LEAF: the value was
  // always contributing.
  await page.locator(tid(`field-input-${root.code}`)).fill('show');
  await expect(page.locator(tid(`field-input-${mid.code}`))).toBeVisible();
  await expect(page.locator(tid(`field-input-${leaf.code}`))).toBeVisible();
});

// ---------------------------------------------------------------------------
// A-17 — THE STORED VALUE SURVIVES HIDING
// ---------------------------------------------------------------------------

for (const subject of ['assets', 'collections'] as const) {
  test(`A-17 on ${subject}: a hidden dependent emits NO write, its stored value is byte-identical, and reveal restores it`, async ({
    page,
    request,
  }) => {
    await loginAsAdminViaAPI(request);
    const isColl = subject === 'collections';
    const kind = isColl ? { subject_kind: 'collection' } : {};
    const ctrl = await makeField(request, `a17_${subject}_ctrl`, kind);
    const dep = await makeField(request, `a17_${subject}_dep`, kind);
    const other = await makeField(request, `a17_${subject}_other`, kind);
    await patchField(request, dep.id, { display_condition: [`${ctrl.code}=Commission`] });

    const subjectId = isColl ? await makeCollection(request) : await makeAsset(request);
    const base = `/api/v1/${subject}/${subjectId}/fields`;

    // 1. The dependent starts VISIBLE with a REAL STORED VALUE. Both
    //    halves are seeded through the API so the starting state does not
    //    depend on the form under test.
    const PRESERVED = '  A value with edges  ';
    for (const [fid, value] of [
      [ctrl.id, 'Commission'],
      [dep.id, PRESERVED],
    ] as const) {
      const r = await request.put(`${base}/${fid}`, {
        data: { value_text: value, if_absent: true },
      });
      expect(r.ok(), `seed → ${r.status()} ${await r.text()}`).toBeTruthy();
    }

    const open = isColl
      ? () => openCollectionEdit(page, subjectId)
      : () => openAssetEdit(page, subjectId);
    const saveTid = isColl ? 'collection-fields-save' : 'asset-fields-save';
    const savedTid = isColl ? 'collection-fields-saved' : 'asset-fields-saved';

    await open();
    // ⛔ ANTI-VACUITY: it really is drawn, and really does hold the value.
    const depInput = page.getByTestId(`field-input-${dep.code}`);
    await expect(depInput).toBeVisible();
    await expect(depInput).toHaveValue(PRESERVED);

    // 2 + 3. The controller changes and the dependent BECOMES HIDDEN.
    await page.getByTestId(`field-input-${ctrl.code}`).fill('Personal');
    await expect(page.getByTestId(`field-input-${dep.code}`)).toHaveCount(0);

    // 4 + 5. Save an unrelated field, completing the real save flow, and
    //    watch every field-value request the page makes. The hidden
    //    dependent must emit NO Set, NO Clear and no empty row.
    const depRequests: string[] = [];
    page.on('request', (req) => {
      if (req.url().includes(`/fields/${dep.id}`) && req.method() !== 'GET') {
        depRequests.push(`${req.method()} ${req.url()}`);
      }
    });
    await page.getByTestId(`field-input-${other.code}`).fill('an unrelated edit');
    await page.getByTestId(saveTid).click();
    await expect(page.getByTestId(savedTid)).toBeVisible();
    expect(
      depRequests,
      'a hidden field must emit no write of any kind while it is hidden',
    ).toEqual([]);

    // 6 + 7. Reload and read the SERVER: the persisted value is
    //    BYTE-IDENTICAL, leading and trailing spaces included. A trim
    //    anywhere in the save path would show up here.
    const stored = await storedValue(request, subject, subjectId, dep.id);
    expect(stored?.value_text).toBe(PRESERVED);

    // 8 + 9. Make the condition true again: the ORIGINAL PERSISTED VALUE
    //    REAPPEARS, rather than a blank control the next save would write.
    await open();
    await page.getByTestId(`field-input-${ctrl.code}`).fill('Commission');
    const revealed = page.getByTestId(`field-input-${dep.code}`);
    await expect(revealed).toBeVisible();
    await expect(revealed).toHaveValue(PRESERVED);
  });
}

// ---------------------------------------------------------------------------
// A-16 — DRAFTS SURVIVE
// ---------------------------------------------------------------------------

test("A-16: a hidden dependent's unsaved DRAFT survives the hide and reappears on reveal, and is not submitted while hidden", async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const ctrl = await makeField(request, 'a16_ctrl');
  const dep = await makeField(request, 'a16_dep');
  const other = await makeField(request, 'a16_other');
  await patchField(request, dep.id, { display_condition: [`${ctrl.code}=Commission`] });
  const assetId = await makeAsset(request);

  await openAssetEdit(page, assetId);
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Commission');

  const depInput = page.locator(tid(`field-input-${dep.code}`));
  await expect(depInput).toBeVisible();
  const DRAFT = 'typed but never saved';
  await depInput.fill(DRAFT);

  // Hide it. The draft is NOT submitted and NOT destroyed.
  const depWrites: string[] = [];
  page.on('request', (req) => {
    if (req.url().includes(`/fields/${dep.id}`) && req.method() !== 'GET') {
      depWrites.push(`${req.method()} ${req.url()}`);
    }
  });
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Personal');
  await expect(page.locator(tid(`field-input-${dep.code}`))).toHaveCount(0);

  await page.locator(tid(`field-input-${other.code}`)).fill('something else');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  expect(depWrites, 'a hidden draft must not be submitted').toEqual([]);

  // REVEAL, before any navigation or reload: the draft is still there.
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Commission');
  const back = page.locator(tid(`field-input-${dep.code}`));
  await expect(back).toBeVisible();
  await expect(back).toHaveValue(DRAFT);

  // Nothing was written to the server while it was hidden.
  const stored = await storedValue(request, 'assets', assetId, dep.id);
  expect(stored, 'the hidden draft must not have reached the server').toBeUndefined();
});

// ---------------------------------------------------------------------------
// A-18 — HIDDEN + REQUIRED INVENTS NO COMPLETENESS GATE
// ---------------------------------------------------------------------------

test('A-18: a REQUIRED field hidden by a condition creates no new save gate, and R1 still refuses an API clear', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const ctrl = await makeField(request, 'a18_ctrl');
  const dep = await makeField(request, 'a18_dep', { required: true });
  const other = await makeField(request, 'a18_other');
  await patchField(request, dep.id, { display_condition: [`${ctrl.code}=Commission`] });
  const assetId = await makeAsset(request);

  // Give the required field a real stored value, so R1 has something to
  // refuse the removal of.
  const seeded = await request.put(`/api/v1/assets/${assetId}/fields/${dep.id}`, {
    data: { value_text: 'required and present', if_absent: true },
  });
  expect(seeded.ok(), `seed → ${seeded.status()} ${await seeded.text()}`).toBeTruthy();

  await openAssetEdit(page, assetId);
  // ⛔ ANTI-VACUITY: it is drawn first.
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Commission');
  await expect(page.locator(tid(`field-input-${dep.code}`))).toBeVisible();

  // Hide it, and SAVE something else. No new gate: the save completes.
  await page.locator(tid(`field-input-${ctrl.code}`)).fill('Personal');
  await expect(page.locator(tid(`field-input-${dep.code}`))).toHaveCount(0);
  await page.locator(tid(`field-input-${other.code}`)).fill('unrelated');
  const save = page.locator(tid('asset-fields-save'));
  await expect(save).toBeEnabled();
  await save.click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();

  // R1 is unchanged: the API still refuses to clear a required field,
  // whether or not any form is drawing the control.
  const cleared = await request.delete(`/api/v1/assets/${assetId}/fields/${dep.id}`);
  expect(
    cleared.status(),
    'R1 must still refuse an API clear of a required field while it is hidden',
  ).toBe(422);
  const still = await storedValue(request, 'assets', assetId, dep.id);
  expect(still?.value_text).toBe('required and present');
});

// ---------------------------------------------------------------------------
// B-1 — the N=0 baseline, restated where it can regress
// ---------------------------------------------------------------------------

test('CLASS B — with NO condition configured, the form behaves exactly as it did', async ({
  page,
  request,
}) => {
  // This one PASSES on the sprint baseline and is here as the
  // counterweight: an evaluator with an inverted default, or one that
  // treated an absent condition as unevaluable-and-therefore-something,
  // would break every form in the product and no test above would notice.
  await loginAsAdminViaAPI(request);
  const plain = await makeField(request, 'b1_plain');
  const assetId = await makeAsset(request);
  await openAssetEdit(page, assetId);
  const input = page.locator(tid(`field-input-${plain.code}`));
  await expect(input).toBeVisible();
  await input.fill('ordinary');
  await page.locator(tid('asset-fields-save')).click();
  await expect(page.locator(tid('asset-fields-saved'))).toBeVisible();
  const stored = await storedValue(request, 'assets', assetId, plain.id);
  expect(stored?.value_text).toBe('ordinary');
  // No tab strip either: one bucket is not a choice.
  await expect(page.locator(tid('asset-fields-tabs'))).toHaveCount(0);
});
