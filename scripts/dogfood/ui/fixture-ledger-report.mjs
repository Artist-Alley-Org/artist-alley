#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// THE ATTRIBUTION TABLE (#1247), NETTED BY ROW IDENTITY (#1351).
//
// Reads the JSONL ledger that helpers/fixture-ledger.ts appends to while
// the suite runs and prints who left what. That is the table #1247's
// repair plan assumed existed: the corpus census says "assets +54" and
// this says which specs the 54 belong to.
//
// Report only. It never deletes a row and nothing downstream treats it
// as a deletion rule — the sweep's provenance rule stays the only thing
// allowed to remove anything.
//
// ─────────────────────────────────────────────────────────────────────
// ⛔ THE LEDGER COUNTS API CALLS. THE CENSUS COUNTS ROWS. THEY ARE NOT
//    THE SAME NUMBER, AND `created - deleted` IS NOT A ROW COUNT (#1351).
// ─────────────────────────────────────────────────────────────────────
//
// #1351 was filed because this reporter said `ui-12-iiif-smoke assets
// MADE 2 GONE 1 LEFT 1` in the same output where the census said the
// database was unchanged. Both were reporting honestly; the arithmetic
// underneath `LEFT` was the wrong arithmetic. Three mechanisms, all
// measured on a real run (2 workers, local, head 3ab8f464), make a
// WRITE CALL stop corresponding one-to-one with a ROW:
//
//   1. A DEDUPLICATING CREATE returns 2xx on a row that already exists.
//      `POST /api/v1/assets` has a per-user dedup pre-check over
//      (owner_user_ref, file_hash); the default `warn` behaviour answers
//      HTTP 200 carrying the EXISTING asset's id
//      (assets/handler.go dedupResponse). ui-12's `beforeAll` runs once
//      PER WORKER and uploads the same one-pixel PNG both times, so at
//      `workers=2` two creates come back — with the SAME id. Two calls,
//      one row. Only one `afterAll` delete then succeeds; the other 404s
//      and is deliberately not recorded. `2 - 1 = 1` and nothing leaked.
//
//   2. AN IDEMPOTENT DELETE returns 2xx twice for one row. Observed on
//      collections (collection-edit-reseed-1262) and field_definition
//      (ui-38-field-detail-page): the second `DELETE` on an
//      already-removed row answers 204, so it is counted again. Two
//      calls, one row, and the net goes NEGATIVE.
//
//   3. A REVIVE IS NOT A CREATE. create-page-1119, advanced-vocab-1191,
//      advanced-operators-1165-1173-1197 and field-participation-1173
//      all `ensureProbeField`: if their probe field is sitting archived
//      from a previous run they PATCH it back to active rather than
//      POSTing a new one, then archive it again in `afterAll`. A counted
//      delete with no counted create, and the live row count is
//      unchanged across the pair.
//
// A "made minus cleaned up" column cannot be plainly true when it can go
// negative, which is #1351's own thesis. So the leak is netted BY ID
// instead: a row is left behind when its id was created during the run
// and no delete of that id followed. The three shapes above then fall
// out as named, attributable anomalies rather than as arithmetic that
// silently disagrees with the census — and on the reproduction run every
// table's LEFT came to 0, which is what the census said all along.
//
// ⛔ The call counts are still printed, and the ledger still records the
//    dedup 200 as a create. Suppressing the record to make the columns
//    agree is the "make one counter match the other" move #1351 forbids:
//    the API really did answer 2xx to a create, and that fact is how
//    mechanism 1 was found at all.
//
// ⛔ AN UNIDENTIFIABLE CREATE COUNTS AS A LEAK. If a create's response
//    carried no id, this cannot prove the row was cleaned up, so it is
//    added to LEFT and named in its own column. Assuming otherwise would
//    let a leak hide behind a response shape.
//
// Usage: node fixture-ledger-report.mjs [ledger.jsonl] [--json out.json]

import { readFileSync, existsSync, writeFileSync, mkdirSync } from 'node:fs';
import { dirname } from 'node:path';

const argv = process.argv.slice(2);
let path = '.pw-results/fixture-ledger.jsonl';
let jsonOut = '';
for (let i = 0; i < argv.length; i++) {
  if (argv[i] === '--json') {
    jsonOut = argv[++i] ?? '';
  } else if (!argv[i].startsWith('--')) {
    path = argv[i];
  }
}

/** An empty summary still gets written, so a reader downstream can tell
 *  "no ledger" from "ledger not read". */
function emitJSON(payload) {
  if (!jsonOut) return;
  try {
    mkdirSync(dirname(jsonOut), { recursive: true });
    writeFileSync(jsonOut, `${JSON.stringify(payload, null, 2)}\n`);
  } catch {
    // A summary that could not be written must not fail the run; the
    // reader treats an absent file as "unknown", not as "clean".
  }
}

if (!existsSync(path)) {
  console.log(`  (no fixture ledger at ${path} — nothing to attribute)`);
  emitJSON({ ledger: path, present: false, lines: 0, tables: {} });
  process.exit(0);
}

/**
 * Net one spec's writes against one table BY ROW ID.
 *
 * `live` holds ids created and not since deleted — the leak. The four
 * counters beside it are the ways a call stops matching a row, kept
 * apart so the report can name which one happened rather than fold them
 * into a number that goes negative.
 */
function newCell() {
  return {
    createCalls: 0,
    deleteCalls: 0,
    live: new Set(), // created here, not yet deleted here
    removed: new Set(), // created here and deleted here
    dupCreates: 0, // 2xx create of an id already created (dedup)
    repeatDeletes: 0, // 2xx delete of an id already deleted (idempotent)
    unmatchedDeletes: new Set(), // deleted here, never created here
    createsWithoutId: 0, // create whose response carried no id
  };
}

/** @type {Map<string, Map<string, ReturnType<typeof newCell>>>} */
const bySpec = new Map();
let lines = 0;
let malformed = 0;
/** Ids created by more than one worker process — the dedup fingerprint. */
const workersByCreatedId = new Map();

for (const line of readFileSync(path, 'utf8').split('\n')) {
  if (!line.trim()) continue;
  lines++;
  let row;
  try {
    row = JSON.parse(line);
  } catch {
    malformed++;
    continue;
  }
  const spec = row.spec ?? '(unknown)';
  const table = row.table ?? '(unknown)';
  if (!bySpec.has(spec)) bySpec.set(spec, new Map());
  const tables = bySpec.get(spec);
  if (!tables.has(table)) tables.set(table, newCell());
  const cell = tables.get(table);
  const id = row.id === undefined || row.id === null ? undefined : String(row.id);

  if (row.op === 'create') {
    cell.createCalls++;
    if (id === undefined) {
      cell.createsWithoutId++;
      continue;
    }
    if (!workersByCreatedId.has(id)) workersByCreatedId.set(id, new Set());
    workersByCreatedId.get(id).add(row.worker ?? null);
    if (cell.live.has(id) || cell.removed.has(id)) {
      // The same row handed back twice. Re-arming `live` is deliberate:
      // a row deleted and then legitimately re-created IS live again.
      cell.dupCreates++;
      cell.removed.delete(id);
      cell.live.add(id);
      continue;
    }
    cell.live.add(id);
  } else if (row.op === 'delete') {
    cell.deleteCalls++;
    if (id === undefined) continue;
    if (cell.live.has(id)) {
      cell.live.delete(id);
      cell.removed.add(id);
    } else if (cell.removed.has(id)) {
      cell.repeatDeletes++;
    } else {
      cell.unmatchedDeletes.add(id);
    }
  }
}

const rows = [];
const totals = new Map();
for (const [spec, tables] of bySpec) {
  for (const [table, cell] of tables) {
    const left = cell.live.size + cell.createsWithoutId;
    const anomalies =
      cell.dupCreates + cell.repeatDeletes + cell.unmatchedDeletes.size + cell.createsWithoutId;
    const t = totals.get(table) ?? {
      createCalls: 0,
      deleteCalls: 0,
      left: 0,
      dupCreates: 0,
      repeatDeletes: 0,
      unmatchedDeletes: 0,
      createsWithoutId: 0,
    };
    t.createCalls += cell.createCalls;
    t.deleteCalls += cell.deleteCalls;
    t.left += left;
    t.dupCreates += cell.dupCreates;
    t.repeatDeletes += cell.repeatDeletes;
    t.unmatchedDeletes += cell.unmatchedDeletes.size;
    t.createsWithoutId += cell.createsWithoutId;
    totals.set(table, t);
    // ⛔ EVERY nonzero cell is listed, not only the positive ones. The
    // previous filter was `net > 0`, so the specs producing a NEGATIVE
    // total — the ones in mechanisms 2 and 3 above — were summed into
    // the totals table and then omitted from the attribution that says
    // whose they are. `collections -1` reproduced on every CI run for
    // weeks with nothing on screen naming the spec (#1351).
    if (left > 0 || anomalies > 0) rows.push({ spec, table, cell, left, anomalies });
  }
}
rows.sort((a, b) => b.left - a.left || b.anomalies - a.anomalies || a.spec.localeCompare(b.spec));

const leaks = rows.filter((r) => r.left > 0);
const oddities = rows.filter((r) => r.left === 0 && r.anomalies > 0);

console.log(
  `\n\x1b[1;36m==>\x1b[0m Fixture attribution (#1247) — ${lines} recorded write(s)` +
    (malformed ? `, ${malformed} unparseable` : ''),
);

if (leaks.length === 0) {
  console.log('\n  \x1b[1;32mEvery row a spec created was deleted by that spec.\x1b[0m');
} else {
  console.log(
    '\n  Rows a spec CREATED and did not delete, netted by row id. The census counts\n' +
      '  the same rows; this says whose they are. Fix the spec, do not raise the budget.\n',
  );
  console.log(
    `  ${'SPEC'.padEnd(46)} ${'TABLE'.padEnd(17)} ${'LEFT'.padStart(5)}`,
  );
  for (const r of leaks) {
    console.log(`  ${r.spec.padEnd(46)} ${r.table.padEnd(17)} ${String(r.left).padStart(5)}`);
  }
  console.log('');
  for (const r of leaks) {
    const sample = [...r.cell.live].slice(0, 3);
    if (sample.length) {
      console.log(
        `  ${r.spec} → ${r.table}: ${sample.join(', ')}${r.cell.live.size > 3 ? ' …' : ''}`,
      );
    }
    if (r.cell.createsWithoutId) {
      console.log(
        `  ${r.spec} → ${r.table}: ${r.cell.createsWithoutId} create(s) whose response carried ` +
          `no id — counted as left because it cannot be shown they were removed`,
      );
    }
  }
}

// ⛔ WHERE THE NEGATIVE NUMBERS WENT (#1351). These cells net to zero
// rows and are the reason the old `created - deleted` column could read
// -1 while every spec had in fact cleaned up. Named, attributable, and
// no longer able to hide behind the headline above.
if (oddities.length > 0) {
  console.log(
    '\n  Calls that are not rows — a spec whose write COUNT and row COUNT differ.\n' +
      '  None of these left a row behind; each is listed so the difference between\n' +
      '  this ledger and the corpus census is attributable rather than mysterious.\n',
  );
  console.log(
    `  ${'SPEC'.padEnd(46)} ${'TABLE'.padEnd(17)} ${'POST'.padStart(5)} ${'DEL'.padStart(4)} ` +
      `${'DEDUP'.padStart(6)} ${'REDEL'.padStart(6)} ${'FOREIGN'.padStart(8)} ${'NO-ID'.padStart(6)}`,
  );
  for (const r of oddities) {
    const c = r.cell;
    console.log(
      `  ${r.spec.padEnd(46)} ${r.table.padEnd(17)} ${String(c.createCalls).padStart(5)} ` +
        `${String(c.deleteCalls).padStart(4)} ${String(c.dupCreates).padStart(6)} ` +
        `${String(c.repeatDeletes).padStart(6)} ${String(c.unmatchedDeletes.size).padStart(8)} ` +
        `${String(c.createsWithoutId).padStart(6)}`,
    );
  }
  console.log(
    '\n  DEDUP   a 2xx create that handed back a row this spec already created\n' +
      '  REDEL   a 2xx delete of a row this spec had already deleted\n' +
      '  FOREIGN a delete of a row this run never created — a seeded row, or one\n' +
      '          revived by PATCH rather than POSTed (ensureProbeField)\n' +
      '  NO-ID   a create whose response carried no id; counted as LEFT',
  );
}

const tableTotals = {};
console.log(
  `\n  ${'TABLE'.padEnd(17)} ${'POST'.padStart(5)} ${'DEL'.padStart(5)} ${'LEFT'.padStart(5)} ` +
    `${'DEDUP'.padStart(6)} ${'REDEL'.padStart(6)} ${'FOREIGN'.padStart(8)} ${'NO-ID'.padStart(6)}`,
);
for (const [table, t] of [...totals].sort()) {
  tableTotals[table] = t;
  console.log(
    `  ${table.padEnd(17)} ${String(t.createCalls).padStart(5)} ${String(t.deleteCalls).padStart(5)} ` +
      `${String(t.left).padStart(5)} ${String(t.dupCreates).padStart(6)} ` +
      `${String(t.repeatDeletes).padStart(6)} ${String(t.unmatchedDeletes).padStart(8)} ` +
      `${String(t.createsWithoutId).padStart(6)}`,
  );
}
console.log(
  '\n  POST/DEL are CALLS; LEFT is ROWS. They differ by DEDUP, REDEL and FOREIGN,\n' +
    '  which is why LEFT is netted by row id and can never be negative (#1351).\n' +
    '  ⚠️ DELETE is a SOFT delete on assets, posts and collections, and an archive\n' +
    '     on field_definition — the row survives it. LEFT counts rows the run left\n' +
    '     VISIBLE, which is what the census counts. `aa sweep-fixtures` removes rows.',
);

// A create seen by more than one worker process is mechanism 1's
// fingerprint, and it is only visible because the ledger records the
// worker index. Printed rather than inferred: at workers=1 it cannot
// happen, which is exactly why CI never reproduced #1351.
const crossWorker = [...workersByCreatedId].filter(
  ([, w]) => w.size > 1 && ![...w].includes(null),
);
if (crossWorker.length) {
  console.log(
    `\n  ${crossWorker.length} row id(s) were handed back to more than one worker — a ` +
      `deduplicating\n  create, not a second row: ` +
      crossWorker
        .slice(0, 3)
        .map(([id, w]) => `${id} (workers ${[...w].sort().join(', ')})`)
        .join('; '),
  );
}

emitJSON({
  ledger: path,
  present: true,
  lines,
  malformed,
  tables: tableTotals,
});
