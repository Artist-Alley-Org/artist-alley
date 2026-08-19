// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// A throwaway account that is created ONCE per instance and reused
// thereafter (#1198).
//
// The specs that need a second principal — an explicit-share grantee —
// used to register `<prefix>_${Date.now()}` in `beforeAll`, and nothing
// ever removed it: there is no user-delete endpoint, and archiving does
// not take a row off /admin/users. Two accounts per suite run, forever.
//
// That is not merely untidy. `/admin/users` pages at limit 50, newest
// first, so after ~25 runs the BOOTSTRAP ADMIN is no longer on page 1 —
// and three specs that went looking for it there started failing on
// every local run while CI, which builds a fresh database each time,
// stayed green. A suite whose result depends on how many times it has
// been run before is not measuring the branch.
//
// So the username is a CONSTANT, not a stamp. First run on an instance
// creates it; every run after signs in as it. The instance gains two
// accounts once, and the second run of the suite sees exactly what the
// first one did — which is the property the accumulation broke.
//
// What still carries a stamp is everything the tests ASSERT on: post
// titles, collection names. Those are per-run by design, so a reused
// account's leftovers from an earlier run can never satisfy this run's
// assertion.

import { expect, type APIRequestContext, type Browser } from '@playwright/test';
import { LOGGED_OUT } from './auth';

export interface FixtureUserSpec {
  username: string;
  password: string;
  fullName: string;
}

export interface FixtureUserResult {
  /** The account's stable user ref. */
  ref: number;
  /**
   * The instance's `self_registration` block as it was found, and ONLY
   * when this call had to change it to create the account. `undefined`
   * means nothing was touched and the caller must restore nothing —
   * which is the normal case after the first run.
   */
  priorSelfRegistration?: unknown;
}

async function json(r: { json(): Promise<unknown> }): Promise<Record<string, unknown>> {
  return (await r.json()) as Record<string, unknown>;
}

/**
 * Resolve the fixture account, creating it only if this instance has
 * never seen it.
 *
 * `request` must be the admin context (it reads and writes the auth
 * system config). Registration happens on a SEPARATE anonymous context,
 * because registering signs the caller in and doing that on the admin's
 * context would swap the admin session out from under the rest of setup.
 */
export async function ensureFixtureUser(
  browser: Browser,
  request: APIRequestContext,
  user: FixtureUserSpec,
): Promise<FixtureUserResult> {
  // Already present from an earlier run? Sign in and read the ref back.
  // This branch is the whole point: it is what makes run N+1 identical
  // to run N instead of two accounts heavier.
  const probe = await browser.newContext({ storageState: LOGGED_OUT });
  try {
    const login = await probe.request.post('/api/v1/auth/login', {
      data: { username: user.username, password: user.password },
      headers: { 'Content-Type': 'application/json' },
    });
    if (login.ok()) {
      const me = await probe.request.get('/api/v1/auth/me');
      expect(me.status(), 'reading the reused fixture account back').toBe(200);
      const ref = Number((await json(me)).ref);
      expect(
        Number.isFinite(ref) && ref > 0,
        `the reused fixture account "${user.username}" has no usable ref`,
      ).toBe(true);
      return { ref };
    }
  } finally {
    await probe.close();
  }

  // First run on this instance. Self-registration may be off, so turn it
  // on for the one call and hand the caller back what it was, to restore
  // in afterAll — leaving an instance with open registration behind
  // would be a real change to the box.
  const authCfg = await request.get('/api/v1/admin/system/auth');
  expect(authCfg.status(), 'admin auth config must be readable').toBe(200);
  const priorSelfRegistration = (await json(authCfg)).self_registration;

  const enable = await request.patch('/api/v1/admin/system/auth', {
    data: {
      self_registration: {
        enabled: true,
        require_email_verification: false,
        default_role: 'Base',
      },
    },
  });
  expect(enable.status(), 'enabling self-registration').toBeLessThan(400);

  const anon = await browser.newContext({ storageState: LOGGED_OUT });
  try {
    const reg = await anon.request.post('/api/v1/auth/register', {
      data: {
        username: user.username,
        email: `${user.username}@example.test`,
        password: user.password,
        full_name: user.fullName,
      },
    });
    expect(reg.status(), `registering the fixture account "${user.username}"`).toBeLessThan(400);
    const ref = Number((await json(reg)).ref);
    expect(Number.isFinite(ref) && ref > 0).toBe(true);
    return { ref, priorSelfRegistration };
  } finally {
    await anon.close();
  }
}

/**
 * Put `self_registration` back, but only if {@link ensureFixtureUser}
 * actually changed it. Safe to call unconditionally from `afterAll`.
 */
export async function restoreSelfRegistration(
  request: APIRequestContext,
  prior: unknown,
): Promise<void> {
  if (prior === undefined) return;
  await request
    .patch('/api/v1/admin/system/auth', { data: { self_registration: prior } })
    .catch(() => undefined);
}
