---
id: "0073"
title: Articles (blogs) are posts with a kind, not a new entity
status: accepted
date: 2026-07-28
area: content
phases: []
supersedes: []
related:
  - "0010"
  - "0023"
  - "0029"
  - "0063"
tags:
  - content
  - posts
  - blogs
  - federation
  - visibility
excerpt: >-
  Long-form written content ships as a `kind` discriminator on the existing
  `posts` entity rather than a parallel table, because a new entity would
  require a sixth independent expression of the visibility rule — the exact
  defect that produced five production leaks in one week.
---

# 0073 — Articles (blogs) are posts with a kind, not a new entity

## Context

Artists and studios need to publish **written** work: devlogs, process write-ups,
release notes for their own projects, post-mortems. Today the product has no
long-form surface at all — `posts` are asset-centric showcases and `description`
is a caption, not an article body.

Three adjacent things already exist and shape the decision:

- **`posts`** already carries `title`, `visibility`, `cover_asset_id`, `team_id`,
  workflow `state_id`, `search_text`, `like_count`/`comment_count`, and the
  federation columns (`origin_server_id`, and peer/actor plumbing).
- **`comments` and `likes` are polymorphic** — `(target_kind, target_id)` with no
  FK — and `comments` is already threaded (`parent_id`, `root_id`, `depth`) and
  already stores both `body` and `body_html`.
- **RSS/Atom (ADR 0023, #43)** and the **announcements widget (ADR 0029, #49)**
  are planned. Announcements are *operator→everyone* notices. Articles are
  *author→audience* content. They are different products and must not be merged.

The real question is whether an article is a new entity or a variant of `posts`.

## Decision

**An article is a `posts` row with `kind = 'article'`.** The existing
asset-centric post becomes `kind = 'showcase'`.

Concretely:
- `posts.kind` — `showcase` (default, existing behaviour) | `article`.
- `posts.body` — nullable long-form Markdown, populated only for articles.
  Rendered server-side into the existing `body_html` convention that `comments`
  already uses, so sanitisation lives in one place.
- Feed and browse queries filter on `kind` so articles do not silently pollute a
  visual grid built for artwork.

### Why not a separate `articles` table

**Because of what happened in the week before this decision.** Five production
leaks shipped, all one root cause: a read path that *expressed* the visibility
rule itself instead of *obtaining* it from the one component that owns it
(#650, #657, #660, #661; epic #665). Each copy was correct when written, and each
drifted when the shared rule moved. None was caught by a test.

`posts` visibility now lives in exactly one place — `app/internal/posts/read_rule.go`
renders the rule once and is spliced into the feed, the by-asset lookup **and**
`canReadPost`, so the list and single-item paths are literally the same SQL and
cannot disagree (#666).

A parallel `articles` table would need its **own** visibility expression: its own
predicate splice, its own single-item gate, its own agreement test. That is a
sixth independent expression of a rule that has already leaked five times. The
entity separation buys tidiness; it costs the exact property we spent a release
buying back.

Reuse also inherits, at zero marginal cost: comments and likes (polymorphic,
already targeting posts), workflow states, full-text search, federation identity,
soft-delete semantics, and the audit trail.

### What this costs, stated plainly

- **`posts` gains a nullable long-text column** that is empty for most rows. This
  is a real but small cost; Postgres TOASTs it out of the main heap.
- **Every existing post query must become kind-aware.** A query that forgets the
  discriminator will show articles in an artwork grid. This is the genuine risk of
  the decision and it is mitigated the same way the visibility rule is: the feed
  filter belongs in the shared query builder, not restated per call site.
- **The `showcase` default must be written explicitly**, not inferred. A NULL kind
  is a third state nobody designed.

## Consequences

- Articles are federated by the same machinery as posts, for free — no second wire
  format, no second inbox handler (relevant to ADR 0010 and the phase-2 work).
- **RSS/Atom (#43) gains an obvious source.** A feed of articles is what a reader
  expects; a feed of image showcases is not. #43 should consume `kind='article'`.
- **Announcements (#49) stay separate.** Operator notices are instance-scoped and
  do not belong to an author; conflating them would put a studio's devlog and a
  maintenance notice in the same stream.
- The browse feed's existing filters (`latest`, `following`, and the still-
  unimplemented `team`/`trending` — see #691) must all become kind-aware.
- A future "articles only" reading surface is a query, not a migration.

## Alternatives considered

**Separate `articles` table.** Rejected above — a sixth visibility expression.

**Markdown assets.** Articles as `.md` files in the asset library, rendered by the
existing doc viewer. Rejected: an article is authored in place, has an audience and
comments, and is not a file someone uploaded. It would also inherit asset
sensitivity tiers, which answer a different question than post visibility.

**Reuse `description` for the body.** Rejected: it is a caption on every existing
surface — card subtitles, search excerpts, feed previews — and quietly turning it
into an article body would change all of them.
