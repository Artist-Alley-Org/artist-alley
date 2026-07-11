---
id: "0025"
title: Brand workspace — design tokens + brand kits + guidelines portal
status: accepted
date: 2026-05-30
area: architecture
phases: 
  - "1.18"
  - "1.33"
supersedes: []
related: 
  - "0024"
tags:
  - architecture
  - ai
  - 3d
excerpt: >-
  Game studios with a marketing / brand org maintain brand guidelines — logos, color tokens, font stacks, voice rules, do / don't examples, usage permissions, license states. Today, this lives in PDFs / Figma / Confluence and goes stale within a sprint.
---
## Context

Game studios with a marketing / brand org maintain brand guidelines —
logos, color tokens, font stacks, voice rules, do / don't examples,
usage permissions, license states. Today, this lives in PDFs / Figma /
Confluence and goes stale within a sprint. A curated brand-guidelines
area is a common DAM surface; the audit (2026-05-30) flagged this as
a real fit for studios with public-facing IP — every AAA franchise,
every published indie.

Artist Alley is already the asset source-of-truth. The brand workspace
is "the curated subset of Artist Alley assets that comprises the brand
kit, plus the prose that explains how to use them."

## Decision

Add Phase 1.33 — Brand workspace — as a curated overlay on the
existing post + collection + theme model. Not a separate authoring
tool; a structured view + permission scope on top of what's there.

### Brand kit entity

A `brand_kit` row binds:

- **A name** + slug (e.g., `acme-galaxy-v2`).
- **A set of collections** that hold the canonical brand assets.
- **A design-token export** — colors, type, spacing, shape — stored
  as a JSON object that exports to: CSS custom properties, Tailwind
  config, iOS asset catalog, Android values XML, Figma tokens v3.
- **A markdown guidelines doc** authored in the admin shell
  (`/brand/{slug}/guidelines`) with embedded asset blocks (drop a
  post into the doc; it renders inline with the live thumbnail).
- **Usage rules** — `internal-only` / `partner` / `press` /
  `public` — surface as badges on every asset in the kit so anyone
  looking at the asset knows the rule.
- **A publish state** — `draft` / `active` / `superseded`.

### Brand portal surfaces

- `/brand/{slug}` — public portal page (if the kit is marked public)
  with the logos, palette, type, links to download official files.
- `/brand/{slug}/guidelines` — the markdown doc with inline assets,
  filterable for "show me only press-allowed."
- `/brand/{slug}/tokens.json` — the machine-readable token export
  for automated pipelines.

### Versioning

Brand kits supersede each other; superseding flags the prior kit as
`superseded` and keeps it live for the configured retention window
(default 90 days) so anyone deep-linking to a fading-out brand still
sees an archive notice with a link to the current version.

### Permissions

The brand kit edit capability is its own role (`brand_steward`)
gated by Pro+ tier — Community tier still gets the read surface.
The `usage_rule` per asset is enforced everywhere the asset surfaces:
download requires a click-through of the usage terms (Phase 1.18
conditional terms feature).

## Consequences

**Positive**

- Brand discipline ships in-product instead of as an off-platform
  PDF that goes stale.
- Token export means design pipelines (web, mobile, in-engine) can
  pull from the same source-of-truth.
- The "is this asset OK to use externally?" question has a
  single, queryable answer.

**Negative**

- Token export per target platform is real work; first cut covers
  CSS, Tailwind, JSON. iOS / Android / Figma exports as follow-ups.
- The portal page is a public surface so it inherits the licensing
  + audit obligations from ADR 0024.

## Alternatives considered

- **Just use a collection.** Doesn't carry tokens, doesn't carry
  guidelines, doesn't carry usage rules. Insufficient.
- **Integrate with Frontify / Brandfolder / etc.** Vendor lock-in
  and adds a third-party data trip for what is already our data.

## Reference

- Phase 1.33 in [`docs/roadmap.md`](../roadmap.md).
- Theme tokens (shipped) — the design-token export reuses this model.
- Conditional terms (Phase 1.18) — usage-rule click-throughs.
