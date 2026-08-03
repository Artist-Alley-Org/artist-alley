---
id: "0074"
title: IIIF Change Discovery over the ArchivePub outbox; no IIIF Auth 2.0
status: accepted
date: 2026-07-28
area: extensibility
phases: []
supersedes: []
related:
  - "0020"
  - "0043"
  - "0053"
  - "0063"
  - "0064"
tags:
  - iiif
  - archivepub
  - federation
  - interoperability
  - visibility
excerpt: >-
  ArchivePub gains a IIIF Change Discovery 1.0 feed serialised from the
  existing outbox, so cultural-heritage harvesters can crawl an instance
  without implementing ArchivePub. The spec explicitly permits publishing
  activities about access-restricted content; we reject that permission,
  because ADR 0064 makes a restricted resource return 404 precisely so its
  existence stays hidden. IIIF Auth 2.0 is evaluated and declined.
---

# IIIF Change Discovery over the ArchivePub outbox; no IIIF Auth 2.0

## Context

We ship IIIF Image API 3.0, Presentation API 3.0, Content Search 2.0 and the
navPlace extension (ADR 0053), and we ship ArchivePub, an ActivityStreams-derived
federation wire format. The question raised was whether ArchivePub should *adopt*
the IIIF setup, or *borrow* from it.

The two are not substitutes. IIIF Presentation describes and delivers **a
resource** — a manifest of canvases, pull-based and addressable. ArchivePub
propagates **an event** between servers — push-based, signed, with an inbox,
an outbox and a social graph. IIIF has no vocabulary for approvals, workflow
state, comments, brand kits or peer follow/accept, which are the reasons
ArchivePub exists. Replacing one with the other is a category error, and this
ADR does not propose it.

The two already interlock at the read layer. `app/internal/iiif/federation/`
resolves federated canvas URIs to the peer's `instance_url`, and IIIF clients
fetch remote tiles directly from that peer without us proxying (ADR 0043's
walled garden). ADR 0053 records the result: cross-instance IIIF interop comes
free from the existing federation surface.

What is *missing* is the other direction. There is no way for an outside party
to learn **what changed** on an instance without implementing ArchivePub. That
matters because the product is positioned at museums, libraries, galleries and
archives, and those institutions harvest via IIIF.

IIIF has a specification for exactly this — the **Change Discovery API 1.0** —
and it is built on ActivityStreams 2.0, the same base ArchivePub derives from.

## Decision

**Publish a IIIF Change Discovery 1.0 feed derived from the existing outbox,
covering anonymously-visible content only.** Do **not** adopt IIIF Auth 2.0.

### 1. It is a separate document, not a change to the envelope

The Change Discovery feed is its own endpoint with its own `@context` of
`http://iiif.io/api/discovery/1/context.json`. It does **not** appear inside
ArchivePub envelopes and does not alter them.

This matters: the ArchivePub `@context` is the fixed string
`https://artist-alley.org/protocol/v1`, it is present in envelopes already
delivered to and stored by peers, and it is published as permanent. Nothing
here touches it. No JSON-LD array-context juggling is required, because the two
documents never share a context.

### 2. Shape

Per the spec, the feed is an `OrderedCollection` whose `id`, `type` and `last`
are required and whose `first` is recommended, paged as `OrderedCollectionPage`
resources carrying `orderedItems`, `partOf`, `prev` and `next`.

Two details are easy to get backwards and are called out deliberately:

- **`last` is required, `first` is only recommended**, and consumers are
  directed to start at the end and walk *backwards*. This is the opposite of a
  conventional feed and the paging implementation must be built for it.
- **Activities are ordered earliest to most recent** within a page, with the
  oldest page first.

An activity requires only `type` and an `object` carrying `id` and `type`;
`endTime` is recommended. This is a light serialisation, not a new subsystem.

### 3. Vocabulary mapping — most ArchivePub activities are NOT published

Change Discovery permits `Create`, `Update`, `Delete`, `Move`, `Add`, `Remove`
and `Refresh`. ArchivePub's vocabulary is much larger, and the mapping is
mostly exclusion:

| ArchivePub | Change Discovery | Why |
|---|---|---|
| `Create` / `Update` / `Delete` | same | Direct equivalents, when the object is a published manifest. |
| `aa:AssetVersion` | `Update` | A new version changes the resource. |
| `Add` / `Remove` (collection membership) | `Add` / `Remove` | Changes the collection manifest. |
| `aa:WorkflowTransition`, `aa:Approve`, `aa:MarkReviewed`, `aa:RequestChanges` | `Update`, **only if** the public manifest actually changed | Internal review state is not a resource change. Emitting one per transition would publish the studio's internal process. |
| `Like`, `Follow`, `Accept`, `Reject`, `Block`, `Announce`, `Undo`, `aa:Mention`, `aa:Subscribe`, `aa:Annotation` | **omitted** | Social-graph events with no Change Discovery equivalent. Publishing them would expose the peer graph and user behaviour to anonymous crawlers. |
| `aa:Share`, `aa:Unshare`, `aa:RevokeShare` | **omitted** | These change *who may see* a resource, not the resource. See below. |

A share that makes something anonymously visible for the first time surfaces as
a `Create`, dated at that moment — not as a share event.

### 4. We reject the spec's permission to publish restricted content

The Change Discovery specification explicitly allows this:

> Activities may be published about content that has access restrictions.
> Clients must not assume that they will be able to access every resource that
> is the object of an Activity.

**We do not take that permission.** ADR 0064 makes a restricted resource return
**404 rather than 403**, specifically so that its *existence* stays hidden.
Publishing a `Create` naming a private asset's URI would defeat that from a
different direction — the resource would still 404 on fetch, but the feed would
have already disclosed that it exists, when it was created, and where it lives.

Therefore the feed is built from the **anonymous** visibility predicate, and is
subject to the same rule as every other read path in this codebase: the rule is
**obtained from `visibility.Filter`, never re-expressed locally**. A read path
that writes out the visibility rule itself is the single defect class that
produced five production leaks in one week (ADR 0063); a public, unauthenticated,
crawlable feed is the worst possible place to reintroduce it.

Two consequences follow, and both are correct:

- An asset that becomes restricted after publication is emitted as `Delete`.
  The spec anticipates exactly this and tells clients to expect URIs that no
  longer resolve.
- The feed's `totalItems` will not match any internal count. It is a count of
  publicly visible resources, not of resources.

### 5. IIIF Auth 2.0 — evaluated, declined

Auth 2.0 solves cross-institution authenticated access for **browser-based IIIF
clients** (Mirador, Universal Viewer), where content loads through `img` and
`video` elements that cannot carry bearer tokens. It requires a probe service, a
token service and an access service — three to four new endpoints — plus a
postMessage-and-iframe token exchange.

We decline it for now:

- Our own SPA authenticates with sessions and does not need it.
- ADR 0043's walled garden has peers fetching directly under existing
  federation auth, so the cross-instance case is already covered.
- The remaining case is a *third-party* IIIF viewer needing authenticated access
  to restricted content on an AA instance. That demand has not appeared, and the
  surface is large for a hypothetical.

One finding is worth preserving rather than losing. Auth 2.0's **`substitute`**
property lets a probe respond `401` while offering a lower-fidelity alternative
at a different URI, with cascading tiers. That is conceptually the same move as
ADR 0020's "restricted assets stay listed but blurred". If we ever expose gated
content to third-party IIIF clients, `substitute` is the mechanism the standard
already provides for it, and we should implement ADR 0020's degradation through
it rather than inventing a parallel scheme.

**Revisit when** a third-party IIIF client needs authenticated access to gated
content on an AA instance — most likely an institutional deployment wanting
Mirador against restricted holdings.

## Consequences

- An outside harvester can crawl an instance's public changes without knowing
  ArchivePub exists, which is the interoperability win.
- Implementation is a serialisation over the existing outbox plus the anonymous
  visibility predicate and a paging scheme. No new storage model.
- The paging must be built end-first, against the usual instinct.
- The feed is a new public read surface and inherits the whole visibility test
  burden. It needs the same treatment as the other read paths: a test asserting
  the feed can never name a resource the anonymous single-item fetch would 404,
  because a list and a single-item view disagreeing is precisely how this class
  of bug has manifested before.
- We carry a documented gap for authenticated third-party IIIF access, with
  `substitute` named as the intended mechanism if it is ever closed.

## References

- IIIF Change Discovery API 1.0 — `https://iiif.io/api/discovery/1.0/`
- IIIF Authorization Flow API 2.0 — `https://iiif.io/api/auth/2.0/`
- ADR 0053 — IIIF Image + Presentation API
- ADR 0064 — restricted resources return 404, not 403
- ADR 0063 — the visibility predicate is obtained, never re-expressed
- ADR 0020 — asset gating; restricted stays listed but degraded
- ADR 0043 — federation walled garden
