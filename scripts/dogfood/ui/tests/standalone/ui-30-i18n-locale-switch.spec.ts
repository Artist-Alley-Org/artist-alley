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
//
// ── #1017: the profile this spec writes is the whole suite's ─────────
//
// `lang.set()` PATCHes the SHARED ADMIN PROFILE, and every spec in this
// suite signs in as that admin. So while this file is in Spanish, any
// spec on the other worker asserting English chrome is reading a
// preference it did not ask for — which is what `--workers 2` did to
// `locale choice persists across a reload`, and why it passed in
// isolation every time anyone looked.
//
// #535's `mode: 'serial'` was already here when #1017 was filed, and it
// could not have fixed it: serial orders the tests inside THIS describe
// block and says nothing about the other worker, which is where the
// reader is. The exclusion has to be over the resource and across files,
// so the borrow now goes through `adminProfileHold` — the same
// cross-file lock `system.public_mode` uses, on a second resource name.
//
// Serial stays, for a different and smaller reason: both tests share one
// hold, and one worker running them in order is what makes a single
// acquire in `beforeAll` correct.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { adminProfileHold } from '../../helpers/admin-profile';
import { tid } from '../../helpers/testids';

const EN_SEARCH_PLACEHOLDER = 'Search assets…';
const ES_SEARCH_PLACEHOLDER = 'Buscar recursos…';

const profile = adminProfileHold('ui-30-i18n-locale-switch');

test.describe('UI-30 i18n locale switch', () => {
  test.describe.configure({ mode: 'serial' });

  test.beforeAll(async ({ request }) => {
    await profile.acquire(request);
  });

  test.afterAll(async ({ request }) => {
    await profile.release(request);
  });

  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
    // ARRANGED, not hoped for. Both tests below open by asserting the
    // English chrome, and until now that was a bet on what the instance
    // happened to be holding — a bet the previous test in this very file
    // loses, since it ends in Spanish. Written directly rather than
    // through the control because this is the arrangement, not the
    // switch under test, and it is inside the hold so no other worker
    // can observe the window.
    await profile.setLanguageDirect(page.request, 'en');
  });

  test('switching locale to Spanish re-renders translated navbar chrome', async ({ page }) => {
    await page.goto('/account/preferences');

    // Default locale renders English chrome.
    const searchbox = page.locator(tid('nav-search'));
    await expect(searchbox).toHaveAttribute('placeholder', EN_SEARCH_PLACEHOLDER);

    // Flip to Spanish via the real preference control. The endonym
    // "Español" is stable across locales.
    await profile.setLanguage(page, 'Español');

    // The navbar placeholder must flip without a reload — the store
    // is reactive and `t()` re-runs on `lang.resolved` change.
    await expect(searchbox).toHaveAttribute('placeholder', ES_SEARCH_PLACEHOLDER);
  });

  test('locale choice persists across a reload (aa_lang cookie)', async ({ page }) => {
    await page.goto('/account/preferences');
    await profile.setLanguage(page, 'Español');

    const searchbox = page.locator(tid('nav-search'));
    await expect(searchbox).toHaveAttribute('placeholder', ES_SEARCH_PLACEHOLDER);

    // Reload: the cookie set by lang.set() must re-resolve to es on
    // the fresh paint.
    await page.reload();
    await expect(page.locator(tid('nav-search'))).toHaveAttribute(
      'placeholder',
      ES_SEARCH_PLACEHOLDER,
    );

    // Switching BACK is its own assertion, not only tidying up — the
    // round trip is what proves the control is a switch rather than a
    // one-way door. (The tidying is `afterAll`'s job, and it runs even
    // when this test has already failed.) Click the "English" button
    // specifically: its endonym never translates, and unlike
    // "System"/"Sistema" it is unique to the language picker, so the
    // selector cannot collide with the theme row.
    await page.goto('/account/preferences');
    await profile.setLanguage(page, 'English');
    await expect(page.locator(tid('nav-search'))).toHaveAttribute(
      'placeholder',
      EN_SEARCH_PLACEHOLDER,
    );
  });

  // The refusal, pinned. `public-mode.ts` has had the same guard since
  // #1248 and nothing ever asserted it fires, so "the lock is taken"
  // rested on every caller remembering. A hold that never acquired must
  // not be able to touch the profile, and the failure must name the lock
  // rather than surface three files away as somebody else's flake.
  //
  // Nothing is mutated: the guard throws before the control is clicked,
  // which is the property being asserted.
  test('a profile change without the hold fails loudly', async ({ page }) => {
    const stray = adminProfileHold('ui-30 unheld probe');
    expect(stray.holding, 'the probe must not hold the lock').toBe(false);
    await expect(stray.setLanguage(page, 'Español')).rejects.toThrow(
      /without holding the "user\.admin_profile" lock/,
    );
    await expect(stray.setLanguageDirect(page.request, 'es')).rejects.toThrow(
      /without holding the "user\.admin_profile" lock/,
    );
  });
});
