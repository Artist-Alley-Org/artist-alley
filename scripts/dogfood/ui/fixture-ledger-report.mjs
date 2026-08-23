#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// THE ATTRIBUTION TABLE (#1247).
//
// Reads the JSONL ledger that helpers/fixture-ledger.ts appends to while
// the suite runs, nets each spec's creations against its own deletions,
// and prints who left what. That is the table #1247's repair plan
// assumed existed: the corpus census says "assets +54" and this says
// which specs the 54 belong to.
//
// Report only. It never deletes a row and nothing downstream treats it
// as a deletion rule — the sweep's provenance rule stays the only thing
// allowed to remove anything.
//
// Usage: node fixture-ledger-report.mjs [ledger.jsonl]

import { readFileSync, existsSync } from 'node:fs';

const path = process.argv[2] ?? '.pw-results/fixture-ledger.jsonl';

if (!existsSync(path)) {
  console.log(`  (no fixture ledger at ${path} — nothing to attribute)`);
  process.exit(0);
}

/** @type {Map<string, Map<string, {created: number, deleted: number, ids: Set<string>}>>} */
const bySpec = new Map();
let lines = 0;
let malformed = 0;

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
  if (!tables.has(table)) tables.set(table, { created: 0, deleted: 0, ids: new Set() });
  const cell = tables.get(table);
  if (row.op === 'create') {
    cell.created++;
    if (row.id) cell.ids.add(String(row.id));
  } else if (row.op === 'delete') {
    cell.deleted++;
    if (row.id) cell.ids.delete(String(row.id));
  }
}

/** Rows a spec created and did not itself remove, spec by spec. */
const rows = [];
const totals = new Map();
for (const [spec, tables] of bySpec) {
  for (const [table, cell] of tables) {
    const net = cell.created - cell.deleted;
    const t = totals.get(table) ?? { created: 0, deleted: 0 };
    t.created += cell.created;
    t.deleted += cell.deleted;
    totals.set(table, t);
    if (net > 0) rows.push({ spec, table, ...cell, net });
  }
}
rows.sort((a, b) => b.net - a.net || a.spec.localeCompare(b.spec));

console.log(
  `\n\x1b[1;36m==>\x1b[0m Fixture attribution (#1247) — ${lines} recorded write(s)` +
    (malformed ? `, ${malformed} unparseable` : ''),
);

if (rows.length === 0) {
  console.log('\n  \x1b[1;32mEvery spec removed everything it created.\x1b[0m');
} else {
  console.log(
    '\n  Rows a spec CREATED and did not delete. The census counts the same rows;\n' +
      '  this says whose they are. Fix the spec, do not raise the budget.\n',
  );
  console.log(
    `  ${'SPEC'.padEnd(46)} ${'TABLE'.padEnd(17)} ${'MADE'.padStart(5)} ${'GONE'.padStart(5)} ${'LEFT'.padStart(5)}`,
  );
  for (const r of rows) {
    console.log(
      `  ${r.spec.padEnd(46)} ${r.table.padEnd(17)} ${String(r.created).padStart(5)} ` +
        `${String(r.deleted).padStart(5)} ${String(r.net).padStart(5)}`,
    );
  }
  // One surviving id per offender, so the row can actually be looked at.
  console.log('');
  for (const r of rows) {
    const sample = [...r.ids].slice(0, 3);
    if (sample.length) {
      console.log(`  ${r.spec} → ${r.table}: ${sample.join(', ')}${r.ids.size > 3 ? ' …' : ''}`);
    }
  }
}

console.log(`\n  ${'TABLE'.padEnd(17)} ${'MADE'.padStart(5)} ${'GONE'.padStart(5)} ${'LEFT'.padStart(5)}`);
for (const [table, t] of [...totals].sort()) {
  console.log(
    `  ${table.padEnd(17)} ${String(t.created).padStart(5)} ${String(t.deleted).padStart(5)} ` +
      `${String(t.created - t.deleted).padStart(5)}`,
  );
}
console.log(
  '\n  ⚠️ DELETE is a SOFT delete on assets, posts and collections, and an archive\n' +
    '     on field_definition — the row survives it. "GONE" means the spec cleaned up,\n' +
    '     not that the row left the table. `aa sweep-fixtures` is what removes rows.',
);
