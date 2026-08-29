// saved-search-filters-1368.spec.ts
//
// #1368: a saved search must replay the search it was saved from, not a
// wider one.
//
// ⭐ THIS IS THE FAIL-BEFORE REGRESSION FOR THE SPRINT, and it is the
// only test in it that is. Everything it touches — the Save Search
// button, its dialog, `GET /api/v1/search/saved`, `run-now` — exists on
// `dev` and on the branch, unchanged, so this file runs identically
// against both and its verdict is about BEHAVIOUR rather than about
// which symbols exist.
//
// # ⛔ WHY IT DRIVES THE BUTTON AND NOT THE ENDPOINT
//
// The tempting shape is to POST /api/v1/search/saved by hand and then
// run it. That test fails in BOTH directions and therefore guards
// nothing. `dev`'s body carries `{name, dsl, notify_channel,
// notify_interval_minutes}` and no filter channel at all, so a
// hand-built request in that shape stays wide after the fix (the server
// was never told the filters), and a hand-built request in the NEW shape
// cannot run on `dev` (which ignores the member). The only request that
// is meaningful on both is the one THE PAGE ITSELF SENDS, which is why
// the flow below goes through the real dialog.
//
// # ⛔ WHAT MAKES A COUNT A DISCRIMINATOR HERE
//
// `run-now` answers with `hit_count` and no ids (handler.go's runNow),
// so the executor's own verdict can only be a number. A number is only
// evidence when the fixture makes it one, so this file manufactures its
// corpus rather than borrowing one:
//
//   - every row carries a token that exists in no catalogue, so
//     `q=<token>` is exactly this file's rows on a 162-asset CI database
//     and on a 2,000-asset workstation alike;
//   - the rows are split across TWO extensions, so the filtered and
//     unfiltered populations are DIFFERENT by construction and the
//     difference is asserted before anything is saved;
//   - both populations sit far below the executor's per-run limit
//     (execute.go pins 100) and below the search page size, so no cap
//     can mask the gap;
//   - the executor pins `types=asset`, so the interactive search asks
//     for assets too and the comparison is like for like.
//
// ⭐ AND THE COUNT IS NOT THE ONLY ASSERTION. The stored DSL is read
// back and replayed through `/api/v1/search?dsl=…`, which DOES return
// ids, so the file also asserts SET equality between the search on
// screen and the query that was actually persisted. That surface is on
// `dev` too, and on `dev` it fails for the same reason the count does.
//
// # What each half proves if it fails
//
//   hit_count  ≠ filtered  the SAVED QUERY is not the search that was
//                          saved — the digest the owner is emailed will
//                          contain hits their own search excludes.
//   id set     ≠ filtered  the stored DSL does not reconstruct the
//                          selection, i.e. the round trip is lossy even
//                          though the executor happened to agree.

import { test, expect, type Page } from '../../helpers/test';
import type { APIRequestContext } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';

/** In every fixture title and nowhere in any catalogue, so
 *  `/search?q=<TOKEN>` is exactly this file's rows on any corpus. */
const TOKEN = `savedfilterfixture${Date.now()}`;

/** The extension the saved search is narrowed BY, and the one it must
 *  exclude. Two populations, deliberately unequal — see the header. */
const KEPT_EXT = 'txt';
const DROPPED_EXT = 'md';
const KEPT_ROWS = 9;
const DROPPED_ROWS = 5;
const TOTAL_ROWS = KEPT_ROWS + DROPPED_ROWS;

/** The saved search's name, which is also how it is found again through
 *  the ordinary list endpoint. */
const SAVED_NAME = `${TOKEN} narrowed`;

const assetIds: string[] = [];
let savedSearchID = '';

async function makeAsset(request: APIRequestContext, i: number, ext: string): Promise<string> {
  // Unique bytes per asset: byte-identical uploads by one owner are
  // COLLAPSED by the content-address unique index, and a collapsed row
  // would quietly change the population this file's arithmetic depends
  // on.
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`${TOKEN} ${ext} ${i} ${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), `upload ${ext} ${i}`).toBe(201);
  const { hash } = (await up.json()) as { hash: string };

  const r = await request.post('/api/v1/assets', {
    data: {
      title: `${TOKEN} plate ${ext} ${i}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: ext,
    },
  });
  expect(r.status(), `create asset ${ext} ${i} -> ${r.status()} ${await r.text()}`).toBe(201);
  return ((await r.json()) as { id: string }).id;
}

type SearchBody = { hits: { id: string }[]; total_count: number };

/** One interactive search, through the same endpoint the page uses. */
async function search(request: APIRequestContext, qs: string): Promise<SearchBody> {
  const r = await request.get(`/api/v1/search?${qs}`);
  expect(r.status(), `GET /api/v1/search?${qs}`).toBe(200);
  return (await r.json()) as SearchBody;
}

const ids = (b: SearchBody) => [...b.hits.map((h) => h.id)].sort();

test.describe('#1368 a saved search replays the search it was saved from', () => {
  test.describe.configure({ mode: 'serial' });
  test.setTimeout(180_000);

  test.beforeAll(async ({ request }) => {
    await loginAsAdminViaAPI(request);
    for (let i = 0; i < KEPT_ROWS; i++) assetIds.push(await makeAsset(request, i, KEPT_EXT));
    for (let i = 0; i < DROPPED_ROWS; i++) assetIds.push(await makeAsset(request, i, DROPPED_EXT));

    // ⭐ THE FIXTURE PROVES ITS OWN PREMISE ON THE WIRE, BEFORE ANY UI
    // IS DRIVEN. If the two populations were equal, every assertion
    // below would pass on the bug — which is the failure mode a count
    // comparison has, and the reason this block is not optional.
    const all = await search(request, `limit=100&types=asset&q=${encodeURIComponent(TOKEN)}`);
    expect(all.total_count, 'the fixture must be the whole of this token').toBe(TOTAL_ROWS);

    const narrowed = await search(
      request,
      `limit=100&types=asset&q=${encodeURIComponent(TOKEN)}&filter=extension:${KEPT_EXT}`,
    );
    expect(narrowed.total_count, 'the filtered population').toBe(KEPT_ROWS);
    expect(
      narrowed.total_count,
      'the filter must MOVE the number, or a count comparison proves nothing',
    ).toBeLessThan(all.total_count);
  });

  test.afterAll(async ({ request }) => {
    if (savedSearchID) {
      await request.delete(`/api/v1/search/saved/${savedSearchID}`).catch(() => undefined);
    }
    for (const id of assetIds) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
  });

  /** Save the search on screen through the real button and dialog.
   *
   *  ⛔ Nothing here is addressed by a test id that does not already
   *  exist on `dev`. The dialog is found by its role and its controls by
   *  theirs, so this function is byte-identical against both branches. */
  async function saveThroughTheDialog(page: Page) {
    await page.getByTestId('save-search').click();
    const dialog = page.getByRole('dialog', { name: 'Save search' });
    await expect(dialog).toBeVisible();
    await dialog.locator('input[type="text"]').fill(SAVED_NAME);
    // Track only: this file is about which rows a replay returns, and a
    // digest email is a different subsystem with its own guards.
    await dialog.locator('select').selectOption('none');
    await dialog.getByRole('button', { name: 'Save search' }).click();
    await expect(page.getByTestId('save-search-result')).toContainText(/Saved/i, {
      timeout: 20_000,
    });
  }

  test('a search narrowed by a facet filter replays narrowed', async ({ page, request }) => {
    const url = `/search?q=${encodeURIComponent(TOKEN)}&types=asset&filter=extension:${KEPT_EXT}`;
    await page.goto(url);

    // The page has to have ANSWERED before the Save button can be
    // clicked — it only renders once hits are on screen — so this
    // doubles as the proof that the address the user saved is the
    // address that produced the result set.
    await expect(page.getByTestId('search-total-count')).toContainText(String(KEPT_ROWS), {
      timeout: 20_000,
    });

    await saveThroughTheDialog(page);

    // Find the row through the ordinary list surface, the way the
    // account page does.
    const list = await request.get('/api/v1/search/saved?limit=50');
    expect(list.status()).toBe(200);
    const rows = (await list.json()) as { items?: { id: string; name: string; dsl: string }[] };
    const row = (rows.items ?? []).find((r) => r.name === SAVED_NAME);
    expect(row, `the saved search "${SAVED_NAME}" is not in the list`).toBeTruthy();
    savedSearchID = row!.id;

    // ── 1. THE EXECUTOR'S OWN VERDICT ─────────────────────────────
    //
    // On `dev` the UI posts the query expression alone, so this replays
    // the UNFILTERED population and the number is TOTAL_ROWS. After the
    // fix the stored DSL carries the selection and it is KEPT_ROWS.
    const run = await request.post(`/api/v1/search/saved/${savedSearchID}/run-now`);
    expect(run.status(), await run.text()).toBe(200);
    const { hit_count } = (await run.json()) as { hit_count: number };
    expect(
      hit_count,
      `the saved search replayed ${hit_count} hits where the search it was saved from ` +
        `returned ${KEPT_ROWS}. The facet filter did not survive the save, so the owner's ` +
        `digest carries hits their own search excludes. Stored DSL: ${row!.dsl}`,
    ).toBe(KEPT_ROWS);

    // ── 2. THE STORED QUERY, REPLAYED WHERE IDS ARE VISIBLE ───────
    //
    // `run-now` answers with a count and no ids, so set equality is
    // reached through the one surface that does return them: the stored
    // DSL run back through /search. Same compile + SelectionFromDSL path
    // the executor takes, and present on `dev`.
    const onScreen = await search(
      request,
      `limit=100&types=asset&q=${encodeURIComponent(TOKEN)}&filter=extension:${KEPT_EXT}`,
    );
    const replayed = await search(
      request,
      `limit=100&types=asset&dsl=${encodeURIComponent(row!.dsl)}`,
    );
    expect(
      ids(replayed),
      `the stored query "${row!.dsl}" does not reconstruct the search it was saved from`,
    ).toEqual(ids(onScreen));
  });

  test('a search with no filters is stored exactly as it was', async ({ page, request }) => {
    // ⭐ THE N=0 CONTROL, and it is what makes the case above
    // attributable. An unfiltered save must behave as it always has —
    // same stored string, same replay — so a failure up there is the
    // FILTERS and not the save path having changed underneath.
    const name = `${TOKEN} unfiltered`;
    await page.goto(`/search?q=${encodeURIComponent(TOKEN)}&types=asset`);
    await expect(page.getByTestId('search-total-count')).toContainText(String(TOTAL_ROWS), {
      timeout: 20_000,
    });

    await page.getByTestId('save-search').click();
    const dialog = page.getByRole('dialog', { name: 'Save search' });
    await dialog.locator('input[type="text"]').fill(name);
    await dialog.locator('select').selectOption('none');
    await dialog.getByRole('button', { name: 'Save search' }).click();
    await expect(page.getByTestId('save-search-result')).toContainText(/Saved/i, {
      timeout: 20_000,
    });

    const list = await request.get('/api/v1/search/saved?limit=50');
    const rows = (await list.json()) as { items?: { id: string; name: string; dsl: string }[] };
    const row = (rows.items ?? []).find((r) => r.name === name);
    expect(row, `the saved search "${name}" is not in the list`).toBeTruthy();

    try {
      expect(row!.dsl, 'an unfiltered save must store the query it was given, untouched').toBe(
        TOKEN,
      );
      const run = await request.post(`/api/v1/search/saved/${row!.id}/run-now`);
      expect(run.status()).toBe(200);
      const { hit_count } = (await run.json()) as { hit_count: number };
      expect(hit_count, 'an unfiltered saved search replays the whole token').toBe(TOTAL_ROWS);
    } finally {
      await request.delete(`/api/v1/search/saved/${row!.id}`).catch(() => undefined);
    }
  });
});
