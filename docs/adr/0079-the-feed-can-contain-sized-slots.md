---
id: "0079"
title: The feed can contain sized slots, and an unfilled slot becomes ordinary content
status: accepted
date: 2026-07-29
area: ux
phases: []
supersedes: []
related:
  - "0017"
  - "0027"
  - "0030"
  - "0038"
tags:
  - browse
  - feed
  - layout
  - monetization
excerpt: >-
  Ads and premium placement are the same mechanism wearing two labels: a
  position in the feed that is larger than one tile. This records the
  primitive once — a slot is a position, a size in grid units, and an ordered
  list of fill sources — rather than building it twice. The load-bearing rule
  is that an unfilled slot degrades to ordinary content, because a hole in a
  grid is far worse than a collapsed banner.
---

# The feed can contain sized slots, and an unfilled slot becomes ordinary content

## Context

Two separate wants arrive at the same shape: an operator running an
ad-supported public instance needs somewhere prominent to put an ad, and
the monetization model wants a visible benefit for a premium tier. Both
described the same thing — **one item in the browse feed occupying more
space than a normal tile**, roughly two columns by two rows.

Three facts about where we start.

**ADR 0030 already defines ad slots, but not this kind.** Its placement
model is fixed-pixel banners positioned *between* content:
`feed.banner` at 970×90, `feed.inline.every-N` "between every Nth feed
item", `sidebar.top` at 300×250. Those are strips in the margins of the
grid, not cells inside it. A 2×2 tile participating in the grid's own
packing is a placement model ADR 0030 does not describe, and its
consequence that an empty slot should "collapse entirely" is written for
a banner.

**The current masonry cannot do it at all.** `MasonryColumns.svelte`
renders N sibling column elements, each an ordinary block flow, with each
item assigned to the shortest column on arrival and never reassigned.
That design was chosen deliberately in #651: the previous `column-width`
multicol *balanced*, so appending a page moved 13 of 24 sampled tiles
sideways while the user was looking at them. The cost of the fix is that a
tile lives inside one column element and there is no shared coordinate
space, so nothing can straddle two columns. This is structural, not a
missing style.

**No ADR governs the card grid.** Only ADR 0014 touches aspect ratio, in
passing. So there is no prior layout decision to supersede — but equally
no recorded reasoning for the one we have.

The framing question — "position-only, or can organic content be promoted
into a large slot?" — turned out to be a false choice. A premium user's
post appearing at 2×2 *is* organic content promoted into a slot. The two
differ in what fills the slot, not in what the slot is.

## Decision

**The feed can contain sized slots. A slot is a position, a size, and an
ordered list of fill sources. Ads and premium placement are consumers of
that primitive, not features that each own a copy of it.**

### 1. Position, size, fill chain

- **Position** — where in the feed sequence the slot falls. Deterministic
  and known before layout runs, which is what makes placing around it
  tractable.
- **Size** — expressed in **grid units** (2×2, 2×1), never pixels. ADR
  0030's pixel dimensions are right for a banner in a fixed margin and
  wrong for a cell that must tile with its neighbours at every breakpoint.
- **Fill chain** — an ordered list of candidate sources. The first that
  can fill it wins. Sources include an ad provider (ADR 0030), a promoted
  post from a premium account (ADR 0017/0038), and operator curation
  (ADR 0027's featured material).

Whether a given install's large slots hold ads, promoted posts, or
nothing is configuration. The layout does not care, and neither does the
grid.

### 2. An unfilled slot becomes ordinary content — never blank space

This is the rule most likely to be got wrong, so it is stated as a
decision rather than left to implementation.

ADR 0030 says to collapse a slot when the provider signals no fill. That
is correct for a 970×90 banner in the page margin. **It is wrong for an
in-grid slot**: collapsing a 2×2 leaves a 2×2 hole in the middle of the
feed, and reserving it leaves the same hole with an outline around it.

So an in-grid slot whose fill chain comes up empty is **replaced by the
ordinary feed items that would have occupied that space**. The user sees
a normal grid. They cannot tell a slot was there.

**This amends ADR 0030's no-fill consequence for in-grid slots only**;
its banner slots keep collapsing, which remains right for them.

### 3. Append-stability is not negotiable

#651 exists because tiles moving under the user's cursor during infinite
scroll is a serious defect. Any implementation of this ADR must preserve
that property.

Concretely, for a CSS Grid implementation: **`grid-auto-flow: dense` is
forbidden.** Dense packing backfills holes, which both reorders items
visually and reintroduces exactly the instability #651 removed. It will be
tempting precisely because it appears to solve the gap problem. Default
row flow places items in order and never relocates an earlier item when a
page is appended; that is the property we need, and the price is holes.

### 4. Masonry placement: minimise the gap, do not pretend to eliminate it

In a uniform grid a 2×2 tiles perfectly and there is nothing to solve. In
masonry there are no uniform rows — each column holds arbitrary tile
heights — so "two rows tall" has no fixed meaning and a wide item cannot
be guaranteed to end flush with its neighbours.

The placement rule for a slot spanning two columns:

1. Choose the **adjacent column pair with the smallest height
   difference** — not simply the shortest column.
2. Place the slot at the greater of the pair's two heights.
3. Set **both** columns' running heights to the slot's bottom edge.

The residual gap is the pair's height difference before placement.
Choosing the closest-matched pair minimises it, and the existing
shortest-column heuristic already keeps columns near level, so in practice
it is a fraction of a tile rather than a visible hole.

Usefully, this **re-levels the pair**: after a large slot, both columns
resume from the same offset, so the layout is tidier below a slot than
above it. Because slot positions are known in advance, that levelling is a
deliberate effect rather than luck.

**The honest claim is "bounded and small", not "zero".** A design that
promises gapless masonry with variable-height content is promising
something the layout cannot deliver.

### 5. Ship the uniform grid first

The uniform grid view can express a 2×2 today with `grid-column: span 2;
grid-row: span 2` and needs no rework. Masonry is gated on replacing the
sibling-column implementation.

So the sized-slot model ships in the grid view first, where it is nearly
free and proves the primitive, and the masonry layout rework is separate
work that inherits it. Wiring ads and premium onto the primitive comes
after, as consumers.

## What this explicitly rejects

- **Building ad placement and premium placement separately.** They are one
  mechanism; two implementations would drift, and the drift would show up
  as a layout bug in whichever one is exercised less.
- **Reserving blank space for an unfilled slot.** See §2. A hole in a grid
  reads as broken, not as inventory.
- **`grid-auto-flow: dense`.** See §3. It trades a solved bug for an
  unsolved one.
- **Sizing in-grid slots in pixels.** They must tile with their neighbours
  at every breakpoint; pixel dimensions cannot.
- **Auto-promoting content into large slots by aspect ratio.** A feed of
  mostly-wide assets would collapse a 4-column grid toward 2 columns and
  read as broken rather than intentional. Slot positions are decided by
  configuration; content is selected *into* them.
- **Claiming gapless masonry.** Bounded is achievable; zero is not.

## Consequences

- The feed's item stream stops being a flat list of content and becomes a
  list of positions, some of which are sized. Anything paginating or
  virtualising the feed has to understand that a position may consume more
  than one item's worth of space.
- `aria-posinset` / `aria-setsize` bookkeeping (from #651) must account for
  slots, or announced positions drift. The current implementation is
  careful here and the care has to extend.
- **A slot is a place where content appears without the viewer having
  chosen it.** Fill sources therefore inherit the visibility rules
  unchanged: a promoted post must be one the viewer could already see, via
  `visibility.Filter` per ADR 0063. A slot must never become a way to
  surface content a caller has no right to.
- Premium slot fill should be automatic for the account tier, not a
  per-post decision by the artist. Making artists opt each upload into
  promotion adds exactly the per-item friction the product exists to
  remove.
- Masonry gains real placement logic and a reason to move off sibling
  columns. That rework carries #651's append-stability requirement and the
  accessibility decisions taken with it — it is not a free refactor.
- ADR 0030's slot inventory grows a kind (in-grid, grid-unit-sized)
  alongside its banners, and its no-fill rule becomes per-kind.

## References

- ADR 0014 — frontend stack (the only prior mention of card aspect ratio)
- ADR 0017 — monetization and licensing (premium tier)
- ADR 0027 — featured collections (operator curation as a fill source)
- ADR 0030 — operator-configurable ad slots (banner placement; amended here for in-grid slots)
- ADR 0038 — premium add-on layer
- ADR 0063 — content visibility predicate (fill sources inherit it)
- #512 — browse page UX overhaul epic
- #651 — append-stable masonry, and the reason a tile cannot currently span
