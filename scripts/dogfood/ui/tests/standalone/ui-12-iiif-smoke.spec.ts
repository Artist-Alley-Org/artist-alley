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

import { test, expect, request } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

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

test.describe('UI-12 IIIF Presentation + Content Search smoke', () => {
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

  test('asset manifest is valid IIIF 3.0 JSON with a canvas', async ({ page, context, baseURL }) => {
    await loginAsAdminViaUI(page);
    const cookies = (await context.cookies()).map((c) => `${c.name}=${c.value}`).join('; ');
    const aid = await firstAssetID(baseURL!, cookies);
    test.skip(!aid, 'no assets in dev DB — upload one first');

    const r = await page.request.get(`/api/v1/iiif/3/asset/${aid}/manifest.json`);
    expect(r.ok()).toBe(true);
    const manifest = await r.json();

    const ctxField = manifest['@context'];
    const contexts = Array.isArray(ctxField) ? ctxField : [ctxField];
    expect(contexts).toContain('http://iiif.io/api/presentation/3/context.json');
    expect(manifest.type).toBe('Manifest');
    expect(manifest.id).toContain(`/iiif/3/asset/${aid}/manifest.json`);
    expect(manifest.label).toBeTruthy();
    // A non-embargoed asset has items[] with at least one Canvas.
    // Embargoed asset (stub) has no items[]; skip the canvas
    // assertion for that case since we can't guarantee the ambient
    // DB has a non-embargoed asset.
    if (Array.isArray(manifest.items) && manifest.items.length > 0) {
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
    }
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

    // Image API 3.0 info.json at the EXTERNAL /iiif/3/ path.
    // 1.54.A's iiif.read capability is anonymous-seeded, but the
    // Playwright fixture is authenticated for consistency with
    // the other tests.
    const r = await page.request.get(`/iiif/3/${aid}/info.json`);
    expect(r.ok(), `external info.json fetch failed: ${r.status()} — nginx rewrite regression?`).toBe(true);
    const info = await r.json();
    expect(info['@context']).toBe('http://iiif.io/api/image/3/context.json');
    // Same CORS assertion — this is what OpenSeadragon needs.
    expect(r.headers()['access-control-allow-origin']).toBe('*');
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
