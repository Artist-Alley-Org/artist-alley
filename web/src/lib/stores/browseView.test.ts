// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// browseView precedence contract (#706).
//
// The rule these tests exist to pin, in one line:
//
//     explicit local choice > account preference > built-in default
//
// It is the kind of rule that reads as obvious and is wrong in one
// direction or the other in most implementations. Getting it backwards
// gives you a laptop that resets to the account default on every
// reload; leaving the account rung out entirely is the bug #706 filed,
// where the preference was stored durably and read by nothing. Both
// directions are asserted below, because only testing the seeding half
// would pass an implementation that always overrides.
//
// The suite drives the real singleton with an explicit `defaults`
// argument rather than faking the auth store: `init(defaults)` takes
// one precisely so the precedence logic is reachable without a session.

import { beforeEach, describe, expect, it } from 'vitest';
import { browseView } from './browseView.svelte';

/** The store hydrates once per page load; tests need many. */
function rehydrate(defaults?: Parameters<typeof browseView.init>[0]) {
  browseView.hydrated = false;
  browseView.init(defaults);
}

beforeEach(() => {
  localStorage.clear();
});

describe('account defaults seed a device with no local choice', () => {
  it('adopts layout, tab and sort together', () => {
    rehydrate({ browse_layout: 'masonry', home_tab: 'following', browse_sort: 'oldest' });

    expect(browseView.mode).toBe('masonry');
    expect(browseView.filter).toBe('following');
    // One preference, both surfaces: the card feeds read `feedDir`,
    // the list-view table header reads `sort`.
    expect(browseView.feedDir).toBe('asc');
    expect(browseView.sort).toEqual({ col: 'posted_at', dir: 'asc' });
  });

  it('accepts `feed` as a layout — a phone default nobody could ask for before', () => {
    rehydrate({ browse_layout: 'feed' });
    expect(browseView.mode).toBe('feed');
  });

  it('does NOT write the seed to localStorage', () => {
    // Persisting it would promote "the account says masonry" into
    // "this device chose masonry", and the device would then outrank
    // every future account change — silently, and forever.
    rehydrate({ browse_layout: 'masonry', home_tab: 'following', browse_sort: 'oldest' });

    expect(localStorage.getItem('aa_browse_mode')).toBeNull();
    expect(localStorage.getItem('aa_browse_filter')).toBeNull();
    expect(localStorage.getItem('aa_browse_feed_dir')).toBeNull();
    expect(localStorage.getItem('aa_browse_list_sort')).toBeNull();
  });
});

describe('an explicit local choice outranks the account', () => {
  it('keeps the stored layout when the account names a different one', () => {
    localStorage.setItem('aa_browse_mode', 'thumbnail');
    rehydrate({ browse_layout: 'masonry' });
    expect(browseView.mode).toBe('thumbnail');
  });

  it('keeps the stored tab and sort direction', () => {
    localStorage.setItem('aa_browse_filter', 'latest');
    localStorage.setItem('aa_browse_feed_dir', 'desc');
    rehydrate({ home_tab: 'following', browse_sort: 'oldest' });

    expect(browseView.filter).toBe('latest');
    expect(browseView.feedDir).toBe('desc');
  });

  it('survives a reload after the user changes the view on the page', () => {
    // The end-to-end shape of the rule: pick a mode while browsing,
    // reload, and the account default must not reclaim the device.
    rehydrate({ browse_layout: 'masonry' });
    expect(browseView.mode).toBe('masonry');

    browseView.setMode('list');
    rehydrate({ browse_layout: 'masonry' });
    expect(browseView.mode).toBe('list');
  });
});

describe('built-in defaults are the last rung', () => {
  it('applies when neither the device nor the account has an opinion', () => {
    rehydrate(null);
    expect(browseView.mode).toBe('grid');
    expect(browseView.filter).toBe('latest');
    expect(browseView.feedDir).toBe('desc');
    expect(browseView.sort).toEqual({ col: 'posted_at', dir: 'desc' });
  });

  it('ignores an account value this build cannot serve', () => {
    // A row stored before #706/#736 shrank the vocabulary. The server
    // sanitizes these out of GET /auth/me too; the store refuses them
    // independently, because a mode it cannot render would leave the
    // view switcher with no active segment.
    rehydrate({ browse_layout: 'carousel', home_tab: 'trending', browse_sort: 'popular' });

    expect(browseView.mode).toBe('grid');
    expect(browseView.filter).toBe('latest');
    expect(browseView.feedDir).toBe('desc');
  });
});

describe('a stale local value is not a local choice', () => {
  it('clears a removed feed filter and lets the account seed instead', () => {
    // `trending` was a real stored value before #691 removed it. It
    // must not sit in localStorage looking like a deliberate choice —
    // that would block the account preference on this device forever.
    localStorage.setItem('aa_browse_filter', 'trending');
    rehydrate({ home_tab: 'following' });

    expect(browseView.filter).toBe('following');
    expect(localStorage.getItem('aa_browse_filter')).toBeNull();
  });
});
