// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The instance's anonymous-browsing switch, borrowed and given back
// under a cross-file lock (#1248).
//
// Four specs need `public_mode` to be a particular value while they
// assert, and the instance has exactly one of it. Each one used to do
// its own read-prior / set / restore-prior, which is the right contract
// for a single writer and a LOST UPDATE for two:
//
//   1195 reads prior=false, sets true
//   1207 reads prior=TRUE  <- 1195's temporary value, not the instance's
//   1195 restores false
//   1207 asserts anonymous behaviour against a private instance, fails,
//        then "restores" TRUE and leaves the box public for the rest of
//        the suite.
//
// The read has to be inside the same critical section as the write, so
// that is where this puts it: `acquire()` takes the lock FIRST and only
// then asks the instance what it holds. Between acquire and release, no
// other spec on any worker — or in any other checkout driving the same
// stack — can move the switch.
//
// `request` is deliberately passed per call rather than captured.
// Playwright disposes the `{ request }` fixture at the end of the hook
// that asked for it ("Fixture { request } from beforeAll cannot be
// reused in a test"), so a hold created in `beforeAll` and used from a
// test must be handed the test's own context.

import { expect, type APIRequestContext } from '@playwright/test';
import { acquireInstanceLock, type InstanceLock } from './instance-lock';

/** The lock name. One string, so every writer contends over the same file. */
export const PUBLIC_MODE_LOCK = 'system.public_mode';

const ENDPOINT = '/api/v1/admin/system/public-mode';

export interface PublicModeHold {
  /** What the instance had when the lock was taken. */
  readonly prior: boolean | undefined;
  /** Is the switch currently ours? */
  readonly holding: boolean;
  /**
   * Take the switch. Reads the prior value INSIDE the lock — which is
   * the whole point — and returns it. Calling it while already held is
   * a no-op that returns the same prior.
   */
  acquire(request: APIRequestContext, opts?: { waitMs?: number }): Promise<boolean>;
  /** Move the switch. Refuses unless this hold owns it. */
  set(request: APIRequestContext, on: boolean): Promise<void>;
  /**
   * Put the prior value back and hand the switch on. Idempotent: a
   * second call (the `afterAll` backstop behind a `finally`) writes
   * nothing, so it can never restore a value that has gone stale.
   */
  release(request: APIRequestContext): Promise<void>;
}

/**
 * Create a hold for one spec file.
 *
 * `owner` is quoted in the lock file and in any wait-timeout message, so
 * a jam names the spec that is holding rather than reading as a hang.
 */
export function publicModeHold(owner: string): PublicModeHold {
  let lock: InstanceLock | undefined;
  let prior: boolean | undefined;
  let changed = false;

  return {
    get prior() {
      return prior;
    },
    get holding() {
      return lock !== undefined;
    },

    async acquire(request, opts) {
      if (lock) return prior as boolean;
      lock = await acquireInstanceLock(PUBLIC_MODE_LOCK, owner, opts);
      try {
        const mode = await request.get(ENDPOINT);
        expect(mode.status(), 'public-mode state must be readable as admin').toBe(200);
        prior = ((await mode.json()) as { enabled: boolean }).enabled;
      } catch (e) {
        // Never hold a lock we cannot use.
        lock.release();
        lock = undefined;
        throw e;
      }
      return prior;
    },

    async set(request, on) {
      expect(
        lock !== undefined,
        `${owner} tried to move public_mode without holding the "${PUBLIC_MODE_LOCK}" lock`,
      ).toBe(true);
      const r = await request.patch(ENDPOINT, { data: { enabled: on } });
      expect(r.status(), `public mode must be settable to ${on}`).toBe(200);
      // The response is the stored value, so this also confirms the write
      // landed rather than merely being accepted (#946).
      expect(((await r.json()) as { enabled: boolean }).enabled).toBe(on);
      if (on !== prior) changed = true;
      else changed = false;
    },

    async release(request) {
      if (!lock) return;
      try {
        // Only write if we moved it. A restore that writes what is
        // already there is harmless but noisy; a restore that runs when
        // nothing was changed is how a stale value gets published.
        if (changed && prior !== undefined) {
          await request
            .patch(ENDPOINT, { data: { enabled: prior } })
            .catch(() => undefined);
        }
      } finally {
        changed = false;
        lock.release();
        lock = undefined;
      }
    },
  };
}
