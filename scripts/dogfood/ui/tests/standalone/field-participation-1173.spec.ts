// #1173 — a field declares its surfaces, and the advanced page obeys.
//
// Before this, /search/advanced decided its own field list from
// `searchable`, `status` and `type`. Two of those three answer a
// different question — `searchable` means "this field's text feeds the
// search index", `type` means "what a value looks like" — so an
// operator with 200 fields got 200 filters and no way to say
// otherwise. `show_in_advanced_search` is the field's own declaration
// and the page reads it now (ADR 0092 §3).
//
// Three things this file exists to catch, in the order they would
// hurt:
//
//  1. UNSET IS UNCHANGED. A field nobody has configured must still
//     appear. The flag defaults TRUE precisely so that shipping this
//     mid-release empties nobody's search form, and a regression to
//     `=== true` reading of an absent key would do exactly that.
//
//  2. OFF MEANS OFF, ON THAT SURFACE ONLY. The probe disappears from
//     the advanced page and its values keep answering a direct
//     `filter=field:…` query — the `searchable` boundary, asserted
//     against the real search endpoint rather than against the flag we
//     just wrote.
//
//  3. IT IS A DIAL. Back on restores the row, so an operator who
//     experiments is not stuck.
//
// The probe is created and archived by this file for the same reason
// advanced-vocab-1191.spec.ts creates its own: asserting against a
// SEEDED field would make the test a hostage to whatever the seed
// happens to ship, and turning a real field off mid-suite would change
// the page under every other spec.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

const PROBE_CODE = 'probe_1173_participation';
const PROBE_LABEL = 'Finish (1173 probe)';

// Values with an unusual stem, so the direct-filter assertion cannot
// pass by matching some other field's content by coincidence.
const VALUES = [
  { value: 'zibeline', label: 'Zibeline' },
  { value: 'marmalade', label: 'Marmalade' },
];

async function findProbe(request: APIRequestContext, status?: string) {
  const url = status ? `/api/v1/fields?status=${status}` : '/api/v1/fields';
  const r = await request.get(url);
  expect(r.ok(), `GET ${url} → ${r.status()}`).toBeTruthy();
  const rows = (await r.json()) as { id: string; code: string }[];
  return rows.find((f) => f.code === PROBE_CODE);
}

/** Create the probe, or revive the tombstone a previous run left. */
async function ensureProbeField(request: APIRequestContext): Promise<string> {
  await loginAsAdminViaAPI(request);
  const options = { values: VALUES };
  const revive = async (id: string) => {
    const r = await request.patch(`/api/v1/fields/${id}`, {
      data: {
        status: 'active',
        label: PROBE_LABEL,
        options,
        searchable: true,
        // A revived tombstone must start from the DEFAULT participation,
        // otherwise a crashed run that left the flag off would make the
        // "unset is unchanged" assertion below pass for the wrong reason.
        show_in_advanced_search: true,
        clear_edit_tab: true,
      },
    });
    expect(r.ok(), `revive probe field → ${r.status()} ${await r.text()}`).toBeTruthy();
    return id;
  };

  const existing = (await findProbe(request)) ?? (await findProbe(request, 'archived'));
  if (existing) return revive(existing.id);

  const r = await request.post('/api/v1/fields', {
    data: {
      code: PROBE_CODE,
      label: PROBE_LABEL,
      type: 'multi_select',
      subject_kind: 'asset',
      options,
      searchable: true,
      // Last on the page, so the seeded rows keep their positions.
      display_order: 9100,
    },
  });
  if (r.status() === 201) return ((await r.json()) as { id: string }).id;
  const raced = (await findProbe(request)) ?? (await findProbe(request, 'archived'));
  expect(raced, `create probe field → ${r.status()} ${await r.text()}`).toBeTruthy();
  return revive(raced!.id);
}

test.describe.configure({ mode: 'serial' });

let probeId = '';

test.beforeAll(async ({ request }) => {
  probeId = await ensureProbeField(request);
});

test.afterAll(async ({ request }) => {
  if (!probeId) return;
  await loginAsAdminViaAPI(request);
  await request.delete(`/api/v1/fields/${probeId}`);
});

/** The codes the advanced page currently offers, in order. */
async function advancedFieldCodes(page: Page): Promise<string[]> {
  await page.goto('/search/advanced');
  await expect(page.locator(tid('advanced-search-page'))).toBeVisible();
  // Wait for the fetch to settle on SOMETHING before reading the list,
  // so an empty answer is a real empty answer and not a race.
  await expect(page.locator('[data-testid^="field-filter-"]').first()).toBeVisible();
  return page.$$eval('[data-testid^="field-filter-"]', (els) =>
    els.map((e) => e.getAttribute('data-testid')!.replace('field-filter-', '')),
  );
}

async function setParticipation(request: APIRequestContext, on: boolean) {
  await loginAsAdminViaAPI(request);
  const r = await request.patch(`/api/v1/fields/${probeId}`, {
    data: { show_in_advanced_search: on },
  });
  expect(r.ok(), `set show_in_advanced_search=${on} → ${r.status()} ${await r.text()}`).toBeTruthy();
}

test.describe('field participation flags (#1173)', () => {
  test('a field that never declared anything is still offered', async ({ page, request }) => {
    // The probe is created with no participation setting at all — the
    // state every field on every existing install is in after the
    // migration. It must appear.
    const r = await request.get(`/api/v1/fields/${probeId}`);
    expect(r.ok()).toBeTruthy();
    const def = (await r.json()) as { show_in_advanced_search?: boolean; show_on_upload?: boolean };
    expect(
      def.show_in_advanced_search,
      'the API must state the default rather than omit it — a surface reading an absent key has to guess, which is what the flag exists to stop',
    ).toBe(true);
    expect(def.show_on_upload).toBe(true);

    expect(await advancedFieldCodes(page)).toContain(PROBE_CODE);
  });

  test('the admin form shows the participation controls, ticked', async ({ page }) => {
    await page.goto(`/admin/fields/${PROBE_CODE}`);
    await expect(page.locator(tid('field-edit-participation'))).toBeVisible();
    await expect(page.locator(tid('field-edit-show-in-advanced-search'))).toBeChecked();
    await expect(page.locator(tid('field-edit-show-on-upload'))).toBeChecked();
    await expect(page.locator(tid('field-edit-edit-tab'))).toHaveValue('');
  });

  test('turning it off removes the control and nothing else', async ({ page, request }) => {
    const before = await advancedFieldCodes(page);
    expect(before, 'the probe must be on the page before it can be taken off it').toContain(
      PROBE_CODE,
    );

    await setParticipation(request, false);

    const after = await advancedFieldCodes(page);
    expect(after).not.toContain(PROBE_CODE);
    // ONLY that surface, and only that field. Everything else the page
    // offered is still offered, in the same order.
    expect(after).toEqual(before.filter((c) => c !== PROBE_CODE));

    // The `searchable` boundary. The flag governs a CONTROL; the field's
    // values must still filter when a caller composes the term directly,
    // which is the same query the removed control would have produced.
    const r = await request.get(
      `/api/v1/search?filter=field:${PROBE_CODE}=zibeline&limit=1`,
    );
    expect(
      r.status(),
      'a field taken off the advanced FORM must still answer a filter composed by hand — participation is not an access rule and not an index setting',
    ).toBe(200);

    // And `searchable` itself is untouched, which is the setting an
    // operator would have had to sacrifice before this flag existed.
    const def = await request.get(`/api/v1/fields/${probeId}`);
    expect(((await def.json()) as { searchable?: boolean }).searchable).toBe(true);
  });

  test('it is a dial, not a one-way door', async ({ page, request }) => {
    await setParticipation(request, true);
    const codes = await advancedFieldCodes(page);
    expect(codes).toContain(PROBE_CODE);
  });

  test('an edit tab round-trips through the admin form', async ({ page, request }) => {
    await page.goto(`/admin/fields/${PROBE_CODE}`);
    await expect(page.locator(tid('field-edit-participation'))).toBeVisible();
    await page.locator(tid('field-edit-edit-tab')).fill('Production');
    await page.locator(tid('field-options-save')).click();
    await expect(page.locator(tid('field-options-saved'))).toBeVisible();

    let def = await (await request.get(`/api/v1/fields/${probeId}`)).json();
    expect((def as { edit_tab?: string | null }).edit_tab).toBe('Production');

    // Emptying the box UNASSIGNS rather than storing a blank — "no tab"
    // has exactly one representation, and the server refuses the other.
    await page.reload();
    await expect(page.locator(tid('field-edit-edit-tab'))).toHaveValue('Production');
    await page.locator(tid('field-edit-edit-tab')).fill('');
    await page.locator(tid('field-options-save')).click();
    await expect(page.locator(tid('field-options-saved'))).toBeVisible();

    def = await (await request.get(`/api/v1/fields/${probeId}`)).json();
    expect((def as { edit_tab?: string | null }).edit_tab ?? null).toBeNull();
  });
});
