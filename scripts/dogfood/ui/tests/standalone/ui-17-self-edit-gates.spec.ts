// ui-17-self-edit-gates.spec.ts
//
// Phase 1.17.F — Per-field self-edit operator gates.
//
// Verifies the full operator → user round trip:
//   1. Admin opens /admin/system/users (the new operator surface)
//      and sees all 5 gate checkboxes.
//   2. Admin unticks `bio`, saves, the page re-renders without
//      error.
//   3. The bio textarea on /account/profile is now disabled and
//      shows the "Locked by operator" hint, while the other
//      inputs remain enabled.
//   4. Re-ticking `bio` and saving restores edit access on the
//      profile page — gates the operator just turned back on are
//      reflected immediately on next page load.
//
// The admin user is also the self-edit subject: the gates page is
// global, so the operator's own profile gets locked at the same
// time. That's intentional — there's no per-user override.
//
// Test resets the gates back to all-on at the end so subsequent
// dogfood runs don't see a half-locked profile.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

test.describe('UI-17 self-edit operator gates', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('admin gates page renders all five toggles', async ({ page }) => {
    await page.goto('/admin/system/users');
    for (const key of ['display_name', 'bio', 'avatar_url', 'location', 'website_url']) {
      await expect(page.getByTestId(`self-edit-gate-${key}`)).toBeVisible();
    }
  });

  test('toggling bio off disables the textarea on profile + restores on toggle back on', async ({ page }) => {
    // 1. Open admin gates page; capture the bio checkbox.
    await page.goto('/admin/system/users');
    const bioCheckbox = page.getByTestId('self-edit-gate-bio');
    await expect(bioCheckbox).toBeVisible();

    // 2. If bio is currently ticked, untick + save (assumed start state).
    if (await bioCheckbox.isChecked()) {
      await bioCheckbox.uncheck();
    }
    await page.getByRole('button', { name: /save gates/i }).click();
    await expect(page.getByTestId('self-edit-gates-saved')).toBeVisible({ timeout: 5_000 });

    // 3. Open /account/profile. Bio is disabled, others still enabled.
    await page.goto('/account/profile');
    const bioInput = page.getByTestId('profile-bio');
    await expect(bioInput).toBeDisabled();
    await expect(page.getByTestId('profile-bio-locked')).toBeVisible();
    await expect(page.getByTestId('profile-display-name')).toBeEnabled();

    // 4. Re-tick bio + save. Reload profile — bio is editable again.
    await page.goto('/admin/system/users');
    const bioCheckboxRound2 = page.getByTestId('self-edit-gate-bio');
    await bioCheckboxRound2.check();
    await page.getByRole('button', { name: /save gates/i }).click();
    await expect(page.getByTestId('self-edit-gates-saved')).toBeVisible({ timeout: 5_000 });

    await page.goto('/account/profile');
    await expect(page.getByTestId('profile-bio')).toBeEnabled();
  });
});
