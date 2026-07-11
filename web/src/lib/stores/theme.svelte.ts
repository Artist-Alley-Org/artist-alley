// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Theme preference: 'light' | 'dark' | 'system'.
//
// The early-script in app.html resolves the initial theme before
// hydration to avoid flash-of-wrong-theme. This module exposes runes-
// backed state so components can read + change the preference at
// runtime, and persists the choice in a cookie so the next page load
// applies it in app.html again.

import { browser } from '$app/environment';

export type ThemePref = 'light' | 'dark' | 'system';
export type ThemeResolved = 'light' | 'dark';

const COOKIE = 'aa_theme';
const COOKIE_MAX_AGE = 60 * 60 * 24 * 365; // 1 year

function readCookie(): ThemePref {
  if (!browser) return 'system';
  const m = document.cookie.match(/(?:^|; )aa_theme=([^;]+)/);
  const v = m ? decodeURIComponent(m[1]) : 'system';
  return v === 'light' || v === 'dark' ? v : 'system';
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
  pref = $state<ThemePref>('system');
  resolved = $state<ThemeResolved>('dark');

  init() {
    this.pref = readCookie();
    this.resolved = this.pref === 'system' ? systemPrefers() : this.pref;
    applyToDom(this.resolved);

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

  set(pref: ThemePref) {
    this.pref = pref;
    this.resolved = pref === 'system' ? systemPrefers() : pref;
    writeCookie(pref);
    applyToDom(this.resolved);
  }
}

export const theme = new ThemeState();
