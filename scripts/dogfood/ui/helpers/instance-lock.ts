// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// A mutex over SHARED INSTANCE STATE, held ACROSS SPEC FILES (#1248).
//
// # What this exists for
//
// Five specs write `system.public_mode`, and every one of them follows
// the same three-step contract: read the prior value, set the one it
// needs, put the prior value back. That contract is correct on its own
// and unsound the moment two of them run at the same time, which two
// workers do by default:
//
//   A reads prior=false          B is elsewhere
//   A sets true                  B reads prior=TRUE   <- A's temporary
//   A asserts, restores false    B asserts "public"   <- against false
//                                B restores TRUE      <- the instance is
//                                                        now wrong for
//                                                        every later spec
//
// Both halves of that are real damage: B's assertions ran against a mode
// it did not ask for, and B's "restore" left the box in a state nobody
// chose. The second half outlives the run.
//
// # Why the serial mode already on those files does not fix it
//
// `test.describe.configure({ mode: 'serial' })` orders tests INSIDE one
// describe block. It says nothing about two different FILES landing in
// two different workers, which is exactly the case here — all four
// already declare serial mode and all four still interleave. Playwright
// has no cross-file mutex, so this is one.
//
// # Why a file lock rather than something in-process
//
// Playwright workers are separate OS processes, so nothing held in a
// module variable can exclude them. A file created with O_EXCL is
// atomic on every filesystem this runs on and is visible to every
// process on the box — including a second CHECKOUT driving the same
// stack, which is why the lock lives under the OS temp dir keyed by the
// instance's base URL rather than inside one worktree's .pw-results.
//
// # Crash safety
//
// A worker that dies holding the lock must not wedge the suite, and a
// worker that is merely slow must not have its lock stolen. Those are
// told apart by two independent signals, not by a timeout alone:
//
//   - the holder's PID. `process.kill(pid, 0)` on a dead process throws
//     ESRCH; a lock whose owner is gone is broken immediately.
//   - a heartbeat. The holder rewrites its record every HEARTBEAT_MS,
//     so "held for four minutes" and "abandoned four minutes ago" are
//     distinguishable — a legitimately long hold is never stolen, and a
//     process wedged with the lock is released after STALE_MS.
//
// Releasing checks a random token written at acquisition: a holder whose
// lock WAS stolen never unlinks the thief's file.
//
// # The lock proves itself
//
// "The windows cannot interleave" is a claim about a race, and a single
// green run is what a race looks like half the time. So every hold
// appends its own window — who, from when, to when, and how long it
// waited — to an audit log, and `instance-lock-audit.mjs` asserts after
// the run that no two windows on the same lock overlapped. A run where
// nothing ever waited is reported as such too: zero contention makes
// the non-overlap trivially true and proves nothing.

import { randomUUID } from 'node:crypto';
import {
  appendFileSync,
  closeSync,
  mkdirSync,
  openSync,
  readFileSync,
  unlinkSync,
  writeFileSync,
  writeSync,
} from 'node:fs';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

/** How often the holder proves it is still alive. */
const HEARTBEAT_MS = 3_000;
/** No heartbeat for this long AND still a live pid → the holder is wedged. */
const STALE_MS = 45_000;
/** Default ceiling on waiting for the lock. */
const DEFAULT_WAIT_MS = 300_000;
/** Poll interval while waiting, plus jitter so waiters do not lockstep. */
const POLL_MS = 40;

interface LockRecord {
  owner: string;
  pid: number;
  token: string;
  acquiredAt: number;
  heartbeatAt: number;
}

/**
 * Where locks live: the OS temp dir, keyed by the instance being driven.
 *
 * Keyed by base URL, not by checkout, because the thing being guarded is
 * the INSTANCE. Two worktrees pointed at the same stack contend for the
 * same setting and must share a lock; two worktrees pointed at different
 * stacks do not and must not.
 */
function lockDir(): string {
  const target = process.env.STUDIO_A_HOST ?? 'http://localhost:5173';
  const key = createHash('sha1').update(target).digest('hex').slice(0, 12);
  const dir = join(tmpdir(), 'aa-dogfood-locks', key);
  mkdirSync(dir, { recursive: true });
  return dir;
}

function lockPath(name: string): string {
  return join(lockDir(), `${name.replace(/[^A-Za-z0-9._-]/g, '_')}.lock`);
}

/**
 * Where each hold's window is recorded.
 *
 * run-ui.sh points this at `.pw-artifacts/`; a bare `npx playwright
 * test` falls back to the lock dir so the audit still exists and the
 * checker still has something to read. Deliberately NOT `.pw-results`:
 * Playwright empties its own `outputDir` at the start of every run.
 */
function auditPath(): string {
  return process.env.AA_LOCK_AUDIT ?? join(lockDir(), 'instance-locks.jsonl');
}

function audit(row: Record<string, unknown>): void {
  try {
    const file = auditPath();
    mkdirSync(dirname(file), { recursive: true });
    appendFileSync(file, `${JSON.stringify(row)}\n`);
  } catch {
    // The audit is evidence, not a gate. Never fail a spec over it.
  }
}

function readRecord(file: string): LockRecord | undefined {
  try {
    return JSON.parse(readFileSync(file, 'utf8')) as LockRecord;
  } catch {
    return undefined;
  }
}

function alive(pid: number): boolean {
  if (!Number.isFinite(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    // EPERM means the process exists and belongs to someone else, which
    // still counts as alive.
    return (e as NodeJS.ErrnoException).code === 'EPERM';
  }
}

/** A held lock. `release()` is idempotent — calling it twice is a no-op. */
export interface InstanceLock {
  readonly name: string;
  readonly owner: string;
  /** ms the lock has been held, for diagnostics. */
  heldMs(): number;
  release(): void;
}

interface HeldEntry {
  file: string;
  token: string;
  timer: NodeJS.Timeout;
  lock: string;
  owner: string;
  waitedMs: number;
  acquiredAt: number;
}

const held = new Set<HeldEntry>();
let exitHookInstalled = false;

function installExitHook(): void {
  if (exitHookInstalled) return;
  exitHookInstalled = true;
  // Synchronous, because `exit` gives no chance to await. A worker that
  // is torn down mid-hold releases here; one that is SIGKILLed does not,
  // and the pid check above covers that case for the next waiter.
  process.on('exit', () => {
    for (const h of held) {
      const rec = readRecord(h.file);
      if (rec?.token === h.token) {
        // Still ours: the hold ended because the process did, not
        // because a spec released it. Record the window anyway — an
        // abandoned hold is exactly the thing the audit should show.
        audit({
          lock: h.lock,
          owner: h.owner,
          pid: process.pid,
          waitedMs: h.waitedMs,
          acquiredAt: h.acquiredAt,
          releasedAt: Date.now(),
          abandoned: true,
        });
        try {
          unlinkSync(h.file);
        } catch {
          /* already gone */
        }
      }
    }
  });
}

/**
 * Take `name` exclusively, waiting for whoever holds it.
 *
 * `owner` is recorded in the lock file and quoted in the timeout message,
 * so a jam names both contenders instead of appearing as a hang.
 */
export async function acquireInstanceLock(
  name: string,
  owner: string,
  opts: { waitMs?: number } = {},
): Promise<InstanceLock> {
  installExitHook();
  const file = lockPath(name);
  const waitMs = opts.waitMs ?? DEFAULT_WAIT_MS;
  const deadline = Date.now() + waitMs;
  const token = randomUUID();
  const waitStart = Date.now();
  let lastHolder = 'nobody';

  for (;;) {
    try {
      const fd = openSync(file, 'wx');
      const rec: LockRecord = {
        owner,
        pid: process.pid,
        token,
        acquiredAt: Date.now(),
        heartbeatAt: Date.now(),
      };
      writeSync(fd, JSON.stringify(rec));
      closeSync(fd);

      const timer = setInterval(() => {
        const cur = readRecord(file);
        if (cur?.token !== token) return; // stolen — stop pretending
        cur.heartbeatAt = Date.now();
        try {
          writeFileSync(file, JSON.stringify(cur));
        } catch {
          /* transient — the next beat covers it */
        }
      }, HEARTBEAT_MS);
      timer.unref();

      const acquiredAt = Date.now();
      const entry: HeldEntry = {
        file,
        token,
        timer,
        lock: name,
        owner,
        waitedMs: acquiredAt - waitStart,
        acquiredAt,
      };
      held.add(entry);

      let released = false;
      return {
        name,
        owner,
        heldMs: () => Date.now() - acquiredAt,
        release: () => {
          if (released) return;
          released = true;
          clearInterval(timer);
          held.delete(entry);
          audit({
            lock: name,
            owner,
            pid: process.pid,
            waitedMs: acquiredAt - waitStart,
            acquiredAt,
            releasedAt: Date.now(),
          });
          const cur = readRecord(file);
          // Only ever unlink OUR lock. If it was stolen while we were
          // wedged, the thief owns it now and removing it would hand the
          // instance to a third waiter while the thief is mid-window.
          if (cur?.token === token) {
            try {
              unlinkSync(file);
            } catch {
              /* already gone */
            }
          }
        },
      };
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'EEXIST') throw e;
    }

    const rec = readRecord(file);
    if (!rec) {
      // Holder released between our create attempt and this read.
      continue;
    }
    lastHolder = `${rec.owner} (pid ${rec.pid})`;
    const stale = !alive(rec.pid) || Date.now() - rec.heartbeatAt > STALE_MS;
    if (stale) {
      // Break it. Two waiters may both do this; O_EXCL below still lets
      // exactly one of them win, and the loser simply keeps waiting.
      try {
        const still = readRecord(file);
        if (still?.token === rec.token) unlinkSync(file);
      } catch {
        /* someone else got there first */
      }
      continue;
    }

    if (Date.now() > deadline) {
      throw new Error(
        `timed out after ${waitMs}ms waiting for the "${name}" instance lock; ` +
          `held by ${lastHolder} since ${new Date(rec.acquiredAt).toISOString()}. ` +
          `This lock serialises specs that write shared instance config — see ` +
          `helpers/instance-lock.ts. A stuck holder is a bug in that spec's teardown, ` +
          `not a reason to widen the timeout.`,
      );
    }
    await new Promise((r) => setTimeout(r, POLL_MS + Math.floor(Math.random() * POLL_MS)));
  }
}
