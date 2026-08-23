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
