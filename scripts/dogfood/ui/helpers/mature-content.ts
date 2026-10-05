// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The install-wide mature-content switch, borrowed and given back under
// the cross-file lock (#1292, following #1248's public-mode pattern).
//
// It is SHARED INSTANCE STATE with exactly one value, so it needs the
// same treatment `public_mode` got and for the same reason: a spec that
// reads the prior value OUTSIDE the critical section reads whatever
// another worker happened to be holding at that moment, asserts against
// a state it did not ask for, and then publishes that temporary value
// as the restore. The lost update outlives the run.
//
// Only ONE spec writes this today. The helper exists anyway, because
// the contract that goes wrong is invisible until the second writer
// arrives, and by then the first one reads like it was always fine.
//
// ⚠️ SWITCHING IT OFF CLEARS NOTHING. Flags already set survive, so a
// restore genuinely restores; see updateMatureContentConfig. The switch
// governs enforcement and publication, never storage.
//
// `request` is passed per call rather than captured, for the reason
// public-mode.ts gives: Playwright disposes the `{ request }` fixture
// at the end of the hook that asked for it.

import { expect, type APIRequestContext } from '@playwright/test';
import { acquireInstanceLock, type InstanceLock } from './instance-lock';

/** The lock name. One string, so every writer contends over one file. */
export const MATURE_CONTENT_LOCK = 'system.mature_content';

const ENDPOINT = '/api/v1/admin/system/mature-content';

export interface MatureContentHold {
  /** What the instance had when the lock was taken. */
  readonly prior: boolean | undefined;
  readonly holding: boolean;
  acquire(request: APIRequestContext, opts?: { waitMs?: number }): Promise<boolean>;
  set(request: APIRequestContext, allowed: boolean): Promise<void>;
  release(request: APIRequestContext): Promise<void>;
}

export function matureContentHold(owner: string): MatureContentHold {
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
      lock = await acquireInstanceLock(MATURE_CONTENT_LOCK, owner, opts);
      try {
        const r = await request.get(ENDPOINT);
        expect(r.status(), 'the mature-content switch must be readable as admin').toBe(200);
        prior = ((await r.json()) as { allowed: boolean }).allowed;
      } catch (e) {
        // Never hold a lock we cannot use.
        lock.release();
        lock = undefined;
        throw e;
      }
      return prior;
    },

    async set(request, allowed) {
      expect(
        lock !== undefined,
        `${owner} tried to move the mature switch without holding "${MATURE_CONTENT_LOCK}"`,
      ).toBe(true);
      const r = await request.patch(ENDPOINT, { data: { allowed } });
      expect(r.status(), `the mature switch must be settable to ${allowed}`).toBe(200);
      // The response is the STORED value, so this confirms the write
      // landed rather than merely being accepted (#946).
      expect(((await r.json()) as { allowed: boolean }).allowed).toBe(allowed);
      changed = allowed !== prior;
    },

    async release(request) {
      if (!lock) return;
      try {
        if (changed && prior !== undefined) {
          await request.patch(ENDPOINT, { data: { allowed: prior } }).catch(() => undefined);
        }
      } finally {
        changed = false;
        lock.release();
        lock = undefined;
      }
    },
  };
}

/** The browse menu's Mature row is ADR 0090's layer 3, and it is stored
 *  per DEVICE (`aa_browse_hide_mature`). This pins it to "include" for
 *  one browser context, so a spec about some OTHER axis is not reading
 *  a wall the mature axis narrowed underneath it.
 *
 *  # ⛔ WHY ANY SPEC NEEDS THIS AT ALL (#1345)
 *
 *  ADR 0090 §2 exempts `system.admin` from the mature gate so a
 *  moderator can see what the instance switch hid, and the 2026-08-28
 *  amendment gives that reader the view control they previously lacked
 *  — defaulting to EXCLUDED, because they never consented to anything.
 *
 *  ⚠️ THE BOOTSTRAP ADMIN IS EXACTLY THAT READER ON A FRESH DATABASE,
 *  which is what CI is on every run, and it is the identity nearly every
 *  spec in this suite signs in as. So the resting browse wall for the
 *  suite's own account is now mature-filtered, and the filter button
 *  honestly reports a narrowing. Three specs asserted "nothing is
 *  hidden, so every box rests ticked and the button is inactive" — true
 *  only while the mature axis happened to be inert for that account.
 *
 *  ⛔ SO THE ASSUMPTION IS STATED, NOT INHERITED. A spec about the type
 *  filter or the AI row makes the third axis explicitly inert instead of
 *  depending on what the account's preferences happen to hold.
 *
 *  ⭐ AND IT IS THE DEVICE KEY RATHER THAN THE ACCOUNT OPT-IN, which is
 *  what keeps this safe to call from any file. The opt-in is one shared
 *  row on the account every worker signs in as — a second writer would
 *  race mature-row-1292's own restore. localStorage is per browser
 *  context, so this contends with nothing and needs no lock.
 *
 *  `'0'` is an explicit include and is meaningful only because #1345
 *  made the key tri-state; before that, "no narrowing" was spelled by
 *  REMOVING the key, which is now "no local choice" and would hand the
 *  reader their class default right back.
 *
 *  An init script rather than a one-off `evaluate`, so it survives every
 *  navigation the spec makes — the store reads the key once, at init.
 */
export async function includeMatureOnThisDevice(page: {
  addInitScript(script: string | (() => void)): Promise<void>;
}): Promise<void> {
  await page.addInitScript(() => {
    try {
      localStorage.setItem('aa_browse_hide_mature', '0');
    } catch {
      /* a context with storage disabled has no narrowing to undo */
    }
  });
}
