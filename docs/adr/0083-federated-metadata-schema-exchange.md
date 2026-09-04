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

## Amendment 2026-09-01 — `regexp_filter` travels, `read_only` does not (#1173)

Two columns landed on `field_definition` in migration `00064`, and this ADR's §2 list has to say
which side of the line each falls on. Classification only. **Nothing here is implemented**, and
nothing should be: federation still transports no metadata at all, so there is no envelope to add
a property to and no round trip to test. The point of recording it now is that the reasoning is
fresh and the criterion is already written down.

**IN: `regexp_filter`.** The criterion in §2 leaves a property out when it "names something that
exists only on the sender". A pattern names the FIELD: it says what a value of `shot_code` looks
like, which is the same class of statement as `required` and `type`, both already in. A peer
adopting `shot_code` wants the shape as well as the name, and importing the pattern binds the
receiver to nothing local. It is also inert if ignored, which keeps a partial implementation
honest rather than dangerous.

**OUT: `read_only`.** It names who may write, which puts it with `read_capability` and
`write_capability`, and §2 excludes those because importing one "silently widens or narrows
access", calling that the worst failure mode a schema import has. The failure here is the narrowing
direction rather than the widening one, and it is quieter for it: a receiver that imported
`read_only` would find its own operators unable to edit a field they own, with no message naming
the peer that decided it. An access rule adopted from elsewhere is exactly the thing an operator
cannot debug.

The §3 collision rule needs no change. `regexp_filter` differing between two peers is an ordinary
per-field diff, and the whole-import rejection already puts the choice in front of the operator
rather than merging silently.

## Amendment 2026-09-03 — `display_condition` is IN, and it is the first property that names a SECOND field (#1173, #1119)

Migration `00065` adds `display_condition jsonb NULL` to `field_definition`, so §2's list needs one
more entry. Classification and import semantics only. **Nothing here is implemented**, and nothing
should be: federation still transports no metadata at all, and ADR 0099 §10 records that this sprint
built no import runtime either.

**IN: `display_condition`.** The §2 criterion leaves a property out when it "names something that
exists only on the sender". A condition names the FIELD: it says when `commission_deadline` is worth
offering, which is the same class of statement as `required`, `show_on_card` and `regexp_filter`, all
already in. It binds the receiver to nothing local, and it is inert if ignored, which is what keeps a
partial implementation honest rather than dangerous. ADR 0099 §1 settles the other half of the
question by making the property a form hint rather than an access rule, so importing one cannot widen
or narrow access the way `read_only` would.

### The sub-rule this property needs and no earlier one did

**Every property in §2's IN list so far describes the field on its own. This one REFERENCES A SECOND
FIELD.** That is new, and it changes what "importing a property" can mean, so the reference semantics
are written down here rather than left to be improvised.

The terms hold **field CODES**, which §1 already calls federation-stable and globally unique. So the
reference itself always survives transport. What may not survive is the **referent**: a peer can send
`commission_deadline` with a condition naming `work_type` while the receiver has no `work_type` at
all, or has one describing a different subject kind.

- **A missing referent is PRESERVED VERBATIM.** The term is not dropped, not rewritten, and not
  silently converted into an unconditional field. At runtime it is unresolvable and ADR 0099 §4's
  whole-condition fail-open shows the dependent, so the field behaves as it would have without the
  condition until the referent arrives. Dropping the term would look identical on the surface and
  would destroy the operator's intent the first time an import ran.
- **Resolvable terms obey the ordinary invariants.** An import whose referents all resolve must not be
  able to store a configuration PATCH would refuse, which means the receiver runs ADR 0099 §6's
  refusal set against the resolvable edges rather than trusting the sender's validation. Import is not
  a back door around a local rule.
- **A mixed condition validates the resolvable edges and preserves the unresolved ones verbatim.**
  Partial resolution is the normal case, not an error.
- **Two checks are DEFERRED for an unresolved edge:** cycle participation and N-way applicability
  contribution. Neither is answerable about a definition that is not here: an edge pointing at nothing
  cannot be placed in the graph, and there is no `applies_to` to intersect against.
- **Whoever later resolves those edges must confront the deferred checks.** That is the moment they
  become answerable and the moment a cycle could first close, so it is where the check belongs. An
  import that only ever validated at receive time would leave a cycle to be discovered by the graph
  walk of some unrelated later PATCH.
- **Bulk import inherits ADR 0099 §8's atomicity requirement.** A batch writing many conditions is the
  case that invariant was written for, not an exception to it.

The §3 collision rule needs no change. Two peers disagreeing about a condition is an ordinary per-field
diff, and the whole-import rejection already puts that in front of an operator instead of merging
silently. It is worth noting that the reference sub-rule makes a per-field cherry-pick more attractive
than it was, because importing a dependent without its controller is now a defined outcome rather than
an accident, but changing §3 is not this amendment's business.
