// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1598: an operator can read one asset's preservation history.
//
// The page is reachable two ways (the storage section's tile and the
// checksum page's link), refuses an id that is not a UUID without
// calling the server, says plainly when no asset has the id, and loads a
// lookup straight from a `?asset=` link.
//
// # Read-only, on purpose
//
// Events are written only by the checksum sweep, which reads every
// stored byte, and they are append-only: nothing short of deleting the
// asset removes one. A spec that wrote them would leave rows the corpus
// ratchet counts. So this spec writes nothing. It reads a real seeded
// asset's history, which is empty on a fresh CI database and listed when
// a sweep has run, and accepts either as long as the page states which.
// The two request-ordering cases (a slow answer for an earlier lookup,
// a failed page change) answer from page.route with synthetic pages, so
// their timing is decided by the test, not the network, and they write
// nothing either.
// The paging, ordering, wording and access rules are pinned below the
// browser: Go tests on a real database (internal/storage history_test,
// internal/http TestStorageAdmin_PreservationHistory) and the vitest
// suite for the labels (web/src/lib/admin/preservationHistory.test.ts).

import { test, expect } from '../../helpers/test';
import type { Route } from '@playwright/test';
import { loginAsAdminViaUI } from '../../helpers/auth';
import { expectPageRendersCleanly } from '../../helpers/assertions';

const HISTORY_ROUTE = '**/api/v1/admin/storage/assets/*/preservation-events*';

/** A synthetic, schema-shaped event; `variant` makes it recognisable. */
function fakeEvent(n: number, variant: string) {
  return {
    id: `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`,
    source: 'local',
    event_type: 'fixity-check',
    occurred_at: new Date(Date.UTC(2026, 10, 19, 12, 0, 0) - n * 1000).toISOString(),
    outcome: 'warning',
    outcome_reason: 'no_recorded_digest',
    object_hash: 'b'.repeat(64),
    variant_key: variant,
    is_original: false,
    observed_generation: 0,
    algorithm: 'sha256',
    observed_sha256: 'c'.repeat(64),
    observed_size_bytes: 1024,
    recorded_sha256: null,
    fixity_status: 'unverifiable',
    sweep_run_id: '11111111-2222-4333-8444-555555555555',
    sweep_run_present: true,
    agent: { name: 'artist-alley', version: null },
  };
}

function fakePage(asset: string, items: object[], total: number, next: string | null) {
  return { asset_id: asset, items, total, next_cursor: next };
}

async function fulfillQuietly(route: Route, body: object, status = 200): Promise<void> {
  // The page may have aborted this request already (that is the point
  // of the race case); fulfilling an aborted request throws.
  try {
    await route.fulfill({ status, json: body });
  } catch {
    /* aborted by the page */
  }
}

test.describe('preservation history (#1598)', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test('the storage tile and the checksum page both lead to it', async ({ page }) => {
    await page.goto('/admin/storage');
    const tile = page.getByRole('link', { name: /Preservation history/ });
    await expect(tile).toHaveAttribute('href', '/admin/storage/history');

    await page.goto('/admin/storage/checksums');
    await page.getByRole('link', { name: "See one asset's preservation history" }).click();
    await expect(page).toHaveURL(/\/admin\/storage\/history$/);
    await expect(page.getByRole('heading', { name: 'Preservation history', level: 2 })).toBeVisible();
    await expectPageRendersCleanly(page);
  });

  test('an id that is not a UUID is refused without a lookup', async ({ page }) => {
    await page.goto('/admin/storage/history');
    let called = false;
    page.on('request', (r) => {
      if (r.url().includes('/preservation-events')) called = true;
    });
    const input = page.getByLabel('Asset ID');
    await input.fill('../not-an-id');
    await page.getByRole('button', { name: 'Show history' }).click();
    await expect(page.getByText(/Enter an asset ID in the form/)).toBeVisible();
    await expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(called).toBe(false);
  });

  test('an unknown asset is reported, not shown as empty', async ({ page }) => {
    await page.goto('/admin/storage/history');
    await page.getByLabel('Asset ID').fill('00000000-0000-4000-8000-000000001598');
    await page.getByRole('button', { name: 'Show history' }).click();
    await expect(page.getByRole('alert')).toHaveText(/No asset has this ID\./);
    await expect(page.getByRole('heading', { level: 3 })).toHaveCount(0);
  });

  test("a real asset's history loads from a link and says what it holds", async ({ page }) => {
    const r = await page.request.get('/api/v1/assets?limit=1');
    expect(r.ok()).toBe(true);
    const body = (await r.json()) as { items: { id: string }[] };
    // The standalone stack is seeded; an empty corpus is a broken fixture, not a skip.
    expect(body.items.length).toBeGreaterThan(0);
    const id = body.items[0].id;

    await page.goto(`/admin/storage/history?asset=${id}`);
    await expect(page.getByLabel('Asset ID')).toHaveValue(id);
    const heading = page.getByRole('heading', { level: 3 });
    await expect(heading).toHaveText(/^(No events \(0 in total\)|Events 1–\d+ of \d+)$/);
    await expect(page.getByText(`Asset ${id}`)).toBeVisible();

    const items = page.locator('main ol > li');
    const n = await items.count();
    if (n === 0) {
      await expect(page.getByText(/No preservation events have been recorded for this asset yet\./)).toBeVisible();
    } else {
      expect(n).toBeLessThanOrEqual(25); // one bounded page
      for (let i = 0; i < n; i++) {
        await expect(items.nth(i)).toContainText(/Passed|Warning|Failed/);
        await expect(items.nth(i)).toContainText(/Original|Derivative: /);
      }
    }
    await expectPageRendersCleanly(page);
  });

  test('a slow answer for an earlier lookup never replaces the latest one', async ({ page }) => {
    const A = '00000000-0000-4000-8000-00000000000a';
    const B = '00000000-0000-4000-8000-00000000000b';
    // A's route is parked, not awaited inside the handler: Playwright
    // runs route handlers one at a time, so a handler that waits would
    // hold B's request too. A parked route stays paused until the test
    // answers it, after B has rendered.
    let heldA: Route | null = null;
    await page.route(HISTORY_ROUTE, async (route) => {
      if (route.request().url().includes(A)) {
        heldA = route;
        return;
      }
      await fulfillQuietly(route, fakePage(B, [fakeEvent(2, 'from-b')], 1, null));
    });

    await page.goto('/admin/storage/history');
    const input = page.getByLabel('Asset ID');
    await input.fill(A);
    await input.press('Enter');
    await input.fill(B);
    await input.press('Enter'); // the form still submits while A is out
    await expect(page.getByText(`Asset ${B}`)).toBeVisible();
    await expect(page.getByText('Derivative: from-b')).toBeVisible();

    expect(heldA).not.toBeNull();
    await fulfillQuietly(heldA!, fakePage(A, [fakeEvent(1, 'from-a')], 1, null));
    // Give a stale response every chance to land before asserting it did not.
    await page.waitForTimeout(300);
    await expect(page.getByText('Derivative: from-a')).toHaveCount(0);
    await expect(page.getByText(`Asset ${B}`)).toBeVisible();
    await expect(page.getByRole('heading', { level: 3 })).toHaveText('Events 1–1 of 1');
    await expect(input).toHaveValue(B);
    await expect(page).toHaveURL(new RegExp(`asset=${B}$`));
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Show history' })).toBeEnabled();
  });

  test('a failed page change keeps the current page and can be retried', async ({ page }) => {
    const C = '00000000-0000-4000-8000-00000000000c';
    const first = Array.from({ length: 25 }, (_, i) => fakeEvent(100 + i, `p1-${String(i).padStart(2, '0')}`));
    const second = [fakeEvent(200, 'p2-00'), fakeEvent(201, 'p2-01')];
    let olderCalls = 0;
    await page.route(HISTORY_ROUTE, async (route) => {
      const cursor = new URL(route.request().url()).searchParams.get('cursor');
      if (!cursor) return fulfillQuietly(route, fakePage(C, first, 27, 'cursor-page-2'));
      olderCalls++;
      if (olderCalls === 1) return fulfillQuietly(route, { error: 'storage read failed, try again' }, 500);
      return fulfillQuietly(route, fakePage(C, second, 27, null));
    });

    await page.goto(`/admin/storage/history?asset=${C}`);
    const heading = page.getByRole('heading', { level: 3 });
    await expect(heading).toHaveText('Events 1–25 of 27');
    await expect(page.getByText('Page 1')).toBeVisible();

    const older = page.getByRole('button', { name: 'Older' });
    await older.click();
    await expect(page.getByRole('alert')).toContainText('storage read failed, try again');
    // Still the page that is on screen, numbered as such, retryable.
    await expect(heading).toHaveText('Events 1–25 of 27');
    await expect(page.getByText('Page 1')).toBeVisible();
    await expect(page.getByText('Derivative: p1-00')).toBeVisible();
    await expect(older).toBeEnabled();
    await expect(page.getByRole('button', { name: 'Newer' })).toBeDisabled();

    await older.click(); // the retry
    await expect(heading).toHaveText('Events 26–27 of 27');
    await expect(page.getByText('Page 2')).toBeVisible();
    await expect(page.getByText('Derivative: p2-01')).toBeVisible();
    await expect(page.getByRole('alert')).toHaveCount(0);
    expect(olderCalls).toBe(2);

    await page.getByRole('button', { name: 'Newer' }).click();
    await expect(heading).toHaveText('Events 1–25 of 27');
    await expect(page.getByText('Page 1')).toBeVisible();
  });
});
