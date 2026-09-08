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
// sharing a member, plus that member selected directly as an asset,
// must reach the server as FOUR entries and come back as THREE
// distinct targets, and no line of application code is permitted to
// know that.
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
    /** The one successful preview, with a NON-EMPTY OPAQUE TOKEN.
     *  Every apply assertion in this file goes through here, so no test
     *  can assert about an apply it never earned a token for. */
    async okPreview(): Promise<Record<string, unknown>> {
      const ok = (await settle()).filter((c) => c.path.endsWith('/preview') && c.status === 200);
      expect(ok, 'exactly one successful preview').toHaveLength(1);
      const body = ok[0].body as Record<string, unknown>;
      expect(typeof body.token, 'the preview token').toBe('string');
      expect((body.token as string).length, 'a non-empty opaque token').toBeGreaterThan(0);
      return body;
    },
    dispose() {
      page.off('response', onResponse);
    },
  };
}

// ---------------------------------------------------------------------------
// Driving the real surface
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// The corpus
// ---------------------------------------------------------------------------

let field: { id: string; code: string };
/** A1 is a member of BOTH posts. A2 belongs to P1 only, A3 to P2 only.
 *  A4 is never in a post, and is the N=1 direct-asset case. */
let A1 = '', A2 = '', A3 = '', A4 = '';
let P1 = '', P2 = '', PEMPTY = '';

test.beforeAll(async ({ request }) => {
  await loginAsAdminViaAPI(request);
  field = await makeField(request, 'text');
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
    await tick(page, 'asset', A4);
    await tick(page, 'post', P1);
    expect(await selectionCount(page), 'the mixed precondition really exists').toBe(2);

    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', 'mixed selection');
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();

    const calls = await watch.previews();
    expect(calls, 'a REAL preview request was sent').toHaveLength(1);

    // THE PAYLOAD. Two entries, each carrying its own kind. On the old
    // store this could only ever have been a list of bare uuids.
    const sel = calls[0].payload.selection as Array<{ kind: string; id: string }>;
    expect(sel).toHaveLength(2);
    expect(sel).toContainEqual({ kind: 'asset', id: A4 });
    expect(sel).toContainEqual({ kind: 'post', id: P1 });

    const body = await watch.okPreview();
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
    await tick(page, 'asset', A4);
    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', 'n1 asset');
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();

    const body = await watch.okPreview();
    const counts = body.counts as Record<string, number>;
    expect(body.selection_entry_count).toBe(1);
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
    await tick(page, 'post', PEMPTY);
    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', 'n1 empty post');
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();

    const body = await watch.okPreview();
    const counts = body.counts as Record<string, number>;

    // The entry is REAL: it was sent, and it is counted.
    expect((await watch.previews())[0].payload.selection).toEqual([
      { kind: 'post', id: PEMPTY },
    ]);
    expect(body.selection_entry_count).toBe(1);
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
    await tick(page, 'post', P1);
    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', 'n1 post');
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();

    const body = await watch.okPreview();
    expect(body.selection_entry_count).toBe(1);
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
    // P1 = {A1, A2}, P2 = {A1, A3}, and A1 is ALSO selected directly.
    // Four entries; three distinct targets.
    await tick(page, 'post', P1);
    await tick(page, 'post', P2);
    await tick(page, 'asset', A1);
    await tick(page, 'asset', A4);
    expect(await selectionCount(page), 'the overlap precondition exists').toBe(4);

    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', 'overlap');
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();

    const body = await watch.okPreview();
    const counts = body.counts as Record<string, number>;

    // The DISTINCT UNION of post-expanded members and direct asset
    // targets, computed here from the fixture and NOWHERE in the app.
    const union = new Set([A1, A2, A1, A3, A1, A4]);
    expect(counts.expanded).toBe(union.size);
    expect(body.selection_entry_count).toBe(4);

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
    await tick(page, 'asset', A4);
    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', 'confirm probe');
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();
    const body = await watch.okPreview();
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
    await tick(page, 'post', P1);
    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', `committed ${RUN}`);
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();

    const body = await watch.okPreview();
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
    await tick(page, 'asset', doomedAsset);
    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', 'gone probe');
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();
    await watch.okPreview();

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
    await tick(page, 'asset', target);
    await openBatch(page);
    await page.getByTestId('batch-field-select').selectOption(vocab.id);
    await page.getByTestId('batch-mode-select').selectOption('overwrite');
    const combo = page.getByTestId(`vocab-input-${vocab.code}`);
    await combo.fill(fresh);
    await page.getByTestId(`vocab-create-${vocab.code}`).click();

    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();
    const body = await watch.okPreview();
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

    await tick(page, 'asset', A4);
    await expect(page.getByTestId('selection-bar')).toBeVisible();
    await expect(page.getByTestId('selection-batch-edit')).toBeVisible();

    await openBatch(page);
    await composeText(page, field.id, field.code, 'overwrite', 'from a phone');
    await preview(page);
    await expect(page.getByTestId('batch-preview-panel')).toBeVisible();
    await watch.okPreview();
  } finally {
    watch.dispose();
  }
});
