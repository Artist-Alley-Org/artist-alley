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

## Amendment — 2026-08-19, after the prior-art pass

The decisions above were written from the owner's ruling and our own code. A prior-art pass
(recorded in the planning notes) confirmed two of them, found a genuine gap in a third, and
flagged one part as having no precedent at all.

**Confirmed — publication is never a side effect.** Every art platform examined makes publishing
an explicit act with its own control; none turns stored files into publications on its own.

**Confirmed — drop, do not convert.** A file left in personal storage on those platforms stays
there indefinitely; nothing manufactures a publication from it. Decision 4 stands.

**⭐ GAP — publication must be REVERSIBLE, and this ADR did not say so.** One mature platform
unpublishes a project straight back to draft: the work leaves public view, keeps its title and
framing, and can be republished. Our model as first written offered only deletion, which makes
"I want this off the site for now" a destructive act. Added as **decision 6**:

> **6. Unpublishing returns a post to its author, intact.** A post may leave every shared
> surface without being deleted — it keeps its title, description, members and framing, and can
> be published again. Its member assets are unaffected either way, because storage and
> publication are separate lifecycles: deleting a post never deletes the files it showed, and
> removing a file from your storage is a different act with different consequences.

**⚠️ NO PRECEDENT — the cross-ownership view (decision 5).** On art platforms a post's files
are always the author's own, so "my asset appears in someone else's post" never arises; that
shape comes from our DAM half, where a shared team library is normal. Decision 5 is therefore
*ours*, not borrowed, and it is the one to design conservatively: existence and count only,
never title or author, unless the ordinary read rule already grants them.

**One distinction to keep sharp.** A classic DAM answers "not ready yet" with a **state** on the
one object; the art tradition answers it with a **separate place**. We now have both — assets
carry workflow states *and* the post/asset split — and they answer different questions:
*workflow state* is "where is this in its production process", *published* is "has its author
put it in front of people". Conflating them would repeat the mature-versus-sensitivity mistake
ADR 0090 exists to prevent.

## Second amendment — 2026-08-19, after checking the closest-neighbour platform

The owner asked whether the pass had covered the platform nearest to our own audience — a
portfolio site for game-studio artists. It had not, and looking properly found a **contradiction
inside this ADR**.

**What that platform does:** a project is saved as a draft or published, publishing is an
explicit act, and a draft can be scheduled to go live later. Critically, it has **no separate
personal storage at all** — files live inside the project, and a draft project *is* the place
work waits. That is the opposite of the stash model in the first amendment, and both are
coherent: the stash keeps files, the draft keeps a work-in-progress publication.

**The contradiction.** Decision 6 says unpublishing "returns a post to its author, intact" — but
this system has nowhere for it to return to. Verified in the database: the `post` workflow
domain has exactly two states, `wip` and `published`; `published` is the initial state, **every
one of the 847 seeded posts is in it, `wip` holds zero, and no product code references `wip` at
all**. A post is therefore born published, and the draft state exists as a row nobody can reach.
Decision 6 as written could not be implemented.

**7. A post has a real draft state, and it is where an unpublished post goes.** The existing
`wip` state becomes reachable: a post can be created as a draft, edited over time, published as
an explicit act, and unpublished back to draft. A draft is visible to its author (and to those
the ordinary read rule already admits, e.g. team members with the relevant capability), and to
nobody else — it appears on no shared surface. This is what makes decision 6 mean something, and
it is what the create/edit page (#1119) needs to let someone save and come back.

**Two ideas examined and deliberately not adopted now.** *Scheduled publishing* (choose a future
moment) is a real feature on that platform and a natural extension of decision 7, but it needs a
scheduler and a story for what a peer sees before the moment arrives — filed as a later
consideration, not part of this model. *Per-surface publication* (visible on one surface but not
another) is expressed here by visibility tiers and collection membership rather than by a
separate axis; adding a second one would repeat the mistake ADR 0090 warns about.

**The shape of our answer, stated plainly.** We keep both mechanisms because we are both things:
the **asset library** (personal storage, a file with its own life, reusable across posts — the
DAM half) and the **draft** (a publication in progress — the portfolio half). Neither platform
tradition has both; the combination is ours, and the two must not be conflated. A file sitting in
storage is not a draft post, and a draft post is not a private file.
