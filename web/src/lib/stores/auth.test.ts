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
