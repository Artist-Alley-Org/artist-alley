---
id: "0096"
title: Icons come from one installed icon set, never hand-pasted SVG
status: accepted
date: 2026-08-25
area: ux
phases: []
supersedes: []
related:
  - "0014"
tags:
  - frontend
  - icons
  - consistency
excerpt: >-
  Every icon in the web client comes from the installed `@lucide/svelte`
  package, imported by name. Hand-pasting SVG markup into a component is not
  an allowed way to add an icon, because it is how a UI accumulates six
  stroke weights and four visual grammars that no reviewer can see drifting.
  The dependency has been installed since sprint 23 and is used by three
  components out of sixty-four that draw icons.
---

# Icons come from one installed icon set, never hand-pasted SVG

## Context

Adding an icon has had no stated rule, so it has been done the fastest way available: open
lucide.dev (or any icon site), copy the `<svg>` markup, paste it into the component. That works the
first time and compounds badly.

**Measured on `dev` at `ef8a278a`:**

| | count |
|---|---|
| components importing `@lucide/svelte` | **3** — `BrowseRail`, `BrowseRailManageMenu`, `FeedKindFilter` |
| components containing inline `<svg>` | **61** |

`@lucide/svelte` is already a **runtime dependency** (`web/package.json`, `1.33.0`), added in
sprint 23 (`56cc2e36`, #1110/#1111). So the tool has been present the whole time and the discipline
has not: the pasted path won on convenience 61 times.

⚠️ **No ADR covered this.** It was believed one did. It did not exist, which is precisely why the
practice drifted — there was nothing to point at in review.

The cost is not the bytes. It is that a pasted icon carries whatever stroke width, corner radius,
viewBox and optical size the source used, so a UI assembled this way slowly acquires several
incompatible visual grammars. Nobody notices in a diff, because each individual paste looks fine.
And an icon that exists only as markup inside one component cannot be swapped, themed, or resized
consistently with the others.

## Decision

**Every icon in `web/` comes from `@lucide/svelte`, imported by name.**

```svelte
import { Bot } from '@lucide/svelte';
<Bot size={16} aria-hidden="true" />
```

- **Adding an icon means importing one**, not pasting markup. Picking from lucide.dev is the
  intended workflow — the set is installed, so a chosen name just works.
- **Hand-authored `<svg>` is allowed only for things that are not icons**: logos, brand marks,
  illustrations, generated diagrams, and drawing surfaces. Those are artwork, not iconography, and
  a shared icon set has no opinion about them.
- **If lucide genuinely lacks a needed symbol**, add it as a named component under a single icons
  module so it still has one home and one import shape — do not inline it at the use site.
- **Accessibility rides the import**: an icon that carries meaning gets an accessible name; an icon
  that decorates text already labelled gets `aria-hidden="true"`. A pasted `<svg>` reliably gets
  neither.

## Consequences

- The 61 components holding inline `<svg>` are **not** migrated by this ADR. They are a follow-up
  sweep, tracked separately, because a 61-file mechanical change wants its own reviewable diff and
  its own before/after screenshots — a visual regression here is invisible to every automated gate
  we have.
- New work has no excuse: the rule is now written, so a pasted icon is a review comment rather than
  a matter of taste.
- ⚠️ The sweep must be **visually** verified, not just type-checked. Swapping a pasted glyph for its
  lucide equivalent changes stroke weight and optical size, and `npm run check` is blind to that.
- One dependency now sits on the render path for most of the UI. That is the trade being accepted:
  bus-factor and supply-chain surface in exchange for a consistent visual grammar. `@lucide/svelte`
  is tree-shaken per import, so the cost is per-icon rather than the whole set.
