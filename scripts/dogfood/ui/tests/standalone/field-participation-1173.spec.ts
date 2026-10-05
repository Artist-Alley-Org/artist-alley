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

    // `searchable` is the fourth setting on this form and the oldest of
    // them: it has existed since the 00001 baseline, both the create and
    // the update API persist it, and until sprint 19 NO admin surface
    // drew a control for it. The one flag that decides whether a field
    // is findable at all could be reached only by hand-writing a PATCH.
    //
    // It sits in its own section rather than beside the participation
    // toggles, and that placement is asserted rather than assumed: 18d
    // had to unpick the conflation of "this field's text is indexed"
    // with "this field's control appears on the advanced page", and one
    // shared heading is how the two get read as one setting again.
    await expect(
      page.locator(tid('field-edit-search-index')),
      'the search-index setting must be its own section, not a fourth line under participation',
    ).toBeVisible();
    await expect(page.locator(tid('field-edit-searchable'))).toBeChecked();
    await expect(page.locator(tid('field-edit-searchable-boundary'))).toBeVisible();
  });

  test('the searchable control writes the flag it names', async ({ page, request }) => {
    // The control has to be wired to `searchable` and to nothing else.
    // A checkbox that saved a participation flag instead would look
    // identical on the page and would leave the index untouched, which
    // is precisely the failure an operator cannot see.
    await page.goto(`/admin/fields/${PROBE_CODE}`);
    await expect(page.locator(tid('field-edit-searchable'))).toBeChecked();

    await page.locator(tid('field-edit-searchable')).uncheck();
    await page.locator(tid('field-options-save')).click();
    await expect(page.locator(tid('field-options-saved'))).toBeVisible();

    let def = (await (await request.get(`/api/v1/fields/${probeId}`)).json()) as {
      searchable?: boolean;
      show_in_advanced_search?: boolean;
      show_on_upload?: boolean;
    };
    expect(def.searchable).toBe(false);
    // Its neighbours are untouched. One field's settings must not move
    // another's, and one control must not move another control's flag.
    expect(def.show_in_advanced_search).toBe(true);
    expect(def.show_on_upload).toBe(true);

    // A dial, and it survives a reload rather than only the response.
    await page.reload();
    await expect(page.locator(tid('field-edit-searchable'))).not.toBeChecked();
    await page.locator(tid('field-edit-searchable')).check();
    await page.locator(tid('field-options-save')).click();
    await expect(page.locator(tid('field-options-saved'))).toBeVisible();

    def = (await (await request.get(`/api/v1/fields/${probeId}`)).json()) as {
      searchable?: boolean;
    };
    expect(def.searchable).toBe(true);
  });

  test('the input rules are offered, and refuse what the server refuses', async ({
    page,
    request,
  }) => {
    // The probe is a multi_select, which is exactly the case worth
    // asserting: `read_only` is legal on every non-mirrored field, and a
    // PATTERN is not — it applies to `text` and `longtext` only, because
    // those are the two types that store the operator's own words. A
    // form that offered the box here would be offering a setting the
    // server answers 400 to.
    await page.goto(`/admin/fields/${PROBE_CODE}`);
    await expect(page.locator(tid('field-edit-input-rules'))).toBeVisible();
    await expect(page.locator(tid('field-edit-read-only'))).not.toBeChecked();
    await expect(page.locator(tid('field-edit-regexp-filter'))).toHaveCount(0);
    await expect(page.locator(tid('field-edit-regexp-filter-type-note'))).toBeVisible();

    await page.locator(tid('field-edit-read-only')).check();
    await page.locator(tid('field-options-save')).click();
    await expect(page.locator(tid('field-options-saved'))).toBeVisible();

    const def = (await (await request.get(`/api/v1/fields/${probeId}`)).json()) as {
      read_only?: boolean;
      regexp_filter?: string | null;
    };
    expect(def.read_only).toBe(true);
    // Unset, and unset is the ONLY way to say "no pattern" — the empty
    // string is refused rather than stored.
    expect(def.regexp_filter ?? null).toBeNull();

    // Put it back, so the probe leaves the suite as it entered it.
    await page.reload();
    await expect(page.locator(tid('field-edit-read-only'))).toBeChecked();
    await page.locator(tid('field-edit-read-only')).uncheck();
    await page.locator(tid('field-options-save')).click();
    await expect(page.locator(tid('field-options-saved'))).toBeVisible();
  });

  test('a mirrored field is offered neither input rule', async ({ page }) => {
    // `title` is a view onto `assets.title` (#822), and the asset's own
    // create and update paths write that column too. A rule set on the
    // field plane would bind one writer and not the other, so the server
    // refuses both settings and the form must not pretend otherwise.
    //
    // Not asserted on the probe: this is the one case where the controls
    // must be ABSENT, and a probe cannot be made mirrored — which
    // columns are mirrorable is a CHECK constraint, deliberately.
    await page.goto('/admin/fields/title');
    await expect(page.locator(tid('field-edit-input-rules'))).toBeVisible();
    await expect(page.locator(tid('field-edit-input-rules-mirrored'))).toBeVisible();
    await expect(page.locator(tid('field-edit-read-only'))).toHaveCount(0);
    await expect(page.locator(tid('field-edit-regexp-filter'))).toHaveCount(0);
    // The search-index control is NOT gated by mirroring: a mirrored
    // field's text is indexed like any other.
    await expect(page.locator(tid('field-edit-searchable'))).toHaveCount(1);
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
