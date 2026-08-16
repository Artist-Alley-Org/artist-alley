// ui-31-reverse-image-dropzone.spec.ts
//
// Reverse-image search dropzone (Phase 1.55.W).
//
// It used to live on /search/advanced. #850 folded that route into
// /search as a slide-over panel — the builder is a way of composing the
// same query, not a separate destination — so the dropzone is now
// reached by opening that panel. `?advanced=1` is the deep link the
// page reads on mount, which keeps this spec a navigation rather than a
// click sequence.
//
// THE ARM IS NOW CONDITIONAL (#1163), and that is what this spec leads
// with. `visual_search_enabled` on the public /appearance boot payload
// says whether this install can answer a reverse-image search at all —
// resolved, i.e. `search.visual.enabled` AND a CLIP sidecar that
// answered at boot. False ⇒ the section is absent, rather than present
// until someone drops an image and reads a 501 back.
//
// The dogfood stack runs no sidecar, so its real answer is FALSE and the
// absent case is the one that needs no help. The interaction tests
// therefore intercept /appearance and flip the flag on, which is the
// only honest way to reach the widget here — and their submit still
// lands on the 501 path, which pins the second channel (a sidecar that
// goes away after boot) at the same time. The full upload→results flow
// still needs a sidecar-enabled instance.

import { type Page } from '@playwright/test';
import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

// A 1x1 red PNG — smallest valid image the picker + endpoint accept.
const PNG_1x1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==',
  'base64',
);

/** Serve the boot payload this install would send if it HAD the CLIP
 *  channel. Intercepted before the navigation, so the appearance store's
 *  boot fetch is what receives it. */
async function withVisualSearch(page: Page, enabled: boolean): Promise<void> {
  await page.route('**/api/v1/appearance', async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.visual_search_enabled = enabled;
    await route.fulfill({ response: res, json: body });
  });
}

test.describe('UI-31 reverse-image dropzone', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('the arm is ABSENT when the instance has no visual channel', async ({ page }) => {
    // No interception — this is the stack's own answer, and the stack
    // runs no sidecar.
    await page.goto('/search?advanced=1');
    // The panel itself is up: the assertion is about the arm, not about
    // a page that failed to render.
    await expect(page.getByTestId('advanced-rows')).toBeVisible();
    await expect(
      page.getByTestId('reverse-image-dropzone'),
      'the reverse-image arm rendered on an install whose /search/by-image ' +
        'answers 501 — #1163: the flag is on the boot payload, so this ' +
        'section should not exist here',
    ).toHaveCount(0);
  });

  test('dropzone renders above the DSL builder when the channel is on', async ({ page }) => {
    await withVisualSearch(page, true);
    await page.goto('/search?advanced=1');
    await expect(page.getByTestId('reverse-image-dropzone')).toBeVisible();
    await expect(page.getByTestId('reverse-image-drop')).toBeVisible();
    // Submit is disabled until an image is selected.
    await expect(page.getByTestId('reverse-image-submit')).toBeDisabled();
    // The DSL builder still renders below it (dropzone is additive).
    await expect(page.getByTestId('advanced-rows')).toBeVisible();
  });

  test('selecting an image shows a preview + enables submit', async ({ page }) => {
    await withVisualSearch(page, true);
    await page.goto('/search?advanced=1');
    await page.getByTestId('reverse-image-file').setInputFiles({
      name: 'test.png',
      mimeType: 'image/png',
      buffer: PNG_1x1,
    });
    // Preview thumbnail + filename render; submit becomes enabled.
    await expect(page.getByAltText('Selected image preview')).toBeVisible();
    await expect(page.getByText('test.png')).toBeVisible();
    await expect(page.getByTestId('reverse-image-submit')).toBeEnabled();
  });

  test('submitting resolves to a handled state (results or not-configured/error)', async ({
    page,
  }) => {
    // The flag says yes and the endpoint says 501 — the exact split the
    // boot flag cannot cover, so the component's own handling has to.
    await withVisualSearch(page, true);
    await page.goto('/search?advanced=1');
    await page.getByTestId('reverse-image-file').setInputFiles({
      name: 'test.png',
      mimeType: 'image/png',
      buffer: PNG_1x1,
    });
    await page.getByTestId('reverse-image-submit').click();

    // Whatever the server returns, the component must land on a handled
    // state — never hang on the loading label. In CI (no sidecar) this
    // is the not-configured or error path; with a sidecar it'd be the
    // results grid.
    const results = page.getByTestId('reverse-image-results');
    const notConfigured = page.getByTestId('reverse-image-not-configured');
    const error = page.getByTestId('reverse-image-error');
    await expect(results.or(notConfigured).or(error)).toBeVisible({ timeout: 10_000 });
  });
});
