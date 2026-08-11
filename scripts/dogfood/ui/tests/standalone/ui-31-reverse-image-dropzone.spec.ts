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
// Test-env note: the standalone dogfood compose stack does NOT run the
// CLIP visual-encoder sidecar, so POST /search/by-image returns 501
// sidecar_not_installed (or 404 when the search service is disabled
// entirely). The happy-path results grid therefore can't be exercised
// in CI — that needs the sidecar. This spec verifies the render + the
// interaction wiring + that a submit resolves to a HANDLED state (the
// component surfaces the not-configured / error path instead of
// hanging or crashing). The full upload→results flow is covered by
// manual verification against a sidecar-enabled instance (see PR body).

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

// A 1x1 red PNG — smallest valid image the picker + endpoint accept.
const PNG_1x1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==',
  'base64',
);

test.describe('UI-31 reverse-image dropzone', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('dropzone renders above the DSL builder in the advanced panel', async ({ page }) => {
    await page.goto('/search?advanced=1');
    await expect(page.getByTestId('reverse-image-dropzone')).toBeVisible();
    await expect(page.getByTestId('reverse-image-drop')).toBeVisible();
    // Submit is disabled until an image is selected.
    await expect(page.getByTestId('reverse-image-submit')).toBeDisabled();
    // The DSL builder still renders below it (dropzone is additive).
    await expect(page.getByTestId('advanced-rows')).toBeVisible();
  });

  test('selecting an image shows a preview + enables submit', async ({ page }) => {
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

  test('submitting resolves to a handled state (results or not-configured/error)', async ({ page }) => {
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
