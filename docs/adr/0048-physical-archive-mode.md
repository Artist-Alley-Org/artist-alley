---
id: "0048"
title: Physical archive mode — accession numbers, loans, provenance
status: accepted
date: 2026-06-06
area: architecture
phases:
  - "1.51"
supersedes: []
related:
  - "0011"
  - "0012"
  - "0018"
  - "0020"
  - "0024"
  - "0034"
  - "0038"
  - "0043"
tags:
  - architecture
  - archive
  - museum
  - loans
  - provenance
  - standards
excerpt: >-
  Physical archive features (accession numbers, location tracking,
  loan management, provenance chains, standards interop) ship
  first-party in-tree as a default-off feature flag — not a premium
  add-on, not a third-party plugin. Museums, galleries, libraries,
  archives, and studios with physical prop / costume collections
  are a real target audience, not a paywallable extension.
---
## Context

The Artist Alley whitepaper claims the underlying model — assets,
posts, collections, metadata, review, versioning, archive — works
for any team cataloging items with associated digital media, citing
museums (artifacts with provenance scans), libraries (manuscripts
with digitisation), galleries (loan management with custody chains),
archives (document collections), and studios (physical props +
costumes + scripts with their digital companions) as adjacent use
cases.

That claim is currently a hand-wave. The product ships features
shaped for game and animation studios (review canvas, presentation
rooms, DCC integrations, brand workspace) but nothing physical-
archive-specific. An institution looking at Artist Alley today
sees:

- ✅ Asset model, metadata, collections, federation, audit log — all
  generalise cleanly.
- ❌ No accession number management, no auto-incrementing per
  format.
- ❌ No location tracking beyond the asset's digital storage path.
- ❌ No loan record entity — no way to track "Studio A borrowed
  this prop for production X, due back 2026-09-15."
- ❌ No provenance chain — chain-of-custody as a first-class
  entity beyond audit-log events.
- ❌ No interop with the museum / archive standards (Spectrum,
  LIDO, Dublin Core, CIDOC CRM) that institutions migrating
  from legacy systems expect.

The result: a museum or gallery operator evaluating Artist Alley
hits the "but where do accession numbers live?" question in the
first hour and concludes the product isn't for them. The whitepaper's
claim becomes false at first contact.

This ADR closes that gap. Physical archive features become a
documented capability path, not a gesture toward "the foundation
generalises."

### Why this isn't a premium add-on

ADR 0038 specifies two criteria for the premium add-on layer:

1. **Operator monetizes third parties** through the feature
   (commerce, ads, memberships).
2. **We carry operational burden on their behalf** (cloud-bridge
   AI, managed backup, premium SSO).

Physical archive features fit neither. Museums, galleries, libraries,
and archives don't typically monetize visitors at scale through the
DAM (they have separate ticketing / shop infrastructure). We don't
operate anything special on their behalf — they self-host the same
binary every other operator does.

Making it a paid add-on would be precisely the "clawback features
into paywalls" failure mode ADR 0038 §"Tier orthogonality" forbids:
a real audience's primary need, gated for revenue. The walled-garden
rule from ADR 0038 is "Premium add-ons add new capabilities; they
never reach back into existing ones." Physical archive is a *new*
capability, but it serves an audience the product is positioned to
serve. The right answer is "ship it free, default-off."

### Why this isn't a plugin

The plugin layer (ADR 0034 §"Distinction from integrations and
plugins" + the deferred WASM-via-Extism story) is for third-party
extensions and out-of-band heavy components. Physical archive
features are first-party (we author them), small (compared to the
binary), and don't ship out-of-band artifacts. Plugins are the wrong
shape.

### What we already have that maps cleanly

| Existing capability | Maps to |
|---|---|
| Asset model + metadata fields | Object catalog records |
| Collections | Museum collections, exhibition groupings |
| Brand workspace | Curatorial / exhibition workspaces |
| Workflow states (per-deployment) | Loan-state lifecycle |
| Capability codes | `loans.approve`, `accession.assign`, `provenance.edit` |
| Audit log | Chain-of-custody event log baseline |
| Federation (per ADR 0043) | Cross-institution loan tracking + shared workspace |
| Share links (per ADR 0018) | Public exhibition pages, external loan request portals |
| Sensitivity tiers (per ADR 0020) | Restricted-access objects (NDA-bound items, donor anonymity) |
| Privacy / DSAR (per ADR 0024) | Donor + lender personal-data handling |

The new work concentrates on the physical-object-specific concepts
that don't naturally fall out of the existing model.

## Decision

Physical archive features ship **first-party, in-tree, default-off
via a `physical_archive` feature flag** in `system_config`.
Operators flip the flag in `/admin/system/features`; the binary
exposes the additional admin surfaces, table extensions, and
import/export tooling. The flag is reversible (turn it off, the
surfaces hide, the data stays).

### Target persona

The flag is built for **museum / gallery / library / archive
operators + studios with physical prop / costume / set-piece
collections**. The non-physical-archive operator (game studio,
animation studio, illustration studio, motion design house) never
flips the flag and sees the existing product unchanged.

### What ships

Four sub-phases. Each lands as its own commit; each is reviewable
independently.

#### 1.51.A — Accession + physical-object foundation

The smallest viable physical-archive surface. Museums can catalog
objects after this ships.

- **Per-collection accession-number formats.** An `accession_formats`
  table holds operator-defined templates with auto-increment
  behavior. Examples: `{year}.{collection}.{seq:04}` →
  `2026.archive.0001`, `2026.archive.0002`; `M-{year}-{seq:05}` →
  `M-2026-00001`; `{prefix}-{seq}` with operator-defined prefix.
  Format template language is small + documented (placeholder
  tokens for year, month, day, collection code, prefix, sequence
  with padding).
- **New `physical_object` asset type.** Inherits the asset model
  (file_hash, owner, sensitivity tier, ACLs) and adds:
  `accession_number` (auto-assigned per the configured format),
  `accession_format_id` (FK), `dimensions` (height × width × depth
  + unit), `weight` (mass + unit), `materials` (free-text +
  optional Getty AAT crosswalk for the controlled-vocab crowd),
  `current_location_id` (FK into `locations`), `acquisition_method`
  (purchase / gift / loan-in / commission / transfer / found-in-
  collection), `acquisition_date`, `acquisition_source`.
- **Hierarchical `locations` table.** Building → room → cabinet →
  shelf → bin. Self-referential parent_id; closure table for fast
  reachability. Per-location capacity hints (optional, free-text).
- **Object-bytes-vs-digital-companions model.** A physical object's
  primary record holds the metadata + accession number; its
  `asset_companions` rows hold the digital companions (provenance
  scans, conservation photos, 3D scans, IIIF tile sets, condition
  reports). Existing companion infrastructure carries this without
  change.
- **Admin surface for format management + location-tree editor.**
  `/admin/archive/accession-formats` and `/admin/archive/locations`.
- **Bulk re-accession tooling** (rare but mandatory for migrations):
  admin can re-issue accession numbers across a collection after
  changing the format template; old numbers are preserved as
  `historical_accession_numbers` (one-to-many table) so external
  citations still resolve.

#### 1.51.B — Loan system

- **`loans` table.** One row per loan-in or loan-out arrangement.
  Carries: counterparty_institution (FK or free-text fallback),
  contact_person, contact_email, loan_direction (`in` / `out`),
  request_date, agreed_start_date, agreed_end_date, return_date,
  insurance_value, transport_company, signed_agreement_attachment
  (asset reference), condition_at_handoff_id, condition_at_return_id,
  notes.
- **`loan_objects` join table.** Many-to-many between loans and
  physical objects (a single loan can include 30 prints from a
  collection).
- **Standard 7-state loan workflow** as the default per-deployment
  workflow_states + workflow_transitions seed:
  `requested → approved → packed → in_transit → received → on_display → returned`
  for loans-in; mirror states for loans-out
  (`incoming_requested → approved → preparing → shipped → received_by_borrower → on_display → returning → returned`).
  Operators can override the state set per-deployment per the
  existing workflow infrastructure.
- **Counterparty institution registry.** Lightweight: free-text
  institution name + optional federation-peer link (an institution
  that's also a paired Artist Alley peer gets richer integration).
  Don't try to be a global museum directory.
- **Loan-state transitions emit audit events** (`loan.*` category):
  `loan.requested`, `loan.approved`, `loan.shipped`, `loan.received`,
  `loan.returned`, etc.
- **Insurance value tracking** is a single decimal field — not a
  full insurance-system integration. Operators who need
  underwriter integration handle that out of band; we record the
  value + the policy number.
- **Extension flow.** Active loans can be extended by mutual
  agreement; extension generates a new dated record on the loan
  with a signed-amendment attachment.

#### 1.51.C — Provenance + condition reports

- **`provenance_events` table.** Chain-of-custody events per object:
  acquisition, transfer between collections, conservation treatment,
  loan-out, return, deaccession. Each event has actor, timestamp,
  type, target_object, optional source/destination location, optional
  related_loan_id, optional attached documents, free-text notes.
- **Object provenance timeline view.** Per-object UI surface
  rendering the chain from acquisition forward. Audit-log events
  with `loan.*` / `accession.*` / `provenance.*` categories cross-
  reference into the timeline so the visible chain is the union
  of explicit provenance events + relevant audit events.
- **Structured condition reports.** A `condition_reports` table per
  object: rating (excellent / good / fair / poor / critical),
  inspector, date, attached imagery (asset references), free-text
  observations, recommended treatment. Reports trigger on loan
  handoff + return automatically; ad-hoc reports possible.
- **Condition report comparison.** Per-loan, before/after comparison
  view between handoff and return reports (rating delta + diff'd
  imagery side-by-side). Materialises the "did the borrower
  damage my Picasso" question as a structured deliverable.

#### 1.51.D — Standards interop

The interop layer that unlocks migration from legacy systems and
makes Artist Alley credible to institutions evaluating it against
established tools.

- **Spectrum 5.0 procedure crosswalk.** The 21 documented
  procedures (Object entry, Acquisition, Loans in, Loans out,
  Movement, Location and movement control, Inventory control, etc.)
  map onto our existing handler endpoints. Documented in
  `docs/spec/archive/spectrum-crosswalk.md`; not a code shipment,
  it's a documentation deliverable proving our handler surface
  covers the canonical procedures institutions train on.
- **LIDO XML import + export.** LIDO is the museum-object-exchange
  XML schema. Import maps an external LIDO document onto our
  physical_object + metadata fields + provenance chain. Export
  emits per-object LIDO documents for sharing with peers or
  external systems. Implementation lives at
  `internal/archive/lido/`; CSV-shaped fallback for institutions
  not on LIDO.
- **Dublin Core crosswalk.** Per-asset metadata projected as
  Dublin Core's 15 terms for any consumer that prefers the simpler
  surface. Read-only; exposed via `GET /assets/{id}/dublin-core`.
- **CIDOC CRM crosswalk (optional).** A documented crosswalk
  document (no code) that maps our entities onto CIDOC CRM classes
  for institutions that adopted CIDOC as their canonical model.
  We don't ship a CIDOC-shaped data model — see §"What we don't
  do" below.
- **Spectrum CSV import.** A practical migration path: institutions
  exporting from legacy systems often have a Spectrum-aligned CSV
  ready. Importer at `/admin/archive/import/spectrum-csv` ingests
  the CSV onto physical_objects + accession_formats + locations +
  provenance_events with a dry-run preview before commit.
- **Getty Vocabulary integration (optional).** The metadata field
  system (per ADR 0012) already supports controlled vocabularies.
  An optional admin-toggleable field that auto-completes against
  Getty AAT / ULAN / TGN / CONA via their public APIs lights up
  for the `materials`, `creator`, `geographic_origin`, and
  `subject` fields. Configurable so institutions that have a
  paid Getty agreement can use it; cached locally so we don't
  hammer Getty's free endpoint.

### What we don't do

- **Adopt CIDOC CRM as our canonical data model.** CIDOC is a deep
  ontology designed for the museum-research-community. It is
  wonderful for cross-institution semantic mapping; it is a
  terrible operational data model for a self-hosted DAM. Adopting
  it would push every operator into a many-class abstract model
  that needs per-institution interpretation. We expose a CIDOC
  crosswalk document for institutions that need it; we don't bend
  our schema around it.
- **Build our own global institution registry.** Counterparty
  institutions for loans are free-text + optional federation-peer
  reference. We don't try to be the "global directory of museums,"
  and we don't centralise counterparty data on artist-alley.org.
- **Conservation workflow at v1.** Real museums need conservation
  treatment records, environmental monitoring, pest management,
  light-exposure logs. That work is deep specialty territory that
  intersects with lab systems we don't operate. A future phase
  could ship this; it's not in 1.51. If it ever ships, evaluate
  premium-add-on fit (the "carry operational burden" criterion
  applies if we integrate with conservation-lab vendor APIs we
  host).
- **Insurance / underwriter API integration.** We record insurance
  value and policy number as fields; we don't connect to
  underwriter systems. Same logic: deep specialty work, real
  audience, but not in 1.51's scope.
- **Per-object physical-security alarming.** No RFID, no IoT
  sensors. Operators with that infrastructure integrate it via
  webhooks (Phase 1.18 integration layer).

### Feature-flag mechanics

`physical_archive` lives in `system_config` as a boolean. Default
false. When false:

- The new admin surfaces (`/admin/archive/*`) return 404.
- The `physical_object` asset-type is hidden from the upload modal.
- The `locations`, `loans`, `provenance_events`, `condition_reports`,
  `accession_formats` tables exist (schema always present) but the
  handlers gate on the flag before reading or writing.
- The Spectrum / LIDO / Dublin Core endpoints return 404.

When true:

- Admin surfaces appear with the new shells.
- Upload modal shows "physical object" as a new asset-type option.
- The full handler surface is live.

Flipping the flag is reversible: off-to-on shows the surfaces; on-
to-off hides them but preserves the data. An operator turning the
flag off after seeding test data doesn't lose anything; turning it
back on resumes where they left off.

### Federation interaction

Per ADR 0043 §"Trust model", per-object share grants extend cleanly
to physical objects. An institution-to-institution loan flow that's
federated peer-to-peer:

- Studio A's Artist Alley shares the `physical_object` and its
  digital companions via `aa:Share`.
- The loan record itself can be shared as a separate object
  (`object_kind='loan'` in `federation_shares`).
- Federated loan-state transitions emit `aa:WorkflowTransition`
  activities that the counterparty peer renders.
- Insurance + signed agreements remain attached to the loan record
  on the origin instance; the peer renders read-only.

A future `aa-museum-loan-registry` premium add-on could federate
loan records across a wider directory of institutions (cross-
institution discovery, standardised loan request forms, escrow-like
agreement workflows). That add-on fits the ADR 0038 criteria
("operational burden we carry": the registry infrastructure + the
discovery service + the agreement-template legal review). Not in
1.51; reserved for a future phase.

## Consequences

### Positive

- **The whitepaper's "any team cataloging items with associated
  media" claim becomes true at first contact.** A museum operator
  evaluating Artist Alley finds the accession numbers, the loan
  records, the provenance chain, the Spectrum-aligned procedures.
- **Real audience served.** Museums + galleries + libraries +
  archives are a meaningful market; studios with physical prop
  archives are a meaningful niche within the existing game /
  animation persona.
- **In-tree default-off keeps the existing personas unaffected.**
  Game studios never flip the flag; their experience is unchanged.
  No bloat for operators who don't need it.
- **No premium-add-on clawback.** Stays consistent with ADR 0038's
  "we never paywall features a real audience's primary need
  requires." The walled-garden rule holds.
- **Federation composes cleanly.** Inter-institution loans federate
  through the same walled-garden protocol every other federated
  workflow uses. No new federation primitives needed.
- **Standards interop unlocks migration paths.** Spectrum CSV + LIDO
  import + Dublin Core export = institutions can move FROM legacy
  systems TO Artist Alley without rebuilding their catalog by hand.
- **Premium add-on reserved for the genuinely-operational-burden
  layer.** Cross-institution loan registry, conservation
  integration, insurance / underwriter API integration all
  potentially fit ADR 0038's criteria — but they ship later,
  when there's real demand. The core stays free.

### Negative

- **Real engineering investment.** Four sub-phases is months of
  work. Accession-number templating, location hierarchy, loan
  state machine, condition-report comparison view, Spectrum / LIDO
  import + export are all real deliverables.
- **Schema growth.** New tables (`accession_formats`, `locations`,
  `loans`, `loan_objects`, `provenance_events`, `condition_reports`,
  `counterparty_institutions`, `historical_accession_numbers`) +
  new columns on `assets`. The post-1.49-cleanup baseline carries
  these forever per ADR 0046's "post-v1.0 append-only" rule, so
  the schema decisions land carefully.
- **Domain knowledge required.** Museum + archive operators expect
  the loan workflow to feel like the procedures they trained on.
  Getting Spectrum-aligned naming + the standard 7-state loan
  lifecycle right requires reading the standards documents end-
  to-end. Not just engineering work.
- **Two ongoing maintenance burdens.** New tables to keep in the
  meta-index per ADR 0042; new Getty Vocabulary integration that
  depends on a free public endpoint we don't control.
- **Conservation + insurance gaps will be visible.** Operators
  used to integrated conservation-lab workflow will notice we
  don't have it. The "what we don't do" section is explicit so
  operators evaluate honestly, but the gap is real and a future
  phase has to address it if the museum audience grows.

## Alternatives considered

- **Premium add-on `aa-archive`.** Charge for the feature. Rejected
  per ADR 0038 — physical archive doesn't meet either of the two
  qualifying criteria (revenue-extraction or operational-burden).
  Paywalling it would be a clawback of "what the audience's
  primary need requires."
- **Third-party plugin (Phase 1.23 WASM ecosystem).** Defer to a
  community plugin. Rejected because (a) the plugin ecosystem is
  years out; (b) physical archive is first-party-scale work;
  (c) standards interop + audit + federation hooks need core
  integration, not a plugin sandbox.
- **Adopt CIDOC CRM as the canonical data model.** Considered.
  Rejected because CIDOC is a research ontology not a transactional
  data model. Forcing every operator through CIDOC abstraction adds
  cost for zero operational gain. Crosswalk yes, canonical no.
- **Fork the product for the museum audience.** Considered. Rejected
  because it doubles the maintenance burden, breaks federation
  between studio and museum operators (the underlying entities
  diverge), and the audiences have substantial overlap (studios
  with prop archives, galleries that also run as digital art
  portals).
- **Skip standards interop; ship only the entities.** Considered.
  Rejected because the standards interop is the migration path
  from legacy systems. An institution that can't import their
  existing catalog from Spectrum / LIDO / a Spectrum-aligned CSV
  won't adopt — they're not rebuilding a 50,000-object catalog
  by hand.

## Implementation

Phase 1.51 lands across four sub-phases per the Decision section
above. Sub-phase ordering matters:

1. **1.51.A — Accession + physical-object foundation** (must land
   first). The other sub-phases depend on the physical_object
   asset-type + accession infrastructure.
2. **1.51.B — Loan system** (depends on 1.51.A). Loans bind
   physical_objects via `loan_objects`.
3. **1.51.C — Provenance + condition reports** (depends on 1.51.A
   and 1.51.B). Conservation reports trigger on loan handoff /
   return; provenance events reference loans.
4. **1.51.D — Standards interop** (depends on all prior). Import /
   export needs the full entity model in place.

Schema changes land in 1.51's own forward-only migrations on top of
the post-1.49 baseline. Per ADR 0046's post-v1.0 rule, the schema
shape decisions here are permanent — careful review before each
sub-phase migration.

Catalogue meta-index (`docs/catalogs.md`) gains new entries per
ADR 0042 for: accession-format placeholder tokens, location-type
discriminators (if any), loan-state vocabulary, loan-direction
enum, provenance-event-type enum, condition-rating scale,
acquisition-method enum.

Coding-standards no changes — physical archive handlers follow
every existing convention.

## References

- ADR 0011 — Asset entity. Physical objects extend the asset model.
- ADR 0012 — Metadata model. Accession-number formats build on the
  field-definition system.
- ADR 0018 — Share links. Public exhibition pages + external loan
  request portals reuse share-link infrastructure.
- ADR 0020 — Asset gating & NDA workflow. Sensitivity tiers extend
  to restricted-access objects (donor-anonymity, lender confidentiality).
- ADR 0024 — Privacy & consent. Lender + donor PII follows the
  same DSAR + retention rules as user PII.
- ADR 0034 — Capability add-ons. Physical archive is *not* an
  add-on; this ADR documents why.
- ADR 0038 — Premium add-on layer. Physical archive is *not* a
  premium add-on; this ADR documents why explicitly.
- ADR 0043 — Federation walled-garden protocol. Inter-institution
  federated loans use the existing `aa:Share` + `aa:WorkflowTransition`
  activity vocabulary; no new federation primitives.
- [Collections Trust Spectrum 5.0](https://collectionstrust.org.uk/spectrum/) —
  the procedural standard our handler surface aligns with.
- [LIDO Schema](http://lido-schema.org/) — museum object exchange
  XML; the import + export target.
- [Dublin Core Metadata Initiative](https://www.dublincore.org/) —
  the simpler-metadata crosswalk target.
- [CIDOC CRM](https://www.cidoc-crm.org/) — the deep ontology we
  publish a crosswalk against but don't adopt as canonical.
- [Getty Vocabularies (AAT / ULAN / TGN / CONA)](https://www.getty.edu/research/tools/vocabularies/) —
  controlled-vocabulary endpoint integration for institutions that
  use them.
- [CollectiveAccess](https://collectiveaccess.org/) — open-source
  museum / archive cataloging tool studied for spec / capability-
  shape inspiration only per ADR 0040 clean-room methodology.
