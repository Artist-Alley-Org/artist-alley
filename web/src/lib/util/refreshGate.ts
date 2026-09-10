// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * Hold a background refresh until the surface is free, then run exactly
 * one (#1407).
 *
 * # The race this exists for
 *
 * Every list on this app owns its own fetch, and each of them can have
 * a request in flight when a publish lands. Firing the refresh straight
 * away puts two requests on the same state, and the surfaces resolve
 * that in three different and equally wrong ways:
 *
 *   - `/search` and `/` supersede by generation, so the refresh wins
 *     and the page the reader had already asked for is DISCARDED. They
 *     scrolled, the loader fired, and the rows never arrive.
 *   - `/teams/{id}` has no generation at all. Its append composes from
 *     whatever the list holds WHEN THE RESPONSE LANDS, so a refresh
 *     that arrives in between leaves the append re-adding rows the
 *     merge just kept.
 *
 * # ⛔ AND THE OBVIOUS FIX IS WORSE THAN THE BUG
 *
 * "Skip the refresh while a request is in flight" loses the publish
 * entirely, and the tempting justification for it is false: the
 * in-flight request may have read the database BEFORE the publish
 * committed, so its response is not guaranteed to contain the new
 * content. Standing down on the strength of somebody else's request is
 * a silent "your work is not here", which is the whole of #1407.
 *
 * # What this does instead
 *
 * `request()` runs the refresh when the surface is idle and otherwise
 * marks it PENDING. `settled()`, called from every fetch's `finally`,
 * runs the pending one once the surface is free. Nothing is cancelled
 * and nothing is dropped.
 *
 * Two properties fall out and both are load bearing:
 *
 *   - **Coalescing.** `pending` is a boolean, so ten publishes during
 *     one slow page produce ONE refresh. The result is identical,
 *     because a refresh asks the server what is true now.
 *   - **Apply time, not queue time.** The gate stores no parameters. It
 *     calls `run`, and `run` reads the surface's current state when it
 *     runs. So a refresh queued behind a query that CHANGES the address
 *     applies to the result set that query left on screen, never to the
 *     one that was there when the publish landed.
 *
 * # Why it defers through a scheduler
 *
 * `settled()` is called from a `finally`, and a caller's own bookkeeping
 * (clearing its busy flags, recording what it fetched) may not have
 * finished at that point. Running the refresh a microtask later means
 * `busy()` is read after the operation has genuinely finished rather
 * than during its unwind. `schedule` is injectable so tests can drive
 * it synchronously.
 *
 * If `busy()` is still true when the deferred attempt arrives, the
 * request STAYS pending: another operation is running, and its own
 * `settled()` will try again. That is why every fetch has to call
 * `settled()`, including one whose response was superseded.
 */
export interface RefreshGate {
  /** A publish landed. Refresh now, or as soon as the surface is free. */
  request(): void;
  /** One of this surface's fetches finished, successfully or not. */
  settled(): void;
  /** True while a refresh is owed. Read by tests and by nothing else. */
  readonly pending: boolean;
}

export function createRefreshGate(opts: {
  /** Is a request on this surface's list state in flight right now? */
  busy: () => boolean;
  /** Perform the refresh. Reads the surface's CURRENT parameters. */
  run: () => void;
  /** Defaults to `queueMicrotask`. Injected by tests. */
  schedule?: (fn: () => void) => void;
}): RefreshGate {
  const schedule = opts.schedule ?? ((fn: () => void) => queueMicrotask(fn));
  let pending = false;
  let scheduled = false;

  function attempt(): void {
    scheduled = false;
    if (!pending) return;
    // Still busy: leave it owed. The operation that is running will
    // call `settled()` and this will be tried again.
    if (opts.busy()) return;
    // Cleared BEFORE `run`, so a publish that lands while the refresh
    // itself is in flight is owed a fresh one rather than being folded
    // into a request that had already gone out.
    pending = false;
    opts.run();
  }

  function kick(): void {
    if (scheduled) return;
    scheduled = true;
    schedule(attempt);
  }

  return {
    request(): void {
      pending = true;
      kick();
    },
    settled(): void {
      if (pending) kick();
    },
    get pending(): boolean {
      return pending;
    },
  };
}
