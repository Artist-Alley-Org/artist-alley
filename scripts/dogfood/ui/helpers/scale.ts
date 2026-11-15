// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// helpers/scale.ts: the full-corpus scale check (#1514, .github/workflows/ui-scale.yml).
//
// Read ONLY by tests/scale/*. Those specs run in their own Playwright
// project (`scale`), on a disposable stack seeded with a whole public
// dataset edition, and never inside the standalone or federation suites.
//
// # The expected sets are not the API's own answer
//
// scripts/ci/scale_expected.py records them from the database, in a READ
// ONLY transaction, before the dataset copy is released, and fails the job
// if the seeded instance disagrees with its own MANIFEST.json or seed
// report. The specs compare the API against that file, so a paging bug and
// a rule the API shares with itself are not the same blind spot.
//
// # Credentials
//
// Seeded users have no password. `signInAs` asks the admin helpdesk
// endpoint for a one-shot password on THIS disposable instance, signs in
// with it, and drops it. It is never logged, put in an assertion message,
// written to a file or returned.

import fs from 'node:fs';
import { expect, request as pwRequest, type APIRequestContext } from '@playwright/test';

export interface ScalePrincipal {
  ref: number;
  /** Live asset ids minus the mature rows this user does not own, in keyset order. */
  assets: string[];
  owned_assets: number;
  /** Live published post ids this user authored, in keyset order. */
  authored_posts: string[];
}

export interface ScaleExpected {
  schema: 'artist-alley.ui-scale-expected/v1';
  site: 'site_a' | 'site_b';
  manifest: { assets: number; posts: number };
  seed_report: Record<string, number>;
  db: {
    assets_live: number;
    posts_live: number;
    assets_mature: number;
    posts_mature: number;
    posts_by_visibility: Record<string, number>;
    per_asset_preview_jobs: number;
  };
  /** Every live asset / live published post id, in the API's keyset order. */
  admin: { assets: string[]; posts: string[] };
  principals: Record<string, ScalePrincipal>;
}

let cached: ScaleExpected | undefined;

/** The recorded expected sets. Missing or malformed is a FAILURE, never a skip. */
export function scaleExpected(): ScaleExpected {
  if (cached) return cached;
  const path = process.env.SCALE_EXPECTED;
  if (!path) {
    throw new Error(
      'SCALE_EXPECTED is not set. The scale project runs only after scripts/ci/scale_expected.py ' +
        'has recorded the seeded instance (see .github/workflows/ui-scale.yml).',
    );
  }
  const doc = JSON.parse(fs.readFileSync(path, 'utf-8')) as ScaleExpected;
  if (doc.schema !== 'artist-alley.ui-scale-expected/v1') {
    throw new Error(`${path}: unexpected schema ${String(doc.schema)}`);
  }
  cached = doc;
  return doc;
}

/**
 * The run's site and principals, from the ENVIRONMENT only (SCALE_SITE,
 * SCALE_PRINCIPALS space-separated), so that test discovery (`--list`)
 * works without a seeded instance. `checkRunInputs` is what fails a run
 * whose environment and recorded file disagree or are missing.
 */
export const scaleSite = (): string => process.env.SCALE_SITE ?? '(SCALE_SITE unset)';
export const scalePrincipalNames = (): string[] =>
  (process.env.SCALE_PRINCIPALS ?? '').split(/\s+/).filter(Boolean);

export function checkRunInputs(): ScaleExpected {
  const e = scaleExpected();
  expect(e.site, 'SCALE_SITE names the site the expected sets were recorded on').toBe(scaleSite());
  const names = scalePrincipalNames();
  expect(names.length, 'SCALE_PRINCIPALS names at least one principal').toBeGreaterThan(0);
  for (const n of names) {
    expect(Object.keys(e.principals), `principal ${n} was recorded on ${e.site}`).toContain(n);
  }
  return e;
}

/** Sign in as a seeded user through a one-shot admin reset on this disposable instance. */
export async function signInAs(
  admin: APIRequestContext,
  baseURL: string,
  username: string,
  ref: number,
): Promise<APIRequestContext> {
  const reset = await admin.post(`/api/v1/admin/users/${ref}/password-reset`, {
    data: { reason: 'ui-scale: disposable CI stack' },
    headers: { 'Content-Type': 'application/json' },
  });
  expect(reset.status(), `one-shot password reset for ${username} (ref ${ref})`).toBe(200);
  let password: string | undefined = ((await reset.json()) as { temporary_password?: string })
    .temporary_password;
  expect(typeof password === 'string' && password.length > 0, 'the reset returned a password').toBe(true);

  const ctx = await pwRequest.newContext({
    baseURL,
    ignoreHTTPSErrors: true,
    storageState: { cookies: [], origins: [] },
  });
  const login = await ctx.post('/api/v1/auth/login', {
    data: { username, password },
    headers: { 'Content-Type': 'application/json' },
  });
  password = undefined;
  expect(login.status(), `sign-in as ${username}`).toBe(200);
  const me = await ctx.get('/api/v1/auth/me');
  expect(me.status(), `reading ${username} back`).toBe(200);
  expect(Number(((await me.json()) as { ref: number }).ref), `${username}'s session ref`).toBe(ref);
  return ctx;
}

export interface WalkPage {
  cursor: string | null;
  ids: string[];
  restricted: number;
  next: string | null;
}

export interface Walk {
  pages: WalkPage[];
  ids: string[];
}

/**
 * Follow `next_cursor` from no cursor until the server stops handing one out.
 *
 * Bounded: a cursor that never terminates (a keyset that fails to advance)
 * fails at `maxPages` instead of hanging the job.
 */
export async function walk(
  ctx: APIRequestContext,
  path: '/api/v1/posts' | '/api/v1/assets',
  params: Record<string, string>,
  limit: number,
  maxPages: number,
): Promise<Walk> {
  const pages: WalkPage[] = [];
  let cursor: string | null = null;
  for (;;) {
    if (pages.length >= maxPages) {
      throw new Error(`${path} limit=${limit}: still handing out a cursor after ${maxPages} pages`);
    }
    const q = new URLSearchParams(params);
    q.set('limit', String(limit));
    if (cursor) q.set('cursor', cursor);
    const r = await ctx.get(`${path}?${q.toString()}`);
    expect(r.status(), `${path} page ${pages.length + 1} (limit ${limit})`).toBe(200);
    const body = (await r.json()) as {
      items?: Array<{ id: string; restricted?: boolean }>;
      next_cursor?: string | null;
    };
    const items = body.items ?? [];
    const next = body.next_cursor ?? null;
    pages.push({
      cursor,
      ids: items.map((i) => i.id),
      restricted: items.filter((i) => i.restricted === true).length,
      next,
    });
    if (next === null) break;
    expect(next, `${path} page ${pages.length}: next_cursor must move`).not.toBe(cursor);
    cursor = next;
  }
  return { pages, ids: pages.flatMap((p) => p.ids) };
}

/** Shape checks every walk must pass, whatever the endpoint or caller. */
export function checkWalkShape(
  w: Walk,
  label: string,
  limit: number,
  opts: { fullNonFinalPages: boolean },
): { shortNonFinal: number } {
  expect(w.pages.length, `${label}: at least one page`).toBeGreaterThan(0);
  const last = w.pages[w.pages.length - 1];
  expect(last.next, `${label}: the final page has no next_cursor`).toBeNull();
  let shortNonFinal = 0;
  w.pages.forEach((p, i) => {
    expect(p.ids.length, `${label}: page ${i + 1} holds at most ${limit} items`).toBeLessThanOrEqual(limit);
    if (i < w.pages.length - 1) {
      expect(p.next, `${label}: non-final page ${i + 1} carries a cursor`).not.toBeNull();
      if (p.ids.length < limit) shortNonFinal++;
      if (opts.fullNonFinalPages) {
        expect(p.ids.length, `${label}: non-final page ${i + 1} is a full page`).toBe(limit);
      }
    }
  });
  const dupes = w.ids.length - new Set(w.ids).size;
  expect(dupes, `${label}: no id is handed out twice`).toBe(0);
  return { shortNonFinal };
}

/** Compare two id sequences without printing thousands of UUIDs on failure. */
export function sequenceDiff(got: string[], want: string[]) {
  const g = new Set(got);
  const w = new Set(want);
  let firstMismatch = -1;
  for (let i = 0; i < Math.max(got.length, want.length); i++) {
    if (got[i] !== want[i]) {
      firstMismatch = i;
      break;
    }
  }
  return {
    length: got.length,
    expectedLength: want.length,
    missing: want.filter((x) => !g.has(x)).slice(0, 5),
    missingCount: want.filter((x) => !g.has(x)).length,
    unexpected: got.filter((x) => !w.has(x)).slice(0, 5),
    unexpectedCount: got.filter((x) => !w.has(x)).length,
    firstMismatch,
  };
}

export const SAME_SEQUENCE = (n: number) => ({
  length: n,
  expectedLength: n,
  missing: [],
  missingCount: 0,
  unexpected: [],
  unexpectedCount: 0,
  firstMismatch: -1,
});

/** Upper bound on pages for a walk over at most `rows` SQL rows. */
export const pageBound = (rows: number, limit: number) => Math.ceil(rows / limit) + 2;
