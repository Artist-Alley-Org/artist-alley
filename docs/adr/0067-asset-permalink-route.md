---
id: "0067"
title: Assets are first-class linkable entities — a standalone /assets/[id] route
status: accepted
date: 2026-07-21
area: architecture
supersedes: []
related:
  - "0053"
tags:
  - frontend
  - routing
  - viewer
excerpt: >-
  Every asset tile links to a per-asset URL, but no such route ever existed —
  clicking an asset inside a collection 404s (#475). Assets get a standalone
  route, symmetric with /posts/[id], rendering the source-agnostic
  AssetPlaylist with a single-asset source. The modal playlist stays for
  in-context browsing; the route is for direct, shareable, reload-safe links.
---

## Context

`AssetPlaylist` is deliberately source-agnostic: a generic playlist shell (cursor, filmstrip,
navigation, viewer) plus a **per-source host** that builds the `PlaylistSource`. Posts already
use this — `PostHost` builds a `PostPlaylistSource` and there is a real `/posts/[id]/+page`
route so a post opens both as an in-feed modal and as a standalone page.

**Assets never got the same treatment.** `AssetCard` renders `<a href="/assets/{id}">`, and so
do several other components, but **there is no `assets/[id]` route** — the route tree has
`collections/[id]` and `posts/[id]`, never `assets`. Collections (and search results, and any
`AssetCard` grid) therefore link every tile to a path that resolves to SvelteKit's default
"Not found." This is **#475**: on the demo, clicking any asset inside a featured collection
404s. It is not a data or role problem — the asset resolves fine via the API; the *frontend
route to view it* does not exist.

The gap is why posts "work" and collections don't: posts open the viewer (modal via `PostHost`,
or the standalone route); collections dead-end on a link to nowhere.

## Decision

**Assets are first-class linkable entities. Add a standalone `assets/[id]/+page` route**,
symmetric with `posts/[id]`, that loads the asset and renders `AssetPlaylist` with a
**single-asset source** (no sibling context — exactly as the standalone `/posts/{id}` route omits
the post's return-to-feed context when entered cold). A missing/forbidden asset renders the
normal error page (404 via the visibility predicate, per ADR 0064 — not a raw crash).

This makes **every existing `/assets/{id}` link resolve** — collections, search, anywhere an
`AssetCard` appears — and makes asset URLs **shareable and reload-safe**, which a media/DAM
product should have regardless of #475.

**The modal playlist UX is unchanged and still primary for in-context browsing.** Opening an
asset *within* a collection should eventually carry sibling context (prev/next through the
collection) via a `CollectionHost` mirroring `PostHost` — a follow-up UX enhancement, not
required for the route to exist. The standalone route and the in-context modal coexist, exactly
as they already do for posts. This refines — does not discard — the "keep playlist UX inside the
modal" convention: the modal stays for browsing; the route is for direct links.

## Consequences

- The dead-link class is closed at the source: `/assets/{id}` becomes real, so no `AssetCard`
  consumer dead-ends. Enforced by the new route/link-integrity check (ADR 0068).
- Asset URLs become shareable and survive reload — a capability the modal-only model lacked.
- **Follow-up (v0.6.0):** `CollectionHost` + `CollectionPlaylistSource` so in-collection asset
  clicks open the modal with prev/next across the collection, matching the post experience.
- Ships in **v0.5.1** as a patch, with its regression + link-integrity tests in the same PR
  (ADR 0068).

## References

- ADR 0053 — IIIF interoperability (the other asset-viewing surface).
- ADR 0064 — content visibility (the route's load inherits the predicate; missing/forbidden → 404).
- ADR 0068 — testing strategy (the tests that ship with this fix).
- #475 — the bug this resolves.
