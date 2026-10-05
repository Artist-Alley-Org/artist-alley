// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The post-publish head merge (#1407).
//
// The UI regression under scripts/dogfood/ui/tests/standalone proves the
// artist sees their work without reloading. These pin the arithmetic
// that regression cannot reach without paging a real wall: what happens
// to a list the reader has already accumulated pages of.
//
// Each case is one of the two bugs the naive fixes have. "Assign the
// fresh page" loses the tail; "push the new row on the end" duplicates
// it and puts it in the wrong place. The merge has to do neither.

import { describe, expect, it } from 'vitest';
import { appendWithoutRepeats, mergeRefreshedHead } from './refreshHead';

interface Row {
  id: string;
}
const rows = (...ids: string[]): Row[] => ids.map((id) => ({ id }));
const ids = (list: Row[]): string[] => list.map((r) => r.id);

describe('mergeRefreshedHead', () => {
  it('keeps pages the reader accumulated past the first one', () => {
    // Three pages of two on screen; the server answers page one with a
    // new row at the head. Assigning the response would leave the
    // reader holding two rows out of seven.
    const onScreen = rows('a', 'b', 'c', 'd', 'e', 'f');
    const fresh = rows('new', 'a');
    expect(ids(mergeRefreshedHead(onScreen, fresh))).toEqual([
      'new',
      'a',
      'b',
      'c',
      'd',
      'e',
      'f',
    ]);
  });

  it('shows a row pushed off page one exactly once, in its new place', () => {
    // `b` was the tail of page one and the arrival displaced it. It is
    // absent from the refreshed page one and present in what is already
    // loaded, which is the case a plain concatenation double-counts.
    const onScreen = rows('a', 'b', 'c', 'd');
    const fresh = rows('new', 'a');
    const merged = mergeRefreshedHead(onScreen, fresh);
    expect(ids(merged)).toEqual(['new', 'a', 'b', 'c', 'd']);
    expect(ids(merged).filter((id) => id === 'b')).toHaveLength(1);
  });

  it('adds every row of a multi-file publish exactly once', () => {
    // N >= 2, the one-post-per-file case. Three posts land together and
    // the server returns all three at the head of page one.
    const onScreen = rows('a', 'b', 'c');
    const fresh = rows('n1', 'n2', 'n3', 'a', 'b');
    const merged = mergeRefreshedHead(onScreen, fresh);
    expect(ids(merged)).toEqual(['n1', 'n2', 'n3', 'a', 'b', 'c']);
    expect(new Set(ids(merged)).size).toBe(merged.length);
  });

  it('is idempotent, so two refreshes cannot double a row', () => {
    // The modal can be opened and submitted twice, and a surface may be
    // refreshed by a publish that changed nothing it shows.
    const onScreen = rows('a', 'b', 'c');
    const fresh = rows('new', 'a', 'b');
    const once = mergeRefreshedHead(onScreen, fresh);
    const twice = mergeRefreshedHead(once, fresh);
    expect(ids(twice)).toEqual(ids(once));
  });

  it('leaves the list alone when the fresh page holds nothing new', () => {
    // Oldest-first, or a narrowed feed the new post does not match:
    // page one genuinely does not contain it, and inventing a position
    // for it would be the out-of-order append this exists to refuse.
    const onScreen = rows('a', 'b', 'c', 'd');
    const fresh = rows('a', 'b');
    expect(ids(mergeRefreshedHead(onScreen, fresh))).toEqual(['a', 'b', 'c', 'd']);
  });

  it('does not resurrect a row the server stopped returning', () => {
    // A refresh must not decide membership. Rows the reader holds
    // beyond page one are kept because nothing here knows they are
    // gone, but the refreshed page is the server's verbatim answer:
    // `b` is not re-added ahead of `c` on the strength of the old list.
    const onScreen = rows('a', 'b', 'c');
    const fresh = rows('a', 'c');
    expect(ids(mergeRefreshedHead(onScreen, fresh))).toEqual(['a', 'c', 'b']);
  });

  it('takes the server representation, never the one already on screen', () => {
    // The store's row is not a Post and the server promotes fields the
    // client only guessed at. A merge that preferred the loaded copy
    // would pin the guess.
    const onScreen = [{ id: 'a', title: 'stale' }];
    const fresh = [{ id: 'a', title: 'server' }];
    expect(mergeRefreshedHead(onScreen, fresh)).toEqual([{ id: 'a', title: 'server' }]);
  });

  it('is a first load when nothing is on screen', () => {
    expect(ids(mergeRefreshedHead([], rows('a', 'b')))).toEqual(['a', 'b']);
  });

  // `/search` (#1407). Its rows come from three tables, so identity is
  // the pair `(type, id)` and not the uuid alone, which is what the
  // engine's own cursor tie-breaks on.
  describe('with a caller-supplied key', () => {
    interface Hit {
      type: string;
      id: string;
    }
    const key = (h: Hit) => `${h.type}:${h.id}`;
    const label = (list: Hit[]) => list.map(key);

    it('keeps two entities that happen to share a uuid apart', () => {
      // The failure a bare `id` key would produce: the refreshed asset
      // silently evicts the post the reader was already looking at.
      const onScreen: Hit[] = [
        { type: 'post', id: 'shared' },
        { type: 'collection', id: 'c1' },
      ];
      const fresh: Hit[] = [{ type: 'asset', id: 'shared' }];
      expect(label(mergeRefreshedHead(onScreen, fresh, key))).toEqual([
        'asset:shared',
        'post:shared',
        'collection:c1',
      ]);
    });

    it('still removes the overlap, matched on the pair', () => {
      const onScreen: Hit[] = [
        { type: 'asset', id: 'a' },
        { type: 'post', id: 'p' },
      ];
      const fresh: Hit[] = [
        { type: 'asset', id: 'new' },
        { type: 'asset', id: 'a' },
      ];
      expect(label(mergeRefreshedHead(onScreen, fresh, key))).toEqual([
        'asset:new',
        'asset:a',
        'post:p',
      ]);
    });
  });
});

// The tail-side twin (#1407). `/teams/{id}` has no generation guard, so
// its append composes from the list AS IT IS WHEN THE RESPONSE LANDS,
// which is not the list the request was issued against.
describe('appendWithoutRepeats', () => {
  interface Row {
    id: string;
  }
  const rows = (...ids: string[]): Row[] => ids.map((id) => ({ id }));
  const ids = (list: Row[]): string[] => list.map((r) => r.id);

  it('puts the incoming page on the end, in the server order', () => {
    expect(ids(appendWithoutRepeats(rows('a', 'b'), rows('c', 'd')))).toEqual([
      'a',
      'b',
      'c',
      'd',
    ]);
  });

  it('⛔ does NOT re-add a row a refresh already kept', () => {
    // The bug: a head refresh landed between the request and its
    // response and retained `b`, which this page is also about to
    // deliver. A bare concatenation shows it twice.
    const onScreen = rows('a', 'b');
    const page = rows('b', 'c');
    expect(ids(appendWithoutRepeats(onScreen, page))).toEqual(['a', 'b', 'c']);
  });

  it('leaves a repeated row where it already sits, rather than moving it', () => {
    const onScreen = rows('a', 'b', 'c');
    const page = rows('a', 'd');
    expect(ids(appendWithoutRepeats(onScreen, page))).toEqual(['a', 'b', 'c', 'd']);
  });

  it('is idempotent, so a page delivered twice adds it once', () => {
    const onScreen = rows('a', 'b');
    const page = rows('c', 'd');
    const once = appendWithoutRepeats(onScreen, page);
    expect(ids(appendWithoutRepeats(once, page))).toEqual(ids(once));
  });
});
