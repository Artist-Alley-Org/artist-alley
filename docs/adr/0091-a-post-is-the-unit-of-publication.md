---
id: "0091"
title: A post is the unit of publication — an asset is personal storage
status: accepted
date: 2026-08-19
area: architecture
phases: []
supersedes: []
related:
  - "0009"
  - "0064"
  - "0088"
tags:
  - collections
  - posts
  - assets
  - visibility
excerpt: >-
  A file you upload belongs to you and appears nowhere else until you make a
  post from it. Collections and browse contain posts only. Publication is
  always a deliberate act, never a side effect of adding a file somewhere.
---

## Context

The product grew two ways to put work in front of people. A **post** was
always the deliberate one: you choose files, you write a title, you publish.
But a **collection** could also hold bare assets, and the browse surface could
render them, so dropping a file into a collection published it — quietly, with
no title, no framing, and no moment where the artist decided the work was
ready.

That second path arrived by accretion rather than design. `collection_resources`
predates `collection_posts` (#882 added the post membership later), and the
browse feed rendered whatever the API returned. Nothing was wrong with any
single step; the composition was the problem.

The owner ruled on it three times in one session, most plainly:

> Collections shouldn't have assets. That doesn't make sense. Non-post assets
> only belong to their uploader. Collections and browse contain posts only.

v0.10.1 shipped the visible half — the collection page's asset section is gone,
along with every affordance that attached a bare asset to a collection (#1185).
This ADR records the model that half implies, and decides the parts it left open.

## Decision

**1. A post is the unit of publication.** Every shared surface — browse, search
results, collections, feeds, federation — carries posts. An asset reaches those
surfaces only as a member of a post.

**2. An asset is personal storage until posted.** An uploaded file belongs to
its uploader and appears in their own asset list. It is not private in the
access-control sense (ADR 0064 still decides who may read what); it is simply
*unpublished* — the way a file on your disk is not a gallery.

**3. Publication is never a side effect.** No action that stores a file may
also publish it. Uploading into a collection creates a post from those files —
the post-creation flow runs, the artist names the work — and the post joins the
collection.

**4. Existing bare memberships are dropped, not converted.** When
`collection_resources` retires, its rows do not become auto-generated posts.
An auto-generated post is a publication nobody authored, with a title nobody
wrote; it would put words in an artist's mouth. The membership disappears; the
assets remain in their owners' storage, losing nothing.

**5. An asset knows where it appears.** From their own asset list an owner can
see every post their asset is part of, including posts by other people. Each
such post is subject to the ordinary read rule: an owner who cannot read the
containing post sees that their asset is used, and nothing about the post
beyond what any placeholder discloses (ADR 0064's field plane).

## Consequences

- The collection page renders posts only; there is no second content shape to
  design for, and features like sorting, filtering and covers have one subject.
- **The seed must author posts.** A catalogue that attaches bare assets to
  collections produces empty collections under this model. v0.10.1's seeder
  already backfills a post per formerly-bare member (owner-authored, public
  only when every member is public) — that behaviour is now a requirement of
  the model, not a fixture convenience.
- `collection_resources` becomes internal or disappears. Its write endpoints
  are retired; the cover picker's read of it is replaced by the ordinary asset
  search (an asset need not be a member to be a cover — ADR 0088).
- Point 5 needs a query that crosses ownership boundaries safely. It is the one
  place where an asset owner learns something about a post they may not read,
  and the disclosure is deliberately minimal: existence and count, never title,
  author or contents unless the read rule already grants them.
- **Federation:** a peer receives posts. An asset without a post has no
  federated representation at all, which simplifies the outbound model — there
  is no "loose file" object to define.

## Alternatives considered

**Convert bare memberships into single-asset posts.** Rejected: it manufactures
authorship. A post carries a title and a framing; generating them from a
filename produces publications the artist never made, and undoing that later is
worse than never doing it.

**Keep bare assets in collections but hide them from browse.** Rejected as the
worst of both: the data model keeps two content shapes forever, every new
feature must ask "and what about loose assets?", and the surface that hides
them is one bug away from showing them.

**Let uploads into a collection stay unpublished until later.** Rejected: it
recreates the quiet-publication path with an extra step, and the artist who
dropped files into a shared collection plainly *intends* to share them. Asking
for a title at that moment is the smallest honest amount of friction.
