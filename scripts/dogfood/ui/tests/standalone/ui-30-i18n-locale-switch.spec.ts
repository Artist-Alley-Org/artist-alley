// ui-30-i18n-locale-switch.spec.ts
//
// Proves the locale-switch mechanism works end-to-end at the
// visible-DOM level — the gap the 1.55.V-1 audit flagged as MUST
// (zero prior specs switched locale + asserted translated text).
//
// Approach — assert on an EXISTING Spanish key, not a sentinel
// locale. The navbar search input's placeholder is `t('nav.search_
// placeholder')`, which has both en ("Search assets…") and es
// ("Buscar recursos…") values in the shipped catalogues. It's
// persistent chrome present on every route (per the "navbar search
// always visible" invariant) and carries a stable `data-testid`, so
// it's the cleanest mechanical proof that switching locale re-renders
// translated text. Most new V-2 keys are en-only (es/fr fall through
// to English per #247), so we deliberately pick a key that DOES have
// Spanish today — the spec proves the SWITCH, not es/fr coverage.
//
// The language buttons on /account/preferences render each locale's
// endonym (`l.nativeName`, e.g. "Español") which is never itself
// translated, so selecting by that text is locale-stable.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

const EN_SEARCH_PLACEHOLDER = 'Search assets…';
const ES_SEARCH_PLACEHOLDER = 'Buscar recursos…';

test.describe('UI-30 i18n locale switch', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('switching locale to Spanish re-renders translated navbar chrome', async ({ page }) => {
    await page.goto('/account/preferences');

    // Default locale renders English chrome.
    const searchbox = page.locator(tid('nav-search'));
    await expect(searchbox).toHaveAttribute('placeholder', EN_SEARCH_PLACEHOLDER);

    // Flip to Spanish via the real preference control. The endonym
    // "Español" is stable across locales.
    await page.getByRole('button', { name: /Español/ }).click();

    // The navbar placeholder must flip without a reload — the store
    // is reactive and `t()` re-runs on `lang.resolved` change.
    await expect(searchbox).toHaveAttribute('placeholder', ES_SEARCH_PLACEHOLDER);
  });

  test('locale choice persists across a reload (aa_lang cookie)', async ({ page }) => {
    await page.goto('/account/preferences');
    await page.getByRole('button', { name: /Español/ }).click();

    const searchbox = page.locator(tid('nav-search'));
    await expect(searchbox).toHaveAttribute('placeholder', ES_SEARCH_PLACEHOLDER);

    // Reload: the cookie set by lang.set() must re-resolve to es on
    // the fresh paint.
    await page.reload();
    await expect(page.locator(tid('nav-search'))).toHaveAttribute(
      'placeholder',
      ES_SEARCH_PLACEHOLDER,
    );

    // Reset to system default so the shared admin session doesn't
    // leak a Spanish preference into sibling specs.
    await page.goto('/account/preferences');
    await page.getByRole('button', { name: /System|Sistema/ }).first().click();
    await expect(page.locator(tid('nav-search'))).toHaveAttribute(
      'placeholder',
      EN_SEARCH_PLACEHOLDER,
    );
  });
});
