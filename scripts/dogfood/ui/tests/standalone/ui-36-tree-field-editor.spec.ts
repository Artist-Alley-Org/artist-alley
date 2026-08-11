// ui-36-tree-field-editor.spec.ts
//
// #779 + #825 — editing a hierarchical field's nested terms.
//
// The editor's own arithmetic (path addressing, reparenting, the
// self-nesting guard) is unit tested next to it in
// web/src/lib/fieldOptions.test.ts, and what the server does with the
// document it produces is pinned in
// app/internal/metadata/tree_editor_e2e_test.go. What neither can see
// is whether the CONTROLS reach a nested term at all — the whole defect
// was a rendered-but-unreachable subtree, which every unit test in the
// world passes over. So these drive the real admin surface with a real
// vocabulary and assert on what an operator can touch.
//
// Four things worth a browser:
//
//   1. A deep term is EDITABLE, and editing a branch does not eat its
//      leaves. The leaf count is the assertion (#825's hazard is a
//      silent drop that leaves the edited branch looking correct).
//   2. A reparent lands, and the value a record already holds keeps
//      resolving — through the collection picker, which is the shipped
//      consumer of selectableTreeOptions.
//   3. The move picker cannot offer a term its own subtree.
//   4. It all works at 390px with a thumb, because a nested editor
//      that needs a mouse is a nested editor an operator does not have.
//
// Fields are created per attempt (DELETE soft-archives and `code` is
// UNIQUE — see ui-18's note) and deleted in afterEach.

import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

/** `country` as migration 00024 ships it: 24 nations, 5 continents. */
const COUNTRY_VALUES = [
  {
    value: 'africa',
    label: 'Africa',
    children: [
      { value: 'eg', label: 'Egypt' },
      { value: 'ke', label: 'Kenya' },
      { value: 'ma', label: 'Morocco' },
      { value: 'ng', label: 'Nigeria' },
      { value: 'za', label: 'South Africa' },
    ],
  },
  {
    value: 'americas',
    label: 'Americas',
    children: [
      { value: 'ar', label: 'Argentina' },
      { value: 'br', label: 'Brazil' },
      { value: 'ca', label: 'Canada' },
      { value: 'mx', label: 'Mexico' },
      { value: 'us', label: 'United States' },
    ],
  },
  {
    value: 'asia',
    label: 'Asia',
    children: [
      { value: 'cn', label: 'China' },
      { value: 'in', label: 'India' },
      { value: 'jp', label: 'Japan' },
      { value: 'kr', label: 'South Korea' },
      { value: 'sg', label: 'Singapore' },
    ],
  },
  {
    value: 'europe',
    label: 'Europe',
    children: [
      { value: 'fr', label: 'France' },
      { value: 'de', label: 'Germany' },
      { value: 'it', label: 'Italy' },
      { value: 'nl', label: 'Netherlands' },
      { value: 'es', label: 'Spain' },
      { value: 'se', label: 'Sweden' },
      { value: 'gb', label: 'United Kingdom' },
    ],
  },
  {
    value: 'oceania',
    label: 'Oceania',
    children: [
      { value: 'au', label: 'Australia' },
      { value: 'nz', label: 'New Zealand' },
    ],
  },
];

const LEAVES = COUNTRY_VALUES.flatMap((b) => b.children.map((c) => c.value));

type Opt = { value: string; label?: string; status?: string; replaced_by?: string; children?: Opt[] };

/** Every slug in a stored options document, at any depth. */
function slugsOf(values: unknown[]): string[] {
  return values.flatMap((v) => {
    if (typeof v === 'string') return [v];
    const o = v as Opt;
    return [o.value, ...(o.children ? slugsOf(o.children) : [])];
  });
}

function findTerm(values: unknown[], slug: string): Opt | undefined {
  for (const v of values) {
    if (typeof v === 'string') {
      if (v === slug) return { value: v };
      continue;
    }
    const o = v as Opt;
    if (o.value === slug) return o;
    const hit = o.children ? findTerm(o.children, slug) : undefined;
    if (hit) return hit;
  }
  return undefined;
}

let createdFieldIds: string[] = [];
let createdCollectionIds: string[] = [];

async function createTreeField(
  page: Page,
  code: string,
  subject: 'asset' | 'collection',
): Promise<{ id: string; code: string }> {
  const res = await page.request.post('/api/v1/fields', {
    data: {
      code,
      label: 'UI-36 Region',
      type: 'tree',
      subject_kind: subject,
      options: { values: COUNTRY_VALUES },
    },
  });
  expect(res.status(), await res.text()).toBe(201);
  const f = (await res.json()) as { id: string; code: string };
  createdFieldIds.push(f.id);
  return f;
}

/** Read the field definition's stored vocabulary back from the API. */
async function readValues(page: Page, id: string): Promise<unknown[]> {
  const res = await page.request.get(`/api/v1/fields/${id}`);
  expect(res.ok(), await res.text()).toBe(true);
  const def = (await res.json()) as { options?: { values?: unknown[] } };
  return def.options?.values ?? [];
}

/**
 * Open the field's own page (#854).
 *
 * Deliberately routed THROUGH the index rather than deep-linked: the
 * link is what replaced the expanding row, so the path an operator
 * actually takes is the one these tests take. Deep-linking is covered
 * separately in ui-38.
 */
async function openEditor(page: Page, code: string): Promise<void> {
  await page.goto('/admin/fields');
  await expect(page.getByTestId(`admin-fields-row-${code}`)).toBeVisible();
  await page.getByTestId(`admin-fields-open-${code}`).click();
  await expect(page.getByTestId('field-editor')).toBeVisible();
}

function fieldCode(prefix: string, testInfo: { workerIndex: number; retry: number }): string {
  return `ui36_${prefix}_${testInfo.workerIndex}_${testInfo.retry}_${Date.now()}`;
}

test.describe('UI-36 tree field editor', () => {
  test.beforeEach(async ({ page }) => {
    createdFieldIds = [];
    createdCollectionIds = [];
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ request }) => {
    for (const id of createdCollectionIds) {
      await request.delete(`/api/v1/collections/${id}?hard=true`).catch(() => undefined);
    }
    for (const id of createdFieldIds) {
      await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
    }
    createdFieldIds = [];
    createdCollectionIds = [];
  });

  // ── 1. A nested term is reachable, and its siblings survive ──────
  test('edits a term two levels down and keeps every leaf', async ({ page }, testInfo) => {
    const f = await createTreeField(page, fieldCode('deep', testInfo), 'asset');
    await openEditor(page, f.code);

    // The read-only apology is gone, because the restriction is.
    await expect(page.getByTestId('field-options-nested-note')).toHaveCount(0);

    // A leaf now has the same controls its continent has.
    const gbLabel = page.getByTestId('field-option-label-gb');
    await expect(gbLabel).toBeVisible();
    await expect(gbLabel).toBeEditable();
    await expect(page.getByTestId('field-option-status-gb')).toBeVisible();

    await page.getByTestId('field-option-label-europe').fill('Europe (EU)');
    await gbLabel.fill('Great Britain');
    await page.screenshot({ path: testInfo.outputPath('tree-editor-open.png'), fullPage: true });
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();

    // THE #825 PIN, against what actually got stored: a parent-only
    // edit that silently dropped its children would leave the branch
    // looking perfect on screen.
    const values = await readValues(page, f.id);
    const slugs = slugsOf(values);
    for (const leaf of LEAVES) {
      expect(slugs, `leaf ${leaf} disappeared on save`).toContain(leaf);
    }
    expect(slugs).toHaveLength(29);
    expect(findTerm(values, 'europe')?.label).toBe('Europe (EU)');
    expect(findTerm(values, 'gb')?.label).toBe('Great Britain');
  });

  // ── 2 + 3. Reparent, the subtree guard, and the held value ───────
  test('moves a term to another branch and the record still resolves', async ({ page }, testInfo) => {
    const f = await createTreeField(page, fieldCode('move', testInfo), 'collection');

    const created = await page.request.post('/api/v1/collections', {
      data: { name: `UI-36 Move ${testInfo.workerIndex}-${Date.now()}` },
    });
    expect(created.status()).toBe(201);
    const coll = (await created.json()) as { id: string };
    createdCollectionIds.push(coll.id);

    // Give the collection a value at depth, through the real picker.
    await page.goto(`/collections/${coll.id}`);
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();
    await expect(page.getByTestId('collection-fields-section')).toBeVisible();
    const picker = page.getByTestId(`field-input-${f.code}`);
    await picker.selectOption('gb');
    await page.getByTestId('collection-fields-save').click();
    await expect(page.getByTestId('collection-fields-saved')).toBeVisible();

    await openEditor(page, f.code);

    // The subtree guard, as the operator meets it: opening the move
    // picker on Europe offers Africa and does NOT offer Europe or any
    // country under it. There is no gesture that nests a branch in
    // itself, so there is nothing to refuse later.
    await page.getByTestId('field-option-move-europe').click();
    await expect(page.getByTestId('field-option-move-picker')).toBeVisible();
    await expect(page.getByTestId('field-option-move-dest-africa')).toBeVisible();
    await expect(page.getByTestId('field-option-move-dest-europe')).toHaveCount(0);
    for (const child of ['fr', 'de', 'gb']) {
      await expect(page.getByTestId(`field-option-move-dest-${child}`)).toHaveCount(0);
    }
    await page.screenshot({ path: testInfo.outputPath('tree-move-picker.png'), fullPage: true });
    await page.getByTestId('field-option-move-cancel').click();

    // Now actually move a leaf across branches.
    await page.getByTestId('field-option-move-gb').click();
    await page.getByTestId('field-option-move-dest-americas').click();
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();

    const values = await readValues(page, f.id);
    expect(slugsOf(values)).toHaveLength(29);
    const americas = findTerm(values, 'americas');
    expect(americas?.children?.map((c) => c.value)).toContain('gb');
    const europe = findTerm(values, 'europe');
    expect(europe?.children?.map((c) => c.value)).not.toContain('gb');

    // The collection's stored value never moved — it is the slug, and
    // the slug did not change. The picker still offers the term, now
    // at its new depth, and it is still what the collection holds.
    await page.goto(`/collections/${coll.id}`);
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();
    const after = page.getByTestId(`field-input-${f.code}`);
    await expect(after).toHaveValue('gb');
    // Every depth is still on offer — branches included.
    const offered = await after.locator('option').evaluateAll((els) =>
      els.map((e) => (e as HTMLOptionElement).value),
    );
    expect(offered).toContain('gb');
    expect(offered).toContain('americas');
    expect(offered).toContain('europe');
    // And the ancestry the option advertises is the NEW one.
    const title = await after
      .locator('option[value="gb"]')
      .evaluate((e) => (e as HTMLOptionElement).title);
    expect(title).toBe('Americas / United Kingdom');
  });

  // ── 4. Lifecycle at depth ────────────────────────────────────────
  test('retires a leaf without orphaning the record that holds it', async ({ page }, testInfo) => {
    const f = await createTreeField(page, fieldCode('life', testInfo), 'collection');

    const created = await page.request.post('/api/v1/collections', {
      data: { name: `UI-36 Life ${testInfo.workerIndex}-${Date.now()}` },
    });
    const coll = (await created.json()) as { id: string };
    createdCollectionIds.push(coll.id);
    const fresh = await page.request.post('/api/v1/collections', {
      data: { name: `UI-36 Fresh ${testInfo.workerIndex}-${Date.now()}` },
    });
    const other = (await fresh.json()) as { id: string };
    createdCollectionIds.push(other.id);

    await page.goto(`/collections/${coll.id}`);
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();
    await page.getByTestId(`field-input-${f.code}`).selectOption('gb');
    await page.getByTestId('collection-fields-save').click();
    await expect(page.getByTestId('collection-fields-saved')).toBeVisible();

    // Deprecate the leaf and point it at a live successor.
    await openEditor(page, f.code);
    await page.getByTestId('field-option-status-gb').selectOption('deprecated');
    const successor = page.getByTestId('field-option-replaced-by-gb');
    await expect(successor).toBeVisible();
    // A retired term is not offered as somebody else's successor —
    // that just moves the problem one hop along.
    const successors = await successor.locator('option').evaluateAll((els) =>
      els.map((e) => (e as HTMLOptionElement).value),
    );
    expect(successors).not.toContain('gb');
    expect(successors).toContain('fr');
    await successor.selectOption('fr');
    await page.getByTestId('field-options-save').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();

    const values = await readValues(page, f.id);
    const gb = findTerm(values, 'gb');
    expect(gb?.status).toBe('deprecated');
    expect(gb?.replaced_by).toBe('fr');
    // Retired, never deleted.
    expect(slugsOf(values)).toContain('gb');

    // A collection that does NOT hold it is no longer offered it…
    await page.goto(`/collections/${other.id}`);
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();
    const freshPicker = page.getByTestId(`field-input-${f.code}`);
    const freshOffered = await freshPicker.locator('option').evaluateAll((els) =>
      els.map((e) => (e as HTMLOptionElement).value),
    );
    expect(freshOffered).not.toContain('gb');

    // …while the one that already holds it keeps it, and is still
    // offered it. Blanking a value on a record nobody edited is the
    // failure ADR 0012 exists to prevent.
    await page.goto(`/collections/${coll.id}`);
    await page.getByTestId('collection-detail-more-button').click();
    await page.getByTestId('collection-detail-edit-menuitem').click();
    const heldPicker = page.getByTestId(`field-input-${f.code}`);
    await expect(heldPicker).toHaveValue('gb');
    const heldOffered = await heldPicker.locator('option').evaluateAll((els) =>
      els.map((e) => (e as HTMLOptionElement).value),
    );
    expect(heldOffered).toContain('gb');

    // And the write gate grandfathers it: re-saving the value a record
    // already holds is accepted, deprecated or not (#841). Driven at
    // the endpoint the section posts to, because the section's Save
    // button is (correctly) disabled until something CHANGES — and
    // there is nothing to change here, which is the whole point.
    const regrant = await page.request.put(
      `/api/v1/collections/${coll.id}/fields/${f.id}`,
      { data: { value_text: 'gb' } },
    );
    expect(regrant.ok(), await regrant.text()).toBe(true);
  });

  // ── 5. The server's refusal is legible, not swallowed ────────────
  test('shows the server’s rejection inline instead of losing it', async ({ page }, testInfo) => {
    const f = await createTreeField(page, fieldCode('reject', testInfo), 'asset');
    await openEditor(page, f.code);

    // The editor blocks a duplicate before it is typed, so the only
    // way to see the SERVER's refusal on this surface is to make the
    // server refuse. The message is the real one
    // (app/internal/metadata/options.go, pinned end-to-end by
    // TestTreeEditorRejectsDuplicateSlugAtDepth); what is under test
    // here is that the editor renders it rather than dropping it for a
    // house string that names nothing.
    await page.route('**/api/v1/fields/*', async (route) => {
      if (route.request().method() !== 'PATCH') return route.fallback();
      await route.fulfill({
        status: 400,
        contentType: 'application/json',
        body: JSON.stringify({
          error: 'options.values[1].children[0]: duplicate option value "gb"',
        }),
      });
    });

    await page.getByTestId('field-option-label-gb').fill('Great Britain');
    await page.getByTestId('field-options-save').click();
    const err = page.getByTestId('field-options-error');
    await expect(err).toBeVisible();
    await expect(err).toContainText('duplicate option value "gb"');
    await page.screenshot({ path: testInfo.outputPath('tree-editor-error.png'), fullPage: true });
    await page.unroute('**/api/v1/fields/*');
  });

  // ── 6. The conflict flow, on a DEEP edit ─────────────────────────
  test('surfaces a stale-baseline conflict on a nested edit', async ({ page }, testInfo) => {
    const f = await createTreeField(page, fieldCode('conflict', testInfo), 'asset');
    await openEditor(page, f.code);

    // Somebody else saves while this editor sits open. Done through
    // the API because that is exactly what a second admin — or the
    // open-vocabulary mint — is from this page's point of view.
    const cur = await page.request.get(`/api/v1/fields/${f.id}`);
    const def = (await cur.json()) as { updated_at: string };
    const other = await page.request.patch(`/api/v1/fields/${f.id}`, {
      data: {
        if_unchanged_since: def.updated_at,
        options: { values: [...COUNTRY_VALUES, { value: 'antarctica', label: 'Antarctica' }] },
      },
    });
    expect(other.status(), await other.text()).toBe(200);

    // The open editor edits a LEAF and saves on its now-stale baseline.
    await page.getByTestId('field-option-label-gb').fill('Great Britain');
    await page.getByTestId('field-options-save').click();
    const conflict = page.getByTestId('field-options-conflict');
    await expect(conflict).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath('tree-editor-conflict.png'), fullPage: true });

    // The edits are still in the form, and the deliberate overwrite
    // goes through on the re-based timestamp.
    await expect(page.getByTestId('field-option-label-gb')).toHaveValue('Great Britain');
    await page.getByTestId('field-options-conflict-overwrite').click();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();
    expect(findTerm(await readValues(page, f.id), 'gb')?.label).toBe('Great Britain');
  });
});

// ── 7. The same gestures with a thumb ──────────────────────────────
//
// Not a proxy: the real controls, at the real width, with real taps.
// A nested editor whose move picker or add-form only works with a
// mouse is a nested editor the operator does not have (#376).
test.describe('UI-36 tree field editor at 390px', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true });

  test.beforeEach(async ({ page }) => {
    createdFieldIds = [];
    createdCollectionIds = [];
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ request }) => {
    for (const id of createdFieldIds) {
      await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
    }
    createdFieldIds = [];
  });

  test('adds a term under a branch and moves it, by tapping', async ({ page }, testInfo) => {
    const f = await createTreeField(page, fieldCode('mobile', testInfo), 'asset');
    await page.goto('/admin/fields');
    await expect(page.getByTestId(`admin-fields-row-${f.code}`)).toBeVisible();
    await page.getByTestId(`admin-fields-open-${f.code}`).tap();
    await expect(page.getByTestId('field-editor')).toBeVisible();

    // Add a child under Europe. The term is typed as a NAME; the slug
    // it will be stored as is shown before it is committed.
    await page.getByTestId('field-option-add-child-europe').tap();
    const input = page.getByTestId('field-option-inline-add-input');
    await expect(input).toBeVisible();
    await input.fill('Republic of Ireland');
    await expect(page.getByTestId('field-option-inline-add-slug')).toContainText(
      'republic-of-ireland',
    );

    // A slug that already exists anywhere in the tree is refused here,
    // inline, before the save — tree-WIDE, which is the rule the
    // server enforces. `gb` sits two levels down, where a top-level
    // scan would not have found it.
    await input.fill('GB');
    await page.getByTestId('field-option-inline-add-confirm').tap();
    await expect(page.getByTestId('field-option-inline-add-error')).toBeVisible();
    await expect(page.getByTestId('field-option-row-gb')).toHaveCount(1);

    // A term that merely READS the same as an existing one is a
    // warning, not a refusal — the server allows it, and two branches
    // legitimately holding a "Georgia" is a real vocabulary.
    await input.fill('Egypt');
    await expect(page.getByTestId('field-option-inline-add-warning')).toContainText('eg');
    await expect(page.getByTestId('field-option-inline-add-error')).toHaveCount(0);

    await input.fill('Republic of Ireland');
    await page.getByTestId('field-option-inline-add-confirm').tap();
    await expect(page.getByTestId('field-option-row-republic-of-ireland')).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath('tree-editor-390.png'), fullPage: true });

    // …and move it somewhere else, by tapping a destination.
    await page.getByTestId('field-option-move-republic-of-ireland').tap();
    await expect(page.getByTestId('field-option-move-picker')).toBeVisible();
    await page.getByTestId('field-option-move-dest-root').tap();

    await page.getByTestId('field-options-save').tap();
    await expect(page.getByTestId('field-options-saved')).toBeVisible();

    const values = await readValues(page, f.id);
    expect(slugsOf(values)).toHaveLength(30);
    // Top level, because that is where it was tapped to.
    expect((values as Opt[]).map((v) => (typeof v === 'string' ? v : v.value))).toContain(
      'republic-of-ireland',
    );
    expect(findTerm(values, 'republic-of-ireland')?.label).toBe('Republic of Ireland');
  });
});
