// #1173 sprint 18d — the advanced page learns the BUILT-IN dimensions.
//
// advanced-operators-1165-1173-1197 covers the metadata-FIELD half of
// this form: a filter row per `field_definition`, the operator grammar
// behind it, and the live count. What is here is the other half, and
// until 18d it did not exist on the page at all — a person could narrow
// by a custom field and by resource type, and by nothing else. Not by
// who made the work, not by what state it is in, not by what kind of
// file it is, not by how big it is, and not by its pixel dimensions,
// even though every one of those filters already worked on the wire.
//
// # ⛔ THE CONTROL IS THE SUBJECT, NOT THE ENDPOINT
//
// Every assertion below that CAN be driven through the rendered control
// is driven through it. Sending `filter=owner:7` to the API and
// checking the rows proves the dimension works, which 18b and 18c
// already proved; it says nothing about whether a person can reach it.
// The API is used only where the form cannot produce the input — a
// fixture that has to exist before the page loads, or a malformed token
// no control can emit.
//
// # ⛔ WHY THE OR ASSERTIONS COMPARE SETS AND NOT COUNTS
//
// ADR 0093's 2026-08-20 amendment: a count assertion that passes on a
// union passes on the bug. Two contributors ticked together must emit
// A ∪ B, and a count is satisfied equally by accidental AND (∅),
// first-value-wins (A) and last-value-wins (B). So the contributor arm
// compares the emitted `filter=` token SET and the resulting hit ids.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

/** A global text field the stock seed always ships — the "the page
 *  loaded its fields" signal every helper below waits on. */
const ALWAYS_FIELD = 'copyright';

/** The shipped numeric field (migration 00017, registered in
 *  db.ShippedFieldCodes). Resolved by CODE here for the same reason the
 *  IIIF handler and the masonry tile shape resolve it by code. */
const PIXEL_FIELD = 'pixel_width';

/** A type-scoped NUMERIC probe, so `applies_to` can be checked on the
 *  family 18d introduced rather than only on the text family
 *  advanced-operators already covers. */
const NUM_PROBE_CODE = 'probe_18d_scoped_number';
const NUM_PROBE_LABEL = 'Scoped number (18d probe)';

test.describe.configure({ mode: 'serial' });

let imageRef = 0;
let videoRef = 0;
let numProbeId = '';

async function typeRef(request: APIRequestContext, name: string): Promise<number> {
  const r = await request.get('/api/v1/asset_types');
  expect(r.ok(), `GET /asset_types → ${r.status()}`).toBeTruthy();
  const rows = (await r.json()) as { ref: number; name?: string | null }[];
  const hit = rows.find((t) => t.name === name);
  expect(hit, `no asset type named ${name}`).toBeTruthy();
  return hit!.ref;
}

async function findField(request: APIRequestContext, code: string, status?: string) {
  const url = status ? `/api/v1/fields?status=${status}` : '/api/v1/fields';
  const r = await request.get(url);
  expect(r.ok(), `GET ${url} → ${r.status()}`).toBeTruthy();
  const rows = (await r.json()) as { id: string; code: string }[];
  return rows.find((f) => f.code === code);
}

/** Create the scoped numeric probe, or revive the archived tombstone a
 *  crashed run left. `DELETE /fields/{id}` archives rather than erases,
 *  and the page already drops archived rows, so a failed cleanup cannot
 *  leak the probe onto the form. */
async function ensureNumberProbe(request: APIRequestContext, ref: number): Promise<string> {
  await loginAsAdminViaAPI(request);
  const shape = {
    status: 'active',
    label: NUM_PROBE_LABEL,
    searchable: true,
    show_in_advanced_search: true,
    applies_to: [ref],
  };
  const revive = async (id: string) => {
    const r = await request.patch(`/api/v1/fields/${id}`, { data: shape });
    expect(r.ok(), `revive number probe → ${r.status()} ${await r.text()}`).toBeTruthy();
    return id;
  };
  const existing =
    (await findField(request, NUM_PROBE_CODE)) ?? (await findField(request, NUM_PROBE_CODE, 'archived'));
  if (existing) return revive(existing.id);
  const r = await request.post('/api/v1/fields', {
    data: {
      code: NUM_PROBE_CODE,
      label: NUM_PROBE_LABEL,
      type: 'number',
      subject_kind: 'asset',
      display_order: 9200,
      ...shape,
    },
  });
  if (r.status() === 201) return ((await r.json()) as { id: string }).id;
  const raced =
    (await findField(request, NUM_PROBE_CODE)) ?? (await findField(request, NUM_PROBE_CODE, 'archived'));
  expect(raced, `create number probe → ${r.status()} ${await r.text()}`).toBeTruthy();
  return revive(raced!.id);
}

test.beforeAll(async ({ request }) => {
  imageRef = await typeRef(request, 'Image');
  videoRef = await typeRef(request, 'Video');
  numProbeId = await ensureNumberProbe(request, imageRef);
});

test.afterAll(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  if (numProbeId) await request.delete(`/api/v1/fields/${numProbeId}`);
});

async function openAdvanced(page: Page) {
  await page.goto('/search/advanced');
  await expect(page.locator(tid('advanced-search-page'))).toBeVisible();
  await expect(page.locator(`[data-testid="field-filter-${ALWAYS_FIELD}"]`)).toBeVisible();
}

/** Every `filter=` token in an address. */
function filtersOf(url: string): string[] {
  return new URL(url).searchParams.getAll('filter');
}

/** Submit the form and return the `filter=` tokens the address carries. */
async function submitAndReadFilters(page: Page): Promise<string[]> {
  await page.locator(tid('advanced-submit')).click();
  await page.waitForURL(/\/search\?/, { timeout: 20_000 });
  return filtersOf(page.url());
}

/**
 * Build a form from scratch and return the `filter=` tokens it submits.
 *
 * ⛔ EVERY CASE REOPENS THE PAGE. Going back to a submitted form does
 * NOT restore its controls — reopen hydration is deliberately not part
 * of this sprint — so a case that relied on `goBack()` would be
 * asserting against an EMPTY form and would pass on almost anything.
 */
async function emitWith(page: Page, build: (p: Page) => Promise<void>): Promise<string[]> {
  await openAdvanced(page);
  await build(page);
  return submitAndReadFilters(page);
}

/** Wait for the contributor suggestion list to hold at least one row. */
async function contributorOptions(page: Page): Promise<number[]> {
  const list = page.locator(tid('advanced-contributor-options'));
  await expect(list).toBeVisible({ timeout: 20_000 });
  const ids = await list.locator('[data-testid^="advanced-contributor-option-"]').evaluateAll(
    (els) => els.map((e) => Number(e.getAttribute('data-testid')!.replace('advanced-contributor-option-', ''))),
  );
  return ids;
}

// ─────────────────────────────────────────────────────────────────────

test.describe('advanced search — the built-in dimensions', () => {
  test('⛔ the panel offers Contributor, File type, File size and workflow state', async ({
    page,
  }) => {
    // ⛔ FAIL-BEFORE. On pre-18d dev this test fails on the first
    // expectation: `/search/advanced` renders a resource-type scope and
    // a row per metadata field, and nothing else. The controls below are
    // the whole of sprint 18d's surface, so their PRESENCE is the
    // regression the sprint closes.
    await openAdvanced(page);

    await expect(
      page.locator(tid('advanced-contributor')),
      'a person cannot narrow by who made the work',
    ).toBeVisible();
    await expect(
      page.locator(tid('advanced-extension')),
      'a person cannot narrow by what kind of file it is',
    ).toBeVisible();
    await expect(
      page.locator(tid('advanced-filesize-min')),
      'a person cannot narrow by how big the file is',
    ).toBeVisible();
    await expect(
      page.locator(tid('advanced-workflow-none')),
      'a person cannot find the files nobody has put into a workflow state',
    ).toBeVisible();
    await expect(
      page.locator(`[data-testid="field-from-${PIXEL_FIELD}"]`),
      'a person cannot narrow by pixel dimensions — the `number` family had no control',
    ).toBeVisible();
  });

  // ── CONTRIBUTOR ────────────────────────────────────────────────────

  test('⛔ contributor: two ticks are a UNION, not an intersection or a last-one-wins', async ({
    page,
    request,
  }) => {
    await openAdvanced(page);
    const refs = await contributorOptions(page);
    expect(refs.length, 'the seeded stack must offer at least two contributors').toBeGreaterThan(1);
    const [a, b] = refs;

    await page.locator(`[data-testid="advanced-contributor-option-${a}"]`).click();
    await expect(page.locator(`[data-testid="advanced-contributor-chip-${a}"]`)).toBeVisible();

    // ⛔ THE ForFacet GUARANTEE, driven through the control. With A
    // selected, B must still be OFFERED. `Selection.ForFacet` drops an
    // OR dimension's own terms "so an OR dimension does not filter
    // itself out of existence"; without it, picking one contributor
    // collapses the list to that one and there is no way back except
    // clearing.
    await expect(
      page.locator(`[data-testid="advanced-contributor-option-${b}"]`),
      `contributor ${b} left the list after ${a} was ticked. That is the drill-down ` +
        'collapse ForFacet exists to prevent — a multi-select that cannot select twice.',
    ).toBeVisible({ timeout: 20_000 });

    await page.locator(`[data-testid="advanced-contributor-option-${b}"]`).click();
    await expect(page.locator(`[data-testid="advanced-contributor-chip-${b}"]`)).toBeVisible();

    const emitted = (await submitAndReadFilters(page)).filter((f) => f.startsWith('owner:'));
    expect(
      emitted.sort(),
      `two ticked contributors emitted ${JSON.stringify(emitted)}. A count assertion is ` +
        'satisfied by accidental AND (no terms), first-wins (A only) and last-wins (B only) ' +
        'alike; only the SET separates all four implementations.',
    ).toEqual([`owner:${a}`, `owner:${b}`].sort());

    // ⛔ AND THE SAME FOUR IMPLEMENTATIONS, ONE LAYER DOWN. Emitting two
    // tokens proves the CONTROL kept both; it says nothing about how the
    // engine combined them.
    //
    //   correct OR         |A ∪ B| = |A| + |B|   (an asset has ONE owner,
    //                                             so the sets are disjoint)
    //   accidental AND     0
    //   first-value-wins   |A|
    //   last-value-wins    |B|
    //
    // ⭐ THE ARITHMETIC IS WRITTEN OUT, which is what separates all four.
    // `both > 0` passes on three of them. The query is FILTER-ONLY so the
    // population is exactly the one the suggestions were drawn from; a
    // text query would be a different population and both counts could
    // legitimately be zero.
    const totalFor = async (...owners: number[]) => {
      const qs = owners.map((o) => `filter=owner:${o}`).join('&');
      const r = await request.get(`/api/v1/search?types=asset&limit=1&${qs}`);
      expect(r.ok(), `search ${qs} → ${r.status()}`).toBeTruthy();
      return ((await r.json()) as { total_count: number }).total_count;
    };
    const nA = await totalFor(a);
    const nB = await totalFor(b);
    const nBoth = await totalFor(a, b);
    expect(
      Math.min(nA, nB),
      'a contributor the lookup OFFERED must own at least one visible asset, or the ' +
        'arithmetic below is satisfied by more than one implementation',
    ).toBeGreaterThan(0);
    expect(
      nBoth,
      `owner:${a} returned ${nA}, owner:${b} returned ${nB}, and both together returned ` +
        `${nBoth}. An asset has exactly ONE owner, so two values of this OR dimension are ` +
        'a value list and the answer is the disjoint union. AND returns 0, first-wins ' +
        `returns ${nA}, last-wins returns ${nB}; only ${nA + nB} is correct.`,
    ).toBe(nA + nB);
  });

  test('contributor: a tick narrows the results to that contributor by exact id', async ({
    page,
    request,
  }) => {
    await openAdvanced(page);
    const refs = await contributorOptions(page);
    const a = refs[0];
    await page.locator(`[data-testid="advanced-contributor-option-${a}"]`).click();
    const emitted = await submitAndReadFilters(page);
    expect(emitted).toContain(`owner:${a}`);

    // The narrowing is real: every hit the same address returns through
    // the API belongs to that owner.
    const r = await request.get(`/api/v1/search?filter=owner:${a}&types=asset&limit=25`);
    expect(r.ok(), `search → ${r.status()}`).toBeTruthy();
    const body = (await r.json()) as { hits: { owner_user_ref?: number }[]; total_count: number };
    expect(body.total_count, 'the contributor was offered, so they own something visible')
      .toBeGreaterThan(0);
    for (const h of body.hits) {
      if (h.owner_user_ref !== undefined) expect(h.owner_user_ref).toBe(a);
    }
  });

  test('⛔ contributor: a selection SURVIVES leaving its own suggestion list', async ({ page }) => {
    // The suggestion list is query-relative, so narrowing by ANOTHER
    // dimension can legitimately take a ticked contributor out of it. If
    // the chips were rendered from the response, that would silently
    // delete an active filter and leave an `owner:` term in the URL with
    // no control on screen.
    await openAdvanced(page);
    const refs = await contributorOptions(page);
    const a = refs[0];
    await page.locator(`[data-testid="advanced-contributor-option-${a}"]`).click();
    await expect(page.locator(`[data-testid="advanced-contributor-chip-${a}"]`)).toBeVisible();

    // Narrow hard on a NON-owner dimension: an extension nothing carries.
    await page.locator(tid('advanced-extension-input')).fill('zzz_no_such_extension');
    await page.locator(tid('advanced-extension-input')).press('Enter');
    await expect(page.locator('[data-testid="advanced-extension-chip-zzz_no_such_extension"]')).toBeVisible();

    // The suggestion list is now empty (or at least no longer offers A)…
    await expect(
      page.locator(`[data-testid="advanced-contributor-option-${a}"]`),
    ).toHaveCount(0, { timeout: 20_000 });
    // …and the CHIP is still there.
    await expect(
      page.locator(`[data-testid="advanced-contributor-chip-${a}"]`),
      'the ticked contributor vanished when the suggestion list stopped offering them. ' +
        'The selection is page state; it is never re-derived from the response.',
    ).toBeVisible();

    // And removing the chip removes it from the address. Asserted in
    // the SAME session, before submitting, because the form does not
    // rehydrate from a back navigation.
    await page.locator(`[data-testid="advanced-contributor-remove-${a}"]`).click();
    await expect(page.locator(`[data-testid="advanced-contributor-chip-${a}"]`)).toHaveCount(0);
    const removed = await submitAndReadFilters(page);
    expect(removed, 'removing the chip must remove the term').not.toContain(`owner:${a}`);

    // Re-built from scratch, the same selection DOES reach the address.
    const kept = await emitWith(page, async (p) => {
      await p.locator(`[data-testid="advanced-contributor-option-${a}"]`).click();
      await expect(p.locator(`[data-testid="advanced-contributor-chip-${a}"]`)).toBeVisible();
      await p.locator(tid('advanced-extension-input')).fill('zzz_no_such_extension');
      await p.locator(tid('advanced-extension-input')).press('Enter');
      await expect(
        p.locator(`[data-testid="advanced-contributor-option-${a}"]`),
      ).toHaveCount(0, { timeout: 20_000 });
    });
    expect(kept, 'the filter must survive into the submitted address').toContain(`owner:${a}`);
  });

  test('⭐ contributor: continuation reaches PAST the first page, with no duplicate and no skip', async ({
    page,
  }) => {
    await openAdvanced(page);
    const first = await contributorOptions(page);

    const more = page.locator(tid('advanced-contributor-more'));
    const hasMore = await more.isVisible().catch(() => false);
    expect(
      hasMore,
      `the stack offered ${first.length} contributors in one page and no continuation. ` +
        'This regression is about the page BOUNDARY, so it needs more qualifying ' +
        'contributors than one response holds — a thin corpus makes it vacuous rather ' +
        'than passing.',
    ).toBeTruthy();

    await more.click();
    // The list GREW rather than being replaced.
    await expect
      .poll(async () => (await contributorOptions(page)).length, { timeout: 20_000 })
      .toBeGreaterThan(first.length);
    const walked = await contributorOptions(page);

    expect(
      new Set(walked).size,
      `continuation produced ${walked.length} rows with ${new Set(walked).size} distinct ` +
        'contributors. A repeat across the boundary means the cursor re-emitted a row it ' +
        'had already passed.',
    ).toBe(walked.length);
    expect(
      walked.slice(0, first.length),
      'the second page must start exactly where the first stopped — a different prefix ' +
        'means the boundary moved, which is a skip on one side and a duplicate on the other',
    ).toEqual(first);

    // A contributor only reachable BEYOND page one is selectable, and
    // emits the same `owner:<user_ref>` as any other.
    const beyond = walked[first.length];
    await page.locator(`[data-testid="advanced-contributor-option-${beyond}"]`).click();
    const emitted = await submitAndReadFilters(page);
    expect(emitted).toContain(`owner:${beyond}`);
  });

  // ── WORKFLOW STATE ─────────────────────────────────────────────────

  test('⛔ workflow: `none` is ONE global control, outside every type section', async ({ page }) => {
    await openAdvanced(page);

    // Rendered with ZERO types selected — `state_id IS NULL` is
    // answerable without one.
    const none = page.locator(tid('advanced-workflow-none'));
    await expect(none).toBeVisible();
    expect(await none.count(), 'there must be exactly one of it').toBe(1);

    await page.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
    await page.locator(`[data-testid="advanced-type-${videoRef}"]`).click();
    await expect(page.locator(`[data-testid="advanced-section-type-${imageRef}"]`)).toBeVisible();
    await expect(page.locator(`[data-testid="advanced-section-type-${videoRef}"]`)).toBeVisible();

    // ⛔ STILL ONE, and NOT inside either section. A per-type copy would
    // claim a scope the wire does not have: `workflow_state:none` carries
    // no domain, so ticking it under Image also returns state-less
    // videos.
    expect(
      await page.locator(tid('advanced-workflow-none')).count(),
      'a second `none` checkbox appeared once two types were selected. `none` is ' +
        '`state_id IS NULL` and is domain-independent, so a per-type copy is a control ' +
        'that lies about its own scope.',
    ).toBe(1);
    for (const ref of [imageRef, videoRef]) {
      expect(
        await page
          .locator(`[data-testid="advanced-section-type-${ref}"] ${tid('advanced-workflow-none')}`)
          .count(),
        `the global \`none\` checkbox was rendered inside the ${ref} section`,
      ).toBe(0);
    }

    await none.check();
    const emitted = await submitAndReadFilters(page);
    expect(emitted).toContain('workflow_state:none');
    expect(
      emitted.filter((f) => f === 'workflow_state:none'),
      'one control, one term',
    ).toHaveLength(1);
  });

  test('⛔ workflow: a concrete state is keyed by its FULL `<domain>/<code>` identity', async ({
    page,
  }) => {
    // ⛔ WHY THIS IS NOT A "TWO POPULATED DOMAINS" FIXTURE. There is no
    // write API for `workflow_states` — `/workflow/states` is read-only
    // and #897's operator management is not built — so a spec cannot
    // create a second domain's vocabulary through the product. What it
    // CAN check is the property that makes a collision impossible: every
    // rendered state is keyed by the full identity, and no domain's
    // states appear under another domain's section. A page that keyed on
    // the bare CODE would fail the second half on any stack where two
    // types define one, and fails the first half on every stack.
    await openAdvanced(page);
    await page.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
    await page.locator(`[data-testid="advanced-type-${videoRef}"]`).click();
    await expect(page.locator(`[data-testid="advanced-section-type-${imageRef}"]`)).toBeVisible();
    await expect(page.locator(`[data-testid="advanced-section-type-${videoRef}"]`)).toBeVisible();

    const identitiesIn = async (ref: number) => {
      const block = page.locator(`[data-testid="advanced-workflow-states-${ref}"]`);
      if ((await block.count()) === 0) return [];
      return (
        await block
          .locator('button')
          .evaluateAll((els) => els.map((e) => e.getAttribute('data-testid')!))
      ).map((i) => i.replace('advanced-workflow-state-', ''));
    };

    const perType: Record<number, string[]> = {};
    for (const ref of [imageRef, videoRef]) perType[ref] = await identitiesIn(ref);

    const populated = Object.values(perType).filter((v) => v.length > 0);
    expect(
      populated.length,
      'no selected type offered any workflow state, so every assertion below is vacuous. ' +
        'The stock install seeds the Image domain.',
    ).toBeGreaterThan(0);

    for (const ref of [imageRef, videoRef]) {
      for (const identity of perType[ref]) {
        expect(
          identity,
          `state ${identity} rendered under type ${ref} without carrying that type's own ` +
            'domain. `workflow_states` is UNIQUE (domain, code), so two types defining ' +
            '`published` define TWO states, and keying on the bare code would tick one ' +
            'and filter by the other.',
        ).toMatch(new RegExp(`^asset:${ref}/`));
      }
    }
    const shared = perType[imageRef].filter((i) => perType[videoRef].includes(i));
    expect(shared, 'one identity was offered under two different domains').toHaveLength(0);

    // The emitted term carries the identity VERBATIM, domain included.
    const first = populated[0][0];
    await page.locator(`[data-testid="advanced-workflow-state-${first}"]`).click();
    const emitted = await submitAndReadFilters(page);
    expect(emitted).toContain(`workflow_state:${first}`);
  });

  test('⛔ workflow: deselecting a type prunes ITS states and leaves `none` alone', async ({
    page,
  }) => {
    await openAdvanced(page);
    await page.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
    const states = page.locator(`[data-testid="advanced-workflow-states-${imageRef}"]`);
    await expect(states).toBeVisible({ timeout: 20_000 });
    const first = (
      await states.locator('button').first().getAttribute('data-testid')
    )!.replace('advanced-workflow-state-', '');

    await page.locator(`[data-testid="advanced-workflow-state-${first}"]`).click();
    await page.locator(tid('advanced-workflow-none')).check();
    await expect(page.locator(tid('advanced-active-count'))).toBeVisible();

    // Take the type away.
    await page.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
    await expect(states).toHaveCount(0);

    const emitted = await submitAndReadFilters(page);
    expect(
      emitted,
      'the concrete state survived its own control being removed — an invisible predicate ' +
        'is the failure the whole `filter=` design exists to avoid',
    ).not.toContain(`workflow_state:${first}`);
    expect(
      emitted,
      '`none` is global, so nothing about a type section may prune it',
    ).toContain('workflow_state:none');
  });

  // ── EXTENSION ──────────────────────────────────────────────────────

  test('⛔ file type: `.PNG` and `png` are ONE term', async ({ page }) => {
    await openAdvanced(page);
    const box = page.locator(tid('advanced-extension-input'));
    for (const spelling of ['.PNG', 'png', ' .png ']) {
      await box.fill(spelling);
      await box.press('Enter');
    }
    await expect(page.locator('[data-testid="advanced-extension-chip-png"]')).toBeVisible();
    expect(
      await page.locator('[data-testid^="advanced-extension-chip-"]').count(),
      'three spellings of one extension produced more than one chip',
    ).toBe(1);

    const emitted = (await submitAndReadFilters(page)).filter((f) => f.startsWith('extension:'));
    expect(
      emitted,
      'three spellings must normalize to exactly one `extension:png`. Two terms would ' +
        'still return the right rows and would produce a different cache key and a ' +
        'different saved-search spelling.',
    ).toEqual(['extension:png']);
  });

  test('⛔ file type: a selected extension SURVIVES leaving the bucket list', async ({ page }) => {
    // `FacetExtension` is not conjunctive, so `ForFacet` drops its own
    // terms when the buckets are computed and a selected extension can
    // legitimately leave its own suggestion list. It must stay ticked.
    await openAdvanced(page);
    const options = page.locator('[data-testid^="advanced-extension-option-"]');
    await expect(options.first()).toBeVisible({ timeout: 20_000 });
    const ext = (await options.first().getAttribute('data-testid'))!.replace(
      'advanced-extension-option-',
      '',
    );
    await page.locator(`[data-testid="advanced-extension-option-${ext}"]`).click();
    await expect(page.locator(`[data-testid="advanced-extension-chip-${ext}"]`)).toBeVisible();

    // Change the QUERY so the bucket source cannot offer it any more.
    await page.locator(tid('advanced-row-value')).first().fill('zzqqxx_no_such_phrase');
    await expect(
      page.locator(`[data-testid="advanced-extension-option-${ext}"]`),
    ).toHaveCount(0, { timeout: 20_000 });
    await expect(
      page.locator(`[data-testid="advanced-extension-chip-${ext}"]`),
      'the ticked extension vanished with its bucket. The selection is page state.',
    ).toBeVisible();

    const emitted = await submitAndReadFilters(page);
    expect(emitted).toContain(`extension:${ext}`);
  });

  // ── FILE SIZE ──────────────────────────────────────────────────────

  test('⛔ file size: the four bound cases, as EXACT digit strings', async ({ page }) => {
    const sizes = (tokens: string[]) => tokens.filter((f) => f.startsWith('file_size:'));

    // Lower only. The default unit is MB.
    expect(
      sizes(await emitWith(page, async (p) => p.locator(tid('advanced-filesize-min')).fill('1'))),
    ).toEqual(['file_size:>=1048576']);

    // Upper only.
    expect(
      sizes(await emitWith(page, async (p) => p.locator(tid('advanced-filesize-max')).fill('2'))),
    ).toEqual(['file_size:<=2097152']);

    // Both — two INDEPENDENT terms carrying different operators, which
    // is what makes them AND rather than a range that ORs into "every
    // asset that has a size at all".
    expect(
      sizes(
        await emitWith(page, async (p) => {
          await p.locator(tid('advanced-filesize-min')).fill('1');
          await p.locator(tid('advanced-filesize-max')).fill('2');
        }),
      ).sort(),
    ).toEqual(['file_size:<=2097152', 'file_size:>=1048576']);

    // Neither. Something else has to be set, or there is nothing to
    // submit and the absence would prove nothing.
    expect(
      sizes(
        await emitWith(page, async (p) => {
          await p.locator(tid('advanced-filesize-unit')).selectOption('B');
          await p.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
        }),
      ),
      'two empty boxes are no bound, not a bound of zero',
    ).toEqual([]);
  });

  test('⛔ file size: exactness past 2^53, and the rounding directions', async ({ page }) => {
    const sizes = (tokens: string[]) => tokens.filter((f) => f.startsWith('file_size:'));
    const bytes = (min: string, max = '') => async (p: Page) => {
      await p.locator(tid('advanced-filesize-unit')).selectOption('B');
      if (min) await p.locator(tid('advanced-filesize-min')).fill(min);
      if (max) await p.locator(tid('advanced-filesize-max')).fill(max);
    };

    // int64 max, verbatim. ⛔ A DIGIT-STRING assertion: routed through a
    // JavaScript `number` this comes back as 9223372036854775808.
    expect(sizes(await emitWith(page, bytes('9223372036854775807')))).toEqual([
      'file_size:>=9223372036854775807',
    ]);

    // int64 max + 1 — REFUSED, and refused silently on the wire: no
    // term, an inline message instead of a clamped bound.
    expect(
      sizes(
        await emitWith(page, async (p) => {
          await bytes('9223372036854775808')(p);
          await expect(p.locator(tid('advanced-filesize-error'))).toBeVisible();
          await p.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
        }),
      ),
      'an out-of-range bound must emit NOTHING. A clamped one would be a filter that ' +
        'quietly became a different filter.',
    ).toEqual([]);

    // Above 2^53, below int64 max: 2^53 + 1, which a double cannot hold.
    expect(sizes(await emitWith(page, bytes('9007199254740993')))).toEqual([
      'file_size:>=9007199254740993',
    ]);

    // ⛔ THE TWO EDGES ROUND OPPOSITE WAYS. 0.9768295288085938 KB is
    // 1000.25 bytes exactly: "at least" admits 1001, "at most" admits
    // 1000. Rounding both the same way would let one end match a file
    // the person excluded.
    expect(
      sizes(
        await emitWith(page, async (p) => {
          await p.locator(tid('advanced-filesize-unit')).selectOption('KB');
          await p.locator(tid('advanced-filesize-min')).fill('0.9768295288085938');
          await p.locator(tid('advanced-filesize-max')).fill('0.9768295288085938');
        }),
      ).sort(),
    ).toEqual(['file_size:<=1000', 'file_size:>=1001']);
  });

  // ── PIXEL DIMENSIONS AND THE NUMBER FAMILY ─────────────────────────

  // ⚠️ REPORT-ONLY, MEASURED, AND IT BOUNDS WHAT THE TWO TESTS BELOW
  // CAN CLAIM. `pixel_width` and `pixel_height` ship with
  // `searchable = false` (migration 00017 — correctly, since a pixel
  // count has no business in `search_text`), and the `field:` predicate
  // in `facet.dimensionSQL` conjuncts `ffd.searchable = TRUE`. So a
  // pixel bound is well-formed, reaches the engine, and matches ZERO
  // rows on a stock install — measured: `field:pixel_width=512` returns
  // 0 while 652 live assets carry that value, and `field:rating>=1`
  // returns 1801.
  //
  // That predicate's own comment says `searchable` is there because "the
  // advanced page renders its rows from exactly that set", which stopped
  // being true when #1173 slice 1 moved the page onto
  // `show_in_advanced_search` (ADR 0092 §3: indexing and participation
  // are two settings). Whether to drop the conjunct is a behavioural
  // change to a shared predicate and belongs to its own decision, so
  // these tests assert what 18d actually owns — the CONTROL, its
  // grouping and the TERM it emits — and deliberately do NOT assert a
  // row count, because a row-count assertion here would be asserting the
  // defect.
  test('⛔ pixel dimensions: the shipped global field appears EXACTLY ONCE, in Media', async ({
    page,
  }) => {
    await openAdvanced(page);
    const row = page.locator(`[data-testid="field-filter-${PIXEL_FIELD}"]`);
    expect(
      await row.count(),
      'a globally-configured pixel field claimed by the media group must not ALSO render ' +
        'in the general section',
    ).toBe(1);
    expect(
      await page.locator(`${tid('advanced-section-media')} [data-testid="field-filter-${PIXEL_FIELD}"]`).count(),
      'the one occurrence must be the media one',
    ).toBe(1);

    // Selecting a type does not duplicate it either.
    await page.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
    await expect(page.locator(`[data-testid="advanced-section-type-${imageRef}"]`)).toBeVisible();
    expect(await row.count(), 'selecting a type duplicated the global pixel field').toBe(1);
  });

  test('pixel dimensions: lower, upper and both bounds compile to the field grammar', async ({
    page,
  }) => {
    const pixels = (tokens: string[]) => tokens.filter((f) => f.startsWith(`field:${PIXEL_FIELD}`));

    expect(
      pixels(
        await emitWith(page, async (p) =>
          p.locator(`[data-testid="field-from-${PIXEL_FIELD}"]`).fill('1920'),
        ),
      ),
    ).toEqual([`field:${PIXEL_FIELD}>=1920`]);

    expect(
      pixels(
        await emitWith(page, async (p) =>
          p.locator(`[data-testid="field-to-${PIXEL_FIELD}"]`).fill('4096'),
        ),
      ),
    ).toEqual([`field:${PIXEL_FIELD}<=4096`]);

    expect(
      pixels(
        await emitWith(page, async (p) => {
          await p.locator(`[data-testid="field-from-${PIXEL_FIELD}"]`).fill('1920');
          await p.locator(`[data-testid="field-to-${PIXEL_FIELD}"]`).fill('4096');
        }),
      ).sort(),
    ).toEqual([`field:${PIXEL_FIELD}<=4096`, `field:${PIXEL_FIELD}>=1920`].sort());
  });

  test('⛔ a TYPE-SCOPED numeric field honours applies_to and is never duplicated', async ({
    page,
  }) => {
    await openAdvanced(page);
    const probe = page.locator(`[data-testid="field-filter-${NUM_PROBE_CODE}"]`);
    const chip = page.locator(`[data-testid="advanced-type-${imageRef}"]`);

    // Absent without its type — and NOT claimed by the media group,
    // which only claims GLOBAL pixel fields.
    await expect(
      probe,
      'a field scoped to one resource type must not appear before that type is chosen',
    ).toHaveCount(0);

    await chip.click();
    await expect(probe).toHaveCount(1, { timeout: 20_000 });
    expect(
      await page.locator(`${tid('advanced-section-media')} [data-testid="field-filter-${NUM_PROBE_CODE}"]`).count(),
      'a type-scoped numeric field must follow its type section, not be regrouped into ' +
        'the media group — the grouping reads the field configuration, it never overrides it',
    ).toBe(0);

    // It bounds like any other number field…
    const withProbe = await emitWith(page, async (p) => {
      await p.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
      await expect(p.locator(`[data-testid="field-filter-${NUM_PROBE_CODE}"]`)).toHaveCount(1, {
        timeout: 20_000,
      });
      await p.locator(`[data-testid="field-from-${NUM_PROBE_CODE}"]`).fill('5');
    });
    expect(withProbe).toContain(`field:${NUM_PROBE_CODE}>=5`);

    // …and its bound is PRUNED when its type goes away.
    const pruned = await emitWith(page, async (p) => {
      const c = p.locator(`[data-testid="advanced-type-${imageRef}"]`);
      await c.click();
      await expect(p.locator(`[data-testid="field-filter-${NUM_PROBE_CODE}"]`)).toHaveCount(1, {
        timeout: 20_000,
      });
      await p.locator(`[data-testid="field-from-${NUM_PROBE_CODE}"]`).fill('5');
      await c.click();
      await expect(p.locator(`[data-testid="field-filter-${NUM_PROBE_CODE}"]`)).toHaveCount(0);
      // Give the form something else to submit, so the absence below is
      // an absence from a real query rather than from an empty one.
      await p.locator(tid('advanced-workflow-none')).check();
    });
    expect(
      pruned.filter((f) => f.startsWith(`field:${NUM_PROBE_CODE}`)),
      'a bound held for a field with no control on screen is an invisible predicate',
    ).toEqual([]);
    expect(pruned, 'the probe query was not empty').toContain('workflow_state:none');
  });

  // ── MIXED ARMS ─────────────────────────────────────────────────────

  test('⛔ an asset-only filter takes posts and collections off the page', async ({ request }) => {
    // ⛔ NOT A ZERO-ONLY PREMISE. The unfiltered query must PROVE it
    // returns all three kinds first; otherwise "no posts afterwards" is
    // satisfied by a query that never had any. And every filtered case
    // must still return ASSETS, or "no posts" is satisfied by a filter
    // that emptied the page entirely.
    //
    // ⚠️ THE INSTRUMENT IS THE HITS, NOT `types_matched`. That field is
    // assigned `types` — the types the caller REQUESTED — in
    // `search/query.go`, so it names `post` on every response whether or
    // not a post came back. A first draft of this test read it and
    // reported a leak that was not there.
    const kinds = async (qs: string) => {
      const r = await request.get(`/api/v1/search?${qs}`);
      expect(r.ok(), `search ${qs} → ${r.status()}`).toBeTruthy();
      const j = (await r.json()) as { hits: { type: string }[] };
      return new Set(j.hits.map((h) => h.type));
    };

    const before = await kinds('q=art&limit=50');
    for (const kind of ['asset', 'post', 'collection']) {
      expect(
        [...before],
        `the unfiltered query returned no ${kind}s, so the assertion below would be vacuous`,
      ).toContain(kind);
    }

    // Only an asset has a file, an extension or a workflow state. A post
    // is a set of members and a collection is a container, so both fall
    // through to `satisfiable=false` and leave the page — the
    // POSITIVE-NARROWING direction, where an entity that cannot answer
    // leaving IS the answer. An arm that treated an active constraint as
    // no constraint would return every post and collection BESIDE the
    // qualifying assets, which is a filter that made the result set
    // LARGER.
    for (const filter of [
      'file_size:>=1',
      'extension:png',
      'workflow_state:asset:1/published',
    ]) {
      const after = await kinds(`q=art&limit=50&filter=${encodeURIComponent(filter)}`);
      expect(
        [...after],
        `${filter} returned no assets at all, so "no posts" below proves nothing about ` +
          'the arm and everything about the corpus',
      ).toContain('asset');
      expect(
        [...after],
        `${filter} returned posts. A filter about FILES that passed a post through would ` +
          'be treating an active constraint as no constraint.',
      ).not.toContain('post');
      expect([...after], `${filter} returned collections`).not.toContain('collection');
    }
  });

  // ── COMPOSITION ────────────────────────────────────────────────────

  test('⛔ the counted URL and the submitted URL are the SAME query', async ({ page }) => {
    await openAdvanced(page);

    // Build something using several of the new dimensions at once.
    await page.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
    await page.locator(tid('advanced-workflow-none')).check();
    await page.locator(tid('advanced-filesize-min')).fill('1');
    await page.locator(tid('advanced-extension-input')).fill('png');
    await page.locator(tid('advanced-extension-input')).press('Enter');
    await page.locator(`[data-testid="field-from-${PIXEL_FIELD}"]`).fill('100');

    // Capture the address the COUNT is fetched with.
    const counted = await page.waitForRequest(
      (r) => r.url().includes('/api/v1/search?') && r.url().includes('limit=1'),
      { timeout: 20_000 },
    );
    const countURL = new URL(counted.url());

    const submitted = new URL(
      await (async () => {
        await page.locator(tid('advanced-submit')).click();
        await page.waitForURL(/\/search\?/, { timeout: 20_000 });
        return page.url();
      })(),
    );

    countURL.searchParams.delete('limit');
    const countParams = [...countURL.searchParams.entries()].map(([k, v]) => `${k}=${v}`).sort();
    const submitParams = [...submitted.searchParams.entries()].map(([k, v]) => `${k}=${v}`).sort();
    expect(
      countParams,
      'the previewed number and the page the button reaches must be the SAME query. Two ' +
        'serializers are two things that can drift, and a count that drifts from its ' +
        'result set is the #902/#1066 shape.',
    ).toEqual(submitParams);
  });

  test('⛔ Save Search keeps every emitted filter', async ({ page }) => {
    // The filters have to RETURN something: the Save Search control
    // lives in the results counter, which only renders once the query
    // has hits. A query that narrows to nothing hides the button, and
    // the test would then be reporting an empty corpus as a missing
    // feature.
    await openAdvanced(page);
    await page.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
    await page.locator(tid('advanced-filesize-unit')).selectOption('B');
    await page.locator(tid('advanced-filesize-min')).fill('1');
    await page.locator(tid('advanced-extension-input')).fill('png');
    await page.locator(tid('advanced-extension-input')).press('Enter');
    const emitted = await submitAndReadFilters(page);
    expect(emitted.length).toBeGreaterThan(2);

    await expect(
      page.locator(tid('save-search')),
      'the composed query returned no results, so the save control never rendered — that ' +
        'is a corpus problem, not a parity one',
    ).toBeVisible({ timeout: 20_000 });
    await page.locator(tid('save-search')).click();
    const posted = page.waitForRequest(
      (r) => r.url().includes('/api/v1/search/saved') && r.method() === 'POST',
      { timeout: 20_000 },
    );
    await page.getByRole('dialog').locator('input[type="text"]').first().fill('18d parity probe');
    await page.getByRole('dialog').getByRole('button', { name: /save/i }).click();
    const body = JSON.parse((await posted).postData() ?? '{}') as { filters?: string[] };
    expect(
      (body.filters ?? []).sort(),
      'a saved search that dropped a filter would replay WIDER than the page it came from, ' +
        'which is #1368 with new dimensions',
    ).toEqual([...emitted].sort());
  });

  test('Start over clears every built-in dimension, including the unit', async ({ page }) => {
    await openAdvanced(page);
    // The contributor is picked FIRST, on purpose: the suggestion list
    // is query-relative, so a 3 GB lower bound legitimately empties it
    // and there would be nothing left to tick.
    const refs = await contributorOptions(page);
    await page.locator(`[data-testid="advanced-contributor-option-${refs[0]}"]`).click();
    await page.locator(`[data-testid="advanced-type-${imageRef}"]`).click();
    await page.locator(tid('advanced-workflow-none')).check();
    await page.locator(tid('advanced-filesize-unit')).selectOption('GB');
    await page.locator(tid('advanced-filesize-min')).fill('3');
    await page.locator(tid('advanced-extension-input')).fill('png');
    await page.locator(tid('advanced-extension-input')).press('Enter');
    await expect(page.locator(tid('advanced-active-count'))).toBeVisible();

    await page.locator(tid('advanced-reset')).click();

    await expect(page.locator(tid('advanced-active-count'))).toHaveCount(0);
    await expect(page.locator(tid('advanced-workflow-none'))).not.toBeChecked();
    await expect(page.locator(tid('advanced-filesize-min'))).toHaveValue('');
    await expect(
      page.locator(tid('advanced-filesize-unit')),
      'an empty box beside a remembered GB is a form that is not actually blank',
    ).toHaveValue('MB');
    await expect(page.locator('[data-testid^="advanced-extension-chip-"]')).toHaveCount(0);
    await expect(page.locator('[data-testid^="advanced-contributor-chip-"]')).toHaveCount(0);
  });

  // ── VIEWPORTS ──────────────────────────────────────────────────────

  test('the built-in controls are reachable at 390px, clear of the sticky bar', async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openAdvanced(page);
    // ⚠️ WAIT FOR THE ASYNCHRONOUS LISTS BEFORE MEASURING ANY GEOMETRY.
    // The contributor and file-type suggestions arrive after a debounce
    // and push everything below them down by ~200px. Measuring first
    // reports an occlusion that the settled layout does not have — a
    // false positive that reads exactly like the real bug this test is
    // for.
    await expect(page.locator(tid('advanced-contributor-options'))).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.locator('[data-testid^="advanced-extension-option-"]').first()).toBeVisible({
      timeout: 20_000,
    });
    await page.waitForTimeout(500);

    for (const id of [
      'advanced-contributor-input',
      'advanced-workflow-none',
      'advanced-extension-input',
      'advanced-filesize-min',
      'advanced-filesize-unit',
    ]) {
      const el = page.locator(tid(id));
      await expect(el, `${id} is not rendered at 390px`).toBeVisible();
      // ⛔ `block: 'end'` ON PURPOSE. It is the worst case — the browser
      // parks the element against the bottom edge, which is exactly
      // where the sticky action bar floats — and it is what a focus or a
      // tab does when the control is below the fold. `scroll-margin` is
      // what has to reserve the bar's height, and this is the scroll
      // that proves it does.
      await el.evaluate((e) => e.scrollIntoView({ block: 'end' }));
      await page.waitForTimeout(150);
      const box = (await el.boundingBox())!;
      const footer = (await page.locator(tid('advanced-submit')).boundingBox())!;
      expect(
        box.y + box.height,
        `${id} came to rest under the sticky action bar at 390px (bottom ${
          box.y + box.height
        }, bar top ${footer.y}), where it cannot be tapped`,
      ).toBeLessThanOrEqual(footer.y);
    }

    // The panel never scrolls the BODY sideways.
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, 'the page scrolls horizontally at 390px').toBeLessThanOrEqual(1);
  });
});
