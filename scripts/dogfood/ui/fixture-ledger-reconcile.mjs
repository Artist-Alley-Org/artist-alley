#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// THE TWO COUNTERS, CHECKED AGAINST EACH OTHER (#1351).
//
// run-ui.sh runs two independent instruments over the same run:
//
//   the CORPUS CENSUS  counts LIVE rows in five tables, before and
//                      after, straight out of postgres. It answers
//                      WHETHER anything was left behind.
//   the FIXTURE LEDGER records every 2xx create and delete the browser
//                      or the API context reported, attributed to the
//                      spec that made it. It answers WHOSE.
//
// #1351 was filed because they printed contradictory things in the same
// output and nothing compared them, so the contradiction sat there for
// weeks: the ledger said `ui-12-iiif-smoke assets LEFT 1` and
// `collections LEFT -1` while the census said the database was
// unchanged. Each was read by eye, by different people, at different
// times. This closes that: the comparison is made by the run itself, on
// the run's own evidence, and a disagreement fails the run.
//
// ── What agreement means ─────────────────────────────────────────────
//
// The ledger's LEFT is rows created during the run and not deleted
// during the run — netted by row id, so a deduplicating create and an
// idempotent delete cannot move it (see fixture-ledger-report.mjs).
// That is the same quantity the census measures as `after - before`.
//
// One term separates them. A FOREIGN delete — a row the run removed but
// never created, such as the archived probe field ensureProbeField
// PATCHes back to life before archiving it again — leaves the ledger
// with a delete it cannot match. Each one either removed a row that was
// already there (census drops by 1) or completed a revive it never saw
// (census unchanged). So:
//
//     LEFT - FOREIGN  <=  census delta  <=  LEFT
//
// and where FOREIGN is 0 the two must be EQUAL. That equality is the
// assertion, and it is the one the pre-#1351 arithmetic failed: assets
// netted +1 with no foreign deletes while the census delta was 0.
//
// ── Why it fails the run rather than warning ─────────────────────────
//
// ⛔ A DISAGREEMENT IS A FINDING, NOT NOISE. The two documented ways to
// produce one are both real defects:
//
//   census > ledger — the run created a row the ledger never saw. That
//     is a spec that ended with a write still on the wire
//     (ui-14-upload-flow's AI-control case did exactly this), or a row
//     arriving by a route CREATE_ROUTES does not list.
//   census < ledger — the run removed a row it did not create, without
//     the ledger recording the delete.
//
// run-ui.sh:115-121 records what happens to a guard nobody has to look
// at, and #1263 records a census that was silently disabled in CI for
// its whole existence. Exit 7 is its own code so a wrapper can tell this
// apart from a failing assertion (1), the corpus ratchet (3), the
// instance-lock audit (4), no tests (5) and the denominator audit (6).
//
// Usage:
//   node fixture-ledger-reconcile.mjs <summary.json> <before> <after>
//
// where <before>/<after> are the census tuples run-ui.sh already has —
// `assets|posts|collections|field_definition|user`.

import { readFileSync, existsSync } from 'node:fs';

// The census tuple's column order, which is also the table order in
// corpus_census(). Kept here as the single mapping between the two
// instruments' vocabularies.
const CENSUS_TABLES = ['assets', 'posts', 'collections', 'field_definition', 'user'];

const [summaryPath, beforeRaw, afterRaw] = process.argv.slice(2);

function bail(msg) {
  console.log(`\n\x1b[1;33mLEDGER/CENSUS RECONCILIATION SKIPPED\x1b[0m — ${msg}`);
  console.log('This run says nothing about whether the two counters agree.');
  process.exit(0);
}

if (!summaryPath || !existsSync(summaryPath)) {
  bail(`no ledger summary at ${summaryPath ?? '(unset)'}`);
}
const before = String(beforeRaw ?? '').split('|');
const after = String(afterRaw ?? '').split('|');
if (before.length !== CENSUS_TABLES.length || after.length !== CENSUS_TABLES.length) {
  bail('the census tuples are not the expected five columns');
}

let summary;
try {
  summary = JSON.parse(readFileSync(summaryPath, 'utf8'));
} catch (e) {
  bail(`the ledger summary could not be read (${e.message})`);
}
if (!summary?.present) {
  bail('the ledger itself was absent, so there is nothing to reconcile');
}

const rows = [];
let disagreements = 0;
for (let i = 0; i < CENSUS_TABLES.length; i++) {
  const table = CENSUS_TABLES[i];
  const b = Number(before[i]);
  const a = Number(after[i]);
  if (!Number.isFinite(b) || !Number.isFinite(a)) {
    bail(`the census tuple column for ${table} is not a number`);
  }
  const t = summary.tables?.[table] ?? {};
  const left = Number(t.left ?? 0);
  const foreign = Number(t.unmatchedDeletes ?? 0);
  const delta = a - b;
  const lo = left - foreign;
  const hi = left;
  const ok = delta >= lo && delta <= hi;
  if (!ok) disagreements++;
  rows.push({ table, delta, left, foreign, lo, hi, ok });
}

console.log('\n\x1b[1;36m==>\x1b[0m Ledger vs census (#1351)');
console.log(
  `  ${'TABLE'.padEnd(17)} ${'CENSUS Δ'.padStart(9)} ${'LEDGER LEFT'.padStart(12)} ` +
    `${'FOREIGN'.padStart(8)} ${'EXPECTED'.padStart(12)}`,
);
for (const r of rows) {
  const expected = r.lo === r.hi ? String(r.hi) : `${r.lo}..${r.hi}`;
  console.log(
    `  ${r.table.padEnd(17)} ${String(r.delta).padStart(9)} ${String(r.left).padStart(12)} ` +
      `${String(r.foreign).padStart(8)} ${expected.padStart(12)}` +
      (r.ok ? '' : '   \x1b[1;31m<== DISAGREE\x1b[0m'),
  );
}

if (disagreements === 0) {
  console.log(
    '\n\x1b[1;32mThe two counters agree\x1b[0m — every row the ledger says was left behind is a\n' +
      'row the census also sees, and vice versa.',
  );
  process.exit(0);
}

console.log(
  `\n\x1b[1;31mTHE TWO COUNTERS DISAGREE\x1b[0m — ${disagreements} table(s). One of them is describing\n` +
    'something other than what its column says, which is #1351 exactly.\n',
);
for (const r of rows.filter((x) => !x.ok)) {
  if (r.delta > r.hi) {
    console.log(
      `  ${r.table}: the census saw ${r.delta - r.hi} more row(s) than the ledger recorded.\n` +
        `    A spec created a row the ledger never saw — a write still on the wire when the\n` +
        `    page was torn down, or a route CREATE_ROUTES does not list. The census is right.`,
    );
  } else {
    console.log(
      `  ${r.table}: the census saw ${r.lo - r.delta} fewer row(s) than the ledger recorded.\n` +
        `    Rows left the table that the ledger has no delete for. The census is right.`,
    );
  }
}
console.log(
  '\nDo NOT reconcile this by changing one counter to match the other. Find the write\n' +
    'the ledger could not see, or the row that left without one.',
);
process.exit(7);
