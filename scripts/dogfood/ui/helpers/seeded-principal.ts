// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The principals the SEED owns, which four specs sign in as (#1270).
//
// # What this replaces, and why it had to change
//
// This was `ensureFixtureUser`: the spec registered a constant-username
// account on first sight of an instance, toggling `self_registration` on
// and straight back to do it. Constant rather than stamped was already
// the fix for one bug (#1198) — a per-run account is never removed,
// because THERE IS NO USER-DELETE ENDPOINT and archiving does not take a
// row off /admin/users, so after ~25 runs the bootstrap admin fell off
// page 1 and three specs started failing for reasons unrelated to the
// branch.
//
// But constant only makes run N+1 cheap. Run 1 still creates four
// accounts, and a FRESH database is run 1 every time — which is what CI
// is, and which is why the suite-level corpus census (#1245) could not
// be turned on there without reddening every run (#1263). The accounts
// are seeded now; a run creates nothing.
//
// # It resolves, and it never creates
//
// ⛔ THE FAILURE MODE THIS FILE EXISTS TO AVOID IS A SILENT FALLBACK. A
// helper that registered the account when sign-in failed would work
// perfectly, forever, and quietly put the leak back on every instance
// that had not been seeded with `--fixtures`. So there is no create arm:
// a missing principal fails the run and says what to run.
//
// # One home for the credentials
//
// The username/password pairs are read out of the seed catalogue the
// seeder itself reads, so there is nothing to keep in sync. A password
// edited in one place cannot leave the other behind.

import fs from 'node:fs';
import path from 'node:path';
import { expect, type Browser } from '@playwright/test';
import { LOGGED_OUT } from './auth';

/** Resolved from THIS FILE rather than Playwright's cwd, so a run
 *  started from somewhere other than scripts/dogfood/ui/ still finds
 *  it. */
const CATALOGUE = path.resolve(__dirname, '../../../../seed/profiles/dataset.fixtures.json');

export interface SeededPrincipal {
  username: string;
  password: string;
  full_name?: string;
  consumed_by?: string;
  why?: string;
}

interface FixtureCatalogue {
  principals: SeededPrincipal[];
}

function catalogue(): FixtureCatalogue {
  return JSON.parse(fs.readFileSync(CATALOGUE, 'utf-8')) as FixtureCatalogue;
}

/**
 * The principal the seed created under `username`.
 *
 * Reading it out of the catalogue rather than repeating the credentials
 * in the spec is the point: the seeder reads the same file.
 */
export function seededPrincipal(username: string): SeededPrincipal {
  const found = catalogue().principals.find((p) => p.username === username);
  if (!found) {
    throw new Error(
      `no principal "${username}" in ${CATALOGUE}. That file is the fixture list — ` +
        `add it there so \`aa seed --fixtures\` creates it, rather than registering ` +
        `it from a spec (#1270).`,
    );
  }
  return found;
}

/**
 * Sign in as a seeded principal and return its user ref.
 *
 * ⛔ NEVER CREATES. On an instance seeded without `--fixtures` this
 * fails, loudly, naming the command — which is the whole difference
 * between this and what it replaced.
 */
export async function requireSeededPrincipal(
  browser: Browser,
  username: string,
): Promise<number> {
  const user = seededPrincipal(username);
  const probe = await browser.newContext({ storageState: LOGGED_OUT });
  try {
    const login = await probe.request.post('/api/v1/auth/login', {
      data: { username: user.username, password: user.password },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(
      login.ok(),
      `could not sign in as the seeded principal "${user.username}" ` +
        `(${login.status()}). This instance was seeded without the test substrate. ` +
        `Re-seed with:\n\n` +
        `    aa seed --site <site> --catalogue seed/profiles --fixtures\n\n` +
        `⛔ The spec deliberately does NOT register the account instead: doing that ` +
        `on a fresh database is the per-run corpus leak #1270 removed, and there is ` +
        `no user-delete endpoint to undo it.`,
    ).toBe(true);

    const me = await probe.request.get('/api/v1/auth/me');
    expect(me.status(), `reading "${user.username}" back`).toBe(200);
    const ref = Number(((await me.json()) as Record<string, unknown>).ref);
    expect(
      Number.isFinite(ref) && ref > 0,
      `the seeded principal "${user.username}" has no usable ref`,
    ).toBe(true);
    return ref;
  } finally {
    await probe.close();
  }
}
