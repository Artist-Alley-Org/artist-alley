// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The post-publish refresh gate (#1407).
//
// The UI regressions hold a real page response open and prove the
// reader sees both. These pin the state machine underneath, including
// the cases a UI test cannot arrange reliably: two publishes inside one
// slow page, a surface that is still busy when the deferred attempt
// arrives, and a publish landing while the refresh it triggered is
// itself in flight.
//
// The scheduler is injected, so every case below is synchronous and
// nothing here can pass by waiting.

import { describe, expect, it } from 'vitest';
import { createRefreshGate } from './refreshGate';

/** A scheduler under the test's control. `flush` runs what is queued. */
function manualScheduler() {
  let queue: Array<() => void> = [];
  return {
    schedule: (fn: () => void) => {
      queue.push(fn);
    },
    flush() {
      const due = queue;
      queue = [];
      for (const fn of due) fn();
    },
    get depth() {
      return queue.length;
    },
  };
}

function gate(initialBusy = false) {
  const s = manualScheduler();
  let busy = initialBusy;
  let runs = 0;
  const g = createRefreshGate({
    busy: () => busy,
    run: () => {
      runs += 1;
    },
    schedule: s.schedule,
  });
  return {
    g,
    s,
    runs: () => runs,
    setBusy: (v: boolean) => {
      busy = v;
    },
  };
}

describe('createRefreshGate', () => {
  it('refreshes immediately when the surface is idle', () => {
    const t = gate(false);
    t.g.request();
    t.s.flush();
    expect(t.runs()).toBe(1);
    expect(t.g.pending).toBe(false);
  });

  it('⛔ does NOT drop the publish when a request is in flight', () => {
    // The whole point. "Skip it while busy" is the fix that loses the
    // artist's work, and the in-flight request cannot be relied on to
    // carry it: it may have read the database before the publish
    // committed.
    const t = gate(true);
    t.g.request();
    t.s.flush();
    expect(t.runs(), 'nothing may run while the surface is busy').toBe(0);
    expect(t.g.pending, 'but it must still be owed').toBe(true);

    t.setBusy(false);
    t.g.settled();
    t.s.flush();
    expect(t.runs(), 'and it must run once the surface is free').toBe(1);
    expect(t.g.pending).toBe(false);
  });

  it('⛔ does NOT cancel the in-flight request', () => {
    // Expressed as what the gate never touches: it has no handle on the
    // running fetch, so there is nothing it could abort. The assertion
    // that carries the meaning is that the refresh happens AFTER, which
    // is what the ordering below records.
    const t = gate(true);
    const order: string[] = [];
    t.g.request();
    t.s.flush();
    order.push('page still in flight');
    t.setBusy(false);
    order.push('page landed');
    t.g.settled();
    t.s.flush();
    order.push(`refresh ran x${t.runs()}`);
    expect(order).toEqual(['page still in flight', 'page landed', 'refresh ran x1']);
  });

  it('coalesces several publishes inside one slow page into one refresh', () => {
    const t = gate(true);
    t.g.request();
    t.g.request();
    t.g.request();
    t.s.flush();
    expect(t.runs()).toBe(0);
    t.setBusy(false);
    t.g.settled();
    t.s.flush();
    expect(t.runs(), 'three publishes, one authoritative refresh').toBe(1);
  });

  it('stays owed when the surface is STILL busy at the deferred attempt', () => {
    // Two overlapping fetches: the first finishes and calls settled
    // while the second is still running. The refresh must not run then,
    // and must not be forgotten either.
    const t = gate(true);
    t.g.request();
    t.s.flush();

    t.g.settled(); // first fetch done, second still in flight
    t.s.flush();
    expect(t.runs(), 'the second fetch is still running').toBe(0);
    expect(t.g.pending).toBe(true);

    t.setBusy(false);
    t.g.settled(); // second fetch done
    t.s.flush();
    expect(t.runs()).toBe(1);
  });

  it('owes a NEW refresh for a publish that lands during the refresh itself', () => {
    // `pending` is cleared before `run`, so a publish arriving while the
    // refresh request is on the wire is not folded into a response that
    // had already been asked for.
    const s = manualScheduler();
    let busy = false;
    let runs = 0;
    const g = createRefreshGate({
      busy: () => busy,
      run: () => {
        runs += 1;
        busy = true; // the refresh is now the in-flight request
        g.request(); // a publish lands while it is on the wire
      },
      schedule: s.schedule,
    });

    g.request();
    s.flush();
    expect(runs).toBe(1);
    expect(g.pending, 'the second publish is owed, not swallowed').toBe(true);

    busy = false;
    g.settled();
    s.flush();
    expect(runs).toBe(2);
  });

  it('settled() on an idle gate with nothing owed does nothing', () => {
    const t = gate(false);
    t.g.settled();
    t.s.flush();
    expect(t.runs()).toBe(0);
    expect(t.g.pending).toBe(false);
  });

  it('reads no parameters, so the refresh applies to whatever is on screen', () => {
    // A refresh queued behind a query that CHANGES the address must
    // describe the set that query left, not the one that was there when
    // the publish landed. The gate can only get that right by holding
    // nothing: `run` reads the surface when it runs.
    const s = manualScheduler();
    let busy = true;
    let onScreen = 'the old query';
    const applied: string[] = [];
    const g = createRefreshGate({
      busy: () => busy,
      run: () => applied.push(onScreen),
      schedule: s.schedule,
    });

    g.request(); // publish lands while a new query is in flight
    s.flush();
    expect(applied).toEqual([]);

    onScreen = 'the new query'; // the query settles and replaces the set
    busy = false;
    g.settled();
    s.flush();
    expect(applied).toEqual(['the new query']);
  });

  // `/teams/{id}` (#1407). Two loaders, one surface. The page used to
  // answer "is anything running" with a boolean each loader wrote, so
  // the first to finish reported the surface idle while the other was
  // still in flight. That is what the page's load-more control reads
  // and what this gate consults, so a boolean is not a smaller version
  // of the right answer, it is the wrong one.
  describe('two loaders sharing one surface', () => {
    function twoLoaderSurface(kind: 'boolean' | 'counter') {
      const s = manualScheduler();
      let flag = false;
      let count = 0;
      const busy = () => (kind === 'boolean' ? flag : count > 0);
      let runs = 0;
      const g = createRefreshGate({
        busy,
        run: () => {
          runs += 1;
        },
        schedule: s.schedule,
      });
      return {
        g,
        s,
        busy,
        runs: () => runs,
        start: () => {
          if (kind === 'boolean') flag = true;
          else count += 1;
        },
        finish: () => {
          if (kind === 'boolean') flag = false;
          else count -= 1;
          g.settled();
        },
      };
    }

    it('⛔ a shared BOOLEAN reports the surface idle while a loader is still running', () => {
      // The defect, written down as a test so the counter below is a
      // fix for something and not a preference.
      const t = twoLoaderSurface('boolean');
      t.start(); // posts
      t.start(); // assets
      t.finish(); // posts lands first and writes `false`
      expect(t.busy(), 'assets is still running, and the flag denies it').toBe(false);
    });

    it('a COUNT stays busy until the last loader lands', () => {
      const t = twoLoaderSurface('counter');
      t.start();
      t.start();
      t.finish();
      expect(t.busy(), 'one loader is still running').toBe(true);
      t.finish();
      expect(t.busy()).toBe(false);
    });

    it('a refresh owed during two loaders waits for BOTH', () => {
      const t = twoLoaderSurface('counter');
      t.start();
      t.start();
      t.g.request();
      t.s.flush();
      expect(t.runs()).toBe(0);

      t.finish(); // the first lands
      t.s.flush();
      expect(t.runs(), 'the second is still in flight').toBe(0);
      expect(t.g.pending).toBe(true);

      t.finish(); // the second lands
      t.s.flush();
      expect(t.runs(), 'exactly one refresh, after both').toBe(1);
    });
  });

  it('does not queue a second attempt while one is already scheduled', () => {
    const t = gate(true);
    t.g.request();
    t.g.settled();
    t.g.settled();
    expect(t.s.depth, 'one attempt in flight is enough').toBe(1);
  });
});
