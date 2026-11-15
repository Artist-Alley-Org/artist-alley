// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// feed-end.spec.ts: an ordinary user scrolls the home feed of a whole
// public dataset edition to its end (#1514, ui-scale.yml).
//
// # What is being proved
//
// The browse wall appends while `nextCursor !== null` and renders
// "You have reached the end of the feed" when the cursor runs out
// (web/src/routes/+page.svelte). On a full corpus that means many page
// loads in a row, and on site_a some of them come back SHORT because the
// server hides posts whose members are all restricted for the reader. The
// client must keep going past those. So:
//
//   - the end-of-feed marker appears within a bounded time;
//   - the feed requests the browser made form ONE unbroken cursor chain,
//     from the first page (no cursor) to a page with no next_cursor;
//   - that chain equals, page by page, an API replay of the browser's own
//     first query (its exact filters: feed, dir, mature, ai, limit) as the
//     same user. The replay is checked against the database by
//     cursor-walk.spec.ts, so this ties the UI to the recorded set;
//   - on site_a, at least one non-final page of that chain was short.
//
// # What it deliberately does not count
//
// Rendered cards. The wall may virtualise, so DOM tile counts are not a
// faithful measure of what was loaded; the responses are. A `head`
// refresh (a later request for page one, #1407) is allowed and ignored by
// the chain.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { checkRunInputs, scaleSite, scalePrincipalNames, signInAs, walk, pageBound } from '../../helpers/scale';

const END_OF_FEED = 'You have reached the end of the feed';
const SCROLL_DEADLINE_MS = 8 * 60_000;
const VIEWPORT = { width: 1920, height: 1080 };

interface FeedResponse {
  at: number;
  status: number;
  params: URLSearchParams;
  ids: string[];
  next: string | null;
}

/** The query without its cursor, as a stable string, so pages of one chain compare equal. */
function baseKey(p: URLSearchParams): string {
  const q = new URLSearchParams(p);
  q.delete('cursor');
  return [...q.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => `${k}=${v}`).join('&');
}

test.describe(`full-corpus feed scroll (${scaleSite()})`, () => {
  test('an ordinary user scrolls the home feed to its end', async ({ browser, request }, testInfo) => {
    test.setTimeout(12 * 60_000);
    const E = checkRunInputs();
    await loginAsAdminViaAPI(request);
    const name = scalePrincipalNames()[0];
    const P = E.principals[name];
    const baseURL = String(testInfo.project.use.baseURL);
    const user = await signInAs(request, baseURL, name, P.ref);
    // In memory only: the session cookie moves to the browser context and
    // is never written to disk.
    const state = await user.storageState();
    const context = await browser.newContext({
      baseURL,
      storageState: state,
      viewport: VIEWPORT,
      locale: 'en-US',
      ignoreHTTPSErrors: true,
    });
    try {
      const page = await context.newPage();
      // The unfiltered "latest" wall, chosen explicitly: without a local
      // choice the segment falls back to the account's own preference
      // (browseView.svelte.ts), and a "following" wall would be a smaller,
      // different feed. The replay below still mirrors whatever was sent.
      await page.addInitScript(() => {
        try {
          localStorage.setItem('aa_browse_filter', 'latest');
        } catch {
          /* storage-less context: the account default still pages */
        }
      });
      const pending: Promise<FeedResponse>[] = [];
      page.on('response', (r) => {
        const u = new URL(r.url());
        if (u.pathname !== '/api/v1/posts') return;
        const at = Date.now();
        pending.push(
          r
            .json()
            .then((b: { items?: Array<{ id: string }>; next_cursor?: string | null }) => ({
              at,
              status: r.status(),
              params: u.searchParams,
              ids: (b.items ?? []).map((i) => i.id),
              next: b.next_cursor ?? null,
            }))
            .catch(() => ({ at, status: r.status(), params: u.searchParams, ids: [], next: null })),
        );
      });

      await page.goto('/');
      await expect(page.locator('[data-testid="browse-wall"]')).toBeVisible({ timeout: 30_000 });

      const end = page.getByText(END_OF_FEED, { exact: true });
      const started = Date.now();
      let scrolls = 0;
      while (!(await end.isVisible())) {
        expect(
          Date.now() - started,
          `the end-of-feed marker did not appear within ${SCROLL_DEADLINE_MS / 1000}s ` +
            `(${pending.length} feed responses so far)`,
        ).toBeLessThan(SCROLL_DEADLINE_MS);
        // The app scrolls <main>, not the window.
        await page.evaluate(() => {
          const m = document.querySelector('main');
          if (m) m.scrollTop = m.scrollHeight;
        });
        scrolls++;
        await page.waitForTimeout(250);
      }
      const elapsedMs = Date.now() - started;
      const responses = (await Promise.all(pending)).sort((a, b) => a.at - b.at);
      expect(responses.filter((r) => r.status !== 200).length, 'every feed response was 200').toBe(0);

      // The chain: the first page the browser asked for, then each cursor it
      // was handed, in turn.
      const first = responses.find((r) => !r.params.get('cursor'));
      expect(first, 'the browser requested a first feed page').toBeDefined();
      const key = baseKey(first!.params);
      const sameQuery = responses.filter((r) => baseKey(r.params) === key);
      const byCursor = new Map<string, FeedResponse>();
      for (const r of sameQuery) {
        const c = r.params.get('cursor');
        if (!c) continue;
        const prior = byCursor.get(c);
        if (prior) {
          expect(r.ids, 'a cursor requested twice returned the same page').toEqual(prior.ids);
        } else {
          byCursor.set(c, r);
        }
      }
      const chain: FeedResponse[] = [first!];
      for (let next = first!.next; next !== null; ) {
        const r = byCursor.get(next);
        expect(r, `the browser never requested cursor ${chain.length + 1} of the chain`).toBeDefined();
        chain.push(r!);
        next = r!.next;
        expect(chain.length, 'the chain terminates').toBeLessThanOrEqual(
          pageBound(E.admin.posts.length, Number(first!.params.get('limit') ?? '36')),
        );
      }

      // Replay the browser's own first query as the same user.
      const params: Record<string, string> = {};
      for (const [k, v] of new URLSearchParams(first!.params)) {
        if (k !== 'cursor' && k !== 'limit') params[k] = v;
      }
      const limit = Number(first!.params.get('limit'));
      expect(Number.isInteger(limit) && limit > 0, 'the browser sent a page size').toBe(true);
      const replay = await walk(user, '/api/v1/posts', params, limit, pageBound(E.admin.posts.length, limit));
      expect(chain.length, 'the browser loaded as many pages as the replay walk').toBe(replay.pages.length);
      chain.forEach((r, i) => {
        expect(r.ids, `page ${i + 1} of the browser's chain equals the replay`).toEqual(replay.pages[i].ids);
      });

      const shortNonFinal = chain.slice(0, -1).filter((r) => r.ids.length < limit).length;
      testInfo.annotations.push({
        type: 'scale',
        description:
          `${E.site} ${name}: ${chain.length} feed pages (limit ${limit}, query ${key}), ` +
          `${chain.reduce((n, r) => n + r.ids.length, 0)} posts, ${shortNonFinal} short non-final, ` +
          `${scrolls} scroll steps, ${Math.round(elapsedMs / 1000)}s to the end marker`,
      });
      expect(chain.length, 'the feed needed at least three page loads').toBeGreaterThanOrEqual(3);
      if (E.site === 'site_a') {
        expect(shortNonFinal, 'the browser kept loading past at least one short page').toBeGreaterThan(0);
      }
    } finally {
      await context.close();
      await user.dispose();
    }
  });
});
