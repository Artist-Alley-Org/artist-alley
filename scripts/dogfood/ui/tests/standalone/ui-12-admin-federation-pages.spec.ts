// ui-12-admin-federation-pages.spec.ts (standalone)
//
// Federation-section admin surfaces (the five live surfaces
// shipped in 1.22.D-c). Asserts each page renders cleanly with
// EMPTY tables — no peer / share rows required. Cross-instance
// assertions (peer row visible, outbox sent, inbox processed)
// live in tests/federation/ui-21-federation-peer-flow.spec.ts.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';

test.describe('UI-12 admin federation pages (standalone)', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('peers page renders', async ({ page }) => {
    await page.goto('/admin/federation/peers');
    await expectPageRendersCleanly(page);
    // No row assertion — federation/ui-21 handles cross-instance
    // visibility. What this catches: a render crash from a missing
    // i18n key, a schema field the page consumes that the API
    // dropped, etc.
  });

  // #1592: accepting a request whose confirmation the other instance
  // refuses must say so on that request, which stays pending; the
  // page used to swallow the error. The API is stubbed (one pending
  // request, a 400 from accept) so no second instance is needed.
  test('a refused accept is shown and the request stays pending', async ({ page }) => {
    const id = '6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11';
    const now = new Date().toISOString();
    const pending = {
      id,
      instance_url: 'https://studio-x-1592.example',
      display_name: 'Studio X 1592',
      instance_public_key: '-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAx1592x1592x1592x1592x1592x1592x1592x1592xx=\n-----END PUBLIC KEY-----\n',
      trust_tier: 'connected',
      encryption_policy: 'plaintext',
      enabled: true,
      status: 'pending_inbound',
      handshake_at: now,
      handshake_by_user_ref: 0,
      notes: '',
      created_at: now,
      updated_at: now,
    };
    const detail = 'handshake: post confirm: handshake: peer answered HTTP 200 with Content-Type "text/html", not application/json';
    let accepts = 0;
    await page.route(/\/api\/v1\/admin\/federation\/peers\/pending-inbound$/, (route) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ items: [pending] }) }),
    );
    await page.route(new RegExp(`/api/v1/admin/federation/peers/${id}/accept$`), (route) => {
      accepts += 1;
      return route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ error: detail }) });
    });

    await page.goto('/admin/federation/peers');
    const request = page.locator('li', { hasText: 'Studio X 1592' });
    await expect(request).toBeVisible();
    await expect(page.getByTestId('accept-error')).toHaveCount(0);

    await request.getByRole('button', { name: 'Accept' }).click();
    const alert = request.getByTestId('accept-error');
    await expect(alert).toBeVisible();
    await expect(alert).toHaveAttribute('role', 'alert');
    await expect(alert).toContainText('The request is still pending');
    await expect(alert).toContainText(detail);
    await expect(request).toBeVisible();
    expect(accepts).toBe(1);
  });

  test('directories page renders', async ({ page }) => {
    await page.goto('/admin/federation/directories');
    await expectPageRendersCleanly(page);
  });

  test('shares page renders', async ({ page }) => {
    await page.goto('/admin/federation/shares');
    await expectPageRendersCleanly(page);
  });

  test('outbox page renders + has the queue table heading', async ({ page }) => {
    await page.goto('/admin/federation/outbox');
    await expectPageRendersCleanly(page);
    // Status filter UI (queued/sent/failed) is the structural marker.
    await expect(page.locator('main')).toContainText(/sent|queued|outbox/i);
  });

  test('inbox page renders + has the inbox state heading', async ({ page }) => {
    await page.goto('/admin/federation/inbox');
    await expectPageRendersCleanly(page);
    await expect(page.locator('main')).toContainText(/inbox|processed|received/i);
  });
});
