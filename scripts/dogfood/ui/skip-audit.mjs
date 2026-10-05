#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// THE DENOMINATOR GATE (#1348, #1344).
//
// "386 passed" reads like success and is a SMALLER SUITE than "396
// passed". Nothing in the run said so, and that is the whole defect:
// the count that moved was the one nobody was watching.
//
// # What actually moved, measured rather than assumed
//
// #1348 reports a push run of 33198346487 at "9 skipped" against the
// pull-request run's 2, on identical code, and reads that as
// preconditions timing out under contention. Attempt 1 of that run says
// something more specific, in Playwright's own words:
//
//     2 failed
//     3 skipped
//     6 did not run
//     386 passed (10.6m)
//
// Playwright separates those two lines and this runner's summary did
// not: it printed `stats.skipped`, which is 3 + 6 = 9. So "2 became 9"
// is two different events added together.
//
//   * 6 DID NOT RUN. post-cover-focal-1210 and asset-usage-1237 declare
//     `mode: 'serial'`, so when one case in the block fails the rest of
//     the block is never attempted. Those tests asked nothing. They are
//     a CONSEQUENCE of the failure, not an independent phenomenon, and
//     reporting them as skips buried both facts.
//
//   * 3 skipped, against 2 on the quiet run. Exactly ONE deliberate
//     skip was new: kind-filter-1166's "composes with the rail chips",
//     whose guard was `test.skip((await chip.count()) === 0)` one line
//     after `page.goto('/')`. `count()` does not wait. On a slow box
//     the rail had not rendered, the count was 0, and the test removed
//     itself. That one IS the contention story, and it is a bug in the
//     guard rather than a property of the box.
//
// # So there are two claims to check, and they are not the same claim
//
//   1. Did every test that was DECLARED actually run? A test that did
//      not run asked nothing, and a summary that folds it into "skipped"
//      hides a shrinking suite behind a word that sounds deliberate.
//
//   2. Was every test that skipped ITSELF one we had already agreed
//      could? That is scripts/dogfood/skip-manifest.txt, which is a list
//      of test NAMES and not a count. The reasoning for names over a
//      tally is in that file's header, and it is the corpus ratchet's
//      lesson: two environments both reported "2 skipped" while
//      disagreeing about WHICH two.
//
// Exit 6 on either. Distinct from Playwright's own 1 (a test failed),
// from 2 (unknown mode), from the corpus ratchet's 3, from the
// instance-lock audit's 4 and from 5 (nothing ran at all), so a wrapper
// can tell "the suite asked fewer questions than it declared" from
// every other way this script ends.
//
// Usage: node skip-audit.mjs <report.json> <manifest.txt> [--full]
//
//   --full  this run selected the whole standalone suite, so a manifest
//           entry naming a test that is not in the report is STALE and
//           fails. Omitted for a --grep run, where absence means the
//           selection, not a deletion.

import { readFileSync, existsSync } from 'node:fs';

const GATE_RC = 6;

const reportPath = process.argv[2] ?? '.pw-results/report.json';
const manifestPath = process.argv[3] ?? '../skip-manifest.txt';
const full = process.argv.includes('--full');

const ESC = String.fromCharCode(27);
const c = (s, n) => `${ESC}[${s}m${n}${ESC}[0m`;

console.log(`\n${c('1;36', '==>')} Denominator audit (#1348, #1344)`);

if (!existsSync(reportPath)) {
  // The zero-test guard in run-ui.sh already owns this case and reports
  // it with its own code. Saying it twice would be noise.
  console.log(`  (no report at ${reportPath}, nothing to audit)`);
  process.exit(0);
}

let report;
try {
  report = JSON.parse(readFileSync(reportPath, 'utf8'));
} catch (e) {
  console.log(`  ${c('1;31', 'UNREADABLE')} ${reportPath}: ${e.message}`);
  console.log('  A run whose own report cannot be parsed has proved nothing about its');
  console.log('  denominator, which is the same claim this gate refuses.');
  process.exit(GATE_RC);
}

// -- flatten the report into one row per test -------------------------
//
// The JSON reporter nests suites by file and then by describe block, so
// the leaf title lives on the `spec` and the file path is carried down
// from wherever it was set. Both are collected: the file disambiguates
// two specs that happen to share a leaf title, and the leaf title is
// what a human copies out of the run's own output.
const tests = [];
const walk = (suite, file) => {
  const f = suite.file ?? file;
  for (const spec of suite.specs ?? []) {
    for (const t of spec.tests ?? []) {
      tests.push({
        file: spec.file ?? f,
        title: spec.title,
        line: spec.line,
        project: t.projectName ?? '',
        status: t.status,
        expectedStatus: t.expectedStatus,
        results: t.results ?? [],
        annotations: t.annotations ?? [],
      });
    }
  }
  for (const child of suite.suites ?? []) walk(child, f);
};
for (const s of report.suites ?? []) walk(s, s.file);

// -- classify, EXACTLY as Playwright's own summary does ---------------
//
// playwright/lib/runner/index.js `generateSummary()`. Copied rather than
// invented so the gate and the "3 skipped / 6 did not run" lines above
// it can never disagree about what a word means:
//
//   outcome 'skipped' AND some result 'interrupted'            -> interrupted
//   outcome 'skipped' AND (no results OR expected != skipped)  -> did not run
//   outcome 'skipped' otherwise                                -> skipped
//
// `expectedStatus === 'skipped'` is the load-bearing half. Playwright
// sets it when a `test.skip()` annotation is applied, whether it was
// written at declaration or evaluated in the body. A test the runner
// simply never got to has no such annotation, so the two are told apart
// by what the test ASKED FOR rather than by what happened to it.
const skipped = [];
const didNotRun = [];
const interrupted = [];
const failed = [];
const flaky = [];
let passed = 0;

for (const t of tests) {
  switch (t.status) {
    case 'skipped':
      if (t.results.some((r) => r.status === 'interrupted')) interrupted.push(t);
      else if (!t.results.length || t.expectedStatus !== 'skipped') didNotRun.push(t);
      else skipped.push(t);
      break;
    case 'expected':
      passed += 1;
      break;
    case 'unexpected':
      failed.push(t);
      break;
    case 'flaky':
      flaky.push(t);
      break;
    default:
      break;
  }
}

const name = (t) => `${t.file}:${t.line} > ${t.title}`;
const reasonOf = (t) => {
  const a = t.annotations.find((x) => x.type === 'skip' || x.type === 'skip-reason');
  return a?.description ?? '';
};
/** Where the guard that fired actually lives, when Playwright recorded it. */
const siteOf = (t) => {
  const a = t.annotations.find((x) => x.type === 'skip' && x.location);
  if (!a) return '';
  const f = a.location.file.split('/scripts/dogfood/ui/').pop() ?? a.location.file;
  return `${f}:${a.location.line}`;
};

// The header line run-ui.sh used to print for itself. NOTRUN is the new
// column and the reason this moved: it was inside SKIP, where a reader
// counting the suite could not see it.
{
  const fmt = (n) => String(n).padStart(3);
  const ms = report.stats?.duration ?? 0;
  console.log(
    `  Total: ${tests.length}   ${c('1;32', 'PASS:')} ${fmt(passed)}   ` +
      `${c('1;31', 'FAIL:')} ${fmt(failed.length)}   ` +
      `${c('1;33', 'SKIP:')} ${fmt(skipped.length)}   ` +
      `${c('1;33', 'NOTRUN:')} ${fmt(didNotRun.length + interrupted.length)}   ` +
      `FLAKY: ${flaky.length}   wall: ${Math.round(ms)}ms`,
  );
  console.log(
    `  SKIP is a test that ran a guard and stood down. NOTRUN is a test that never` +
      ` executed.`,
  );
}

let rc = 0;

// -- 1. tests that never ran ------------------------------------------
//
// Almost always downstream of a failure in a serial describe, in which
// case the run is red already and the failure is the more actionable
// code. Reported here regardless, because the point of #1348 is that
// nobody could see it: it is the difference between "the suite passed"
// and "the suite asked six fewer questions and passed the rest".
if (didNotRun.length || interrupted.length) {
  console.log(`\n  ${c('1;33', 'TESTS THAT NEVER RAN')} (${didNotRun.length + interrupted.length})`);
  for (const t of [...didNotRun, ...interrupted]) console.log(`    - ${name(t)}`);
  if (failed.length) {
    console.log(
      `\n  These are downstream of the ${failed.length} failure(s) above: a case that fails inside`,
    );
    console.log('  a `mode: serial` describe stops the rest of its block being attempted.');
    console.log('  Fix the failure and they run again. The run is already red on its account.');
  } else {
    console.log('\n  AND NOTHING FAILED, so this is not a serial cascade. Tests were declared,');
    console.log('  never attempted, and the pass count was reported as though they had been.');
    rc = GATE_RC;
  }
}

// -- 2. the manifest --------------------------------------------------
if (!existsSync(manifestPath)) {
  console.log(`\n  ${c('1;31', 'NO MANIFEST')} at ${manifestPath}.`);
  console.log('  Every skip in this suite is meant to be declared. Without the file there is');
  console.log('  nothing to check the run against, which is not the same as having nothing to');
  console.log('  check.');
  process.exit(GATE_RC);
}

/** @type {Array<{kind:string,file:string,title:string,why:string,line:number}>} */
const entries = [];
{
  const lines = readFileSync(manifestPath, 'utf8').split('\n');
  for (let i = 0; i < lines.length; i += 1) {
    const raw = lines[i].trim();
    if (!raw || raw.startsWith('#')) continue;
    const sp = raw.indexOf(' ');
    const kind = sp === -1 ? raw : raw.slice(0, sp);
    const rest = sp === -1 ? '' : raw.slice(sp + 1);
    const parts = rest.split(' :: ').map((s) => s.trim());
    if ((kind !== 'skip' && kind !== 'load') || parts.length < 3 || !parts[0] || !parts[1]) {
      console.log(`\n  ${c('1;31', 'MALFORMED MANIFEST LINE')} ${manifestPath}:${i + 1}`);
      console.log(`    ${raw}`);
      console.log('  Expected: skip|load <spec file> :: <exact test title> :: <justification>');
      process.exit(GATE_RC);
    }
    entries.push({
      kind,
      file: parts[0],
      title: parts[1],
      why: parts.slice(2).join(' :: '),
      line: i + 1,
    });
  }
}

const key = (file, title) => `${file} ${title}`;
const declaredSkips = new Map();
const declaredLoad = new Map();
for (const e of entries) {
  (e.kind === 'skip' ? declaredSkips : declaredLoad).set(key(e.file, e.title), e);
}

const declaredFired = new Set();
const undeclared = [];

if (skipped.length) {
  console.log(`\n  ${c('1;33', 'SKIPPED')} (${skipped.length})`);
  for (const t of skipped) {
    const k = key(t.file, t.title);
    const e = declaredSkips.get(k);
    if (e) {
      declaredFired.add(k);
      console.log(`    ${c('1;32', 'declared')}    ${name(t)}`);
    } else {
      undeclared.push(t);
      console.log(`    ${c('1;31', 'UNDECLARED')}  ${name(t)}`);
    }
    const site = siteOf(t);
    console.log(`                guard ${site || '(location not recorded)'}`);
    console.log(`                reason: ${reasonOf(t) || '(none given)'}`);
  }
}

if (undeclared.length) {
  console.log(
    `\n  ${c('1;31', 'A SKIP NOBODY AGREED TO')}: ${undeclared.length} test(s) removed themselves`,
  );
  console.log('  from this run and are not in the manifest, so the pass count covers less than');
  console.log('  the suite claims to.');
  console.log('\n  Before adding a line to the manifest, decide which of these it is:');
  console.log('   * the guard reads a value the page has not produced YET (a bare .count(),');
  console.log('     .isEnabled() or .isVisible() straight after a goto). That is the #1348');
  console.log('     defect. Ask the API instead, or wait for the signal the guard depends on.');
  console.log('     It is NOT declarable.');
  console.log('   * the guard is never false on CI. That is #1344: a deleted test with a');
  console.log('     comment. Set the precondition (helpers/public-mode.ts is the pattern) or');
  console.log('     delete the test. Also NOT declarable.');
  console.log('   * the environment genuinely cannot host the assertion, and the spec MEASURED');
  console.log('     that rather than guessing. Then it is declarable, and the justification has');
  console.log('     to say what about the environment decided it.');
  console.log(`\n  Manifest: ${manifestPath}`);
  rc = GATE_RC;
}

// -- 3. is the manifest still describing this suite? ------------------
const present = new Set(tests.map((t) => key(t.file, t.title)));
const stale = entries.filter((e) => !present.has(key(e.file, e.title)));
if (stale.length) {
  if (full) {
    console.log(`\n  ${c('1;31', 'STALE MANIFEST ENTRIES')} (${stale.length})`);
    for (const e of stale) {
      console.log(`    ${manifestPath}:${e.line}  ${e.kind} ${e.file} > ${e.title}`);
    }
    console.log('\n  No test by that name is in this run. A renamed or deleted test must not');
    console.log('  leave its permission standing: the next test to land on that name would');
    console.log('  inherit it. Update the line or remove it.');
    rc = GATE_RC;
  } else {
    console.log(`\n  (${stale.length} manifest entry(s) not in this selection, not checked:`);
    console.log('   this run was filtered)');
  }
}

// -- 4. prune candidates ----------------------------------------------
//
// NOT a failure. A skip that is legitimately environment-dependent does
// not fire on the other environment, and failing here would make one
// file unkeepable across CI and a workstation at once. It is printed so
// that an entry whose guard was later FIXED, and now fires nowhere, is
// visible to review rather than accumulating as silent permission.
if (full) {
  const unfired = entries.filter(
    (e) =>
      e.kind === 'skip' &&
      present.has(key(e.file, e.title)) &&
      !declaredFired.has(key(e.file, e.title)),
  );
  if (unfired.length) {
    console.log(`\n  ${c('1;36', 'declared but did not fire')} (${unfired.length})`);
    for (const e of unfired) console.log(`    ${e.file} > ${e.title}`);
    console.log('  Expected when the environment can host the assertion. If one of these has');
    console.log('  not fired anywhere in a long time, its guard is probably dead: prune it.');
  }
}

// -- 5. the load register ---------------------------------------------
//
// Not a permission and not a quarantine. #1348's third acceptance item
// asks that browse-lookahead-1159 stop being rediscovered every sprint;
// this is that, without deleting the coverage a quarantine would.
if (failed.length || flaky.length) {
  const known = [...failed, ...flaky].filter((t) => declaredLoad.has(key(t.file, t.title)));
  if (known.length) {
    console.log(
      `\n  ${c('1;33', 'KNOWN LOAD-SENSITIVE')} (${known.length} of ${failed.length + flaky.length})`,
    );
    for (const t of known) {
      console.log(`    ${name(t)}`);
      console.log(`      ${declaredLoad.get(key(t.file, t.title)).why}`);
    }
    console.log('  This is context, NOT an excuse: the run is still red and the failure is');
    console.log('  still real until the box is ruled out.');
  }
}

if (rc === 0) {
  if (didNotRun.length || interrupted.length) {
    // ⛔ NOT "the denominator holds". The tests above asked nothing, and
    // printing the green line under a list of them is the mixed signal
    // this gate exists to remove. The run is red on the failure that
    // caused them, and this line must not read like an all-clear.
    console.log(
      `\n  ${c('1;33', 'Denominator SHORT')} by ${didNotRun.length + interrupted.length}: no undeclared skip,`,
    );
    console.log('  but the pass count above is over a smaller suite than the total claims.');
  } else {
    console.log(
      `\n  ${c('1;32', 'Denominator holds')}: every declared test either ran or skipped by agreement.`,
    );
  }
}
process.exit(rc);
