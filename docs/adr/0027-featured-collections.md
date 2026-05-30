# ADR 0027: Featured collections — nested curation, access-scoped homepage

- Date: 2026-05-30
- Status: Accepted

## Context

The current homepage and account dashboards show "recent uploads" /
"unreviewed" / "team feed" as flat lists. RS supports nested
**featured collection trees** as the front-page curation primitive —
"Concept Art > Characters > 2026 Pitches > Hero Shots" — with each
node having its own thumbnail + description + access scope. Studios
with a marketing org need this depth to maintain discoverable curated
areas (press hits, "best of," seasonal collections, current
campaigns).

## Decision

Add Phase 1.35 — Featured collections & homepage curation — extending
the existing collection model with a tree edge and a `featured`
scope.

### Tree edge

A `collection_parent_id` column on `collections`. NULL = top-level.
A parent collection can contain child collections AND posts. Depth is
capped at 5 to prevent runaway nesting in the UI.

### Featured scope

A `featured` boolean + `featured_scope` enum (`team` / `org` /
`public`) on `collections`. The homepage surfaces collections at the
appropriate scope:

- `team` — visible only to that team's members; shows in the team
  pane of the dashboard.
- `org` — visible to all signed-in users; shows on the global
  homepage.
- `public` — visible to anonymous users when anonymous browse is
  enabled (Phase 1.21); shows on the public landing.

### Cascade publish

Marking a parent featured offers a "cascade to children" toggle so a
campaign with 20 sub-collections doesn't need 20 clicks.

### Hero card

Each featured collection has an optional `hero_asset_id` — the
specific post / asset to render as the card cover. Falls back to
the most-recent post in the collection if unset.

### Sort and order

Within a featured tree, siblings sort by an explicit
`featured_order` integer (drag-reorder in admin) so curators control
the narrative.

### Access during anonymous browse

A `public` featured collection's individual assets still respect
their own sensitivity tier (ADR 0020). A `public` collection with
`embargo` content shows only the title; assets are hidden until
embargo lifts.

## Consequences

**Positive**

- The homepage stops looking like a feed and starts looking like a
  studio's actual marketing surface.
- Curators can build narrative arcs (campaign → series → pieces)
  without flattening structure.
- The same tree powers the team dashboard and the public homepage
  — one model, three audiences.

**Negative**

- Tree edges introduce N+1 query risk; mitigation via the existing
  `cache.Registry` for featured-tree resolution.
- Five-level depth cap is judgment; loosen if real use cases require.

## Alternatives considered

- **Flat featured list.** Doesn't represent how marketing teams
  actually organize content.
- **Tag-based featured.** Doesn't compose into a tree without
  awkward parent-tag conventions.

## Reference

- Phase 1.35 in [`docs/roadmap.md`](../roadmap.md).
- Anonymous browse policy (Phase 1.21).
- Sensitivity tier interplay (ADR 0020).
