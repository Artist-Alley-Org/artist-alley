// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Theme preference: 'light' | 'dark' | 'system'.
//
// The early-script in app.html resolves the initial theme before
// hydration to avoid flash-of-wrong-theme. This module exposes runes-
// backed state so components can read + change the preference at
// runtime, and persists the choice in a cookie so the next page load
// applies it in app.html again.
//
// DEFAULT (#590): no cookie → 'dark'. Dark is the brand theme, so a
// first-time visitor gets it whatever their OS says. 'system' remains a
// deliberate user choice (offered in UserMenu / MobileNavDrawer /
// account preferences) for people who want OS-following, and an
// explicit 'light' or 'dark' always wins over everything.
//
// The resolution here MUST match app.html's pre-paint script exactly.
// If they disagree the page repaints after hydration — the flash that
// script exists to prevent.
//
// # Two stores, one preference (#677)
//
// The cookie is the DEVICE's copy and the account is the USER's copy,
// and they answer different questions. Only the cookie can be read
// before paint, so the cookie stays authoritative for what gets
// painted; the account is what makes a choice travel to a second
// browser. The reconciliation is therefore one-way at hydration and
// one-way at write:
//
//   init()  cookie present → the device has spoken; leave it alone.
//           cookie absent  → adopt the account's value and WRITE THE
//                            COOKIE, so the pre-paint script agrees
//                            from the next load onward.
//   set()   write the cookie, then mirror to the profile when signed
//           in — the same shape lang.set() uses for `language`.
//
// The cookie write in the absent case is the part that matters for
// flash. A static build cannot know the account theme before
// /auth/me answers, so the FIRST load on a new device paints the
// default and corrects itself once the session resolves. Writing the
// cookie there makes that a one-time event instead of every load
// forever, which is the difference between "it followed me here" and
// "this app flashes".
//
// Note the precedence is the opposite of lang.init(), which lets the
// profile outrank the cookie. That is deliberate and the difference is
// visibility: adopting the wrong language re-renders text you can
// re-read, while adopting the wrong theme repaints the whole page
// after it is already on screen.

import { browser } from '$app/environment';
import { api } from '$api/client';
import { auth } from '$stores/auth.svelte';

export type ThemePref = 'light' | 'dark' | 'system';
export type ThemeResolved = 'light' | 'dark';

const COOKIE = 'aa_theme';
const COOKIE_MAX_AGE = 60 * 60 * 24 * 365; // 1 year

/** No stored preference → dark. Mirrors app.html's pre-paint script. */
const DEFAULT_PREF: ThemePref = 'dark';

/** This device's explicit choice, or null when it has none.
 *
 *  An unrecognised cookie value reads as null — "nothing stored" —
 *  rather than silently becoming 'system'; that mis-read is what made
 *  a light-OS visitor land on light.
 *
 *  Null vs DEFAULT_PREF is the distinction #677 needs. Both used to
 *  collapse into 'dark' here, which made "this device chose dark" and
 *  "this device never chose" the same answer — and the account value
 *  can only be allowed to seed the second one. */
function readCookie(): ThemePref | null {
  if (!browser) return null;
  const m = document.cookie.match(/(?:^|; )aa_theme=([^;]+)/);
  if (!m) return null;
  const v = decodeURIComponent(m[1]);
  return v === 'light' || v === 'dark' || v === 'system' ? v : null;
}

/** The account's stored preference, or null when it has none.
 *
 *  `''` is NOT 'system'. An empty string means the account has never
 *  recorded a theme, so it must fall through to the device default;
 *  'system' is a deliberate "follow my OS" that has to travel. The
 *  column carried only the first meaning until migration 00033, which
 *  is why an explicit "System" pick could not reach a second device. */
function readAccount(): ThemePref | null {
  const v = auth.user?.theme;
  return v === 'light' || v === 'dark' || v === 'system' ? v : null;
}

function writeCookie(v: ThemePref) {
  if (!browser) return;
  document.cookie = `${COOKIE}=${encodeURIComponent(v)}; Max-Age=${COOKIE_MAX_AGE}; Path=/; SameSite=Lax`;
}

function systemPrefers(): ThemeResolved {
  if (!browser) return 'dark';
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function applyToDom(r: ThemeResolved) {
  if (!browser) return;
  const root = document.documentElement;
  root.classList.toggle('dark', r === 'dark');
  root.classList.toggle('light', r === 'light');
  root.setAttribute('data-theme', r);
}

class ThemeState {
  pref = $state<ThemePref>(DEFAULT_PREF);
  resolved = $state<ThemeResolved>('dark');

  /** Resolve the preference and paint it.
   *
   *  Called from the root layout's onMount, which runs after
   *  +layout.ts's load has already populated the auth store — the same
   *  ordering guarantee lang.init() relies on. Nothing here awaits a
   *  network call, so there is no window in which the theme is
   *  unresolved. */
  init() {
    this.pref = readCookie() ?? DEFAULT_PREF;
    this.resolved = this.pref === 'system' ? systemPrefers() : this.pref;
    applyToDom(this.resolved);
    this.syncFromAccount();

    // React to system pref changes while pref is 'system'.
    if (browser) {
      const mql = window.matchMedia('(prefers-color-scheme: dark)');
      mql.addEventListener('change', () => {
        if (this.pref === 'system') {
          this.resolved = systemPrefers();
          applyToDom(this.resolved);
        }
      });
    }
  }

  /** Adopt the signed-in account's theme on a device that has none of
   *  its own, and persist it to the cookie so the pre-paint script
   *  agrees from the next load onward.
   *
   *  Also called when the session changes, which is the case init()
   *  alone cannot cover: the root layout mounts once, so a visitor who
   *  lands on /login and signs in there would otherwise keep the
   *  default theme until a full reload.
   *
   *  The two early returns are the precedence rule, and neither is
   *  optional. A cookie means the device has spoken and the account
   *  does not get to argue; no account value means there is nothing to
   *  adopt, and writing the built-in default into the cookie here
   *  would permanently mark this device as "chose dark" and block
   *  every future account preference. */
  syncFromAccount(): void {
    if (readCookie() !== null) return;
    const account = readAccount();
    if (account === null) return;
    this.pref = account;
    this.resolved = account === 'system' ? systemPrefers() : account;
    writeCookie(account);
    applyToDom(this.resolved);
  }

  /** Record an explicit choice: paint it, persist it to this device,
   *  and mirror it to the account so it follows the user to the next
   *  browser (#677).
   *
   *  The cookie write is synchronous and the PATCH is not awaited by
   *  the paint, because the cookie is what the next load reads. A
   *  failed mirror is soft: this device is already correct, and the
   *  next successful `set` re-sends it. Same contract as lang.set(). */
  set(pref: ThemePref) {
    this.pref = pref;
    this.resolved = pref === 'system' ? systemPrefers() : pref;
    writeCookie(pref);
    applyToDom(this.resolved);
    void this.persistToAccount(pref);
  }

  private async persistToAccount(pref: ThemePref): Promise<void> {
    const user = auth.user;
    if (!user) return;
    try {
      await api.PATCH('/users/{ref}', {
        params: { path: { ref: user.ref } },
        body: { theme: pref } as never,
      });
      // Keep the in-memory session in step so a later init() — or any
      // other reader of auth.user.theme — doesn't see the pre-change
      // value that /auth/me last reported.
      user.theme = pref;
    } catch {
      // Soft fail — the cookie + local state still drive the UI.
    }
  }
}

export const theme = new ThemeState();
