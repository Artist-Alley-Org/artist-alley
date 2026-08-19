// ui-39-collect-someone-elses-post.spec.ts
//
// #882 — the golden path the issue asks for, at the UI layer:
//
//   A finds B's post → saves it to A's own collection → sees it there.
//
// Everything here is another user's work. The spec deliberately picks a
// post whose author is NOT the signed-in caller, because "you can
// collect your own post" was already true through the upload modal's
// context-aware create and proves nothing about this change.
//
// Structural, not seed-id-bound: it provisions its own collection (via
// the picker's inline create, which is the real flow — one click from
// deciding to save), and cleans it up over the API afterwards.
//
// Driven at BOTH the 1080p desktop width and the 390px mobile width,
// because the affordance lives inside a popover menu and a modal, and
// those are exactly the two shapes that go wrong when the viewport
// narrows.

import { test, expect, type Page } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

interface PostListItem {
  id: string;
  title?: string;
  author_user_ref: number;
}

/** Whoever we are signed in as. */
async function callerRef(page: Page): Promise<number> {
  const me = await page.request.get('/api/v1/auth/me');
  expect(me.status()).toBe(200);
  return (await me.json()).ref as number;
}

/**
 * A post written by SOMEONE ELSE, with a non-empty title so the
 * collection page has something to assert on.
 *
 * The caller must be able to READ it — it comes off the caller's own
 * /posts feed, which is filtered by the read rule, so anything returned
 * here is by construction something the add gate will accept. A post
 * the caller cannot read is not reachable from the UI to begin with;
 * that half is covered by the Go gate tests, which can construct it.
 */
async function foreignPost(page: Page, me: number): Promise<PostListItem> {
  const res = await page.request.get('/api/v1/posts?limit=50');
  expect(res.status()).toBe(200);
  const items = ((await res.json()).items ?? []) as PostListItem[];
  const pick = items.find((p) => p.author_user_ref !== me && (p.title ?? '').trim().length > 0);
  expect(
    pick,
    'seed has no titled post authored by someone other than the caller — ' +
      'this spec is about collecting OTHER people’s work, so there is nothing to test',
  ).toBeTruthy();
  return pick as PostListItem;
}

/** Delete the collection we made, whatever the test did. */
async function dropCollection(page: Page, name: string) {
  const res = await page.request.get(`/api/v1/collections?tab=mine&q=${encodeURIComponent(name)}&limit=10`);
  if (res.status() !== 200) return;
  for (const c of ((await res.json()).items ?? []) as { id: string; name: string }[]) {
    if (c.name === name) await page.request.delete(`/api/v1/collections/${c.id}`);
  }
}

/**
 * The whole flow, at one viewport. Returns the collection id so the
 * caller can assert against the API as well as the page.
 */
async function saveForeignPostToNewCollection(
  page: Page,
  post: PostListItem,
  collectionName: string,
  /** When set, capture the open menu + the open picker under this
   *  prefix. The affordance is the change; a screenshot of the RESULT
   *  alone does not show that it was reachable.
   *
   *  ⚠️ Pass `testInfo.outputPath(name)`, never a bare name (#1211). A
   *  relative `path:` resolves against the Playwright CWD, which IS
   *  scripts/dogfood/ui — six of these were landing on TRACKED PNGs and
   *  dirtying the tree on every local run. */
  shotPrefix?: string,
): Promise<string> {
  await page.goto(`/posts/${post.id}`);
  await expect(page.locator('main')).toBeVisible();

  // The post's own ⋮ menu — the sidebar header's playlist/post actions.
  await page.getByRole('button', { name: 'Post actions' }).first().click();
  const save = page.locator('[data-testid="post-add-to-collection"]');
  await expect(
    save,
    'the "Save to collection" item is missing from a post the caller did not write — ' +
      'the affordance is gated on ownership somewhere it should not be',
  ).toBeVisible();
  if (shotPrefix) await page.screenshot({ path: `${shotPrefix}-menu.png` });
  await save.click();

  // The picker, with its inline create — one step, because being made
  // to create the collection first and come back is the friction #882
  // is not supposed to add.
  const picker = page.locator('[data-testid="collection-picker"]');
  await expect(picker).toBeVisible();
  if (shotPrefix) await page.screenshot({ path: `${shotPrefix}-picker.png` });
  await picker.locator('[data-testid="collection-picker-new-name"]').fill(collectionName);
  await picker.locator('[data-testid="collection-picker-create"]').click();

  // Success closes the modal. That is the ONLY success signal the
  // component gives, so it is what we wait on — and it is a real one:
  // a failed add keeps the modal open with an error banner.
  await expect(
    picker,
    'the picker stayed open, which is how it reports that the add failed',
  ).toBeHidden({ timeout: 15_000 });

  // Resolve the id the picker just created, over the API.
  const res = await page.request.get(
    `/api/v1/collections?tab=mine&q=${encodeURIComponent(collectionName)}&limit=10`,
  );
  expect(res.status()).toBe(200);
  const made = ((await res.json()).items ?? []).find(
    (c: { name: string }) => c.name === collectionName,
  );
  expect(made, 'the picker reported success but no collection by that name exists').toBeTruthy();
  return made.id as string;
}

test.describe('UI-39 collect someone else’s post (#882)', () => {
  // Above the 30s default: each case drives a post page, a modal, a
  // create + an add round-trip and then a collection page, and the
  // seeded post pages carry a real viewer. A tight budget here makes
  // the CLEANUP the thing that reports the timeout, which hides
  // whichever assertion actually ran long.
  test.setTimeout(90_000);

  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('desktop 1080p: save another user’s post, then see it in the collection', async ({ page }, testInfo) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    const me = await callerRef(page);
    const post = await foreignPost(page, me);
    const name = `UI-39 desktop ${Date.now()}`;

    try {
      const collectionId = await saveForeignPostToNewCollection(
        page, post, name, testInfo.outputPath('collect-post-desktop'),
      );

      // The REFERENCE landed — asked of the API, not of the page.
      const listed = await page.request.get(`/api/v1/collections/${collectionId}/posts`);
      expect(listed.status()).toBe(200);
      const ids = ((await listed.json()).items ?? []).map((p: { id: string }) => p.id);
      expect(ids, 'the post is not in the collection the picker said it went into').toContain(post.id);

      // …and the collection PAGE renders it. This is the half that was
      // missing entirely before #882: collection_posts had no listing
      // endpoint and no surface, so a reference could be created and
      // never seen.
      await page.goto(`/collections/${collectionId}`);
      const postsSection = page.locator('[data-testid="collection-posts"]');
      await expect(postsSection).toBeVisible();
      await expect(postsSection.locator(`a[href="/posts/${post.id}"]`).first()).toBeVisible();
      await page.screenshot({
        path: testInfo.outputPath('collect-post-desktop.png'),
        fullPage: false,
      });
    } finally {
      await dropCollection(page, name);
    }
  });

  test('mobile 390px: the same flow through the narrow layout', async ({ page }, testInfo) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const me = await callerRef(page);
    const post = await foreignPost(page, me);
    const name = `UI-39 mobile ${Date.now()}`;

    try {
      const collectionId = await saveForeignPostToNewCollection(
        page, post, name, testInfo.outputPath('collect-post-mobile-390'),
      );

      await page.goto(`/collections/${collectionId}`);
      const postsSection = page.locator('[data-testid="collection-posts"]');
      await expect(postsSection).toBeVisible();
      await expect(postsSection.locator(`a[href="/posts/${post.id}"]`).first()).toBeVisible();
      await page.screenshot({
        path: testInfo.outputPath('collect-post-mobile-390.png'),
        fullPage: false,
      });
    } finally {
      await dropCollection(page, name);
    }
  });

  test('removing it from my collection leaves the post alone', async ({ page }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    const me = await callerRef(page);
    const post = await foreignPost(page, me);
    const name = `UI-39 remove ${Date.now()}`;

    try {
      const collectionId = await saveForeignPostToNewCollection(page, post, name);

      const removed = await page.request.delete(
        `/api/v1/collections/${collectionId}/posts/${post.id}`,
      );
      expect(removed.status()).toBe(204);

      const listed = await page.request.get(`/api/v1/collections/${collectionId}/posts`);
      expect(((await listed.json()).items ?? []).length).toBe(0);

      // The post itself is untouched — un-pinning a reference must
      // never reach the referent.
      const still = await page.request.get(`/api/v1/posts/${post.id}`);
      expect(
        still.status(),
        'removing the post from a collection deleted the POST',
      ).toBe(200);
    } finally {
      await dropCollection(page, name);
    }
  });
});
