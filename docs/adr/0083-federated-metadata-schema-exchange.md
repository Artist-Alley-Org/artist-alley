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

## Amendment 2026-09-04 — §2's list is completed, and lifecycle stays local (20b hygiene)

§2 was written during #738 and has been extended one property at a time ever since, which left a
gap nobody was looking at: **six properties on `field_definition` appear in neither list**, and four
of them assert `FEDERATES with the definition` in their own API descriptions. The authority is
supposed to be this section, so a property classified only in a description is classified nowhere.

Measured on `dev` at `3d53230a`, the unclassified set was `show_on_card`,
`show_in_advanced_search`, `show_on_upload`, `edit_tab`, `open_vocabulary` and `status`. Everything
already listed agrees with its own description, so this is a completion rather than a correction.

**IN, each because it names the FIELD rather than the server:**

- **`show_on_card`**, **`show_in_advanced_search`**, **`show_on_upload`**, **`edit_tab`.** All four
  are participation or placement hints: they say where a field belongs on a surface, which is the
  same class of statement as `type` and `display_group`, both already in. None binds to anything
  local: unlike `applies_to`'s asset-type integers, a tab name is a string the definition carries.
  Each is inert if ignored, which keeps a partial implementation honest rather than dangerous, and
  all four already claimed to travel, so this records what the API has said all along.
  ⭐ One consequence worth naming for `edit_tab`, because it is new since #1173 shipped its
  consumer: a receiver may have no tab of that name, and that is fine. Sprint 20b's rule builds
  buckets from whatever values the rendered definitions carry, so an imported tab name simply
  creates a bucket on the receiver rather than failing to find one.
- **`open_vocabulary`.** It says what a write to this field's vocabulary means: an unknown term
  creates the term instead of being refused. That is a statement about the field, in the same class
  as `required` and `regexp_filter`, and a peer adopting the field wants the write policy along with
  the shape.

**OUT, for the reason already written into this section:**

- **`status`.** §2 excludes per-option `status` / `replaced_by` because "lifecycle is local
  editorial history, not schema", and the definition's own `status` is the same thing one level up.
  That THIS server has deprecated or archived a field is a decision its operators made about their
  own corpus; importing it would retire a field on the receiver that the receiver may still be
  using, and the receiver has no way to tell that from a field that was never offered. Retirement
  stays a local act.

The §3 collision rule needs no change: each of these differing between two peers is an ordinary
per-field diff, and whole-import rejection already puts the choice in front of the operator.

⚠️ **Classification only, and nothing here is implemented.** Federation still transports no metadata
at all, so there is no envelope to add a property to and no round trip to test. The point of
finishing the list now is that §2 can be trusted as the authority again: before this, four
properties were classified in a description and two were classified nowhere.

## Amendment 2026-11-17: a Dublin Core baseline beneath exchanged schemas (#1513)

Exchanged field schemas stay as this ADR decides. Beneath them, every
peer that negotiated the `archival-1` profile (spec §17) understands one
fixed baseline: the `description` member of `aa:asset`, a plain-text
subset of the DCMI Metadata Terms (`title`, `description`, `creator`,
`subject`, `date`, `language`), plus `rights` (a Creative Commons or
RightsStatements.org URI) and `requiredStatement`. A receiver whose
local schema has no matching fields can still show and preserve that
baseline, so an asset is never received with no description at all.

Rights need no new column on the sending side: a rights URI and a
required statement are two field definitions in the existing `rights`
display group, beside today's free-text `copyright` and `credit`. The
mapping onto peer-visible values, with visibility and sensitivity
filtering, is #1577. Nothing here is implemented yet.

## Amendment 2026-11-22: the export half is built, and this is its wire contract (#1576)

`POST /api/v1/fields/export` (operation `exportFieldSchema`) is the first implemented piece of
this ADR. It exports; it does not import. Preview, atomic import, provenance and the two-instance
adoption gate are separate work under #1576. Nothing here propagates over federation: the export
is a document an operator takes from one server, as §Consequences requires.

### Selection (§1, as decided)

- The request is `{"codes": [...]}`: 1 to 100 entries, each matching the field-code rule
  `^[a-z][a-z0-9_]*$`, counted as sent. Duplicates fold into one. There is no persisted set and
  no "export everything".
- **Only `active` definitions export.** A selected code that is unknown, `deprecated` or
  `archived` refuses the whole request (422, `unknown_codes` first, then `retired_codes`), with
  the sorted list of offending codes. There is no partial export. That a definition is retired
  here is a local decision (the 2026-09-04 amendment), and exporting a retired field as a
  schema to adopt would hand that decision to a peer.
- Authorization comes before any stored schema is read: `fields.admin` or `system.admin`.
  Anonymous gets 401 and a member 403, with bodies that do not depend on the selection. The
  ordinary `GET /fields` gate (any signed-in user) is deliberately not inherited, because an
  export is an administrative act. Two rejections come earlier and depend on the request bytes
  alone: a body over 64 KiB (413, a route middleware in front of the generated adapter, which
  otherwise decodes the body before the operation runs) and a body that is not JSON (400).

### The document

`{"format": "aa-field-schema/1", "fields": [...]}`. `format` is the only version marker: one
string naming the contract and its version, so an importer has one thing to compare and must
refuse any value it does not implement. Fields are sorted by `code`, vocabulary terms keep their
stored order, and there is no time, request id, server id or origin claim, so the same selection
over the same schema exports byte-identical JSON. An origin or provenance claim is left to the
import design. If one is added, it is a separate untrusted claim, never a local id or an
authorization assertion.

Each field carries exactly the §2 IN list as completed by the amendments: `code`, `label`,
`description`, `type`, `subject_kind`, `required`, `searchable`, `display_group`,
`display_order`, `options`, `regexp_filter`, `display_condition`, `show_on_card`,
`show_in_advanced_search`, `show_on_upload`, `edit_tab`, `open_vocabulary`. **Every member is
always present**: an optional property with no value is `null` (`regexp_filter`,
`display_condition`, `edit_tab`), and `description` is `""` when empty. The projection is built
member by member from the stored columns into a DTO with no other members. It does not reuse the
`FieldDefinition` API shape, so a column added later reaches the export only when it is
classified here.

- **`options`** is an array (possibly empty) for `select`, `multi_select` and `tree`, and `null`
  for every other type. Each term is `{value, label, children}`. `label` is `null` when the term
  has no label of its own, and `children` is `[]` for a leaf. Only active terms travel. A
  `deprecated` or `archived` term is left out **with its subtree**, because a child exported
  without its retired parent would claim a place in a tree that does not exist. Per-option
  `status`, `replaced_by` and aliases never travel (§2: lifecycle and match keys are local), and
  neither do other keys of the stored options document.
- **`display_condition`** terms travel verbatim by code, including terms whose referent is not
  in the export or not on this server (the 2026-09-03 amendment's preservation rule).
- **Never exported**: ids, `applies_to`, `read_capability`, `write_capability`, `read_only`,
  extraction source and mode, `default_value` and team overrides, `status`,
  `deprecated_replacement_id`, `origin_server_id`, timestamps and audit refs, `mirrors_column`.
  No field values or asset content are part of a schema.

### Integrity and bounds

- A stored definition that does not decode faithfully refuses the request (422
  `malformed_definition`, the sorted codes). That covers an options document that is not an
  object, `values` that is not an array, a blank or duplicate slug anywhere in the tree, an
  unknown option status, a `replaced_by` naming nothing, a `display_condition` that is not an
  array of non-blank strings, and a type or subject kind outside the schema's sets. This is
  stricter than `GET /fields`, which leaves an undecodable condition out to keep forms working.
  An export that silently dropped a member would look complete and be wrong.
- The response body is capped at exactly 2 MiB, counted as the bytes the server writes
  (the JSON and its trailing newline) (422 `export_too_large`). Neither a field nor a
  vocabulary is ever truncated to fit.
- The endpoint is read-only: one `SELECT` by code. It writes no definition, option, default,
  permission or asset.

## Amendment 2026-11-22 (later): preview and import contract (#1576)

**Status: backend built; operator UI and two-instance adoption not yet.** These are built, and
the sections "Built: the preview step" and "Built: the apply step" below record what exists:
- the preview step (`POST /fields/import/preview`);
- the apply step (`POST /fields/import/apply`), with receipts and provenance;
- the serialized ordinary `POST /fields`;
- the shared `aa-field-schema/1` limits and the export's `format_limit_exceeded` refusal.

Not built yet: an operator UI, a receipt history surface and the ordinary-`PATCH` controller-row
locking described below as "the G8 fix". The two-instance adoption gate has run; see
"Verified: two-instance adoption (S3)". The export contract
above is otherwise unchanged. Exchange stays operator-initiated: an operator brings an `aa-field-schema/1` document to
this server. There is no `field_set_id`, no automatic federation propagation, and import never
brings field values.

### Flow

1. `POST /api/v1/fields/import/preview` (`previewFieldSchemaImport`) takes the document and an
   optional, untrusted source label and URI. It writes nothing except a short-lived preview record.
   For each incoming code it returns:
   - the incoming portable definition;
   - the local state;
   - the per-property and per-term differences;
   - the decisions allowed for that code.

   It also lists the incoming condition terms that do not resolve here.
2. `POST /api/v1/fields/import/apply` (`applyFieldSchemaImport`) takes the preview id, the
   document digest the preview returned, and **one explicit decision for every incoming code**,
   identical matches included. It applies all of them in one transaction or none of them.

The preview's answers are a review aid, not a promise. Apply re-validates the final configuration
under its locks, and it can refuse with 422 `graph_refused` when an unrelated part of the condition
graph changed after the preview.

### Decisions per incoming code (first version)

Rows are checked top to bottom, and the first that matches decides.

| local state for the code | allowed decisions | outcome recorded |
|---|---|---|
| deprecated or archived (a retired mirrored field included) | `skip` | `skipped` |
| active and mirrored (`mirrors_column` set) | `keep_local`, `skip` | `kept_local` / `skipped` |
| absent | `create`, `skip` | `created` / `skipped` |
| active, but its stored data does not decode, so it cannot be projected | `keep_local`, `skip` | `kept_local` / `skipped` |
| active, portable projection identical to the incoming field | `use_existing`, `skip` | `matched` / `skipped` |
| active, anything different (type, subject kind, any property, any term) | `keep_local`, `skip` | `kept_local` / `skipped` |

- **"Identical" means equal portable projections.** That is the local row projected exactly as the
  exporter projects it, compared with the incoming field. Nothing else counts.
- **Only `created` and `matched` say this server holds the incoming shape.** `kept_local` and
  `skipped` never do. An apply whose decisions are all `skip` or `keep_local` returns an honest
  receipt that changed nothing.
- **Nothing existing is changed by the first version:**
  - no property is overwritten, `display_condition` included;
  - no vocabulary term is added, edited or removed;
  - no code is renamed, no condition term is rewritten, and nothing retired is revived.

  Differences are reported with the local and incoming values, so an operator who wants a change
  can make it with the ordinary field API, under the ordinary rules.

**Future, not part of the first version: adding incoming vocabulary terms to an existing
definition.** If it is built, it needs its own review. Its receipt must record the exact terms
added and every difference that remains, as a `partial` outcome, never as `matched`. Until then,
no preview offers it.

### What the receiver owns

A `create` takes only the portable properties, exactly as sent: no trimming, relabelling or
reordering. Everything local starts at the receiver's own defaults, which are those of
`POST /fields`:
- `applies_to` empty;
- no read or write capability;
- `read_only` false;
- no extraction source, with `skip_if_set`;
- no default and no team overrides;
- `status` active;
- no `mirrors_column`.

The document's options become local option entries `{value, label}` with children, all active, and
with no aliases or `replaced_by`. A `null` label becomes an unlabelled entry.

### The document: shared v1 limits and strict decoding

`aa-field-schema/1` has these limits, and both the exporter and the importer use them:
- **Fields:** 1 to 100 per document, sorted strictly ascending by `code` with no duplicates. The
  exporter already emits this.
- **Options:** for `select`, `multi_select` and `tree` an array, possibly empty; for every other
  type `null`.
- **Option tree:** at most 64 levels deep, and at most 100,000 option nodes per document.
  - **Built:** the exporter refuses a selection that breaks either bound, with 422
    `format_limit_exceeded`. It names the fields too deep, or no codes when the total is too large.
    It measures the exported ACTIVE projection, because retired terms do not travel. Nothing is
    cut to fit.
  - **The size limit does not imply the depth limit.** A 65-level chain of one-letter terms is a
    few kilobytes, so the depth bound is enforced on its own on both paths. The node bound is in
    practice reached by the 2 MiB size first (a minimal term encodes to about 41 bytes), and is
    still checked first and reported as its own problem.
- **Option members:** `value` is a non-empty string. `label` is a non-empty string or `null`, never
  `""`. `children` is always an array.
- **Optional properties:** `regexp_filter`, `display_condition` and `edit_tab` are `null` when
  absent. `display_condition` is otherwise a non-empty array of non-empty strings. `description`
  is a string, possibly empty.
- **Per-field rules:** each field must also satisfy the rules `POST /fields` and `PATCH` apply to
  the same values:
  - the code pattern, and a non-empty label;
  - a known type and subject kind;
  - tree-wide unique option slugs;
  - a valid `regexp_filter`;
  - a non-blank `edit_tab`;
  - well-formed condition terms, none naming the field itself.

  A document breaking any of these is refused with the JSON path of every problem; it is never
  repaired. A valid export can therefore still be refused when its source row violates today's
  create rules, for example a label stored with surrounding spaces by a direct database write. The
  refusal is explicit.

**The whole request is decoded strictly, from the raw bytes.** The generated decoder matches keys
without regard to case and lets a duplicate win, which loses exactly what has to be refused. Both
operations are therefore served by raw route handlers in front of the generated ones, in the way
the peer handshake already is. Their OpenAPI request and response schemas stay real and complete.
The strict decoder refuses, at the envelope, `source`, document, field, option and decision-map
levels:
- unknown members, case-variant keys and duplicate keys;
- a missing required member, a `false` boolean included;
- wrong types, and `null` where the contract has no null;
- trailing bytes or a second JSON value after the request;
- any sender-local property, such as `id`, `applies_to`, a capability, `read_only`, extraction,
  defaults, `status` or `mirrors_column`, because each is an unknown member;
- a `format` other than `aa-field-schema/1`.

### Authorization and bounds

- **Authorization comes first.** It is checked before the body is read. 401 for no identity and 403
  without `fields.admin` or `system.admin`, with bodies that do not depend on the request.
- **Then the body is read, under a cap:**
  - Preview is capped at **2,162,688 bytes** (2 MiB plus 64 KiB for the envelope), with 413 above
    it.
  - Apply is capped at **2,162,688 bytes** too, with 413 above it. A tighter apply cap would make
    some valid previews impossible to apply, because the decision map repeats up to 100 codes of
    any length the document allowed.
- **Then the request is decoded.**
  - An envelope that is not JSON or breaks the envelope rules gets 400 with paths.
  - A document problem gets 422 `document_invalid` with paths, or 422 `unsupported_format`.
- **Canonical form.** The document is re-encoded exactly the way the exporter encodes it, with
  generated member order and a trailing newline. It must be at most 2,097,152 bytes, or the answer
  is 422 `document_too_large`. Its SHA-256 is the document digest, which for an unmodified export
  equals the SHA-256 of the export response body.
- **Largest export.** Because the wrapper allowance covers the envelope, the largest valid export
  always fits.

### Previews, expiry, replay and provenance

- **Previews are server-side and bounded.** A preview lasts 15 minutes. At most 3 previews per
  actor and 20 overall may hold a payload, and an EXPIRED preview counts until the cleanup erases
  it. So a late cleanup cannot let retained documents grow, and a slot can stay taken for a few
  minutes after expiry (429). The counts are taken under a dedicated advisory lock in a READ
  COMMITTED transaction, so a preview that waited for the lock counts the rows committed before it
  got it.
- **What a preview records:**
  - the actor;
  - the canonical document and its digest;
  - the untrusted source claims;
  - the allowed decisions;
  - a digest of every local row it read for the incoming codes and the codes their conditions name.
  Every local row the preview shows and binds, with its digest, comes from ONE statement, so
  what is shown and what is bound are the same row version. Codes with no row are recorded as
  absent, so a later appearance is detectable. The set holds every incoming code and every code an
  incoming condition names, as many as the document holds, and no separate count limits it.
- **What happens when it ends:**
  - On a successful apply, the preview's document and row digests are erased in the same
    transaction. What remains is the durable receipt: actor, preview id, document digest, a digest
    of the decision map, and the per-code items.
  - An expired preview is reduced to a small tombstone (id, actor, times and document digest; the
    document, decisions, bound state and source claims are erased) by a scheduled cleanup.
    The tombstone answers 410 for 24 hours and is then deleted. After that the id answers 404, the
    same as an id that never existed.
- **Replay is answered from the receipt.** A repeated apply first looks for a receipt for that
  preview, before any expiry or staleness check.
  - Same actor, still authorized, same document digest and same decision map: the stored receipt
    comes back, marked as a replay, and nothing is written again.
  - A different decision map gets 409 `preview_consumed`.
  - Replay works for as long as the receipt is kept. Receipts are provenance, and they are kept
    until an operator removes them.
- **Other refusals, all writing nothing:**
  - another actor's preview or an unknown id: 404;
  - a different document digest: 409 `digest_mismatch`;
  - a changed row for any incoming code or any directly referenced code: 409 `preview_stale` with
    the codes.

  Any other change in the graph does not make a preview stale. It is caught instead by apply's own
  validation, as 422 `graph_refused`.
- **Provenance is receiver-recorded history,** written in the same transaction as the definitions.
  One receipt per apply holds:
  - the actor, time, document digest and format;
  - the source label and URI, as claims the operator typed, never as peer identity or
    authorization;
  - one item per incoming code, with its decision, outcome and the local definition id.

  It never travels in the portable document. An updated remote schema always needs a new preview.

### Atomic apply, ordinary creation and the final graph

- **Who can change what the validation reads:**
  - A field's `display_condition` changes only under the subject-kind advisory locks of ADR 0099
    §8.
  - With this amendment, every ordinary `POST /fields` also takes them.
  - `status` and `applies_to` can still change through row-locking writers that take no advisory
    lock: `PATCH` without a condition, and archive.
  - `code`, `type`, `subject_kind` and `mirrors_column` cannot change through the API.
- **Apply and ordinary `POST /fields` run the same sequence,** in one transaction:
  1. Take both advisory locks, asset then collection. That freezes every condition edge and every
     code.
  2. Lock the preview row (apply only).
  3. Read the edges. Then compute the rows whose attributes the validation consults: every created
     field, every stored dependent that names a created code, and every controller those fields
     name.
  4. Lock those rows `FOR UPDATE` in code order.
  5. Recheck freshness and the decisions from the locked rows.
  6. Validate, write, and record.
- **Why this is enough.** A concurrent archive or `applies_to` edit that would invalidate the
  configuration being committed waits for the commit, and is then ordinary later drift (ADR 0099
  §7).
- **Ordering.** No writer takes an advisory lock after a row lock, and every writer that takes both
  advisory locks takes them in the same order.
- **The final proposed graph is validated, not a sequence of steps.** It holds the current edges,
  the locked attributes and every created field, and it does not depend on document order.
  - Every resolvable edge passes the ordinary ADR 0099 §6 refusal list.
  - A stored dependent whose term now resolves to a created field is validated with it.
  - Terms naming a code that exists in neither are kept verbatim. Only their cycle participation
    and applicability contribution are deferred.
  - The existing cycle walk runs over that complete graph from every created dependent and every
    newly resolved dependent. Any new cycle must contain one of their edges.
  - Any refusal rolls back the whole apply, receipt and preview consumption included.
- **Deferred edges are confronted when they resolve.** A later import revalidates them through the
  same final graph. An ordinary `POST /fields` of a named code validates every dependent that names
  it under the same locks, and is refused with 409 `display_condition_conflict` naming the
  dependents if any would be invalid. This ships in the same release as the first apply, never
  after it.
- **Ordinary `PATCH` keeps refusing a newly submitted condition that names a missing field.** An
  imported condition with unresolved terms survives unrelated edits, because only a request that
  touches `display_condition` validates it. Editing that condition requires resolving or removing
  its missing terms.
- **After commit,** the changed field caches, the extraction configuration and any search or
  composition caches that read definitions are invalidated, and one audit event records the
  receipt.

### Built: the preview step (S1)

`POST /api/v1/fields/import/preview` exists as described in "Flow", "The document" and
"Authorization and bounds".
- **Routing:** a raw route registered in front of the generated adapter. The OpenAPI operation is
  fully described; the generated method is never reached.
- **Before the body:** identity, then `fields.admin` or `system.admin`, before any byte is read.
- **Reading:** under a 2,162,688-byte cap, regardless of Content-Length.
- **Decoding:** strict, from the raw bytes. Invalid UTF-8 and escapes of unpaired surrogates are
  refused, never replaced.
- **Storage:** one preview row (migration 00095), holding the canonical document bytes, its
  digest, the untrusted source claims, the allowed decisions and the bound state. The bound state
  is a digest per present row and the list of absent codes.
- **Local states and decisions:** follow the table above, with `retired` checked first.
- **Unresolved terms:** listed, never refused for being absent.
- **No writes elsewhere:** a preview changes no definition, option, value, default, permission or
  role.
- **Cleanup:** a five-minute cleanup compacts expired previews to tombstones (keeping id, actor,
  times and document digest; erasing the document, decisions, bound state and source claims) and
  deletes tombstones after 24 hours. It works in batches that skip rows another transaction holds, and
  takes no other lock.

The apply step, receipts, provenance and the serialized ordinary create were added in S2,
recorded in the next section.

### Built: the apply step and serialized creation (S2)

`POST /api/v1/fields/import/apply` (`applyFieldSchemaImport`) is a raw route over the generated
adapter, like the preview.
- **Before the body:** identity (401), then `fields.admin` or `system.admin` (403).
- **Reading:** under a 2,162,688-byte cap (413).
- **Envelope `{preview_id, document_digest, decisions}`:** decoded strictly from the raw bytes.
  `preview_id` must be a lower-case UUID, and `document_digest` `sha256:` and 64 lower-case hex.
  Every envelope fault is 400 `request_invalid`.
- **Parser context:** structural. Only the value of the top-level member named exactly
  `document` is the document, so envelope keys such as `document.x` or `document[0]` stay 400.
- **Checks, by branch:**
  1. **A receipt for the preview** (checked first, before any expiry or staleness check; a receipt
     can be replayed for as long as it is kept):
     - another operator's gives 404 `preview_not_found`;
     - a different digest gives 409 `digest_mismatch`;
     - different decisions give 409 `preview_consumed`;
     - otherwise the original receipt, `replayed: true`, with nothing written.
  2. **Otherwise, one READ COMMITTED transaction:**
     - both graph locks (1173: asset, then collection);
     - the receipt again (a concurrent winner answers as a replay);
     - the preview row `FOR UPDATE`; then the database clock, in a separate statement, against its
       expiry. Missing, another operator's, or more than 24 hours past expiry (whether or not the
       cleanup has deleted it) gives 404. A tombstone, or a live preview past expiry, gives 410;
     - digest gives 409 `digest_mismatch`;
     - the strict condition graph;
     - the read set `FOR UPDATE` in code order: the incoming and directly referenced rows, the
       stored dependents a creation resolves, and their controllers;
     - bound-row digests and absences gives 409 `preview_stale`;
     - the exact decision key set and allowed decisions, recomputed from the locked rows, gives
       422 `decision_invalid`;
     - the complete final graph, validated for every created dependent and every stored dependent
       it resolves, gives 422 `graph_refused`;
     - inserts (every portable member, receiver defaults for every local setting), the receipt
       with its items, and deletion of the preview row;
     - **the database clock again, immediately before commit.** A preview that expired while the
       apply waited on any lock gives 410, and everything written above rolls back. A projected
       expiry in the locking SELECT is not used for the deadline, because it can be evaluated
       before the lock wait ends.
  3. **Any failure rolls back everything.** A value the database cannot store, such as a code too
     long to index, gives 422 `unstorable_value`.
- **After commit:** the field-by-id cache is invalidated for each created definition, exactly as
  ordinary `POST /fields` does. No further cache needs it:
  - the extraction cache lists only fields with an extraction source, and created fields have
    none;
  - IIIF manifests and search results are built from field values, and a new field has none;
  - field lists and composition are read from the database.

  One `metadata.field_schema.imported` audit event is written for every newly committed receipt,
  `changed` 0 included, and none for a replay, refusal or rollback.
- **Receipts:** migration 00096.
  - `preview_id` is UNIQUE.
  - Items are keyed by `(receipt_id, ordinal)`, the sorted position 0 to 99, and the code is plain
    unindexed text. So a receipt adds no code-length limit, and a code too long to index can
    still be recorded as `skipped`.
  - `field_id` has no foreign key, so later changes never rewrite a receipt.
  - Receipts are not swept.
- **Ordinary `POST /fields`:** now one transaction that takes both graph locks before looking for
  stored dependents. It validates the dependents the new code resolves, with the new field's real
  type, subject kind and `applies_to`, and refuses with 409 `display_condition_conflict` naming
  them. Otherwise it creates as before.
- **Ordinary `PATCH`:** unchanged. It still refuses a newly submitted missing reference, and an
  imported unresolved condition survives unrelated edits.
- **The G8 fix is not built, and stays a follow-up.** That is the controller-row locking for
  `PATCH`: today `PATCH` validates a submitted condition without row-locking its controllers, and
  a `PATCH` that changes `applies_to` and the condition together validates against the stored row.
  Both pre-date import and are unchanged here. Apply and the ordinary create lock their own read
  sets: the dependents a creation resolves and their controllers. A concurrent archive or
  `applies_to` change on those rows waits for their commit.

### Verified: two-instance adoption (S3)

`TestTwoStack_FieldSchemaExchange` (app/internal/federation/twostack) runs the operator chain
between two current-code servers. Each server is the real production router over TLS, on its
own migrated database, storage root and site identity. The test does not use a published
release binary.

The chain: A exports a selection; B previews A's exact response bytes, so the preview digest
equals the SHA-256 of A's body; B applies explicit decisions; B exports. Along the way it shows:
- **The four outcomes:** `matched` for an identical field whose local capability, read-only,
  `applies_to`, default, team override, extraction, alias and retired-term settings all stay
  B's own; `created` for a nested vocabulary and for a text field with a pattern, a tab and a
  condition naming a controller B lacks; `kept_local` for a type conflict; `skipped` for an
  option conflict.
- **Byte-identical exports:** the adopted subset exports from A and from B byte for byte.
- **Preservation:** every other B definition row, every existing value row (compared whole, on
  an owned asset with values on the matched and conflicting fields, including an archived local
  term), the default overrides and the permission tables are unchanged, and so is A. That holds
  through the preview, the refused and successful applies, the replay, the stale refusal and the
  later reviews; the only exception is the one intended local label edit. A control that
  rewrites a value during an otherwise successful apply is caught.
- **Provenance:** receipt and audit event on B; a replay changes nothing.
- **Refusals:** all refused, with nothing written:
  - anonymous and cross-instance credentials on preview and apply;
  - a member on preview and on apply, with the same body for valid and malformed requests;
  - a sender-local member, a malformed document and an oversized body;
  - incomplete decisions, another operator's preview, and a stale preview.
- **Updates and later resolution:** an updated source schema leaves B alone until a new
  preview, where it is kept local on request. The omitted controller, adopted later through the
  same chain, resolves the term B kept verbatim.
- **The reverse direction:** a compact B to A adoption works the same way.

Not shown by this gate: an operator UI or receipt history (not built), the full local and hosted
test suites, any federation profile, and a cross-version or release gate.

### Built: the operator pages and the receipt history (S4)

The sections above stay as they were written; this one records what S4 added.

**History API.** `GET /fields/import/receipts` and `GET /fields/import/receipts/{receipt_id}`
are read-only. Both are raw routes, like preview and apply, so authorization comes before the
query or the path is read: 401 anonymous, 403 without `fields.admin` or `system.admin`, then 400
for a malformed request and 404 for an unknown id. Any field administrator reads every local
receipt. That grants nothing on apply, where a preview stays bound to the operator who made it.
- The list is newest first by `(applied_at, id)`, 25 by default and 1 to 100 per page. Paging
  is by keyset over a matching index (migration 00096), never by offset. The cursor is opaque,
  versioned, at most 512 bytes, and accepted only in the exact canonical spelling the server
  writes for a position (no padding, newline or alternative trailing bits). A query that does
  not parse (a bad escape, a semicolon separator) or a cursor that is not canonical is a 400,
  never a silent restart at the first page. The cursor is a position, not a credential: it is
  not signed, and a well-formed cursor for any position is accepted as that position.
- A summary carries the digests, the unverified source note, `changed` and the field count.
  The detail adds the preview id and, in recorded order, each code's decision, outcome,
  incoming digest and the field id as it was then, which may name a field since deleted. There
  is no replay flag. A removed account reads as a null actor.
- Reading writes nothing: no audit entry, no preview touched. A failed query is a 500, never an
  empty page.

**Pages.** `/admin/fields/schema-exchange` and `/admin/fields/import-history`, linked from the
fields index. The fields admin tile now names `fields.admin`, so a delegated field
administrator can find them.
- Export sends a deliberate selection of 1 to 100 active, non-mirrored fields and saves the
  response bytes as they arrived.
- Import sends the document text as read or pasted, embedded unparsed in the preview envelope;
  the page parses it only to prove it is one JSON value. Before that, a file is refused unread
  when it cannot fit the server's 2,162,688-byte preview body, its bytes must be valid UTF-8
  (refused, never repaired), pasted text with an unpaired surrogate is refused, and the exact
  request is measured against the same bound. The 2 MiB canonical document limit stays the
  server's to judge. The source note is optional and sent as typed, checked as the server
  checks it (label at most 200 characters, URI at most 2,048 bytes, absolute http or https).
- Every incoming field shows all 17 portable members before a decision, open by default for a
  field this server lacks, and each decision is labelled with its effect. No decision is
  pre-selected, and Apply appears only when every code has an allowed one, beside a summary
  and a reminder that local settings and values stay local.
- The apply request is built once and frozen. A failure with an unknown outcome keeps that
  exact request and LOCKS the page on it: the document, file, source note, decisions and
  Preview are disabled until a retry of the same request resolves it, and the page links to
  the history. The server applies a request at most once and answers a repeat with the
  recorded receipt, even after the preview's clock expiry. Discarding the request needs an
  explicit acknowledgment that it may already have applied. (This supersedes the first S4 text,
  under which any edit or new preview discarded the frozen request.) Otherwise an edit or new
  preview discards the preview, its decisions, the frozen request and the previous result, and
  a late answer for an older preview is ignored.
- Stale, expired, consumed and missing previews end the preview; a 422 lets the decisions
  change; 401, 403 and 429 keep the request and leave an unknown outcome unknown.
- Each page belongs to one signed-in operator. A change of account, a loss or degradation of
  rights, or a sign-out clears the page and ignores answers still in flight; an apply still in
  flight or with an unknown outcome is never retried under another account and is reported as
  possibly applied by the operator who sent it. A pending file read belongs to the file choice,
  so editing the source note while it reads does not strand it.
- Failed reads offer Retry at the same position; the history detail says it records what
  happened at apply time, not the fields as they are now. Visible text is in the en, es and fr
  catalogues; codes, property names, decisions and digests stay as data. Operator-typed and
  document text renders as text only.

Verified by real-router tests for the history endpoints, rendered component tests for both
pages, and a maintained standalone browser spec
(`scripts/dogfood/ui/tests/standalone/field-schema-exchange-1576.spec.ts`).

### Still not decided by this amendment

- Adding vocabulary terms to existing definitions.

#1576 stays open until the full local and hosted test suites pass. The backend API, provenance,
the two-instance gate, the operator pages and the receipt history are done.

## Amendment 2026-11-23: the rights baseline is sent (#1577, first slice)

The 2026-11-17 amendment above says a rights URI and a required statement are two field
definitions in the `rights` display group. Migration 00097 seeds them: `rights_uri` and
`required_statement`, plain text, ungated, active, for every asset type, with the participation
defaults every shipped field has, each with a fixed id. An
install that already has a field with either code, or a row with either id, keeps it untouched and
gets no seeded row. No write validation is added to either field.

An `aa:AssetVersion` (spec §17.2) now carries, from those two rows and the asset's own columns:
- `description.title` and `description.description`, from `assets.title` and
  `assets.description`;
- `rights`, from `rights_uri`, only when it is a Creative Commons or RightsStatements.org URI;
- `requiredStatement`, from `required_statement`, labelled `Attribution`.

The peer is not a local identity and inherits nobody's capabilities, so a member is sent only from
a source no capability gates:
- a column only when no field definition mirroring it carries a `read_capability`;
- a field value only from the row with the seeded id and code, while it is active, plain text,
  applicable to the asset, not a mirror and ungated.

A value the wire rules refuse, or that is too long, is left out with a reason code, never
truncated. No other field is ever used in a seeded field's place, and `credit` and `copyright` are
not mapped. The members are captured when the announcement is recorded, so a redelivery sends the
same bytes. They are re-checked against current privacy on every delivery: if a recorded source
became gated, retired or inapplicable, the whole announcement is refused. A receiver keeps them as
untrusted declared claims and imports nothing into its own schema.

Not in this slice: descriptive changes in an `Update` (§17.2.8 still excludes them), custom field
values (they need a new profile), the other DCMI terms, identifiers, local IIIF rights, and
preservation events.
