---
id: "0065"
title: Featuring is a placement, not a property of the thing featured
status: accepted
date: 2026-07-20
area: architecture
phases:
  - "1.35"
supersedes:
  - "0027"
related:
  - "0027"
  - "0063"
tags:
  - architecture
  - ux
  - curation
  - data-model
excerpt: >-
  Two featured mechanisms already exist — a boolean on collections and a
  polymorphic featured_items table. ADR 0027 would have grown the weaker
  one toward the stronger one's job. Instead featured_items becomes the
  single home for curation, gains an audience scope, and collections.featured
  is removed. The collection tree survives as structure, not as featuring.
---

## Context

**Two mechanisms already exist**, and this was reported rather than predicted —
issue #382 was titled *"two disconnected featured mechanisms"*.

| mechanism | shape | capability |
|---|---|---|
| `collections.featured` (baseline `00001`) | boolean on the row | collections only, no order, no audience |
| `featured_items` (migration `00002`) | `subject_kind` + `subject_id` + `position` + `created_by_user_ref` | polymorphic (asset *or* collection), ordered, attributed |

Both are live: `collections.featured` is read by `collections/handler.go` and
`list_page.go`; `featured_items` is seeded by the demo pipeline and surfaced at
`/admin/featured`.

[ADR 0027](0027-featured-collections.md) proposed extending the **boolean** with
a `featured_scope` enum (`team`/`org`/`public`), plus a `collection_parent_id`
tree and cascade-publish.

The forcing function is that #416 (logged-out frontend) shipped, and posts stay
members-only pending the `followers` tier — so the featured rail is now the
**only** content an anonymous visitor sees at `/`. Whatever model backs it is
about to become load-bearing on a public surface.

## Decision

**Featuring is a property of a *placement*, not of the thing featured.**

A collection is not inherently featured. It is featured **on the public landing
page, at position 3**, or **on a team dashboard, at position 1** — possibly both
at once, with different ordering, set by different people. That is a
relationship between *(subject, audience, position)*, and a boolean on the
subject row cannot express it.

Therefore:

1. **`featured_items` is the single home for curation.** It is already the right
   shape — a placement table keyed on a polymorphic subject.
2. **Add an audience scope**: `scope` (`public` | `org` | `team`) plus `team_id`
   when team-scoped.
3. **Replace the uniqueness constraint.** Today's
   `UNIQUE (subject_kind, subject_id)` means a subject can be featured **exactly
   once, globally** — so featuring something publicly *and* internally is
   currently impossible. It becomes
   `UNIQUE NULLS NOT DISTINCT (subject_kind, subject_id, scope, team_id)`.

   **`NULLS NOT DISTINCT` is load-bearing, and this ADR originally got it wrong.**
   `team_id` is NULL for `public` and `org` placements, and Postgres treats NULLs
   as distinct under a plain `UNIQUE` — so the constraint as first specified would
   have enforced **nothing at all** on exactly the two scopes that matter, allowing
   the same subject to be featured publicly without limit. Verified empirically
   during #417 (PR #456) rather than reasoned about: two `('x', NULL)` rows both
   insert under plain `UNIQUE`; the second is rejected under `NULLS NOT DISTINCT`.
   Requires PG15+; the stack and CI run 16.

4. **`team_id` is its own column, not derived from the subject.** A placement names
   its *audience*, which is not necessarily the subject's owner — and more
   decisively, **`collections` has no `team_id` column at all** (only
   `owner_user_ref`). Deriving the audience from the subject is not expressible for
   half the subject kinds.
5. **Remove `collections.featured`**, migrating existing rows into
   `featured_items` at `scope = 'org'` (its current de-facto meaning).
6. **Keep from ADR 0027 only what is not about featuring**:
   `collection_parent_id` (hierarchy) and a hero/thumbnail field (presentation).

### What this supersedes in ADR 0027

Superseded: the `featured` boolean + `featured_scope` enum on `collections`, and
cascade-publish as a property of that boolean.

Retained: the `collection_parent_id` tree edge, the depth-5 cap, and the
observation that nested curation is a standard front-page primitive. Cascade
returns later as a bulk operation over placements — creating N rows in
`featured_items` — rather than as a flag that means different things at different
depths.

## Why a placement table rather than a richer flag

**It is what a studio actually does.** Marketing curates a public showcase while
an art director features internal reference — same collection, different
audiences, different order, neither aware of the other. A single row on the
collection forces those two jobs to fight over one field.

**It features assets, not just collections.** A hero render can go on the landing
page without inventing a collection to hold it. The polymorphic `subject_kind`
already supports this; the boolean structurally cannot.

**Curation stops mutating the subject.** Reordering the landing page writes only
to `featured_items`. Under a row flag, editing the homepage means writing to
collection rows — which is both a wider blast radius and a confusing audit trail.

**The tree composes instead of competing.** You feature a *node*; the hierarchy
does not need to know featuring exists. Conflating them is what produced two
mechanisms in the first place.

## Why not grow the boolean (the ADR 0027 path)

Adding `featured_scope` to `collections` reimplements a join table one column at
a time. Ordering per scope needs another column; a second audience for the same
collection needs a second row, at which point it *is* a join table — with the
subject's identity as its primary key, which is the wrong key.

And it leaves the stronger mechanism in place. **Two sources of truth for "what
is featured" is the same defect class as a visibility rule expressed in two
places**, which cost this project #210, #212, #432 and #449 in a single week
(see [ADR 0063](0063-content-visibility-predicate.md)). The difference here is
that the duplication is visible *before* a public surface is built on it.

## Consequences

- **#417 (public featured rail) builds against `featured_items` only.** It must
  not read `collections.featured`, and must not add a third read path.
- **The scope column gates the public surface.** The rail selects
  `scope = 'public'`; anonymous callers never see `org`/`team` placements. This
  composes with — and does not replace — the visibility predicate: a placement is
  only rendered if the subject itself passes ADR 0063's predicate, so featuring
  can never widen access. Per-asset sensitivity still applies (ADR 0020): an
  `embargo` asset shows its title only.
- **A migration removes `collections.featured`** and rewrites its readers in
  `collections/handler.go` and `list_page.go`. This is schema-breaking, which is
  acceptable pre-v0.1.0 — and materially cheaper now than after the public rail
  ships on top of it.
- **`hero_asset_id` and `collection_parent_id` remain unbuilt** and stay with
  Phase 1.35. Neither is required by the rail.
- Curation becomes independently auditable — `featured_items` already carries
  `created_by_user_ref`, which a boolean never did.
