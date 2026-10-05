#!/usr/bin/env node --test
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// THE ASSERTION THAT DISCRIMINATES #1351.
//
// `node --test fixture-ledger.test.mjs` — run first by run-ui.sh, so
// the harness is checked before the suite it accounts for is trusted.
//
// Covers both halves of the accounting: fixture-ledger-report.mjs (what
// each spec left behind) and fixture-ledger-reconcile.mjs (whether that
// agrees with the corpus census).
//
// Every fixture below is a verbatim shape from a real ledger — the
// 402-test standalone run at `workers=2` on head 3ab8f464 that
// reproduced #1351 — reduced to the lines that carry the mechanism.
// Each one is a case where the number of WRITE CALLS and the number of
// ROWS differ, and each one made the old `created - deleted` arithmetic
// disagree with the corpus census.
//
// ⛔ THESE FAIL AGAINST THE UNFIXED REPORTER, which is the point:
//
//   dedup double-create   old LEFT  1   (census said 0)  → now 0
//   repeated delete       old LEFT -1   (census said 0)  → now 0
//   revive then archive   old LEFT -1   (census said 0)  → now 0
//   hidden attribution    old: the responsible spec was omitted from
//                         the table entirely by the `net > 0` filter,
//                         under the headline "Every spec removed
//                         everything it created"
//
// The last one is why nobody could see which spec produced
// `collections -1` for weeks: the totals summed it and the attribution
// dropped it.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
// `AA_LEDGER_REPORT` points these cases at a DIFFERENT reporter, which
// is how "shown to fail against the unfixed behaviour" was demonstrated:
// the pre-#1351 reporter, given the same fixtures, reads LEFT 1 for the
// dedup case and -1 for the other two.
const REPORT = process.env.AA_LEDGER_REPORT || join(HERE, 'fixture-ledger-report.mjs');

/** Run the reporter over a synthetic ledger; return its stdout + summary. */
function report(rows) {
  const dir = mkdtempSync(join(tmpdir(), 'aa-ledger-'));
  const ledger = join(dir, 'fixture-ledger.jsonl');
  const summary = join(dir, 'summary.json');
  writeFileSync(ledger, rows.map((r) => JSON.stringify(r)).join('\n') + '\n');
  const stdout = execFileSync(process.execPath, [REPORT, ledger, '--json', summary], {
    encoding: 'utf8',
  });
  return { stdout, summary: JSON.parse(readFileSync(summary, 'utf8')) };
}

const create = (spec, table, id, worker = 0) => ({
  ts: '2026-08-29T03:20:57.924Z',
  spec,
  test: 't',
  worker,
  via: 'api',
  op: 'create',
  table,
  id,
  status: 201,
});
const del = (spec, table, id, worker = 0) => ({
  ts: '2026-08-29T03:21:03.425Z',
  spec,
  test: 't',
  worker,
  via: 'api',
  op: 'delete',
  table,
  id,
  status: 204,
});

// ── Mechanism 1: a deduplicating create ──────────────────────────────
//
// `POST /api/v1/assets` hits the per-user dedup pre-check over
// (owner_user_ref, file_hash) and the default `warn` behaviour answers
// HTTP 200 carrying the EXISTING asset's id. ui-12-iiif-smoke's
// `beforeAll` runs once per worker and uploads the same one-pixel PNG
// both times, so at workers=2 there are two 2xx creates of ONE id. Only
// the first `afterAll` delete succeeds; the second 404s and is
// deliberately not recorded (fixture-ledger.ts:205-206).
test('a create that dedupes to a row already created leaves nothing', () => {
  const ID = 'd46df911-1c17-413a-98cf-b02ca692b967';
  const { stdout, summary } = report([
    create('ui-12-iiif-smoke.spec.ts', 'assets', ID, 0),
    { ...create('ui-12-iiif-smoke.spec.ts', 'assets', ID, 1), status: 200 },
    del('ui-12-iiif-smoke.spec.ts', 'assets', ID, 0),
  ]);
  const t = summary.tables.assets;
  assert.equal(t.createCalls, 2, 'both 2xx creates stay on the record');
  assert.equal(t.deleteCalls, 1);
  // The old reporter printed LEFT 1 here while the census said 0.
  assert.equal(t.left, 0, 'two calls, one row, one delete — nothing was left behind');
  assert.equal(t.dupCreates, 1, 'the extra call is named, not netted away');
  assert.match(stdout, /Every row a spec created was deleted by that spec/);
  // The worker index is what makes the mechanism legible at all.
  assert.match(stdout, /handed back to more than one worker/);
  assert.match(stdout, new RegExp(ID));
});

// ── Mechanism 2: an idempotent delete ────────────────────────────────
//
// collection-edit-reseed-1262 and ui-38-field-detail-page both DELETE a
// row twice and get 204 both times. Two calls, one row.
test('a repeated 2xx delete of one row never drives LEFT negative', () => {
  const ID = 'bcaad204-7844-4c4c-a8ac-e6c49be05ba9';
  const { stdout, summary } = report([
    create('collection-edit-reseed-1262.spec.ts', 'collections', ID),
    del('collection-edit-reseed-1262.spec.ts', 'collections', ID),
    del('collection-edit-reseed-1262.spec.ts', 'collections', ID),
  ]);
  const t = summary.tables.collections;
  assert.equal(t.deleteCalls, 2);
  // The old reporter printed `collections -1` here, on every CI run.
  assert.equal(t.left, 0);
  assert.equal(t.repeatDeletes, 1);
  assert.match(stdout, /collection-edit-reseed-1262\.spec\.ts/);
});

// ── Mechanism 3: a revive is not a create ────────────────────────────
//
// ensureProbeField (create-page-1119 and three others) PATCHes an
// archived probe field back to active rather than POSTing a new one,
// then archives it again in afterAll. A counted delete with no counted
// create, and the live row count is unchanged across the pair.
test('a delete of a row the run never created is attributed, not netted', () => {
  const ID = '808e1fa2-162e-4f52-8699-7aa21c1a32f7';
  const { stdout, summary } = report([
    del('create-page-1119.spec.ts', 'field_definition', ID),
  ]);
  const t = summary.tables.field_definition;
  assert.equal(t.left, 0);
  assert.equal(t.unmatchedDeletes, 1);
  assert.match(stdout, /create-page-1119\.spec\.ts/);
});

// ── The filter that hid all three ────────────────────────────────────
//
// `if (net > 0)` kept negative and zero cells out of the attribution
// while the totals summed them, so a run could print "Every spec
// removed everything it created" and `collections -1` in the same
// output and never name the spec. Any cell whose calls and rows differ
// is listed now.
test('a spec whose calls and rows differ is named even when it left nothing', () => {
  const { stdout } = report([
    create('clean-spec.spec.ts', 'posts', 'p1'),
    del('clean-spec.spec.ts', 'posts', 'p1'),
    del('odd-spec.spec.ts', 'collections', 'c-seeded'),
  ]);
  assert.match(stdout, /Calls that are not rows/);
  assert.match(stdout, /odd-spec\.spec\.ts/);
  assert.doesNotMatch(stdout, /clean-spec\.spec\.ts/, 'a balanced spec stays off the report');
});

// ── The leak the ledger exists to catch still reads as a leak ────────
test('a row created and not deleted is reported, with its id', () => {
  const { stdout, summary } = report([
    create('leaky.spec.ts', 'assets', 'aaaa-1111'),
    create('leaky.spec.ts', 'assets', 'bbbb-2222'),
    del('leaky.spec.ts', 'assets', 'aaaa-1111'),
  ]);
  assert.equal(summary.tables.assets.left, 1);
  assert.match(stdout, /leaky\.spec\.ts/);
  assert.match(stdout, /bbbb-2222/);
  assert.doesNotMatch(stdout, /Every row a spec created was deleted/);
});

// A row deleted and then legitimately re-created is live again. Netting
// by id has to re-arm, or a spec that recycles an id would report a leak
// as clean.
test('a row re-created after its delete counts as left behind', () => {
  const { summary } = report([
    create('recycle.spec.ts', 'posts', 'p1'),
    del('recycle.spec.ts', 'posts', 'p1'),
    create('recycle.spec.ts', 'posts', 'p1'),
  ]);
  assert.equal(summary.tables.posts.left, 1);
  assert.equal(summary.tables.posts.dupCreates, 1);
});

// ⛔ An unidentifiable create counts as a leak. Assuming otherwise would
// let a leak hide behind a response shape.
test('a create whose response carried no id counts as left behind', () => {
  const { stdout, summary } = report([
    { ...create('no-id.spec.ts', 'assets', null), id: null },
    del('no-id.spec.ts', 'assets', 'unrelated'),
  ]);
  assert.equal(summary.tables.assets.left, 1);
  assert.equal(summary.tables.assets.createsWithoutId, 1);
  assert.match(stdout, /carried\s+no id/);
});

// The property the whole change rests on, stated as a property.
test('LEFT is never negative for any table', () => {
  const { summary } = report([
    del('a.spec.ts', 'assets', 'x1'),
    del('a.spec.ts', 'posts', 'x2'),
    del('a.spec.ts', 'collections', 'x3'),
    del('a.spec.ts', 'field_definition', 'x4'),
    create('a.spec.ts', 'assets', 'x5'),
    del('a.spec.ts', 'assets', 'x5'),
    del('a.spec.ts', 'assets', 'x5'),
  ]);
  for (const [table, t] of Object.entries(summary.tables)) {
    assert.ok(t.left >= 0, `${table} LEFT went negative: ${t.left}`);
  }
});

// An absent ledger is "unknown", never "clean".
test('a missing ledger writes a summary that says so', () => {
  const dir = mkdtempSync(join(tmpdir(), 'aa-ledger-'));
  const summaryPath = join(dir, 'summary.json');
  execFileSync(process.execPath, [REPORT, join(dir, 'nope.jsonl'), '--json', summaryPath], {
    encoding: 'utf8',
  });
  const summary = JSON.parse(readFileSync(summaryPath, 'utf8'));
  assert.equal(summary.present, false);
  assert.deepEqual(summary.tables, {});
});

// ─────────────────────────────────────────────────────────────────────
// fixture-ledger-reconcile.mjs — the comparison that was never made.
//
// #1351 existed because the ledger and the census printed contradictory
// numbers into the same log and nothing but a reader compared them.
// These cases pin the comparison, including its one legitimate slack
// term (a delete of a row the run never created).
// ─────────────────────────────────────────────────────────────────────

const RECONCILE = join(HERE, 'fixture-ledger-reconcile.mjs');

/** Run the reconciler; return { code, stdout }. Never throws on exit 7. */
function reconcile(tables, before, after, { present = true } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'aa-reconcile-'));
  const summary = join(dir, 'summary.json');
  writeFileSync(summary, JSON.stringify({ present, lines: 1, tables }));
  const r = spawnSync(process.execPath, [RECONCILE, summary, before, after], {
    encoding: 'utf8',
  });
  return { code: r.status, stdout: r.stdout };
}

const CLEAN = '100|50|10|20|5';

test('agreement when the census delta equals the ledger LEFT', () => {
  const r = reconcile({ assets: { left: 0, unmatchedDeletes: 0 } }, CLEAN, CLEAN);
  assert.equal(r.code, 0);
  assert.match(r.stdout, /The two counters agree/);
});

// The #1351 shape itself: the ledger claims a row was left behind and
// the census sees none. Under the pre-fix arithmetic this was every run.
test('a ledger LEFT the census cannot see fails the run with exit 7', () => {
  const r = reconcile({ assets: { left: 1, unmatchedDeletes: 0 } }, CLEAN, CLEAN);
  assert.equal(r.code, 7);
  assert.match(r.stdout, /THE TWO COUNTERS DISAGREE/);
  assert.match(r.stdout, /assets/);
});

// The documented case the ledger cannot see: a write still on the wire
// when the page was torn down. The census catches it; this says so.
test('a row the census saw and the ledger did not fails with exit 7', () => {
  const r = reconcile({ assets: { left: 0, unmatchedDeletes: 0 } }, CLEAN, '101|50|10|20|5');
  assert.equal(r.code, 7);
  assert.match(r.stdout, /still on the wire|CREATE_ROUTES does not list/);
});

// A delete of a row the run never created (ensureProbeField's revive)
// either removed a pre-existing row or completed a revive, so the census
// delta may legitimately sit anywhere in LEFT-FOREIGN .. LEFT.
test('a foreign delete widens the expected band by exactly one per delete', () => {
  const tables = { field_definition: { left: 0, unmatchedDeletes: 1 } };
  assert.equal(reconcile(tables, CLEAN, CLEAN).code, 0, 'revived and re-archived');
  assert.equal(reconcile(tables, CLEAN, '100|50|10|19|5').code, 0, 'a pre-existing row removed');
  assert.equal(reconcile(tables, CLEAN, '100|50|10|18|5').code, 7, 'two rows for one delete');
});

// ⛔ A skipped comparison is not a passed one, and it must not be a
// failed one either — run-ui.sh already fails loudly elsewhere when the
// census cannot run.
test('a missing or absent ledger skips rather than passing or failing', () => {
  const missing = spawnSync(process.execPath, [RECONCILE, '/nonexistent.json', CLEAN, CLEAN], {
    encoding: 'utf8',
  });
  assert.equal(missing.status, 0);
  assert.match(missing.stdout, /RECONCILIATION SKIPPED/);

  const absent = reconcile({}, CLEAN, CLEAN, { present: false });
  assert.equal(absent.code, 0);
  assert.match(absent.stdout, /RECONCILIATION SKIPPED/);
});

test('a malformed census tuple skips rather than comparing nonsense', () => {
  const r = reconcile({}, '100|50|10', CLEAN);
  assert.equal(r.code, 0);
  assert.match(r.stdout, /RECONCILIATION SKIPPED/);
});
