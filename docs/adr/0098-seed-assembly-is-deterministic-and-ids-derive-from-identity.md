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

- ⚠️ **Correcting an id derivation MOVES ids** — **68** posts in site_a. ⛔ **Corrected 2026-08-26
  (PR #1305): this ADR first said 64** (29 roundups + 35 project sprints). The missing four are
  `cinematics_showreel`, which carries the same derivation three lines further down and was simply
  not counted. Measured: `{team_roundup: 29, project_sprint: 35, cinematics_showreel: 4}`.
  ⚠️ And the defect is wider still — `multi_asset` (**107** more site_a posts) shares it, which
  would take a migration from 68 to 175. That is scoped separately (#1310), deliberately: **a
  measurement in an accepted ADR should not quietly grow into a different decision.**
  Under ADR 0097 the publish guard will see a destination holding content the source does not and
  **refuse**, which is the guard working. The migration is therefore deliberate work with a
  reconcile step, not a quiet regeneration.
- The repair already applied to the *data* (sprint 14, PR #1297) does not fix this: the assembler
  was not touched, so the next assembly reintroduces the collisions. **Data repair without generator
  repair is a delay, not a fix.**
- Determinism is testable and should be tested: assemble twice from one input and diff. That check
  did not exist, which is why 840 drifted timestamps reached a published dataset unnoticed.
- ⭐ This is the same family as ADR 0095's rejection of naming heuristics: **an identifier must
  carry identity, not resemblance.** There, appearance was a bad proxy for provenance; here, an
  anchor is a bad proxy for a post.


---

## Amendment, 2026-08-26 (PR #1305): Context §2 was wrong, and so was the danger it named

Two claims in this ADR's Context were false when written. Both were mine, and the sprint that
implemented the ADR found them.

**1. ⛔ The generator was never drifting. The published output was HAND-EDITED.**

Context §2 attributes 840 differing `created_at` values to unstable assembly. It is not. The post
`created_at` is byte-identical to its anchor asset's, and the assets agree with the archive on
`created_at` across all 2,005 shared rows — **zero differences**. The archive itself holds the
evidence:

| backup on the share | fields differing from the live `posts.json` |
|---|---|
| `posts.json.pre-1217.bak`, `.pre-1260.bak` | **none** |
| `.pre-feedcurate.bak` | `created_at` **833** |
| `.pre-feedcurate2.bak` | `created_at` **794** |
| `.pre-hero.bak` | `created_at` **840**, `asset_ids` **390**, `updated_at` **297**, `title` 6 |
| `.pre-hero2`, `2b`, `2c` | progressively fewer |

**Eight backups across two editing campaigns.** They account for every figure §2 cites. The
published dataset carries deliberate editorial curation that **no pass can reproduce** — the same
class as #1275, and larger, because this is authored work rather than a stale measurement. Tracked
as #1309.

⭐ **Decision 3 survives this and is arguably strengthened**: assembly *should* be reproducible, and
the reason to care is exactly that someone hand-curating the output is invisible until a
regeneration destroys it. But it was justified here with the wrong evidence.

**2. ⛔ The deletion predicate it names no longer exists.**

Context §2 says the sweep protects a post when `author_user_ref <> 1 OR created_at < '2026-08-17'`,
and calls a drifting timestamp a route to deletion. On `dev` that rule is now
`rules.go:187` — **`Protected: id = ANY($1::uuid[])`** — protecting exactly the seed catalogue's
ids, and its own comment documents replacing the claim this ADR relied on.

⚠️ **The danger did not go away; it moved to the id** — which is what Decision 1 changes. So an
id migration is the thing that can drop a real post out of protection. Measured with both
catalogues against one database: **68 posts move REAL → UNCLASSIFIED**, FIXTURE stayed 178,
CONTRADICT stayed 0. Unclassified is reported and never deleted, so the sweep does not consume
them — but that had to be *measured*, not assumed, and this ADR pointed at the wrong predicate.

**3. `--recompose-posts` did not run at all** when this ADR was written — `TypeError:
AssetRecord.__init__() got an unexpected keyword argument 'replaced_source_path'`. Post composition
had been unregenerable for as long as the profiles had been annotated, so none of §2 was testable
until PR #1305 fixed it.

⭐ **The lesson worth carrying**: I reasoned about the generator's code and offered two hypotheses
for the drift. Both were wrong, and the filesystem held the answer the whole time in files named
`pre-feedcurate` and `pre-hero`. **When an output disagrees with its generator, look at the output's
neighbours before theorising about the generator.**

---

## Amendment, 2026-08-27 (#1318, #1310, #1309, #1314): what landed, and the one thing that did not

**Decision 1 is now complete for bundles.** `bundle_post_id` takes the post's membership and
nothing else. The old key named the anchor member, which is a value that merely *accompanies* the
post, and this ADR's whole argument is that identity must not derive from an accident of ordering.
No cluster name sits in front of the membership: chunks partition a cluster and clusters partition
the pool, so membership alone cannot collide, while adding `(collection, team, asset_type)` would
only move the id when a member's collection was renamed. Migrated: **107** ids in studio-a, **216**
in studio-b, **295** in dataset, and `migrate_post_ids --check` now covers **618** more posts.

⚠️ **The improvement claimed for this change is smaller than predicted, and the real one is
elsewhere.** Dropping one asset moves **15 of 340** bundle ids under both the old key and the new
one, because #1296 made the chunk cycle per-cluster, so a removal re-partitions its cluster and
every later chunk changes membership and anchor alike. There is no gain there. The gain is a case
the two keys genuinely disagree on: the anchor key read `updated_at` while the cluster sorts by
`created_at`, two independent fields, so editing one asset's `updated_at` moved a bundle id while
changing no membership at all. Measured on studio-a: **1 of 340** under the anchor key, **0** under
the new one.

⭐ Recording this because the predicted benefit and the actual benefit were different benefits. An
identity rule justified by a collision argument was validated by a **stability** argument instead,
and reporting the predicted number would have been wrong in a way nothing downstream would catch.

**The hand curation is now data.** The previous amendment established that the published
`posts.json` carried editorial work no pass could reproduce, and left it as a finding. Owner ruling,
2026-08-27: *"I like the handmade edits. Codify them."* `post-curation.site_a.json` holds **1,513
values across 841 posts**, recovered from the eight `posts.json.pre-*.bak` backups walked in mtime
order. Nothing changed after `pre-1217`, so a post touched by several passes needs no conflict rule
and there is none. The backup chain says which `(post, field)` pairs a human touched; the live file
says what the last pass left there.

⛔ **A silent no-op was one filename away.** Filing this under the existing
`{stem}-posts.{site}.json` convention would have routed it through `merge_posts`, which skips a post
whose id it already holds. Every curated post already exists, so all 841 would have been skipped and
the run would have **reported success**. `apply_post_curation` amends in place instead, and a test
now pins `merge_posts`'s skip so nobody re-routes it there quietly. This is the same shape as the
guard-suite failures from #1300: a pass that covers nothing reads exactly like a pass that covers
everything.

### ⛔ The claim this ADR should now be careful about: the profile is NOT assembler output

#1314 alleged that `post_kind` disagreed with the title template on 228 posts, which would have made
a wrong kind feed an id. It does not reproduce: re-running the generator's own title formatter over
each post's own fields, a fresh assembly disagrees on **zero** across all 1,103 site_a posts.

But the **committed** profile disagrees on 374, and the reason is more interesting than the issue
was. None of the three causes is a generator defect:

| count | cause |
|---|---|
| 244 | `asset_group` posts carrying a collection-chunk title from an **older generator** |
| 83 | `solo_showcase` and `multi_asset` titles **authored by upgrade documents**, which no template produced |
| 47 | `revision_*` and `video_*` titles embedding a member asset's name as it stood **before** the HQ replacement renamed it |

⚠️ **So the committed profile is a historical accumulation, not the current assembler's output**, and
it holds **863** posts where a fresh assembly produces 1,103. Decision 3 says assembly is
deterministic, and it is; what this ADR did not say is that **the committed artifact is not the thing
that determinism is a property of.** Every guard that compares the profile against a fresh assembly
is therefore comparing two different kinds of object, and will keep finding differences that are
history rather than defects. Tracked separately.

---

## Amendment, 2026-08-27 (#1322): the divergence has a cause, and it removes an option

The amendment above recorded that the committed profile holds **863** posts where a fresh assembly
produces **1,103**, and called it a historical accumulation. That was the symptom. The cause was
found while planning the next sprint, and it is more consequential than the symptom.

**`group_id` is absent from the committed asset profiles.** Zero non-null values in both
`studio-a.assets.json` and `studio-b.assets.json`, and the key is not present at all. It is absent
from the published `MANIFEST.json` too.

`derive_posts` pass 1 is the authoritative pass and keys on exactly that field
(`sanitize_and_assemble.py:1196`, minting the id at `:1400`). So a recompose emits **zero
`asset_group` posts**, and every asset that would have been grouped falls through into passes 2 and
3. Measured against the committed studio-a profile: 244 `asset_group` posts exist only in the
committed document, against 314 `multi_asset` and 163 `solo_showcase` that exist only in a fresh
one. Only **336 of 863** ids survive a recompose at all.

⛔ **So regeneration is not an available remedy.** `recompose_posts`'s docstring calls the profiles
*"the exact asset set each site ships"*, and they are, for assets. They are **not a sufficient input
for composition**, because the field the first pass depends on only ever existed in the source
catalogue, which is not checked in. Of the three ways to resolve the divergence, one is simply off
the table.

⭐ **This sharpens what Decision 3 actually claims.** Assembly *is* deterministic, and #1296 made it
locally so. What is false is the unstated implication that a **recompose has the same inputs the
original assembly had**. Determinism is a property of a function over its inputs; it says nothing
when an input has been dropped from the artifact you kept. **The ADR should be read as: assembly is
reproducible GIVEN the source catalogue, and the checked-in profiles are not that catalogue.**

⚠️ And the practical hazard is not theoretical. Running the full chain today produces 1,430 posts
and silently drops **200 of the 841** hand-curated posts, because their ids do not exist in a
regenerated document. That silence was fixed separately in #1324, which is what makes leaving this
question open safe rather than merely tolerable.

Recovering group *membership* from the 244 committed `asset_group` posts is possible, but the group
post id derives from the `group_id` string, so it would move 244 more ids on site_a and 110 on
site_b. Tracked as #1322, which is a ruling to be made rather than work to be scheduled.

---

## Amendment, 2026-08-27 (#1322): RULING. The committed profile is the source of truth

Owner ruling, 2026-08-27, closing the question the two amendments above opened.

**The deciding fact was not about the data. `--recompose-posts` has no automated callers.** Not CI,
not the Makefile, not any script. A repository-wide search finds its own definition, one test that
names it in a docstring, and nothing else. It is a command a person types, and nobody types it.

So "the corpus should be rebuildable from scratch" was being treated as a requirement while nothing
depended on it. That aspiration is what made four sprints of guard output confusing, because every
comparison of the shipped file against a rebuild was comparing two different kinds of object and
reporting the difference as a finding.

### Decision

**`seed/profiles/*.posts.json` is authoritative.** It is the corpus the project ships, seeds from
and tests against. It is maintained by upgrade documents, which is how it already grows, and it is
not expected to be reproducible by re-running the composer.

**Verification compares the profile against itself and against what is published.** Those are the
comparisons that decide whether a publish is safe. Comparing it against a fresh composition is not
a check, because the composer no longer has the input that produced it.

⛔ **And the rebuild path must stop being a silent trap.** `recompose_posts` writes straight over
`profiles/{stem}.posts.json`, so a person typing the command today replaces the authoritative
corpus with a materially different one: 1,103 posts against 863, only 336 shared ids, and 200
hand-curated posts that no longer have a row to land on. #1324's guard now refuses that at the
point the curation is applied, which is why this is a trap rather than an active wound, but the
command should decline to overwrite rather than depend on a downstream pass to catch it.

### What this gives up, stated plainly

**A composition bug can no longer be fixed across the existing corpus**, only for posts added
afterwards. That is a real loss and it is accepted. It has also been the de facto situation for
months, since `--recompose-posts` did not execute at all until PR #1305 repaired it.

### The option deliberately not taken, and how to buy it back

Group membership *is* recoverable, from the 244 `asset_group` posts the committed profile already
holds, and `group_id` could be written back onto the asset records. The obstacle is that the group
post id derives from the `group_id` string, which is gone. The same fix #1310 applied to bundles
would resolve it: derive the group post id from **membership** instead. That is available if
regeneration ever becomes something we need.

It is not taken now because it would move **354 more post ids** (244 on site_a, 110 on site_b),
immediately after moving 618, to restore a capability with no callers.

⭐ **The generalisable point: a property is only worth maintaining if something depends on it.**
Determinism here was real, and the artifact it was a property of was not the artifact we ship. The
question to ask of an invariant is not "is it true?" but "what breaks if it stops being true?" The
answer here was nothing, and it took four sprints to ask.
