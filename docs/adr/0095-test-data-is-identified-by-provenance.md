---
id: "0095"
title: Test-created rows are identified by provenance, never by naming heuristics
status: accepted
date: 2026-08-21
area: process
phases: []
supersedes: []
related:
  - "0068"
  - "0080"
tags:
  - testing
  - seed
  - data-safety
excerpt: >-
  The persistent dogfood database mixes seeded content with rows left behind by
  test runs, and they must be separable to sweep one without destroying the
  other. The separator is provenance — the seeder stamps
  `acquisition_source` and the upload API never does — because every naming,
  extension and date heuristic was tried, all of them failed, and one would
  have deleted real assets.
---

## Context

The coding stack's database is **persistent by design**: reseeding costs 10–15 minutes per sprint,
so the stack and its corpus survive between sprints. The cost is that every dogfood run leaves rows
behind — measured at roughly **50 assets per run** across ~25 specs, none individually broken enough
to notice.

By 2026-08-20 this had silted up badly enough to produce **deterministic false failures**: the
newest 200 assets were 191 `.txt` and 9 `.glb` with **zero images**, so a spec needing a raster
image could not find one, and the newest feed post had a blank title, so a spec deriving its search
term from it searched for "Untitled" and found nothing. Nine specs failed for reasons in nobody's
diff.

Sweeping the leaked rows requires answering one question: **which rows are test fixtures and which
are real seeded content?** Getting it wrong in the destructive direction is unrecoverable.

## Decision

**1. Provenance is the separator.** The seeder stamps `metadata->'acquisition_source'` on every
asset it creates (`Kenney.nl`, `Pexels`, `Google Fonts`, `Wikimedia Commons`, `Khronos Sample
Models`, `Project Gutenberg`, `Blender Foundation`, `Polyhaven`, …). **The upload API never writes
that key.** So an asset carrying it came from the catalogue and an asset lacking it was created
through the API — which, on this stack, means a test made it.

Verified at adoption: it partitioned all **3,491** rows, agreeing with an independent signal on
every one, with **zero disagreements**.

**2. ⛔ Naming, extension and date heuristics are forbidden for this purpose.** Not discouraged —
forbidden, because each was tried and each failed, and the failure mode is data loss:

- **Extension.** *"Nothing real in this corpus is a `.txt`"* is **false**: three Google Fonts `OFL`
  licences plus Kenney.nl's `Tilemap` and `Tilesheet` are genuine seeded assets. That rule would
  have deleted all five.
- **Title/naming.** The convention-based rules found ~285 fixtures against **1,544** actual, because
  the largest families are PNGs named `Dogfood PNG (ui-13-…)` that no extension or title pattern
  catches.
- **Date cutoff.** Cannot separate them: a dogfood run overlapped the seed by seven seconds — first
  fixture collection at 01:46:15, last real post at 01:46:22.

The common failure is that all three infer *identity* from *appearance*. Provenance records identity
at creation time, which is the only moment it is known for certain.

**3. The sweep is dry-run by default and aborts on contradiction.** It reports what it would remove
before removing anything, and if the provenance signal disagrees with any independent check it stops
rather than guessing. A too-broad sweep is worse than a dirty database, because one is recoverable
and the other is not.

## Consequences

- ⛔ **This creates a load-bearing invariant on PRODUCT code: the upload API must never write
  `metadata->'acquisition_source'`.** If any upload path starts stamping it, `aa sweep-fixtures`
  stops being able to tell a user's asset from a fixture, and the next sweep deletes real content.
  Anyone adding provenance to the upload path must change the sweep first.
- The seeder must keep stamping it. A future importer that creates catalogue content **must** stamp
  it too, or its content becomes indistinguishable from a fixture.
- The sweep is surfaced as a **dry run** in `aa-sprint-reset.sh`, so each sprint starts from a known
  corpus rather than an accumulating one.
- ⚠️ **A sweep clears accumulation; it does not stop the leak.** The specs still leak ~50 assets per
  run, bounded by a ratchet (`scripts/dogfood/corpus-budget.txt`) whose target is all zeroes
  (#1247). A ratchet nobody tightens is a permanent allowance.
- This applies to the **persistent dogfood stack**. CI seeds a fresh coverage-selected corpus per
  run (ADR 0080) and has no accumulation problem — which is exactly why the failures it caused were
  invisible in CI and only bit locally.

## Alternatives considered

**Reseed instead of sweeping.** Rejected: it discards the persistence the stack exists for, at
10–15 minutes per sprint.

**Fix the leaking specs first and never sweep.** Rejected as the *only* answer — it is the right
long-term target (#1247), but ~25 specs leak 1–10 rows each, so it is its own arc, and the
accumulated rows still need clearing meanwhile.

**A dedicated `is_fixture` column.** Rejected: it is a second source of truth that a test could
forget to set, whereas provenance is stamped by the code path that already knows the answer, and is
absent by default for anything created through the API.

---

## Amendment, 2026-08-23 (#1247, PR #1261): what "cleaned up" MEASURES

This ADR decided how to tell a fixture from real content. Sprint 9 found that the *invariant built
on top of it* was measuring the wrong quantity, which is a different failure and worth recording
beside the identification rule.

**Every delete a dogfood spec can call is a soft delete or an archive.** `collections/queries.sql:138`,
`posts/queries.sql:56` and `assets/queries.sql:93` all `SET deleted_at = NOW()`;
`metadata/queries.sql:146` sets `status = 'archived'`; `user` has no delete endpoint at all. So
`count(*)` **can never come down through any API a spec can reach**, and the corpus census scored a
spec that created three rows and correctly deleted all three as a `+3` leak.

The scale of the error was visible the whole time and nobody looked: **151 collection rows, 7 live.**
Four of the five tables in the budget were already clean before any fix, and #1247 had been planned
as "roughly 25 specs each leaving one to ten rows behind."

**Decision: the corpus invariant is the LIVE row count** — a run must end with the live counts it
began with. That is the only quantity a spec can control, so it is the only fair thing to score it on.

**And the census reports BOTH:**

```
Corpus census (before): assets|posts|collections|fields|users = 2015|851|7|27|36
                        (raw incl. soft-deleted: 3063|1299|347|476|36)
```

The second half is what keeps this from being a re-measurement that flatters the number. Soft-deleted
rows really do accumulate — one full run adds ~59 assets, ~25 posts, ~16 collections, ~22 fields to
the raw counts — and whether that ever needs a retention policy is a product question, not a test one.
Hiding it would have made that question unaskable.

⚠️ **Attribution may use names; DELETION may not.** The fixture ledger identifies a leaking row by
whatever stamp the creating spec left, which is fine for a report. Deletion stays on this ADR's
provenance rule (`NOT (metadata ? 'acquisition_source')`), because an appearance-based rule for
deletion is what nearly destroyed five real assets in sprint 6a — the reason this ADR exists.

⚠️ **The invariant still does not run in CI.** `run-ui.sh` enables the census only when the port in
`STUDIO_A_HOST` matches the checkout's `VITE_HOST_PORT`, and CI drives the suite at
`http://app.aa:8080`. So the all-zeroes budget currently guards a developer's laptop and nothing
else — which is how the leak reached ~50 rows a run unnoticed. Tracked as **#1263**.

---

## Amendment, 2026-08-24 (sprint 10): the two environments differ, and tests must not encode either

The 2026-08-23 amendment recorded what "cleaned up" *measures*. This is its sibling, added because
the same asymmetry bit **twice in one sprint, in two unrelated forms** — which makes it a property
of the system rather than a pair of bugs.

**The asymmetry.** The dev/coding stack carries a **persistent, deeply-seeded** database (~2015
live assets, reused across sprints). CI builds a **fresh, shallow** one every run (~150 assets).
Anything calibrated against one misreports against the other, in either direction:

| bitten as | what happened |
|---|---|
| **fixtures** (#1263) | the suite's one-time fixture cost — 4 assets + 4 posts from a top-up loop, 4 users that no API can delete — is invisible on a persistent stack because it amortises, and is paid on **every** CI run because every CI run is a first run |
| **pixels** (#1223's spec) | a scroll test hardcoded a 400px park point measured against 5019px of dev-corpus scroll height. On CI's corpus a 9-post collection has a scroll range of **exactly 0** at both 1080p and 390px — not a smaller number, *no park point at all* |

**Consequences for how tests are written here:**

1. **Derive from what is measured at runtime, never from a constant observed on one stack.** A
   pixel, a row count, an index into a list — all encode an environment. `items[0]` is the same
   mistake as `400`: it presumes the first collection is deep enough.
2. **Separate the assertions that need corpus depth from the ones that do not.** #1223's spec ended
   up with four cases that read the lock directly and two that need something to scroll; only the
   latter can be defeated by a shallow corpus, and the discriminating cases still run everywhere.
3. **A skip must be loud.** A test that silently skips where it cannot run is the vacuous green
   this ADR's whole family exists to prevent (see also #1272 — `run-ui.sh` exits 0 when Playwright
   finds no tests).
4. ⚠️ **Measure at the worst moment, not a convenient one.** `<main>`'s `margin-top` is driven by
   the auto-hiding chrome, so hiding the navbar makes `<main>` taller and its scroll range
   *smaller*. A range measured with the chrome up is an over-estimate that the first downward
   scroll invalidates.

**The standing fix** is to remove the asymmetry rather than teach every spec to tolerate it: move
the one-time fixtures into `aa seed` so a fresh CI database starts already-fixtured (**#1270**).
Until that lands, **#1263's census-in-CI is held** at `wip/1263-census-in-ci` rather than merged,
because a guard that is permanently red is the pathology #1245 exists to end.

---

## Amendment, 2026-08-25 (#1285): a loud skip is honest and still leaves a hole

The 2026-08-24 amendment named two axes on which the dev and CI corpora differ. Sprint 11b found a
**third**, and it is the one that defeats the previous amendment's own remedy.

That amendment said: *"Skips must be loud."* They now are — and
`modal-scroll-lock-1223 › 1080p` **has never run in CI**, skipping correctly with

> the deepest seeded collection offers only 32px of scroll range (needs 250px)

CI seeds with `--profile ci` (`ui-pr.yml:343`), a **coverage-complete subset** — 162 assets / 94
posts against a workstation's 2,008 / 902. It is built to touch every media type and code path, not
to be deep. So the gate for a *scroll-locking* bug cannot exercise scroll locking.

**The three axes, so the next one is recognisable:**

| axis | mechanism |
|---|---|
| fresh vs persistent | one-time costs never amortise in CI (#1263) |
| deep vs shallow | a constant measured on the dev corpus (#1223's 400px park) |
| **subset vs full** | `--profile ci` omits *depth* by design (#1285) |

**Consequence, and it amends the previous amendment rather than repeating it:** loudness makes a
skip *honest*, not *harmless*. A test that never runs measures nothing, and nothing in the pipeline
currently notices that a named case has skipped on every run for a week.

⭐ **The question to ask of an environment-sensitive assertion is not "does it pass?" but "does it
RUN, everywhere it is meant to?"** Read the run output for skips, not only for failures — and
prefer provisioning the shape (the `post-band-format-1190:71` pattern this ADR's family keeps
arriving back at) over depending on whatever the corpus happens to hold.

---

## Amendment, 2026-08-26 (#1276, PR #1297): this ADR's own rule was broken by the tool it governs

**Decision 3 above says "the sweep is dry-run by default and **aborts on contradiction**." That
behaviour was true and it was UNTESTED.** `TestSweep_ContradictionAbortsEverything` — the test that
proves the abort — **had never executed since #1245.**

`sweep_test.go`'s helper asked for `asset_types.id`. The column is **`ref`**. Every call raised
42703, and the helper caught *any* error:

```go
if err := pool.QueryRow(ctx, `SELECT id FROM asset_types ORDER BY id LIMIT 1`).Scan(&id); err != nil {
    t.Skipf("no asset_types in this database: %v", err)
}
```

Three tests skipped, silently, behind a message that **blamed the database for a bug in the query**.

⛔⛔ **The bitter part is that this ADR already forbade it.** The 2026-08-24 amendment, consequence
3, reads: *"A skip must be loud. A test that silently skips where it cannot run is the vacuous
green."* The rule was written here, and the tool this ADR exists to make safe broke it — for
roughly two weeks, unnoticed, while the safety property it guards was cited (by the planning agent,
in the sprint-14 brief) as settled.

**What this changes:**

1. **Decision 3 stands and is now genuinely verified.** All six tests execute; a deliberate
   contradiction was introduced and the sweep refused to delete anything and named the row.
2. ⭐ **"A skip must be loud" is sharpened: a skip must be reachable for exactly ONE reason, and
   must prove that reason.** Skip on the specific precondition (`pgx.ErrNoRows`); anything else is
   `t.Fatal`. **A catch-all skip is a silent `return` wearing an explanation** — and a *plausible*
   explanation is worse than none, because it gets accepted instead of investigated.
3. **Green is a claim about the tests that RAN.** Nobody checks the denominator. For any invariant a
   destructive tool depends on, prove the test **fails** when the invariant is broken, rather than
   confirming the code path exists.

⚠️ Recorded because an accepted ADR is what people trust *instead of* reading the code. This one
asserted a safety property in the present tense for two weeks while nothing was checking it.

---

## Amendment, 2026-08-27 (#1320, PR #1327): a fourth axis, and this one is invisible by construction

The 2026-08-25 amendment named three axes and asked that the next one be recognisable. Here it is,
and it is not a difference in the corpus's *shape*.

Sprint 14f gave `aa seed` the ability to report what it declined to touch. Its first run against the
persistent development database said:

```
resumed=861  drifted=779
```

**712 titles, 693 dates, 326 memberships, 29 visibilities**, deterministic across consecutive runs.

| axis | mechanism |
|---|---|
| fresh vs persistent | one-time costs never amortise in CI (#1263) |
| deep vs shallow | a constant measured on the dev corpus (#1223's 400px park) |
| subset vs full | `--profile ci` omits *depth* by design (#1285) |
| **stale vs current** | the loader could add a row and never correct one, so a persistent database never converges on its catalogue (#1320) |

⛔ **The three earlier axes are differences you could in principle notice by looking. This one was
not.** The loader reported the same clean result whether it had applied the catalogue or ignored it,
so the divergence had no surface at all. It was found by building the report, not by observing a
symptom, and the number was larger than anyone had guessed.

⭐ **The rule this adds to the family: an environment can differ from its source not only in shape
but in AGE, and age has no shape.** A corpus that is the right size, the right depth and the right
breadth can still be answering with values from months ago. Ask what makes a test environment
*converge* on its inputs, and if the answer is nothing, the environment is a snapshot wearing a
pipeline's clothes.

⚠️ The remedy is deliberately not automatic. The loader reports and points at the destructive full
reset rather than updating rows itself, for the reason ADR 0081 gives about shipped content: an
override is data resolved over the shipped value, not silently replaced by it. Closing the gap for
the published corpus is #1319.
