// ui-21-federation-peer-flow.spec.ts (federation)
//
// Cross-instance assertions only. Requires:
//   - studio-a (dev) is running
//   - studio-b (dogfood profile) is running
//   - scripts/dogfood/pair.sh has paired them
//
// The standalone page-renders coverage for the same admin
// surfaces lives in
// tests/standalone/ui-12-admin-federation-pages.spec.ts.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

test.describe('UI-21 federation peer flow (cross-instance)', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('peers page lists studio-b after pair.sh', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    // pair.sh registers studio-b; the row MUST be visible by its
    // instance URL.
    await expect(page.locator('main')).toContainText(/studio-b\.local/);
  });

  test('peer row shows an enabled / connected status', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    const main = page.locator('main');
    await expect(main).toContainText(/studio-b\.local/);
    // No row should read as disabled/disconnected after fresh pair.sh.
    await expect(main).not.toContainText(/disabled|disconnected|errored|failed/i);
  });

  test('Like API succeeds + the dispatch trail is queryable', async ({ page }) => {
    // Drive a local Like; the outbox dispatcher signs + delivers
    // it to studio-b. This is the UI-driven sibling of the shell
    // scenario `06-wire-dispatch` — the same wire surface, but
    // with the test running under the actual admin session.
    const list = await page.request.get('/api/v1/posts?limit=1');
    expect(list.status()).toBe(200);
    const post = (await list.json()).items[0];
    expect(post).toBeTruthy();

    const like = await page.request.post(`/api/v1/posts/${post.id}/like`);
    expect([200, 201, 204]).toContain(like.status());

    // The outbox admin page is the operator's view of the
    // dispatch queue. Visit it + confirm the page survives the
    // post-like outbox emission.
    await page.goto('/admin/federation/outbox');
    await expect(page.locator('main')).toBeVisible();
  });
});
