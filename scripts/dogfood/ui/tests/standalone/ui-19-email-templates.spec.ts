// ui-19-email-templates.spec.ts
//
// Admin → Content → Email templates (#795, ADR 0081 §2 as amended).
//
// Drives the real surface: list the emails + their in-scope fields,
// override a subject and see the "changed" badge + a working preview,
// hit the fail-loud 422 by referencing a field the event does not carry
// (the server names it), revert, and check the page at 390px with a
// touch pointer.
//
// The specs clean up after themselves via the API so a re-run starts
// from the shipped templates.

import { test, expect, Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

// Screenshots land where AA_SHOT_DIR points (the workspace root when run
// by the agent), else the cwd.
const SHOT_DIR = process.env.AA_SHOT_DIR ?? '.';

const TEMPLATE = 'admin_test'; // first alphabetically → selected by default
const PART = 'subject';

async function clearOverride(page: Page, template: string, part: string) {
  await page.request.delete(`/api/v1/email-templates/${template}/${part}`);
}

async function openEmailTemplates(page: Page) {
  await page.goto('/admin/email-templates');
  await expect(page.locator(tid('email-templates-page'))).toBeVisible();
  // The fields panel must render — proof the catalogue (shipped + fields)
  // loaded, not just an empty shell.
  await expect(page.locator(tid('email-templates-fields'))).toBeVisible();
}

test.describe('UI-19 email templates', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('lists events, fields, and a shipped body', async ({ page }) => {
    await openEmailTemplates(page);
    // The field list names real view-model fields for admin_test.
    await expect(page.locator(tid('email-templates-fields'))).toContainText('site_name');
    // The shipped subject is shown alongside the editor.
    await expect(page.locator(tid('email-templates-shipped-subject'))).not.toBeEmpty();
    await page.screenshot({ path: `${SHOT_DIR}/email-templates-editor.png`, fullPage: true });
  });

  test('override a subject, preview updates, revert restores', async ({ page }) => {
    await clearOverride(page, TEMPLATE, PART);
    await openEmailTemplates(page);

    const input = page.locator(tid(`email-templates-input-${PART}`));
    await input.fill('OVERRIDDEN — {{.site_name}} test');
    await page.locator(tid(`email-templates-save-${PART}`)).click();
    await expect(page.locator(tid('email-templates-toast'))).toBeVisible();
    await expect(page.locator(tid(`email-templates-changed-${PART}`))).toBeVisible();
    await page.screenshot({ path: `${SHOT_DIR}/email-templates-preview.png`, fullPage: true });

    // Revert brings the shipped subject back (the changed badge clears).
    await page.locator(tid(`email-templates-revert-${PART}`)).click();
    await expect(page.locator(tid('email-templates-toast'))).toBeVisible();
    await expect(page.locator(tid(`email-templates-changed-${PART}`))).toHaveCount(0);
  });

  test('a field the event does not carry is refused, naming it', async ({ page }) => {
    await clearOverride(page, TEMPLATE, PART);
    await openEmailTemplates(page);

    const input = page.locator(tid(`email-templates-input-${PART}`));
    // verify_url is a real field — for register_verify, NOT admin_test.
    await input.fill('Go to {{.verify_url}}');
    await page.locator(tid(`email-templates-save-${PART}`)).click();

    const toast = page.locator(tid('email-templates-toast'));
    await expect(toast).toBeVisible();
    await expect(toast).toContainText('verify_url');
    await page.screenshot({ path: `${SHOT_DIR}/email-templates-422.png`, fullPage: true });
  });

  test('works at 390px with a touch pointer', async ({ browser }) => {
    const ctx = await browser.newContext({
      viewport: { width: 390, height: 844 },
      hasTouch: true,
      isMobile: true,
      locale: 'en-US',
      storageState: '.pw-results/admin-state.json',
      ignoreHTTPSErrors: true,
    });
    const page = await ctx.newPage();
    try {
      await clearOverride(page, TEMPLATE, PART);
      await openEmailTemplates(page);

      const input = page.locator(tid(`email-templates-input-${PART}`));
      await input.tap();
      await input.fill('Mobile — {{.site_name}}');
      await page.locator(tid(`email-templates-save-${PART}`)).tap();
      await expect(page.locator(tid('email-templates-toast'))).toBeVisible();

      // No sideways scroll at 390px.
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
      );
      expect(overflow).toBeLessThanOrEqual(1);
      await page.screenshot({ path: `${SHOT_DIR}/email-templates-390.png`, fullPage: true });

      await page.locator(tid(`email-templates-revert-${PART}`)).tap();
      await expect(page.locator(tid('email-templates-toast'))).toBeVisible();
    } finally {
      await clearOverride(page, TEMPLATE, PART);
      await ctx.close();
    }
  });
});
