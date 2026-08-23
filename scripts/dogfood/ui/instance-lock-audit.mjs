#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// THE PROOF THAT THE WINDOWS DID NOT INTERLEAVE (#1248).
//
// helpers/instance-lock.ts appends one line per hold — lock, owner, pid,
// how long it waited, and the exact window it held. This reads them back
// and checks the property the lock exists for: for any one lock, no two
// windows overlap.
//
// Why it is checked every run rather than argued once: "two workers
// cannot interleave" is a claim about a race, and a race that is merely
// unlikely passes most runs. A green suite is not evidence; a run whose
// own audit says the windows were disjoint is.
//
// It also reports CONTENTION. If no hold ever waited, non-overlap is
// trivially true and the run proves nothing about exclusion — that is
// said out loud rather than counted as a pass, because a suite that
// happened not to contend is exactly the state the old code was in on
// the runs where it looked fine.
//
// Exit 4 on an overlap: distinct from Playwright's own failure code and
// from the corpus ratchet's 3, so the three failure kinds are told apart
// by the exit status alone.
//
// Usage: node instance-lock-audit.mjs [instance-locks.jsonl]

import { readFileSync, existsSync } from 'node:fs';

const path = process.argv[2] ?? '.pw-results/instance-locks.jsonl';

if (!existsSync(path)) {
  console.log(`  (no instance-lock audit at ${path} — no spec took a shared-config lock)`);
  process.exit(0);
}

/** @type {Map<string, Array<{owner:string,pid:number,waitedMs:number,acquiredAt:number,releasedAt:number,abandoned?:boolean}>>} */
const byLock = new Map();
for (const line of readFileSync(path, 'utf8').split('\n')) {
  if (!line.trim()) continue;
  let row;
  try {
    row = JSON.parse(line);
  } catch {
    continue;
  }
  if (!row.lock) continue;
  if (!byLock.has(row.lock)) byLock.set(row.lock, []);
  byLock.get(row.lock).push(row);
}

console.log('\n\x1b[1;36m==>\x1b[0m Shared-instance-config locks (#1248)');

let overlaps = 0;
let contended = 0;
let holds = 0;

for (const [lock, rowsRaw] of byLock) {
  const rows = rowsRaw.slice().sort((a, b) => a.acquiredAt - b.acquiredAt);
  holds += rows.length;
  const waited = rows.filter((r) => r.waitedMs >= 25);
  contended += waited.length;
  const abandoned = rows.filter((r) => r.abandoned);

  console.log(
    `\n  ${lock}: ${rows.length} hold(s), ${waited.length} of which waited for another spec` +
      (abandoned.length ? `, ${abandoned.length} abandoned by a dying worker` : ''),
  );
  const t0 = rows.length ? rows[0].acquiredAt : 0;
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i];
    const prev = rows[i - 1];
    const clash = prev && r.acquiredAt < prev.releasedAt;
    if (clash) overlaps++;
    console.log(
      `    ${clash ? '\x1b[1;31mOVERLAP\x1b[0m' : '       '} ` +
        `[${String(r.acquiredAt - t0).padStart(7)}ms → ${String(r.releasedAt - t0).padStart(7)}ms] ` +
        `waited ${String(r.waitedMs).padStart(6)}ms  ${r.owner} (pid ${r.pid})` +
        (r.abandoned ? '  <abandoned>' : ''),
    );
  }
}

if (overlaps > 0) {
  console.log(
    `\n\x1b[1;31mTWO SPECS HELD THE SAME INSTANCE SETTING AT ONCE\x1b[0m — ${overlaps} overlapping window(s).\n` +
      'The cross-file lock did not exclude them, so a read-prior/set/restore sequence\n' +
      'could have published another spec’s temporary value. Fix the lock, not the specs.',
  );
  process.exit(4);
}

if (holds === 0) {
  console.log('  (no holds recorded)');
} else if (contended === 0) {
  console.log(
    `\n  ${holds} hold(s), none of which ever waited. The windows did not overlap, but\n` +
      '  nothing contended either, so this run does not demonstrate exclusion.',
  );
} else {
  console.log(
    `\n\x1b[1;32mThe windows are disjoint\x1b[0m — ${holds} hold(s), ${contended} of which had to wait\n` +
      '  for another spec to give the setting back. Contention happened and was serialised.',
  );
}
