// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Scroll (and list-state) restoration for the app's list surfaces (#584).
//
// # Why this exists at all
//
// SvelteKit restores scroll position on back/forward for free — but it
// snapshots `window`. This app never scrolls the window: the shell is
// `<div class="h-dvh … overflow-hidden">` with `<main class="flex-1
// overflow-y-auto">` as the real scroll container (see +layout.svelte).
// So the framework faithfully restores a scroll offset that is always
// 0, and a user who scrolled 1500px into the feed, opened a post on
// the standalone /posts/{id} route and closed it landed back at the
// top — with the feed's three extra loaded pages gone too (measured:
// 1500px and 144 cards → 0px and 36).
//
// This only ever affected the routes that UNMOUNT the list: /posts/{id}
// and /assets/{id}. The browse feed's `/?post=` overlay keeps the feed
// mounted underneath and was measured at 1500px → 1500px before any of
// this — no scroll container is ever torn down, so there is nothing to
// restore.
//
// SvelteKit's `snapshot` export is the sanctioned hook for state the
// framework can't infer. This wraps it so each list route adds one
// line instead of re-deriving the same rAF dance.
//
// # Why the payload isn't stored in the snapshot itself
//
// SvelteKit JSON-serialises snapshot values into sessionStorage, one
// per history entry. That's right for a scroll offset and wrong for
// the browse feed's loaded pages — four pages of posts is ~150KB, and
// a session accumulates history entries until the 5MB quota bites.
// So the serialised half is just `{ y, token }`, and the bulky half
// lives in a module-scope map keyed by that token, capped below. A
// full page reload drops the map, `restore` finds nothing, and the
// route re-fetches — which is what a reload should do anyway.

import { tick } from 'svelte';
import { chromeScroll } from '$stores/chromeScroll.svelte';

/** What actually gets written to sessionStorage. Deliberately tiny. */
interface StoredSnapshot {
  /** `main.scrollTop` at the moment we navigated away. */
  y: number;
  /** Key into the in-memory side table, when the route captured data. */
  token?: number;
}

/** How many captured payloads to keep. Well past any realistic
 *  back-stack depth a user walks in one session; small enough that a
 *  few feeds' worth of posts can't grow without bound. */
const MAX_ENTRIES = 8;

const store = new Map<number, unknown>();
let nextToken = 1;

function remember(data: unknown): number {
  const token = nextToken++;
  store.set(token, data);
  // Map preserves insertion order, so the first key is the oldest.
  while (store.size > MAX_ENTRIES) {
    const oldest = store.keys().next();
    if (oldest.done) break;
    store.delete(oldest.value);
  }
  return token;
}

/** The app-shell scroll container. Looked up per call rather than
 *  cached: page components outlive individual `<main>` elements only
 *  in theory, but a stale reference here fails silently, which is the
 *  worst failure mode for a fix nobody re-measures. */
function scroller(): HTMLElement | null {
  return typeof document === 'undefined' ? null : document.querySelector('main');
}

/** Hard cap on how long we keep re-asserting the offset. A restored
 *  route re-runs its own fetch (collection members, search hits, the
 *  featured rail), so `main` is usually still the wrong height when
 *  `restore` fires. ~1s at 60fps — long enough for a local API
 *  round-trip, short enough that a genuinely slow page doesn't yank
 *  the user later. */
const MAX_FRAMES = 60;

/** Consecutive frames at the target offset with an unchanged content
 *  height before we call it settled and stop. */
const STABLE_FRAMES = 6;

/** Anything under this and there's nothing worth restoring — and it
 *  avoids fighting a route that legitimately wants to start at the
 *  top. */
const MIN_OFFSET = 8;

/**
 * Put `main` back to `y` and hold it there until the page stops
 * moving under us.
 *
 * A single write is not enough, in BOTH directions. Too early and the
 * route's own fetch hasn't landed, so the content is shorter than the
 * offset and the write clamps. Too late is the subtler one: anything
 * that renders ABOVE the restore point afterwards — the browse feed's
 * featured rail is the live example — triggers Chrome's scroll
 * anchoring, which shifts scrollTop to hold the *visual* position and
 * therefore lands the user somewhere they never were. Measured: a
 * restore to 1500px settled at 1805px, the rail's exact height.
 *
 * So: re-assert every frame until the offset sticks and the content
 * height has stopped changing.
 */
function applyScroll(y: number): void {
  if (y < MIN_OFFSET) return;

  let frames = 0;
  let stable = 0;
  let lastHeight = -1;
  let aborted = false;

  // A real interaction wins immediately. Watching the events rather
  // than diffing scrollTop matters because scroll anchoring moves
  // scrollTop with no user involved at all — the very thing we're
  // here to undo.
  const abort = () => {
    aborted = true;
  };
  const events = ['wheel', 'touchstart', 'pointerdown', 'keydown'] as const;
  for (const e of events) window.addEventListener(e, abort, { passive: true, capture: true });
  const release = () => {
    for (const e of events) window.removeEventListener(e, abort, { capture: true });
    // Programmatic scrolling runs through the same listener the
    // auto-hiding chrome uses, and a 0 → 1500 jump reads as "scrolling
    // down fast" — the header would slide away on arrival. Put it
    // back; the next real scroll-down hides it again normally.
    chromeScroll.reveal();
  };

  const step = () => {
    const main = scroller();
    if (aborted || !main) {
      release();
      return;
    }

    const max = Math.max(0, main.scrollHeight - main.clientHeight);
    const target = Math.min(y, max);
    if (main.scrollTop !== target) main.scrollTop = target;

    const settled = target === y && main.scrollHeight === lastHeight;
    lastHeight = main.scrollHeight;
    stable = settled ? stable + 1 : 0;

    if (stable >= STABLE_FRAMES || frames++ >= MAX_FRAMES) {
      release();
      return;
    }
    requestAnimationFrame(step);
  };

  // One tick so a `restore` that assigned list state has rendered it,
  // then rAF so layout has run.
  void tick().then(() => requestAnimationFrame(step));
}

export interface ScrollSnapshot {
  capture(): StoredSnapshot;
  restore(value: StoredSnapshot): void;
}

/**
 * Build the `snapshot` export for a scrolling list route.
 *
 * ```svelte
 * export const snapshot = createScrollSnapshot();
 * ```
 *
 * Routes that also hold list state the load function can't rebuild
 * (an infinite-scroll feed's accumulated pages) pass `capture` /
 * `restore` and get that handed back before the scroll is applied.
 *
 * `restore` may run before OR after the component's own mount effects
 * — SvelteKit doesn't promise an order — so a route that fetches on
 * mount must be idempotent about it. See the browse feed's
 * `loadedKey` guard for the pattern.
 */
export function createScrollSnapshot<T = undefined>(
  opts: { capture?: () => T; restore?: (data: T) => void } = {},
): ScrollSnapshot {
  return {
    capture(): StoredSnapshot {
      const y = scroller()?.scrollTop ?? 0;
      if (!opts.capture) return { y };
      return { y, token: remember(opts.capture()) };
    },
    restore(value: StoredSnapshot): void {
      if (!value) return;
      if (opts.restore && value.token !== undefined && store.has(value.token)) {
        opts.restore(store.get(value.token) as T);
      }
      applyScroll(value.y ?? 0);
    },
  };
}
