---
id: "0101"
title: Preservation history is a per-asset record of PREMIS events; the activities ledger only carries them
status: accepted
date: 2026-11-17
area: architecture
phases:
  - "1.22"
supersedes: []
related:
  - "0043"
  - "0044"
  - "0053"
  - "0083"
tags:
  - federation
  - preservation
  - premis
  - fixity
  - archival
excerpt: >-
  An archive receiving a shared asset needs its preservation history:
  when it was ingested, when its files were fixity-checked and with what
  result, how its format was identified, when it was transferred,
  migrated or withdrawn. That history is recorded per asset as PREMIS 3
  events with the Library of Congress event-type terms. The activities
  ledger of ADR 0044 stays the transport: it carries events between
  peers in the profile-gated aa:Provenance activity, but it is not where
  an asset's history lives. Specified, not implemented.
---

## Status (2026-11-17)

Specified with the `archival-1` profile (spec §17.4, #1513). Nothing is
implemented: no event is recorded, sent or received yet. The receiving
side is #1574 (ingest and fixity events) and #1577 (mapping received
events); local events such as scheduled fixity checks are follow-up
work.

## Context

Archival tools (Archivematica, OCFL-based repositories, the POWRR
walkthroughs for Exactly and Fixity) all keep a per-object event
history: ingestion, fixity checks, format identification, virus checks,
migration, replication, deaccession. PREMIS 3 is the shared vocabulary
for it: Objects, Events, Rights and Agents, with event types from the
Library of Congress preservation vocabulary.

Artist Alley has pieces of this already, in the wrong shape for it:

- the checksum sweep (`storage/sweep.go`) runs when an admin starts it,
  and its results sit in a sweep table, not in any asset's history;
- uploads compute SHA-256 server-side (`storage/service.go`), but the
  fact that an asset was ingested with that hash is not recorded as an
  event;
- the activities ledger (ADR 0044) records federation activities, which
  are messages between instances, not the history of an object.

## Decision

1. **An asset's preservation history is a list of PREMIS events about
   that asset.** Each event has an id, a type (one of the archival-1
   tokens, each standing for the LoC term with the same label), a
   time, an outcome (`success`, `failure`, `warning`), optional detail,
   the object it is about (asset, and optionally a version and a file by
   CAS URI) and one or more agents (a person by actor URI, or software
   by name and version). The wire shape is spec §17.4.
2. **The activities ledger is transport, not storage.** ADR 0044's
   ledger carries an event to a peer as an `aa:Provenance` activity,
   only under the negotiated `archival-1` profile. Received events are
   kept with the receiving copy of the asset, marked as the sender's
   claims, and the receiver adds its own events.
3. **A transfer is recorded on both sides.** The sender records a
   `transfer`; the receiver records an `ingestion` and a `fixity-check`,
   whose outcome is `failure` when a file fails the arrival check
   (spec §17.6).
4. **Local events come from what already exists.** Upload ingestion,
   the checksum sweep's per-asset results, a future format
   identification and a withdrawal each become events on the asset.
   Making the sweep write per-asset events is a follow-up, not part of
   this decision's first implementation.

## Consequences

- A receiving archive can show and export (for example as BagIt
  metadata, spec §17.7) how an asset was handled before and after it
  arrived.
- Events are claims: a receiver trusts its own `fixity-check`, not the
  sender's. Software versions in agents can reveal deployment details,
  so an operator may omit the version on events it sends.
- The ledger stays small and transport-shaped (ADR 0044), and the
  per-asset history can grow without changing it.

## Alternatives considered

- **Fold preservation history into ADR 0044's ledger.** Rejected: the
  ledger records messages between instances, and much of an asset's
  history (local fixity checks, migrations) is never sent anywhere.
- **Invent a custom event vocabulary.** Rejected: PREMIS and the LoC
  terms are what archives already read; a custom list would need a
  mapping on every receiving side.

## References

- Spec v1 §17.4 (event shape), §17.6 (fixity on arrival), §17.7 (BagIt).
- PREMIS 3: https://www.loc.gov/standards/premis/v3/
- LoC Preservation Events Controlled Vocabulary (2017): https://www.loc.gov/standards/premis/v3/preservation-events.pdf (term labels; six of the eight terms used here also have an id.loc.gov eventType link)
- ADR 0043 (amendment 2026-11-17), ADR 0044, ADR 0053, ADR 0083.
