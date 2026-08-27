---
id: "0098"
title: Seed assembly is deterministic, and a derived id is a function of what identifies the thing
status: accepted
date: 2026-08-26
area: process
phases: []
supersedes: []
related:
  - "0095"
  - "0097"
tags:
  - seed
  - determinism
  - data-safety
excerpt: >-
  The seed assembler derives post ids from an anchor that does not identify
  the post, so two different roundups collide on one id and one of them can
  never be seeded. It also emits timestamps that change between runs, and
  `posted_at` is copied from one of them — so feed order moves, and a post
  can drift across the fixture sweep's protection boundary. Assembly must be
  a pure function of its inputs, and a derived id must be a function of
  identity.
---

# Seed assembly is deterministic, and a derived id is a function of what identifies the thing

## Context

`seed/scripts/sanitize_and_assemble.py` builds the seed profiles. Two of its derivations are not
functions of what they claim to describe.

**1. Colliding post ids (#1293).** Roundup posts are built from a *random* sample and identified by
the sample's anchor:

```python
sample = rng.sample(team_assets, rng.randint(5, min(10, len(team_assets))))
anchor = max(sample, key=lambda x: x.updated_at)   # most-recent asset IN THIS SAMPLE
...
"id": stable_uuid("post", "roundup", team_name, anchor.id)      # :1071
```

A team's genuinely most-recent asset lands in many samples, so different roundups — different
membership, different titles — derive the **same id**. Measured before repair:
`studio-a.posts.json` held **873 rows under 861 ids**, `studio-b` **771 under 767**, and every
colliding pair *disagreed*: `"— 7 drops"` beside `"— 5 drops"` beside `"— 8 drops"` under one id.
`stable_uuid("post", "sprint", project_name, label, anchor.id)` (`:1127`) has the identical shape.

⚠️ **The consequence is not a duplicate — it is a disappearance.** `aa seed` keys on the stable id,
so of *n* colliding rows exactly one survives and the rest can never be seeded. Sixteen roundup
posts existed in the catalogue and could not reach a database.

**2. Unstable timestamps (#1296).** Comparing the repo profile against the published site over the
861 shared ids: **840 differ in `created_at`**, 297 in `updated_at`, 390 in member order — while
**membership as a set differs on zero**. The two sides agree on what every post contains and
disagree on when it was made.

That is not cosmetic, because `created_at` is consumed:

- `posted_at = u.created_at` in the seeder — so **feed order moves between assemblies**.
- The fixture sweep protects a post when `author_user_ref <> 1 OR created_at < '2026-08-17'`
  (ADR 0095). **42 seeded posts sit at or after that boundary**, and seeded posts are authored by
  ref 1 — so a post whose timestamp drifts across the line stops being protected and becomes
  **deletable by `aa sweep-fixtures`**.

## Decision

**Assembly is a pure function of its inputs, and identity is derived from identity.**

1. **A derived id must be a function of what distinguishes the thing it names.** If two outputs can
   differ in content, their ids must differ. An input that merely *accompanies* the thing — an
   anchor, a sample's extremum, a dominant value — does not identify it and must not be the whole
   key.
2. ⛔ **A collision is a silent deletion, not a duplicate.** Anything keyed on a stable id that can
   collide loses rows on the way in. Uniqueness of derived ids is therefore an assembly-time
   invariant, asserted before the profile is written — not discovered downstream.
3. **Re-running assembly over unchanged inputs produces an unchanged profile.** Timestamps, member
   order and every other emitted field are derived, not sampled from the clock or from unordered
   iteration. Randomness stays seeded and its seed stays an input.
4. **Fields that downstream behaviour reads are load-bearing**, whatever they look like.
   `created_at` sets `posted_at` and participates in a *deletion* predicate, so its instability is a
   correctness bug and not churn in a generated file.

## Consequences

- ⚠️ **Correcting an id derivation MOVES ids** — 64 posts (29 roundups, 35 project sprints) in
  site_a. Under ADR 0097 the publish guard will see a destination holding content the source does
  not and **refuse**, which is the guard working. The migration is therefore deliberate work with a
  reconcile step, not a quiet regeneration.
- The repair already applied to the *data* (sprint 14, PR #1297) does not fix this: the assembler
  was not touched, so the next assembly reintroduces the collisions. **Data repair without generator
  repair is a delay, not a fix.**
- Determinism is testable and should be tested: assemble twice from one input and diff. That check
  did not exist, which is why 840 drifted timestamps reached a published dataset unnoticed.
- ⭐ This is the same family as ADR 0095's rejection of naming heuristics: **an identifier must
  carry identity, not resemblance.** There, appearance was a bad proxy for provenance; here, an
  anchor is a bad proxy for a post.
