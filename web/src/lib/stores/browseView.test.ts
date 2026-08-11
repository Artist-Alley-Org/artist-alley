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

const ALL_MODES = ['grid', 'masonry', 'thumbnail', 'list', 'feed'] as const;

beforeEach(() => {
  localStorage.clear();
  // The store is a singleton, so the operator's enabled set leaks
  // between tests unless it is put back. Every test above this line
  // assumes all five are offered.
  browseView.setEnabledModes([...ALL_MODES]);
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

// Signing in is a client-side navigation: the root layout mounted on
// /login, so the store hydrated with no session to consult. The layout
// re-applies on session change, and this is that sequence.
//
// These tests would NOT have caught the bug review found. That one was
// in the Go handler — /auth/login returns the same `CurrentUser` schema
// as /auth/me from a different code path, and that path never joined
// the preferences row, so the session object the layout handed this
// store was genuinely empty. A store test supplies its own defaults
// and therefore constructs the very input the bug destroyed. The guard
// for it lives in app/internal/auth/account_prefs_session_test.go,
// where the observable is the response.
//
// They are still worth having: they pin the store half of the
// contract, which is that a late-arriving account is applied at all
// rather than being ignored because hydration already happened.
describe('an account arriving after hydration (sign-in)', () => {
  it('applies the seed without a reload', () => {
    rehydrate(null);
    expect(browseView.mode).toBe('grid');
    expect(browseView.filter).toBe('latest');

    // The session lands; the layout re-applies.
    browseView.applyAccountDefaults({
      browse_layout: 'masonry',
      home_tab: 'following',
      browse_sort: 'oldest',
    });

    expect(browseView.mode).toBe('masonry');
    expect(browseView.filter).toBe('following');
    expect(browseView.feedDir).toBe('asc');
  });

  it('still does not write the late seed to localStorage', () => {
    rehydrate(null);
    browseView.applyAccountDefaults({ browse_layout: 'masonry' });
    expect(localStorage.getItem('aa_browse_mode')).toBeNull();
  });

  it('does not overwrite a choice made before signing in', () => {
    // Browsing as a guest on a public install, picking a layout, and
    // then signing in must not yank the view out from under them.
    rehydrate(null);
    browseView.setMode('thumbnail');

    browseView.applyAccountDefaults({ browse_layout: 'masonry' });
    expect(browseView.mode).toBe('thumbnail');
  });
});

// #709 — the operator's enabled set is the OUTERMOST rung, and being
// outermost is the whole difficulty: each of the three rungs below it
// can hold a mode the operator has since disabled, and each one has to
// fall through rather than be honoured.
//
// One test per rung, because they fail independently. An implementation
// that filters localStorage but not the account default passes a single
// combined test whenever the device happens to have an opinion.
describe('the operator can disable a layout (#709)', () => {
  it('falls through a DEVICE choice naming a disabled layout', () => {
    // The case that motivated the feature: the operator turns masonry
    // off, and a user whose browser still remembers it must not land on
    // an empty page.
    localStorage.setItem('aa_browse_mode', 'masonry');
    browseView.setEnabledModes(['grid', 'list']);
    rehydrate(null);

    expect(browseView.mode).toBe('grid');
  });

  it('falls through an ACCOUNT default naming a disabled layout', () => {
    browseView.setEnabledModes(['grid', 'list']);
    rehydrate({ browse_layout: 'masonry' });

    expect(browseView.mode).toBe('grid');
  });

  it('falls through the COARSE-POINTER default when feed is disabled', () => {
    // Phones default to `feed`. An operator who disables it has to get
    // a real layout on a phone, not the built-in that no longer exists
    // on this install.
    const original = window.matchMedia;
    window.matchMedia = ((q: string) => ({
      matches: q === '(pointer: coarse)',
      media: q,
      addEventListener() {},
      removeEventListener() {},
    })) as unknown as typeof window.matchMedia;
    try {
      browseView.setEnabledModes(['grid', 'list']);
      rehydrate(null);
      expect(browseView.mode).toBe('grid');
    } finally {
      window.matchMedia = original;
    }
  });

  it('falls back to the first ENABLED layout, not to `grid`', () => {
    // An operator can disable the built-in default too. Falling back to
    // `grid` regardless would put every user with no stored choice on
    // the one layout the install refuses to render.
    browseView.setEnabledModes(['list', 'feed']);
    rehydrate(null);

    expect(browseView.mode).toBe('list');
  });

  it('keeps a device choice the operator still offers', () => {
    // The filter must not become "reset everyone on every boot".
    localStorage.setItem('aa_browse_mode', 'list');
    browseView.setEnabledModes(['grid', 'list']);
    rehydrate({ browse_layout: 'grid' });

    expect(browseView.mode).toBe('list');
  });

  it('re-resolves when the enabled set arrives AFTER hydration', () => {
    // The real sequence: the store hydrates from localStorage, then the
    // public boot fetch lands. Without a re-resolve the user sits on a
    // layout the switcher no longer draws.
    localStorage.setItem('aa_browse_mode', 'masonry');
    rehydrate(null);
    expect(browseView.mode).toBe('masonry');

    browseView.setEnabledModes(['grid', 'list']);
    expect(browseView.mode).toBe('grid');
  });

  it('refuses setMode for a disabled layout, and does not persist it', () => {
    browseView.setEnabledModes(['grid', 'list']);
    rehydrate(null);

    browseView.setMode('masonry');

    expect(browseView.mode).toBe('grid');
    expect(localStorage.getItem('aa_browse_mode')).toBeNull();
  });

  it('ignores an empty enabled set rather than blacking out browse', () => {
    // The server refuses to store one, so an empty set here means a
    // stale cache or a mangled response. Honouring it would leave the
    // switcher with no buttons.
    browseView.setEnabledModes(['grid', 'list']);
    browseView.setEnabledModes([]);

    expect(browseView.enabledModes).toEqual(['grid', 'list']);
    expect(browseView.isEnabled('grid')).toBe(true);
  });

  it('normalises the enabled set to render order', () => {
    // What the switcher draws is this build's order, not whatever order
    // the operator's checkboxes serialised in.
    browseView.setEnabledModes(['feed', 'grid']);
    expect(browseView.enabledModes).toEqual(['grid', 'feed']);
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
