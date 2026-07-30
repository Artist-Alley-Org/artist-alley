---
id: "0080"
title: CI's fixture is selected by coverage, and a missing coverage class fails the seed
status: accepted
date: 2026-07-30
area: process
phases: []
supersedes: []
related:
  - "0012"
  - "0060"
  - "0068"
tags:
  - testing
  - ci
  - seed
  - quality
excerpt: >-
  Seeding CI's full 1,947-asset catalogue cost 14 minutes of a 20-minute job to
  feed a 3-minute suite, and cutting it by volume shed the relations the suite
  exists to test. The fixture is now chosen by what it must cover rather than
  how large it is, and a catalogue that cannot supply a required coverage class
  fails the seed instead of quietly producing a suite that passes without
  exercising it.
---

## Context

The Playwright job seeded the entire catalogue — 1,947 assets — and that seed took **14m13s
of a 20-minute job**. The suite it fed ran in 2m49s. The seed step's own run-to-run spread,
659s to 1020s across eight runs, was larger than the entire test phase, and on 2026-07-30 it
tipped `dev` over the cap by one second (20m1s against 20m0s).

A per-extension cap already existed (`--limit-per-extension`) and was the obvious lever. It
is the wrong lever, because it selects **assets** and treats posts as collateral: a post
survives only if every one of its assets does. Measured against the real catalogue, keeping
20 assets per extension retained 84 of 859 posts and left most collections empty. Posts are
what carry collection membership, author, team, tags and state — so shrinking by asset count
sheds precisely the relations the suite is there to exercise. Volume and coverage are not the
same axis, and the existing control moved the wrong one.

The deeper problem is the one ADR 0068 keeps finding: **a fixture that quietly lacks a class
produces a green suite that proves nothing about that class.** An untextured 3D catalogue
shipped green twice. A companion-parsing test passes perfectly against a dataset with no
companions in it, because it asserts on rows that were never meant to exist. Nothing in the
seed had any notion of what it was obliged to contain.

## Decision

**CI's fixture is chosen by coverage, not by size, and a coverage gap is a hard error.**

1. **Select posts, close over their assets.** Greedy set-cover runs over *posts*, pulling in
   the assets each one references, so every retained post is whole by construction and no
   relation is left dangling. This is the inversion that makes the rest work: coverage is the
   selection axis and volume is the consequence, rather than the other way round.

2. **A depth floor sizes the fixture, not the cover.** Minimum cover is degenerate — a single
   tile cannot exercise a grid, a masonry layout or pagination. The floor (at least *K* posts
   per collection and *K* assets per extension, bounded by supply) is what determines the
   fixture's size. The cover determines only that nothing is missing.

3. **The floor is bounded by render cost as well as supply.** Cheap classes pad; expensive
   ones do not. Video extensions get a lower floor than the default because sprite generation
   dominates render CPU and image assets already supply grid density. This is safe *because*
   the cover runs first: reducing the floor can never reduce coverage, only padding.

4. **Prefer the cheapest candidate that covers the same thing.** A size-blind implementation
   selected 11% of assets and still carried 82% of the bytes.

5. **A missing coverage class fails the seed.** Two tiers, both fatal:
   - the coverage universe derived from the mounted catalogue must come out fully covered by
     the selection; and
   - a declared floor of required *classes* — stated in code, independent of whatever
     catalogue is mounted — must be present in the result.

   The second tier is the one that matters. A universe derived from the fixture can only
   detect that selection lost something; it cannot detect that the *dataset* never had it. The
   declared list is deliberately **classes, not counts**: a dataset refresh that drops a long
   tail should not break CI, but one that drops every font, or every model with external
   textures, must.

6. **Coverage is established from the bytes where the bytes are what matters.** Whether a
   model declares external textures is a property of the file, not of a manifest row. A check
   that counts rows by extension passes against a catalogue whose models are all
   self-contained — which is the exact failure this clause exists to prevent.

7. **The full catalogue remains the demo's seed.** The coverage profile is a CI-only shrink.
   Combining it with the per-extension cap is refused rather than merged: the two select on
   opposite axes and the cap runs second, so together they would silently re-open the hole the
   profile closes.

## Consequences

- The CI seed dropped from 14m13s to 3m14s and the job from ~20 minutes (timing out) to
  12m37s. The suite itself got *faster* — 2m49s to 2m20s — because it no longer competes with
  the preview worker for CPU.
- **The declared class list is normative and will need amending.** Adding a renderer, a
  container format, or a relation the suite asserts on means adding it there. An entry that is
  never added is a class CI cannot prove it exercises; this ADR is the reason the list is not
  merely advisory.
- A dataset refresh can now fail CI at the seed step. That is the intent — it is strictly
  better than the alternative, which is a green suite over a fixture missing the thing under
  test.
- **Shrinking the seed moved a contention problem rather than removing it.** Preview jobs are
  enqueued, not awaited, and the long seed had been acting as an accidental drain window. With
  a short seed the render tail lands on the first minutes of the suite instead. CI now waits
  for the preview queue explicitly — bounded, non-fatal, and scoped to preview job types,
  because recurring coordinator jobs sit pending with a future schedule for the life of the
  stack and a naive drain-to-zero would never terminate.
- That wait still expires with video renders in flight. It is a mitigation; the cure is
  re-encoding the handful of large source files that dominate render CPU, which is a dataset
  change and cannot be reached from code.
- **Generalises ADR 0068.** Its 2026-07-29 amendment asks whether a fixture can *reach* the
  state a test claims to cover. This extends the same idea from realism to completeness: a
  fixture must provably *contain* every class the suite claims to exercise, and the seed —
  not a reviewer — is what enforces it.
