// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The SHARED ADMIN PROFILE, borrowed and given back under a cross-file
// lock (#1017).
//
// # What is shared
//
// Every spec in this suite signs in as the same bootstrap admin, so
// `user_profiles` for that one account is instance-wide state in the
// same sense `system.public_mode` is — with a wider blast radius, since
// `public_mode` is read by four specs and the admin's `language` is read
// by every rendered string on every page in the suite.
//
// ui-30 flips it to Spanish and back. While it is Spanish, ANY spec on
// another worker that asserts English chrome is asserting against a
// preference it did not ask for, and the failure it reports names a
// control it never touched. That is the shape #1017 filed: `ui-30 ›
// locale choice persists across a reload` failing at `--workers 2` and
// passing in isolation.
//
// # Why `test.describe.configure({ mode: 'serial' })` was not enough
//
// #535 already added serial mode to ui-30, and #1017 was filed against
// a tree that had it. Serial orders the tests INSIDE one describe block;
// it says nothing about what the OTHER worker is doing at the same time,
// and the other worker is where the reader is. So the exclusion has to
// be over the resource and across files, which is what
// helpers/instance-lock provides — it is keyed by NAME, so this is a
// second resource on the existing mechanism rather than a second
// mechanism.
//
// # The part worth copying from public-mode.ts
//
// Not the shape of the hold: the REFUSAL. `setLanguage` will not touch
// the profile unless this hold owns the lock, so "a spec forgot to take
// it" is an immediate, named failure instead of a race that shows up as
// somebody else's flake three files away. `ui-30` asserts that refusal
// directly, which is the half public-mode.ts never pinned.
//
// # Why the mutation goes through the UI and the restore through the API
//
// The mutation is the thing under test in ui-30 — clicking the real
// preference control is the point, and a helper that PATCHed instead
// would be testing the API and calling it a locale switch. The restore
// is not under test: it has to land even when the test that changed the
// value has already failed, so it is a direct PATCH from `afterAll`,
// asserted against the value the server echoes back rather than assumed
// (#946).
//
// # The one writer that legitimately does NOT take this lock
//
// `tests/setup/admin.setup.ts` PATCHes the same field, and should keep
// doing so without a hold. It is the `setup` PROJECT, which the whole
// standalone project declares as a dependency, so it runs before any
// spec exists to contend with — and what it is doing is repairing an
// instance somebody else left in Spanish, which is the state a hold
// cannot help with because the process that made it is gone. It is a
// run-level backstop, not a second writer; it fires once, at the start,
// and cannot do anything for a spec running on the other worker while
// this file is mid-switch. That window is what the lock is for.
//
// `request` is passed per call rather than captured, for the reason
// public-mode.ts gives: Playwright disposes the `{ request }` fixture at
// the end of the hook that asked for it.

import { expect, type APIRequestContext, type Page } from '@playwright/test';
import { acquireInstanceLock, type InstanceLock } from './instance-lock';

/** The lock name. One string, so every writer contends over the same file. */
export const ADMIN_PROFILE_LOCK = 'user.admin_profile';

const ME = '/api/v1/auth/me';

/** The preferences this hold borrows. `language` is the only one a spec
 *  writes today; the snapshot is a record so adding `theme` later does
 *  not change the contract. */
export interface AdminProfileSnapshot {
  /** The admin's user ref — the path parameter the restore PATCHes. */
  readonly ref: number;
  /** BCP47 tag, or '' for "follow system". */
  readonly language: string;
}

export interface AdminProfileHold {
  /** What the profile held when the lock was taken. */
  readonly prior: AdminProfileSnapshot | undefined;
  /** Is the profile currently ours? */
  readonly holding: boolean;
  /**
   * Take the profile. Reads the current preferences INSIDE the lock —
   * which is the whole point — and returns them. Calling it while
   * already held is a no-op that returns the same snapshot.
   */
  acquire(request: APIRequestContext, opts?: { waitMs?: number }): Promise<AdminProfileSnapshot>;
  /**
   * Switch the language with the REAL preference control, by the
   * endonym on its button ("English", "Español", "Français" — never
   * translated, so the selector is locale-stable).
   *
   * Refuses unless this hold owns the lock.
   */
  setLanguage(page: Page, endonym: string): Promise<void>;
  /**
   * Write the language straight to the profile. This is for ARRANGING a
   * starting state and for the restore — never for the switch a spec is
   * asserting on, which has to go through the control.
   *
   * Refuses unless this hold owns the lock.
   */
  setLanguageDirect(request: APIRequestContext, tag: string): Promise<void>;
  /**
   * Put the prior preferences back and hand the profile on. Idempotent:
   * a second call (the `afterAll` backstop behind a `finally`) writes
   * nothing, so it can never restore a value that has gone stale.
   */
  release(request: APIRequestContext): Promise<void>;
}

/**
 * Create a hold for one spec file.
 *
 * `owner` is quoted in the lock file, in any wait-timeout message and in
 * the refusal above, so a jam or a missing acquire names the spec rather
 * than reading as a hang or as somebody else's flake.
 */
export function adminProfileHold(owner: string): AdminProfileHold {
  let lock: InstanceLock | undefined;
  let prior: AdminProfileSnapshot | undefined;
  let changed = false;

  return {
    get prior() {
      return prior;
    },
    get holding() {
      return lock !== undefined;
    },

    async acquire(request, opts) {
      if (lock) return prior as AdminProfileSnapshot;
      lock = await acquireInstanceLock(ADMIN_PROFILE_LOCK, owner, opts);
      try {
        const me = await request.get(ME);
        expect(me.status(), 'the admin profile must be readable as admin').toBe(200);
        const body = (await me.json()) as { ref: number; language?: string };
        prior = { ref: body.ref, language: body.language ?? '' };
      } catch (e) {
        // Never hold a lock we cannot use.
        lock.release();
        lock = undefined;
        throw e;
      }
      return prior;
    },

    async setLanguage(page, endonym) {
      expect(
        lock !== undefined,
        `${owner} tried to change the shared admin profile without holding the ` +
          `"${ADMIN_PROFILE_LOCK}" lock`,
      ).toBe(true);
      // The control lives on the preferences page; go there if we are
      // not already, so a caller cannot half-arrange this.
      if (!new URL(page.url()).pathname.startsWith('/account/preferences')) {
        await page.goto('/account/preferences');
      }
      // Substring, NOT exact: the button's accessible name is the
      // endonym plus the catalogue's completion percentage for anything
      // under 100% ("Español (63%)"), so an exact match finds only
      // English and times out on every other locale. The endonym itself
      // is the stable handle — see ui-30's note on why.
      await page.getByRole('button', { name: endonym }).click();
      changed = true;
    },

    async setLanguageDirect(request, tag) {
      expect(
        lock !== undefined,
        `${owner} tried to change the shared admin profile without holding the ` +
          `"${ADMIN_PROFILE_LOCK}" lock`,
      ).toBe(true);
      await writeLanguage(request, owner, (prior as AdminProfileSnapshot).ref, tag);
      changed = true;
    },

    async release(request) {
      if (!lock) return;
      try {
        // Only write if we moved it. A restore that writes what is
        // already there is harmless but noisy; a restore that runs when
        // nothing was changed is how a stale value gets published.
        if (changed && prior !== undefined) {
          await writeLanguage(request, owner, prior.ref, prior.language);
        }
      } finally {
        changed = false;
        lock.release();
        lock = undefined;
      }
    },
  };
}

/** PATCH the language and prove it STUCK. The read-back is the point:
 *  the handler echoes its own request body, so asserting the response
 *  alone would pass on a write that never landed (#946). */
async function writeLanguage(
  request: APIRequestContext,
  owner: string,
  ref: number,
  tag: string,
): Promise<void> {
  const r = await request.patch(`/api/v1/users/${ref}`, { data: { language: tag } });
  expect(
    r.ok(),
    `setting the admin's language to "${tag}" failed with HTTP ${r.status()} — every spec ` +
      `that reads this profile inherits whatever ${owner} left`,
  ).toBe(true);
  const me = await request.get(ME);
  expect(me.status(), 'the admin profile must be readable after the write').toBe(200);
  expect(
    ((await me.json()) as { language?: string }).language ?? '',
    `the admin profile did not come back holding "${tag}"`,
  ).toBe(tag);
}
