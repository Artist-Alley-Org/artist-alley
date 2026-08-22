// #1240 — the upload modal's visibility control agrees with the request
// it produces, and with /create.
//
// # What was wrong
//
// The modal's <select> offered three tiers — public, followers, private
// — and was bound to `upload.compose.visibility`, which the store
// defaults to `org-only`. A <select> bound to a value none of its
// options carry renders BLANK. So the control showed no tier, the
// artist had nothing to read, and submitting untouched posted
// `org-only` regardless: the form reported one thing and did another,
// on the field that decides who can see the work.
//
// # Why these three assertions
//
// A fix that only ADDS the missing option would satisfy "not blank" and
// still leave two create surfaces describing the same four tiers in two
// orders with two sets of words. So:
//
//  1. THE OPTION LIST, in order, against /create's — the acceptance the
//     issue itself states.
//  2. THE RENDERED DEFAULT — `org-only` selected, not blank. This is the
//     assertion that FAILS on the old three-option select, and it fails
//     for the original reason: a bound value with no matching option
//     leaves `select.value` empty.
//  3. THE POSTED TIER EQUALS THE SHOWN TIER, read off the live DOM and
//     compared to the real request body, with the control untouched.
//     Asserting `visibility === 'org-only'` alone would pass on a form
//     that displayed something else entirely — the bug was never the
//     value, it was the disagreement.

import { test, expect, type Page } from '../../helpers/test';
import { tid } from '../../helpers/testids';

// /create's select is the reference. Order matters: `org-only` is first
// because it is the default, and a default that renders honestly has to
// be an option.
const TIERS = ['org-only', 'followers', 'private', 'public'];

/** Option values, in DOM order, from a <select>. */
function optionValues(page: Page, testid: string) {
  return page
    .locator(`${testid} option`)
    .evaluateAll((els) => els.map((el) => (el as HTMLOptionElement).value));
}

/** What the control is SHOWING — its selected option's value. */
function shownValue(page: Page, testid: string) {
  return page.locator(testid).evaluate((el) => (el as HTMLSelectElement).value);
}

const createdPosts: string[] = [];
const createdAssets: string[] = [];

test.afterEach(async ({ page }) => {
  // Leave the corpus as we found it — a spec that adds a post to the
  // top of a newest-first feed changes what the next spec sees (#1198).
  for (const id of createdPosts.splice(0)) {
    await page.request.delete(`/api/v1/posts/${id}`).catch(() => undefined);
  }
  for (const id of createdAssets.splice(0)) {
    await page.request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
  }
});

test.describe('#1240 the upload modal reports the tier it posts', () => {
  test('the modal offers /create’s four tiers, in /create’s order', async ({ page }) => {
    await page.goto('/create');
    await expect(page.locator(tid('create-visibility'))).toBeVisible();
    const reference = await optionValues(page, tid('create-visibility'));
    expect(
      reference,
      'the reference surface itself changed — update this spec deliberately, not by copying',
    ).toEqual(TIERS);

    await page.goto('/');
    await page.locator(tid('nav-upload-button')).click();
    await expect(page.getByRole('dialog')).toBeVisible();
    await expect(page.locator(tid('upload-visibility'))).toBeVisible();

    expect(await optionValues(page, tid('upload-visibility'))).toEqual(reference);

    // The help line /create carries. Without it the tier names are the
    // only explanation of a decision the artist is making once.
    await expect(
      page.locator(tid('upload-visibility')).locator('xpath=following-sibling::span[1]'),
    ).toHaveText(/hidden by default/i);
  });

  test('a fresh modal SHOWS the default tier rather than nothing', async ({ page }) => {
    await page.goto('/');
    await page.locator(tid('nav-upload-button')).click();
    await expect(page.getByRole('dialog')).toBeVisible();

    // The load-bearing case. On the pre-#1240 three-option select this
    // is '' — the browser has no option to select for a bound value the
    // list does not contain, and the control renders blank.
    expect(
      await shownValue(page, tid('upload-visibility')),
      'the control is showing no tier at all; the store default is not among its options',
    ).toBe('org-only');
  });

  test('the tier it posts is the tier it showed, untouched', async ({ page }) => {
    await page.goto('/');
    await page.locator(tid('nav-upload-button')).click();
    await expect(page.getByRole('dialog')).toBeVisible();

    await page.locator(tid('upload-file-input')).setInputFiles({
      name: 'ui-1240.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from(`1240 visibility fixture ${Date.now()}-${Math.random()}`),
    });
    await expect(page.locator(tid('upload-visibility'))).toBeVisible();

    // Read what the artist can see, BEFORE submitting and without
    // touching the control. This is the value the assertion below
    // compares against — not a constant, so the two cannot agree by
    // both being written wrong.
    const shown = await shownValue(page, tid('upload-visibility'));

    const postRequest = page.waitForRequest(
      (r) => r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/posts',
    );
    const postResponse = page.waitForResponse(
      (r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/posts',
    );

    const submit = page.locator(tid('upload-submit'));
    await expect(submit).toBeEnabled({ timeout: 30_000 });
    await submit.click();

    const req = await postRequest;
    const res = await postResponse;
    expect(res.status()).toBe(201);
    const created = (await res.json()) as { id: string; visibility: string; members?: { asset_id: string }[] };
    createdPosts.push(created.id);
    for (const m of created.members ?? []) createdAssets.push(m.asset_id);

    const body = req.postDataJSON() as { visibility?: string };
    expect(
      body.visibility,
      'the modal posted a tier the artist was never shown — the control and the request disagree',
    ).toBe(shown);
    // And the SERVER agrees, read back from the created row rather than
    // from the request we just made.
    expect(created.visibility).toBe(shown);
  });
});
