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
