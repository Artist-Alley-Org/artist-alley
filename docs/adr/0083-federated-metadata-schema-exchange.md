---
id: "0083"
title: Peers will need to exchange field schemas — the requirement is real and deliberately unbuilt
status: accepted
date: 2026-08-01
area: architecture
phases: []
supersedes: []
related:
  - "0012"
  - "0043"
  - "0053"
tags:
  - federation
  - metadata
  - iiif
excerpt: >-
  #738 deleted field_set_id, and the reasoning risked reading as "schema
  sharing was rejected". It was not. Federation transports content by
  reference and no metadata at all, while federated IIIF manifests already
  span instances — so two peers can appear in one viewer with field schemas
  nothing has ever reconciled. This records the requirement, why the deleted
  column was the wrong shape for it, and what to build instead.
---

## Context

**#738 removed `field_definition.field_set_id`.** The column had been plumbed through every
sqlc query, three `openapi.yaml` schemas and the generated models since the baseline, and
nothing had ever written a value to it. ADR 0012 had declared its purpose:

> `field_set_id` groups related fields into an export/import unit. Operators publish a
> `field_set` JSON to share with peers; peers import to adopt identical field schemas.

The deletion was correct and is not revisited here. But the argument advanced for it —
that four federation ADRs written after 0012 (0007, 0042, 0043, 0049) never mention field sets,
so the idea was not carried forward — **treated an absence of decision as a decision against.**
It is not. The person who designed federation confirms schema exchange is wanted; it simply has
not been built.

Recording that distinction is the entire purpose of this ADR. Without it, the next person to
read #738 concludes we considered federated schema sharing and rejected it.

### What federation actually carries today

Verified 2026-08-01: **no metadata at all.** The activity vocabulary has no field-definition or
field-value verb, and the outbox resolver projects no metadata onto an object. Federation moves
content by *reference*.

### Why that is already a problem, not a future one

ADR 0053 §Federation:

> Federated collections expose IIIF manifests that include canvases from federated assets. The
> canvas `id` field is the remote actor URI (per ADR 0043); IIIF clients fetch the remote canvas
> directly via that URI. AA does not proxy. **Cross-instance IIIF interop comes free.**

So a single Mirador manifest can already span two instances. Each instance renders its own
canvases' metadata from its own field definitions — and **nothing has ever reconciled those two
schemas.** One peer's `pipeline_stage` may be another's `stage`, or absent, or a `select` whose
option slugs differ. The viewer shows both without any indication the labels are not comparable.

ADR 0012 anticipated this and offered only a social answer: *"admins coordinate across peers by
adopting the same slugs."* There is no mechanism to do that coordination, which is what makes
this a real gap rather than a hypothetical one.

## Decision

**Federated field-schema exchange is a genuine requirement, deliberately unbuilt, and this ADR
is the record that it was never rejected.**

Three things are settled now so the eventual implementation does not relitigate them:

### 1. The unit of exchange is a list of field codes, not a persisted set

#738's decisive finding: **an export unit does not need to be persisted state.** Exporting N
fields needs an endpoint taking a list of field *codes*, chosen at export time. Persisting the
grouping buys a saved selection and costs a third grouping axis that must stay consistent with
`display_group` and `applies_to` forever.

If a saved selection is ever wanted, it is a convenience layer over the export call — not the
mechanism.

### 2. What travels, and what must not

Settled during #738 and preserved here. **In:** `code`, `label`, `description`, `type`,
`subject_kind`, `required`, `searchable`, `display_group`, `display_order`, and option *values*.

**Out, each because it names something that exists only on the sender:**

- `applies_to` — local asset-type integer refs. Valid enough on the receiver to bind to the
  **wrong** type, which is worse than failing.
- `read_capability` / `write_capability` — importing these silently widens or narrows access.
  The worst failure mode a schema import has.
- `extraction_source` / `extraction_mode` — depends on the receiver's pipeline.
- `default_value` and `field_default_override` (ADR 0081 §3, migration 00021) — team-scoped;
  those teams do not exist on the receiver.
- Per-option `status` / `replaced_by` (ADR 0012 amendment) — lifecycle is local editorial
  history, not schema.
- `origin_server_id`, ids, timestamps, audit refs, `deprecated_replacement_id`.

### 3. Collision is the normal case and must never resolve silently

Field codes are globally unique per instance, and the whole point is that peers adopt the *same*
slugs — so importing a schema that collides with an existing code is the expected path, not the
edge.

**Reject the whole import, return a per-field diff, let the operator choose per field.** Atomic
rejection keeps a half-adopted schema unreachable. A silent merge mutates a schema an operator
already depends on, which is worse than having no import at all.

## Consequences

- **`field_set_id` is gone and stays gone.** When this is built it introduces no column on
  `field_definition`; the export is a request, not stored state.
- **The trigger to build is IIIF, not federation generally.** The requirement bites when
  manifests span instances and their metadata needs to be comparable — which is already true.
  Until an operator actually federates with a peer running a different schema, the pain is
  latent.
- **Federation gaining a metadata verb is a separate, larger decision.** This ADR covers
  operator-initiated schema exchange (publish / import), not automatic propagation of field
  definitions over the activity stream. Those are different trust models and the second one
  needs its own ADR.
- ADR 0012's "admins coordinate by adopting the same slugs" is honest about the current state
  and should be read as describing a gap rather than a design.
