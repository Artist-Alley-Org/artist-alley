// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Auth store capability-gate tests — pinning canSeeTile's contract so
// the #399 regression (cap-less help/about tiles hidden from read-cap
// operators) can't come back. We drive the runtime singleton directly
// by setting `caps`, the same field refresh() populates from the
// server.

import { afterEach, describe, expect, it } from 'vitest';
import { auth } from './auth.svelte';
import { lang } from './lang.svelte';
import { DEFAULT_LOCALE } from '$lib/i18n/locales';

afterEach(() => {
  // Reset shared singleton state between cases. `capsStatus` is part of
  // that state (#956) — leaving it 'unavailable' would make every later
  // case in this file gate on a degraded session and fail for a reason
  // that has nothing to do with what it is testing.
  auth.caps = [];
  auth.capsStatus = 'resolved';
  auth.user = null;
});

// #871 — the boot path. +layout.ts hands hydrateFrom the raw /auth/me
// body and then immediately flips `ready`, and the /admin gate decides
// on the very next frame. So hydrateFrom adopting the user WITHOUT the
// capabilities is not "capabilities arrive slightly later" — it is the
// gate answering "no permission" with the wrong inputs, in red, at a
// real administrator.
//
// This asserts the two land together off ONE body, which is the only
// shape that has no window in it. Note what it does not assert: that
// capabilities are correct — that is the server's business and
// app/internal/auth/session_capabilities_test.go pins it there.
describe('hydrateFrom (boot path)', () => {
  it('adopts capabilities from the same body as the user', () => {
    auth.hydrateFrom({
      ref: 1,
      username: 'admin',
      auth_method: 'session',
      capabilities: ['system.admin'],
      capabilities_status: 'resolved',
    });
    expect(auth.user?.username).toBe('admin');
    expect(auth.caps).toEqual(['system.admin']);
    expect(auth.capsUnavailable).toBe(false);
    expect(auth.canSeeAdmin).toBe(true);
  });

  it('falls back to holding nothing when a resolved body carries no capabilities', () => {
    auth.caps = ['system.admin'];
    auth.hydrateFrom({
      ref: 2,
      username: 'nobody',
      auth_method: 'session',
      capabilities_status: 'resolved',
    });
    // Not merely "unchanged": a stale set from a previous identity
    // would be a capability the current user does not hold.
    expect(auth.caps).toEqual([]);
    expect(auth.canSeeAdmin).toBe(false);
    // …and this account really does hold nothing, so the gate is
    // entitled to say so. That is the state the degraded one below must
    // stay distinguishable from.
    expect(auth.capsUnavailable).toBe(false);
  });

  it('ignores a malformed capabilities value rather than trusting it', () => {
    auth.hydrateFrom({
      ref: 3,
      username: 'weird',
      auth_method: 'session',
      capabilities: ['users.read', 7, null, 'system.admin'],
      capabilities_status: 'resolved',
    });
    expect(auth.caps).toEqual(['users.read', 'system.admin']);
  });
});

// #956 — "I could not determine your rights" and "you have none" are
// different answers, and the store is where they stop being the same
// one. Everything here asserts BOTH halves of the contract on every
// case: the degraded session must be distinguishable, and it must still
// grant absolutely nothing. A change that satisfied only the first half
// would be a privilege escalation dressed as a bug fix.
describe('capabilities_status (degraded lookup)', () => {
  it('reports unavailable when the server could not resolve the set', () => {
    auth.hydrateFrom({
      ref: 4,
      username: 'admin',
      auth_method: 'session',
      capabilities_status: 'unavailable',
    });
    expect(auth.capsUnavailable).toBe(true);
    // The user IS adopted — this is a signed-in session whose rights
    // are unknown, not an anonymous one. +layout.ts redirects the
    // latter to /login, so if this were null the degraded state could
    // never reach the admin gate at all.
    expect(auth.user?.username).toBe('admin');
  });

  it('grants nothing at all while unavailable', () => {
    auth.hydrateFrom({
      ref: 5,
      username: 'admin',
      auth_method: 'session',
      capabilities_status: 'unavailable',
    });
    expect(auth.canSeeAdmin).toBe(false);
    expect(auth.can('system.admin')).toBe(false);
    expect(auth.can('federation.read')).toBe(false);
    expect(auth.canSeeTile({})).toBe(false);
    expect(auth.canSeeTile({ cap: 'federation.read' })).toBe(false);
  });

  it('refuses even a capability list that arrived alongside unavailable', () => {
    // A body should never carry both, but if one does, the status wins.
    // Trusting the list would make `unavailable` a way to smuggle
    // capabilities past a state whose entire meaning is "this list is
    // not to be believed".
    auth.hydrateFrom({
      ref: 6,
      username: 'admin',
      auth_method: 'session',
      capabilities: ['system.admin'],
      capabilities_status: 'unavailable',
    });
    expect(auth.can('system.admin')).toBe(false);
    expect(auth.canSeeAdmin).toBe(false);
  });

  it('treats an absent or unrecognised status as unavailable, not as resolved', () => {
    // Fail-closed on BOTH axes: grant nothing, and do not claim the
    // account holds nothing either. Defaulting to 'resolved' would
    // restore the exact conflation this field exists to end, silently.
    auth.hydrateFrom({ ref: 7, username: 'admin', auth_method: 'session' });
    expect(auth.capsUnavailable).toBe(true);
    expect(auth.canSeeAdmin).toBe(false);

    auth.hydrateFrom({
      ref: 8,
      username: 'admin',
      auth_method: 'session',
      capabilities_status: 'RESOLVED',
    });
    expect(auth.capsUnavailable).toBe(true);
  });

  it('clears back to resolved on sign-out (nobody is a determination)', () => {
    auth.hydrateFrom({
      ref: 9,
      username: 'admin',
      auth_method: 'session',
      capabilities_status: 'unavailable',
    });
    auth.clear();
    expect(auth.capsUnavailable).toBe(false);
    expect(auth.caps).toEqual([]);
    expect(auth.user).toBeNull();
  });
});

describe('canSeeTile', () => {
  it('shows a public tile to everyone (help/docs/about are not sensitive)', () => {
    auth.caps = [];
    expect(auth.canSeeTile({ public: true })).toBe(true);
  });

  it('hides a cap-less, non-public tile from a non-admin (superuser-only #385)', () => {
    // The #399 regression to guard against: a blanket "no cap → visible"
    // would leak every unmigrated admin tile to any read-cap holder.
    auth.caps = ['system.jobs.read'];
    expect(auth.canSeeTile({})).toBe(false);
  });

  it('hides a capped tile from a user who lacks that capability', () => {
    auth.caps = [];
    expect(auth.canSeeTile({ cap: 'system.jobs.read' })).toBe(false);
  });

  it('shows a capped tile to a holder of exactly that capability', () => {
    auth.caps = ['system.jobs.read'];
    expect(auth.canSeeTile({ cap: 'system.jobs.read' })).toBe(true);
  });

  it('shows every tile to system.admin (wildcard short-circuit)', () => {
    auth.caps = ['system.admin'];
    expect(auth.canSeeTile({})).toBe(true);
    expect(auth.canSeeTile({ cap: 'system.jobs.read' })).toBe(true);
    expect(auth.canSeeTile({ public: true })).toBe(true);
  });
});

// #869 — the account's LANGUAGE is applied, not merely stored.
//
// The bug was narrow and easy to re-introduce: mapUser() copied
// `language` onto the user object and nothing called it. lang.init()
// read the profile too, but it runs once from the root layout's
// onMount — so a cold navigate was already right and signing in on
// /login was not, because by then init() had come and gone against a
// null user. Hanging the apply on adopt() is what makes the two agree,
// and these cases pin it to adopt() rather than to any one caller: a
// fourth path that publishes a user gets the behaviour for free, and a
// refactor that drops the call fails here.
describe('adopt applies the account language (#869)', () => {
  afterEach(() => {
    lang.pref = '';
    lang.resolved = DEFAULT_LOCALE;
  });

  it('applies a supported language off the session body', () => {
    auth.hydrateFrom({
      ref: 5, username: 'pierre', auth_method: 'session',
      capabilities: [], capabilities_status: 'resolved',
      language: 'fr',
    });
    expect(lang.resolved).toBe('fr');
    expect(lang.pref).toBe('fr');
  });

  it('resolves a regional tag down to its language', () => {
    // The server validates `language` by maxLength alone, so "fr-CA"
    // is a value an account can genuinely hold.
    auth.hydrateFrom({
      ref: 6, username: 'chantal', auth_method: 'session',
      capabilities: [], capabilities_status: 'resolved',
      language: 'fr-CA',
    });
    expect(lang.resolved).toBe('fr');
  });

  it('leaves the device preference alone when the account has none', () => {
    // Null/absent is the stored value for every account that never
    // picked one, and it means "follow this device" — so a sign-in
    // must not overwrite a locale the device already resolved.
    lang.pref = 'es';
    lang.resolved = 'es';
    auth.hydrateFrom({
      ref: 7, username: 'nolang', auth_method: 'session',
      capabilities: [], capabilities_status: 'resolved',
      language: null,
    });
    expect(lang.resolved).toBe('es');
    expect(lang.pref).toBe('es');
  });

  it('falls back to the default for an unrecognised tag', () => {
    auth.hydrateFrom({
      ref: 8, username: 'klaus', auth_method: 'session',
      capabilities: [], capabilities_status: 'resolved',
      language: 'de',
    });
    expect(lang.resolved).toBe(DEFAULT_LOCALE);
  });
});
