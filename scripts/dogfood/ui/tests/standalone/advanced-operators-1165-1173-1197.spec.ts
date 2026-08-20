// #1165 / #1197 / #1173 slice 1 — the advanced page reaches its shape.
//
// Three behaviours, and the reason they are one file is that they are
// one form: the operator grammar is what gives text and date fields a
// control, the sticky button is what runs the form, and the live count
// is what the form says it will return.
//
// # 1. #1197 — the sticky Search button
//
// It used to enable off the LAST SUBMITTED query, which only moved when
// the builder's own inner button ran. Typing a condition and reaching
// for the page's primary action found it disabled.
//
// The assertion here is deliberately on BEHAVIOUR rather than on the
// `disabled` attribute: a button that is enabled and navigates nowhere
// would pass an attribute check and still be broken. So the test types,
// clicks the outer button, and asserts the condition reached the URL.
//
// # 2. #1165 — text-contains and date-range operators
//
// The grammar carries `~`, `>=` and `<=` beside `=`. What is checked on
// the page is that the widgets compile to those tokens, that the tokens
// survive the URL, and — the assertion the issue names — that an unknown
// operator ERRORS rather than matching everything. The last one is
// driven through the API rather than the form, because the form cannot
// produce a malformed operator and the risk is a hand-written or
// federated token, which is exactly what the API takes.
//
// # 3. #1173 — per-type sections and the live count
//
// A field scoped to one resource type must appear when that type is
// selected and vanish when it is not. The seed has no such field — every
// active field definition on a stock install is global — so this file
// MAKES one, the same way advanced-vocab-1191 makes its large
// vocabulary, and archives it again at the end.
//
// ⛔ THE COUNT IS THE DANGEROUS PART. It is a derived copy of a result
// set, so if it is computed by any path other than the one that produces
// the results it becomes an oracle: narrow a filter until the number
// moves and you have recovered a value that was withheld. That is the
// #902 / #1066 shape. The assertion is therefore EQUALITY against what
// the results page actually reports for the SAME viewer, not "the count
// is plausible" and not "the count is non-zero".

import { test, expect, type APIRequestContext, type Page } from '@playwright/test';
import { loginAsAdminViaAPI, LOGGED_OUT } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

const PROBE_CODE = 'probe_1173_image_only';
const PROBE_LABEL = 'Image-only note (1173 probe)';

/** The resource type the probe is scoped to. 1 is Image on every
 *  install — it is seeded by the baseline migration — and the test
 *  resolves the ref from the API anyway rather than trusting that. */
const PROBE_TYPE_NAME = 'Image';

/** Global fields the stock seed ships, used as the "always visible"
 *  control. `copyright` is a text field and `license_expires` a date
 *  one, so between them they also carry both #1165 operators. */
const TEXT_FIELD = 'copyright';
const DATE_FIELD = 'license_expires';

async function findProbe(request: APIRequestContext, status?: string) {
  const url = status ? `/api/v1/fields?status=${status}` : '/api/v1/fields';
  const r = await request.get(url);
  expect(r.ok(), `GET ${url} → ${r.status()}`).toBeTruthy();
  const rows = (await r.json()) as { id: string; code: string }[];
  return rows.find((f) => f.code === PROBE_CODE);
}

async function imageTypeRef(request: APIRequestContext): Promise<number> {
  const r = await request.get('/api/v1/asset_types');
  expect(r.ok(), `GET /asset_types → ${r.status()}`).toBeTruthy();
  const rows = (await r.json()) as { ref: number; name?: string | null }[];
  const hit = rows.find((t) => t.name === PROBE_TYPE_NAME);
  expect(hit, `no asset type named ${PROBE_TYPE_NAME}`).toBeTruthy();
  return hit!.ref;
}

/**
 * Create the type-scoped probe, or revive the tombstone a previous run
 * left. `DELETE /fields/{id}` archives rather than erases, so a crashed
 * run leaves an ARCHIVED row that the advanced page already filters out
 * — a failed cleanup cannot leak the probe onto the page.
 *
 * A `text` type on purpose: it renders whatever its vocabulary is,
 * whereas a `select` with no values is skipped by the page and the
 * section assertions would then be testing the wrong absence.
 */
async function ensureProbeField(request: APIRequestContext, typeRef: number): Promise<string> {
  await loginAsAdminViaAPI(request);
  const shape = {
    status: 'active',
    label: PROBE_LABEL,
    searchable: true,
    show_in_advanced_search: true,
    applies_to: [typeRef],
  };
  const revive = async (id: string) => {
    const r = await request.patch(`/api/v1/fields/${id}`, { data: shape });
    expect(r.ok(), `revive probe → ${r.status()} ${await r.text()}`).toBeTruthy();
    return id;
  };

  const existing = (await findProbe(request)) ?? (await findProbe(request, 'archived'));
  if (existing) return revive(existing.id);

  const r = await request.post('/api/v1/fields', {
    data: {
      code: PROBE_CODE,
      label: PROBE_LABEL,
      type: 'text',
      subject_kind: 'asset',
      display_order: 9100,
      ...shape,
    },
  });
  if (r.status() === 201) return ((await r.json()) as { id: string }).id;
  const raced = (await findProbe(request)) ?? (await findProbe(request, 'archived'));
  expect(raced, `create probe → ${r.status()} ${await r.text()}`).toBeTruthy();
  return revive(raced!.id);
}

// Serial for the same reason advanced-vocab-1191 is: `beforeAll` runs
// once per worker, and two workers racing to create the same
// uniquely-coded field is a collision rather than a test.
test.describe.configure({ mode: 'serial' });

let probeId = '';
let typeRef = 0;
let priorPublicMode: boolean | undefined;

/**
 * Read / set / restore the instance's anonymous-browsing switch.
 *
 * ⚠️ INSTANCE CONFIG IS A FIXTURE, and this file learned it the hard
 * way: the anonymous arm below passed locally and 401'd in CI, because
 * the coding stack happens to have public mode ON and CI's dogfood stack
 * has it OFF. A spec that asserts anonymous behaviour cannot inherit
 * that setting from whichever stack it lands on — it has to own it, the
 * same way it owns the field definition it filters against.
 *
 * The prior value is captured before anything is changed and restored in
 * `afterAll` EVEN ON FAILURE, because the dogfood stack is persistent:
 * a spec that left the toggle flipped would silently change the instance
 * every later spec runs against. Same read/set/restore contract as
 * collection-public-tier-1195.
 */
async function setPublicMode(request: APIRequestContext, on: boolean) {
  const r = await request.patch('/api/v1/admin/system/public-mode', { data: { enabled: on } });
  expect(r.status(), `public mode must be settable to ${on}`).toBe(200);
  expect(((await r.json()) as { enabled: boolean }).enabled).toBe(on);
}

test.beforeAll(async ({ request }) => {
  typeRef = await imageTypeRef(request);
  probeId = await ensureProbeField(request, typeRef);

  const mode = await request.get('/api/v1/admin/system/public-mode');
  expect(mode.status(), 'public-mode state must be readable as admin').toBe(200);
  priorPublicMode = ((await mode.json()) as { enabled: boolean }).enabled;
});

test.afterAll(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  // Restored FIRST and unconditionally: the early return below is about
  // the probe field, and letting it skip the switch would leave the
  // instance altered for every spec that runs after this one.
  if (priorPublicMode !== undefined) {
    await request
      .patch('/api/v1/admin/system/public-mode', { data: { enabled: priorPublicMode } })
      .catch(() => undefined);
  }
  if (!probeId) return;
  await request.delete(`/api/v1/fields/${probeId}`);
});

async function openAdvanced(page: Page) {
  await page.goto('/search/advanced');
  await expect(page.locator(tid('advanced-search-page'))).toBeVisible();
  // The page fetches /fields on mount; wait for a field it always has.
  await expect(page.locator(`[data-testid="field-filter-${TEXT_FIELD}"]`)).toBeVisible();
}

/** The count the page previews, as a number. */
async function previewedCount(page: Page): Promise<number> {
  const el = page.locator(tid('advanced-result-count'));
  await expect(el).toBeVisible();
  await expect(el).not.toHaveText(/^\s*$/);
  // Settle: the count is debounced, so read it once it has stopped
  // moving rather than at an arbitrary instant.
  let last = '';
  for (let i = 0; i < 20; i++) {
    const now = (await el.textContent())?.trim() ?? '';
    if (now === last && /\d/.test(now)) break;
    last = now;
    await page.waitForTimeout(250);
  }
  const m = last.replace(/,/g, '').match(/(\d+)/);
  expect(m, `no number in previewed count ${JSON.stringify(last)}`).toBeTruthy();
  return Number(m![1]);
}

test.describe('advanced search — operators, sections, live count', () => {
  test('#1197: a typed condition makes the sticky Search RUN, not just enable', async ({
    page,
  }) => {
    await openAdvanced(page);

    // Type into the builder and do NOT press its inner button.
    await page.locator(tid('advanced-row-value')).first().fill('aurora');

    // Behaviour, not the attribute: click the page's own action and
    // assert the typed condition reached the address it navigated to.
    await page.locator(tid('advanced-submit')).click();
    await page.waitForURL(/\/search\?/, { timeout: 15_000 });
    const q = decodeURIComponent(new URL(page.url()).search);
    expect(
      q,
      'the outer Search must run what is TYPED. Before #1197 the compiled query ' +
        'only moved on the builder\'s own submit, so this button was disabled and ' +
        'clicking it did nothing.',
    ).toContain('title:aurora');
  });

  test('#1165: a contains filter round-trips through the URL and narrows', async ({ page }) => {
    await openAdvanced(page);

    const all = await previewedCount(page).catch(() => -1);
    expect(all, 'an untouched form should preview nothing').toBe(-1);

    await page.locator(`[data-testid="field-contains-${TEXT_FIELD}"]`).fill('aurora');
    const previewed = await previewedCount(page);
    expect(previewed).toBeGreaterThan(0);

    await page.locator(tid('advanced-submit')).click();
    await page.waitForURL(/\/search\?/, { timeout: 15_000 });
    const q = decodeURIComponent(new URL(page.url()).search);
    expect(q, 'the contains operator must survive the URL verbatim').toContain(
      `filter=field:${TEXT_FIELD}~aurora`,
    );
  });

  test('#1165: two date bounds NARROW each other rather than widening', async ({ page }) => {
    await openAdvanced(page);
    const from = page.locator(`[data-testid="field-from-${DATE_FIELD}"]`);
    const to = page.locator(`[data-testid="field-to-${DATE_FIELD}"]`);

    await from.fill('2026-01-01');
    const lowerOnly = await previewedCount(page);
    expect(lowerOnly, 'the seed carries licence dates through 2026').toBeGreaterThan(0);

    await to.fill('2026-06-30');
    const bounded = await previewedCount(page);

    // ⛔ THE ASSERTION THAT MATTERS. If the two bounds ORed — which is
    // what the `field:` dimension did before #1165, because every term
    // in it shared one OR group regardless of which field or operator
    // it named — then adding the upper bound would have WIDENED the
    // result set to every row carrying a date at all.
    expect(
      bounded,
      `adding an upper bound moved the count ${lowerOnly} → ${bounded}. A range must ` +
        'narrow; a count that grew means the two bounds ORed, which matches every ' +
        'row that has a date at all.',
    ).toBeLessThanOrEqual(lowerOnly);

    await page.locator(tid('advanced-submit')).click();
    await page.waitForURL(/\/search\?/, { timeout: 15_000 });
    const q = decodeURIComponent(new URL(page.url()).search);
    expect(q).toContain(`filter=field:${DATE_FIELD}>=2026-01-01`);
    expect(q).toContain(`filter=field:${DATE_FIELD}<=2026-06-30`);
  });

  test('#1165: an unknown operator ERRORS rather than matching everything', async ({ request }) => {
    // Driven through the API, because the form cannot emit one of these
    // and the real source of a malformed token is a hand-edited URL, a
    // saved query or a federated peer.
    for (const bad of [
      `field:${DATE_FIELD}>2026-01-01`, // exclusive bound: not defined
      `field:${TEXT_FIELD}!=x`, // not-equal: not defined
      `field:${DATE_FIELD}>=whenever`, // unparseable bound
      `field:${TEXT_FIELD}`, // no operator at all
    ]) {
      const r = await request.get(`/api/v1/search?filter=${encodeURIComponent(bad)}`);
      expect(
        r.status(),
        `filter=${bad} must be refused. Accepting it would run SOME query, and ` +
          'whichever one it ran would not be the one that was asked for — a filter ' +
          'that silently matches everything is worse than one that errors.',
      ).toBe(400);
    }

    // And a WELL-FORMED one beside them still works, so the refusal is
    // the operator's and not the dimension's.
    const ok = await request.get(
      `/api/v1/search?filter=${encodeURIComponent(`field:${TEXT_FIELD}~aurora`)}&limit=1`,
    );
    expect(ok.status()).toBe(200);
  });

  test('#1173: a type-scoped field appears with its type and vanishes without it', async ({
    page,
  }) => {
    await openAdvanced(page);
    const probe = page.locator(`[data-testid="field-filter-${PROBE_CODE}"]`);
    const global = page.locator(`[data-testid="field-filter-${TEXT_FIELD}"]`);
    const chip = page.locator(`[data-testid="advanced-type-${typeRef}"]`);

    // Global fields are present from the start; the scoped one is not.
    await expect(global).toBeVisible();
    await expect(probe).toHaveCount(0);

    await chip.click();
    await expect(probe).toBeVisible();
    await expect(global, 'global fields stay visible in every scope').toBeVisible();

    await chip.click();
    await expect(
      probe,
      'deselecting the type must take its fields away again — a filter with no ' +
        'control on screen is a predicate nobody can see',
    ).toHaveCount(0);
    await expect(global).toBeVisible();
  });

  test('#1173: the type scope is itself a filter, in the same grammar', async ({ page }) => {
    await openAdvanced(page);
    await page.locator(`[data-testid="advanced-type-${typeRef}"]`).click();
    await page.locator(tid('advanced-submit')).click();
    await page.waitForURL(/\/search\?/, { timeout: 15_000 });
    const q = decodeURIComponent(new URL(page.url()).search);
    expect(
      q,
      'selecting a resource type must compose through the existing asset_type ' +
        'dimension rather than through a parameter of the advanced page\'s own',
    ).toContain(`filter=type:${typeRef}`);
  });

  test('#1173: a field hidden from the form never appears in any section', async ({
    page,
    request,
  }) => {
    // Participation composes with scoping rather than being overridden
    // by it: a field taken off the form must not re-enter through its
    // type's section.
    await loginAsAdminViaAPI(request);
    const off = await request.patch(`/api/v1/fields/${probeId}`, {
      data: { show_in_advanced_search: false },
    });
    expect(off.ok(), `hide probe → ${off.status()}`).toBeTruthy();
    try {
      await openAdvanced(page);
      await page.locator(`[data-testid="advanced-type-${typeRef}"]`).click();
      await expect(page.locator(`[data-testid="field-filter-${TEXT_FIELD}"]`)).toBeVisible();
      await expect(
        page.locator(`[data-testid="field-filter-${PROBE_CODE}"]`),
        'show_in_advanced_search=false must win over applies_to',
      ).toHaveCount(0);
    } finally {
      await request.patch(`/api/v1/fields/${probeId}`, {
        data: { show_in_advanced_search: true },
      });
    }
  });

  test('⛔ #1173: the live count EQUALS what the results page returns', async ({ page }) => {
    await openAdvanced(page);
    await page.locator(`[data-testid="field-contains-${TEXT_FIELD}"]`).fill('aurora');
    const previewed = await previewedCount(page);

    await page.locator(tid('advanced-submit')).click();
    await page.waitForURL(/\/search\?/, { timeout: 15_000 });

    // Read the number the RESULTS page reports for the same address.
    const reported = page.locator(tid('search-total-count'));
    let actual: number;
    if ((await reported.count()) > 0) {
      await expect(reported).toBeVisible();
      actual = Number(((await reported.textContent()) ?? '').replace(/[^\d]/g, ''));
    } else {
      // No dedicated testid on this surface — fall back to the body
      // text, which is what a person reads anyway.
      await page.waitForTimeout(2500);
      const body = await page.locator('body').innerText();
      const m = body.replace(/,/g, '').match(/(\d+)\s+results?/i);
      expect(m, 'the results page reported no count to compare against').toBeTruthy();
      actual = Number(m![1]);
    }

    expect(
      actual,
      `the page previewed ${previewed} results and the search returned ${actual}. ` +
        'A count is a derived copy of a result set: if the two are computed by ' +
        'different paths the number becomes an oracle the list is not, which is ' +
        'the #902/#1066 shape.',
    ).toBe(previewed);
  });

  test('⛔ #1173: the count is computed for THIS viewer, not for an admin', async ({
    browser,
    request,
  }) => {
    // The same filter, two viewers. The count must differ — a preview
    // that showed everyone the privileged number would be reporting how
    // many rows the caller is NOT allowed to see.
    //
    // # Why the filter is `sensitivity:` and not a `field:` term
    //
    // The comparison needs content the admin can see and a stranger
    // cannot, and it has to be there on EVERY stack this runs on. A
    // hardcoded field value cannot promise that: CI seeds a
    // coverage-selected subset (~148 of 1,947 assets), so whether any
    // non-public asset in it happens to carry a particular word in its
    // copyright is luck. A test whose premise is luck reports a thin
    // seed as a security regression.
    //
    // `sensitivity` is a REQUIRED coverage dimension (seed/coverage.go's
    // dimAssetSens), and the seed step fails outright if a required
    // class is missing — so a restricted asset is guaranteed to exist.
    // It is the same Selection, the same engine and the same
    // `total_count` the advanced page previews, so what is under test is
    // unchanged; only the fixture's guarantee got stronger.
    const filter = encodeURIComponent('sensitivity:restricted');

    await loginAsAdminViaAPI(request);

    // Anonymous browsing ON, so the anonymous leg is refused (or not) by
    // the SEARCH's own read rule rather than by the instance switch. A
    // 401 here would not be evidence about the count at all — it would
    // mean the request never reached the query, which is exactly how
    // this test failed in CI while passing locally.
    await setPublicMode(request, true);

    const asAdmin = await request.get(`/api/v1/search?filter=${filter}&limit=1`);
    expect(asAdmin.ok(), `admin search → ${asAdmin.status()}`).toBeTruthy();
    const adminTotal = ((await asAdmin.json()) as { total_count: number }).total_count;

    // A precondition, stated separately so a thin seed says so in those
    // words instead of arriving as a confusing `0 < 0`.
    expect(
      adminTotal,
      'the seeded stack must hold at least one restricted asset for this comparison — ' +
        'sensitivity is a required coverage class, so zero here is a SEED failure ' +
        'rather than a count failure',
    ).toBeGreaterThan(0);

    const anon = await browser.newContext({ storageState: LOGGED_OUT });
    try {
      const r = await anon.request.get(`/api/v1/search?filter=${filter}&limit=1`);
      expect(r.ok(), `anonymous search → ${r.status()}`).toBeTruthy();
      const anonTotal = ((await r.json()) as { total_count: number }).total_count;
      expect(
        anonTotal,
        `anonymous counted ${anonTotal} and the admin counted ${adminTotal}. A count ` +
          'that ignored the viewer would let a stranger measure the restricted ' +
          'corpus one filter at a time.',
      ).toBeLessThan(adminTotal);
    } finally {
      await anon.close();
    }
  });
});
