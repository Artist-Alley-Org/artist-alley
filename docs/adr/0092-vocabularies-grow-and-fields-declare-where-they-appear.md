---
id: "0092"
title: Vocabularies grow from use, and a field declares where it appears
status: accepted
date: 2026-08-19
area: architecture
phases: []
supersedes: []
related:
  - "0012"
  - "0091"
tags:
  - metadata
  - fields
  - search
  - vocabulary
excerpt: >-
  A production catalogue has thousands of values per field, so a vocabulary is
  searched on the server, never shipped whole; a keyword field grows as people
  use it, gated by one capability; and each field declares which surfaces it
  appears on rather than every surface guessing.
---

## Context

Our metadata fields work, but they were designed against a seeded catalogue
with a dozen values per field. The owner, who runs a production DAM daily, put
it plainly:

> The metadata fields are all just listed. Which isn't a problem in our local
> because we have only like a dozen at most in some fields. However, real
> production will have 1000s. We need to use dynamic boxes … We are thinking
> too small for the task.

Three specific gaps follow from that, and a fourth from the same conversation:

1. **Every surface renders a field's whole vocabulary.** Fine at twelve values,
   unusable at two thousand, and the payload ships them all whether or not the
   reader opens the control.
2. **No field type grows from use.** Every vocabulary is operator-curated up
   front. The reference DAM's *dynamic keyword list* — a field whose value set
   grows as people type new terms — has no counterpart here, and it is the type
   artists actually reach for.
3. **No field says where it belongs.** Advanced search shows every filterable
   field; the edit page shows every field; nothing lets an operator say "this
   one is for the search page, that one is for upload only."
4. **A vocabulary drifts** — "concept-art", "concept art", "ConceptArt" — with
   no way to merge them after the fact (#789).

## Decision

**1. A vocabulary is a searchable resource, not a payload.** Any surface that
offers vocabulary values queries the server with a prefix and a limit. Shipping
a whole vocabulary to the client is permitted only as an optimisation for
demonstrably small ones, and never as the only path — the search endpoint is
the contract, the bulk fetch is the shortcut.

**2. A keyword field grows on use, gated by one capability.** A new field type
accepts values outside its current set: typing an unknown term and saving
creates it. Who may do that is a capability, so an instance can let everyone
extend a vocabulary, or restrict extension to librarians while everyone else
picks from what exists. A user without it sees the same control with the
create arm absent — never a silent failure.

**3. A field declares its surfaces.** Field definitions carry participation
flags — which search surfaces it appears on, whether it shows at upload, which
edit tab it sits in, whether it is visible at all (retired without deletion).
Surfaces read the flags; they do not infer participation from a field's type or
from whether it happens to have values.

**4. Normalisation is alias-then-merge, and it is reversible in the tracker
sense.** Drift is resolved by pointing one value at another (an alias), and a
merge rewrites references and leaves a tombstone rather than deleting a row —
so a value that turns out to have been merged wrongly can be told apart from
one that never existed. This is where we go beyond the reference DAM, which
has no merge tooling at all.

## Consequences

- **The client never assumes it has the whole list.** Any code path that
  filters a vocabulary in memory must be reachable only when the whole list was
  deliberately fetched — otherwise it silently searches a prefix of the truth.
  v0.10.1's advanced-search typeahead is exactly this shape and its comment says
  so; this ADR makes that the rule rather than that page's local note.
- **Free-text keywords make a moderation surface necessary.** A field anyone
  can extend accumulates typos and worse. The alias/merge tooling is not
  optional polish; it is the counterpart to letting the vocabulary grow.
- Participation flags mean an operator can make the advanced page smaller than
  the field catalogue. That is the point: an install with 200 fields does not
  want 200 filters, and the person who knows which 12 matter is the operator.
- Search composition stays one representation (the query grammar), so a field
  filter selected on the advanced page and one applied from a feed panel are
  the same query — a field's *participation* decides where the control appears,
  never how the query is expressed.
- Existing installs are unaffected until an operator sets a flag: absent flags
  mean today's behaviour.

## Alternatives considered

**Ship vocabularies and paginate client-side.** Rejected: the payload is the
problem, and a 2,000-value field on a page with 20 such fields is 40,000 values
before the reader touches anything.

**Let any text field accept new values (no distinct keyword type).** Rejected:
"this field has a controlled vocabulary that happens to be extensible" and
"this field is free text" are different promises to the person searching, and
collapsing them makes faceting meaningless.

**Delete on merge instead of tombstoning.** Rejected: a merge that turns out to
be wrong becomes indistinguishable from a value that never existed, and
federation makes that permanent — a peer that saw the old value has no way to
learn what happened to it.
