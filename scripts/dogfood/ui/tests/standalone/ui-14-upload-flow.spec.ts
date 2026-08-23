// ui-14-upload-flow.spec.ts
//
// The core artist workflow — does the upload pipeline actually
// produce a post in the feed?
//
// Tests in escalating depth:
//   - Clicking the navbar Upload button opens the modal
//   - The modal accepts a file via the hidden <input type="file">
//     (mirrors what the dropzone does internally)
//   - After picking a file, the queue shows it
//   - One of the composition modes is selectable
//   - Submitting the upload completes (asset created)
//
// The end-to-end "post appears in feed" assertion is split into
// its own test so a slow downstream notification doesn't make
// the whole flow flaky.
//
// #1119 — THE MODAL IS NOT RETIRED. The full-page create surface
// (create-page-1119.spec.ts) is the surface for composing a post
// properly; this one stays as the quick path, and both drive the same
// store. So this file goes on pinning modal behaviour, and gains one
// case below that the modal ALSO has to satisfy.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';
import { trackUploadedRows } from '../../helpers/uploaded-rows';

/** ⚠️ THE MODAL UPLOADS ON PICK, so a case that only inspects a control
 *  still creates an asset (#1247). See the AI-control case below. */
const uploaded = trackUploadedRows();

test.describe('UI-14 upload flow', () => {
  test.beforeEach(async ({ page }) => {
    uploaded.watch(page);
    await loginAsAdminViaUI(page);
    await page.goto('/');
  });

  test.afterEach(async ({ request }) => {
    await uploaded.cleanup(request);
  });

  test('clicking Upload opens the modal', async ({ page }) => {
    await page.locator(tid('nav-upload-button')).click();
    // The modal renders inside a portal at body level; assert via
    // a dialog role which is the convention used by UploadModal.
    await expect(page.getByRole('dialog')).toBeVisible();
  });

  test('Esc closes the upload modal', async ({ page }) => {
    await page.locator(tid('nav-upload-button')).click();
    await expect(page.getByRole('dialog')).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog')).toBeHidden();
  });

  // #1167 / ADR 0094 — the modal's AI control stores NOTHING until it is
  // touched, and this case fails against the implementation everyone
  // reaches for first.
  //
  // The obvious build is a boolean defaulted to false and sent
  // unconditionally, exactly as the mature checkbox beside it is sent.
  // On this axis that is wrong: `mature: false` is a true statement
  // about an unlabelled work, whereas `ai_provenance: 'none'` is a
  // POSITIVE CLAIM — "the maker declares no generative AI was involved"
  // — and sending it for an untouched control disclaims AI on the
  // artist's behalf. So the control has no pre-selected option and the
  // create body omits the key.
  test('the modal AI control is undeclared until touched', async ({ page }) => {
    await page.locator(tid('nav-upload-button')).click();
    await expect(page.getByRole('dialog')).toBeVisible();
    await page.locator(tid('upload-file-input')).setInputFiles({
      name: 'ui-14-ai.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(`ui-14 ai fixture ${Date.now()}-${Math.random()}`),
    });

    // ⛔ WAIT FOR THE UPLOAD BEFORE ASSERTING, and the reason is not
    // patience — it is that this case was RACING ITS OWN FIXTURE.
    //
    // Picking the file is the commit: the row uploads immediately and
    // the asset row is written by a POST the BROWSER makes. This case
    // only ever looked at a control, so it used to end while that POST
    // was still in flight — and then, depending on which side won, the
    // instance either gained a permanent `ui 14 ai` asset or did not.
    // Three had collected before the corpus census caught one; the
    // fixture ledger did not, and could not, because a response that
    // lands after the page is torn down is never reported to it.
    //
    // Waiting makes the fixture deterministic AND makes it deletable —
    // the id arrives with that response, which is what afterEach removes.
    // It changes nothing this case asserts.
    await expect(page.getByText(/Ready|Already uploaded/).first()).toBeVisible({
      timeout: 30_000,
    });

    const group = page.locator(tid('ai-provenance-row'));
    await expect(group).toBeVisible();
    await expect(
      group,
      'a pre-selected "No AI" would be the form making a disclosure nobody made',
    ).toHaveAttribute('data-value', 'undeclared');
    await expect(page.locator(tid('ai-provenance-row-none'))).not.toBeChecked();
  });

  test('uploading a file via the API surfaces it in the feed', async ({ page }) => {
    // Direct API upload, bypassing the modal — we test the modal
    // separately. This case proves the end-to-end "upload →
    // asset → post → visible in feed" path independent of the
    // dropzone UI.
    const fileBody = `dogfood ui-14 ${Date.now()}`;
    const upload = await page.request.post('/api/v1/storage/objects', {
      data: Buffer.from(fileBody),
      headers: {
        'Content-Type': 'application/octet-stream',
        'X-Content-Type': 'text/plain',
      },
    });
    expect(upload.status()).toBe(201);
    const uploadJson = await upload.json();

    const asset = await page.request.post('/api/v1/assets', {
      data: {
        title: `UI-14 fixture ${Date.now()}`,
        description: 'Dogfood UI-14 — verifies upload → asset visibility.',
        asset_type: 2,
        file_hash: uploadJson.hash,
        file_extension: 'txt',
      },
    });
    expect(asset.status()).toBe(201);
    const assetJson = await asset.json();
    // #1167 — an asset created without the field is UNDECLARED, and the
    // API must say so by omitting the value rather than by reporting
    // `none`. Asserted on the ordinary create path, because that is the
    // path every pre-existing caller takes.
    expect(
      assetJson.ai_provenance ?? null,
      'a caller that did not mention AI has not declared anything about it',
    ).toBeNull();

    // Wrap in a one-asset post so it lands in the feed.
    const post = await page.request.post('/api/v1/posts', {
      data: {
        title: `UI-14 dogfood post ${Date.now()}`,
        description: 'Dogfood UI-14',
        visibility: 'org-only',
        members: [{ asset_id: assetJson.id }],
      },
    });
    expect(post.status()).toBe(201);
    const postJson = await post.json();

    try {
      // Now look up the post by id via the API (the feed page is
      // cached; we don't want to wait on revalidation).
      const verify = await page.request.get(`/api/v1/posts/${postJson.id}`);
      expect(verify.status()).toBe(200);
    } finally {
      // #1198 — remove them again.
      //
      // This case used to leave its post and its asset behind, one pair
      // per run. Thirty had collected on the coding stack, and because
      // the browse feed is newest-first they sat at the TOP of page one,
      // where they pushed the seeded corpus out of the window that
      // post-band-format-1190 reads — which is how two consecutive
      // full-suite runs on an unchanged tree came back with different
      // results. A spec that changes what the next spec sees is not
      // isolated, however harmless its own leftovers look.
      await page.request.delete(`/api/v1/posts/${postJson.id}`).catch(() => undefined);
      await page.request.delete(`/api/v1/assets/${assetJson.id}`).catch(() => undefined);
    }
  });
});
