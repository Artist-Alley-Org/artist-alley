// vocabulary-extend-cap-789.spec.ts
//
// ADR 0092 §2 — "A user without it sees the same control with the
// create arm absent — never a silent failure."
//
// That sentence is a claim about a rendered control, and it is only
// provable in a browser. The server side is covered in Go
// (metadata/vocabulary_curation_e2e_test.go asserts the 422 and its
// reason); what NOTHING below the browser can see is whether the
// picker a person actually looks at offers to create a term the write
// path is going to refuse. A create row that produces a 422 is worse
// than no create row: it invites the gesture and then punishes it.
//
// So this drives the real upload row, twice, with two real principals:
//
//   1. the bootstrap admin, who holds fields.vocabulary.extend — the
//      create arm is there, exactly as #831 shipped it;
//   2. a Base-role fixture account with the capability REVOKED through
//      the admin API — the same control, the same list, the same
//      filtering, no create arm, and the refusal said out loud.
//
// The revoke is PER USER (`/admin/users/{ref}/revokes`), not a change
// to the Base role: a spec that edited a shipped role would change the
// instance for every other spec in the run, and for the operator whose
// box this is.

import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI, LOGGED_OUT } from '../../helpers/auth';
import { ensureFixtureUser, restoreSelfRegistration } from '../../helpers/fixture-user';
import { tid } from '../../helpers/testids';

const EXTEND_CAP = 'fields.vocabulary.extend';

// A constant account, reused across runs (#1198) — a per-run one is
// never deleted and eventually pushes the bootstrap admin off page 1 of
// /admin/users.
const PROBE_USER = 'vocab789_nonholder';
const PROBE_PASS = 'N0Extend!789vocab';

let probeRef = 0;
let priorSelfRegistration: unknown;
let fieldId = '';
let fieldCode = '';

/** Open the upload modal with one small file queued. */
async function queueOneFile(page: Page, name: string): Promise<void> {
  await page.locator(tid('nav-upload-button')).click();
  await expect(page.getByRole('dialog')).toBeVisible();
  await page.locator(tid('upload-file-input')).setInputFiles({
    name,
    mimeType: 'text/plain',
    buffer: Buffer.from(`vocab789 ${name} ${Date.now()}`),
  });
  await expect(page.getByText(/Ready|Already uploaded/).first()).toBeVisible({ timeout: 30_000 });
}

/**
 * A clip rectangle around the control AND the dropdown it opens
 * beneath, with room to spare. The two screenshots this file takes are
 * the whole point of driving a browser at all — the pair is "same
 * control, create arm present" against "same control, create arm gone"
 * — and a full-page shot of a long upload form shows neither.
 */
async function comboboxClip(
  page: Page,
  code: string,
): Promise<{ x: number; y: number; width: number; height: number }> {
  const box = await page.locator(tid(`vocab-combobox-${code}`)).first().boundingBox();
  if (!box) throw new Error(`combobox ${code} has no box`);
  return {
    x: Math.max(0, box.x - 12),
    y: Math.max(0, box.y - 32),
    width: box.width + 24,
    height: box.height + 320,
  };
}

async function openMetadata(page: Page, code: string): Promise<void> {
  await page.getByText('Metadata', { exact: false }).first().click();
  await expect(page.locator(tid(`vocab-input-${code}`)).first()).toBeVisible({ timeout: 20_000 });
}

test.describe('vocabulary extension is a capability (#789, ADR 0092 §2)', () => {
  test.beforeAll(async ({ browser, request }) => {
    const fixture = await ensureFixtureUser(browser, request, {
      username: PROBE_USER,
      password: PROBE_PASS,
      fullName: 'vocab789 non-holder',
    });
    probeRef = fixture.ref;
    priorSelfRegistration = fixture.priorSelfRegistration;

    // A field of this spec's own, so the assertions do not depend on
    // what `keywords` happens to contain on this instance. It carries
    // an ALIAS, because "an alias resolves for someone who may not
    // create" is the half of the rule most likely to be got wrong —
    // matching is not extension.
    fieldCode = `vocab789_${Date.now()}`;
    const res = await request.post('/api/v1/fields', {
      data: {
        code: fieldCode,
        label: 'Vocab 789 probe',
        type: 'multi_select',
        subject_kind: 'asset',
        applies_to: [1],
        open_vocabulary: true,
        options: {
          values: [
            { value: 'gb', label: 'United Kingdom', aliases: ['uk', 'britain'] },
            { value: 'fr', label: 'France' },
          ],
        },
      },
    });
    expect(res.status(), await res.text()).toBe(201);
    fieldId = String(((await res.json()) as { id: string }).id);

    const revoke = await request.post(`/api/v1/admin/users/${probeRef}/revokes`, {
      data: { capability: EXTEND_CAP },
    });
    expect(revoke.status(), await revoke.text()).toBeLessThan(400);
  });

  test.afterAll(async ({ request }) => {
    if (probeRef) {
      await request
        .delete(`/api/v1/admin/users/${probeRef}/revokes/${EXTEND_CAP}`)
        .catch(() => undefined);
    }
    if (fieldId) {
      await request.delete(`/api/v1/fields/${fieldId}`).catch(() => undefined);
    }
    await restoreSelfRegistration(request, priorSelfRegistration);
  });

  test('a capability HOLDER is offered the create arm', async ({ page }, testInfo) => {
    test.setTimeout(120_000);
    await loginAsAdminViaUI(page);
    await page.goto('/');
    await queueOneFile(page, 'vocab789-holder.txt');
    await openMetadata(page, fieldCode);

    const root = page.locator(tid(`vocab-combobox-${fieldCode}`)).first();
    const input = page.locator(tid(`vocab-input-${fieldCode}`)).first();
    // Scroll the control into view before typing: the dropdown renders
    // directly under the input, and a screenshot of a control below the
    // fold is not evidence of anything.
    await root.scrollIntoViewIfNeeded();
    await input.fill('Prussian Blue');

    const create = page.locator(tid(`vocab-create-${fieldCode}`)).first();
    await expect(create, 'the holder must see the create row').toBeVisible();
    await expect(create).toContainText('prussian-blue');
    await page.screenshot({
      path: testInfo.outputPath('vocab789-holder-create-arm.png'),
      clip: await comboboxClip(page, fieldCode),
    });

    // An alias resolves rather than offering to create a duplicate —
    // the picker mirrors the write path or its preview is a lie.
    await input.fill('britain');
    await expect(
      page.locator(tid(`vocab-create-${fieldCode}`)),
      'an alias must not offer to create a second term',
    ).toHaveCount(0);
    await expect(
      page.locator(`[data-testid="vocab-option-${fieldCode}"][data-value="gb"]`).first(),
    ).toBeVisible();
  });

  test('a NON-holder sees the same control with the create arm absent', async ({
    browser,
  }, testInfo) => {
    test.setTimeout(120_000);
    const ctx = await browser.newContext({ storageState: LOGGED_OUT });
    try {
      const page = await ctx.newPage();
      await page.goto('/login');
      await page.locator(tid('login-username')).fill(PROBE_USER);
      await page.locator(tid('login-password')).fill(PROBE_PASS);
      await page.locator(tid('login-submit')).click();
      await expect(page).toHaveURL(/\/(?:\?|$)/);

      await queueOneFile(page, 'vocab789-nonholder.txt');
      await openMetadata(page, fieldCode);

      const root = page.locator(tid(`vocab-combobox-${fieldCode}`)).first();
      const input = page.locator(tid(`vocab-input-${fieldCode}`)).first();
      await root.scrollIntoViewIfNeeded();

      // THE SAME CONTROL: the existing terms still filter as you type.
      await input.fill('Fran');
      await expect(
        page.locator(`[data-testid="vocab-option-${fieldCode}"][data-value="fr"]`).first(),
        'a non-holder must still be able to PICK — extension is not membership',
      ).toBeVisible();

      // …and an alias still resolves for them. Matching is not
      // extending, and a redirect an operator configured must not
      // require the capability to create.
      await input.fill('britain');
      await expect(
        page.locator(`[data-testid="vocab-option-${fieldCode}"][data-value="gb"]`).first(),
      ).toBeVisible();

      // THE CREATE ARM IS ABSENT, and the reason is said out loud
      // rather than shown as an empty list.
      await input.fill('Prussian Blue');
      await expect(
        page.locator(tid(`vocab-create-${fieldCode}`)),
        'a caller without fields.vocabulary.extend must not be offered a create row',
      ).toHaveCount(0);
      // …and it names the right refusal. The field DOES accept new
      // terms; this person may not add them. Reusing the closed-field
      // sentence here would tell them their instance works in a way it
      // does not.
      const blocked = page.locator(tid(`vocab-blocked-${fieldCode}`)).first();
      await expect(blocked).toBeVisible();
      await expect(blocked).toContainText(/you can't add new ones here/i);
      await page.screenshot({
        path: testInfo.outputPath('vocab789-nonholder-no-create-arm.png'),
        clip: await comboboxClip(page, fieldCode),
      });
    } finally {
      await ctx.close();
    }
  });
});
