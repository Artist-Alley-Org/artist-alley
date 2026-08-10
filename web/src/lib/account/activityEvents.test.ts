// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The catalogue and the copy must agree, in BOTH directions (#600).
//
// activityEventKey builds an i18n key from an event type, and `t()`
// falls back to the key string when a lookup misses — so a type listed
// here without copy in en.json renders
// `account.activity.event.foo.bar.by_me` into the page, visibly. The
// list is the guard that stops an unknown type reaching the lookup at
// all; this test is the guard that stops a KNOWN one reaching it
// without copy behind it.
//
// The reverse direction matters too: copy with no type in the list is
// dead string, and dead strings are how a catalogue rots into something
// nobody trusts to be current.

import { describe, expect, it } from 'vitest';

import enDict from '$lib/i18n/en.json';
import { KNOWN_ACTIVITY_EVENTS, activityEventKey } from './activityEvents';

const ROLES = ['by_me', 'on_my_account'] as const;

const catalogue = (enDict as Record<string, any>).account?.activity?.event as
  | Record<string, Record<string, string>>
  | undefined;

describe('activity event copy', () => {
  it('en.json carries an account.activity.event catalogue', () => {
    expect(catalogue).toBeTypeOf('object');
  });

  it('every known event type has copy in both voices', () => {
    const missing: string[] = [];
    for (const type of KNOWN_ACTIVITY_EVENTS) {
      for (const role of ROLES) {
        const phrase = catalogue?.[type]?.[role];
        if (typeof phrase !== 'string' || phrase.trim() === '') {
          missing.push(`${type}.${role}`);
        }
      }
    }
    expect(missing).toEqual([]);
  });

  it('the catalogue carries no copy for types the module will never look up', () => {
    const known = new Set<string>(KNOWN_ACTIVITY_EVENTS);
    const orphans = Object.keys(catalogue ?? {}).filter((type) => !known.has(type));
    expect(orphans).toEqual([]);
  });

  it('builds the (type, role) key for a known type', () => {
    expect(activityEventKey('login.succeeded', 'by_me')).toBe(
      'account.activity.event.login.succeeded.by_me',
    );
    expect(activityEventKey('login.succeeded', 'on_my_account')).toBe(
      'account.activity.event.login.succeeded.on_my_account',
    );
  });

  // The whole point of the list: an unknown type must NOT produce a key,
  // because a key that misses is printed verbatim by t().
  it('returns null for a type it has no copy for', () => {
    expect(activityEventKey('some.event.that.does.not.exist', 'by_me')).toBeNull();
  });

  // The sentences are what the user reads instead of a payload, so they
  // have to actually read as sentences.
  it('every phrase is a plain sentence, not a key or a fragment', () => {
    const bad: string[] = [];
    for (const type of KNOWN_ACTIVITY_EVENTS) {
      for (const role of ROLES) {
        const phrase = catalogue?.[type]?.[role] ?? '';
        if (!/[.!?]$/.test(phrase) || phrase.includes('{') || phrase.includes('_')) {
          bad.push(`${type}.${role}: ${phrase}`);
        }
      }
    }
    expect(bad).toEqual([]);
  });
});
