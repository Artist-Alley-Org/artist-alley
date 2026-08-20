// #754 — an uploaded multi-file model says what it still needs.
//
// # The bug
//
// A multi-file model only renders if its siblings are registered as
// companions. Nothing on the upload path derived that list, so an
// artist uploading `structure-wall.glb` without knowing it names
// `Textures/planks.png` got an upload that succeeded, a job that
// succeeded, and a card and viewer that came out grey. The failure is
// SILENT and reads as a renderer bug — the exact symptom chain #689
// chased into the renderer before #750 traced it to missing companion
// rows.
//
// # ⚠️ Why the fixture is BUILT rather than read from disk
//
// Storage is content-addressed and this stack's database PERSISTS
// between runs. A checked-in .glb uploads once and deduplicates forever
// after — the "new" upload resolves to the hash already stored and the
// assertion below would pass on whatever a previous run left behind, or
// on nothing at all. `buildGlbWithExternalTextures` embeds a nonce in
// the JSON chunk, so every run uploads genuinely new bytes. The nonce
// also rides the declared PATHS, so "the missing files are named" is
// asserted against names that cannot exist anywhere else.
//
// # What each case would catch
//
//  1. THE HEADLINE: the declared-but-unattached paths come back BY
//     NAME. A fix that merely reported a count, or a boolean, leaves the
//     artist exactly as stuck.
//
//  2. ATTACHING ONE MOVES IT. `missing` is a live subtraction against
//     the companion rows, not a snapshot taken at ingest — the answer
//     has to change when the artist acts on it, or the note it drives
//     never goes away.
//
//  3. AN UNREADABLE FORMAT IS NOT AN EMPTY ONE. `status: unsupported`
//     for a .png, never `ok` with an empty list. That distinction is the
//     one this whole endpoint exists to preserve: a confident "needs
//     nothing" about a file nobody parsed is the original bug wearing a
//     new hat.

import { test, expect } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { buildGlbWithExternalTextures } from '../../helpers/glb-fixture';

interface Requirements {
  asset_id: string;
  status: 'ok' | 'unsupported' | 'unreadable';
  partial: boolean;
  declared: string[];
  missing: string[];
  attached: string[];
  detail?: string;
}

/** Upload bytes and create the asset row for them. Returns the id. */
async function uploadAsset(
  request: import('@playwright/test').APIRequestContext,
  bytes: Buffer,
  ext: string,
): Promise<string> {
  const up = await request.post('/api/v1/storage/objects', {
    data: bytes,
    headers: {
      'Content-Type': 'application/octet-stream',
      'X-Content-Type': 'application/octet-stream',
    },
  });
  expect(up.status(), `upload bytes → ${up.status()} ${await up.text()}`).toBe(201);
  const { hash, deduped } = (await up.json()) as { hash: string; deduped?: boolean };
  expect(
    deduped ?? false,
    'the fixture deduplicated, so these bytes are NOT new and every assertion below ' +
      'would be about a previous run. The nonce in glb-fixture.ts exists to stop this.',
  ).toBeFalsy();

  const asset = await request.post('/api/v1/assets', {
    data: {
      title: `754 companion probe ${Date.now()}`,
      asset_type: 1, // generic — the server promotes by extension
      file_hash: hash,
      file_extension: ext,
    },
  });
  expect(asset.status(), `create asset → ${asset.status()} ${await asset.text()}`).toBe(201);
  return ((await asset.json()) as { id: string }).id;
}

async function requirements(
  request: import('@playwright/test').APIRequestContext,
  assetId: string,
): Promise<Requirements> {
  const r = await request.get(`/api/v1/assets/${assetId}/companion-requirements`);
  expect(r.status(), `requirements → ${r.status()} ${await r.text()}`).toBe(200);
  return (await r.json()) as Requirements;
}

test.describe('companion discovery at ingest (#754)', () => {
  test.beforeAll(async ({ request }) => {
    await loginAsAdminViaAPI(request);
  });

  test('a GLB with external textures names every file it is still missing', async ({
    request,
  }) => {
    const fixture = buildGlbWithExternalTextures([
      `Textures/planks-${Date.now()}.png`,
      `Textures/cobblestone-${Date.now()}.png`,
    ]);
    const assetId = await uploadAsset(request, fixture.bytes, 'glb');

    try {
      const req = await requirements(request, assetId);

      expect(
        req.status,
        'a GLB we CAN read must report `ok` — `unsupported` here would mean the ' +
          'extension table lost glb, which is exactly what #750 was',
      ).toBe('ok');
      expect(req.partial, 'GLB references are fully knowable from the bytes').toBe(false);

      // The headline. BY NAME, not by count.
      expect(req.declared.sort()).toEqual([...fixture.declared].sort());
      expect(
        req.missing.sort(),
        'nothing is attached yet, so every declared path is missing — and the artist ' +
          'needs the PATHS, because the path is what they have to name the file',
      ).toEqual([...fixture.declared].sort());
      expect(req.attached).toEqual([]);

      // The embedded geometry buffer has no `uri` and needs no
      // companion. If it appeared here the artist would be sent hunting
      // for a file that does not exist.
      expect(req.declared.some((p) => p.endsWith('.bin'))).toBe(false);
    } finally {
      await request.delete(`/api/v1/assets/${assetId}`).catch(() => undefined);
    }
  });

  test('attaching a companion moves it out of `missing`', async ({ request }) => {
    const fixture = buildGlbWithExternalTextures([
      `Textures/one-${Date.now()}.png`,
      `Textures/two-${Date.now()}.png`,
    ]);
    const assetId = await uploadAsset(request, fixture.bytes, 'glb');
    const [first, second] = fixture.declared;

    try {
      const attach = await request.post(`/api/v1/assets/${assetId}/companions`, {
        data: Buffer.from(`fake png ${Date.now()}`),
        headers: {
          'Content-Type': 'application/octet-stream',
          'X-Companion-Path': first,
          'X-Content-Type': 'image/png',
        },
      });
      expect(attach.status(), `attach → ${attach.status()} ${await attach.text()}`).toBe(201);

      const req = await requirements(request, assetId);
      expect(req.attached, 'the path just satisfied must move sides').toEqual([first]);
      expect(
        req.missing,
        'the subtraction is recomputed per request, not frozen at ingest — a note ' +
          'the artist can never clear is worse than no note',
      ).toEqual([second]);
      expect(req.declared.sort()).toEqual([...fixture.declared].sort());
    } finally {
      await request.delete(`/api/v1/assets/${assetId}`).catch(() => undefined);
    }
  });

  test('a format we cannot read reports `unsupported`, never an empty `ok`', async ({
    request,
  }) => {
    const assetId = await uploadAsset(
      request,
      Buffer.from(`not a model at all ${Date.now()}`),
      'png',
    );
    try {
      const req = await requirements(request, assetId);
      expect(
        req.status,
        '`ok` with an empty list would be the server claiming this file needs nothing, ' +
          'which it has no basis for. `unsupported` says "we cannot tell", which is true.',
      ).toBe('unsupported');
      expect(req.missing).toEqual([]);
      expect(req.declared).toEqual([]);
    } finally {
      await request.delete(`/api/v1/assets/${assetId}`).catch(() => undefined);
    }
  });
});
