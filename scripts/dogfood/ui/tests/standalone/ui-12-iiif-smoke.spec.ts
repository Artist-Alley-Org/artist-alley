// ui-12-iiif-smoke.spec.ts
//
// Phase 1.54.B smoke — IIIF Presentation API 3.0 + Content Search 2.0
// + 2.0→3.0 redirect. Three lightweight structural assertions per the
// brief; pixel snapshots are deliberately absent since Mirador upgrades
// break deep tests routinely.
//
// Preconditions:
//   - Dev stack up (docker compose up -d) with the API mounted at
//     :5173 via Vite; admin bootstrap present.
//   - At least one asset + one collection exist in the seed DB. When
//     the ambient dev DB has neither (fresh clone), the tests skip
//     with a clear reason rather than failing spuriously.

import { test, expect, type APIRequestContext } from '../../helpers/test';
import { request } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

// ── The manifest fixture (#1227) ─────────────────────────────────────
//
// `firstAssetID` returns whatever is newest, and the canvas assertions
// below used to sit behind `if (Array.isArray(manifest.items) && …)`
// with a comment admitting why: "we can't guarantee the ambient DB has
// a non-embargoed asset". Correct, and it made the Canvas /
// AnnotationPage / ImageService3 checks — the substance of the test —
// conditional on the order of a shared corpus. Nothing said which runs
// had exercised them, and an embargoed asset arriving at the head of the
// list would have turned the test into a shape check on an embargo stub.
//
// A provisioned asset settles it: the builder emits exactly one canvas
// for every non-embargoed asset (presentation/builder.go:115) and none
// for the embargo stub (:243), so a fresh upload is guaranteed to be the
// case the assertions describe, and they run unconditionally.
//
// Only the manifest test needs it. The redirect + reachability tests
// keep reading the catalogue on purpose: their subject is the ROUTE and
// the asset id is a path segment they never look inside, so any id
// answers the question they ask.
const MANIFEST_FIXTURE_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==',
  'base64',
);
let manifestAssetID: string | undefined;

// Grab the first asset + first collection from the admin surface. Both
// are used across the three tests; keeping the discovery in a helper
// (not a shared beforeAll) so a missing entity skips just its own test.
async function firstAssetID(baseURL: string, cookies: string): Promise<string | null> {
  const api = await request.newContext({ baseURL, extraHTTPHeaders: { cookie: cookies } });
  try {
    // /api/v1/assets returns { items: [...] }; take the first id.
    const r = await api.get('/api/v1/assets?limit=1');
    if (!r.ok()) return null;
    const body = await r.json();
    return body?.items?.[0]?.id ?? null;
  } finally {
    await api.dispose();
  }
}

async function firstCollectionID(baseURL: string, cookies: string): Promise<string | null> {
  const api = await request.newContext({ baseURL, extraHTTPHeaders: { cookie: cookies } });
  try {
    const r = await api.get('/api/v1/collections?limit=1');
    if (!r.ok()) return null;
    const body = await r.json();
    return body?.items?.[0]?.id ?? null;
  } finally {
    await api.dispose();
  }
}

// An asset info.json can actually be built for, plus the dimensions the
// browse payload reports for it — so the test can assert the two
// surfaces AGREE rather than merely that neither is empty.
//
// The predicate is exactly the handler's own two preconditions for a
// 200 (iiif/http.go serveInfo), so a selected asset cannot 404 for an
// unrelated reason:
//
//   - `preview_available` — a servable `col` exists. `col` is a
//     configured variant with MaxDim > 0, so this implies
//     servableVariant(), which asks for ANY configured rung, not all.
//     (`ladder_available` would also do, but it demands the COMPLETE
//     ladder and would skip assets info.json serves perfectly well.)
//   - a positive pixel_width/pixel_height pair — BuildInfo returns
//     ErrUnsupportedAsset on a non-positive dimension, and PoolLookup
//     COALESCEs an absent value to 0.
//
// `scanned` is returned so the caller can tell "this install has no
// assets" (skip, fresh clone) from "this install has assets and not one
// of them is IIIF-servable" (fail — that is #757 itself).
type ServableAsset = { id: string; width: number; height: number };

async function firstIIIFServableAsset(
  baseURL: string,
  cookies: string,
): Promise<{ asset: ServableAsset | null; scanned: number }> {
  const api = await request.newContext({ baseURL, extraHTTPHeaders: { cookie: cookies } });
  try {
    const r = await api.get('/api/v1/assets?limit=50');
    if (!r.ok()) return { asset: null, scanned: 0 };
    const items: any[] = (await r.json())?.items ?? [];
    const hit = items.find(
      (a) => a?.preview_available === true && a?.pixel_width > 0 && a?.pixel_height > 0,
    );
    return {
      asset: hit ? { id: hit.id, width: hit.pixel_width, height: hit.pixel_height } : null,
      scanned: items.length,
    };
  } finally {
    await api.dispose();
  }
}

test.describe('UI-12 IIIF Presentation + Content Search smoke', () => {
  test.beforeAll(async ({ request: api }: { request: APIRequestContext }) => {
    const up = await api.post('/api/v1/storage/objects', {
      data: MANIFEST_FIXTURE_PNG,
      headers: { 'Content-Type': 'application/octet-stream', 'X-Content-Type': 'image/png' },
    });
    expect(up.status(), 'uploading the manifest fixture bytes').toBe(201);
    const fileHash = String(((await up.json()) as { hash: string }).hash);
    const stamp = Date.now();
    const r = await api.post('/api/v1/assets', {
      data: {
        title: `UI-12 manifest fixture ${stamp}`,
        asset_type: 1,
        file_extension: 'png',
        file_hash: fileHash,
        original_filename: `ui-12-manifest-${stamp}.png`,
      },
    });
    expect(r.status(), 'creating the manifest fixture asset').toBeLessThan(300);
    manifestAssetID = String(((await r.json()) as { id: string }).id);
  });

  test.afterAll(async ({ request: api }: { request: APIRequestContext }) => {
    if (!manifestAssetID) return;
    await api.delete(`/api/v1/assets/${manifestAssetID}`).catch(() => undefined);
  });

  test('collection manifest is valid IIIF 3.0 JSON', async ({ page, context, baseURL }) => {
    await loginAsAdminViaUI(page);
    const cookies = (await context.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
    const cid = await firstCollectionID(baseURL!, cookies);
    test.skip(!cid, 'no collections in dev DB — seed one first');

    // Fetch the manifest through the /api/v1 mount (matches 1.54.A
    // shape until the /iiif/3 alias lands).
    const r = await page.request.get(`/api/v1/iiif/3/collection/${cid}/manifest.json`);
    expect(r.ok(), `manifest fetch failed: ${r.status()}`).toBe(true);
    const manifest = await r.json();

    // IIIF Presentation 3.0 structural asserts. The @context may be
    // a plain string OR an array (navPlace present) — handle both.
    const ctxField = manifest['@context'];
    const contexts = Array.isArray(ctxField) ? ctxField : [ctxField];
    expect(contexts).toContain('http://iiif.io/api/presentation/3/context.json');
    expect(manifest.type).toBe('Collection');
    expect(manifest.id).toContain(`/iiif/3/collection/${cid}/manifest.json`);
    expect(manifest.label).toBeTruthy();
    // items[] is the member list; may be empty for a fresh
    // collection, which is legal.
    expect(Array.isArray(manifest.items)).toBe(true);
    // SeeAlso should surface the Content Search 2.0 service.
    const hasSearch = (manifest.seeAlso ?? []).some((s: any) => s.type === 'SearchService2');
    expect(hasSearch, 'seeAlso should surface SearchService2').toBe(true);
  });

  test('asset manifest is valid IIIF 3.0 JSON with a canvas', async ({ page }) => {
    await loginAsAdminViaUI(page);
    const aid = manifestAssetID;
    expect(aid, 'the manifest fixture did not land in beforeAll').toBeTruthy();

    const r = await page.request.get(`/api/v1/iiif/3/asset/${aid}/manifest.json`);
    expect(r.ok()).toBe(true);
    const manifest = await r.json();

    const ctxField = manifest['@context'];
    const contexts = Array.isArray(ctxField) ? ctxField : [ctxField];
    expect(contexts).toContain('http://iiif.io/api/presentation/3/context.json');
    expect(manifest.type).toBe('Manifest');
    expect(manifest.id).toContain(`/iiif/3/asset/${aid}/manifest.json`);
    expect(manifest.label).toBeTruthy();
    // The fixture asset is freshly uploaded and therefore NOT embargoed,
    // which is what makes this unconditional: the builder emits exactly
    // one canvas for a readable asset and none for the embargo stub, so
    // "items is empty" is now a failure and not a case to step around.
    expect(
      Array.isArray(manifest.items) && manifest.items.length > 0,
      'a readable asset manifest must carry a Canvas; an empty items[] is the embargo stub, ' +
        'and this fixture is not embargoed',
    ).toBe(true);
    const canvas = manifest.items[0];
    expect(canvas.type).toBe('Canvas');
    expect(canvas.items?.[0]?.type).toBe('AnnotationPage');
    // The AnnotationPage's Annotation targets the Canvas via
    // motivation=painting for an image body.
    const ann = canvas.items[0].items?.[0];
    expect(ann?.motivation).toBe('painting');
    // Body carries an ImageService3 profile:level0 reference —
    // this is what makes Mirador tile via 1.54.A's Image API.
    const svc = ann?.body?.service?.[0];
    expect(svc?.type).toBe('ImageService3');
    expect(svc?.profile).toBe('level0');
  });

  test('legacy /iiif/2/{id}/info.json redirects 301 to /iiif/3/', async ({ page, context, baseURL }) => {
    await loginAsAdminViaUI(page);
    const cookies = (await context.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
    const aid = await firstAssetID(baseURL!, cookies);
    test.skip(!aid, 'no assets in dev DB — upload one first');

    // Disable auto-redirect so we can assert on the 301 + Location.
    const r = await page.request.get(`/api/v1/iiif/2/${aid}/info.json`, {
      maxRedirects: 0,
    });
    expect(r.status()).toBe(301);
    const loc = r.headers()['location'];
    expect(loc).toContain(`/iiif/3/${aid}/info.json`);
  });

  test('content search returns a valid AnnotationPage', async ({ page, context, baseURL }) => {
    await loginAsAdminViaUI(page);
    const cookies = (await context.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
    const cid = await firstCollectionID(baseURL!, cookies);
    test.skip(!cid, 'no collections in dev DB — seed one first');

    // A generic query that likely returns zero hits is fine — the
    // AnnotationPage shape MUST still validate.
    const r = await page.request.get(`/api/v1/iiif/3/collection/${cid}/search?q=test`);
    expect(r.ok()).toBe(true);
    const page2 = await r.json();
    expect(page2['@context']).toBe('http://iiif.io/api/search/2/context.json');
    expect(page2.type).toBe('AnnotationPage');
    expect(Array.isArray(page2.items)).toBe(true);
  });

  test('/admin/iiif/health renders with subsystem card', async ({ page }) => {
    await loginAsAdminViaUI(page);
    // The health endpoint is admin-gated; hitting it also exercises
    // the healthhandler shim.
    const r = await page.request.get('/api/v1/admin/iiif/health');
    expect(r.ok()).toBe(true);
    const health = await r.json();
    expect(health.subsystem).toBe('iiif');
    expect(health.by_result).toBeTruthy();
    // Notes[] carries operator hints + latency percentiles per
    // the B-5 dashboard subsystem-card shape.
    expect(Array.isArray(health.notes)).toBe(true);
    expect(health.notes.length).toBeGreaterThan(0);
    // The dashboard fetches this alongside /admin/search/health.
    await page.goto('/admin/search/dashboard');
    await expect(page.getByRole('heading', { name: /IIIF/i })).toBeVisible();
  });

  // Phase 1.54.C — regressions for the /iiif/{2,3} external URL
  // alias. Third-party viewers (Mirador, Universal Viewer,
  // OpenSeadragon embeds) fetch the URLs the handler emits
  // (publicBaseURL(r) + "/iiif/3/...") which do NOT include the
  // /api/v1 prefix that the mount point imposes. The nginx
  // rewrite in infra/nginx/default.conf bridges the gap; these
  // tests hit the EXTERNAL URL directly to prevent this bug from
  // silently regressing.
  //
  // The internal /api/v1/iiif/3/... tests above still exist as
  // regression coverage for the internal path.

  test('Presentation manifest reachable at external /iiif/3/ URL', async ({ page, context, baseURL }) => {
    await loginAsAdminViaUI(page);
    const cookies = (await context.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
    const cid = await firstCollectionID(baseURL!, cookies);
    test.skip(!cid, 'no collections in dev DB — seed one first');

    // Hit the EXTERNAL URL — no /api/v1 prefix. This is what
    // Mirador embeds actually fetch.
    const r = await page.request.get(`/iiif/3/collection/${cid}/manifest.json`);
    expect(r.ok(), `external manifest fetch failed: ${r.status()} — nginx rewrite regression?`).toBe(true);
    const manifest = await r.json();
    const ctxField = manifest['@context'];
    const contexts = Array.isArray(ctxField) ? ctxField : [ctxField];
    expect(contexts).toContain('http://iiif.io/api/presentation/3/context.json');
    // CORS is non-negotiable for cross-origin viewers.
    expect(r.headers()['access-control-allow-origin']).toBe('*');
  });

  test('Image API info.json reachable at external /iiif/3/ URL', async ({ page, context, baseURL }) => {
    await loginAsAdminViaUI(page);
    const cookies = (await context.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
    const aid = await firstAssetID(baseURL!, cookies);
    test.skip(!aid, 'no assets in dev DB — upload one first');

    // Image API 3.0 info.json at the EXTERNAL /iiif/3/ path. This
    // test verifies the ROUTE is reachable, not that the specific
    // asset happens to render — the seed data's first asset isn't
    // guaranteed IIIF-tile-ready. What we're regression-testing here
    // is the /iiif/3/{id}/... alias: if the request falls through to
    // the SPA (embed_web static handler), the content-type will be
    // text/html; if the Go handler answered, content-type is JSON —
    // whether the body is a valid info doc (200) or "asset not
    // IIIF-compatible" (404) is a separate concern.
    //
    // THE ASSERTION HAS TO ADMIT BOTH JSON MEDIA TYPES, and the
    // original `toContain('application/json')` did not (#757). The
    // success path sets `application/ld+json; profile="…"` per Image
    // API 3.0 §5.1, and `application/json` is NOT a substring of
    // `application/ld+json` — the `ld+` sits in the middle. Only the
    // error path, writeJSONError's flat `application/json`, could ever
    // satisfy it. So a test whose stated intent was "200 or 404, I
    // don't mind which" in fact passed ONLY while info.json was
    // broken, and it went green for every asset in the catalogue
    // because PoolLookup COALESCEd the never-written pixel dimensions
    // to 0 and BuildInfo rejected them. It failed the moment #757 gave
    // the fields a writer, which is the correct behaviour arriving.
    //
    // Matching a prefix regex rather than adding a second substring
    // check keeps the failure legible AND keeps the real subject —
    // "did this fall through to the SPA?" — in one expression.
    const r = await page.request.get(`/iiif/3/${aid}/info.json`);
    const ct = r.headers()['content-type'] ?? '';
    expect(
      ct,
      `external info.json didn't reach the IIIF handler; got content-type "${ct}" (status ${r.status()}). ` +
        `text/html means the /iiif/3 alias fell through to the SPA — check the nginx rewrite.`,
    ).toMatch(/^application\/(ld\+)?json\b/);
    // CORS is set by the IIIF handler on every response (success or
    // error) — this is what OpenSeadragon needs from a cross-origin
    // embed. Verified even when the specific asset returns 404.
    expect(r.headers()['access-control-allow-origin']).toBe('*');
  });

  // #757 — info.json serves the asset's REAL dimensions.
  //
  // The test above deliberately tolerates a 404, because it is about
  // routing. That tolerance is what let the whole Image API ship
  // non-functional: every asset was 0x0 to IIIF, every info.json 404ed
  // with "asset has no recorded pixel dimensions; run the EXIF
  // extractor", and no test asked for a 200. The error body was naming
  // its own cause the entire time.
  //
  // So this test does ask, and it asks for the property that would have
  // caught it: the dimensions in info.json must be positive AND must
  // equal what the browse payload reports, because "both surfaces read
  // the same recorded pair" is the actual invariant. Asserting merely
  // non-zero would pass on any constant (ADR 0068 — the same rule that
  // made #757's own unit test assert varied ratios rather than
  // non-null fields).
  test('info.json serves real pixel dimensions, not 0x0 (#757)', async ({ page, context, baseURL }) => {
    await loginAsAdminViaUI(page);
    const cookies = (await context.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
    const { asset, scanned } = await firstIIIFServableAsset(baseURL!, cookies);

    // No assets at all is a fresh clone — skip, same convention as the
    // rest of this file. Assets that exist but are none of them
    // IIIF-servable is NOT a skip: that is the #757 state exactly, and
    // skipping into green is how it survived three releases.
    test.skip(scanned === 0, 'no assets in dev DB — upload one first');
    expect(
      asset,
      `scanned ${scanned} asset(s) and none is IIIF-servable — every one is either missing a ` +
        `rendered preview or has no recorded pixel_width/pixel_height. The latter is #757: ` +
        `the preview pipeline records the ladder source's shape, so run \`aa rebuild-previews\`.`,
    ).not.toBeNull();

    const r = await page.request.get(`/iiif/3/${asset!.id}/info.json`);
    expect(
      r.status(),
      `info.json for a previewable asset with recorded dimensions must be 200; ` +
        `a 404 here means BuildInfo rejected the pair (#757).`,
    ).toBe(200);

    // Image API 3.0 §5.1 requires application/ld+json with the profile
    // parameter. This is the exact header the routing test above could
    // not accept.
    expect(r.headers()['content-type']).toMatch(/^application\/ld\+json/);
    expect(r.headers()['access-control-allow-origin']).toBe('*');

    const info = await r.json();
    expect(info['@context']).toBe('http://iiif.io/api/image/3/context.json');
    expect(info.type).toBe('ImageService3');
    expect(info.profile).toBe('level0');

    // The payload the bug destroyed.
    expect(info.width, 'info.json width is 0 — the recorded pair never reached IIIF (#757)')
      .toBeGreaterThan(0);
    expect(info.height).toBeGreaterThan(0);
    expect(
      { width: info.width, height: info.height },
      'info.json and the browse payload disagree about the asset\'s size; both read the ' +
        'same pixel_width/pixel_height field values, so they cannot legitimately differ',
    ).toEqual({ width: asset!.width, height: asset!.height });

    // Level 0 advertises the sizes it can actually serve; an empty list
    // means BuildInfo produced a document no viewer can tile from.
    expect(Array.isArray(info.sizes)).toBe(true);
    expect(info.sizes.length).toBeGreaterThan(0);
  });

  test('Image API 2.0 URL redirects with external /iiif/3/ Location', async ({ page, context, baseURL }) => {
    await loginAsAdminViaUI(page);
    const cookies = (await context.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
    const aid = await firstAssetID(baseURL!, cookies);
    test.skip(!aid, 'no assets in dev DB — upload one first');

    // Legacy 2.0 URL at the EXTERNAL /iiif/2/ path. Redirect target
    // should be the EXTERNAL /iiif/3/... URL (relative Location OK
    // — browsers resolve against the current origin, which is
    // exactly right for third-party viewer compatibility).
    const r = await page.request.get(`/iiif/2/${aid}/info.json`, {
      maxRedirects: 0,
    });
    expect(r.status()).toBe(301);
    const loc = r.headers()['location'];
    expect(loc).toContain(`/iiif/3/${aid}/info.json`);
    // The Location MUST NOT include /api/v1 — that would send the
    // viewer to the internal-only path.
    expect(loc).not.toContain('/api/v1/');
  });
});
