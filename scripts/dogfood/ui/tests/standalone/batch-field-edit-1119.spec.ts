// The batch metadata editor has a CALLER (#1119, #1173).
//
// # What was true before this file
//
// Sprint 20c-i shipped the whole server half: POST
// /batch/asset-fields/preview, POST /batch/asset-fields/apply, the six
// preview partitions, the single-use token, the five apply outcomes and
// the audit envelope. It had ZERO production callers. `grep -rn
// "batch-asset-field\|batchAssetField\|/batch/assets" web/src` returned
// nothing, and it could not have been otherwise: the endpoints take a
// TYPED selection of `{kind, id}` entries, and the shipped selection
// store held `ids = $state<string[]>([])` with its own header saying
// "what the ids MEAN is context-dependent and intentionally NOT baked
// in here".
//
// So there were two separate holes, and this file drives both:
//
//   1. NO TYPED SELECTION COULD BE BUILT. Every test below that reaches
//      a preview fails on the old build at the `selection-batch-edit`
//      control, which did not exist anywhere in the app.
//   2. SELECTION WAS ORPHANED OFF BROWSE. `SelectionBar` was mounted by
//      `/` alone, so on the nine other surfaces that can put things in
//      the selection (the profile among them) a reader could tick
//      cards and then find no count, no Clear and no action on the
//      page. `the profile exposes count, clear and the batch action`
//      is red on that build for exactly that reason.
//
// # What this file is NOT allowed to do
//
// It never recomputes the server's answer. `counts.expanded`,
// `selection_entry_count`, `empty_posts` and the partition totals are
// read out of the preview RESPONSE and asserted against the fixture the
// test itself built. The overlap case is the sharp one: two posts
// sharing a member, that member selected directly as an asset as
// well, and a lone asset belonging to no post, must reach the server
// as FOUR entries whose six naive contributions collapse to a
// distinct union of FOUR targets, and no line of application code is
// permitted to know that.
//
// # The committed-apply case is the point of the file
//
// A UI that treats HTTP 200 as "it worked" fails
// `a committed apply reports the targets that did NOT change`. That
// test drives a real apply whose outcome_counts are `changed: 1,
// conflict: 1`, and asserts the surface says so.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page, Response } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';

test.describe.configure({ mode: 'serial' });

/** Per-run suffix. DELETE /fields SOFT-ARCHIVES and `code` is UNIQUE,
 *  so a fixed code collides on the retry a flake produces (#527). */
const RUN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`;

const PROFILE = '/users/by-username/admin';

const createdFields: string[] = [];
const createdAssets: string[] = [];
const createdPosts: string[] = [];

// ---------------------------------------------------------------------------
// Fixtures, built through the API
// ---------------------------------------------------------------------------

async function makeField(
  request: APIRequestContext,
  name: string,
  body: Record<string, unknown> = {},
): Promise<{ id: string; code: string }> {
  const code = `bfe_${name}_${RUN}`;
  const r = await request.post('/api/v1/fields', {
    data: {
      code,
      label: `BFE ${name} ${RUN}`,
      type: 'text',
      subject_kind: 'asset',
      display_order: 9300,
      ...body,
    },
  });
  expect(r.status(), `create field ${code} -> ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  createdFields.push(id);
  return { id, code };
}

async function makeAsset(request: APIRequestContext, label: string): Promise<string> {
  // Novel bytes: storage is content-addressed, so a fixed body would
  // dedupe onto an EXISTING asset another spec owns.
  const up = await request.post('/api/v1/storage/objects', {
    data: Buffer.from(`bfe ${RUN} ${label} ${Math.random()}`),
    headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'text/plain' },
  });
  expect(up.status(), `upload -> ${up.status()}`).toBe(201);
  const { hash } = (await up.json()) as { hash: string };
  const r = await request.post('/api/v1/assets', {
    data: {
      title: `BFE ${RUN} ${label}`,
      asset_type: 2,
      file_hash: hash,
      file_extension: 'txt',
    },
  });
  expect(r.status(), `create asset -> ${r.status()} ${await r.text()}`).toBe(201);
  const id = ((await r.json()) as { id: string }).id;
  createdAssets.push(id);
  return id;
}

async function makePost(
  request: APIRequestContext,
  label: string,
  members: string[],
): Promise<string> {
  const r = await request.post('/api/v1/posts', {
    data: {
      title: `BFE ${RUN} ${label}`,
      description: 'batch metadata fixture',
      visibility: 'org-only',
      members: members.map((asset_id, i) => ({ asset_id, sort_order: i })),
    },
  });
  expect(r.status(), `create post -> ${r.status()} ${await r.text()}`).toBeLessThan(300);
  const id = ((await r.json()) as { id: string }).id;
  createdPosts.push(id);
  return id;
}

async function putValue(
  request: APIRequestContext,
  assetId: string,
  fieldId: string,
  body: Record<string, unknown>,
) {
  const r = await request.put(`/api/v1/assets/${assetId}/fields/${fieldId}`, { data: body });
  expect(r.ok(), `put value -> ${r.status()} ${await r.text()}`).toBeTruthy();
}

async function storedText(
  request: APIRequestContext,
  assetId: string,
  fieldId: string,
): Promise<unknown> {
  const r = await request.get(`/api/v1/assets/${assetId}/fields`);
  expect(r.ok()).toBeTruthy();
  const rows = (await r.json()) as Array<Record<string, unknown>>;
  return rows.find((v) => v.field_id === fieldId)?.value_text;
}

// ---------------------------------------------------------------------------
// The network watcher.
//
// EVERY assertion about what reached the server reads THIS, not the
// page. A UI that renders a convincing preview panel without having
// sent a request fails here, and so does one that sends an untyped
// selection.
// ---------------------------------------------------------------------------

interface BatchCall {
  path: string;
  payload: Record<string, unknown>;
  status: number;
  body: Record<string, unknown> | null;
}

function watchBatch(page: Page) {
  const pending: Promise<BatchCall>[] = [];
  const onResponse = (res: Response) => {
    const path = new URL(res.url()).pathname;
    if (!path.includes('/batch/asset-fields/')) return;
    pending.push(
      (async () => ({
        path,
        payload: (res.request().postDataJSON() ?? {}) as Record<string, unknown>,
        status: res.status(),
        body: (await res.json().catch(() => null)) as Record<string, unknown> | null,
      }))(),
    );
  };
  page.on('response', onResponse);
  const settle = async () => Promise.all(pending);
  return {
    async all(): Promise<BatchCall[]> {
      return settle();
    },
    async previews(): Promise<BatchCall[]> {
      return (await settle()).filter((c) => c.path.endsWith('/preview'));
    },
    async applies(): Promise<BatchCall[]> {
      return (await settle()).filter((c) => c.path.endsWith('/apply'));
    },
    /** How many batch calls have been seen so far.
     *
     *  Synchronous, and read BEFORE the action under test, so a test
     *  can assert about the calls THAT ACTION made rather than about
     *  every call the test has ever provoked. A file that previews
     *  twice (the token lifecycle cases below do) cannot otherwise say
     *  which request it means. */
    mark(): number {
      return pending.length;
    },
    async since(from: number): Promise<BatchCall[]> {
      return (await settle()).slice(from);
    },
    dispose() {
      page.off('response', onResponse);
    },
  };
}

type BatchWatcher = ReturnType<typeof watchBatch>;

// ---------------------------------------------------------------------------
// Driving the real surface
// ---------------------------------------------------------------------------

/** One typed selection entry, in the shape the contract requires. */
interface SelEntry {
  kind: 'asset' | 'post';
  id: string;
}
const assetEntry = (id: string): SelEntry => ({ kind: 'asset', id });
const postEntry = (id: string): SelEntry => ({ kind: 'post', id });

/** Order-insensitive comparison key. The server orders its TARGETS by
 *  asset id, but the selection is sent in tick order, and the test
 *  cares that EXACTLY these pairs were sent, not in which sequence. */
const entryKeys = (e: SelEntry[]): string[] => e.map((x) => `${x.kind}:${x.id}`).sort();

async function gotoProfile(page: Page) {
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto(PROFILE);
  await expect(page.getByTestId('profile-wall')).toBeVisible();
}

/** Prove the card is THERE and SELECTABLE before ticking it, so a test
 *  can never pass on a selection it did not make. Also asserts the
 *  DOM identity contract: a selectable card publishes BOTH halves. */
async function tick(page: Page, kind: 'asset' | 'post', id: string) {
  const card = page.locator(`[data-select-id="${id}"]`).first();
  await expect(card, `a selectable card for ${kind} ${id}`).toHaveCount(1);
  await expect(card).toHaveAttribute('data-select-kind', kind);
  await card.scrollIntoViewIfNeeded();
  const boxEl = card.locator('[role="checkbox"]').first();
  await expect(boxEl).toHaveAttribute('aria-checked', 'false');
  await boxEl.click();
  await expect(boxEl).toHaveAttribute('aria-checked', 'true');
}

async function selectionCount(page: Page): Promise<number> {
  const bar = page.getByTestId('selection-bar');
  if ((await bar.count()) === 0) return 0;
  const txt = (await page.getByTestId('selection-count').textContent()) ?? '';
  return Number(txt.trim().split(/\s+/)[0]);
}

async function clearSelection(page: Page) {
  if ((await page.getByTestId('selection-bar').count()) > 0) {
    await page.getByTestId('selection-clear').click();
  }
  await expect(page.getByTestId('selection-bar')).toHaveCount(0);
}

async function openBatch(page: Page) {
  await page.getByTestId('selection-batch-edit').click();
  await expect(page.getByTestId('batch-edit-modal')).toBeVisible();
}

async function composeText(page: Page, fieldId: string, code: string, mode: string, text: string) {
  await page.getByTestId('batch-field-select').selectOption(fieldId);
  await page.getByTestId('batch-mode-select').selectOption(mode);
  await page.getByTestId(`field-input-${code}`).fill(text);
}

async function preview(page: Page) {
  await page.getByTestId('batch-preview-submit').click();
}

async function applyWith(page: Page, reason: string, confirm?: number) {
  await page.getByTestId('batch-reason').fill(reason);
  if (confirm !== undefined) {
    await page.getByTestId('batch-confirm-count').fill(String(confirm));
  }
  await page.getByTestId('batch-apply-submit').click();
}

async function num(page: Page, testid: string): Promise<number> {
  return Number((await page.getByTestId(testid).textContent())?.trim());
}

/** Tick exactly these entries, and prove the selection is what was
 *  asked for before anything is previewed. */
async function selectEntries(page: Page, entries: SelEntry[]) {
  for (const e of entries) await tick(page, e.kind, e.id);
  expect(
    await selectionCount(page),
    'the precondition this case needs really exists on the page',
  ).toBe(entries.length);
}

/**
 * THE SUCCESSFUL-PREVIEW STRUCTURAL BASELINE, in one place.
 *
 * Every case in this file that expects to reach a preview panel goes
 * through here, and each one therefore proves the same five things in
 * the same order:
 *
 *   1. the intended selectable cards EXIST and publish both halves of
 *      their identity (`tick`);
 *   2. they were actually SELECTED (the count is the entry count);
 *   3. a REAL preview request was sent by this action, exactly one;
 *   4. that request carried EXACTLY the intended `{kind, id}` entries,
 *      no more and no fewer;
 *   5. it came back 200 with a NON-EMPTY OPAQUE TOKEN, and the server
 *      counted the same number of entries it was sent.
 *
 * Only then may a caller assert anything about an apply. A case that
 * skipped step 4 could pass on a client that sent a bare uuid list, and
 * a case that skipped step 5 could assert about an apply it never
 * earned a token for.
 *
 * ⛔ Intentional-refusal cases and N=0 do NOT use this: they have the
 * opposite obligation and assert it separately.
 */
async function previewOk(
  page: Page,
  watch: BatchWatcher,
  entries: SelEntry[],
  compose: () => Promise<void>,
): Promise<Record<string, unknown>> {
  await selectEntries(page, entries);
  await openBatch(page);
  await compose();

  const from = watch.mark();
  await preview(page);
  await expect(page.getByTestId('batch-preview-panel')).toBeVisible();

  const fresh = (await watch.since(from)).filter((c) => c.path.endsWith('/preview'));
  expect(fresh, 'exactly one REAL preview request was sent by this action').toHaveLength(1);
  const call = fresh[0];
  expect(call.status, `preview -> ${call.status} ${JSON.stringify(call.body)}`).toBe(200);

  const sent = (call.payload.selection ?? []) as SelEntry[];
  expect(entryKeys(sent), 'the TYPED selection payload that earned this token').toEqual(
    entryKeys(entries),
  );

  const body = call.body as Record<string, unknown>;
  expect(typeof body.token, 'the preview token').toBe('string');
  expect((body.token as string).length, 'a non-empty opaque token').toBeGreaterThan(0);
  expect(body.selection_entry_count, 'the server counted the entries it was sent').toBe(
    entries.length,
  );
  return body;
}

// ---------------------------------------------------------------------------
// The corpus
// ---------------------------------------------------------------------------

let field: { id: string; code: string };
/** A multi_select definition, so a test can reach `append` / `remove`
 *  and then leave them behind. */
let multiField: { id: string; code: string };
/** A1 is a member of BOTH posts. A2 belongs to P1 only, A3 to P2 only.
 *  A4 is never in a post, and is the N=1 direct-asset case. */
let A1 = '', A2 = '', A3 = '', A4 = '';
let P1 = '', P2 = '', PEMPTY = '';

test.beforeAll(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  field = await makeField(request, 'text');
  multiField = await makeField(request, 'multi', {
    type: 'multi_select',
    options: { values: [{ value: 'one', label: 'One' }, { value: 'two', label: 'Two' }] },
  });
  A1 = await makeAsset(request, 'a1');
  A2 = await makeAsset(request, 'a2');
  A3 = await makeAsset(request, 'a3');
  A4 = await makeAsset(request, 'a4');
  P1 = await makePost(request, 'p1', [A1, A2]);
  P2 = await makePost(request, 'p2', [A1, A3]);

  // An EMPTY post. `PostCreate` requires at least one member, so the
  // only way to reach a memberless post is to take the member back out.
  const seed = await makeAsset(request, 'seed');
  PEMPTY = await makePost(request, 'pempty', [seed]);
  const rm = await request.delete(`/api/v1/posts/${PEMPTY}/assets/${seed}`);
  expect(rm.status(), `emptying the post -> ${rm.status()} ${await rm.text()}`).toBe(204);
  const members = await request.get(`/api/v1/posts/${PEMPTY}`);
  const holds = ((await members.json()) as { members?: unknown[] }).members ?? [];
  expect(holds, 'the empty post really holds nothing').toHaveLength(0);
});

test.afterAll(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  for (const id of createdPosts) {
    await request.delete(`/api/v1/posts/${id}?hard=true`).catch(() => undefined);
  }
  for (const id of createdAssets) {
    await request.delete(`/api/v1/assets/${id}?hard=true`).catch(() => undefined);
  }
  for (const id of createdFields) {
    await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
  }
});

test.beforeEach(async ({ page }) => {
  await gotoProfile(page);
  await clearSelection(page);
});

// ---------------------------------------------------------------------------
// RED-BEFORE 4: the orphaned selection
// ---------------------------------------------------------------------------

test('the profile exposes count, clear AND the batch action', async ({ page }) => {
  // A NON-BROWSE surface. On the shipped build `SelectionBar` was
  // mounted by `/` alone, so everything below times out: ticking a card
  // here produced a selection with nothing anywhere on the page to
  // show it, empty it or act on it.
  await tick(page, 'asset', A4);

  await expect(page.getByTestId('selection-bar')).toBeVisible();
  expect(await selectionCount(page)).toBe(1);
  await expect(page.getByTestId('selection-batch-edit')).toBeVisible();
  await expect(page.getByTestId('selection-clear')).toBeVisible();

  // And it FOLLOWS the selection across navigation, which is the case a
  // per-page mount cannot answer at all: browse has no card for this
  // asset, and the selection is still real there.
  //
  // A CLIENT-SIDE navigation, deliberately, and it has to be: the store
  // is a live singleton in one JS module, so a full document load is a
  // new module and a new empty selection. That is the shipped
  // behaviour and it is not what "survives navigation" has ever meant
  // here (selection.svelte.ts). `page.goto` would be testing the
  // browser's reload, not the app's routing.
  // Back to the top first: the shell's chrome AUTO-HIDES on a downward
  // scroll (#1122), and `tick` scrolled a card into view to reach it,
  // so the brand link is off screen until the wall comes back up.
  await page.getByTestId('app-shell-main').evaluate((el) => el.scrollTo(0, 0));
  const home = page.locator('header a[href="/"]').first();
  await expect(home).toBeInViewport();
  await home.click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByTestId('selection-bar')).toBeVisible();
  expect(await selectionCount(page)).toBe(1);

  await page.getByTestId('selection-clear').click();
  await expect(page.getByTestId('selection-bar')).toHaveCount(0);
});

// ---------------------------------------------------------------------------
// N = 0: there is NO legal preview request
// ---------------------------------------------------------------------------

test('with nothing selected there is no batch action and no request', async ({ page }) => {
  const watch = watchBatch(page);
  try {
    expect(await selectionCount(page)).toBe(0);
    // The bar renders only while a selection is active, so the preview
    // action is not merely disabled, it is not on the page.
    await expect(page.getByTestId('selection-bar')).toHaveCount(0);
    await expect(page.getByTestId('selection-batch-edit')).toHaveCount(0);

    await page.waitForTimeout(500);
    expect(await watch.all(), 'no batch request may be emitted').toHaveLength(0);
  } finally {
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// RED-BEFORE 1 + 3: a MIXED typed selection reaches a real preview
// ---------------------------------------------------------------------------

test('a mixed asset+post selection reaches the server as TYPED entries', async ({ page }) => {
  const watch = watchBatch(page);
  try {
    // THE PAYLOAD, checked by the shared baseline: two entries, each
    // carrying its own kind. On the old store this could only ever have
    // been a list of bare uuids.
    const body = await previewOk(page, watch, [assetEntry(A4), postEntry(P1)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'mixed selection'),
    );
    expect(body.selection_entry_count).toBe(2);
    // A4 is a lone asset; P1 holds A1 and A2. Three distinct targets,
    // computed by the SERVER from membership it read for itself.
    expect((body.counts as Record<string, number>).expanded).toBe(3);
    expect(await num(page, 'batch-selection-entry-count')).toBe(2);
    expect(await num(page, 'batch-count-expanded')).toBe(3);
  } finally {
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// The boundary matrix
// ---------------------------------------------------------------------------

test('N=1 direct asset expands to exactly itself', async ({ page }) => {
  const watch = watchBatch(page);
  try {
    const body = await previewOk(page, watch, [assetEntry(A4)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'n1 asset'),
    );
    const counts = body.counts as Record<string, number>;
    expect(counts.expanded).toBe(1);
    expect((body.targets as Array<{ asset_id: string }>).map((t) => t.asset_id)).toEqual([A4]);
    expect(body.empty_posts ?? []).toEqual([]);
  } finally {
    watch.dispose();
  }
});

test('N=1 EMPTY post is a real entry that expands to nothing', async ({ page }) => {
  const watch = watchBatch(page);
  try {
    // The entry is REAL: the baseline proves it was sent as a typed
    // `post` entry and that the server counted it.
    const body = await previewOk(page, watch, [postEntry(PEMPTY)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'n1 empty post'),
    );
    const counts = body.counts as Record<string, number>;

    // It expanded to zero. No invented target, and no crash.
    expect(counts.expanded).toBe(0);
    expect(counts.would_change).toBe(0);
    expect(body.targets).toEqual([]);
    expect(body.empty_posts).toEqual([PEMPTY]);
    await expect(page.getByTestId('batch-empty-posts')).toBeVisible();
    expect(await num(page, 'batch-count-expanded')).toBe(0);
  } finally {
    watch.dispose();
  }
});

test('N=1 non-empty post expands through its membership', async ({ page }) => {
  const watch = watchBatch(page);
  try {
    const body = await previewOk(page, watch, [postEntry(P1)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'n1 post'),
    );
    expect((body.counts as Record<string, number>).expanded).toBe(2);
    expect(
      (body.targets as Array<{ asset_id: string }>).map((t) => t.asset_id).sort(),
    ).toEqual([A1, A2].sort());
  } finally {
    watch.dispose();
  }
});

test('overlapping posts plus a duplicated direct asset reconcile to the distinct union', async ({
  page,
}) => {
  const watch = watchBatch(page);
  try {
    // THE FIXTURE, stated exactly.
    //
    //   P1 = {A1, A2}          2 members
    //   P2 = {A1, A3}          2 members
    //   A1  selected directly  1, and it is ALSO a member of BOTH posts
    //   A4  selected directly  1, and it is in no post
    //
    //   FOUR selection entries.
    //   Naive membership + direct cardinality: 2 + 2 + 1 + 1 = 6.
    //   The server's DISTINCT UNION: {A1, A2, A3, A4} = 4.
    //
    // So the interesting number is not that six became four by
    // arithmetic anybody could do here, it is that the SERVER did it,
    // from membership it read for itself, and that A1 reached through
    // two posts and directly is ONE target written ONCE.
    const body = await previewOk(
      page,
      watch,
      [postEntry(P1), postEntry(P2), assetEntry(A1), assetEntry(A4)],
      () => composeText(page, field.id, field.code, 'overwrite', 'overlap'),
    );
    const counts = body.counts as Record<string, number>;

    // The distinct union, computed here from the fixture and NOWHERE in
    // the app. The duplicate A1s are written out rather than collapsed
    // so the set literal reads as the six naive contributions it is.
    const union = new Set([A1, A2, A1, A3, A1, A4]);
    expect(union.size, 'four distinct targets out of six naive contributions').toBe(4);
    expect(counts.expanded).toBe(union.size);

    // The server's own reconciliation identity, asserted rather than
    // recomputed.
    expect(counts.expanded).toBe(
      counts.would_change +
        counts.no_op +
        counts.refused +
        counts.inapplicable +
        counts.unreadable +
        counts.unauthorized,
    );
    expect(counts.eligible).toBe(counts.would_change + counts.no_op);

    expect(await num(page, 'batch-selection-entry-count')).toBe(4);
    expect(await num(page, 'batch-count-expanded')).toBe(union.size);
  } finally {
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// A refused preview. NO token is obtained, and the refusal is the
// SERVER'S.
// ---------------------------------------------------------------------------

test('a field archived AFTER the picker loaded is refused authoritatively', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const doomed = await makeField(request, 'doomed');
  const watch = watchBatch(page);
  try {
    await tick(page, 'asset', A4);
    await openBatch(page);
    await composeText(page, doomed.id, doomed.code, 'overwrite', 'too late');

    // THE RACE, made real: the definition is archived while this
    // operator's snapshot of the field list still lists it. DELETE
    // /fields soft-archives.
    const del = await request.delete(`/api/v1/fields/${doomed.id}`);
    expect(del.ok(), `archive field -> ${del.status()}`).toBeTruthy();

    await preview(page);

    const refusal = page.getByTestId('batch-refusal');
    await expect(refusal).toBeVisible();
    await expect(refusal).toHaveAttribute('data-refusal-reason', 'field_archived');
    await expect(refusal).toHaveAttribute('data-refusal-status', '422');
    // The review step was never reached, and no apply control exists.
    await expect(page.getByTestId('batch-preview-panel')).toHaveCount(0);
    await expect(page.getByTestId('batch-apply-submit')).toHaveCount(0);

    const calls = await watch.previews();
    expect(calls, 'the real preview request was issued').toHaveLength(1);
    expect(calls[0].status).toBe(422);
    expect((calls[0].body as Record<string, unknown>).reason).toBe('field_archived');
    // ⛔ NO token was obtained. Not "an unused token", none at all.
    expect(calls.filter((c) => c.status === 200), 'no successful preview').toHaveLength(0);
    expect((calls[0].body as Record<string, unknown>).token).toBeUndefined();
    expect(await watch.applies(), 'and nothing was applied').toHaveLength(0);
  } finally {
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// A refused APPLY. Pre-commit, so nothing was written and the token
// survives.
// ---------------------------------------------------------------------------

test('a mistyped confirmation count is refused before anything is written', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const watch = watchBatch(page);
  try {
    const body = await previewOk(page, watch, [assetEntry(A4)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'confirm probe'),
    );
    const wouldChange = (body.counts as Record<string, number>).would_change;
    expect(wouldChange).toBe(1);

    await applyWith(page, 'a deliberate miscount', wouldChange + 7);

    const refusal = page.getByTestId('batch-refusal');
    await expect(refusal).toBeVisible();
    await expect(refusal).toHaveAttribute('data-refusal-reason', 'confirm_count_mismatch');
    await expect(refusal).toHaveAttribute('data-refusal-status', '400');
    // PRE-COMMIT: no result, and the review step is still standing with
    // its token, so the operator corrects the number in place.
    await expect(page.getByTestId('batch-result')).toHaveCount(0);
    await expect(page.getByTestId('batch-apply-submit')).toBeVisible();
    expect(await storedText(request, A4, field.id)).toBeUndefined();

    // The same token, corrected, commits.
    await applyWith(page, 'corrected', wouldChange);
    await expect(page.getByTestId('batch-result')).toBeVisible();
    expect(await storedText(request, A4, field.id)).toBe('confirm probe');
  } finally {
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// THE COMMITTED APPLY, and the four outcomes that are not `changed`
// ---------------------------------------------------------------------------

test('a committed apply reports the targets that did NOT change', async ({ page, request }) => {
  await loginAsAdminViaAPI(request);
  const watch = watchBatch(page);
  try {
    // Two targets through one post. Both are would_change under
    // `overwrite`, which reports no_op as zero even against a target
    // already holding the value.
    const body = await previewOk(page, watch, [postEntry(P1)], () =>
      composeText(page, field.id, field.code, 'overwrite', `committed ${RUN}`),
    );
    const wouldChange = (body.counts as Record<string, number>).would_change;
    expect(wouldChange, 'both members are eligible').toBe(2);

    // MOVE ONE TARGET'S VALUE between the preview and the apply. Its
    // `set_at` no longer matches what the token bound, so it comes back
    // `conflict` while the rest of the batch proceeds.
    await putValue(request, A2, field.id, { value_text: 'moved by somebody else' });

    await applyWith(page, 'committed partial outcome probe', wouldChange);
    await expect(page.getByTestId('batch-result')).toBeVisible();

    // ⛔ A UI THAT ONLY CHECKED HTTP STATUS FAILS FROM HERE DOWN.
    expect(await num(page, 'batch-outcome-changed')).toBe(1);
    expect(await num(page, 'batch-outcome-conflict')).toBe(1);
    expect(await num(page, 'batch-outcome-gone')).toBe(0);
    expect(await num(page, 'batch-outcome-unauthorized')).toBe(0);
    expect(await num(page, 'batch-outcome-error')).toBe(0);

    // The non-changed target is NAMED, not hidden behind a success.
    const row = page.locator('[data-testid="batch-outcome-target"]');
    await expect(row).toHaveCount(1);
    await expect(row).toHaveAttribute('data-asset-id', A2);
    await expect(row).toHaveAttribute('data-outcome', 'conflict');

    // And the server said so.
    const applies = await watch.applies();
    expect(applies).toHaveLength(1);
    expect(applies[0].status).toBe(200);
    const oc = (applies[0].body as Record<string, Record<string, number>>).outcome_counts;
    expect(oc).toMatchObject({ changed: 1, conflict: 1, gone: 0, unauthorized_at_apply: 0, error: 0 });

    // What was INTENDED vs what HAPPENED are different numbers, and the
    // result carries both.
    expect(
      (applies[0].body as Record<string, Record<string, number>>).counts.would_change,
    ).toBe(2);

    // The writes are real, per target.
    expect(await storedText(request, A1, field.id)).toBe(`committed ${RUN}`);
    expect(await storedText(request, A2, field.id)).toBe('moved by somebody else');

    // THE SELECTION SURVIVES A COMMITTED APPLY. No silent auto-clear.
    await expect(page.getByTestId('batch-selection-kept')).toBeVisible();
    await page.getByTestId('batch-again').click();
    await page.keyboard.press('Escape');
    expect(await selectionCount(page)).toBe(1);
  } finally {
    watch.dispose();
  }
});

test('a target SOFT-DELETED between preview and apply comes back gone', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const doomedAsset = await makeAsset(request, 'gone');
  const watch = watchBatch(page);
  try {
    await gotoProfile(page);
    await clearSelection(page);
    await previewOk(page, watch, [assetEntry(doomedAsset)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'gone probe'),
    );

    // SOFT delete, which is what DELETE does on an asset. An ARCHIVED
    // asset would NOT be gone and would still be written; this is the
    // other thing.
    const del = await request.delete(`/api/v1/assets/${doomedAsset}`);
    expect(del.ok(), `soft delete -> ${del.status()}`).toBeTruthy();

    await applyWith(page, 'gone probe', 1);
    await expect(page.getByTestId('batch-result')).toBeVisible();

    expect(await num(page, 'batch-outcome-changed')).toBe(0);
    expect(await num(page, 'batch-outcome-gone')).toBe(1);
    const row = page.locator('[data-testid="batch-outcome-target"]');
    await expect(row).toHaveCount(1);
    await expect(row).toHaveAttribute('data-outcome', 'gone');

    // `changed: 0` IS a committed apply: the token was spent and an
    // operation id came back. It is not presented as a request that did
    // not go through.
    const applies = await watch.applies();
    expect(applies[0].status).toBe(200);
    await expect(page.getByTestId('batch-result')).toHaveAttribute(
      'data-operation-id',
      /[0-9a-f-]{36}/,
    );
  } finally {
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// THE TOKEN'S LIFECYCLE, as the operator experiences it
//
// Both refusals below are 409s, and 409 is the one family whose whole
// remedy is "preview again": the token is provably this caller's own,
// but it is spent or stale. So both cases assert the same product
// transition, which is the thing a client can get wrong: the refusal is
// VISIBLE, no committed result is shown, the stale preview is DISCARDED
// rather than left pressable, and the selection is untouched.
// ---------------------------------------------------------------------------

test('a SPENT token is refused as preview_consumed and the preview is discarded', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const target = await makeAsset(request, 'singleuse');
  const watch = watchBatch(page);
  try {
    await gotoProfile(page);
    await clearSelection(page);
    const body = await previewOk(page, watch, [assetEntry(target)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'single use probe'),
    );
    const token = body.token as string;
    const wouldChange = (body.counts as Record<string, number>).would_change;
    expect(wouldChange).toBe(1);

    // SPEND THE UI'S OWN TOKEN, out of band, through the exact apply
    // contract: three members, the token, a reason, and the
    // confirmation count `overwrite` requires. This is a REAL commit by
    // the same caller, which is what makes the next press a genuine
    // second use of one token rather than a simulated one.
    //
    // ⛔ Not "the first apply returned 200, so it must be single use".
    // That asserts nothing about the SECOND use, which is the property.
    const spent = await request.post('/api/v1/batch/asset-fields/apply', {
      data: { token, reason: 'spent out of band', confirm_count: wouldChange },
    });
    expect(
      spent.status(),
      `out-of-band apply -> ${spent.status()} ${await spent.text()}`,
    ).toBe(200);

    // Now the operator presses Apply on the preview they are still
    // looking at, holding the token that has just been consumed.
    const from = watch.mark();
    await applyWith(page, 'the operator presses apply', wouldChange);

    // The RENDERED refusal is awaited before the network is inspected,
    // and that ordering is load-bearing rather than stylistic: the
    // watcher only holds responses it has already seen, so reading it
    // the instant after a click races the request it is asking about.
    const refusal = page.getByTestId('batch-refusal');
    await expect(refusal).toBeVisible();
    await expect(refusal).toHaveAttribute('data-refusal-reason', 'preview_consumed');
    await expect(refusal).toHaveAttribute('data-refusal-status', '409');

    const applies = (await watch.since(from)).filter((c) => c.path.endsWith('/apply'));
    expect(applies, 'the UI sent a REAL apply request').toHaveLength(1);
    expect(applies[0].status).toBe(409);
    expect((applies[0].body as Record<string, unknown>).reason).toBe('preview_consumed');

    // NO committed result for this second press, and the spent preview
    // is gone: there is no Apply left to press, and the operator is
    // back at compose with a Preview button.
    await expect(page.getByTestId('batch-result')).toHaveCount(0);
    await expect(page.getByTestId('batch-apply-submit')).toHaveCount(0);
    await expect(page.getByTestId('batch-preview-panel')).toHaveCount(0);
    await expect(page.getByTestId('batch-preview-submit')).toBeVisible();

    // Exactly ONE apply committed, and it was the out-of-band one. The
    // second press wrote nothing.
    expect(await storedText(request, target, field.id)).toBe('single use probe');
    expect(await selectionCount(page)).toBe(1);
  } finally {
    watch.dispose();
  }
});

test('an EXPIRED preview is refused and the operator must preview again', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const target = await makeAsset(request, 'expired');
  const watch = watchBatch(page);
  const APPLY_GLOB = '**/batch/asset-fields/apply';
  try {
    await gotoProfile(page);
    await clearSelection(page);
    await previewOk(page, watch, [assetEntry(target)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'expiry probe'),
    );

    // ⚠️ THE ONE MOCKED RESPONSE IN THIS FILE, and the reason it is a
    // mock is worth writing down rather than discovering later.
    //
    // `batchPreviewTTL` is a `const` in batch_token.go, fifteen
    // minutes, with no configuration seam, no admin endpoint and no
    // database handle reachable from this suite. The only faithful
    // alternatives are sleeping for a quarter of an hour, which is not
    // a test, or reaching into `metadata_batch_preview` behind the
    // product, which asserts less than this does.
    //
    // So the mock is as narrow as it can be and still prove the thing:
    // ONE apply response, on the real endpoint, carrying the SERVER'S
    // OWN 409 `preview_expired` body verbatim from batch_apply.go. The
    // request is real, the UI's own handling is real, and everything
    // asserted below is the product's behaviour rather than the mock's.
    let served = false;
    await page.route(APPLY_GLOB, async (route) => {
      if (served) return route.continue();
      served = true;
      await route.fulfill({
        status: 409,
        contentType: 'application/json',
        body: JSON.stringify({
          error: 'this preview has expired; re-preview to see the current state',
          reason: 'preview_expired',
        }),
      });
    });

    const from = watch.mark();
    await applyWith(page, 'applying a stale preview', 1);

    // Rendered refusal first, then the network, for the reason given in
    // the consumed-token case above.
    const refusal = page.getByTestId('batch-refusal');
    await expect(refusal).toBeVisible();
    await expect(refusal).toHaveAttribute('data-refusal-reason', 'preview_expired');
    await expect(refusal).toHaveAttribute('data-refusal-status', '409');

    const applies = (await watch.since(from)).filter((c) => c.path.endsWith('/apply'));
    expect(applies, 'the UI sent a REAL apply request').toHaveLength(1);
    expect(applies[0].status).toBe(409);
    expect((applies[0].body as Record<string, unknown>).reason).toBe('preview_expired');
    expect(served, 'the interception actually fired').toBe(true);

    // Same product transition as a consumed token: nothing committed,
    // the stale preview discarded, a fresh preview required.
    await expect(page.getByTestId('batch-result')).toHaveCount(0);
    await expect(page.getByTestId('batch-apply-submit')).toHaveCount(0);
    await expect(page.getByTestId('batch-preview-panel')).toHaveCount(0);
    await expect(page.getByTestId('batch-preview-submit')).toBeVisible();

    expect(await storedText(request, target, field.id)).toBeUndefined();
    expect(await selectionCount(page)).toBe(1);

    // And previewing again really works: the operator is not stuck.
    await page.unroute(APPLY_GLOB);
    const again = watch.mark();
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();
    const fresh = (await watch.since(again)).filter((c) => c.path.endsWith('/preview'));
    expect(fresh).toHaveLength(1);
    expect(fresh[0].status).toBe(200);
    expect(entryKeys(fresh[0].payload.selection as SelEntry[])).toEqual(
      entryKeys([assetEntry(target)]),
    );
  } finally {
    await page.unroute(APPLY_GLOB).catch(() => undefined);
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// mintable_terms is NOT committed_terms
// ---------------------------------------------------------------------------

test('a preview term is not reported as created when no target stored it', async ({
  page,
  request,
}) => {
  await loginAsAdminViaAPI(request);
  const vocab = await makeField(request, 'vocab', {
    type: 'multi_select',
    open_vocabulary: true,
    options: { values: [{ value: 'alpha', label: 'Alpha' }, { value: 'beta', label: 'Beta' }] },
  });
  const target = await makeAsset(request, 'vocabtarget');
  await putValue(request, target, vocab.id, { value_options: ['alpha'] });
  const fresh = `bfeterm${RUN}`;

  const watch = watchBatch(page);
  try {
    await gotoProfile(page);
    await clearSelection(page);
    const body = await previewOk(page, watch, [assetEntry(target)], async () => {
      await page.getByTestId('batch-field-select').selectOption(vocab.id);
      await page.getByTestId('batch-mode-select').selectOption('overwrite');
      const combo = page.getByTestId(`vocab-input-${vocab.code}`);
      await combo.fill(fresh);
      await page.getByTestId(`vocab-create-${vocab.code}`).click();
    });
    // The preview says the term WOULD be created.
    expect(body.mintable_terms).toContain(fresh);
    await expect(page.getByTestId('batch-mintable-terms')).toContainText(fresh);

    // Move the only would_change target onto an EXISTING term, so its
    // value drifts without the options document moving (which would be
    // a batch-wide `vocabulary_drift` instead).
    await putValue(request, target, vocab.id, { value_options: ['beta'] });

    await applyWith(page, 'mintable is not committed', 1);
    await expect(page.getByTestId('batch-result')).toBeVisible();

    expect(await num(page, 'batch-outcome-conflict')).toBe(1);
    expect(await num(page, 'batch-outcome-changed')).toBe(0);

    // NOTHING was minted, and the surface says nothing was.
    const applies = await watch.applies();
    expect((applies[0].body as Record<string, unknown>).committed_terms ?? []).toEqual([]);
    await expect(page.getByTestId('batch-committed-terms')).toHaveAttribute('data-count', '0');
    await expect(page.getByTestId('batch-committed-terms')).not.toContainText(fresh);
  } finally {
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// append / remove are multi_select-ONLY, and they do not survive the
// field that made them available
//
// The hazard is a composition one rather than a rule one: `mode` is
// component state of its own, while the modes actually OFFERED are
// derived from the selected field's type. Pick `append` on a
// multi_select, switch to a text field, and a control that kept its
// stale value would send a mode the server refuses batch-wide with 422
// `mode_not_supported_for_type`, and worse, would be SHOWING one
// thing while SENDING another.
// ---------------------------------------------------------------------------

test('append does not survive a switch to a non-multi_select field', async ({ page }) => {
  const watch = watchBatch(page);
  try {
    let offered: string[] = [];
    let shown = '';

    const body = await previewOk(page, watch, [assetEntry(A4)], async () => {
      const modeSelect = page.getByTestId('batch-mode-select');

      // 1 + 2. A multi_select field, in a mode only multi_select has.
      await page.getByTestId('batch-field-select').selectOption(multiField.id);
      await expect(modeSelect.locator('option')).toHaveCount(4);
      await modeSelect.selectOption('append');
      await expect(modeSelect).toHaveValue('append');

      // 3. Switch to a TEXT field, whose modes do not include `append`.
      await page.getByTestId('batch-field-select').selectOption(field.id);

      offered = await modeSelect
        .locator('option')
        .evaluateAll((os) => os.map((o) => (o as HTMLOptionElement).value));
      shown = await modeSelect.inputValue();

      // The visible control and the bound value must AGREE. A select
      // sitting on `append` with no such option rendered, or on an
      // empty value with options available, is the failure either way.
      expect(offered, 'append and remove are withdrawn with the field').toEqual([
        'overwrite',
        'fill_empties',
      ]);
      expect(offered, 'the control shows a mode it actually offers').toContain(shown);

      await page.getByTestId(`field-input-${field.code}`).fill('after the switch');
    });

    // 4 + 5. THE REAL REQUEST. No stale append/remove reached the wire,
    // and what was sent is what the control was showing.
    const calls = await watch.previews();
    const sent = calls[calls.length - 1];
    expect(sent.payload.mode, 'the mode that was actually sent').toBe(shown);
    expect(['overwrite', 'fill_empties']).toContain(sent.payload.mode);
    // The server echoes the mode it read, so this is its reading too.
    expect(body.mode).toBe(shown);

    // And the withdrawal is not one-way: the modes come back with a
    // multi_select field, so `append` stayed multi_select-only rather
    // than being disabled everywhere.
    await page.getByTestId('batch-back').click();
    await page.getByTestId('batch-field-select').selectOption(multiField.id);
    const backAgain = await page
      .getByTestId('batch-mode-select')
      .locator('option')
      .evaluateAll((os) => os.map((o) => (o as HTMLOptionElement).value));
    expect(backAgain).toEqual(['overwrite', 'fill_empties', 'append', 'remove']);
  } finally {
    watch.dispose();
  }
});

// ---------------------------------------------------------------------------
// 390px. Mobile is a REDUCED app, not a shrunken one: what matters is
// that the bar and the whole flow are still REACHABLE there.
// ---------------------------------------------------------------------------

test('the selection bar and the batch flow are reachable at 390px', async ({ page }) => {
  const watch = watchBatch(page);
  try {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(PROFILE);
    await expect(page.getByTestId('profile-wall')).toBeVisible();
    await clearSelection(page);

    // The bar has to be reachable BEFORE the flow can be, so that is
    // asserted here rather than left to the baseline.
    await tick(page, 'asset', A4);
    await expect(page.getByTestId('selection-bar')).toBeVisible();
    await expect(page.getByTestId('selection-batch-edit')).toBeVisible();
    await page.getByTestId('selection-clear').click();

    await previewOk(page, watch, [assetEntry(A4)], () =>
      composeText(page, field.id, field.code, 'overwrite', 'from a phone'),
    );
  } finally {
    watch.dispose();
  }
});
