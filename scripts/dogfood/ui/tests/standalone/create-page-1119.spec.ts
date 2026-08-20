// #1119 slice 1 — the full-page create surface.
//
// Three things this file exists to hold, in the order they would hurt:
//
//  1. ⛔ THE FRICTION LINE. From empty, dropping a file and publishing is
//     TWO actions and requires no field the artist did not choose to
//     fill. This is the one assertion the whole epic is held to, and it
//     is asserted by COUNTING the interactions rather than by prose in a
//     comment — a page that grows a required title, or a "make a post?"
//     checkbox to find and tick, fails here and nowhere else.
//
//  2. `show_on_upload` IS OBEYED, BOTH WAYS. #1173 shipped the flag with
//     no consumer outside the admin editor that writes it. Turning it
//     off through the REAL admin form must remove exactly that field
//     from the create page, and turning it back on must restore it. One
//     direction alone is not a dial: a page that renders nothing passes
//     the "off" half.
//
//  3. A DRAFT IS A DRAFT EVERYWHERE. Created from this page, absent from
//     browse, present in the author's own drafts, and — the arm that
//     matters — NOT readable by an anonymous visitor even when its
//     visibility is `public`. `draft` and `visibility` are different
//     questions and a post that answers only the second is publication
//     by omission.
//
// The probe field is created and archived by this file rather than
// borrowed from the seed, for the reason field-participation-1173 gives:
// asserting against a seeded field makes the test a hostage to whatever
// the seed ships, and turning a real field off mid-suite changes the
// page under every other spec.

import { test, expect, type APIRequestContext, type Page } from '@playwright/test';
import { loginAsAdminViaAPI, LOGGED_OUT } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

const PROBE_CODE = 'probe_1119_create_page';
const PROBE_LABEL = 'Finish (1119 probe)';

/**
 * Everything this file made, torn down after each case.
 *
 * Deleting the post is not enough: a post delete does not remove its
 * member assets, and the assets are what the browse/uploads grids show.
 */
const createdAssets: string[] = [];
const createdPosts: string[] = [];

test.describe.configure({ mode: 'serial' });

let probeId = '';

async function findProbe(request: APIRequestContext, status?: string) {
  const url = status ? `/api/v1/fields?status=${status}` : '/api/v1/fields';
  const r = await request.get(url);
  expect(r.ok(), `GET ${url} → ${r.status()}`).toBeTruthy();
  const rows = (await r.json()) as { id: string; code: string }[];
  return rows.find((f) => f.code === PROBE_CODE);
}

/**
 * The probe applies to EVERY asset type (`applies_to` omitted), because
 * the create page asks for the type the server assigned to the uploaded
 * file and the fixture below uploads a plain text file. Pinning the
 * probe to one type would make this spec depend on that promotion
 * landing on a particular number.
 */
async function ensureProbeField(request: APIRequestContext): Promise<string> {
  await loginAsAdminViaAPI(request);
  const revive = async (id: string) => {
    const r = await request.patch(`/api/v1/fields/${id}`, {
      data: {
        status: 'active',
        label: PROBE_LABEL,
        // A revived tombstone must start from the DEFAULT participation,
        // or a crashed run that left the flag off makes the "unset is
        // unchanged" assertion pass for the wrong reason.
        show_on_upload: true,
      },
    });
    expect(r.ok(), `revive probe → ${r.status()} ${await r.text()}`).toBeTruthy();
    return id;
  };
  const existing = (await findProbe(request)) ?? (await findProbe(request, 'archived'));
  if (existing) return revive(existing.id);

  const r = await request.post('/api/v1/fields', {
    data: {
      code: PROBE_CODE,
      label: PROBE_LABEL,
      type: 'text',
      subject_kind: 'asset',
      display_order: 9200,
    },
  });
  if (r.status() === 201) return ((await r.json()) as { id: string }).id;
  const raced = (await findProbe(request)) ?? (await findProbe(request, 'archived'));
  expect(raced, `create probe → ${r.status()} ${await r.text()}`).toBeTruthy();
  return revive(raced!.id);
}

test.beforeAll(async ({ request }) => {
  probeId = await ensureProbeField(request);
});

test.afterAll(async ({ request }) => {
  if (!probeId) return;
  await loginAsAdminViaAPI(request);
  await request.delete(`/api/v1/fields/${probeId}`);
});

test.afterEach(async ({ request }) => {
  // Posts first, then assets: an asset that is still a member is one the
  // delete has to reason about, and there is no reason to make it.
  for (const id of createdPosts.splice(0)) {
    await request.delete(`/api/v1/posts/${id}`).catch(() => undefined);
  }
  for (const id of createdAssets.splice(0)) {
    await request.delete(`/api/v1/assets/${id}`).catch(() => undefined);
  }
});

/**
 * Open the field disclosure. Clicking the <details> itself is not the
 * same thing — the toggle lives on <summary>, and a click that lands
 * anywhere else in an OPEN details does nothing at all.
 */
async function openFields(page: Page) {
  const details = page.locator(tid('create-fields'));
  await expect(details).toBeVisible();
  if (!(await details.evaluate((el) => (el as HTMLDetailsElement).open))) {
    await details.locator('summary').click();
  }
  await expect(details).toHaveJSProperty('open', true);
}

/**
 * Put one novel file into the page's queue, wait for it to be ready, and
 * return the ASSET ID the server created for it.
 *
 * ⛔ RETURNING THE ID IS NOT A CONVENIENCE — IT IS THE CLEANUP CONTRACT.
 *
 * Adding a file creates an asset row IMMEDIATELY; the upload does not
 * wait for submit. So every call here leaves a real asset behind, and a
 * spec that only deletes the POST it published leaks one per case, per
 * run, forever. #1198 is the same lesson: leftovers accumulated, the
 * browse feed is newest-first, and they sat at the TOP of page one where
 * they pushed the seeded corpus out of the window OTHER specs read.
 * `marquee-select-1177` sweeps the uploads grid and `ui-13` searches for
 * a seeded term; both go red from leftovers that are nothing to do with
 * them, which is how a spec that changes what the next spec sees looks
 * from the outside.
 *
 * The id is read off the create RESPONSE rather than guessed from the
 * page, so it is exact and available even when the test never publishes.
 */
async function dropOneFile(page: Page, name = 'create-probe.txt'): Promise<string> {
  const created = page.waitForResponse(
    (r) => r.url().includes('/api/v1/assets') && r.request().method() === 'POST' && r.ok(),
    { timeout: 30_000 },
  );
  await page.locator(tid('create-file-input')).setInputFiles({
    name,
    mimeType: 'text/plain',
    // Novel bytes: content-addressed storage would otherwise dedupe a
    // fixed body and the row would come back as an existing asset.
    buffer: Buffer.from(`create page fixture ${Date.now()}-${Math.random()}`),
  });
  await expect(page.locator(tid('create-file-row'))).toHaveCount(1);
  await expect(page.locator(tid('create-publish'))).toBeEnabled({ timeout: 30_000 });
  const assetId = ((await (await created).json()) as { id: string }).id;
  createdAssets.push(assetId);
  return assetId;
}


test.describe('the create page (#1119)', () => {
  test('drop a file, press publish — TWO actions, nothing required', async ({ page }) => {
    await page.goto('/create');
    await expect(page.locator(tid('create-page'))).toBeVisible();

    // Nothing typed, nothing ticked, nothing opened.
    // ── ACTION 1 ──────────────────────────────────────────────────
    await dropOneFile(page);

    // The button must be live with NO further input. If a title, a
    // visibility choice or a "create a post?" tick were required, it
    // would be disabled here and this is where the friction line breaks.
    await expect(
      page.locator(tid('create-publish')),
      'Publish must be reachable from a bare drop. Anything that disables it here is ' +
        'a field the artist did not choose to fill.',
    ).toBeEnabled();
    await expect(page.locator(tid('create-title'))).toHaveValue('');

    // ── ACTION 2 ──────────────────────────────────────────────────
    await page.locator(tid('create-publish')).click();

    // Landed on the post it just made.
    await page.waitForURL(/\/posts\/[0-9a-f-]{36}/, { timeout: 30_000 });
    const postId = page.url().split('/posts/')[1];

    const verify = await page.request.get(`/api/v1/posts/${postId}`);
    expect(verify.status()).toBe(200);
    const post = (await verify.json()) as { draft?: boolean; members?: unknown[] };
    expect(post.draft, 'Publish means published').toBeFalsy();
    expect(post.members?.length).toBe(1);

    createdPosts.push(postId);
  });

  test('show_on_upload governs the create page, in BOTH directions', async ({ page, request }) => {
    // ── on (the default, unset) ───────────────────────────────────
    await page.goto('/create');
    await dropOneFile(page);
    await openFields(page);
    await expect(
      page.locator(tid(`create-field-${PROBE_CODE}`)),
      'a field nobody has configured must still be offered — participation is OPT-OUT, ' +
        'and reading an absent flag as "off" would empty every install at once',
    ).toBeVisible({ timeout: 15_000 });

    // ── off, through the REAL admin form ──────────────────────────
    await page.goto(`/admin/fields/${PROBE_CODE}`);
    await expect(page.locator(tid('field-edit-participation'))).toBeVisible();
    const box = page.locator(tid('field-edit-show-on-upload'));
    await expect(box).toBeChecked();
    await box.uncheck();
    await page.locator(tid('field-options-save')).click();
    await expect(page.locator(tid('field-options-saved'))).toBeVisible();

    const off = await request.get(`/api/v1/fields/${probeId}`);
    expect(((await off.json()) as { show_on_upload?: boolean }).show_on_upload).toBe(false);

    await page.goto('/create');
    await dropOneFile(page);
    await openFields(page);
    // Wait for SOMETHING to settle before reading an absence, so an
    // empty answer is a real empty answer and not a race.
    await expect(page.locator(tid('create-fields'))).toBeVisible();
    await expect(page.locator(tid(`create-field-${PROBE_CODE}`))).toHaveCount(0);

    // ── back on: it is a dial, not a one-way door ─────────────────
    await page.goto(`/admin/fields/${PROBE_CODE}`);
    await page.locator(tid('field-edit-show-on-upload')).check();
    await page.locator(tid('field-options-save')).click();
    await expect(page.locator(tid('field-options-saved'))).toBeVisible();

    await page.goto('/create');
    await dropOneFile(page);
    await openFields(page);
    await expect(page.locator(tid(`create-field-${PROBE_CODE}`))).toBeVisible({
      timeout: 15_000,
    });
  });

  test('a draft made here is invisible to everyone but its author', async ({ page, browser }) => {
    await page.goto('/create');
    await dropOneFile(page);

    // PUBLIC visibility, deliberately. `draft` and `visibility` answer
    // different questions, and the whole point of the draft state is
    // that it is STRICTER than the visibility — a `public` draft is no
    // more readable by a stranger than a `private` one.
    await page.locator(tid('create-visibility')).selectOption('public');
    await page.locator(tid('create-save-draft')).click();

    await page.waitForURL(/\/posts\/[0-9a-f-]{36}/, { timeout: 30_000 });
    const postId = page.url().split('/posts/')[1];

    try {
      const mine = await page.request.get(`/api/v1/posts/${postId}`);
      expect(mine.status(), 'the author can read their own draft').toBe(200);
      const post = (await mine.json()) as { draft?: boolean; visibility?: string };
      expect(post.draft).toBe(true);
      expect(post.visibility).toBe('public');

      // Present in the author's drafts.
      const drafts = await page.request.get('/api/v1/posts?draft=true&limit=100');
      expect(drafts.status()).toBe(200);
      const draftBody = (await drafts.json()) as { items?: { id: string }[] };
      expect(
        (draftBody.items ?? []).some((p) => p.id === postId),
        'a draft the author cannot find is a draft they have lost',
      ).toBeTruthy();

      // Absent from browse.
      const browse = await page.request.get('/api/v1/posts?limit=100');
      const browseBody = (await browse.json()) as { items?: { id: string }[] };
      expect((browseBody.items ?? []).some((p) => p.id === postId)).toBeFalsy();

      // ⛔ And invisible to an anonymous visitor DESPITE `public`.
      const anon = await browser.newContext({ storageState: LOGGED_OUT });
      try {
        const res = await anon.request.get(`/api/v1/posts/${postId}`);
        expect(
          res.status(),
          'a PUBLIC draft must not be readable by a stranger — if this passes at 200 ' +
            'the draft state is decorative and publication happens by omission',
        ).not.toBe(200);
      } finally {
        await anon.close();
      }
    } finally {
      createdPosts.push(postId);
    }
  });

  // ⛔ THE REGRESSION THIS FILE SHIPPED ONCE AND MUST NOT SHIP AGAIN.
  //
  // The upload modal links to the create page, and the modal is mounted
  // in the ROOT LAYOUT — so that anchor is in the DOM on every page,
  // visible or not. When the page lived at `/posts/new` the link became
  // the FIRST match for `a[href^="/posts/"]`, which is how ten dogfood
  // cases locate a post card. Invisible and first: three specs spent 30s
  // each clicking a link they could not see, and it passed locally
  // because the feed happened to render before the assertion ran.
  //
  // So the rule is a property of the app, not of those ten locators: no
  // link OUTSIDE the feed may claim the post-permalink prefix. Asserted
  // on browse, with the modal open, which is the exact state that broke.
  test('no offscreen link claims the post-permalink prefix', async ({ page }) => {
    await page.goto('/');
    await page.locator(tid('nav-upload-button')).click();
    await expect(page.getByRole('dialog')).toBeVisible();

    const strays = await page.$$eval('a[href^="/posts/"]', (els) =>
      els
        .filter((el) => !el.closest('main'))
        .map((el) => `${el.getAttribute('href')} (${el.getAttribute('data-testid') ?? 'no testid'})`),
    );
    expect(
      strays,
      'a link outside <main> matching a[href^="/posts/"] will be picked up by every ' +
        'post-card locator in this suite, and if it is invisible they hang on it',
    ).toEqual([]);
  });

  test('the page works at 390px — the controls are reachable, not merely present', async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto('/create');
    await expect(page.locator(tid('create-page'))).toBeVisible();
    await dropOneFile(page);

    // The rail stacks under the main column at this width; both halves
    // have to remain operable rather than merely rendered.
    await expect(page.locator(tid('create-visibility'))).toBeVisible();
    await page.locator(tid('create-publish')).scrollIntoViewIfNeeded();
    await expect(page.locator(tid('create-publish'))).toBeEnabled();

    // Nothing may scroll the BODY sideways at target width.
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, 'the page overflows horizontally at 390px').toBeLessThanOrEqual(1);
  });
});
