// upload-original-basename-1595.spec.ts
//
// #1595: a file added to the upload queue on /create keeps its own name
// as the asset's write-once original base name.
//
//   - The raw upload (POST /storage/objects) carries File.name in
//     X-Original-Filename, percent-encoded as UTF-8, so a name outside
//     ISO-8859-1 survives the header.
//   - The queue's POST /assets passes that upload's receipt, and the
//     asset reads back with original_basename = the file's name and
//     original_basename_source = upload_request.
//   - Editing metadata.original_filename afterwards changes neither.
//
// The cover editor, the second caller of the same upload helper, is
// asserted in collection-cover-editor-1207.spec.ts.

import { test, expect } from '../../helpers/test';
import { tid } from '../../helpers/testids';

const STAMP = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

test.describe('#1595: the upload queue records the file name as provenance', () => {
  test('a queued file keeps its name as write-once upload_request provenance', async ({ page }) => {
    const name = `写真 été ${STAMP}.txt`;
    await page.goto('/create');

    const rawUpload = page.waitForRequest(
      (r) => r.method() === 'POST' && r.url().endsWith('/api/v1/storage/objects'),
    );
    const created = page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().endsWith('/api/v1/assets'),
      { timeout: 30_000 },
    );
    await page.locator(tid('create-file-input')).setInputFiles({
      name,
      mimeType: 'text/plain',
      buffer: Buffer.from(`1595 queue fixture ${STAMP} ${Math.random()}`),
    });

    const header = (await rawUpload).headers()['x-original-filename'];
    expect(header, 'the raw upload sent no X-Original-Filename').toBeTruthy();
    expect(decodeURIComponent(header!)).toBe(name);

    const res = await created;
    expect([200, 201], `POST /assets answered ${res.status()}`).toContain(res.status());
    const body = res.request().postDataJSON() as { upload_receipt?: string };
    expect(body.upload_receipt, 'the queue did not pass the upload receipt').toBeTruthy();
    const assetId = ((await res.json()) as { id: string }).id;

    try {
      const asset = (await (await page.request.get(`/api/v1/assets/${assetId}`)).json()) as {
        original_basename?: string | null;
        original_basename_source?: string;
      };
      expect(asset.original_basename).toBe(name);
      expect(asset.original_basename_source).toBe('upload_request');

      // The editable claim moves; the provenance does not.
      const edit = await page.request.patch(`/api/v1/assets/${assetId}`, {
        data: { metadata: { original_filename: 'renamed.txt' } },
      });
      expect(edit.status()).toBe(200);
      const after = (await (await page.request.get(`/api/v1/assets/${assetId}`)).json()) as {
        original_basename?: string | null;
        original_basename_source?: string;
        metadata?: { original_filename?: string };
      };
      expect(after.metadata?.original_filename).toBe('renamed.txt');
      expect(after.original_basename).toBe(name);
      expect(after.original_basename_source).toBe('upload_request');
    } finally {
      await page.request.delete(`/api/v1/assets/${assetId}`).catch(() => undefined);
    }
  });
});
