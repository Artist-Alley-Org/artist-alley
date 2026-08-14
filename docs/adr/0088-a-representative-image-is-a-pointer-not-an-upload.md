---
id: "0088"
title: A representative image is a pointer at an asset, not a bespoke upload
status: accepted
date: 2026-08-13
area: architecture
phases: []
supersedes: []
related:
  - "0063"
  - "0064"
  - "0071"
  - "0083"
tags:
  - collections
  - assets
  - api
excerpt: >-
  Surfaces that need "the image representing this thing" — a collection cover, a team hero —
  store a nullable pointer at an ordinary asset, gated per-viewer, falling back to a derived
  default. Not a bespoke upload. The instance-logo endpoint is the precedent that argues against
  itself: its content-type sniffing, size bounds, storage pin and MRU history all exist because a
  logo has no asset to point at. The deciding argument is one representation, not cost.
---

# 0088 — A representative image is a POINTER at an asset, not a bespoke upload

- **Deciders:** planning agent, with the operator's standing "only the most robust fix" instruction
- **Context:** #1027 (collection cover override), PR #1072. Builds on #1026 / PR #1069.
- **Related:** ADR 0063 (one expression of a rule), ADR 0064 (the picture plane), ADR 0083
  (what federates), ADR 0071 (preview ladder client contract)

## Context

Several surfaces want "the image that represents this thing" where the thing is not itself an
image: a collection (#1027), and — already visible on the roadmap — a followed-team rail with
team-selected hero images (#982). The obvious two designs are:

1. **A bespoke upload** — the object owns its own image bytes.
2. **A pointer** at an existing asset.

We already had one instance of shape (1) in the tree: `POST /admin/system/appearance/logo`.
It is the natural precedent to reach for, and reaching for it would have been wrong.

## Decision

**A representative image is a nullable pointer at an ordinary asset, gated per-viewer, falling
back to a derived default when unset or unreadable. It is not a new owned object.**

For #1027 that is `collections.cover_asset_id UUID NULL REFERENCES assets(id) ON DELETE SET NULL`,
with the composed mosaic (#1026) as the fallback.

### 1. Why not the upload shape, stated from the precedent rather than against it

`POST /admin/system/appearance/logo` had to build, and maintain:

- content type derived by **decoding the bytes**, never client-declared, because the logo is
  served on a public unauthenticated path;
- raster-only, SVG refused as an executable document format;
- a 2 MiB ceiling and a 16–1024px edge bound;
- a content-addressed `appearance:logo` **pin**;
- a `logo_history` MRU capped at 5, releasing the evicted entry's pin to GC.

⭐ **Every one of those exists because the instance logo has no asset to point at.** An operator's
logo is not a work in the archive. A collection's cover *is* — or can trivially be made one.

Pointing at an asset inherits storage, the `col` rendition, the read predicate, federation
identity and the GC lifecycle **that already exist and are already tested**. The upload shape
would re-derive all of it per surface.

### 2. The deciding argument is ONE REPRESENTATION, not cost

The client contract for a cover is `{asset_id}` plus "fetch `/assets/{asset_id}/variants/col`".
A bespoke upload makes a cover **sometimes an asset id and sometimes a storage hash**, so every
consumer branches on which kind it got.

That is the defect ADR 0063 exists to prevent, one level up: not a rule expressed twice, but a
*value* represented two ways. This milestone paid for that class three times — #902 (a column
that materialises a security decision vs a conjunct that evaluates it), #1023 (a display-name
ladder transcribed four times, three of them wrong), #1026 (a cover composed in two places).

**"Upload a dedicated banner" is not lost.** It becomes: upload it as an ordinary asset, then
point at it. One extra step, zero new machinery, and the banner gains versioning, permissions and
GC for free.

### 3. Not restricted to members either

The tempting middle option — "point at one of this collection's own members" — was rejected. It
guarantees relevance but dies when the member is removed, which is the exact failure #1027's own
text names. A free pointer survives it.

### 4. The gate is the PICTURE plane, and the fallback is mandatory

Per ADR 0064 the question "may this caller see this image" is the picture plane, not the field
plane, and **not** the mutation capability — a team-scoped `assets.admin` holder gets the fields
of assets they administer and explicitly not the picture.

Two obligations follow, and the second is the one that will be forgotten:

- **On write**, the setter must be able to picture the asset they choose.
- ⭐ **On read, a cover the viewer may not picture MUST fall back to the derived default — never
  render blank.** A blank tile is precisely the crowding bug #1026 had just fixed; reintroducing
  it through the override would have been the same defect through a new door.

### 5. It does not federate

ADR 0083 excludes anything that *"names something that exists only on the sender"*. A local
`asset_id` is exactly that. The pointer stays home; a receiver composes its own default.

`ON DELETE SET NULL` is the behaviour, not the cheap option: RESTRICT would let one collection's
curation decision block an unrelated asset's deletion, and CASCADE would delete the collection.

## Consequences

- New "representative image" surfaces (#982's team hero images, any future profile banner) have a
  shape to copy, and a reason not to copy the logo endpoint.
- The **picker** and the **API** may legitimately differ in generality — #1027 shipped a
  member-only picker over an any-asset API, recorded as **#1074**. That gap is acceptable
  precisely because the pointer model makes "add it, then pick it" work.
- ⚠️ **The upload shape is not banned.** `appearance:logo` remains correct *for the logo*, because
  an instance logo is genuinely not archive content. The test is the one in §1: **is the image a
  thing this system already stores as an asset, or is it chrome?** Chrome may own its bytes;
  content points.
- A surface adopting this owes a derived fallback. If there is nothing sensible to fall back to,
  that is a signal the pointer model may not fit — reconsider rather than shipping a blank.
