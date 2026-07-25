// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Shared close policy for the standalone viewer routes (#581).
//
// ADR 0067 draws the line: a viewer entered COLD (shared link, bookmark,
// pasted URL, cmd-click into a new tab) has no return context, so closing
// lands on the browse feed; a viewer entered IN-APP should return to
// wherever the user was — the collection, search results, a profile.
//
// The previous implementation gated that on `document.referrer`, which is
// the wrong signal for a SvelteKit app: client-side navigation never
// performs a document load, so the referrer is NEVER set when a user
// clicks a collection tile. The "in-app" branch could therefore only fire
// after a full page load, and every in-app open closed to `/` — the bug
// this fixes. (`posts/[id]` even carried a comment acknowledging the
// referrer was empty for direct nav; the guard was known-fragile.)
//
// `afterNavigate` is the framework's own answer, but the signal to read
// is `nav.type`, NOT `nav.from`. On a cold load SvelteKit 2 fires
// afterNavigate with `type: 'enter'` and a `from` that is a non-null
// object carrying no `url` — so the intuitive `nav.from !== null` test
// reports "came from in-app" on a freshly opened tab (measured: a pasted
// /assets/{id} URL closed by walking back to about:blank). `type` is the
// documented discriminator: 'enter' is the initial document load;
// 'link' / 'goto' / 'popstate' / 'form' are all client-side navigation.
// Both are checked below so neither signal alone can mislead.
//
// Kept in ONE place because the policy is byte-identical across
// /assets/[id] and /posts/[id] and must not drift — same instinct as
// $lib/util/portal.ts (#580).
//
// `.svelte.ts` because this uses runes ($state) — a plain .ts would fail
// at runtime, not compile time.

import { afterNavigate, goto } from '$app/navigation';

/** Where a cold-entered viewer closes to. The browse feed (ADR 0067). */
const COLD_CLOSE_TARGET = '/';

/**
 * Wire up the standalone viewer close policy. Call at component init
 * (it registers an `afterNavigate` hook, so it inherits that lifecycle).
 *
 * @returns `handleClose` — pass straight to the viewer's `onClose`.
 */
export function createCloseToOrigin(): { handleClose: () => Promise<void> } {
  // Whether this route was reached by in-app navigation. Starts false so
  // a cold load closes to the feed even if the hook never fires.
  let cameFromInApp = $state(false);

  afterNavigate((nav) => {
    // 'enter' is the cold document load; anything else reached this route
    // from inside the app. The `from.url` check is the corroborating
    // signal — a real in-app navigation always names where it came from.
    cameFromInApp = nav.type !== 'enter' && !!nav.from?.url;
  });

  async function handleClose() {
    if (cameFromInApp) {
      // history.back() rather than goto(from.url): going back POPS the
      // viewer's entry instead of pushing a third one, so the stack
      // doesn't grow every time a user opens and closes something, and
      // the browser's own back button keeps meaning what the user
      // expects.
      //
      // NOTE: this does NOT currently restore the origin page's scroll
      // position — measured, a collection scrolled to 1200px comes back
      // at 0. The app scrolls an inner `<main class="overflow-y-auto">`
      // rather than the window, and SvelteKit's scroll snapshotting only
      // tracks window scroll. Fixing that needs a snapshot on the
      // scrolling container (SvelteKit's `snapshot` export), which is a
      // separate change; goto() would not restore it either.
      history.back();
      return;
    }
    await goto(COLD_CLOSE_TARGET);
  }

  return {
    get handleClose() {
      return handleClose;
    },
  };
}
