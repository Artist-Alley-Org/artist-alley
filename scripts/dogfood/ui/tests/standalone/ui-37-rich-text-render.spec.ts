// ui-37-rich-text-render.spec.ts
//
// #816 — a rich_text field renders as formatted markup on the post
// metadata panel, and a hostile value cannot execute.
//
// This is the app's FIRST {@html} surface, so the browser half of the
// proof is not optional. The Go tests
// (app/internal/metadata/rich_text_value_e2e_test.go) pin what the API
// returns; a string assertion on an API response cannot tell you
// whether a browser handed that string to a parser would run anything.
// Only a browser can, so:
//
//   1. `window.__xss` is installed BEFORE the page scripts run, via
//      addInitScript, and records every call.
//   2. A value carrying <script>, onerror= and a javascript: href is
//      written through the real API and rendered on the real panel.
//   3. The canary must still be un-fired, AND the formatting must have
//      survived — a sanitiser that deleted everything would satisfy a
//      test that only checked the canary.
//
// Naming is deliberate: removing the sanitiser fails
// `neutralises a hostile rich_text value` and
// `renders formatted rich_text on the post metadata panel` by name.
//
// Fields are created per attempt and deleted in afterEach (DELETE
// soft-archives and `code` is UNIQUE — see ui-18's note).

import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

/** The formatting a reader is supposed to get. */
const FORMATTED =
  '<p>Cleared for <strong>internal</strong> review use only.</p>' +
  '<ul><li>No resale</li><li>Credit <em>Aurora R&amp;D</em></li></ul>' +
  '<p>Full terms: <a href="https://example.test/licence">licence</a></p>';

/**
 * The canary payload. Three shapes, because they fail differently:
 * a script element (parser-level), an inline handler (attribute-level),
 * and a javascript: href (scheme-level). Each calls window.__xss with
 * its own tag so a failure names which one got through.
 */
const HOSTILE =
  '<p>Cleared for <strong>internal</strong> use.</p>' +
  '<script>window.__xss && window.__xss("script")</script>' +
  '<img src=x onerror="window.__xss && window.__xss(\'onerror\')">' +
  '<a href="javascript:window.__xss(\'href\')">terms</a>' +
  '<p>Full terms: <a href="https://example.test/licence">licence</a></p>';

let createdFieldIds: string[] = [];

function fieldCode(prefix: string, testInfo: { workerIndex: number; retry: number }): string {
  return `ui37_${prefix}_${testInfo.workerIndex}_${testInfo.retry}_${Date.now()}`;
}

async function createRichTextField(
  page: Page,
  code: string,
  label: string,
): Promise<{ id: string; code: string }> {
  const res = await page.request.post('/api/v1/fields', {
    data: { code, label, type: 'rich_text', subject_kind: 'asset' },
  });
  expect(res.status(), await res.text()).toBe(201);
  const f = (await res.json()) as { id: string; code: string };
  createdFieldIds.push(f.id);
  return f;
}

/**
 * The first post that has an asset, and the asset the viewer will
 * actually be SHOWING when the page opens.
 *
 * That second part matters: the metadata panel renders the fields of
 * the asset under the playlist cursor, and postSource starts on the
 * cover member when one is pinned and on member 0 otherwise. Writing
 * the value onto any other member would render a panel that is
 * correctly empty, and the test would fail for a reason that has
 * nothing to do with sanitisation.
 */
async function firstPostWithAsset(page: Page): Promise<{ postId: string; assetId: string }> {
  const list = await page.request.get('/api/v1/posts?limit=10');
  expect(list.ok(), await list.text()).toBe(true);
  const items = ((await list.json()).items ?? []) as { id: string }[];
  expect(items.length, 'no posts on this instance — seed it first').toBeGreaterThan(0);

  for (const p of items) {
    const detail = await page.request.get(`/api/v1/posts/${p.id}`);
    if (!detail.ok()) continue;
    const body = (await detail.json()) as {
      members?: { asset_id?: string }[];
      cover_asset_id?: string | null;
    };
    const members = (body.members ?? []).map((m) => m.asset_id).filter(Boolean) as string[];
    if (members.length === 0) continue;
    const cover = body.cover_asset_id;
    return { postId: p.id, assetId: cover && members.includes(cover) ? cover : members[0] };
  }
  throw new Error('no post with an asset found in the first 10 posts');
}

async function setValue(page: Page, assetId: string, fieldId: string, valueText: string) {
  const res = await page.request.put(`/api/v1/assets/${assetId}/fields/${fieldId}`, {
    data: { value_text: valueText },
  });
  expect(res.ok(), await res.text()).toBe(true);
}

/**
 * Open the post page and expand the collapsed metadata section.
 *
 * The section is a native `<details>` that ships closed, and it only
 * exists once the asset's field values have arrived — so a single
 * click fired on arrival can land on a summary that is re-rendered a
 * moment later and toggle nothing. Retry until the element reports
 * `open`, rather than clicking once and hoping: the first version of
 * this helper passed at 1080p and failed at 390px for exactly that
 * reason, which is a flake, not a finding.
 */
async function openMetadataPanel(page: Page, postId: string, fieldCode: string) {
  await page.goto(`/posts/${postId}`);
  const details = page
    .locator('details.aa-collapse')
    .filter({ has: page.getByTestId('post-metadata') });
  await expect(details).toBeVisible();

  const isOpen = () => details.evaluate((el) => (el as HTMLDetailsElement).open);
  await expect(async () => {
    if (!(await isOpen())) await details.locator('summary').click();
    expect(await isOpen()).toBe(true);
  }).toPass({ timeout: 15_000 });

  const dd = page.getByTestId(`post-field-${fieldCode}`);
  await expect(dd).toBeVisible();
  return dd;
}

test.describe('UI-37 rich_text renders as markup', () => {
  test.beforeEach(async ({ page }) => {
    createdFieldIds = [];
    // The canary must exist before ANY page script runs, or a payload
    // that fires on parse would find no function to call and the test
    // would pass because the trap was late rather than because the
    // sanitiser worked.
    await page.addInitScript(() => {
      (window as unknown as { __xssFired: string[] }).__xssFired = [];
      (window as unknown as { __xss: (w: string) => void }).__xss = (where: string) => {
        (window as unknown as { __xssFired: string[] }).__xssFired.push(where);
      };
    });
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ request }) => {
    for (const id of createdFieldIds) {
      await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
    }
    createdFieldIds = [];
  });

  test('renders formatted rich_text on the post metadata panel', async ({ page }, testInfo) => {
    const f = await createRichTextField(page, fieldCode('fmt', testInfo), 'UI-37 formatted');
    const { postId, assetId } = await firstPostWithAsset(page);
    await setValue(page, assetId, f.id, FORMATTED);

    const dd = await openMetadataPanel(page, postId, f.code);

    // The bug: the tags used to arrive as visible characters.
    await expect(dd).not.toContainText('<p>');
    await expect(dd).not.toContainText('<strong>');

    // The fix: they arrive as ELEMENTS. Locating them inside the dd is
    // the assertion — text alone would pass on the escaped rendering.
    await expect(dd.locator('strong', { hasText: 'internal' })).toBeVisible();
    await expect(dd.locator('em', { hasText: 'Aurora R&D' })).toBeVisible();
    await expect(dd.locator('li')).toHaveCount(2);
    await expect(dd.locator('p').first()).toBeVisible();

    // The link survives with the rel the policy forces on it.
    const link = dd.locator('a[href="https://example.test/licence"]');
    await expect(link).toBeVisible();
    await expect(link).toHaveAttribute('rel', 'noopener noreferrer');

    await dd.scrollIntoViewIfNeeded();
    await page.screenshot({
      path: testInfo.outputPath('rich-text-formatted-1080p.png'),
      fullPage: true,
    });
    // Element-scoped too: the metadata panel scrolls independently, so
    // a page shot can crop out the very row the test is about.
    await dd.screenshot({ path: testInfo.outputPath('rich-text-formatted-value.png') });
  });

  test('neutralises a hostile rich_text value', async ({ page }, testInfo) => {
    const f = await createRichTextField(page, fieldCode('xss', testInfo), 'UI-37 hostile');
    const { postId, assetId } = await firstPostWithAsset(page);
    await setValue(page, assetId, f.id, HOSTILE);

    const dd = await openMetadataPanel(page, postId, f.code);

    // Give an onerror/async payload a beat to fire if it is going to.
    await page.waitForTimeout(500);

    const fired = await page.evaluate(
      () => (window as unknown as { __xssFired: string[] }).__xssFired,
    );
    expect(fired, `XSS canary fired from: ${fired?.join(', ')}`).toEqual([]);

    // ── The control ─────────────────────────────────────────────────
    // An un-fired canary proves nothing unless the canary CAN fire.
    // The same payload, injected the same way the renderer injects it
    // (innerHTML) but without going through the sanitiser, must trip
    // it — otherwise the assertion above is a trap that was never
    // armed and would pass with the sanitiser deleted. Same reasoning
    // as assertTimezoneIsLive in web/src/lib/fieldDisplay.test.ts.
    const control = await page.evaluate(async () => {
      const w = window as unknown as { __xssFired: string[] };
      const probe = document.createElement('div');
      probe.style.display = 'none';
      probe.innerHTML = '<img src=x onerror="window.__xss(\'control\')">';
      document.body.appendChild(probe);
      await new Promise((r) => setTimeout(r, 500));
      probe.remove();
      const hit = w.__xssFired.includes('control');
      w.__xssFired = [];
      return hit;
    });
    expect(control, 'the XSS canary never fires — this test proves nothing').toBe(true);

    // Nothing hostile survived into the DOM either — a canary alone
    // would pass on a payload that merely failed to fire this time.
    await expect(dd.locator('script')).toHaveCount(0);
    await expect(dd.locator('img')).toHaveCount(0);
    await expect(dd.locator('a[href^="javascript:"]')).toHaveCount(0);
    await expect(dd.locator('[onerror]')).toHaveCount(0);

    // And the sanitiser did not simply empty the field: the legitimate
    // formatting and the legitimate link are still there.
    await expect(dd.locator('strong', { hasText: 'internal' })).toBeVisible();
    await expect(dd.locator('a[href="https://example.test/licence"]')).toBeVisible();

    await dd.scrollIntoViewIfNeeded();
    await page.screenshot({
      path: testInfo.outputPath('rich-text-hostile-neutralised.png'),
      fullPage: true,
    });
    await dd.screenshot({ path: testInfo.outputPath('rich-text-hostile-value.png') });
  });
});

// The same panel on a phone. A metadata value that renders as markup
// has block elements, margins and a list — none of which the escaped
// rendering had — so it is a new chance to blow out the sidebar's
// width (#376). Real width, real coarse pointer.
test.describe('UI-37 rich_text at 390px', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true });

  test.beforeEach(async ({ page }) => {
    createdFieldIds = [];
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ request }) => {
    for (const id of createdFieldIds) {
      await request.delete(`/api/v1/fields/${id}`).catch(() => undefined);
    }
    createdFieldIds = [];
  });

  test('formats without overflowing the panel', async ({ page }, testInfo) => {
    const f = await createRichTextField(page, fieldCode('mob', testInfo), 'UI-37 formatted');
    const { postId, assetId } = await firstPostWithAsset(page);
    await setValue(page, assetId, f.id, FORMATTED);

    const dd = await openMetadataPanel(page, postId, f.code);
    await expect(dd.locator('strong', { hasText: 'internal' })).toBeVisible();
    await expect(dd.locator('li')).toHaveCount(2);

    // The page itself must not scroll sideways.
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, 'the formatted value pushed the page wider than the viewport').toBeLessThanOrEqual(1);

    await dd.scrollIntoViewIfNeeded();
    await page.screenshot({
      path: testInfo.outputPath('rich-text-formatted-390.png'),
      fullPage: true,
    });
  });
});
