// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Theme precedence contract (#677).
//
// Same rule as browseView, one rung shorter:
//
//     cookie (this device) > account > built-in default (dark)
//
// The account rung is the new one. It is what makes a theme follow a
// user to a second browser, and the reason it sits BELOW the cookie
// rather than above it is flash: only the cookie can be read by
// app.html's pre-paint script, so anything that outranks the cookie
// repaints the page after hydration.
//
// The cookie write in the seeded case is therefore load-bearing and
// gets its own assertion. Without it the adoption would be re-derived
// on every single load, which means every single load starts on the
// wrong theme and corrects itself — the exact flash the pre-paint
// script exists to prevent, reintroduced through the back door.

import { beforeEach, describe, expect, it } from 'vitest';
import { theme } from './theme.svelte';
import { auth } from './auth.svelte';

function clearThemeCookie() {
  document.cookie = 'aa_theme=; Max-Age=0; Path=/';
}

function readThemeCookie(): string | null {
  const m = document.cookie.match(/(?:^|; )aa_theme=([^;]+)/);
  return m ? decodeURIComponent(m[1]) : null;
}

/** A session carrying an account-level theme, with nothing else the
 *  store reads. */
function signIn(theme: 'light' | 'dark' | 'system' | '' | null) {
  // matureContentAllowed is required on AuthUser (#1116): the store's
  // mapUser always supplies it, and making it required is what stops a
  // producer from leaving the field on `false` — which would mean "this
  // install forbids mature content" rather than "unknown".
  auth.user = { ref: 1, username: 'tester', theme, matureContentAllowed: true };
}

beforeEach(() => {
  clearThemeCookie();
  auth.user = null;
  theme.pref = 'dark';
  theme.resolved = 'dark';
});

describe('the account seeds a device with no cookie', () => {
  it('adopts an explicit light and paints it', () => {
    signIn('light');
    theme.init();

    expect(theme.pref).toBe('light');
    expect(theme.resolved).toBe('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('writes the cookie so the next load paints it before hydration', () => {
    signIn('light');
    theme.init();
    expect(readThemeCookie()).toBe('light');
  });

  it('carries `system` across as `system`, not as the default', () => {
    // The whole reason migration 00033 exists. Before it, an explicit
    // "follow my OS" was stored as '' — indistinguishable from never
    // having chosen — so it could not reach a second device at all.
    signIn('system');
    theme.init();
    expect(theme.pref).toBe('system');
  });

  it('treats an empty account theme as "no preference", not as system', () => {
    signIn('');
    theme.init();
    expect(theme.pref).toBe('dark');
    // And nothing is stamped into the cookie: this device has made no
    // choice and neither has the account, so a preference set later on
    // another device must still be able to reach it.
    expect(readThemeCookie()).toBeNull();
  });
});

describe('this device outranks the account', () => {
  it('keeps the cookie when the account says otherwise', () => {
    document.cookie = 'aa_theme=dark; Path=/';
    signIn('light');
    theme.init();

    expect(theme.pref).toBe('dark');
    expect(readThemeCookie()).toBe('dark');
  });

  it('leaves a signed-out visitor on the built-in default', () => {
    theme.init();
    expect(theme.pref).toBe('dark');
    expect(readThemeCookie()).toBeNull();
  });
});

describe('syncFromAccount after a sign-in', () => {
  it('adopts the account theme without a reload', () => {
    // The root layout mounts once, so a visitor who lands on /login
    // has already run init() with no session. Signing in is a
    // client-side navigation; this is what makes their theme appear.
    theme.init();
    expect(theme.pref).toBe('dark');

    signIn('light');
    theme.syncFromAccount();
    expect(theme.pref).toBe('light');
  });

  it('is a no-op once this device has chosen', () => {
    document.cookie = 'aa_theme=dark; Path=/';
    theme.init();
    signIn('light');
    theme.syncFromAccount();
    expect(theme.pref).toBe('dark');
  });
});
