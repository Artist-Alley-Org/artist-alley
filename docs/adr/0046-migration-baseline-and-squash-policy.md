---
id: "0046"
title: Migration baseline + squash policy
status: accepted
date: 2026-06-06
area: process
phases:
  - "1.49"
supersedes: []
related:
  - "0006"
  - "0042"
tags:
  - process
  - migrations
  - database
  - conventions
excerpt: >-
  Pre-MVP migration sequences may be squashed into a single
  baseline migration. Squashes are deliberate, audited, and
  destructive — no upgrade path is supported from the prior
  sequence. After v1.0 launch, squashes are forbidden; the
  migration history becomes append-only forever.
---

## ⚠️ Semantic review pending (2026-07-08)

The user has clarified two release milestones:

- **v0.1.0** — first tagged release (marker: ResourceSpace refs deleted + base feature set complete).
- **v1.0.0** — out of beta (marker: real production usage + soak + stable quality).

This ADR currently frames the append-only-forever trigger as "after `v1.0.0`" / "after v1.0 launch." Whether the trigger should actually be v0.1.0 (strict; schemas stabilise with the first tag) or v1.0.0 (SemVer-standard; 0.x remains workshop) is an **open decision tracked at issue #228**.

Read this ADR's `v1.0.0` mentions as **pending-review** until #228 resolves. Every "after v1.0.0" clause below applies to whichever milestone #228 lands on; the ADR text will be updated once the decision is made.

Full milestone-model context: [`docs/v0_1_readiness.md` §0](../v0_1_readiness.md).

## Implementation status (2026-06-15)

The pre-MVP baseline squash is **in progress**:

- **1.49.C-1 audit** ✅ shipped via PR #132 (`b3b796b`). The first
  audit report under this ADR's policy lives at
  [`docs/cleanup-audit-2026-06.md`](../cleanup-audit-2026-06.md) —
  26 findings (17 drop-recommended, 9 cosmetic, 0 critical) across
  14 migrations / 61 tables / 64 FKs / 149 indexes. Closes #81.
- **1.49.C-2 squash** ⏭ ready to ship. Scope locked at 24
  single-line schema edits (17 column drops + 2 renames + 4 FK
  annotations + 1 column comment) per the audit's "Recommended C-2
  net changes" section. Tracked at #82.
- **Future audits.** This ADR's audit-report shape (§ "Audit-report
  shape" below) is now exercised end-to-end; the `docs/
  cleanup-audit-<date>.md` filename pattern is locked. Subsequent
  pre-MVP audits (if any) follow the same template.

After 1.49.C-2 lands + v1.0.0 ships, this ADR's append-only
clause activates. No further squashes permitted.

## Context

The Artist Alley codebase landed its first 60ish migrations during
the pre-MVP development sprint. The accumulated sequence carries
real signal — each migration documents a decision in time — but
also accumulates noise:

- Tables added then never used (orphans), columns added then
  effectively abandoned (smell), indexes added then made redundant
  by later indexes.
- Naming inconsistencies that accreted across separate sub-phases
  (`*_id` UUID vs `*_ref` BIGINT, `created_by_user_ref` vs
  `created_by_user_id`).
- Migrations from short-lived design directions that were
  superseded before they shipped to a real user (strangler-fig
  artifacts, abandoned legacy bridges).
- Every fresh-DB-in-CI run replays the entire sequence — slow,
  noisy, and a recurring source of `goose plpgsql` marker bugs that
  hide on local DBs because goose skips already-applied
  migrations.

The first time we squash 60 migrations into a clean baseline, the
move is uncontroversial — pre-MVP means no real users to migrate
forward. The harder question is: **when else?** Without a policy,
every time the migration count crosses a perceived threshold a
contributor will be tempted to squash again. After v1.0 ships, that
temptation must die. Real users with real data cannot tolerate
"sorry, this version of Artist Alley requires a fresh DB."

This ADR draws the line.

## Decision

**Pre-MVP migrations may be squashed into a single baseline.** The
squash is allowed because no production user exists who needs a
forward path. The cost is paid only by developers, who can be
trusted to `docker compose down -v` before pulling.

**After v1.0 launch, migrations are append-only forever.** No
further squashes. The migration history becomes a permanent record
of how the schema reached its current shape, in the order it
actually reached it.

### When a squash is allowed

A migration baseline squash is permitted only when **all** of the
following hold:

1. The project has not yet shipped a tagged `v1.0.0` release to a
   public release channel (Homebrew, apt, dnf, GHCR `:latest`).
2. The squash is approved in an ADR with the canonical migration
   header (per §"Baseline migration shape" below).
3. The squash is audited in a separate commit *before* the destructive
   commit lands. The audit commit ships a written report
   (`docs/cleanup-audit-<date>.md`) listing every dropped table,
   dropped column, renamed identifier, and the reasoning for each.
4. The destructive commit is reviewable in isolation: it ships only
   the new baseline file, the deletions of the old migrations, the
   updated `app/schema.sql`, and the regenerated sqlc output. No
   feature work in the same commit.
5. The branch is named `chore/migration-baseline-vN` (where N starts
   at 1) so reviewers immediately understand the scope.

A baseline squash that fails any of these conditions is a
project-policy violation and must be rejected at review.

### When a squash is forbidden

After `v1.0.0` ships, baseline squashes are forbidden. **This is
not a guideline; it is a contract with operators.** Real users on
real instances cannot reset their database. Every schema change
after v1.0 must be a forward-only migration that any production
instance can apply with `goose up`.

Exceptions exist only with explicit ADR amendment to this one,
naming the specific operational reason and a guaranteed-safe
forward path for every supported prior schema version. The bar is
intentionally high — easier to ship a complicated migration than
to break operator trust.

### Baseline migration shape

A baseline migration file lives at
`app/internal/db/migrations/00001_baseline_vN.sql` (where N
matches the branch numbering). The header is mandatory and matches
this template:

```sql
-- goose Up
-- =====================================================================
-- MIGRATION BASELINE v<N>
-- =====================================================================
-- Baseline as of <YYYY-MM-DD> — pre-v1.0 cleanup pass.
-- Supersedes the prior migration sequence; no upgrade path from the
-- prior sequence is supported. Developers running locally must
-- `docker compose down -v && up -d` after pulling this commit.
--
-- Authority: ADR 0046
-- Audit:     docs/cleanup-audit-<date>.md
-- Branch:    chore/migration-baseline-v<N>
-- =====================================================================

-- … schema in stable ordering …

-- goose Down
-- (Down is intentionally empty for a baseline migration.
--  Rolling back means restoring from a backup, not running migrations.)
```

The down section is empty by design. A baseline cannot be rolled
back through migrations — restoration is a backup-restore operation.

### Audit-report shape

The cleanup-audit document (`docs/cleanup-audit-<date>.md`) lists:

- **Dropped tables.** Per-table: name, where the orphan was confirmed
  (which packages searched for usage, what `git grep` patterns ran),
  reasoning, last migration that touched it.
- **Dropped columns.** Per-column: table, name, type, default, why
  it was abandoned, last sqlc-generated reference (or none).
- **Renamed identifiers.** Per-rename: old name, new name, reason,
  affected packages.
- **Index consolidations.** Per-index: which indexes were merged or
  dropped, what query patterns justified the surviving set.
- **CHECK constraint changes.** Per-constraint: any tightening or
  widening, with the Go-side typed-constant mirror per ADR 0042.
- **Naming-convention decisions.** The canonical convention adopted
  (e.g., "FKs are `*_id` UUID; legacy `*_ref` BIGINT renamed where
  the column is in scope, deferred where it isn't").

Audit reports are kept in `docs/` permanently. Each squash adds
one report.

### Schema-as-source-of-truth invariant

`app/schema.sql` is the canonical schema description that sqlc
type-checks Go queries against. After a baseline squash:

1. `app/schema.sql` mirrors the baseline migration's final state
   exactly.
2. sqlc is regenerated. The diff is reviewable.
3. Both files land in the same commit as the baseline.

When new migrations land after the baseline, they extend
`app/schema.sql` in the same commit. The two never drift.

## Consequences

### Positive

- **One canonical baseline per pre-MVP cleanup pass** instead of a
  60-migration archaeology dig every fresh-DB-in-CI run.
- **Audit reports are permanent.** Future contributors can read
  "why did we drop the X table" without digging through deleted
  migration files. Git history preserves the migration code; the
  audit preserves the reasoning.
- **The v1.0 commitment is documented.** Operators considering
  Artist Alley for production can read this ADR and know that the
  schema-stability story changes hard at v1.0 in their favour.
- **CI gets faster.** A 60-migration replay drops to one. Local
  dev-stack-from-scratch is materially less painful.
- **Catches drift.** The act of squashing forces a full audit pass.
  Orphan tables that accumulated over six months get found and
  dropped instead of haunting the schema forever.

### Negative

- **One-time destructive operation per squash.** Anyone running
  locally has to `docker compose down -v` and lose their dev data.
  Pre-MVP this is acceptable (real users don't exist); the policy
  prevents it from becoming a recurring inconvenience.
- **The audit pass is real work.** Per-table cross-referencing,
  per-column usage analysis, naming-convention adjudication. 4-6
  days of careful work per squash. Not a quick "regenerate schema"
  task.
- **Squash removes commit-level granularity in `git log`.** A
  curious developer asking "when did this column appear?" gets the
  baseline commit as the answer, not the original migration commit.
  Mitigation: the audit report documents the structural decisions;
  `git log -- app/internal/db/migrations/*` of the pre-baseline
  range still shows the historical migration commits.
- **The v1.0 commitment is a real commitment.** Once it's made, any
  schema mistake has to be lived with through forward-only
  migrations. There is no "let's just squash again" escape hatch.

## Alternatives considered

- **Keep all migrations forever, never squash.** Simpler policy.
  Rejected because pre-MVP accumulated migrations are net-negative —
  they encode design directions that no longer apply, slow CI, and
  hide drift. The first squash is worth doing.

- **Allow squashes liberally even after v1.0.** Reduces engineering
  toil at the cost of operator trust. Rejected because the operator
  experience is the load-bearing product story; "sorry, this
  release requires a fresh DB" breaks it.

- **Keep old migrations in a `migrations/_archive/` directory.**
  Considered. Rejected because (a) git history preserves them; (b)
  the archive directory creates confusion about whether those
  migrations are still in the apply path; (c) the audit report
  serves the "why did we change X" question better than the raw
  archived SQL.

- **Auto-generate the baseline from `pg_dump` on every CI run.**
  Considered as a continuous-squash mechanism. Rejected because the
  schema would diverge from the committed baseline silently, the
  audit step would never happen, and the policy line for "never
  after v1.0" would have no place to anchor.

## Implementation

This ADR's implementation is operational:

1. **Update the developer-reference documentation** to point at this
   ADR when discussing the migration policy. Add a "Migration
   policy" subsection to `site/src/content/docs/developers/reference.mdx`.

2. **Coding-standards entry.** Add a "Database migrations" rule to
   `site/src/content/docs/developers/coding-standards.mdx` referencing
   this ADR, with the short version of the rule ("pre-MVP: squashes
   allowed per ADR 0046; post-v1.0: forward-only forever").

3. **Phase 1.49 — Pre-MVP cleanup** ships the first baseline squash
   under this policy. The audit report from that pass lands at
   `docs/cleanup-audit-2026-06.md` (or similar dated name) as the
   first reference implementation of the squash procedure.

4. **The next squash decision (if any) is on operators**, not on us.
   Once v1.0 ships, the policy holds without per-release ADR work.

## References

- ADR 0006 — Go as target backend. sqlc is the build-time
  validation layer that sees the squashed schema.
- ADR 0042 — Distributed catalogs. CHECK constraints in the
  baseline must mirror the typed Go constants per the convention.
- `app/schema.sql` — canonical schema mirror.
- `app/internal/db/migrations/` — migration sequence directory.
- [goose migration tool](https://github.com/pressly/goose) — the
  runtime that applies migrations.
