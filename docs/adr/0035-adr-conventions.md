---
id: "0035"
title: ADR conventions and documentation pipeline
status: accepted
date: 2026-05-31
area: process
phases: []
supersedes: []
related: []
tags:
  - documentation
  - tooling
  - conventions
excerpt: >-
  Every Artist Alley ADR ships with YAML frontmatter, follows a strict
  section structure, and is rendered with an automated cross-reference
  header (status badge, supersedes trail, related ADRs, related phases)
  by the docs site pipeline.
---
## Context

The first 34 ADRs accreted organically. Each carried date + status as
plain markdown bullets, sections were inconsistent (some carried
"Implementation", others didn't; some had "References", others
"Reference"), and cross-references between ADRs were prose mentions
the docs pipeline couldn't index. As the catalogue grew past ~30
entries, the operational pain showed:

- No way to ask "which ADRs affect Phase 1.42?" without grep.
- No way to know an ADR was superseded except by reading it.
- No filter UI on the docs site — every ADR was equally surfaced
  regardless of status.
- No automated lint — a typoed reference to "ADR 0038" silently
  pointed at nothing.

This ADR establishes the conventions that turn the ADR catalogue
from a folder of markdown files into a structured, queryable,
interconnected docs-site artefact.

## Decision

Every ADR ships with **YAML frontmatter**, follows a **strict section
structure**, and is consumed by a **build-time pipeline** that
validates references, derives inverse links, and renders an
automated metadata card at the top of each page.

### Frontmatter schema

```yaml
---
id: "0035"                    # zero-padded string, matches filename prefix
title: ADR conventions ...    # short title, matches first H1
status: accepted              # see lifecycle below
date: 2026-05-31              # YYYY-MM-DD, the decision date
area: process                 # see area taxonomy below
phases:                       # roadmap phase IDs this ADR informs
  - "1.42"
  - "1.18.B-3"
supersedes: []                # ADR IDs this replaces
related:                      # other ADRs that interact
  - "0017"
tags:                         # free-form topical tags
  - documentation
  - tooling
excerpt: >-                   # one-sentence summary, shown on the index
  Short single-sentence excerpt for catalogue list view.
---
```

**Required fields:** `id`, `title`, `status`, `date`, `area`,
`excerpt`. **Optional:** `phases`, `supersedes`, `related`, `tags`.
Strings stay quoted; arrays may be empty (`[]`).

### Status lifecycle

| Status | Meaning |
|---|---|
| `proposed` | Drafted, open for discussion. Renders with a yellow "Proposed" badge. |
| `accepted` | Decision is in effect. Renders with a green badge. The default for landed ADRs. |
| `superseded` | A later ADR replaces this one. Renders with a grey badge + a `Superseded by ADR XXXX` callout. Body content stays intact as historical record. |
| `deprecated` | The thing this ADR governs no longer exists or no longer matters. Renders with a grey badge + a "Deprecated" callout. |
| `rejected` | Drafted but not adopted. Kept as historical record. |

**Status transitions are one-way per row** — once superseded, an ADR
does not return to accepted. A new ADR with a clean `id` is the
correct path forward.

### Area taxonomy

| Area | Examples |
|---|---|
| `architecture` | Backend shape, data model, service topology |
| `security` | Auth, encryption, secrets management |
| `licensing` | License model, dual-license direction |
| `monetization` | Pricing tiers, ad / commerce model |
| `process` | Conventions, workflow, branching, release |
| `ux` | Frontend patterns, design system, accessibility |
| `ops` | Operability, observability, deployment |
| `infrastructure` | Storage, federation, networking |
| `extensibility` | Add-ons, plugins, integrations |

Areas are used for grouping in the sidebar and as filter facets
on the ADR index page. Adding a new area is a code change in the
sync script (so it's intentional, not accidental).

### Section structure

The body uses a fixed section ordering. Sync-time linter rejects
ADRs that diverge:

```markdown
# ADR XXXX: <Title>

## Context

Why we are deciding this. What constraints / problems / failure
modes drove the decision. Cite specific failure modes that motivate
each major decision point.

## Decision

What we are deciding. State it crisply. Use sub-headings
(`### Sub-decision`) when the decision has multiple components.

## Consequences

### Positive

Expected benefits.

### Negative

Costs, risks, accepted tradeoffs. Be honest — a consequences
section that has only positives is a tell that the decision was
not actually weighed.

## Alternatives considered

What we rejected and why. One bullet per alternative. Brief — if
an alternative is complex enough to need paragraphs, it should
probably be its own ADR.

## Implementation                                # optional

Concrete shipping plan: phase breakdown, code locations, contracts.
Omit if the decision is purely architectural.

## References                                    # optional

External links + cross-references the prose section didn't already
mention. The pipeline auto-renders the related-ADR card from
frontmatter — references here are for *external* resources.
```

### Cross-reference rules

- **Phase IDs** match the roadmap JSON exactly: `1.42`, `1.18.B-3`,
  `1.18.A`. Sync-time linter rejects unknown phase IDs.
- **ADR IDs** match the filename prefix: `0017`, `0034`. Sync-time
  linter rejects unknown IDs and emits a warning when an ADR has
  no inbound links (potential orphan).
- **External links** use fully-qualified URLs.

### File naming

`docs/adr/NNNN-slug-with-hyphens.md` where `NNNN` is zero-padded
four-digit. New ADRs claim the next free number. Renaming an
existing ADR's slug is fine; the `id` field is the stable handle.

### Pipeline behaviour (sync-adrs.mjs)

The sync script does five things on every site build:

1. **Parse + validate** every ADR's frontmatter against the schema.
   Fail the build on missing required fields, unknown status,
   unknown area, unknown phase / ADR cross-references.
2. **Derive inverse maps** — `superseded_by` from `supersedes`,
   `back_links` from `related`, and `adrs_by_phase` so the roadmap
   visualization can show "see ADRs 0034, 0026" next to each phase.
3. **Emit augmented MDX** into `site/src/content/docs/adr/` — each
   ADR's frontmatter gains the derived fields and the rendered
   page lays them out via the `<AdrCard>` component.
4. **Generate the index page** `site/src/content/docs/adr/index.mdx`
   with a filterable table of every ADR (status, area, date, title,
   excerpt).
5. **Lint orphan warnings** — ADRs with zero inbound links + no
   roadmap-phase reference probably need either a `related:` entry
   somewhere or a check that they're still relevant.

### Site-side rendering

Each ADR page renders an `<AdrCard>` block at the top:

- **Status badge** colored by lifecycle state.
- **Area + date** as the secondary line.
- **Supersedes trail** — visible block showing this ADR replaces
  ADR XXXX (with link), or is replaced by ADR YYYY.
- **Related ADRs** — chip row of linked ADR IDs + titles.
- **Related phases** — chip row linking to roadmap entries.

The body follows below in regular Starlight prose styling.

The **ADR landing page** at `/adr/` is a filterable table — facets
for status, area, and (related-)phase — so anyone landing on the
docs site can sweep the catalogue.

## Consequences

### Positive

- Cross-references become real graph edges rather than prose
  mentions — broken links are caught at build, the docs site can
  render bidirectional connections.
- The catalogue scales: 100 ADRs is queryable through the index
  filters; the sidebar groups stay coherent through area buckets.
- Frontmatter is the source of truth for ADR metadata, so external
  tooling (changelog generators, project planners) can consume the
  catalogue programmatically.
- Status lifecycle is explicit. A superseded ADR no longer looks
  like an active recommendation.
- Authoring a new ADR is paint-by-numbers: copy a template, fill
  the frontmatter, write the body in the standard sections.

### Negative

- Existing 34 ADRs need a one-time migration to the new format.
  Mitigation: script-assisted bulk migration (see Implementation).
- The frontmatter schema is now a contract — adding fields needs
  care so older ADRs don't break. Mitigation: every field is
  optional except the documented required set; defaults are
  applied for omitted fields.
- Authors need to remember the section order. Mitigation: linter
  fails the build with a clear error message naming the missing
  section.

## Alternatives considered

- **Keep the loose markdown shape and add a docs-site table by
  hand.** Doesn't scale past a handful of ADRs; broken cross-refs
  stay broken. Rejected.
- **Use a third-party ADR-tooling project (adr-tools, adr-manager,
  etc.).** Most tools target the standalone CLI / repo flow, not
  docs-site integration. We'd end up with two formats to reconcile.
  Rejected.
- **YAML-only ADRs with rendered markdown body.** Removes the
  human-readable plain-markdown source. Authors edit prose, not
  data structures. Rejected.

## Implementation

Sub-phases all land together (single PR / commit on
`feat/viewer-polish`):

1. This ADR (0035) ships first to establish the standard.
2. Bulk migration script adds frontmatter to ADRs 0001–0034 by
   parsing existing `- Date:` + `- Status:` lines and inferring
   `area` + `phases` + `related` from body content. Output is
   reviewed before commit.
3. `site/scripts/sync-adrs.mjs` upgraded to validate, derive
   inverse maps, augment MDX, and emit the index page.
4. `site/src/components/AdrCard.astro` renders the metadata block.
5. `site/src/components/AdrIndex.astro` renders the filterable
   landing page; consumed by the new
   `site/src/content/docs/adr/index.mdx`.
6. `site/astro.config.mjs` sidebar grouping switches from flat
   autogenerate to area-grouped + status-aware ordering.

## References

- Michael Nygard's original ADR pattern — the structural inspiration.
- [`docs/adr/`](.) — the catalogue this ADR governs.
