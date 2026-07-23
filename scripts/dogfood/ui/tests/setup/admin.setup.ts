// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Shared admin session for the standalone suite (#481, tail of #485).
//
// This runs ONCE as a project dependency before the standalone tests
// and writes the authenticated cookie jar to storageState. The
// standalone project loads it via `use.storageState`, so ~130 per-test
// login round-trips collapse into a single one. Those per-test logins
// were the auth-session races AND the connection load that surfaced as
// mid-suite ECONNREFUSED against the prod build at app.aa:8080 under CI
// load (the config's "Vite hiccups" note is stale for CI — CI never
// runs Vite; ui-pr.yml points STUDIO_A_HOST at the embedded prod build).
//
// The login FORM is still covered: ui-16 opts out of the shared state
// (storageState: undefined) and drives the real /login form + sign-out.

import { test as setup, expect } from '@playwright/test';
import { loginAsAdminViaAPI, ADMIN_STATE_PATH } from '../../helpers/auth';
import { waitForAppReady, withTransientRetry } from '../../helpers/ready';

setup('authenticate admin + pin site name once', async ({ request }) => {
  // This step gates the ENTIRE standalone suite (dependencies: ['setup']),
  // so a single startup blip here cascades into ~130 failures. Give it
  // headroom over the global 30s test timeout for the readiness poll +
  // retries, then (1) wait for the app to actually answer before the
  // first call and (2) wrap each raw API call so one transient
  // ECONNRESET / "context disposed" retries instead of reding the suite.
  setup.setTimeout(90_000);
  await waitForAppReady(request);

  await withTransientRetry('admin login', () => loginAsAdminViaAPI(request));

  // Pin the display name to "Artist Alley" (the frontend default the
  // brand assertions in ui-07 / ui-29 expect). A stack seeded with any
  // other name — the dogfood box is "Tx Site" — otherwise reds those
  // specs. The name rides the public /appearance boot fetch, sourced
  // from this admin config, so setting it here makes every subsequent
  // page render "Artist Alley". Read-modify-write to preserve base_url.
  const cur = await withTransientRetry('get site config', () =>
    request.get('/api/v1/admin/system/site'),
  );
  const body = cur.ok() ? await cur.json() : {};
  const patched = await withTransientRetry('set site name', () =>
    request.patch('/api/v1/admin/system/site', {
      data: { ...body, name: 'Artist Alley' },
      headers: { 'Content-Type': 'application/json' },
    }),
  );
  expect(patched.ok(), `set site name failed: HTTP ${patched.status()}`).toBeTruthy();

  // Pin the admin's UI language to English so the copy/title assertions
  // are deterministic regardless of what language the account was left
  // in (the dogfood admin is 'es' → "Administración …"). The en-US
  // browser locale (config) covers logged-out contexts; a signed-in
  // profile pref wins over the locale, so the admin needs this too.
  const me = await withTransientRetry('get me', () => request.get('/api/v1/auth/me'));
  if (me.ok()) {
    const ref = (await me.json())?.ref;
    if (ref != null) {
      await withTransientRetry('set admin language', () =>
        request.patch(`/api/v1/users/${ref}`, {
          data: { language: 'en' },
          headers: { 'Content-Type': 'application/json' },
        }),
      );
    }
  }

  await request.storageState({ path: ADMIN_STATE_PATH });
});
