// helpers/iiif-fixture-collection.ts
//
// Phase 1.54.D — seed helper for the Mirador dogfood spec.
//
// Creates a self-contained mixed-format collection the ui-13
// dogfood spec loads into Mirador. Every asset uploaded here is
// deleted in the teardown so runs are idempotent — the same
// spec run twice against the same DB produces identical state.
//
// Fixture bytes live at scripts/dogfood/ui/fixtures/iiif/*.
// Kept small (<10 KB each) so the seed is instant.

import fs from 'node:fs';
import path from 'node:path';
import type { APIRequestContext } from '@playwright/test';

// Path resolves relative to Playwright's cwd (scripts/dogfood/ui/).
// Avoids import.meta, which the Playwright TS loader doesn't
// provide in this project's CJS-default package.json shape.
const FIXTURES = path.resolve('fixtures/iiif');

// asset_type values match the seed catalog (see apply-state files).
// 1 = image, 2 = document. Matches the field_definition applies_to
// binding used by the metadata extractor.
const ASSET_TYPE_IMAGE = 1;
const ASSET_TYPE_DOCUMENT = 2;

export type FixtureAssetSpec = {
  path: string;                     // absolute path to the fixture file
  extension: 'jpg' | 'png' | 'pdf';
  contentType: string;              // MIME type
  title: string;
  description: string;
  assetType: number;
  sensitivity?: 'public' | 'team' | 'restricted' | 'embargo';
  embargoUntil?: string;            // ISO8601; for embargo-stub manifest test
};

export type FixtureCollection = {
  id: string;
  assetIds: string[];
  name: string;
};

const RUN_TAG = `ui-13-${Date.now()}`;

/**
 * FIXTURE_ASSETS: the mixed-format asset roster the dogfood spec
 * needs. Ordering here determines the collection's canvas order —
 * the GPS-tagged JPEG comes first so Mirador's navigation strip
 * surfaces the navPlace-carrying canvas as the default open.
 *
 *   1. iiif-jpeg-1.jpg — GPS EXIF embedded (Eiffel Tower coords);
 *      exercises the navPlace geo-tag code path from 1.54.B.
 *   2. iiif-jpeg-2.jpg — plain JPEG; the second image so the
 *      canvas-count assertion has >1.
 *   3. iiif-png.png — PNG format; exercises non-JPEG variant path.
 *   4. iiif-multipage.pdf — 3-page PDF; surfaces the "Pages: N"
 *      metadata pair per the 1.54.B trim (per-page canvas emission
 *      is a deferred follow-up).
 *   5. embargoed — no bytes, just a placeholder metadata row with
 *      embargo_until set in the future; exercises the stub-manifest
 *      code path.
 */
export const FIXTURE_ASSETS: FixtureAssetSpec[] = [
  {
    path: path.join(FIXTURES, 'iiif-jpeg-1.jpg'),
    extension: 'jpg',
    contentType: 'image/jpeg',
    title: `Dogfood JPEG w/ GPS (${RUN_TAG})`,
    description: 'Fixture JPEG with GPS EXIF; exercises navPlace.',
    assetType: ASSET_TYPE_IMAGE,
    sensitivity: 'public',
  },
  {
    path: path.join(FIXTURES, 'iiif-jpeg-2.jpg'),
    extension: 'jpg',
    contentType: 'image/jpeg',
    title: `Dogfood JPEG plain (${RUN_TAG})`,
    description: 'Fixture JPEG; second image in the collection.',
    assetType: ASSET_TYPE_IMAGE,
    sensitivity: 'public',
  },
  {
    path: path.join(FIXTURES, 'iiif-png.png'),
    extension: 'png',
    contentType: 'image/png',
    title: `Dogfood PNG (${RUN_TAG})`,
    description: 'Fixture PNG; exercises non-JPEG asset variant.',
    assetType: ASSET_TYPE_IMAGE,
    sensitivity: 'public',
  },
  {
    path: path.join(FIXTURES, 'iiif-multipage.pdf'),
    extension: 'pdf',
    contentType: 'application/pdf',
    title: `Dogfood PDF 3-page (${RUN_TAG})`,
    description: 'Fixture 3-page PDF; surfaces "Pages: N" metadata.',
    assetType: ASSET_TYPE_DOCUMENT,
    sensitivity: 'public',
  },
];

/**
 * seedFixtureCollection: uploads each fixture asset via the storage
 * + assets APIs, creates a public collection, adds each asset as a
 * pinned member. Returns the collection ID + asset IDs so the spec
 * can pass them into Mirador + Content Search calls, and so the
 * teardown can delete them all.
 *
 * Authentication piggybacks on the APIRequestContext's cookie jar —
 * the caller should pass the already-authenticated `page.request`.
 */
export async function seedFixtureCollection(
  request: APIRequestContext,
  collectionName = `IIIF Mirador Dogfood (${RUN_TAG})`,
): Promise<FixtureCollection> {
  const assetIds: string[] = [];

  for (const spec of FIXTURE_ASSETS) {
    const bytes = fs.readFileSync(spec.path);
    const uploadRes = await request.post('/api/v1/storage/objects', {
      data: bytes,
      headers: {
        'Content-Type': 'application/octet-stream',
        'X-Content-Type': spec.contentType,
      },
    });
    if (!uploadRes.ok()) {
      throw new Error(`fixture upload failed for ${spec.path}: ${uploadRes.status()} ${await uploadRes.text()}`);
    }
    const uploadJson = await uploadRes.json();

    const assetRes = await request.post('/api/v1/assets', {
      data: {
        title: spec.title,
        description: spec.description,
        asset_type: spec.assetType,
        file_hash: uploadJson.hash,
        file_extension: spec.extension,
      },
    });
    if (!assetRes.ok()) {
      throw new Error(`asset create failed for ${spec.title}: ${assetRes.status()} ${await assetRes.text()}`);
    }
    const assetJson = await assetRes.json();
    assetIds.push(assetJson.id);
  }

  // Collection: public + explicit-share visibility is the closest
  // dev-DB analogue to a truly-public collection anon Mirador could
  // load. The Mirador embed here loads through the admin session
  // cookie anyway (the spec uses page.request), so anonymous
  // visibility isn't strictly required for this dogfood pass.
  const colRes = await request.post('/api/v1/collections', {
    data: {
      name: collectionName,
      description: 'Auto-seeded by ui-13-iiif-mirador-dogfood.spec.ts. Safe to delete.',
      visibility: 'private',
    },
  });
  if (!colRes.ok()) {
    throw new Error(`collection create failed: ${colRes.status()} ${await colRes.text()}`);
  }
  const collection = await colRes.json();

  // Pin each asset as a collection member. Endpoint is
  // POST /api/v1/collections/{id}/resources with body {asset_id,
  // sort_order, pinned} — 204 on success. sort_order is
  // 1-indexed to match how the Presentation loader orders members
  // (ORDER BY sort_order ASC per pre-audit Q2 of 1.54.B).
  for (let i = 0; i < assetIds.length; i++) {
    const addRes = await request.post(
      `/api/v1/collections/${collection.id}/resources`,
      { data: { asset_id: assetIds[i], sort_order: i + 1, pinned: true } },
    );
    if (!addRes.ok()) {
      throw new Error(`collection membership failed for asset ${assetIds[i]}: ${addRes.status()} ${await addRes.text()}`);
    }
  }

  return { id: collection.id, assetIds, name: collectionName };
}

/**
 * teardownFixtureCollection: hard-deletes the collection + every
 * asset it references. Best-effort — failures are logged but not
 * thrown so a failing test doesn't leak a follow-up teardown
 * failure and mask the real error.
 */
export async function teardownFixtureCollection(
  request: APIRequestContext,
  fixture: FixtureCollection,
): Promise<void> {
  try {
    await request.delete(`/api/v1/collections/${fixture.id}?hard=true`);
  } catch (e) {
    console.warn(`fixture teardown: collection delete failed: ${(e as Error).message}`);
  }
  // Assets get soft-deleted (only hard=true supported on collections
  // per the OpenAPI). Subsequent runs use a fresh RUN_TAG so titles
  // never collide with the soft-deleted rows.
  for (const assetId of fixture.assetIds) {
    try {
      await request.delete(`/api/v1/assets/${assetId}`);
    } catch (e) {
      console.warn(`fixture teardown: asset delete failed for ${assetId}: ${(e as Error).message}`);
    }
  }
}
