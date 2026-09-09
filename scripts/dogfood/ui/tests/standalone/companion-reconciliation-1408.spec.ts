// #1408 — a model and its companions dropped TOGETHER are reconciled.
//
// # The bug
//
// Interactive upload never looked at the batch. The global drop handler
// passed `dataTransfer.files` straight through and every `File` became
// its own `UploadRow`, which uploads the instant it exists — so a model
// plus three textures was four unrelated assets, the model's
// `companions` stayed empty, and #754's warning naming the missing
// files had nothing that could ever clear it. Four separate mechanisms
// held it in place: the batch was never inspected; `dataTransfer.files`
// throws the relative path away, so a directory drop arrives as
// basenames and `wood/diffuse.png` cannot be told from
// `metal/diffuse.png`; a manual attachment defaulted its path to the
// bare filename, which the server's EXACT-match rule can never satisfy
// against a declared `textures/img.jpg`; and /create had no attach
// control at all — it mounted the note naming the missing files on a
// page offering no way to supply one.
//
// # What each case would catch
//
//  1. SAME-DROP, WITH REAL DIRECTORIES, TWO MODELS. The headline. Both
//     models declare `textures/diffuse.png` and there are two different
//     files by that name. A fix that matched on basename attaches one
//     of them to both models and is wrong half the time — so the
//     assertion is on the BYTES stored against each asset, not on the
//     count of companion rows, because a swap is also two rows.
//
//  2. AN UNRELATED FILE CLEARS NOTHING. `wrong-name.png` in the same
//     batch must not satisfy a declaration, and the model must still
//     report the path it is genuinely missing.
//
//  3. A LATE ATTACHMENT IS ACTUALLY UPLOADED. Adding a companion after
//     the row reaches `ready` used to push onto a list nobody sent:
//     the row looked attached, the server had nothing, and the warning
//     stayed. Asserted against `GET /assets/{id}/companions`, and
//     against the note going away WITHOUT a reload.
//
//  4. AMBIGUITY IS SURFACED, NEVER GUESSED. Flat-selected, two models
//     declaring the same basename in different directories, one file.
//     There is no fact available that says which model wanted it. The
//     spec asserts NOTHING was attached and the question was asked —
//     then answers it and asserts the answer landed on the right model
//     and only that one.
//
// # ⚠️ Structural non-vacuity
//
// Each case asserts its own fixture first: that there really are two
// models, that their declared paths really do collide on basename, and
// that the declared list came back from the server rather than being
// assumed. A fixture that quietly lost a model would make every
// "no cross-wiring" assertion below trivially true.

import type { Page, APIRequestContext } from '@playwright/test';
import { test, expect } from '../../helpers/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';
import {
  buildGltf,
  markerFile,
  writeTree,
  titleForFilename,
  type FixtureTree,
} from '../../helpers/companion-fixture';

interface Companion {
  id: string;
  asset_id: string;
  path: string;
  size_bytes: number;
}

interface Requirements {
  status: 'ok' | 'unsupported' | 'unreadable';
  partial: boolean;
  declared: string[];
  missing: string[];
  attached: string[];
}

/**
 * Asset ids for files uploaded through the BROWSER.
 *
 * The POST happens in the page, so the id never reaches the test except
 * by watching the traffic. Recorded at creation and keyed by the title
 * the store derives from the filename — which carries this run's nonce,
 * so it names exactly one row in the database.
 */
function watchAssetIds(page: Page): Map<string, string> {
  const byTitle = new Map<string, string>();
  page.on('response', async (res) => {
    if (res.request().method().toUpperCase() !== 'POST') return;
    if (res.status() !== 201) return;
    let path: string;
    try {
      path = new URL(res.url()).pathname;
    } catch {
      return;
    }
    if (path !== '/api/v1/assets') return;
    let title = '';
    try {
      title = (JSON.parse(res.request().postData() ?? '{}') as { title?: string }).title ?? '';
    } catch {
      return;
    }
    try {
      const id = ((await res.json()) as { id?: string }).id;
      if (id && title) byTitle.set(title, id);
    } catch {
      /* body already consumed or not JSON */
    }
  });
  return byTitle;
}

async function assetIdFor(ids: Map<string, string>, filename: string): Promise<string> {
  const title = titleForFilename(filename);
  await expect
    .poll(() => ids.get(title), {
      timeout: 45_000,
      message: `no POST /assets was seen for ${filename} (title "${title}")`,
    })
    .toBeTruthy();
  return ids.get(title)!;
}

async function companionsOf(request: APIRequestContext, assetId: string): Promise<Companion[]> {
  const r = await request.get(`/api/v1/assets/${assetId}/companions`);
  expect(r.status(), `companions → ${r.status()} ${await r.text()}`).toBe(200);
  return (await r.json()) as Companion[];
}

async function requirementsOf(
  request: APIRequestContext,
  assetId: string,
): Promise<Requirements> {
  const r = await request.get(`/api/v1/assets/${assetId}/companion-requirements`);
  expect(r.status(), `requirements → ${r.status()} ${await r.text()}`).toBe(200);
  return (await r.json()) as Requirements;
}

/** The marker embedded in a stored companion's bytes. Identity, not shape. */
async function companionMarker(
  request: APIRequestContext,
  assetId: string,
  companionId: string,
): Promise<string> {
  const r = await request.get(`/api/v1/assets/${assetId}/companions/${companionId}`);
  expect(r.status(), `companion bytes → ${r.status()}`).toBe(200);
  const body = (await r.body()).toString('utf8');
  const m = /AA-1408-FIXTURE:([^:]+):/.exec(body);
  return m ? m[1] : `<no marker in ${body.slice(0, 40)}>`;
}

const created: string[] = [];
const createdPosts: string[] = [];
let trees: FixtureTree[] = [];

test.describe('same-drop companion reconciliation (#1408)', () => {
  test.beforeAll(async ({ request }) => {
    await loginAsAdminViaAPI(request);
  });

  test.afterEach(async ({ request }) => {
    // Posts before the assets they hold: deleting a post leaves its
    // members standing, and the reverse order leaves a post pointing at
    // deleted files for as long as the loop runs.
    for (const id of createdPosts.splice(0)) {
      await request.delete(`/api/v1/posts/${id}`).catch(() => undefined);
    }
    for (const id of created.splice(0)) {
      await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
    }
    for (const t of trees.splice(0)) t.cleanup();
  });

  // ── 1. THE HEADLINE ───────────────────────────────────────────────
  test(
    'a folder of TWO models with colliding texture names reconciles without cross-wiring (modal)',
    async ({ page, request }) => {
      const n = `${Date.now()}${Math.floor(Math.random() * 1000)}`;
      const woodModel = `wood${n}.gltf`;
      const metalModel = `metal${n}.gltf`;
      const stray = `stray${n}.png`;

      // ⭐ The collision is DELIBERATE and identical on both sides:
      // same declared string, same basename, different directories once
      // resolved against each model's own location.
      const declaredRel = 'textures/diffuse.png';
      const tree = writeTree({
        [`wood/${woodModel}`]: buildGltf([declaredRel], `wood-${n}`),
        'wood/textures/diffuse.png': markerFile(`WOOD-${n}`),
        [`metal/${metalModel}`]: buildGltf([declaredRel], `metal-${n}`),
        'metal/textures/diffuse.png': markerFile(`METAL-${n}`),
        // Negative control: nothing declares it, so it must become its
        // own asset and satisfy nothing.
        [stray]: markerFile(`STRAY-${n}`),
      });
      trees.push(tree);

      const ids = watchAssetIds(page);
      await page.goto('/');
      await page.locator(tid('nav-upload-button')).click();

      // A `webkitdirectory` input is what makes `webkitRelativePath`
      // exist at all. Without it the browser hands over basenames and
      // the two `diffuse.png` files are indistinguishable — which is
      // case 4 below, not this one.
      const folderInput = page.locator(tid('upload-folder-input'));
      await expect(
        folderInput,
        'the modal has no folder input, so a directory selection cannot preserve ' +
          'relative paths and same-drop reconciliation cannot work (#1408)',
      ).toHaveCount(1);
      await folderInput.setInputFiles(tree.root);

      const woodId = await assetIdFor(ids, woodModel);
      const metalId = await assetIdFor(ids, metalModel);
      const strayId = await assetIdFor(ids, stray);
      created.push(woodId, metalId, strayId);

      // ⚠️ STRUCTURAL: prove the fixture is what the rest of this test
      // claims — two DISTINCT models, and a declared basename that
      // genuinely collides across them, read back from the server
      // rather than assumed.
      expect(woodId).not.toBe(metalId);
      const woodReq = await requirementsOf(request, woodId);
      const metalReq = await requirementsOf(request, metalId);
      expect(woodReq.declared).toEqual([declaredRel]);
      expect(metalReq.declared).toEqual([declaredRel]);
      expect(
        new Set([...woodReq.declared, ...metalReq.declared].map((p) => p.split('/').pop())).size,
        'the two models must declare the SAME basename or this test proves nothing',
      ).toBe(1);

      // The warning goes away by itself, on both rows, with no reload.
      await expect(page.locator('[data-testid^="companion-req-satisfied-"]')).toHaveCount(2, {
        timeout: 60_000,
      });
      await expect(page.locator('[data-testid^="companion-req-missing-"]')).toHaveCount(0);

      // ⭐ PERSISTED IDENTITY. Not "two companion rows exist" — a swap
      // is also two rows. The bytes stored against each asset say which
      // file landed where.
      const woodComps = await companionsOf(request, woodId);
      const metalComps = await companionsOf(request, metalId);
      expect(woodComps.map((c) => c.path)).toEqual([declaredRel]);
      expect(metalComps.map((c) => c.path)).toEqual([declaredRel]);
      expect(await companionMarker(request, woodId, woodComps[0].id)).toBe(`WOOD-${n}`);
      expect(await companionMarker(request, metalId, metalComps[0].id)).toBe(`METAL-${n}`);

      // ⛔ The batch actually SETTLED. `heldCandidates` is what this
      // banner reads and what blocks Publish, so a candidate left in
      // the held list after it was successfully attached leaves a
      // permanent progress note over a button that refuses. Every
      // companion can say DONE while this is still on screen.
      await expect(page.locator(tid('companion-holding-modal'))).toHaveCount(0);
      await expect(page.locator(tid('companion-decisions-modal'))).toHaveCount(0);

      // The negative control became its own asset and touched neither.
      const strayComps = await companionsOf(request, strayId);
      expect(strayComps).toEqual([]);
      expect(woodComps).toHaveLength(1);
      expect(metalComps).toHaveLength(1);
    },
  );

  // ── 2 + 3. /create: unrelated file, and a LATE attachment ──────────
  test(
    'on /create an unrelated file clears nothing and a late attachment is really uploaded',
    async ({ page, request }) => {
      const n = `${Date.now()}${Math.floor(Math.random() * 1000)}`;
      const model = `solo${n}.gltf`;
      const diffuse = `diffuse${n}.png`;
      const normal = `normal${n}.png`;
      const wrong = `wrong${n}.png`;

      const tree = writeTree({
        [model]: buildGltf([diffuse, normal], `solo-${n}`),
        [diffuse]: markerFile(`DIFFUSE-${n}`),
        [normal]: markerFile(`NORMAL-${n}`),
        [wrong]: markerFile(`WRONG-${n}`),
      });
      trees.push(tree);

      const ids = watchAssetIds(page);
      await page.goto('/create');

      // ⚠️ SCOPED TO THIS PAGE'S LIST, on purpose. The upload MODAL is
      // mounted once in the layout and renders the same store rows even
      // while closed, so an unscoped `[data-testid^="companion-req-..."]`
      // counts every note twice and the count says nothing about which
      // surface rendered it. This spec's claim is about /create.
      // `:not(...-path)` drops the per-path <li> inside the note, which
      // shares the prefix.
      const createMissing = page.locator(
        '[data-testid="create-file-list"] [data-testid^="companion-req-missing-"]' +
          ':not([data-testid="companion-req-missing-path"])',
      );
      const createSatisfied = page.locator(
        '[data-testid="create-file-list"] [data-testid^="companion-req-satisfied-"]',
      );
      const createMissingPaths = page.locator(
        '[data-testid="create-file-list"] [data-testid="companion-req-missing-path"]',
      );

      // Flat selection — three individual files, no directory. This is
      // the case with NO relative path information at all.
      await page
        .locator(tid('create-file-input'))
        .setInputFiles([tree.path(model), tree.path(diffuse), tree.path(wrong)]);

      const modelId = await assetIdFor(ids, model);
      const wrongId = await assetIdFor(ids, wrong);
      created.push(modelId, wrongId);

      // ⚠️ STRUCTURAL: the model really declares TWO files, only one of
      // which was supplied.
      const before = await requirementsOf(request, modelId);
      expect(before.declared.sort()).toEqual([diffuse, normal].sort());

      // The declared file attached itself; the unrelated one did not,
      // and became its own asset instead.
      await expect
        .poll(async () => (await companionsOf(request, modelId)).map((c) => c.path), {
          timeout: 60_000,
        })
        .toEqual([diffuse]);
      expect(
        await companionsOf(request, wrongId),
        'the unrelated file must be an ASSET, not a companion',
      ).toEqual([]);

      // ⛔ And it cleared NOTHING: the file the model is genuinely
      // missing is still reported, by name.
      const mid = await requirementsOf(request, modelId);
      expect(mid.missing).toEqual([normal]);
      expect(mid.attached).toEqual([diffuse]);
      await expect(createMissing).toHaveCount(1);
      // By NAME, on the page — #754's contract, still intact.
      await expect(createMissingPaths).toHaveText([normal]);

      // ── the late attachment. The row is READY; this is precisely the
      // moment the old code pushed onto a list nobody ever sent.
      const attach = page.locator(tid('create-add-companion'));
      await expect(
        attach,
        '/create mounted the note naming the missing file and NO control that could ' +
          'supply one — the picker existed only inside the modal (#1408)',
      ).toHaveCount(1);
      // Through the REAL control: the artist clicks "add companion" and
      // picks a file, which is the path the hidden input alone would
      // not exercise (it never learns which row asked).
      const [chooser] = await Promise.all([
        page.waitForEvent('filechooser'),
        attach.click(),
      ]);
      await chooser.setFiles([tree.path(normal)]);

      // The path was SUGGESTED from what the model declares, not typed.
      // `path: file.name` was the old default; where a model declares a
      // subdirectory that default can never satisfy the server's exact
      // match, and the artist was left to work that out unaided.
      const lateRow = page.locator(tid('create-companion-row')).filter({ hasText: normal });
      await expect(lateRow).toHaveCount(1);
      await expect(lateRow.locator(tid('create-companion-path'))).toHaveValue(normal);

      // Persisted, and the note goes away with no reload.
      await expect
        .poll(async () => (await companionsOf(request, modelId)).map((c) => c.path).sort(), {
          timeout: 60_000,
        })
        .toEqual([diffuse, normal].sort());
      await expect(createSatisfied).toHaveCount(1, { timeout: 30_000 });
      await expect(createMissing).toHaveCount(0);

      const after = await requirementsOf(request, modelId);
      expect(after.missing).toEqual([]);
      const comps = await companionsOf(request, modelId);
      const late = comps.find((c) => c.path === normal)!;
      expect(await companionMarker(request, modelId, late.id)).toBe(`NORMAL-${n}`);

      // ⛔ And the TWO-ACTION FLOW still ends in a published post.
      // Holding a candidate blocks submit by design, so a candidate
      // left in the held list after being attached would fail here and
      // nowhere else — every companion reads DONE either way.
      await expect(page.locator(tid('companion-holding-create'))).toHaveCount(0);
      const publish = page.locator(tid('create-publish'));
      await publish.scrollIntoViewIfNeeded();
      await expect(publish).toBeEnabled({ timeout: 30_000 });
      const [postRes] = await Promise.all([
        page.waitForResponse(
          (r) => new URL(r.url()).pathname === '/api/v1/posts' && r.request().method() === 'POST',
        ),
        publish.click(),
      ]);
      expect(postRes.status(), 'publishing a reconciled model must still work').toBe(201);
      createdPosts.push(((await postRes.json()) as { id: string }).id);
      await expect(page.locator(tid('create-error'))).toHaveCount(0);
    },
  );

  // ── 4. AMBIGUITY IS SURFACED, NEVER GUESSED ───────────────────────
  test(
    'a colliding basename with no directory information is asked about, not guessed (modal)',
    async ({ page, request }) => {
      const n = `${Date.now()}${Math.floor(Math.random() * 1000)}`;
      const woodModel = `awood${n}.gltf`;
      const metalModel = `ametal${n}.gltf`;

      // Two models, the SAME basename declared in DIFFERENT directories,
      // and one file. Flat-selected, so nothing says which is meant.
      const tree = writeTree({
        [woodModel]: buildGltf(['wood/diffuse.png'], `awood-${n}`),
        [metalModel]: buildGltf(['metal/diffuse.png'], `ametal-${n}`),
        'diffuse.png': markerFile(`SHARED-${n}`),
      });
      trees.push(tree);

      const ids = watchAssetIds(page);
      await page.goto('/');
      await page.locator(tid('nav-upload-button')).click();
      await page
        .locator(tid('upload-file-input'))
        .setInputFiles([tree.path(woodModel), tree.path(metalModel), tree.path('diffuse.png')]);

      const woodId = await assetIdFor(ids, woodModel);
      const metalId = await assetIdFor(ids, metalModel);
      created.push(woodId, metalId);

      // ⚠️ STRUCTURAL: two models, one shared basename, two distinct
      // declared paths — the definition of the ambiguity under test.
      const woodReq = await requirementsOf(request, woodId);
      const metalReq = await requirementsOf(request, metalId);
      expect(woodReq.declared).toEqual(['wood/diffuse.png']);
      expect(metalReq.declared).toEqual(['metal/diffuse.png']);

      // The question is ASKED.
      const decisions = page.locator(tid('companion-decisions-modal'));
      await expect(decisions, 'ambiguity must be surfaced, never guessed').toBeVisible({
        timeout: 60_000,
      });
      await expect(page.locator(tid('companion-decision'))).toHaveCount(1);

      // ⛔ And nothing was attached while it stands unanswered — on
      // EITHER model. A guess would have put it on one of them.
      expect(await companionsOf(request, woodId)).toEqual([]);
      expect(await companionsOf(request, metalId)).toEqual([]);
      // Nor did it quietly become a third asset behind the artist's back.
      expect(ids.has(titleForFilename('diffuse.png'))).toBe(false);

      // Answer it: wood. And only wood.
      await page.locator(tid('companion-decision-target')).selectOption({ index: 0 });
      const chosenPath = await page.locator(tid('companion-decision-path')).inputValue();
      const chosenIsWood = chosenPath === 'wood/diffuse.png';
      const targetId = chosenIsWood ? woodId : metalId;
      const otherId = chosenIsWood ? metalId : woodId;
      await page.locator(tid('companion-decision-attach')).click();

      await expect
        .poll(async () => (await companionsOf(request, targetId)).map((c) => c.path), {
          timeout: 60_000,
        })
        .toEqual([chosenPath]);
      expect(
        await companionsOf(request, otherId),
        'answering for one model must not attach anything to the other',
      ).toEqual([]);
      await expect(page.locator(tid('companion-decisions-modal'))).toHaveCount(0);
    },
  );
});
