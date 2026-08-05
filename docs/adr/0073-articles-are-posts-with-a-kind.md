---
id: "0073"
title: Articles (blogs) are posts with a kind, not a new entity
status: accepted
date: 2026-07-28
area: architecture
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

`posts` visibility now lives in exactly one place — `app/internal/visibility/post_rule.go`
renders the rule once and is spliced into the feed, the by-asset lookup, the
single-item gate **and** the three search surfaces, so every path is literally the
same SQL and they cannot disagree (#666, #873). It lived in
`app/internal/posts/read_rule.go` until #873, which found the sixth expression this
section warns about already in the tree: `visibility.Filter`'s own `EntityPost`
branch, still rendering `public OR author` while `posts` rendered the real rule.
The moral holds and gains a corollary — "one place" has to mean the place every
reader can reach, and a package-private rule guarantees the surfaces outside that
package will write their own.

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

### Addendum: a post with zero members is a REACHABLE state (recorded 2026-08-05, #918)

An article is the obvious post with nothing attached, but it is not the only one, and until
#918 the zero-member case was effectively untested — which cost two stacked defects that
between them made such a post **fail to open at all**. Recorded so nobody treats the state as
hypothetical again:

- **The API refuses to create one.** `POST /posts` rejects an empty `members` array with
  `400 "members: at least one asset required"` (verified 2026-08-05). So the state is not
  reachable by construction through the current endpoint.
- **It is reachable by deletion.** Soft-delete the last member's asset and the post remains,
  with zero visible members. That is an ordinary thing to do, not an edge case someone has to
  go looking for.
- Whatever the article surface ends up being, it will need a create path that produces exactly
  this shape — so the renderer has to handle it regardless.

The failure it produced is worth naming because it was invisible rather than loud: the post
loader's re-fetch guard read `state.items.length > 0`, which is never true here, so the post
re-requested itself indefinitely (measured at over 1,600 requests in six seconds) and sat on a
loading skeleton forever. Separately, every scrap of post chrome — header, author, the actions
menu — is contributed as the viewer's Details tool, which only mounts when there is an item to
view, so even once loading stopped the empty branch was a single line of grey text. Both are
fixed; the lesson is that "no members" is a first-class render state for posts, not an error
path.

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
