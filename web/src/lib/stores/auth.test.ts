// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Auth store capability-gate tests — pinning canSeeTile's contract so
// the #399 regression (cap-less help/about tiles hidden from read-cap
// operators) can't come back. We drive the runtime singleton directly
// by setting `caps`, the same field refresh() populates from the
// server.

import { afterEach, describe, expect, it } from 'vitest';
import { auth } from './auth.svelte';

afterEach(() => {
  // Reset shared singleton state between cases.
  auth.caps = [];
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
    });
    expect(auth.user?.username).toBe('admin');
    expect(auth.caps).toEqual(['system.admin']);
    expect(auth.canSeeAdmin).toBe(true);
  });

  it('falls back to holding nothing when the server omits the field', () => {
    auth.caps = ['system.admin'];
    auth.hydrateFrom({ ref: 2, username: 'nobody', auth_method: 'session' });
    // Not merely "unchanged": a stale set from a previous identity
    // would be a capability the current user does not hold.
    expect(auth.caps).toEqual([]);
    expect(auth.canSeeAdmin).toBe(false);
  });

  it('ignores a malformed capabilities value rather than trusting it', () => {
    auth.hydrateFrom({
      ref: 3,
      username: 'weird',
      auth_method: 'session',
      capabilities: ['users.read', 7, null, 'system.admin'],
    });
    expect(auth.caps).toEqual(['users.read', 'system.admin']);
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
