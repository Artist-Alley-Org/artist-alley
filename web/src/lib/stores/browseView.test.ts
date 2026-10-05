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
import {
  browseView,
  COLUMN_MIN_PX,
  LIST_COLUMNS,
  columnMinPx,
} from './browseView.svelte';
import { auth } from './auth.svelte';

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

// ── Column widths (#1100) ───────────────────────────────────────────
//
// The contract worth pinning is not "a number round-trips". It is that
// WIDTH and VISIBILITY are two records, so hiding a column through the
// picker is not a way to lose the width you dragged it to — which is
// the bug the obvious implementation (one richer list) produces, and
// the one a user would meet within a minute of using both controls.

describe('list-view column widths', () => {
  const titleDef = LIST_COLUMNS.find((c) => c.id === 'title')!;

  it('persists a dragged width and reads it back on the next hydration', () => {
    rehydrate(null);
    browseView.setColumnWidth('title', 420);

    expect(browseView.columnTrack(titleDef)).toBe('420px');
    rehydrate(null);
    expect(browseView.columnWidths.title).toBe(420);
  });

  it('clamps to the column floor rather than refusing the drag', () => {
    // Parking at the minimum keeps the gesture alive: the pointer can
    // travel back out and the column follows. Refusing would freeze the
    // handle at the moment the user overshoots.
    rehydrate(null);
    browseView.setColumnWidth('title', 4);

    expect(browseView.columnWidths.title).toBe(COLUMN_MIN_PX);
  });

  it('honours a per-column floor below the global one', () => {
    // `thumbnail` draws a 32px square and nothing else, so the global
    // 80px floor would make the table's narrowest column the one that
    // cannot be narrowed.
    rehydrate(null);
    expect(columnMinPx('thumbnail')).toBeLessThan(COLUMN_MIN_PX);

    browseView.setColumnWidth('thumbnail', 10);
    expect(browseView.columnWidths.thumbnail).toBe(columnMinPx('thumbnail'));
  });

  it('falls back to the registry default when nothing is stored', () => {
    rehydrate(null);
    expect(browseView.columnTrack(titleDef)).toBe(titleDef.width);
  });

  it('keeps a dragged width across hide + re-show through the picker', () => {
    rehydrate(null);
    browseView.setColumnWidth('title', 500);

    browseView.toggleColumn('title');
    expect(browseView.listColumns).not.toContain('title');
    browseView.toggleColumn('title');

    expect(browseView.listColumns).toContain('title');
    expect(browseView.columnTrack(titleDef)).toBe('500px');
  });

  it('double-click reset drops the width back to the default', () => {
    rehydrate(null);
    browseView.setColumnWidth('title', 500);
    browseView.resetColumnWidth('title');

    expect(browseView.columnWidths.title).toBeUndefined();
    expect(browseView.columnTrack(titleDef)).toBe(titleDef.width);
  });

  it('"reset columns" resets BOTH halves', () => {
    // The default column set at whatever widths the last drag left is a
    // state the user cannot get out of except by dragging each column
    // back by hand.
    rehydrate(null);
    browseView.setColumnWidth('title', 500);
    browseView.toggleColumn('visibility');

    browseView.resetColumns();

    expect(browseView.columnWidths).toEqual({});
    expect(browseView.listColumns).not.toContain('visibility');
  });

  it('drops stored widths that cannot be honoured instead of repairing them', () => {
    // An unknown id, a sub-floor number and a non-number are all "not a
    // preference". Clamping the sub-floor one UP would put a width on
    // screen that nobody chose.
    localStorage.setItem(
      'aa_browse_list_col_widths',
      JSON.stringify({ title: 400, gone: 300, author: 4, tags: 'wide' }),
    );
    rehydrate(null);

    expect(browseView.columnWidths).toEqual({ title: 400 });
  });
});

// ── "Hide AI-made work" (#1251 slice 3, ADR 0094 fourth amendment) ───
//
// Three properties, and each one inverts silently — the toggle keeps
// rendering and the wall keeps filling whichever way they go:
//
//   1. OFF sends NOTHING, not `pure`. The two wire values partition the
//      corpus, so an off state that sent `pure` would show ONLY AI work
//      — the exact inverse of the control, from a one-word slip.
//   2. It PERSISTS across a hydration, because a preference that resets
//      on reload is one the reader has to set on every visit.
//   3. It FAILS TOWARD SHOWING when storage is unreadable. Every unknown
//      on this axis resolves toward showing (ADR 0094 §3): wrongly
//      hiding human work is the worse error.
describe('the AI toggle', () => {
  it('sends nothing when OFF and `not_pure` when ON — never `pure`', () => {
    rehydrate(null);
    expect(browseView.hideAI).toBe(false);
    expect(browseView.aiParam).toBeNull();

    browseView.setHideAI(true);
    expect(browseView.aiParam).toBe('not_pure');

    browseView.setHideAI(false);
    expect(browseView.aiParam).toBeNull();
  });

  it('survives a reload', () => {
    rehydrate(null);
    browseView.setHideAI(true);

    rehydrate(null);
    expect(browseView.hideAI).toBe(true);
    expect(browseView.aiParam).toBe('not_pure');
  });

  it('leaves no stored value behind when turned back off', () => {
    // "Off" is the default, so a stored `false` is a key that says
    // nothing — and one that makes "this device chose off" and "this
    // device never chose" indistinguishable.
    rehydrate(null);
    browseView.setHideAI(true);
    expect(localStorage.getItem('aa_browse_hide_ai')).toBe('1');

    browseView.setHideAI(false);
    expect(localStorage.getItem('aa_browse_hide_ai')).toBeNull();

    rehydrate(null);
    expect(browseView.hideAI).toBe(false);
  });

  it('renders OFF, not broken, when localStorage throws', () => {
    // A private window, a quota-exceeded profile, or a browser set to
    // block site data. The page must render the ordinary unfiltered
    // wall — never crash, and never silently filter.
    const real = Object.getOwnPropertyDescriptor(window, 'localStorage');
    const boom = () => { throw new DOMException('denied', 'SecurityError'); };
    Object.defineProperty(window, 'localStorage', {
      configurable: true,
      get() { return { getItem: boom, setItem: boom, removeItem: boom }; },
    });
    try {
      browseView.hydrated = false;
      expect(() => browseView.init(null)).not.toThrow();
      expect(browseView.hideAI).toBe(false);
      expect(browseView.aiParam).toBeNull();
      // And a WRITE against the same storage must not throw either —
      // the click handler has no catch of its own.
      expect(() => browseView.setHideAI(true)).not.toThrow();
      expect(browseView.hideAI).toBe(true);
    } finally {
      if (real) Object.defineProperty(window, 'localStorage', real);
      else delete (window as unknown as Record<string, unknown>).localStorage;
    }
  });

  it('ignores a stored value that is not the one it writes', () => {
    // Anything but `'1'` is not a choice this build made. It resolves
    // toward SHOWING rather than being repaired into a preference.
    for (const junk of ['true', 'yes', '0', 'not_pure', '']) {
      localStorage.setItem('aa_browse_hide_ai', junk);
      rehydrate(null);
      expect(browseView.hideAI, `stored ${JSON.stringify(junk)}`).toBe(false);
    }
  });
});

// #1292: the CONTENT category's Mature row, ADR 0090's LAYER 3.
//
// ⭐ THE CASCADE IS PART OF THE MAPPING, and that is the half these
// cases exist for. The flag is stored per DEVICE and the two rungs
// above it are answered per SESSION, so the interesting states are the
// ones where they disagree: a reader who narrowed their feed and then
// opted out, signed out, or moved to an install with the feature off.
// A `matureParam` that read only the flag would go on filtering for all
// three, with no row left in the menu to turn it off: invisible state,
// and a wall that stays narrow for a reason nothing on screen explains.
//
// ⛔ AND IT CANNOT WIDEN. There is no value of the control that adds a
// row: `matureParam` returns `'not_mature'` or nothing, so the only
// thing a reader can express is a subtraction from what the three
// conjuncts already allowed.
describe('the mature view filter', () => {
  /** A signed-in reader with the two cascade rungs set. Nothing else on
   *  AuthUser matters here, and the two required mature fields are
   *  spelled out at every call so a case cannot lean on a default. */
  function signIn(allowed: boolean, optedIn: boolean, exempt = false) {
    auth.user = {
      ref: 1,
      username: 'reader',
      matureContentAllowed: allowed,
      matureOptedIn: optedIn,
    };
    // The MODERATION EXEMPTION (ADR 0090 §2), spelled the way the store
    // reads it: `auth.can('system.admin')`. It mirrors the server's
    // `MatureAdmin: caller.Can(auth.SuperAdminCapability)`, so a case
    // that set some other capability would not be testing the
    // exemption at all.
    auth.caps = exempt ? ['system.admin'] : [];
    auth.capsStatus = 'resolved';
  }

  beforeEach(() => {
    auth.user = null;
    // The store is a singleton and `caps` is not cleared by signing
    // out, so an exempt case would leak into every test after it.
    auth.caps = [];
    auth.capsStatus = 'resolved';
  });

  it('is not offered to a signed-out reader, and sends nothing', () => {
    // An anonymous viewer can never opt in, because there is nowhere to
    // store the answer (ADR 0090 §2), so there is no consent for a view
    // filter to narrow. It fails both rungs by construction rather than
    // by a third check.
    rehydrate(null);
    browseView.setHideMature(true);
    expect(browseView.matureFilterAvailable).toBe(false);
    expect(browseView.matureParam).toBeNull();
  });

  it('is not offered when the INSTANCE forbids mature content', () => {
    signIn(false, true);
    rehydrate(null);
    browseView.setHideMature(true);
    expect(browseView.matureFilterAvailable).toBe(false);
    expect(browseView.matureParam).toBeNull();
  });

  it('is not offered to an UNEXEMPT reader who has not opted in', () => {
    // Rung 2, still absence for the reader it was written about: a
    // control meaning "leave mature out of these results" is
    // meaningless to a reader who was never going to be shown any.
    //
    // ⚠️ THE QUALIFIER IS THE WHOLE POINT SINCE #1345. This case is
    // about a reader who can neither consent-in nor receive rows by
    // exemption; the class below it differs on exactly one of those
    // and gets the opposite answer.
    signIn(true, false);
    rehydrate(null);
    browseView.setHideMature(true);
    expect(browseView.matureFilterAvailable).toBe(false);
    expect(browseView.matureParam).toBeNull();
  });

  it('⭐ defaults to INCLUDED for a reader who has opted in', () => {
    // ADR 0090's amendment states this as a requirement: layer 3
    // defaults to included, so shipping it changed nothing for a reader
    // who had already consented.
    signIn(true, true);
    rehydrate(null);
    expect(browseView.matureFilterAvailable).toBe(true);
    expect(browseView.hideMature).toBe(false);
    expect(browseView.matureParam).toBeNull();
  });

  it('sends `not_mature` once narrowed, and nothing again once cleared', () => {
    signIn(true, true);
    rehydrate(null);

    browseView.setHideMature(true);
    expect(browseView.matureParam).toBe('not_mature');

    browseView.setHideMature(false);
    expect(browseView.matureParam).toBeNull();
  });

  it('survives a reload', () => {
    signIn(true, true);
    rehydrate(null);
    browseView.setHideMature(true);

    rehydrate(null);
    expect(browseView.hideMature).toBe(true);
    expect(browseView.matureParam).toBe('not_mature');
  });

  it('⭐ stops sending the moment the reader stops qualifying', () => {
    // The device keeps its flag; the request must not keep the filter.
    // This is the case a `matureParam` reading only the flag passes
    // every other test in this block and fails here.
    signIn(true, true);
    rehydrate(null);
    browseView.setHideMature(true);
    expect(browseView.matureParam).toBe('not_mature');

    signIn(true, false);
    expect(browseView.hideMature, 'the device keeps its answer').toBe(true);
    expect(browseView.matureParam, 'but the request must stop carrying it').toBeNull();

    auth.user = null;
    expect(browseView.matureParam).toBeNull();
  });

  it('⛔ NEVER writes the account preference', () => {
    // Layer 2 is the consent and lives on `user_preferences`. Layer 3
    // may only narrow what that consent allowed, so this setter touches
    // one localStorage key and nothing else.
    signIn(true, true);
    rehydrate(null);
    browseView.setHideMature(true);
    expect(auth.user?.matureOptedIn, 'the consent must be untouched').toBe(true);
    expect(localStorage.getItem('aa_browse_hide_mature')).toBe('1');

    browseView.setHideMature(false);
    // ⚠️ IT STORES `0` RATHER THAN REMOVING THE KEY, since #1345.
    // readHideAI's "a stored false is a key that says nothing" argument
    // holds only while the axis has ONE default. This one has two, so a
    // removed key would erase an exempt reader's deliberate include and
    // let their class default re-narrow the wall on reload.
    expect(localStorage.getItem('aa_browse_hide_mature')).toBe('0');
    expect(auth.user?.matureOptedIn, 'and the consent is still untouched').toBe(true);
  });

  it('ignores a stored value that is not one of the two it writes', () => {
    // ⚠️ `'0'` IS NO LONGER JUNK. Since #1345 the key is tri-state and
    // `0` is an explicit include, so it is asserted as a value in the
    // nine-case block below rather than discarded here.
    signIn(true, true);
    for (const junk of ['true', 'yes', 'not_mature', '']) {
      localStorage.setItem('aa_browse_hide_mature', junk);
      rehydrate(null);
      expect(browseView.hideMature, `stored ${JSON.stringify(junk)}`).toBe(false);
      expect(browseView.matureParam).toBeNull();
    }
  });

  it('renders INCLUDED, not broken, when localStorage throws', () => {
    signIn(true, true);
    const real = Object.getOwnPropertyDescriptor(window, 'localStorage');
    const boom = () => { throw new DOMException('denied', 'SecurityError'); };
    Object.defineProperty(window, 'localStorage', {
      configurable: true,
      get() { return { getItem: boom, setItem: boom, removeItem: boom }; },
    });
    try {
      browseView.hydrated = false;
      expect(() => browseView.init(null)).not.toThrow();
      expect(browseView.hideMature).toBe(false);
      expect(browseView.matureParam).toBeNull();
      expect(() => browseView.setHideMature(true)).not.toThrow();
      expect(browseView.hideMature).toBe(true);
    } finally {
      if (real) Object.defineProperty(window, 'localStorage', real);
      else delete (window as unknown as Record<string, unknown>).localStorage;
    }
  });

  it('does not disturb the AI flag beside it', () => {
    // Ten separate localStorage keys, one per setting: writing one
    // cannot touch another, and this is the pair a reader is most
    // likely to set together.
    signIn(true, true);
    rehydrate(null);
    browseView.setHideAI(true);
    browseView.setHideMature(true);
    rehydrate(null);
    expect(browseView.hideAI).toBe(true);
    expect(browseView.hideMature).toBe(true);
    expect(browseView.aiParam).toBe('not_pure');
    expect(browseView.matureParam).toBe('not_mature');
  });
});

// ⛔ #1345: THE ROW RENDERS ON CAPABILITY TO RECEIVE, NOT ON CONSENT.
//
// ADR 0090 §2 exempts `system.admin` from the mature disqualification so
// a moderator can see what the instance switch hid. The #1292 cascade
// asked about CONSENT, so an exempt account that never opted in was
// shown mature rows and offered no control over them: the one class of
// reader who sees mature content without opting in was the one class
// with no way to stop.
//
// # ⭐ IT IS NINE CASES, NOT TWO, AND THE INTERESTING THREE ARE THE
// # ONES WHERE THE KEY IS ABSENT
//
// Three reader classes crossed with three storage states. The classes
// differ in what the DEFAULT is, so a matrix that only drove the two
// states where the device HAS an answer would pass on an implementation
// with no class default at all — which is exactly the implementation
// this replaces.
//
//   class                                     key absent   key '1'   key '0'
//   instance forbids                          no row       no row    no row
//   allowed, opted in                         included     excluded  included
//   allowed, exempt, never opted in           EXCLUDED     excluded  included
//
// The third row's first cell is the amendment's new decision and the
// only cell that distinguishes the two classes that HAVE a row.
describe('#1345 the mature row is offered on CAPABILITY, and defaults by reader class', () => {
  /** The three storage states, spelled once. `null` means the device
   *  has never answered, which is a THIRD state and not a synonym for
   *  either boolean. */
  const STORAGE = [null, '1', '0'] as const;

  function reader(opts: { allowed: boolean; optedIn: boolean; exempt: boolean }) {
    auth.user = {
      ref: 1,
      username: 'reader',
      matureContentAllowed: opts.allowed,
      matureOptedIn: opts.optedIn,
    };
    auth.caps = opts.exempt ? ['system.admin'] : [];
    auth.capsStatus = 'resolved';
  }

  function withStorage(v: string | null) {
    localStorage.clear();
    if (v !== null) localStorage.setItem('aa_browse_hide_mature', v);
    browseView.hydrated = false;
    browseView.init(null);
  }

  beforeEach(() => {
    auth.user = null;
    auth.caps = [];
    auth.capsStatus = 'resolved';
  });

  // ── Class 1: the instance forbids mature content ────────────────────
  it('offers no row on an install with the feature off, whatever the device stored', () => {
    // Rung 1 outranks everything below it, INCLUDING the exemption: the
    // operator's answer is about the install. Driven with the exemption
    // HELD, so the only thing that can withhold the row is the switch.
    for (const v of STORAGE) {
      reader({ allowed: false, optedIn: true, exempt: true });
      withStorage(v);
      expect(browseView.matureFilterAvailable, `stored ${v}`).toBe(false);
      expect(browseView.matureParam, `stored ${v}`).toBeNull();
    }
  });

  // ── Class 2: allowed and opted in (unchanged by #1345) ──────────────
  it('⭐ a reader who opted in still defaults to INCLUDED', () => {
    // The upgrade property from #1292, and this widening must not move
    // it: shipping the row changed no wall for a reader who had already
    // consented.
    reader({ allowed: true, optedIn: true, exempt: false });
    withStorage(null);
    expect(browseView.matureFilterAvailable).toBe(true);
    expect(browseView.hideMature).toBe(false);
    expect(browseView.matureParam).toBeNull();
  });

  it('a reader who opted in honours either stored answer', () => {
    reader({ allowed: true, optedIn: true, exempt: false });
    withStorage('1');
    expect(browseView.hideMature).toBe(true);
    expect(browseView.matureParam).toBe('not_mature');

    reader({ allowed: true, optedIn: true, exempt: false });
    withStorage('0');
    expect(browseView.hideMature).toBe(false);
    expect(browseView.matureParam).toBeNull();
  });

  // ── Class 3: allowed, exempt, never opted in (the new class) ────────
  it('⛔ THE REGRESSION GUARD: an exempt reader who never opted in IS offered the row', () => {
    // This is the case that fails against the old getter, which read
    // `matureContentAllowed === true && matureOptedIn === true` and
    // returned false here — no row, on the one reader the gate sends
    // mature rows to without a consent.
    reader({ allowed: true, optedIn: false, exempt: true });
    withStorage(null);
    expect(
      browseView.matureFilterAvailable,
      'the exemption means rows reach this reader, so a control over them can do something',
    ).toBe(true);
  });

  it('⛔ THE REGRESSION GUARD: and it defaults to EXCLUDED for them', () => {
    // The second half, and it fails against the old getter too — for a
    // different reason, which is why it is a separate case. A widening
    // that only flipped `matureFilterAvailable` would leave the default
    // at INCLUDED and hand a moderator who never consented exactly the
    // wall they already had, with a tick box that starts on.
    reader({ allowed: true, optedIn: false, exempt: true });
    withStorage(null);
    expect(browseView.hideMature, 'never said yes to anything').toBe(true);
    expect(browseView.matureParam).toBe('not_mature');
  });

  it('an exempt reader can turn it off, and that choice SURVIVES A RELOAD', () => {
    // ⛔ THE HALF A REMOVE-ON-FALSE STORE CANNOT DO. Writing `0` is
    // what separates "this device chose include" from "this device has
    // not answered"; without it the class default would re-narrow this
    // reader's wall on the very next load.
    reader({ allowed: true, optedIn: false, exempt: true });
    withStorage(null);
    expect(browseView.hideMature).toBe(true);

    browseView.setHideMature(false);
    expect(browseView.hideMature).toBe(false);
    expect(browseView.matureParam).toBeNull();
    expect(localStorage.getItem('aa_browse_hide_mature')).toBe('0');

    // The reload.
    browseView.hydrated = false;
    browseView.init(null);
    expect(browseView.hideMature, 'the deliberate include must not be forgotten').toBe(false);
    expect(browseView.matureParam).toBeNull();
  });

  it('an exempt reader who stored EXCLUDE keeps excluding', () => {
    reader({ allowed: true, optedIn: false, exempt: true });
    withStorage('1');
    expect(browseView.hideMature).toBe(true);
    expect(browseView.matureParam).toBe('not_mature');
  });

  // ── The exemption is the SPECIFIC capability, not "some admin" ──────
  it('⛔ a read-cap operator is NOT exempt, so the row stays absent', () => {
    // `canSeeAdmin` is true for a holder of any admin ENTRY capability,
    // and it is the wrong predicate here: the gate waives the mature
    // rule for `system.admin` and nothing else, so offering the row to
    // these accounts would be a control that could never do anything —
    // the exact failure the cascade exists to prevent.
    reader({ allowed: true, optedIn: false, exempt: false });
    auth.caps = ['users.read', 'roles.read', 'teams.read', 'assets.admin'];
    withStorage(null);
    expect(browseView.matureExempt).toBe(false);
    expect(browseView.matureFilterAvailable).toBe(false);
    expect(browseView.matureParam).toBeNull();
  });

  it('⛔ unknown rights are NO rights: a caps blip withdraws the row', () => {
    // `can()` returns false while capsStatus is `unavailable` (#956),
    // so a resolver failure must not be able to OFFER a control. It
    // withdraws one instead, which is the safe direction on this axis.
    reader({ allowed: true, optedIn: false, exempt: true });
    auth.capsStatus = 'unavailable';
    withStorage(null);
    expect(browseView.matureExempt).toBe(false);
    expect(browseView.matureFilterAvailable).toBe(false);
  });

  // ── The layers stay separate ────────────────────────────────────────
  it('⛔ NO path here writes the account preference', () => {
    // Layer 3 narrows; layer 2 is the consent. An exempt reader ticking
    // the row has not granted themselves anything, and unticking it has
    // not revoked their exemption.
    reader({ allowed: true, optedIn: false, exempt: true });
    withStorage(null);

    browseView.setHideMature(false);
    browseView.setHideMature(true);

    expect(auth.user?.matureOptedIn, 'the consent must be untouched').toBe(false);
    expect(browseView.matureExempt, 'and so must the exemption').toBe(true);
    // One key, and it is the device's.
    expect(
      Object.keys(localStorage).filter((k) => k.includes('mature')),
      'exactly one storage key, and no user_preferences write',
    ).toEqual(['aa_browse_hide_mature']);
  });

  it('⭐ a guest who signs in as a moderator gets the moderator default with no reload', () => {
    // The class is a property of the SESSION, so the default cannot be
    // a value frozen at init(). The store hydrates once per page load
    // and the root layout re-runs the account seed on identity change;
    // this rung has to answer correctly across that without one.
    auth.user = null;
    withStorage(null);
    expect(browseView.matureFilterAvailable, 'a guest is offered nothing').toBe(false);
    expect(browseView.matureParam).toBeNull();

    reader({ allowed: true, optedIn: false, exempt: true });
    expect(browseView.matureFilterAvailable, 'the row appears without a rehydrate').toBe(true);
    expect(browseView.hideMature, 'and with the moderator default').toBe(true);
    expect(browseView.matureParam).toBe('not_mature');
  });
});
